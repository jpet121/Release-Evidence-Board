package board_test

import (
	"bytes"
	"slices"
	"strings"
	"testing"
	"time"

	"release-evidence-board/internal/board"
)

func TestFollowupLabelOnlyVariationsRetainExactFailure(t *testing.T) {
	app, _ := openBoard(t, func() time.Time { return fixtureTime })
	original := fixtureCandidate(t, app, vectorBaseline())
	stamp := board.FormatUTC(fixtureTime)
	raw := csvBytes(t, resultRow(original, "T1", "1", "RETAINED-FAIL", "FAIL-RESULT", stamp, stamp, "FAIL"))
	receipt := importBytes(t, app, original.ID, raw)

	names := vectorBaseline()
	names.ID = "LABEL-ONLY"
	renamed := vectorBaseline()
	// An independent roster cannot claim the already-approved B1 revision.
	// The required bookkeeping change has its own control below.
	renamed.Revision = 3
	renamed.Behaviors[0].ID = "OTHER-BEH"
	renamed.Behaviors[0].Policy.ID = "OTHER-POL"
	renamed.Behaviors[0].Tests[0].ID = "OTHER-TEST"
	bookkeeping := vectorBaseline()
	bookkeeping.Revision = 2
	bookkeeping.Behaviors[0].Revision = 2
	bookkeeping.Behaviors[0].Policy.Revision = 2
	labelDigest := approveFixture(t, app, names).Descriptor.Digest
	rosterDigest := approveFixture(t, app, renamed).Descriptor.Digest
	revisionDigest := approveFixture(t, app, bookkeeping).Descriptor.Digest
	for _, item := range []struct {
		name   string
		ids    [2]string
		change func(*board.CandidateInput)
	}{
		{"release_id", [2]string{"AC-003-13.P001", "AC-003-13.P002"}, func(i *board.CandidateInput) { i.Manifest.ReleaseID = "RELABEL" }},
		{"application_version", [2]string{"AC-003-13.P003", "AC-003-13.P004"}, func(i *board.CandidateInput) { i.Manifest.ApplicationVersion = "9.0.1" }},
		{"baseline_id", [2]string{"AC-003-13.P005", "AC-003-13.P006"}, func(i *board.CandidateInput) { i.BaselineDigest = labelDigest }},
		{"roster IDs", [2]string{"AC-003-13.P007", "AC-003-13.P008"}, func(i *board.CandidateInput) { i.BaselineDigest = rosterDigest }},
		{"bookkeeping revisions", [2]string{"AC-003-13.P009", "AC-003-13.P010"}, func(i *board.CandidateInput) { i.BaselineDigest = revisionDigest }},
		{"display_version", [2]string{}, func(i *board.CandidateInput) { i.DisplayVersion = "Friendly presentation only" }},
	} {
		for index, path := range []string{"fresh candidate", "derived candidate"} {
			t.Run(item.name+"/"+path, func(t *testing.T) {
				input := board.CandidateInput{Manifest: vectorManifest(), BaselineDigest: original.BaselineDigest, DisplayVersion: original.DisplayVersion}
				item.change(&input)
				if index == 1 {
					input.PredecessorID = original.ID
				}
				returned, reused, err := app.CreateCandidate(supervisor, project, input)
				if err != nil || !reused || returned.ID != original.ID || returned.ManifestDigest != original.ManifestDigest ||
					returned.BaselineDigest != original.BaselineDigest || returned.DisplayVersion != original.DisplayVersion {
					t.Fatalf("label %q replaced the retained identity: reused=%t error=%v", item.name, reused, err)
				}
				catalog, err := app.Candidates(supervisor, project)
				if err != nil || len(catalog) != 1 || catalog[0].ID != original.ID {
					t.Fatalf("label change created an independent candidate: %v", err)
				}
				assessment, err := app.Assess(supervisor, project, returned.ID)
				if err != nil || assessment.Status != board.Blocked || assessment.LedgerHighWater != receipt.LedgerHighWater ||
					len(assessment.Behaviors) != 1 || assessment.Behaviors[0].Status != board.Fail ||
					assessment.Behaviors[0].CurrentRunID != "RETAINED-FAIL" {
					t.Fatalf("label change lost the retained definitive failure: %#v, %v", assessment, err)
				}
				source, err := app.SourceCSV(supervisor, project, receipt.ID)
				if err != nil || !bytes.Equal(source, raw) {
					t.Fatalf("equivalent creation replaced my historical source evidence: %v", err)
				}
				if id := item.ids[index]; id != "" {
					recordApprovalInstance(t, id, map[string]any{"labelOnlyChange": item.name, "creationPath": path},
						"Reuse the existing candidate for assessment; a label-only variation cannot create an independent assessment or reset a failure")
				}
			})
		}
	}
}

func TestFollowupMaterialChangesRequireTheirOwnEvidence(t *testing.T) {
	for _, variant := range []struct {
		id, field     string
		expectedDelta string
		edit          func(*board.Manifest, *board.Baseline)
	}{
		{"AC-003-07.P001", "build_digest", "Build artifact", func(m *board.Manifest, _ *board.Baseline) { m.BuildDigest = strings.Repeat("d", 64) }},
		{"AC-003-08.P001", "policy", "Approved behavior, roster or policy", func(_ *board.Manifest, b *board.Baseline) { b.Behaviors[0].Policy.MaxAgeHours = 72 }},
		{"AC-003-08.P002", "expected outcome", "Approved behavior, roster or policy", func(_ *board.Manifest, b *board.Baseline) {
			b.Behaviors[0].Tests[0].ExpectedOutcome = "The protected resource is absent"
		}},
	} {
		t.Run(variant.id, func(t *testing.T) {
			app, _ := openBoard(t, func() time.Time { return fixtureTime })
			original := fixtureCandidate(t, app, vectorBaseline())
			stamp := board.FormatUTC(fixtureTime)
			raw := csvBytes(t, resultRow(original, "T1", "1", "ORIGINAL-PASS", "ORIGINAL-RESULT", stamp, stamp, "PASS"))
			receipt := importBytes(t, app, original.ID, raw)
			initial, err := app.Assess(supervisor, project, original.ID)
			if err != nil || initial.Status != board.Sufficient {
				t.Fatalf("fixture's original complete evidence was not sufficient: %#v %v", initial, err)
			}

			manifest, baseline := vectorManifest(), vectorBaseline()
			variant.edit(&manifest, &baseline)
			digest := original.BaselineDigest
			if variant.field != "build_digest" {
				pending, err := app.CreateDraft(supervisor, project, baseline, board.DraftOptions{FromBaseline: original.BaselineDigest})
				if err != nil {
					t.Fatal(err)
				}
				approved, err := app.Approve(supervisor, project, pending.ID, pending.Revision)
				if err != nil || approved.Descriptor.Digest == digest {
					t.Fatalf("changed contract did not mint a distinct approved digest: %v", err)
				}
				digest = approved.Descriptor.Digest
			}
			newCandidate, reused, err := app.CreateCandidate(supervisor, project, board.CandidateInput{
				Manifest: manifest, BaselineDigest: digest, PredecessorID: original.ID,
			})
			if err != nil || reused || newCandidate.ID == original.ID || newCandidate.PredecessorID != original.ID ||
				!slices.Equal(newCandidate.Changes, []string{variant.expectedDelta}) {
				t.Fatalf("material change did not create the exact successor: %#v %t %v", newCandidate, reused, err)
			}
			if variant.field == "build_digest" && newCandidate.ManifestDigest == original.ManifestDigest {
				t.Fatal("different build bytes retained the old manifest digest")
			}

			next, err := app.Assess(supervisor, project, newCandidate.ID)
			if err != nil || next.Status != board.Blocked || next.LedgerHighWater != receipt.LedgerHighWater ||
				len(next.Behaviors) != 1 || next.Behaviors[0].Status != board.Unknown || next.Behaviors[0].CurrentRunID != "" ||
				len(next.Behaviors[0].Runs) != 0 {
				t.Fatalf("predecessor evidence appeared in the successor's assessment: %#v %v", next, err)
			}
			old, err := app.Assess(supervisor, project, original.ID)
			if err != nil || old.Status != board.Sufficient || old.LedgerHighWater != receipt.LedgerHighWater ||
				len(old.Behaviors) != 1 || old.Behaviors[0].CurrentRunID != "ORIGINAL-PASS" {
				t.Fatalf("successor creation changed the old assessment: %#v %v", old, err)
			}
			stored, err := app.SourceCSV(supervisor, project, receipt.ID)
			if err != nil || !bytes.Equal(stored, raw) {
				t.Fatalf("successor creation lost the earlier evidence bytes: %v", err)
			}
			if variant.field == "build_digest" {
				recordApprovalInstance(t, variant.id, map[string]any{}, "New digest; old evidence cannot satisfy new candidate")
			} else {
				recordApprovalInstance(t, variant.id, map[string]any{"changedField": variant.field}, "New baseline digest; no old evidence copied")
			}
		})
	}
}
