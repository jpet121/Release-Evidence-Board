package board

import (
	"errors"
	"testing"
)

func TestFollowupUnknownAccountRemainsForbiddenWithSyntheticWorkloadInput(t *testing.T) {
	app, _, _ := reviewBoard(t)
	const nonexistent = "absent-synthetic-account"
	const sample = "release-board-unknown-account-workload-fixture"
	if _, err := app.Account(nonexistent); !errors.Is(err, ErrNotFound) {
		t.Fatalf("fixture does not establish an absent account: %v", err)
	}
	if account, err := app.Authenticate(nonexistent, sample); !errors.Is(err, ErrForbidden) || account.ID != "" || len(account.Roles) != 0 {
		t.Errorf("synthetic workload input authenticated a nonexistent account: %v", err)
	}
	if existing, err := app.Authenticate(secondActor, "synthetic-review-password"); err != nil || existing.ID != secondActor || !existing.CanReview() {
		t.Errorf("known synthetic account could not authenticate normally: %v", err)
	}
}
