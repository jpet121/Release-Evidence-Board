package board_test

import (
	"crypto/sha256"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"release-evidence-board/internal/board"
)

func TestFollowupInferredPredecessorUsesTimeBeforeStableTieOrder(t *testing.T) {
	now := fixtureTime
	app, _ := openBoard(t, func() time.Time { return now })
	approved := approveFixture(t, app, vectorBaseline())
	type proposal struct {
		id       string
		manifest board.Manifest
	}
	var related []proposal
	for _, mark := range []string{"a", "b", "c"} {
		manifest := vectorManifest()
		manifest.BuildDigest = strings.Repeat(mark, 64)
		descriptor, err := manifest.Descriptor()
		if err != nil {
			t.Fatal(err)
		}
		id := sha256.Sum256([]byte(descriptor.Digest + approved.Descriptor.Digest))
		related = append(related, proposal{fmt.Sprintf("C-%x", id), manifest})
	}
	slices.SortFunc(related, func(left, right proposal) int { return strings.Compare(left.id, right.id) })
	create := func(manifest board.Manifest) board.Candidate {
		t.Helper()
		item, reused, err := app.CreateCandidate(supervisor, project, board.CandidateInput{
			Manifest: manifest, BaselineDigest: approved.Descriptor.Digest,
		})
		if err != nil || reused {
			t.Fatalf("candidate was not independently retained: %v", err)
		}
		return item
	}
	oldest := create(related[2].manifest)
	now = now.Add(time.Hour)
	tiedWinner := create(related[1].manifest)
	tiedEarlierID := create(related[0].manifest)
	if oldest.ID != related[2].id || tiedWinner.ID != related[1].id || tiedEarlierID.ID != related[0].id {
		t.Fatal("fixture does not distinguish the older high ID from the equal-time candidates")
	}
	now = now.Add(time.Hour)
	unrelated := vectorManifest()
	unrelated.ReleaseID = "UNRELATED-RELEASE"
	unrelated.BuildDigest = strings.Repeat("d", 64)
	if other := create(unrelated); other.PredecessorID != "" {
		t.Fatalf("unrelated release unexpectedly inherited a predecessor: %s", other.PredecessorID)
	}
	nextManifest := vectorManifest()
	nextManifest.BuildDigest = strings.Repeat("e", 64)
	next := create(nextManifest)
	if next.PredecessorID != tiedWinner.ID || !slices.Equal(next.Changes, []string{"Build artifact"}) {
		t.Errorf("candidate did not inherit the latest eligible predecessor then stable tie order: %s %v",
			next.PredecessorID, next.Changes)
	}
	if saved, _, _, err := app.Candidate(supervisor, project, tiedWinner.ID); err != nil || !reflect.DeepEqual(saved, tiedWinner) {
		t.Errorf("inference altered the selected historical candidate: %v", err)
	}
}
