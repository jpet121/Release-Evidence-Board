package board

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
)

const (
	RuleVersion   = "reb-rules/1.5"
	Sufficient    = "SUFFICIENT"
	Blocked       = "BLOCKED"
	NotAssessable = "NOT_ASSESSABLE"
)

type Run struct {
	ID, CandidateID, BehaviorID string
	Closed                      time.Time
	Sequence                    uint64
	Rows                        []SourceRow
	FileHash, ReceiptID         string
	ImportedAt                  time.Time
}

type Reason struct {
	Code, RunID, TestID string
	Attempt             int
}

type RunAssessment struct {
	Run
	Complete, Definitive                  bool
	Planned, Passed, Failed, BestPossible int
	OldestResult                          time.Time
	HasResults                            bool
	Reasons                               []Reason
}

type BehaviorAssessment struct {
	Behavior
	Status, CurrentRunID string
	CurrentTieIncomplete bool
	Planned, Passed      int
	AgeMicroseconds      *int64
	HasCurrentExpiry     bool
	Reasons              []Reason
	Runs                 []RunAssessment
}

type Assessment struct {
	Candidate       Candidate
	Manifest        StoredManifest
	Baseline        ApprovedBaseline
	AssessedAt      time.Time
	LedgerHighWater uint64
	Status          string
	Counts          map[string]int
	Behaviors       []BehaviorAssessment
	EarliestExpiry  time.Time
	HasExpiry       bool
	PredecessorGone bool
}

func (b *Board) Assess(actor, project, id string) (Assessment, error) {
	var assessment Assessment
	err := b.db.View(func(tx *bolt.Tx) error {
		if err := b.authorize(tx, actor, project, read); err != nil {
			return err
		}
		if err := b.assessmentAvailable(tx); err != nil {
			return err
		}
		var err error
		assessment, err = makeAssessment(tx, id, b.now(), nil)
		return err
	})
	return assessment, b.recordIntegrityIssue(actor, err)
}

func (b *Board) assessmentAvailable(tx *bolt.Tx) error {
	if b.integrityHold.Load() {
		return ErrIntegrity
	}
	return assessmentAllowed(tx)
}

func assessmentAllowed(tx *bolt.Tx) error {
	meta := tx.Bucket([]byte(metaBucket))
	if err := validateAssessmentHoldType(meta); err != nil {
		return err
	}
	if len(meta.Get([]byte("assessment_hold"))) != 0 {
		return ErrIntegrity
	}
	return nil
}

func validateAssessmentHoldType(meta *bolt.Bucket) error {
	if meta.Bucket([]byte("assessment_hold")) != nil {
		return integrity("assessment hold has the wrong storage type")
	}
	return nil
}

var errHoldRecorded = errors.New("assessment hold already persisted")

func (b *Board) recordIntegrityIssue(actor string, issue error) error {
	if !errors.Is(issue, ErrIntegrity) || b.integrityHold.Swap(true) {
		return issue
	}
	writeError := b.db.Update(func(tx *bolt.Tx) error {
		meta := tx.Bucket([]byte(metaBucket))
		if meta.Get([]byte("assessment_hold")) != nil {
			return errHoldRecorded
		}
		if err := meta.Put([]byte("assessment_hold"), []byte("integrity")); err != nil {
			return err
		}
		return b.audit(tx, actor, "assessment.integrity_hold", "failure")
	})
	if errors.Is(writeError, errHoldRecorded) {
		writeError = nil
	}
	return errors.Join(issue, writeError)
}

func makeAssessment(tx *bolt.Tx, id string, at time.Time, proposed []Run) (Assessment, error) {
	candidate, manifest, approved, err := verifiedCandidateRecords(tx, id)
	if err != nil {
		return Assessment{}, err
	}
	runs, ledger, err := verifiedCandidateRuns(tx, id)
	if err != nil {
		return Assessment{}, err
	}
	runs = append(runs, proposed...)
	var groups = make(map[string][]Run)
	for _, run := range runs {
		groups[run.BehaviorID] = append(groups[run.BehaviorID], run)
		ledger = max(ledger, run.Sequence)
	}
	assessment := Assessment{
		Candidate: candidate, Manifest: manifest, Baseline: approved, AssessedAt: at,
		LedgerHighWater: ledger, Status: Sufficient,
		Counts: map[string]int{Pass: 0, Fail: 0, Stale: 0, Unknown: 0},
	}
	if candidate.PredecessorID != "" {
		assessment.PredecessorGone = tx.Bucket([]byte(tombstoneBucket)).Get([]byte(candidate.PredecessorID)) != nil
	}
	behaviors := slices.Clone(approved.Baseline.Behaviors)
	slices.SortFunc(behaviors, func(a, b Behavior) int { return strings.Compare(a.ID, b.ID) })
	for _, behavior := range behaviors {
		summary, expiry := EvaluateBehavior(behavior, groups[behavior.ID], at)
		assessment.Behaviors = append(assessment.Behaviors, summary)
		if summary.HasCurrentExpiry && (!assessment.HasExpiry || expiry.Before(assessment.EarliestExpiry)) {
			assessment.EarliestExpiry = expiry
			assessment.HasExpiry = true
		}
		if behavior.Required {
			assessment.Counts[summary.Status]++
			if summary.Status != Pass {
				assessment.Status = Blocked
			}
		}
	}
	return assessment, nil
}

func candidateRuns(tx *bolt.Tx, id string) ([]Run, error) {
	var runs []Run
	prefix := []byte(id + "\x00")
	cursor := tx.Bucket([]byte(candidateRun)).Cursor()
	key, value := cursor.Seek(prefix)
	for bytes.HasPrefix(key, prefix) {
		run, err := getJSON[Run](tx, runBucket, string(value))
		if errors.Is(err, ErrNotFound) || (err == nil && (run.CandidateID != id || string(value) != run.ID ||
			string(key) != id+"\x00"+run.ID)) {
			return nil, fmt.Errorf("%w: run record missing or inconsistent", ErrIntegrity)
		}
		if err != nil {
			return nil, err
		}
		runs = append(runs, run)
		key, value = cursor.Next()
	}
	return runs, nil
}

// EvaluateBehavior evaluates validated immutable runs for one approved behavior.
func EvaluateBehavior(behavior Behavior, runs []Run, now time.Time) (BehaviorAssessment, time.Time) {
	assessment := BehaviorAssessment{Behavior: behavior, Status: Unknown}
	for _, plan := range behavior.Tests {
		assessment.Planned += plan.PlannedAttempts
	}
	if len(runs) == 0 {
		assessment.Reasons = []Reason{{Code: "MISSING_RUN"}}
		return assessment, time.Time{}
	}
	runs = slices.Clone(runs)
	slices.SortFunc(runs, compareSequence)
	latestIndex := 0
	definitive := false
	for i, run := range runs {
		summary := evaluateRun(behavior, run)
		assessment.Runs = append(assessment.Runs, summary)
		if i == 0 || runIsLater(summary, assessment.Runs[latestIndex]) {
			latestIndex = i
		}
		definitive = definitive || summary.Definitive
	}
	current := assessment.Runs[latestIndex]
	for i, run := range assessment.Runs {
		if i == latestIndex || run.Definitive {
			assessment.Reasons = append(assessment.Reasons, run.Reasons...)
		}
		if !current.Complete && run.Complete && run.Closed.Equal(current.Closed) {
			assessment.CurrentTieIncomplete = true
		}
	}
	assessment.CurrentRunID = current.ID
	assessment.Passed = current.Passed
	if current.HasResults {
		microseconds := max(now.UnixMicro()-current.OldestResult.UnixMicro(), 0)
		assessment.AgeMicroseconds = &microseconds
	}
	var expiry time.Time
	stale := false
	if current.Complete {
		expiry = current.OldestResult.Add(time.Duration(behavior.Policy.MaxAgeHours) * time.Hour)
		assessment.HasCurrentExpiry = true
		stale = now.After(expiry)
		if stale {
			assessment.Reasons = append(assessment.Reasons, Reason{Code: "STALE_CURRENT", RunID: current.ID})
		}
	}
	switch {
	case definitive:
		assessment.Status = Fail
	case !current.Complete:
		assessment.Status = Unknown
	case stale:
		assessment.Status = Stale
	default:
		assessment.Status = Pass
	}
	return assessment, expiry
}

func compareSequence(a, b Run) int {
	return cmp.Compare(a.Sequence, b.Sequence)
}

func runIsLater(candidate, current RunAssessment) bool {
	if !candidate.Closed.Equal(current.Closed) {
		return candidate.Closed.After(current.Closed)
	}
	if candidate.Complete != current.Complete {
		return !candidate.Complete
	}
	return candidate.Sequence > current.Sequence
}

type slot struct {
	TestID  string
	Attempt int
}

func evaluateRun(behavior Behavior, run Run) RunAssessment {
	summary := RunAssessment{Run: run, Complete: true}
	seen := make(map[slot]SourceRow)
	for _, row := range run.Rows {
		if row.Kind() == Result {
			number, _ := positiveDecimal(row.Attempt())
			seen[slot{TestID: row.TestID(), Attempt: number}] = row
		}
	}
	if run.Closed.After(run.ImportedAt) {
		summary.Reasons = append(summary.Reasons, Reason{Code: "CLOCK_SKEW", RunID: run.ID})
	}
	for _, test := range behavior.Tests {
		for attempt := 1; attempt <= test.PlannedAttempts; attempt++ {
			summary.Planned++
			row := seen[slot{TestID: test.ID, Attempt: attempt}]
			reason := Reason{RunID: run.ID, TestID: test.ID, Attempt: attempt}
			switch row.Outcome() {
			case Pass:
				summary.Passed++
			case Fail:
				summary.Failed++
				if test.MustPass {
					summary.Definitive = true
					reason.Code = "MUST_PASS_FAILED"
				}
			case "":
				summary.Complete = false
				reason.Code = "MISSING_SLOT"
			default:
				summary.Complete = false
				reason.Code = row.Outcome() + "_RESULT"
			}
			if reason.Code != "" {
				summary.Reasons = append(summary.Reasons, reason)
			}
			if row.Outcome() != "" {
				finished, _ := parseTimestamp(row.FinishedAt())
				if !summary.HasResults || finished.Before(summary.OldestResult) {
					summary.OldestResult = finished
				}
				summary.HasResults = true
			}
		}
	}
	summary.BestPossible = summary.Planned - summary.Failed
	if summary.BestPossible*10000 < behavior.Policy.MinPassBasisPoints*summary.Planned {
		summary.Definitive = true
		summary.Reasons = append(summary.Reasons, Reason{Code: "RATE_BELOW_THRESHOLD", RunID: run.ID})
	}
	return summary
}
