package board_test

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

type offlineElement struct {
	tag      string
	attrs    map[string]string
	text     strings.Builder
	children []*offlineElement
}

func (e *offlineElement) matching(test func(*offlineElement) bool) []*offlineElement {
	var out []*offlineElement
	for _, child := range e.children {
		if test(child) {
			out = append(out, child)
		}
		out = append(out, child.matching(test)...)
	}
	return out
}

func (e *offlineElement) tagged(tag string) []*offlineElement {
	return e.matching(func(child *offlineElement) bool { return child.tag == tag })
}

func (e *offlineElement) plain() string { return strings.TrimSpace(e.text.String()) }

func offlineOne(t *testing.T, label string, matches []*offlineElement) *offlineElement {
	t.Helper()
	if len(matches) != 1 {
		t.Fatalf("frozen HTML %s count was %d, want 1", label, len(matches))
	}
	return matches[0]
}

func offlineParse(t *testing.T, raw []byte) (*offlineElement, map[string]*offlineElement) {
	t.Helper()
	decoder := xml.NewDecoder(bytes.NewReader(raw))
	decoder.Strict = false
	decoder.AutoClose = xml.HTMLAutoClose
	decoder.Entity = xml.HTMLEntity
	root := &offlineElement{}
	stack := []*offlineElement{root}
	anchors := map[string]*offlineElement{}
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		independentRequire(t, err)
		switch value := token.(type) {
		case xml.StartElement:
			node := &offlineElement{tag: value.Name.Local, attrs: map[string]string{}}
			for _, attr := range value.Attr {
				node.attrs[attr.Name.Local] = attr.Value
			}
			id := node.attrs["id"]
			if id != "" {
				if anchors[id] != nil {
					t.Fatalf("duplicate offline link target %q", id)
				}
				anchors[id] = node
			}
			parent := stack[len(stack)-1]
			parent.children = append(parent.children, node)
			stack = append(stack, node)
		case xml.EndElement:
			stack = stack[:len(stack)-1]
		case xml.CharData:
			for _, parent := range stack {
				parent.text.Write(value)
			}
		}
	}
	return root, anchors
}

func offlineSafeLinks(t *testing.T, root *offlineElement, anchors map[string]*offlineElement) {
	t.Helper()
	for _, node := range root.matching(func(*offlineElement) bool { return true }) {
		switch node.tag {
		case "script", "iframe", "object", "embed", "base", "form", "link", "img", "audio", "video":
			t.Errorf("offline HTML contains active or resource element <%s>", node.tag)
		case "style":
			css := strings.ToLower(node.plain())
			if strings.Contains(css, "@import") || strings.Contains(css, "url(") {
				t.Error("offline HTML style declares an external resource")
			}
		}
		for key, value := range node.attrs {
			if strings.HasPrefix(key, "on") || slices.Contains([]string{"src", "srcset", "poster", "action"}, key) {
				t.Errorf("offline HTML contains active attribute %q", key)
			}
			if key != "href" {
				continue
			}
			if strings.HasPrefix(value, "#") {
				if anchors[strings.TrimPrefix(value, "#")] == nil {
					t.Errorf("offline HTML has broken anchor %q", value)
				}
				continue
			}
			parsed, err := url.Parse(value)
			if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || node.tag != "a" || !strings.Contains(node.plain(), "Unarchived source") {
				t.Errorf("unsafe or unlabeled external source link %q", value)
			}
		}
	}
}

type offlineRun struct {
	id       string
	sequence uint64
	close    time.Time
	oldest   *time.Time
	failures int
	mustFail bool
	unusable bool
	seen     map[string]bool
	definite bool
	complete bool
}

type offlineBehavior struct {
	status     string
	current    *offlineRun
	definitive map[string]*offlineRun
}

func (e *independentEnvironment) checkOfflineHTML(raw []byte) {
	t := e.t
	t.Helper()
	root, anchors := offlineParse(t, raw)
	offlineSafeLinks(t, root, anchors)
	for _, id := range []string{"manifest-descriptor", "baseline-descriptor", "evidence-descriptor"} {
		if anchors[id] == nil {
			t.Fatalf("frozen HTML lacks %q", id)
		}
	}
	baselineText := anchors["baseline-descriptor"].plain()
	roster := independentBaseline(t, []byte(baselineText))
	evidenceText := anchors["evidence-descriptor"].plain()
	evidence := independentDecode[[]json.RawMessage](t, []byte(evidenceText))
	if len(evidence) != 7 {
		t.Fatalf("frozen evidence contains %d fields, want 7", len(evidence))
	}
	str := func(field int) string { return independentDecode[string](t, evidence[field]) }
	independentEqual(t, "canonical evidence format", str(0), "reb-evidence/1")
	independentEqual(t, "evidence/manifest identity", str(1), independentSHA([]byte(anchors["manifest-descriptor"].plain())))
	independentEqual(t, "evidence/baseline identity", str(2), independentSHA([]byte(baselineText)))
	independentEqual(t, "rule version", str(3), "reb-rules/1.5")
	ledger := independentDecode[uint64](t, evidence[4])
	at := independentTime(t, str(5))
	rows := independentDecode[[][]json.RawMessage](t, evidence[6])
	header := offlineOne(t, "report header", root.tagged("header"))
	dates := header.tagged("time")
	if len(dates) < 1 {
		t.Fatal("report header missing assessment time")
	}
	independentSameInstant(t, "visible historical assessment time", dates[0].plain(), at)
	for _, phrase := range []string{"Historical assessment", "No deployment authority", fmt.Sprintf("Ledger high-water mark %d", ledger)} {
		if !strings.Contains(header.plain(), phrase) {
			t.Errorf("frozen report header lacks %q", phrase)
		}
	}
	var candidates = map[string]offlineBehavior{}
	counts := map[string]int{"PASS": 0, "FAIL": 0, "STALE": 0, "UNKNOWN": 0}
	var earliest *time.Time
	clockStrings := map[string][2]string{}
	for _, behavior := range roster.Behaviors {
		article := anchors["behavior-"+behavior.ID]
		if article == nil {
			t.Fatalf("no offline anchor for behavior %q", behavior.ID)
		}
		plan := map[string]bool{}
		for _, test := range behavior.Tests {
			for attempt := 1; attempt <= test.PlannedAttempts; attempt++ {
				plan[test.ID+":"+strconv.Itoa(attempt)] = test.MustPass
			}
		}
		runs := map[string]*offlineRun{}
		for _, row := range rows {
			if len(row) != 20 {
				t.Fatalf("offline evidence row contains %d fields, want 20", len(row))
			}
			value := func(field int) string { return independentDecode[string](t, row[field]) }
			if value(7) != behavior.ID {
				continue
			}
			runID, closeAt := value(12), independentTime(t, value(15))
			seq := independentDecode[uint64](t, row[19])
			run := runs[runID]
			if run == nil {
				run = &offlineRun{id: runID, sequence: seq, close: closeAt, seen: map[string]bool{}}
				runs[runID] = run
			}
			independentEqual(t, "consistent row sequence", seq, run.sequence)
			independentEqual(t, "consistent row producer close", closeAt, run.close)
			runNode := anchors["runid-"+runID]
			if runNode == nil {
				t.Fatalf("no visible run anchor for %s", runID)
			}
			h4 := offlineOne(t, "run title", runNode.tagged("h4"))
			independentEqual(t, "run title", h4.plain(), fmt.Sprintf("%s · ledger sequence %d", runID, seq))
			if timeNodes := runNode.tagged("time"); len(timeNodes) < 1 {
				t.Errorf("visible producer finish missing for %s", runID)
			} else {
				independentSameInstant(t, "visible producer finish for "+runID, timeNodes[0].plain(), closeAt)
			}
			var visibleText string
			if value(1) == "RUN_ONLY" {
				visibleText = "RUN_ONLY · No result rows"
				clockStrings[runID] = [2]string{"", value(15)}
			} else {
				key := value(11) + ":" + value(13)
				must, exists := plan[key]
				if !exists || run.seen[key] {
					t.Fatalf("offline result refers to invalid or duplicate slot %s", key)
				}
				run.seen[key] = true
				switch outcome := value(16); outcome {
				case "FAIL":
					run.failures++
					run.mustFail = run.mustFail || must
				case "PASS":
				case "ERROR", "SKIPPED", "UNKNOWN":
					run.unusable = true
				default:
					t.Fatalf("unknown retained result outcome %q", outcome)
				}
				resultAt := independentTime(t, value(14))
				if run.oldest == nil || resultAt.Before(*run.oldest) {
					run.oldest = &resultAt
				}
				clockStrings[value(2)] = [2]string{value(14), value(15)}
				visibleText = fmt.Sprintf("Test %s · attempt %s · result finish %s · outcome %s", value(11), value(13), value(14), value(16))
			}
			if !strings.Contains(runNode.plain(), visibleText) {
				t.Errorf("visible offline row data missing %q", visibleText)
			}
		}
		observed := offlineBehavior{status: "UNKNOWN", definitive: map[string]*offlineRun{}}
		for _, run := range runs {
			run.definite = run.mustFail || (len(plan)-run.failures)*10000 < behavior.Policy.MinPassBasisPoints*len(plan)
			run.complete = !run.unusable && len(run.seen) == len(plan)
			if run.definite {
				observed.definitive[run.id] = run
			}
			previous := observed.current
			if previous == nil || run.close.After(previous.close) || (run.close.Equal(previous.close) && ((!run.complete && previous.complete) || (run.complete == previous.complete && run.sequence > previous.sequence))) {
				observed.current = run
			}
		}
		if current := observed.current; current != nil && current.complete {
			if current.oldest == nil {
				t.Fatalf("complete run %s has no recorded result timestamp", current.id)
			}
			expires := current.oldest.Add(time.Duration(behavior.Policy.MaxAgeHours) * time.Hour)
			if earliest == nil || expires.Before(*earliest) {
				earliest = &expires
			}
			observed.status = "PASS"
			if at.After(expires) {
				observed.status = "STALE"
			}
		}
		if len(observed.definitive) > 0 {
			observed.status = "FAIL"
		}
		candidates[behavior.ID] = observed
		visible := offlineOne(t, "visible behavior title", article.tagged("h3"))
		independentEqual(t, "recomputed visible behavior "+behavior.ID, visible.plain(), behavior.ID+" · "+observed.status)
		if behavior.Required {
			counts[observed.status]++
		}
	}
	result := "SUFFICIENT"
	if counts["FAIL"]+counts["STALE"]+counts["UNKNOWN"] > 0 {
		result = "BLOCKED"
	}
	for status, count := range counts {
		if !strings.Contains(header.plain(), fmt.Sprintf("%s %d", status, count)) {
			t.Errorf("header lacks required %s count %d", status, count)
		}
	}
	visibleOutcome := offlineOne(t, "visible candidate heading", header.tagged("h1"))
	independentEqual(t, "recomputed visible candidate status", visibleOutcome.plain(), "Historical assessment · "+result)
	if earliest == nil {
		if !strings.Contains(header.plain(), "Earliest current complete-run expiry none") {
			t.Error("no complete-run expiry was available")
		}
	} else if len(dates) < 2 {
		t.Errorf("visible expiry missing; offline reconstruction is %s", earliest)
	} else {
		independentSameInstant(t, "visible expiry", dates[1].plain(), *earliest)
	}
	expect := independentDecode[struct {
		CandidateStatus    string
		BehaviorStatus     string
		CurrentRun         string
		HighWaterMark      uint64
		AssessmentTime     string
		SourceClockStrings map[string][2]string
		RequiredCounts     map[string]int
		RequiredBehaviors  map[string]struct {
			Status, CurrentRun, DefinitiveRun string
			CurrentSequence                   uint64
			ReasonCodes                       []string
		}
	}](t, e.scenario.Expected)
	independentEqual(t, "recomputed literal candidate status", result, expect.CandidateStatus)
	independentEqual(t, "recomputed literal ledger high-water", ledger, expect.HighWaterMark)
	independentEqual(t, "recomputed literal assessment time", str(5), expect.AssessmentTime)
	for key, expected := range expect.SourceClockStrings {
		independentEqual(t, "exact canonical timestamp strings for "+key, clockStrings[key], expected)
	}
	if len(expect.RequiredBehaviors) == 0 {
		b := candidates["BEH1"]
		independentEqual(t, "recomputed literal status", b.status, expect.BehaviorStatus)
		if b.current == nil {
			t.Fatal("offline replay found no current run")
		}
		independentEqual(t, "recomputed literal run", b.current.id, expect.CurrentRun)
	} else {
		independentEqual(t, "recomputed literal status counts", counts, expect.RequiredCounts)
		for id, expected := range expect.RequiredBehaviors {
			observed := candidates[id]
			independentEqual(t, "recomputed literal "+id+" status", observed.status, expected.Status)
			if observed.current == nil {
				t.Fatalf("offline replay found no current run for %s", id)
			}
			independentEqual(t, "recomputed literal "+id+" current run", observed.current.id, expected.CurrentRun)
			independentEqual(t, "recomputed literal "+id+" sequence", observed.current.sequence, expected.CurrentSequence)
			if expected.DefinitiveRun != "" && observed.definitive[expected.DefinitiveRun] == nil {
				t.Errorf("offline replay lost the persistent failure on %s", expected.DefinitiveRun)
			}
			for _, reason := range expected.ReasonCodes {
				if !strings.Contains(anchors["behavior-"+id].plain(), reason) {
					t.Errorf("offline report omitted %s for %s", reason, id)
				}
			}
		}
	}
	if !strings.Contains(root.plain(), independentSHA([]byte(evidenceText))) {
		t.Error("HTML evidence SHA-256 is missing or differs from the exact embedded evidence bytes")
	}
	t.Logf("offline HTML-only reconstruction observed %s with counts %v, %d literal source rows, %d resolvable anchor targets", result, counts, len(rows), len(anchors))
}

func independentSameInstant(t *testing.T, label, shown string, expected time.Time) {
	t.Helper()
	instant, err := time.Parse(time.RFC3339Nano, shown)
	if err != nil || !instant.Equal(expected) {
		t.Errorf("%s: got %q, want UTC instant %s", label, shown, expected.Format(time.RFC3339Nano))
	}
}
