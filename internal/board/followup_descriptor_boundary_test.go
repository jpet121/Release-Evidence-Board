package board_test

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"release-evidence-board/internal/board"
)

func TestFollowupDescriptorOrderIsCanonical(t *testing.T) {
	original := vectorBaseline()
	original.Behaviors[0].Tests = append(original.Behaviors[0].Tests,
		board.TestPlan{ID: "T3", ExpectedOutcome: "Second condition", PlannedAttempts: 1})
	original.Behaviors = append(original.Behaviors, additionalApprovalBehavior(true))
	canonical, err := original.Descriptor()
	if err != nil || canonical.Digest == "" {
		t.Fatalf("valid ordered baseline did not encode: %v", err)
	}
	for _, c := range []struct {
		id, order       string
		behavior, tests bool
	}{
		{"AC-003-02.P001", "behavior order", true, false},
		{"AC-003-02.P002", "test order", false, true},
		{"AC-003-02.P003", "both behavior and test order", true, true},
	} {
		t.Run(c.id, func(t *testing.T) {
			variant := original
			variant.Behaviors = slices.Clone(original.Behaviors)
			variant.Behaviors[0].Tests = slices.Clone(original.Behaviors[0].Tests)
			if c.tests {
				slices.Reverse(variant.Behaviors[0].Tests)
			}
			if c.behavior {
				slices.Reverse(variant.Behaviors)
			}
			encoded, err := variant.Descriptor()
			if err != nil || encoded != canonical {
				t.Fatalf("reordered input changed canonical bytes or digest: %v", err)
			}
			recordApprovalInstance(t, c.id, map[string]any{"changedOrder": c.order}, "Identical digest after required ID sorting")
		})
	}
}

func TestFollowupCanonicalDescriptorEscapesNUL(t *testing.T) {
	baseline := vectorBaseline()
	baseline.Behaviors[0].Text = "left\x00right"
	actual, err := baseline.Descriptor()
	want := `["reb-baseline/1","DEMO","B1",1,[["BEH1",1,true,"left\u0000right","POL1",1,10000,24,[["T1","Access denied",true,1]]]]]`
	expected := sha256.Sum256([]byte(want))
	if err != nil || actual.Text != want || actual.Digest != hex.EncodeToString(expected[:]) {
		t.Fatalf("NUL produced noncanonical bytes or digest: %#v %v", actual, err)
	}
	recordApprovalInstance(t, "AC-003-10.P003", map[string]any{"character": "U+0000"}, "Exact C01 escaping, UTF-8, no BOM/newline")
}

func TestFollowupArtifactDigestIsNotRecursive(t *testing.T) {
	artifact := sha256.Sum256([]byte("abc"))
	manifest := vectorManifest()
	manifest.BuildDigest = hex.EncodeToString(artifact[:])
	descriptor, err := manifest.Descriptor()
	if err != nil || manifest.BuildDigest != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" ||
		descriptor.Digest != vectorManifestDigest || descriptor.Digest == manifest.BuildDigest ||
		!strings.Contains(descriptor.Text, manifest.BuildDigest) {
		t.Fatalf("artifact bytes and canonical manifest did not retain distinct digest identities: %v", err)
	}
	recordApprovalInstance(t, "AC-003-12.P001", map[string]any{}, "Artifact hash is not recursive manifest hash")
}

func TestFollowupCandidateChecksEachManifestComponent(t *testing.T) {
	for _, c := range []struct {
		id, key, value string
		accepted       bool
		set            func(*board.Manifest, string)
	}{
		{"AC-003-04.P001", "absent digest", "", false, func(m *board.Manifest, s string) { m.BuildDigest = s }},
		{"AC-003-05.P001", "prompt_digest", "not_applicable", true, func(m *board.Manifest, s string) { m.PromptDigest = s }},
		{"AC-003-05.P002", "model_config_digest", "not_applicable", true, func(m *board.Manifest, s string) { m.ModelConfigDigest = s }},
		{"AC-003-05.P003", "knowledge_digest", "not_applicable", true, func(m *board.Manifest, s string) { m.KnowledgeDigest = s }},
		{"AC-003-05.P004", "build_digest", "not_applicable", false, func(m *board.Manifest, s string) { m.BuildDigest = s }},
	} {
		t.Run(c.id, func(t *testing.T) {
			app, _ := openBoard(t, func() time.Time { return fixtureTime })
			baseline := approveFixture(t, app, vectorBaseline())
			manifest := vectorManifest()
			manifest.PromptDigest = strings.Repeat("b", 64)
			manifest.ModelConfigDigest = strings.Repeat("c", 64)
			manifest.KnowledgeDigest = strings.Repeat("d", 64)
			c.set(&manifest, c.value)
			candidate, reused, err := app.CreateCandidate(supervisor, project, board.CandidateInput{
				Manifest: manifest, BaselineDigest: baseline.Descriptor.Digest,
			})
			if c.accepted {
				if err != nil || reused || candidate.ID == "" {
					t.Fatalf("known optional digest literal was rejected: %v", err)
				}
				_, retained, _, err := app.Candidate(supervisor, project, candidate.ID)
				if err != nil || retained.Manifest != manifest || retained.Descriptor.Digest != candidate.ManifestDigest ||
					!strings.Contains(retained.Descriptor.Text, "not_applicable") {
					t.Fatalf("manifest did not retain the exact component value: %v", err)
				}
			} else {
				if !errors.Is(err, board.ErrInvalid) || reused || candidate.ID != "" {
					t.Fatalf("candidate admitted invalid build digest: %v", err)
				}
				catalog, err := app.Candidates(supervisor, project)
				if err != nil || len(catalog) != 0 {
					t.Fatalf("invalid build digest left a retained candidate: %v", err)
				}
			}
			if c.id == "AC-003-04.P001" {
				recordApprovalInstance(t, c.id, map[string]any{"defect": c.key}, "Rejected")
			} else {
				recordApprovalInstance(t, c.id, map[string]any{"digestFieldUsingNotApplicable": c.key},
					"Accepted; not_applicable in build_digest rejected")
			}
		})
	}
}
