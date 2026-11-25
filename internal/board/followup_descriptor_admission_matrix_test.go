package board_test

import (
	"strings"
	"testing"

	"release-evidence-board/internal/board"
)

func TestFollowupManifestDescriptorRejectsIndividualIdentityFields(t *testing.T) {
	cases := []struct {
		name, diagnostic string
		edit             func(*board.Manifest)
	}{
		{"product", "product, release and environment revision IDs", func(m *board.Manifest) { m.ProductID = "invalid product" }},
		{"release", "product, release and environment revision IDs", func(m *board.Manifest) { m.ReleaseID = "invalid release" }},
		{"environment", "product, release and environment revision IDs", func(m *board.Manifest) { m.EnvironmentID = "" }},
		{"empty version", "application version must be nonempty", func(m *board.Manifest) { m.ApplicationVersion = "" }},
		{"prompt digest", "prompt digest must be", func(m *board.Manifest) { m.PromptDigest = strings.Repeat("A", 64) }},
		{"model digest", "model configuration digest must be", func(m *board.Manifest) { m.ModelConfigDigest = "invalid" }},
		{"knowledge digest", "knowledge digest must be", func(m *board.Manifest) { m.KnowledgeDigest = "invalid" }},
	}
	for _, entry := range cases {
		t.Run(entry.name, func(t *testing.T) {
			input := vectorManifest()
			entry.edit(&input)
			got, err := input.Descriptor()
			if err == nil || !strings.Contains(err.Error(), entry.diagnostic) || got != (board.Descriptor{}) {
				t.Errorf("invalid manifest field produced a descriptor: got=%v error=%v", got, err)
			}
		})
	}
}

func TestFollowupBaselineDescriptorRejectsIndividualIdentityFields(t *testing.T) {
	cases := []struct {
		name, diagnostic string
		edit             func(*board.Baseline)
	}{
		{"product", "baseline requires valid product and baseline IDs", func(b *board.Baseline) { b.ProductID = "invalid product" }},
		{"baseline", "baseline requires valid product and baseline IDs", func(b *board.Baseline) { b.ID = "invalid baseline" }},
		{"behavior", "behavior requires a valid ID", func(b *board.Baseline) { b.Behaviors[0].ID = "invalid behavior" }},
		{"policy", "invalid policy for behavior", func(b *board.Baseline) { b.Behaviors[0].Policy.ID = "invalid policy" }},
		{"test", "invalid test in behavior", func(b *board.Baseline) { b.Behaviors[0].Tests[0].ID = "invalid test" }},
	}
	for _, entry := range cases {
		t.Run(entry.name, func(t *testing.T) {
			input := vectorBaseline()
			entry.edit(&input)
			got, err := input.Descriptor()
			if err == nil || !strings.Contains(err.Error(), entry.diagnostic) || got != (board.Descriptor{}) {
				t.Errorf("invalid baseline field produced a descriptor: got=%v error=%v", got, err)
			}
		})
	}
}
