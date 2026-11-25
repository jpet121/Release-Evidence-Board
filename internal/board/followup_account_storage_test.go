package board

import (
	"bytes"
	"errors"
	"reflect"
	"slices"
	"testing"

	bolt "go.etcd.io/bbolt"
)

func TestFollowupOperatorRetainsOwnAccessAndReportsAccountStorageRefusals(t *testing.T) {
	f := newIntakeCustodyFixture(t)
	const retainedPassword = "synthetic-catalog-password"
	retainedRoles := []Role{Operator, Reviewer}
	if err := f.app.SetAccount(secondActor, secondActor, retainedPassword, retainedRoles); err != nil {
		t.Fatalf("operator could not retain their own authorized account: %v", err)
	}
	before, err := f.app.Authenticate(secondActor, retainedPassword)
	if err != nil || !slices.Equal(before.Roles, retainedRoles) {
		t.Fatalf("authorized in-place account update did not persist its explicit roles: %v", err)
	}
	if err := f.app.SetAccount(secondActor, secondActor, retainedPassword, []Role{Viewer}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("operator unexpectedly removed their own operating access: %v", err)
	}
	const other = "catalog-fixture-target"
	if err := f.app.db.Update(func(tx *bolt.Tx) error {
		_, err := tx.Bucket([]byte(accountBucket)).CreateBucket([]byte(other))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.app.SetAccount(secondActor, other, retainedPassword, []Role{Viewer}); !errors.Is(err, bolt.ErrIncompatibleValue) {
		t.Fatalf("account assignment hid the actual storage write refusal: %v", err)
	}
	events, err := f.app.Audit(secondActor, secondProject)
	if err != nil || slices.ContainsFunc(events, func(event AuditEvent) bool {
		return event.Action == "access.set" && event.Outcome == "success" && slices.Contains(event.References, other)
	}) {
		t.Fatalf("failed account assignment advertised a successful new record: %v", err)
	}
	var original []byte
	if err := f.app.db.Update(func(tx *bolt.Tx) error {
		accounts := tx.Bucket([]byte(accountBucket))
		if err := accounts.DeleteBucket([]byte(other)); err != nil {
			return err
		}
		original = bytes.Clone(accounts.Get([]byte(secondActor)))
		return accounts.Put([]byte(secondActor), []byte("{"))
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := f.app.Candidate(secondActor, secondProject, f.candidate.ID); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("candidate read hid the malformed stored authorizing account: %v", err)
	}
	if err := f.app.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(accountBucket)).Put([]byte(secondActor), original)
	}); err != nil {
		t.Fatal(err)
	}
	if after, err := f.app.Authenticate(secondActor, retainedPassword); err != nil || !reflect.DeepEqual(after, before) {
		t.Fatalf("failed account changes altered the original authorized account: %v", err)
	}
	f.checkReopened(t, true)
}
