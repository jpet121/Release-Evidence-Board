package board_test

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"release-evidence-board/internal/board"
)

const independentProject = "pilot"
const independentOwner = "owner"
const independentAuthor = "author"
const independentMID = "2026-11-17T12:00:00Z"

type independentBytes struct {
	UTF8Text string `json:"utf8Text"`
	SHA256   string `json:"sha256"`
	Base64   string `json:"base64"`
}

type independentScenario struct {
	ID    string   `json:"id"`
	Title string   `json:"title"`
	Cases []string `json:"acceptanceCases"`
	Setup struct {
		Clock             string                      `json:"clock"`
		ApprovedBaseline  independentBytes            `json:"approvedBaseline"`
		CommittedManifest independentBytes            `json:"committedManifest"`
		SecondaryManifest independentBytes            `json:"secondaryManifest"`
		DescriptorInput   independentBytes            `json:"descriptorInput"`
		Uploads           map[string]independentBytes `json:"uploads"`
	} `json:"setup"`
	Actions []struct {
		Op     string `json:"op"`
		Upload string `json:"upload"`
		At     string `json:"at"`
	} `json:"actions"`
	Expected json.RawMessage `json:"expect"`
}

type independentVector struct {
	ID       string `json:"id"`
	OracleID string `json:"oracleId"`
	GoStage  int    `json:"goStage"`
}

type independentModelDecision struct {
	CandidateStatus string `json:"candidateStatus"`
	Behaviors       []struct {
		ID               string   `json:"id"`
		Status           string   `json:"status"`
		CurrentRun       *string  `json:"currentRun"`
		CurrentSequence  *uint64  `json:"currentSequence"`
		Planned          int      `json:"planned"`
		Passes           int      `json:"passes"`
		DefinitiveRuns   []string `json:"definitiveRuns"`
		CurrentAgeUs     *int64   `json:"currentAgeUs"`
		CompleteExpiryUs *int64   `json:"completeExpiryUs"`
	} `json:"behaviors"`
}

type independentEnvironment struct {
	t         *testing.T
	b         *board.Board
	now       time.Time
	path      string
	candidate board.Candidate
	manifest  board.Manifest
	baseline  board.ApprovedBaseline
	scenario  independentScenario
	previews  map[string]board.Preview
	receipts  map[string]board.Receipt
	stage     int
	vectors   []independentVector
	model     map[string]independentModelDecision
}

func independentRead[T any](t *testing.T, path string) T {
	t.Helper()
	raw, err := os.ReadFile(path)
	independentRequire(t, err)
	return independentDecode[T](t, raw)
}

func independentDecode[T any](t *testing.T, raw []byte) T {
	t.Helper()
	var value T
	independentRequire(t, json.Unmarshal(raw, &value))
	return value
}

func independentRequire(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func independentEqual(t *testing.T, label string, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s: got %#v, want %#v", label, got, want)
	}
}

func independentTime(t *testing.T, value string) time.Time {
	t.Helper()
	if value == "" {
		value = independentMID
	}
	result, err := time.Parse(time.RFC3339Nano, value)
	independentRequire(t, err)
	return result
}

func independentSHA(raw []byte) string {
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

func (f independentBytes) raw(t *testing.T) []byte {
	t.Helper()
	raw := []byte(f.UTF8Text)
	if f.Base64 != "" {
		var err error
		raw, err = base64.StdEncoding.DecodeString(f.Base64)
		independentRequire(t, err)
	}
	if independentSHA(raw) != f.SHA256 {
		t.Fatalf("authored fixture hash mismatch for %s", f.SHA256)
	}
	return raw
}

func independentManifest(t *testing.T, value []byte) board.Manifest {
	t.Helper()
	r := independentDecode[[]string](t, value)
	return board.Manifest{ProductID: r[1], ReleaseID: r[2], ApplicationVersion: r[3], BuildDigest: r[4], PromptDigest: r[5], ModelConfigDigest: r[6], KnowledgeDigest: r[7], EnvironmentID: r[8]}
}

func independentBaseline(t *testing.T, value []byte) board.Baseline {
	t.Helper()
	r := independentDecode[[]json.RawMessage](t, value)
	str := func(raw json.RawMessage) string { return independentDecode[string](t, raw) }
	integer := func(raw json.RawMessage) int { return independentDecode[int](t, raw) }
	boolean := func(raw json.RawMessage) bool { return independentDecode[bool](t, raw) }
	result := board.Baseline{ProductID: str(r[1]), ID: str(r[2]), Revision: integer(r[3])}
	for _, b := range independentDecode[[][]json.RawMessage](t, r[4]) {
		behavior := board.Behavior{ID: str(b[0]), Revision: integer(b[1]), Required: boolean(b[2]), Text: str(b[3]),
			Policy: board.Policy{ID: str(b[4]), Revision: integer(b[5]), MinPassBasisPoints: integer(b[6]), MaxAgeHours: integer(b[7])}}
		for _, p := range independentDecode[[][]json.RawMessage](t, b[8]) {
			behavior.Tests = append(behavior.Tests, board.TestPlan{ID: str(p[0]), ExpectedOutcome: str(p[1]), MustPass: boolean(p[2]), PlannedAttempts: integer(p[3])})
		}
		result.Behaviors = append(result.Behaviors, behavior)
	}
	return result
}

func newIndependentEnvironment(t *testing.T, scenario independentScenario, vectors []independentVector, model map[string]independentModelDecision) *independentEnvironment {
	t.Helper()
	e := &independentEnvironment{t: t, path: filepath.Join(t.TempDir(), "board.db"), scenario: scenario, now: independentTime(t, scenario.Setup.Clock),
		previews: map[string]board.Preview{}, receipts: map[string]board.Receipt{}, vectors: vectors, model: model}
	e.open()
	t.Cleanup(func() { _ = e.b.Close() })
	independentRequire(t, e.b.Bootstrap(independentOwner, "owner-synthetic-password"))
	e.addAccount(independentAuthor, board.Contributor)
	if scenario.Setup.ApprovedBaseline.SHA256 == "" {
		return e
	}
	e.manifest = independentManifest(t, scenario.Setup.CommittedManifest.raw(t))
	m, err := e.manifest.Descriptor()
	independentRequire(t, err)
	independentEqual(t, "manifest digest", m.Digest, scenario.Setup.CommittedManifest.SHA256)
	e.baseline = e.approve(independentBaseline(t, scenario.Setup.ApprovedBaseline.raw(t)), independentAuthor, independentOwner)
	independentEqual(t, "baseline digest", e.baseline.Descriptor.Digest, scenario.Setup.ApprovedBaseline.SHA256)
	e.candidate, _, err = e.b.CreateCandidate(independentAuthor, independentProject, board.CandidateInput{
		Manifest: e.manifest, BaselineDigest: e.baseline.Descriptor.Digest, DisplayVersion: e.manifest.ApplicationVersion})
	independentRequire(t, err)
	return e
}

func (e *independentEnvironment) open() {
	e.t.Helper()
	var err error
	e.b, err = board.Open(e.path, board.Settings{ProjectID: independentProject, ProductID: "DEMO", Clock: func() time.Time { return e.now }})
	independentRequire(e.t, err)
}

func (e *independentEnvironment) addAccount(actor string, role board.Role) {
	e.t.Helper()
	independentRequire(e.t, e.b.SetAccount(independentOwner, actor, actor+"-synthetic-password", []board.Role{role}))
}

func (e *independentEnvironment) approve(input board.Baseline, author, reviewer string) board.ApprovedBaseline {
	e.t.Helper()
	draft, err := e.b.CreateDraft(author, independentProject, input, board.DraftOptions{})
	independentRequire(e.t, err)
	approved, err := e.b.Approve(reviewer, independentProject, draft.ID, draft.Revision)
	independentRequire(e.t, err)
	return approved
}

func (e *independentEnvironment) raw(name string) []byte {
	e.t.Helper()
	input, found := e.scenario.Setup.Uploads[name]
	if !found {
		e.t.Fatalf("no upload %s in oracle %s", name, e.scenario.ID)
	}
	return input.raw(e.t)
}

func (e *independentEnvironment) preview(name, candidate string) board.Preview {
	e.t.Helper()
	preview, err := e.b.PreviewImport(independentAuthor, independentProject, candidate, e.raw(name))
	independentRequire(e.t, err)
	if len(preview.Issues) != 0 || preview.Token == "" {
		e.t.Fatalf("expected valid preview for %s, got %#v", name, preview)
	}
	independentEqual(e.t, "preview hash", preview.Hash, e.scenario.Setup.Uploads[name].SHA256)
	e.previews[name] = preview
	return preview
}

func (e *independentEnvironment) confirm(name string) board.Receipt {
	e.t.Helper()
	receipt, err := e.b.CommitImport(independentAuthor, independentProject, e.previews[name].Token, e.raw(name))
	independentRequire(e.t, err)
	independentEqual(e.t, "committed hash", receipt.Hash, e.scenario.Setup.Uploads[name].SHA256)
	e.receipts[name] = receipt
	return receipt
}

func (e *independentEnvironment) commit(name string) board.Receipt {
	e.t.Helper()
	e.preview(name, e.candidate.ID)
	return e.confirm(name)
}

func (e *independentEnvironment) invalidPreview(name string) board.Preview {
	e.t.Helper()
	preview, err := e.b.PreviewImport(independentAuthor, independentProject, e.candidate.ID, e.raw(name))
	if len(preview.Issues) == 0 && (!errors.Is(err, board.ErrInvalid) && !errors.Is(err, board.ErrConflict)) {
		e.t.Fatalf("wanted an invalid preview for %s, got issues %v and err %v", name, preview.Issues, err)
	}
	return preview
}

func (e *independentEnvironment) assess(candidate, at string) board.Assessment {
	e.t.Helper()
	if at != "" {
		e.now = independentTime(e.t, at)
	}
	a, err := e.b.Assess(independentOwner, independentProject, candidate)
	independentRequire(e.t, err)
	e.stage++
	for _, v := range e.vectors {
		if v.GoStage == e.stage {
			e.compareModel(v, a)
		}
	}
	return a
}

func independentBehavior(t *testing.T, assessment board.Assessment, id string) board.BehaviorAssessment {
	t.Helper()
	for _, behavior := range assessment.Behaviors {
		if behavior.ID == id {
			return behavior
		}
	}
	t.Fatalf("behavior %q missing from assessment", id)
	return board.BehaviorAssessment{}
}

func (e *independentEnvironment) compareModel(v independentVector, actual board.Assessment) {
	e.t.Helper()
	model, found := e.model[v.ID]
	if !found {
		e.t.Fatalf("recorded independent Lean output missing for %s", v.ID)
	}
	e.t.Logf("model differential %s", v.ID)
	independentEqual(e.t, v.ID+" candidate status", actual.Status, model.CandidateStatus)
	independentEqual(e.t, v.ID+" behavior count", len(actual.Behaviors), len(model.Behaviors))
	for _, want := range model.Behaviors {
		b := independentBehavior(e.t, actual, want.ID)
		independentEqual(e.t, want.ID+" status", b.Status, want.Status)
		independentEqual(e.t, want.ID+" planned count", b.Planned, want.Planned)
		independentEqual(e.t, want.ID+" pass count", b.Passed, want.Passes)
		var current *string
		var sequence *uint64
		var expiry *int64
		definitive := []string{}
		for _, run := range b.Runs {
			if run.Definitive {
				definitive = append(definitive, run.ID)
			}
			if run.ID == b.CurrentRunID {
				current = &run.ID
				sequence = &run.Sequence
				if run.Complete && run.HasResults {
					value := run.OldestResult.Add(time.Duration(b.Policy.MaxAgeHours) * time.Hour).UnixMicro()
					expiry = &value
				}
			}
		}
		slices.Sort(definitive)
		slices.Sort(want.DefinitiveRuns)
		independentEqual(e.t, want.ID+" current run", current, want.CurrentRun)
		independentEqual(e.t, want.ID+" current sequence", sequence, want.CurrentSequence)
		independentEqual(e.t, want.ID+" current age", b.AgeMicroseconds, want.CurrentAgeUs)
		independentEqual(e.t, want.ID+" expiry", expiry, want.CompleteExpiryUs)
		independentEqual(e.t, want.ID+" definitive runs", definitive, want.DefinitiveRuns)
		e.t.Logf("%s %s observed status=%s planned=%d passes=%d definitive=%v", v.ID, b.ID, b.Status, b.Planned, b.Passed, definitive)
	}
}

type independentStatus struct {
	CurrentRun        *string  `json:"currentRun"`
	BehaviorStatus    string   `json:"behaviorStatus"`
	CandidateStatus   string   `json:"candidateStatus"`
	ReasonCodes       []string `json:"reasonCodes"`
	MissingSlots      []string `json:"missingSlots"`
	PlannedCount      *int     `json:"plannedCount"`
	PassCount         *int     `json:"passCount"`
	HighWaterMark     *uint64  `json:"highWaterMark"`
	MaxPossiblePasses *int     `json:"maxPossiblePasses"`
	PreviewTieWinner  string   `json:"previewTieWinner"`
}

func (e *independentEnvironment) checkStatus(actual board.Assessment, expected independentStatus) {
	e.t.Helper()
	id := "BEH1"
	b := independentBehavior(e.t, actual, id)
	independentEqual(e.t, "candidate status", actual.Status, expected.CandidateStatus)
	independentEqual(e.t, "behavior status", b.Status, expected.BehaviorStatus)
	if expected.CurrentRun != nil {
		independentEqual(e.t, "current run", b.CurrentRunID, *expected.CurrentRun)
	} else {
		independentEqual(e.t, "missing current run", b.CurrentRunID, "")
	}
	if expected.HighWaterMark != nil {
		independentEqual(e.t, "ledger high-water mark", actual.LedgerHighWater, *expected.HighWaterMark)
	}
	if expected.PlannedCount != nil {
		independentEqual(e.t, "planned count", b.Planned, *expected.PlannedCount)
	}
	if expected.PassCount != nil {
		independentEqual(e.t, "pass count", b.Passed, *expected.PassCount)
	}
	if expected.MaxPossiblePasses != nil {
		found := false
		for _, r := range b.Runs {
			if r.ID == b.CurrentRunID {
				found = true
				independentEqual(e.t, "best possible passes", r.BestPossible, *expected.MaxPossiblePasses)
			}
		}
		if !found {
			e.t.Error("run missing from history")
		}
	}
	var codes []string
	var missing []string
	for _, reason := range b.Reasons {
		codes = append(codes, reason.Code)
		if reason.Code == "MISSING_SLOT" && reason.RunID == b.CurrentRunID {
			missing = append(missing, fmt.Sprintf("%s:%d", reason.TestID, reason.Attempt))
		}
	}
	for _, want := range expected.ReasonCodes {
		if !slices.Contains(codes, want) {
			e.t.Errorf("required reason %s absent from %v", want, codes)
		}
	}
	if expected.PreviewTieWinner != "" {
		independentEqual(e.t, "incomplete finish-time tie", b.CurrentTieIncomplete, true)
	}
	if expected.MissingSlots != nil {
		slices.Sort(missing)
		slices.Sort(expected.MissingSlots)
		if !slices.Equal(missing, expected.MissingSlots) {
			e.t.Errorf("missing slots: got %v, want %v", missing, expected.MissingSlots)
		}
	}
}

func TestIndependentAcceptanceOracles(t *testing.T) {
	root := filepath.Join("..", "..", "compliance")
	fixtures := independentRead[struct{ Scenarios []independentScenario }](t, filepath.Join(root, "independent-oracles.json"))
	vectors := independentRead[struct{ Vectors []independentVector }](t, filepath.Join(root, "models", "model-vectors.json"))
	audit := independentRead[struct {
		Result                         string
		CheckedAndExecutedSourceSHA256 string
		VectorTableSHA256              string
		Checks                         []struct {
			ID, Result string
			Actual     independentModelDecision
		}
	}](t, filepath.Join(root, "models", "model-execution.json"))
	if audit.Result != "PASS" {
		t.Fatal("independent model execution record does not pass")
	}
	for filename, want := range map[string]string{"IndependentOracle.lean": audit.CheckedAndExecutedSourceSHA256, "model-vectors.json": audit.VectorTableSHA256} {
		raw, err := os.ReadFile(filepath.Join(root, "models", filename))
		independentRequire(t, err)
		if independentSHA(raw) != want {
			t.Fatalf("model execution record does not bind current %s", filename)
		}
	}
	model := map[string]independentModelDecision{}
	for _, c := range audit.Checks {
		if c.Result == "PASS" {
			model[c.ID] = c.Actual
		}
	}
	byOracle := map[string][]independentVector{}
	for _, v := range vectors.Vectors {
		byOracle[v.OracleID] = append(byOracle[v.OracleID], v)
	}
	for _, scenario := range fixtures.Scenarios {
		t.Run(scenario.ID, func(t *testing.T) {
			t.Logf("%s [%s]", scenario.Title, strings.Join(scenario.Cases, ", "))
			switch scenario.ID {
			case "IOR-039":
				t.Skip("UNMEASURED BY BOARD API: AI-off journeys need browser navigation and network observation")
			case "IOR-041":
				t.Skip("CONDITIONAL DISABLED: provider adapter is absent")
			}
			e := newIndependentEnvironment(t, scenario, byOracle[scenario.ID], model)
			e.run()
			for _, v := range e.vectors {
				if v.GoStage > e.stage {
					t.Errorf("independent vector stage %d was not executed for %s", v.GoStage, v.ID)
				}
			}
			if !t.Failed() {
				e.recordScopedOracleAcceptance()
			}
		})
	}
}

func (e *independentEnvironment) run() {
	t := e.t
	switch e.scenario.ID {
	case "IOR-001", "IOR-003":
		e.checkRepeat()
	case "IOR-002":
		e.checkIncidentReplay()
	case "IOR-024", "IOR-025":
		preview := e.invalidPreview("future")
		if preview.Token != "" || len(preview.Issues) != 1 || preview.Issues[0].Code != "CLOCK_SKEW_LIMIT" || preview.Issues[0].Column != "run_finished_at" {
			t.Fatalf("past clock allowance yielded unexpected admission or diagnostic: %+v", preview.Issues)
		}
		a := e.assess(e.candidate.ID, "")
		independentEqual(t, "no new sequences", a.LedgerHighWater, uint64(0))
		independentEqual(t, "no rejected runs in history", len(independentBehavior(t, a, "BEH1").Runs), 0)
	case "IOR-026":
		e.commit("two")
		first := e.assess(e.candidate.ID, "2026-11-18T11:00:00Z")
		independentEqual(t, "fresh at equality", first.Status, "SUFFICIENT")
		independentEqual(t, "behavior at equality", independentBehavior(t, first, "BEH1").Status, "PASS")
		e.now = independentTime(t, "2026-11-18T11:00:00.000001Z")
		report, err := e.b.Freeze(independentOwner, independentProject, e.candidate.ID)
		independentRequire(t, err)
		independentEqual(t, "freeze uses current time", report.AssessedAt, e.now)
		independentEqual(t, "frozen freshness", report.Status, "BLOCKED")
		independentEqual(t, "historical expiry", report.EarliestExpiry.Format(time.RFC3339Nano), "2026-11-18T11:00:00Z")
		_, html, err := e.b.ReportHTML(independentOwner, independentProject, report.ID)
		independentRequire(t, err)
		page := string(html)
		if !strings.Contains(page, "Historical assessment · BLOCKED") || !strings.Contains(page, "STALE") || !strings.Contains(page, "2026-11-18T11:00:00.000001Z") || !strings.Contains(page, "2026-11-18T11:00:00Z") || strings.Contains(page, "Historical assessment · SUFFICIENT") {
			t.Error("frozen report missing stale status or exact assessment time")
		}
		final := e.assess(e.candidate.ID, "")
		stale := independentBehavior(t, final, "BEH1")
		independentEqual(t, "computed stale behavior", stale.Status, "STALE")
		if stale.AgeMicroseconds == nil || *stale.AgeMicroseconds != 86400000001 {
			t.Error("oldest result did not set the exact stale age")
		}
	case "IOR-027", "IOR-028":
		e.checkApproval()
	case "IOR-029", "IOR-030":
		e.checkIdentity()
	case "IOR-031", "IOR-032":
		e.checkDescriptors()
	case "IOR-033", "IOR-034", "IOR-035", "IOR-036", "IOR-043", "IOR-044", "IOR-045", "IOR-051":
		e.checkFrozen()
	case "IOR-037":
		first := e.commit("one")
		repeat, err := e.b.CommitImport(independentAuthor, independentProject, e.previews["one"].Token, e.raw("one"))
		independentRequire(t, err)
		independentEqual(t, "exact RUN_ONLY retry", repeat.ID, first.ID)
		independentEqual(t, "exact retry retained original ledger", repeat.LedgerHighWater, uint64(1))
		quoted := e.commit("quoted")
		independentEqual(t, "alternate quoting is no-op", len(quoted.AddedRuns), 0)
		independentEqual(t, "no additional ledger sequence", quoted.LedgerHighWater, uint64(1))
		independentEqual(t, "quotes no-op refers to original receipt", quoted.PreviousReceipts, []string{first.ID})
		rejected := e.invalidPreview("append")
		if rejected.Token != "" || len(rejected.Issues) != 1 || rejected.Issues[0].Column != "run_id" {
			t.Fatalf("later RESULT import did not reject reuse of retained RUN_ONLY: %+v", rejected.Issues)
		}
		a := e.assess(e.candidate.ID, "")
		runs := independentBehavior(t, a, "BEH1").Runs
		if len(runs) != 1 || len(runs[0].Rows) != 1 || runs[0].Rows[0].Kind() != "RUN_ONLY" {
			t.Error("committed RUN_ONLY declaration changed after the rejected append")
		}
	case "IOR-038":
		e.checkCorruptBackup()
	case "IOR-040":
		first := e.commit("b")
		preview := e.invalidPreview("bad")
		if preview.Token != "" || !slices.ContainsFunc(preview.Issues, func(d board.Diagnostic) bool {
			return d.Code == "ENCODING" && d.Column == "result_id" && d.LogicalRow == 2 && d.PhysicalLine == 2
		}) {
			t.Fatalf("malformed UTF-8 file returned a staged token or unexpected diagnostics: %+v", preview.Issues)
		}
		independentRequire(t, e.b.Close())
		e.open()
		a := e.assess(e.candidate.ID, "")
		independentEqual(t, "retained ledger", a.LedgerHighWater, uint64(1))
		runs := independentBehavior(t, a, "BEH1").Runs
		if len(runs) != 1 {
			t.Fatalf("restart retained %d runs, want 1", len(runs))
		}
		independentEqual(t, "retained link to receipt", runs[0].ReceiptID, first.ID)
	case "IOR-042":
		e.commit("mixed")
		a := e.assess(e.candidate.ID, "")
		independentEqual(t, "required passes", a.Counts["PASS"], 1)
		independentEqual(t, "required failures", a.Counts["FAIL"], 0)
		independentEqual(t, "only required decisions govern summary", a.Status, "SUFFICIENT")
		optional, required := independentBehavior(t, a, "OPTIONAL"), independentBehavior(t, a, "BEH1")
		independentEqual(t, "optional false", optional.Required, false)
		independentEqual(t, "required true", required.Required, true)
		independentEqual(t, "optional failure visible", optional.Status, "FAIL")
		independentEqual(t, "only required behavior passed", required.Status, "PASS")
		if !slices.ContainsFunc(optional.Reasons, func(reason board.Reason) bool { return reason.Code == "RATE_BELOW_THRESHOLD" }) {
			t.Error("optional behavior omitted its actual rate blocker")
		}
	case "IOR-046", "IOR-047", "IOR-048", "IOR-049", "IOR-050", "IOR-054":
		e.checkDetachedIntegrity()
	case "IOR-052":
		e.checkRenamedDerivedApproval()
	case "IOR-053":
		e.checkMixedPreview()
	case "IOR-055":
		e.checkRacingFirstCommit()
	case "IOR-056":
		e.checkAbandonedPreviewRestart()
	case "IOR-057", "IOR-058":
		e.checkBadBackupSidecar()
	case "IOR-059", "IOR-060", "IOR-061", "IOR-062", "IOR-063", "IOR-064", "IOR-065", "IOR-079":
		e.checkRetainedKeyOrReceipt()
	case "IOR-066", "IOR-067":
		e.checkFrozenCompanionMetadata()
	case "IOR-068":
		e.checkPreviewHoldRace()
	case "IOR-069":
		e.checkMixedLedger()
	case "IOR-070", "IOR-071":
		e.checkOptionalizedAlias()
	case "IOR-072", "IOR-073", "IOR-074", "IOR-075", "IOR-076", "IOR-077", "IOR-078":
		e.checkCanonicalBoundaries()
	case "IOR-080", "IOR-081", "IOR-082", "IOR-083", "IOR-084", "IOR-089":
		e.checkPurgeOwnershipFault()
	case "IOR-085", "IOR-086", "IOR-087", "IOR-088":
		e.checkMaterialCustody()
	case "IOR-090", "IOR-091", "IOR-092":
		e.checkDraftAuthority()
	default:
		e.runEvidenceActions()
	}
}

func (e *independentEnvironment) runEvidenceActions() {
	e.t.Helper()
	for _, a := range e.scenario.Actions {
		switch a.Op {
		case "preview":
			e.preview(a.Upload, e.candidate.ID)
		case "confirm":
			e.confirm(a.Upload)
		case "assess":
			assessment := e.assess(e.candidate.ID, a.At)
			var expect independentStatus
			if e.scenario.ID == "IOR-016" || e.scenario.ID == "IOR-017" || e.scenario.ID == "IOR-018" {
				parts := independentDecode[map[string]independentStatus](e.t, e.scenario.Expected)
				if e.stage == 1 {
					expect = parts["partial"]
				} else {
					expect = parts["afterPassingRerun"]
				}
			} else {
				expect = independentDecode[independentStatus](e.t, e.scenario.Expected)
			}
			e.checkStatus(assessment, expect)
			if expect.PreviewTieWinner != "" {
				p := e.previews[e.scenario.Actions[2].Upload]
				if len(p.Projected) == 0 {
					e.t.Error("preview lacks projected tie outcome")
				} else {
					b := independentBehavior(e.t, p.Projected[0], "BEH1")
					independentEqual(e.t, "preview tie winner", b.CurrentRunID, expect.PreviewTieWinner)
					independentEqual(e.t, "preview identifies incomplete tie break", b.CurrentTieIncomplete, true)
				}
			}
		default:
			e.t.Fatalf("unexpected action %s", a.Op)
		}
	}
}

func (e *independentEnvironment) checkRepeat() {
	t := e.t
	first := e.commit("a")
	e.commit("b")
	if e.previews["a"].Revision >= e.previews["b"].Revision {
		t.Fatal("exact retry control did not have an intervening revision")
	}
	if e.scenario.ID == "IOR-003" {
		e.addAccount("viewer", board.Viewer)
		_, changed := e.b.CommitImport(independentAuthor, independentProject, e.previews["a"].Token, e.raw("b"))
		if changed == nil {
			t.Error("changed bytes accepted for existing committed import")
		}
		for _, actor := range []string{"outside-project", "viewer"} {
			_, err := e.b.CommitImport(actor, independentProject, e.previews["a"].Token, e.raw("a"))
			independentEqual(t, actor+" exact replay denied", errors.Is(err, board.ErrForbidden), true)
		}
	} else {
		before, err := e.b.Audit(independentOwner, independentProject)
		independentRequire(t, err)
		beforePhysical := detachedPhysicalState(t, e.path)
		var wg sync.WaitGroup
		raw := e.raw("a")
		for range 2 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				receipt, err := e.b.CommitImport(independentAuthor, independentProject, e.previews["a"].Token, raw)
				if err != nil {
					t.Error(err)
					return
				}
				independentEqual(t, "concurrent exact replay", receipt.ID, first.ID)
			}()
		}
		wg.Wait()
		after, err := e.b.Audit(independentOwner, independentProject)
		independentRequire(t, err)
		independentEqual(t, "exact replay added no audit mutation", len(after), len(before))
		independentEqual(t, "concurrent exact replay leaves durable metadata and bytes unchanged", detachedPhysicalState(t, e.path), beforePhysical)
	}
	a := e.assess(e.candidate.ID, "")
	independentEqual(t, "still exactly two product sequences", a.LedgerHighWater, uint64(2))
	runs := independentBehavior(t, a, "BEH1").Runs
	independentEqual(t, "still exactly two runs", len(runs), 2)
	for _, run := range runs {
		independentEqual(t, "still exactly one result per retained run", len(run.Rows), 1)
	}
}

func (e *independentEnvironment) checkIncidentReplay() {
	t := e.t
	first := e.commit("a")
	e.commit("b")
	if e.previews["a"].Revision >= e.previews["b"].Revision {
		t.Fatal("fixture did not create a stale original preview")
	}
	independentRequire(t, e.b.SuspendAssessments(independentOwner, independentProject))
	before, err := e.b.Audit(independentOwner, independentProject)
	independentRequire(t, err)
	beforePhysical := detachedPhysicalState(t, e.path)
	replayed, err := e.b.CommitImport(independentAuthor, independentProject, e.previews["a"].Token, e.raw("a"))
	independentRequire(t, err)
	independentEqual(t, "latched retry returns original receipt", replayed, first)
	after, err := e.b.Audit(independentOwner, independentProject)
	independentRequire(t, err)
	independentEqual(t, "latched retry added no audit mutation", len(after), len(before))
	independentEqual(t, "latched replay leaves durable metadata and bytes unchanged", detachedPhysicalState(t, e.path), beforePhysical)
	original, err := e.b.SourceCSV(independentOwner, independentProject, replayed.ID)
	independentRequire(t, err)
	independentEqual(t, "latched replay retains original bytes", original, e.raw("a"))
	_, err = e.b.Freeze(independentOwner, independentProject, e.candidate.ID)
	independentEqual(t, "latch prevents new report", errors.Is(err, board.ErrIntegrity), true)
}

func (e *independentEnvironment) checkRacingFirstCommit() {
	t := e.t
	first := e.preview("a", e.candidate.ID)
	second := e.preview("a", e.candidate.ID)
	independentEqual(t, "both previews bind the pre-import revision", second.Revision, first.Revision)
	before := detachedPhysicalState(t, e.path)
	start := make(chan struct{})
	type result struct {
		receipt board.Receipt
		err     error
	}
	results := make(chan result, 2)
	raw := e.raw("a")
	for _, token := range []string{first.Token, second.Token} {
		go func(token string) {
			<-start
			receipt, err := e.b.CommitImport(independentAuthor, independentProject, token, raw)
			results <- result{receipt: receipt, err: err}
		}(token)
	}
	close(start)
	firstResult, secondResult := <-results, <-results
	independentRequire(t, firstResult.err)
	independentRequire(t, secondResult.err)
	independentEqual(t, "fresh racing confirms both identify the sole committed receipt", secondResult.receipt, firstResult.receipt)
	after := detachedPhysicalState(t, e.path)
	independentEqual(t, "one material transaction across both racing confirms", after.transaction-before.transaction, 1)
	a := e.assess(e.candidate.ID, "")
	independentEqual(t, "exactly one run sequence", a.LedgerHighWater, uint64(1))
	independentEqual(t, "exactly one result row", len(independentBehavior(t, a, "BEH1").Runs[0].Rows), 1)
}

func (e *independentEnvironment) checkApproval() {
	t := e.t
	e.addAccount("reducer", board.Reviewer)
	e.addAccount("independent", board.Reviewer)
	input := independentBaseline(t, e.scenario.Setup.ApprovedBaseline.raw(t))
	input.ID = "INDEPENDENT"
	input.Behaviors[0].Policy.ID = "NEWPOL"
	input.Behaviors[0].Policy.MinPassBasisPoints = 9000
	if e.scenario.ID == "IOR-028" {
		input.ID = "DOWN"
		input.Behaviors[0].Policy.ID = "DIFFERENT"
		input.Behaviors[0].Policy.MinPassBasisPoints = 9900
	}
	draft, err := e.b.CreateDraft("reducer", independentProject, input, board.DraftOptions{})
	independentRequire(t, err)
	e.commit("failed")
	if e.scenario.ID == "IOR-027" {
		e.commit("passing")
		e.assess(e.candidate.ID, "2026-11-18T12:00:00Z")
		_, err = e.b.Approve("reducer", independentProject, draft.ID, draft.Revision)
		independentEqual(t, "reducing reviewer denied", errors.Is(err, board.ErrForbidden), true)
		e.now = independentTime(t, "2026-11-18T12:00:01Z")
		approved, err := e.b.Approve("independent", independentProject, draft.ID, draft.Revision)
		independentRequire(t, err)
		independentEqual(t, "approval actor", approved.Approver, "independent")
		independentEqual(t, "approval time", approved.ApprovedAt, e.now)
		if len(approved.Thresholds) != 1 {
			t.Fatalf("D20 approval records: %#v", approved.Thresholds)
		}
		want := board.ThresholdChange{CandidateID: e.candidate.ID, BehaviorID: "BEH1", Before: 10000, After: 9000, ReducingActors: []string{"reducer"}}
		independentEqual(t, "D20 approval record", approved.Thresholds[0], want)
		old, err := e.b.Assess(independentOwner, independentProject, e.candidate.ID)
		independentRequire(t, err)
		run := "passing"
		e.checkStatus(old, independentStatus{CandidateStatus: "BLOCKED", BehaviorStatus: "FAIL", CurrentRun: &run, ReasonCodes: []string{"MUST_PASS_FAILED", "RATE_BELOW_THRESHOLD", "STALE_CURRENT"}})
		successor, _, err := e.b.CreateCandidate(independentAuthor, independentProject, board.CandidateInput{Manifest: e.manifest, BaselineDigest: approved.Descriptor.Digest, PredecessorID: e.candidate.ID, Correction: "Threshold lowered with independent approval"})
		independentRequire(t, err)
		a := e.assess(successor.ID, "2026-11-18T12:00:02Z")
		e.checkStatus(a, independentStatus{CandidateStatus: "BLOCKED", BehaviorStatus: "UNKNOWN", ReasonCodes: []string{"MISSING_RUN"}})
		return
	}
	t.Log("approval interleaving uses committed failure before exact draft approval; in-flight pre-transaction barrier remains unmeasured")
	_, err = e.b.Approve("reducer", independentProject, draft.ID, draft.Revision)
	independentEqual(t, "approval sees failure after draft creation", errors.Is(err, board.ErrForbidden), true)
	e.addAccount("third", board.Reviewer)
	input.ID = "COPY"
	copied, err := e.b.CreateDraft("independent", independentProject, input, board.DraftOptions{FromDraft: draft.ID})
	independentRequire(t, err)
	input.Behaviors[0].Policy.MinPassBasisPoints = 9800
	changed, err := e.b.SaveDraft("independent", independentProject, copied.ID, copied.Revision, input)
	independentRequire(t, err)
	_, err = e.b.Approve("third", independentProject, copied.ID, copied.Revision)
	independentEqual(t, "stale copied draft cannot be approved", errors.Is(err, board.ErrConflict), true)
	for _, actor := range []string{"reducer", "independent"} {
		_, err = e.b.Approve(actor, independentProject, changed.ID, changed.Revision)
		independentEqual(t, actor+" cannot approve copied reduction", errors.Is(err, board.ErrForbidden), true)
	}
	approved, err := e.b.Approve("third", independentProject, changed.ID, changed.Revision)
	independentRequire(t, err)
	if len(approved.Thresholds) != 1 {
		t.Fatalf("D20 copied approval records: %#v", approved.Thresholds)
	}
	independentEqual(t, "copy preserves reduction by two actors", approved.Thresholds[0].ReducingActors, []string{"independent", "reducer"})
}

func (e *independentEnvironment) checkIdentity() {
	t := e.t
	if e.scenario.ID == "IOR-029" {
		exact, reused, err := e.b.CreateCandidate(independentAuthor, independentProject, board.CandidateInput{Manifest: e.manifest, BaselineDigest: e.baseline.Descriptor.Digest})
		independentRequire(t, err)
		independentEqual(t, "exact pair reused", reused, true)
		independentEqual(t, "exact pair uses original assessment", exact.ID, e.candidate.ID)
		label := independentBaseline(t, e.scenario.Setup.ApprovedBaseline.raw(t))
		label.ID, label.Revision = "LABEL", 2
		label.Behaviors[0].ID, label.Behaviors[0].Revision = "BEH-LABEL", 2
		label.Behaviors[0].Policy.ID, label.Behaviors[0].Policy.Revision = "POL-LABEL", 2
		label.Behaviors[0].Tests[0].ID = "T-LABEL"
		approved := e.approve(label, independentAuthor, independentOwner)
		manifest := e.manifest
		manifest.ReleaseID, manifest.ApplicationVersion = "R2", "2.0"
		actual, reuse, err := e.b.CreateCandidate(independentAuthor, independentProject, board.CandidateInput{Manifest: manifest, BaselineDigest: approved.Descriptor.Digest})
		independentRequire(t, err)
		independentEqual(t, "semantic equivalent pair reused", reuse, true)
		independentEqual(t, "original assessment reused", actual.ID, e.candidate.ID)
		all, err := e.b.Candidates(independentOwner, independentProject)
		independentRequire(t, err)
		if len(all) != 1 || all[0].ID != e.candidate.ID {
			t.Fatal("equivalent input created an extra candidate")
		}
		e.commit("failed")
		e.commit("passing")
		a := e.assess(e.candidate.ID, "")
		independentEqual(t, "cannot bypass later failure", a.Status, "BLOCKED")
		return
	}
	e.commit("failed")
	manifest := e.manifest
	manifest.ReleaseID, manifest.EnvironmentID = "R2", "E2"
	successor, reused, err := e.b.CreateCandidate(independentAuthor, independentProject, board.CandidateInput{Manifest: manifest, BaselineDigest: e.baseline.Descriptor.Digest, PredecessorID: e.candidate.ID, Correction: "E1 -> E2"})
	independentRequire(t, err)
	stored, immutable, _, err := e.b.Candidate(independentAuthor, independentProject, successor.ID)
	independentRequire(t, err)
	if reused || stored.ID == e.candidate.ID || stored.PredecessorID != e.candidate.ID || len(stored.Changes) != 1 || immutable.Manifest.EnvironmentID != "E2" {
		t.Fatalf("successor failed to record predecessor and changed immutable environment: %+v", stored)
	}
	p := e.preview("oldPass", successor.ID)
	if !slices.Contains(p.HistoricalCandidates, e.candidate.ID) {
		t.Error("historical mismatch warning is absent")
	}
	receipt := e.confirm("oldPass")
	independentEqual(t, "historical upload is stored against original identity only", receipt.CandidateIDs, []string{e.candidate.ID})
	before := e.assess(successor.ID, "")
	e.checkStatus(before, independentStatus{CandidateStatus: "BLOCKED", BehaviorStatus: "UNKNOWN", ReasonCodes: []string{"MISSING_RUN"}})
	independentEqual(t, "historical source did not create a successor run", len(independentBehavior(t, before, "BEH1").Runs), 0)
	e.preview("newPass", successor.ID)
	e.confirm("newPass")
	a := e.assess(successor.ID, "")
	current := "corrected"
	e.checkStatus(a, independentStatus{CandidateStatus: "SUFFICIENT", BehaviorStatus: "PASS", CurrentRun: &current})
	old := e.assess(e.candidate.ID, "")
	current = "passing"
	e.checkStatus(old, independentStatus{CandidateStatus: "BLOCKED", BehaviorStatus: "FAIL", CurrentRun: &current, ReasonCodes: []string{"MUST_PASS_FAILED", "RATE_BELOW_THRESHOLD"}})

	// Keep the same release identity in this separate derivation so the UI/API
	// can link its own predecessor without having received an explicit ID.
	autoManifest := e.manifest
	autoManifest.EnvironmentID = "E3"
	automatic, reused, err := e.b.CreateCandidate(independentAuthor, independentProject, board.CandidateInput{Manifest: autoManifest, BaselineDigest: e.baseline.Descriptor.Digest, Correction: "E1 -> E3"})
	independentRequire(t, err)
	readback, _, _, err := e.b.Candidate(independentAuthor, independentProject, automatic.ID)
	independentRequire(t, err)
	if reused || readback.PredecessorID != e.candidate.ID || !slices.Equal(readback.Changes, []string{"Environment or harness revision"}) {
		t.Fatalf("automatic same-release derivation did not persist its prior immutable identity: %+v", readback)
	}
}

func (e *independentEnvironment) checkRenamedDerivedApproval() {
	t := e.t
	e.commit("history")
	before := e.assess(e.candidate.ID, "")
	independentEqual(t, "prior threshold and failure block candidate", before.Status, "BLOCKED")
	e.addAccount("reducer", board.Reviewer)
	e.addAccount("independent", board.Reviewer)
	contents := independentBaseline(t, e.scenario.Setup.ApprovedBaseline.raw(t))
	draft, err := e.b.CreateDraft("reducer", independentProject, contents, board.DraftOptions{FromBaseline: e.baseline.Descriptor.Digest})
	independentRequire(t, err)
	changed := draft.Baseline
	behavior := &changed.Behaviors[0]
	behavior.ID = "BEH-RENAMED"
	behavior.Tests[0].ID = "T1-RENAMED"
	behavior.Tests[1].ID = "T2-RENAMED"
	behavior.Policy.MinPassBasisPoints = 5000
	_, err = e.b.SaveDraft("reducer", independentProject, draft.ID, draft.Revision, changed)
	independentEqual(t, "inherited ID rename is rejected", errors.Is(err, board.ErrInvalid), true)
	unchanged, err := e.b.Draft("reducer", independentProject, draft.ID)
	independentRequire(t, err)
	independentEqual(t, "rename rejection preserves the exact draft revision", unchanged.Revision, draft.Revision)
	independentEqual(t, "rename rejection preserves the inherited behavior ID", unchanged.Baseline.Behaviors[0].ID, "BEH1")
	independentEqual(t, "rename rejection preserves the threshold", unchanged.Baseline.Behaviors[0].Policy.MinPassBasisPoints, 10000)
	allowedEdit := unchanged.Baseline
	allowedEdit.Behaviors[0].Policy.MinPassBasisPoints = 5000
	edited, err := e.b.SaveDraft("reducer", independentProject, unchanged.ID, unchanged.Revision, allowedEdit)
	independentRequire(t, err)
	_, err = e.b.Approve("reducer", independentProject, edited.ID, edited.Revision)
	independentEqual(t, "derived edit remains subject to D20", errors.Is(err, board.ErrForbidden), true)
	approved, err := e.b.Approve("independent", independentProject, edited.ID, edited.Revision)
	independentRequire(t, err)
	appointment := board.ThresholdChange{CandidateID: e.candidate.ID, BehaviorID: "BEH1", Before: 10000, After: 5000, ReducingActors: []string{"reducer"}}
	independentEqual(t, "D20 approval records stable failed behavior identity and reviewer separation", approved.Thresholds, []board.ThresholdChange{appointment})
	successor, _, err := e.b.CreateCandidate(independentAuthor, independentProject, board.CandidateInput{Manifest: e.manifest,
		BaselineDigest: approved.Descriptor.Digest, PredecessorID: e.candidate.ID, Correction: "Independently approved threshold change"})
	independentRequire(t, err)
	newStatus := e.assess(successor.ID, "")
	independentEqual(t, "successor needs fresh matching evidence", newStatus.Status, "BLOCKED")
	independentEqual(t, "successor behavior has UNKNOWN", independentBehavior(t, newStatus, "BEH1").Status, "UNKNOWN")
	independentEqual(t, "successor cannot reset original failure", e.assess(e.candidate.ID, "").Status, "BLOCKED")
}

func (e *independentEnvironment) checkMixedPreview() {
	t := e.t
	manifest := independentManifest(t, e.scenario.Setup.SecondaryManifest.raw(t))
	other, _, err := e.b.CreateCandidate(independentAuthor, independentProject, board.CandidateInput{Manifest: manifest,
		BaselineDigest: e.baseline.Descriptor.Digest, DisplayVersion: "1.0.0"})
	independentRequire(t, err)
	e.preview("currentB", other.ID)
	e.confirm("currentB")
	p := e.preview("olderB", e.candidate.ID)
	independentEqual(t, "historical candidate warning references actual pair", p.HistoricalCandidates, []string{other.ID})
	if len(p.Projected) != 2 {
		t.Fatalf("wanted projections for both same-version candidates, got %d", len(p.Projected))
	}
	statuses := map[string]string{}
	for _, a := range p.Projected {
		statuses[a.Candidate.ID] = a.Status
		independentEqual(t, "projected display versions deliberately coincide", a.Candidate.DisplayVersion, "1.0.0")
		independentEqual(t, "projected baseline revision", a.Baseline.Baseline.Revision, 1)
	}
	independentEqual(t, "status keyed by exact candidate identity", statuses, map[string]string{e.candidate.ID: "BLOCKED", other.ID: "SUFFICIENT"})
	ordering := p.Ordering[other.ID]["BEH1"]
	independentEqual(t, "previous selected run", ordering.PreviousRunID, "current-b")
	independentEqual(t, "projected selected run stays current", ordering.ProjectedRunID, "current-b")
	observedGaps := map[string][]string{}
	for _, r := range ordering.Incoming {
		for _, reason := range r.Reasons {
			if reason.Code == "MISSING_SLOT" {
				observedGaps[r.ID] = append(observedGaps[r.ID], fmt.Sprintf("%s:%d", reason.TestID, reason.Attempt))
			}
		}
		slices.Sort(observedGaps[r.ID])
	}
	independentEqual(t, "projected historical RUN_ONLY and partial run preserve their missing slots", observedGaps,
		map[string][]string{"older-empty": {"T1:1", "T2:1"}, "older-partial": {"T2:1"}})
	independentEqual(t, "projection cannot change committed ledger", e.assess(other.ID, "").LedgerHighWater, uint64(1))
}

func (e *independentEnvironment) checkDescriptors() {
	t := e.t
	if e.scenario.ID == "IOR-031" {
		expect := independentDecode[struct {
			Manifest, Baseline independentBytes
			Special            []independentBytes
		}](t, e.scenario.Expected)
		m, err := independentManifest(t, expect.Manifest.raw(t)).Descriptor()
		independentRequire(t, err)
		independentEqual(t, "literal manifest bytes", m.Text, expect.Manifest.UTF8Text)
		independentEqual(t, "literal manifest digest", m.Digest, expect.Manifest.SHA256)
		for _, v := range append([]independentBytes{expect.Baseline}, expect.Special...) {
			descriptor, err := independentBaseline(t, v.raw(t)).Descriptor()
			independentRequire(t, err)
			independentEqual(t, "literal baseline bytes", descriptor.Text, v.UTF8Text)
			independentEqual(t, "literal baseline digest", descriptor.Digest, v.SHA256)
		}
		return
	}
	expect := independentDecode[struct{ BothValidOutputs independentBytes }](t, e.scenario.Expected).BothValidOutputs
	ordered := independentBaseline(t, expect.raw(t))
	for range 2 {
		got, err := ordered.Descriptor()
		independentRequire(t, err)
		independentEqual(t, "ASCII sorted descriptor", got.Text, expect.UTF8Text)
		independentEqual(t, "sorted descriptor digest", got.Digest, expect.SHA256)
		slices.Reverse(ordered.Behaviors)
		for i := range ordered.Behaviors {
			slices.Reverse(ordered.Behaviors[i].Tests)
		}
	}
	t.Log("UNMEASURED AT TYPED SEAM: raw float, object, null and lone surrogate need untyped ingress checks")
}

func (e *independentEnvironment) checkFrozen() {
	t := e.t
	var initial board.Assessment
	for _, a := range e.scenario.Actions {
		if a.Op == "preview" && a.Upload != "later" {
			e.preview(a.Upload, e.candidate.ID)
		}
		if a.Op == "confirm" && a.Upload != "later" {
			e.confirm(a.Upload)
		}
		if a.Op == "assess" {
			e.assess(e.candidate.ID, a.At)
		}
		if a.Op == "freezeReport" {
			initial = e.assess(e.candidate.ID, a.At)
			break
		}
	}
	r, err := e.b.Freeze(independentOwner, independentProject, e.candidate.ID)
	independentRequire(t, err)
	_, raw, err := e.b.ReportHTML(independentOwner, independentProject, r.ID)
	independentRequire(t, err)
	independentEqual(t, "separately stored immutable HTML hash", r.SHA256, independentSHA(raw))
	independentEqual(t, "frozen snapshot ledger", r.LedgerHighWater, initial.LedgerHighWater)
	if e.scenario.ID == "IOR-051" {
		independentEqual(t, "zero-instant expiry is present in the live assessment", initial.HasExpiry, true)
		independentEqual(t, "zero-instant expiry is present in the report", r.HasExpiry, true)
		independentEqual(t, "exact year-one expiry value", r.EarliestExpiry.Format(time.RFC3339Nano), "0001-01-01T00:00:00Z")
	}
	for name, receipt := range e.receipts {
		original, err := e.b.SourceCSV(independentOwner, independentProject, receipt.ID)
		independentRequire(t, err)
		independentEqual(t, "protected original bytes for "+name, original, e.raw(name))
	}
	e.checkOfflineHTML(raw)
	if e.scenario.ID == "IOR-036" {
		if path := os.Getenv("REB_INDEPENDENT_REPORT_SAMPLE"); path != "" {
			independentRequire(t, os.WriteFile(path, raw, 0600))
		}
		e.commit("later")
		_, again, err := e.b.ReportHTML(independentOwner, independentProject, r.ID)
		independentRequire(t, err)
		independentEqual(t, "previous report HTML unchanged", again, raw)
		for _, value := range []string{"B-FAIL", "B-PASS", "B-STALE", "B-TIE", "01-tie-empty", "02-must-fail", "2026-11-17T11:00:00Z", "&lt;tag&gt;"} {
			if !strings.Contains(string(raw), value) {
				t.Errorf("offline evidence identifier missing %q", value)
			}
		}
	} else {
		v := independentDecode[struct{ Vector independentBytes }](t, e.scenario.Expected).Vector
		if v.SHA256 != "" {
			independentEqual(t, "known canonical evidence digest", r.EvidenceDigest, v.SHA256)
		}
	}
	if strings.Contains(string(raw), "<script") {
		t.Error("offline report contains script")
	}
	t.Log("HTML bytes, selected fields and protected originals verified. Offline browser traversal needs separate evidence")
}
