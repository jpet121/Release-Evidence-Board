package board_test

import (
	"testing"

	"release-evidence-board/internal/board"
)

const (
	vectorManifestDigest = "fb6399a38982efad88825b73d432d8d2ccc3c23101e33cb6da7ed9749b7587d4"
	vectorBaselineDigest = "1f3aadc243cd5f236c248dfb9422c692efc017a87c4b781d564edafb8512808e"
)

func vectorManifest() board.Manifest {
	return board.Manifest{
		ProductID: "DEMO", ReleaseID: "R1", ApplicationVersion: "1.0.0",
		BuildDigest:       "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad",
		PromptDigest:      "not_applicable",
		ModelConfigDigest: "not_applicable",
		KnowledgeDigest:   "not_applicable",
		EnvironmentID:     "E1",
	}
}

func vectorBaseline() board.Baseline {
	return board.Baseline{
		ProductID: "DEMO", ID: "B1", Revision: 1,
		Behaviors: []board.Behavior{{
			ID: "BEH1", Revision: 1, Required: true, Text: "Reject unauthorized reads",
			Policy: board.Policy{ID: "POL1", Revision: 1, MinPassBasisPoints: 10000, MaxAgeHours: 24},
			Tests:  []board.TestPlan{{ID: "T1", ExpectedOutcome: "Access denied", MustPass: true, PlannedAttempts: 1}},
		}},
	}
}

func TestAC00301DescriptorVectors(t *testing.T) {
	t.Run("V01", func(t *testing.T) {
		descriptor, err := vectorManifest().Descriptor()
		if err != nil {
			t.Fatal(err)
		}
		want := `["reb-manifest/1","DEMO","R1","1.0.0","ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad","not_applicable","not_applicable","not_applicable","E1"]`
		if descriptor.Text != want || descriptor.Digest != vectorManifestDigest {
			t.Fatalf("manifest descriptor bytes or digest differ from V01: %#v", descriptor)
		}
	})

	t.Run("V02", func(t *testing.T) {
		descriptor, err := vectorBaseline().Descriptor()
		if err != nil {
			t.Fatal(err)
		}
		want := `["reb-baseline/1","DEMO","B1",1,[["BEH1",1,true,"Reject unauthorized reads","POL1",1,10000,24,[["T1","Access denied",true,1]]]]]`
		if descriptor.Text != want || descriptor.Digest != vectorBaselineDigest {
			t.Fatalf("baseline descriptor bytes or digest differ from V02: %#v", descriptor)
		}
	})
}
