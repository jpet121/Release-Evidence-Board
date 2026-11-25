package browser_test

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"release-evidence-board/internal/board"
)

// A single real upload exercises all three advertised input limits. Keep the
// fixture modest enough for an ordinary native run.
func TestFollowupMaximumApprovedRosterAndProtectedCSV(t *testing.T) {
	const behaviors, attempts, totalRows, totalBytes = 500, 20, 10000, 10485760
	fx := newAuthorityFixture(t)
	baseline := board.Baseline{ProductID: "DEMO", ID: "MAXIMUM", Revision: 1}
	for n := 1; n <= behaviors; n++ {
		key := fmt.Sprintf("%03d", n)
		baseline.Behaviors = append(baseline.Behaviors, board.Behavior{
			ID: "B-" + key, Revision: 1, Required: true, Text: "Maximum roster control",
			Policy: board.Policy{ID: "P-" + key, Revision: 1, MinPassBasisPoints: 10000, MaxAgeHours: 24},
			Tests:  []board.TestPlan{{ID: "T-" + key, ExpectedOutcome: "PASS", MustPass: true, PlannedAttempts: attempts}},
		})
	}
	draft, err := fx.app.CreateDraft("reviewer", "pilot", baseline, board.DraftOptions{})
	if err != nil {
		t.Fatal(err)
	}
	approved, err := fx.app.Approve("reviewer", "pilot", draft.ID, draft.Revision)
	if err != nil {
		t.Fatalf("maximum roster did not approve: %v", err)
	}
	_, template, _, err := fx.app.Candidate("reviewer", "pilot", fx.candidateID)
	if err != nil {
		t.Fatal(err)
	}
	manifest := template.Manifest
	manifest.ReleaseID = "R-MAXIMUM"
	manifest.BuildDigest = strings.Repeat("b", 64)
	candidate, reused, err := fx.app.CreateCandidate("test-contributor", "pilot", board.CandidateInput{Manifest: manifest, BaselineDigest: approved.Descriptor.Digest})
	if err != nil || reused {
		t.Fatalf("maximum assessment fixture failed to create its own candidate: %v", err)
	}
	_, _, retained, err := fx.app.Candidate("viewer", "pilot", candidate.ID)
	if err != nil || retained.Descriptor.Digest != approved.Descriptor.Digest || len(retained.Baseline.Behaviors) != behaviors {
		t.Fatalf("assigned Viewer did not observe the full approved roster: %v", err)
	}
	observedAttempts := 0
	for _, behavior := range retained.Baseline.Behaviors {
		if !behavior.Required || len(behavior.Tests) != 1 || behavior.Tests[0].PlannedAttempts != attempts {
			t.Fatalf("roster entry %s does not retain the submitted attempt count", behavior.ID)
		}
		observedAttempts += behavior.Tests[0].PlannedAttempts
	}
	if observedAttempts != totalRows {
		t.Fatalf("maximum approved roster plans %d attempts", observedAttempts)
	}
	recordThresholdHTTPInstance(t, "AC-002-13.P001", map[string]any{"behaviors": behaviors, "plannedAttempts": totalRows}, "Accepted")

	stamp := board.FormatUTC(time.Now().UTC().Add(-3 * time.Minute))
	raw := maximumAcceptanceCSV(t, candidate, manifest.ReleaseID, stamp, behaviors, attempts, totalBytes)
	digest := fixtureHash(raw)
	path := "/p/pilot/candidates/" + candidate.ID
	preview := fx.send(t, fx.contributor, path+"/preview", nil, raw)
	token := authorityPreviewToken.FindStringSubmatch(preview.body)
	if preview.status != http.StatusOK || len(token) != 2 || !strings.Contains(preview.body, "10000 parsed rows") || !strings.Contains(preview.body, digest) {
		t.Fatalf("maximum source was not presented as a confirmable exact preview: HTTP %d", preview.status)
	}
	committed := fx.send(t, fx.contributor, path+"/commit", url.Values{"preview": {token[1]}}, nil)
	id, seen := strings.CutPrefix(committed.location, "/p/pilot/receipts/")
	if committed.status != http.StatusSeeOther || !seen || id != "I-"+digest {
		t.Fatalf("maximum accepted preview did not redirect to its bound receipt: HTTP %d", committed.status)
	}
	receipt, err := fx.app.Receipt("viewer", "pilot", id)
	if err != nil || receipt.Rows != totalRows || receipt.Hash != digest || receipt.Uploader != "test-contributor" || receipt.LedgerHighWater != behaviors || len(receipt.AddedRuns) != behaviors {
		t.Fatalf("confirmed maximum commit lost evidence, row count, ledger or attribution: %v", err)
	}
	stored, headers, status := readFollowupArtifact(t, fx.viewer.client, fx.visit+committed.location+"/source")
	if status != http.StatusOK || headers.Get("Content-Type") != "text/csv; charset=utf-8" || len(stored) != totalBytes || !bytes.Equal(stored, raw) {
		t.Fatalf("current assigned Viewer did not retrieve all %d original bytes: HTTP %d", totalBytes, status)
	}
	assessed, err := fx.app.Assess("viewer", "pilot", candidate.ID)
	if err != nil || assessed.Status != "SUFFICIENT" || assessed.Counts["PASS"] != behaviors || assessed.LedgerHighWater != behaviors {
		t.Fatalf("current decision did not assess every maximum run: %s, %v", assessed.Status, err)
	}
	recordThresholdHTTPInstance(t, "AC-004-14.P001", map[string]any{"bytes": len(stored)}, "Accepted")
	recordThresholdHTTPInstance(t, "AC-004-14.P002", map[string]any{"dataRows": receipt.Rows}, "Accepted")
}

func maximumAcceptanceCSV(t *testing.T, candidate board.Candidate, release, stamp string, behaviors, attempts, size int) []byte {
	t.Helper()
	encode := func(padding int) []byte {
		var data bytes.Buffer
		data.Grow(size)
		data.WriteString(followupHeader)
		writer := csv.NewWriter(&data)
		base, residue := padding/(behaviors*attempts), padding%(behaviors*attempts)
		for n := 1; n <= behaviors; n++ {
			key := fmt.Sprintf("%03d", n)
			behaviorID := "B-" + key
			for slot := 1; slot <= attempts; slot++ {
				width := base
				if (n-1)*attempts+slot <= residue {
					width++
				}
				row := followupBrowserResult(candidate, release, behaviorID, "P-"+key, "T-"+key, "RUN-"+behaviorID, fmt.Sprintf("RES-%s-%02d", behaviorID, slot), stamp, "PASS")
				row[13], row[18] = strconv.Itoa(slot), strings.Repeat("x", width)
				if err := writer.Write(row); err != nil {
					t.Fatal(err)
				}
			}
		}
		writer.Flush()
		if err := writer.Error(); err != nil {
			t.Fatal(err)
		}
		return data.Bytes()
	}
	extra := size - len(encode(0))
	if extra < 0 || extra > behaviors*attempts*1000 {
		t.Fatalf("could not produce exactly %d bytes within excerpt constraints", size)
	}
	raw := encode(extra)
	if len(raw) != size {
		t.Fatalf("serialized upload contains %d bytes, want %d", len(raw), size)
	}
	return raw
}
