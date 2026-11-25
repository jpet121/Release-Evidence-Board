package board

import (
	"bytes"
	"encoding/json"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	bolt "go.etcd.io/bbolt"
)

const (
	aiSnapshotKey     = "ai_snapshot"
	aiSnapshotVersion = "reb-ai-state/1"
	aiHistoryBucket   = "ai_history_v1"
	aiHistoryBinding  = "ai_history_v1_sha256"
	aiBudgetBucket    = "ai_budget_v1"
	aiBudgetBinding   = "ai_budget_v1_sha256"
	aiSnapshotMaxSize = 16 << 20
	aiDailyRequests   = 20
	aiDailyMicroUSD   = 5_000_000
)

// HistoricalAI contains only the information suitable for Operator health.
// Exact request, response and edit bytes stay in protected Board snapshots.
type HistoricalAI struct {
	Requests      []HistoricalAIRequest
	RequestsToday int
	BilledToday   int64
	Held          int64
	Remaining     int64
	Exhausted     bool
}

type HistoricalAIRequest struct {
	ID, CandidateID, RequestedAt, DisabledAt, PayloadSHA256, ResponseSHA256 string
	Reviews                                                                 []HistoricalAIReview
}

type HistoricalAIReview struct {
	SuggestionID, Actor, At, Disposition string
}

type aiRequest struct {
	CandidateID        string            `json:"candidate_id"`
	Requester          string            `json:"requester"`
	RequestedAt        string            `json:"requested_at"`
	CompletedAt        string            `json:"completed_at"`
	DisabledAt         string            `json:"disabled_at"`
	ModelSHA256        string            `json:"model_sha256"`
	PromptSHA256       string            `json:"prompt_sha256"`
	ConfigSHA256       string            `json:"config_sha256"`
	Payload            []byte            `json:"payload"`
	PayloadSHA256      string            `json:"payload_sha256"`
	Response           []byte            `json:"response"`
	ResponseSHA256     string            `json:"response_sha256"`
	SourceIDs          []string          `json:"source_ids"`
	SuggestionsEnabled bool              `json:"suggestions_enabled"`
	GroupingEnabled    bool              `json:"grouping_enabled"`
	InputTokens        int               `json:"input_tokens"`
	OutputTokens       int               `json:"output_tokens"`
	Reviews            []json.RawMessage `json:"reviews"`
}

type aiBudgetFact struct {
	RequestedAt string `json:"requested_at"`
	State       string `json:"state"`
	Amount      int64  `json:"amount_microusd"`
}

type aiReview struct {
	SuggestionID string `json:"suggestion_id"`
	Actor        string `json:"actor"`
	At           string `json:"at"`
	Disposition  string `json:"disposition"`
	Edit         string `json:"edit"`
}

type aiReply struct {
	Suggestions []json.RawMessage `json:"suggestions"`
	Groups      []json.RawMessage `json:"groups"`
}

type aiSuggestion struct {
	ID          string   `json:"id"`
	Behavior    string   `json:"behavior_id"`
	Sources     []string `json:"source_row_ids"`
	Test        string   `json:"proposed_test"`
	Rationale   string   `json:"rationale"`
	Uncertainty string   `json:"uncertainty"`
	Question    string   `json:"question"`
}

type aiGroup struct {
	ID      string   `json:"id"`
	Label   string   `json:"label"`
	Sources []string `json:"source_row_ids"`
}

func readHistoricalAI(tx *bolt.Tx, now time.Time) (*HistoricalAI, error) {
	meta := tx.Bucket([]byte(metaBucket))
	marker := meta.Get([]byte(aiSnapshotKey))
	buckets := []string{aiHistoryBucket, aiHistoryBinding, aiBudgetBucket, aiBudgetBinding}
	var present int
	for _, name := range buckets {
		if tx.Bucket([]byte(name)) != nil {
			present++
		}
	}
	if marker == nil && meta.Bucket([]byte(aiSnapshotKey)) == nil && present == 0 {
		return nil, nil
	}
	if string(marker) != aiSnapshotVersion || present != len(buckets) {
		return nil, integrity("historical AI snapshot version or storage sections are invalid")
	}
	summary := new(HistoricalAI)
	facts, err := readAIBudget(tx, now, summary)
	if err != nil {
		return nil, err
	}
	err = visitBoundAIRecords(tx, aiHistoryBucket, aiHistoryBinding, func(id string, raw []byte) error {
		request, err := historicalAIRequest(tx, id, raw, facts, now)
		if err != nil {
			return err
		}
		summary.Requests = append(summary.Requests, request)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return summary, nil
}

func visitBoundAIRecords(tx *bolt.Tx, records, bindings string, visit func(string, []byte) error) error {
	if err := verifyCompanionKeys(tx, bindings, records); err != nil {
		return err
	}
	return tx.Bucket([]byte(records)).ForEach(func(key, raw []byte) error {
		id := string(key)
		if !idPattern.MatchString(id) || raw == nil || len(raw) > aiSnapshotMaxSize {
			return integrity("historical AI record key or length is invalid")
		}
		if err := checkRecordBinding(tx, records, bindings, id); err != nil {
			return err
		}
		return visit(id, raw)
	})
}

func readAIBudget(tx *bolt.Tx, now time.Time, summary *HistoricalAI) (map[string]string, error) {
	requests := make(map[string]string)
	today := now.UTC().Format(time.DateOnly)
	err := visitBoundAIRecords(tx, aiBudgetBucket, aiBudgetBinding, func(id string, raw []byte) error {
		fact, ok := decodeAIObject[aiBudgetFact](raw, "requested_at", "state", "amount_microusd")
		at, valid := parseTimestamp(fact.RequestedAt)
		if !ok || !valid || (!now.IsZero() && at.After(now)) || fact.Amount < 0 ||
			(fact.State != "billed" && (fact.State != "unresolved" || fact.Amount == 0)) {
			return integrity("historical AI accounting fact is invalid")
		}
		requests[id] = fact.RequestedAt
		onDay := at.UTC().Format(time.DateOnly) == today
		if onDay {
			summary.RequestsToday++
		}
		var total *int64
		switch {
		case fact.State == "unresolved":
			total = &summary.Held
		case onDay:
			total = &summary.BilledToday
		}
		if total != nil {
			if fact.Amount > math.MaxInt64-*total {
				return integrity("historical AI accounting exceeds representable amounts")
			}
			*total += fact.Amount
		}
		return nil
	})
	// Compare before subtracting to avoid overflow even with very large
	// unresolved facts. Unresolved reservations carry across UTC days.
	summary.Remaining = aiDailyMicroUSD - min(aiDailyMicroUSD, summary.Held)
	summary.Remaining -= min(summary.Remaining, summary.BilledToday)
	summary.Exhausted = summary.RequestsToday >= aiDailyRequests || summary.Remaining == 0
	return requests, err
}

func historicalAIRequest(tx *bolt.Tx, id string, raw []byte, facts map[string]string, now time.Time) (HistoricalAIRequest, error) {
	item, ok := decodeAIObject[aiRequest](raw, "candidate_id", "requester", "requested_at", "completed_at", "disabled_at",
		"model_sha256", "prompt_sha256", "config_sha256", "payload", "payload_sha256", "response", "response_sha256",
		"source_ids", "suggestions_enabled", "grouping_enabled", "input_tokens", "output_tokens", "reviews")
	requested, requestOK := parseTimestamp(item.RequestedAt)
	completed, completeOK := parseTimestamp(item.CompletedAt)
	disabled, disabledOK := parseTimestamp(item.DisabledAt)
	if !ok || !requestOK || !completeOK || !disabledOK || completed.Before(requested) || disabled.Before(completed) ||
		(!now.IsZero() && disabled.After(now)) || facts[id] != item.RequestedAt ||
		!idPattern.MatchString(item.Requester) || !digestPattern.MatchString(item.ModelSHA256) ||
		!digestPattern.MatchString(item.PromptSHA256) || !digestPattern.MatchString(item.ConfigSHA256) ||
		item.InputTokens < 1 || item.InputTokens > 20_000 || item.OutputTokens < 1 || item.OutputTokens > 4_000 ||
		len(item.Payload) == 0 || !utf8.Valid(item.Payload) || digest(item.Payload) != item.PayloadSHA256 ||
		digest(item.Response) != item.ResponseSHA256 || (!item.SuggestionsEnabled && !item.GroupingEnabled) {
		return HistoricalAIRequest{}, integrity("historical AI request identity, chronology, usage or bytes are invalid")
	}
	candidate, _, baseline, err := verifiedCandidateRecords(tx, item.CandidateID)
	if err != nil || requested.Before(candidate.CreatedAt) {
		return HistoricalAIRequest{}, integrity("historical AI request refers to an invalid candidate")
	}
	behaviors := make(map[string]string, len(baseline.Baseline.Behaviors))
	for _, behavior := range baseline.Baseline.Behaviors {
		behaviors[behavior.ID] = "behavior:" + behavior.ID + ":" + strconv.Itoa(behavior.Revision)
	}
	citations, results, err := aiRequestSources(tx, item, behaviors, requested)
	if err != nil {
		return HistoricalAIRequest{}, err
	}
	suggestions, err := validateAIReply(item, behaviors, citations, results)
	if err != nil {
		return HistoricalAIRequest{}, err
	}
	reviews, err := validateAIReviews(item.Reviews, suggestions, completed, disabled)
	if err != nil {
		return HistoricalAIRequest{}, err
	}
	return HistoricalAIRequest{ID: id, CandidateID: item.CandidateID, RequestedAt: item.RequestedAt,
		DisabledAt: item.DisabledAt, PayloadSHA256: item.PayloadSHA256, ResponseSHA256: item.ResponseSHA256,
		Reviews: reviews}, nil
}

func aiRequestSources(tx *bolt.Tx, item aiRequest, behaviors map[string]string, requested time.Time) (map[string]bool, map[string]bool, error) {
	if len(item.SourceIDs) == 0 {
		return nil, nil, integrity("historical AI request is missing source citations")
	}
	citations := make(map[string]bool, len(item.SourceIDs))
	results := make(map[string]bool)
	for _, source := range item.SourceIDs {
		if citations[source] {
			return nil, nil, integrity("historical AI request repeats a citation")
		}
		if id, ok := strings.CutPrefix(source, "result:"); ok {
			runID, err := getJSON[string](tx, resultBucket, id)
			if err != nil {
				return nil, nil, integrity("historical AI request cites a missing result")
			}
			run, err := getJSON[Run](tx, runBucket, runID)
			if err != nil || run.CandidateID != item.CandidateID || run.ImportedAt.After(requested) ||
				run.Closed.After(requested) || !slices.ContainsFunc(run.Rows, func(row SourceRow) bool {
				return row.Kind() == Result && row.ResultID() == id && row.Outcome() == "FAIL"
			}) {
				return nil, nil, integrity("historical AI request cites a result outside its selected failure evidence")
			}
			results[id] = true
		} else {
			rest, ok := strings.CutPrefix(source, "behavior:")
			id, _, hasRevision := strings.Cut(rest, ":")
			if !ok || !hasRevision || behaviors[id] != source {
				return nil, nil, integrity("historical AI request cites behavior outside its approved revision")
			}
		}
		citations[source] = true
	}
	return citations, results, nil
}

func validateAIReply(item aiRequest, behaviors map[string]string, citations, results map[string]bool) (map[string]bool, error) {
	reply, ok := decodeAIObject[aiReply](item.Response, "suggestions", "groups")
	if !ok || len(reply.Suggestions) > 20 || len(reply.Groups) > 20 ||
		(!item.SuggestionsEnabled && len(reply.Suggestions) > 0) || (!item.GroupingEnabled && len(reply.Groups) > 0) {
		return nil, integrity("historical AI response does not match its enabled C04 arrays")
	}
	suggestions := make(map[string]bool, len(reply.Suggestions))
	for _, raw := range reply.Suggestions {
		value, ok := decodeAIObject[aiSuggestion](raw, "id", "behavior_id", "source_row_ids", "proposed_test", "rationale", "uncertainty", "question")
		if !ok || !idPattern.MatchString(value.ID) || suggestions[value.ID] || behaviors[value.Behavior] == "" ||
			!validAISources(value.Sources, citations, 1) || !validText(value.Test, 2000) || !validText(value.Rationale, 2000) ||
			utf8.RuneCountInString(value.Question) > 2000 ||
			(value.Uncertainty != "clear" && (value.Uncertainty != "needs_review" || strings.TrimSpace(value.Question) == "")) {
			return nil, integrity("historical AI suggestion violates C04")
		}
		suggestions[value.ID] = true
	}
	groups := make(map[string]bool, len(reply.Groups))
	for _, raw := range reply.Groups {
		value, ok := decodeAIObject[aiGroup](raw, "id", "label", "source_row_ids")
		if !ok || !idPattern.MatchString(value.ID) || groups[value.ID] || !validText(value.Label, 2000) ||
			!validAISources(value.Sources, results, 2) {
			return nil, integrity("historical AI group violates C04")
		}
		groups[value.ID] = true
	}
	return suggestions, nil
}

func validAISources(used []string, allowed map[string]bool, minimum int) bool {
	if len(used) < minimum {
		return false
	}
	seen := make(map[string]bool, len(used))
	for _, id := range used {
		if !allowed[id] || seen[id] {
			return false
		}
		seen[id] = true
	}
	return true
}

func validateAIReviews(rows []json.RawMessage, suggestions map[string]bool, completed, disabled time.Time) ([]HistoricalAIReview, error) {
	summaries := make([]HistoricalAIReview, 0, len(rows))
	last := completed
	for _, raw := range rows {
		review, ok := decodeAIObject[aiReview](raw, "suggestion_id", "actor", "at", "disposition", "edit")
		at, timeOK := parseTimestamp(review.At)
		if !ok || !timeOK || at.Before(last) || at.After(disabled) || !idPattern.MatchString(review.Actor) ||
			!suggestions[review.SuggestionID] ||
			!((review.Disposition == "edit" && validText(review.Edit, 2000)) ||
				((review.Disposition == "accept" || review.Disposition == "reject") && review.Edit == "")) {
			return nil, integrity("historical AI review lacks valid chronology, attribution or disposition")
		}
		last = at
		summaries = append(summaries, HistoricalAIReview{review.SuggestionID, review.Actor, review.At, review.Disposition})
	}
	return summaries, nil
}

// Require each exact key once. JSON's ordinary struct decoder otherwise
// accepts duplicate, omitted and case-insensitive keys and null primitives.
func decodeAIObject[T any](raw []byte, keys ...string) (value T, ok bool) {
	if !validAIJSONScalars(raw) {
		return value, false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	open, err := decoder.Token()
	if err != nil || open != json.Delim('{') {
		return value, false
	}
	unseen := make(map[string]bool, len(keys))
	for _, key := range keys {
		unseen[key] = true
	}
	for decoder.More() {
		rawKey, err := decoder.Token()
		if err != nil {
			return value, false
		}
		// In an object-key state, a successful Token is always a string.
		key := rawKey.(string)
		if !unseen[key] {
			return value, false
		}
		delete(unseen, key)
		var field json.RawMessage
		if err := decoder.Decode(&field); err != nil || string(field) == "null" {
			return value, false
		}
	}
	// The exact-key pass above does not consume the closing delimiter or
	// trailing bytes. Decode the whole document before accepting its value.
	if len(unseen) != 0 || json.Unmarshal(raw, &value) != nil {
		return value, false
	}
	return value, true
}

func validAIJSONScalars(raw []byte) bool {
	if !utf8.Valid(raw) {
		return false
	}
	for i := 0; i+5 < len(raw); i++ {
		if raw[i] != '\\' {
			continue
		}
		i++
		if raw[i] != 'u' {
			continue
		}
		unit, _ := strconv.ParseUint(string(raw[i+1:i+5]), 16, 16)
		i += 4
		if unit < 0xd800 || unit > 0xdfff {
			continue
		}
		if unit >= 0xdc00 || i+6 >= len(raw) || raw[i+1] != '\\' || raw[i+2] != 'u' {
			return false
		}
		next, _ := strconv.ParseUint(string(raw[i+3:i+7]), 16, 16)
		if next < 0xdc00 || next > 0xdfff {
			return false
		}
		i += 6
	}
	return true
}
