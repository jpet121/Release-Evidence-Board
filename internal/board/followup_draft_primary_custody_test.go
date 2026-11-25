package board

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	bolt "go.etcd.io/bbolt"
)

func TestFollowupNextDraftRefusesOccupiedRecordsAndBindings(t *testing.T) {
	for _, fault := range []struct {
		name, section string
		nested        bool
	}{
		{"occupied record", draftBucket, false},
		{"occupied binding", draftMetaBucket, false},
		{"nested record", draftBucket, true},
		{"nested binding", draftMetaBucket, true},
	} {
		t.Run(fault.name, func(t *testing.T) {
			f := newIntakeCustodyFixture(t)
			_, _, approval, err := f.app.Candidate(secondActor, secondProject, f.candidate.ID)
			if err != nil {
				t.Fatal(err)
			}
			original, err := f.app.Draft(secondActor, secondProject, approval.DraftID)
			if err != nil || original.ID != "D-1" {
				t.Fatalf("initial durable draft is not D-1: %q, %v", original.ID, err)
			}
			key := []byte("D-2")
			if err := f.app.db.Update(func(tx *bolt.Tx) error {
				bucket := tx.Bucket([]byte(fault.section))
				if fault.nested {
					_, err := bucket.CreateBucket(key)
					return err
				}
				return bucket.Put(key, []byte("retained fixture record"))
			}); err != nil {
				t.Fatal(err)
			}
			contents := approval.Baseline
			contents.Revision++
			_, err = f.app.CreateDraft(secondActor, secondProject, contents, DraftOptions{})
			want := ErrIntegrity
			if fault.nested {
				want = bolt.ErrIncompatibleValue
			}
			if !errors.Is(err, want) {
				t.Errorf("next draft failed to refuse the occupied native slot: %v", err)
			}
			if err := f.app.db.Update(func(tx *bolt.Tx) error {
				bucket := tx.Bucket([]byte(fault.section))
				if fault.nested {
					return bucket.DeleteBucket(key)
				}
				return bucket.Delete(key)
			}); err != nil {
				t.Fatal(err)
			}
			reopened := f.checkReopened(t, !fault.nested)
			if source, err := reopened.Draft(secondActor, secondProject, original.ID); err != nil || !reflect.DeepEqual(source, original) {
				t.Errorf("failed draft creation changed the original approval source: %v", err)
			}
			if draft, err := reopened.CreateDraft(secondActor, secondProject, contents, DraftOptions{}); err != nil || draft.ID != "D-2" {
				t.Errorf("failed draft creation consumed or partly wrote its allocation: %q, %v", draft.ID, err)
			}
		})
	}
}

func TestFollowupDraftReadsRejectBadIdentityAndMissingPrimary(t *testing.T) {
	for _, fault := range []string{"missing primary", "different draft ID", "invalid draft key", "zero revision", "different product"} {
		t.Run(fault, func(t *testing.T) {
			f := newIntakeCustodyFixture(t)
			_, _, approval, err := f.app.Candidate(secondActor, secondProject, f.candidate.ID)
			if err != nil {
				t.Fatal(err)
			}
			original, err := f.app.Draft(secondActor, secondProject, approval.DraftID)
			if err != nil {
				t.Fatal(err)
			}
			changed := original
			key := original.ID
			switch fault {
			case "different draft ID":
				changed.ID = "D-2"
			case "invalid draft key":
				key = "D bad key"
				changed.ID = key
			case "zero revision":
				changed.Revision = 0
			case "different product":
				changed.Baseline.ProductID = "OTHER"
			}
			if err := f.app.db.Update(func(tx *bolt.Tx) error {
				if fault == "missing primary" {
					return tx.Bucket([]byte(draftBucket)).Delete([]byte(key))
				}
				return putBoundJSON(tx, draftBucket, draftMetaBucket, key, changed)
			}); err != nil {
				t.Fatal(err)
			}
			want := "draft key, revision or product is inconsistent"
			if fault == "missing primary" {
				want = "retained draft record is missing"
			}
			if record, err := f.app.Draft(secondActor, secondProject, key); !errors.Is(err, ErrIntegrity) || !strings.Contains(err.Error(), want) || record.ID != "" {
				t.Errorf("unverified draft read returned a usable record: %q, %v", record.ID, err)
			}
			if err := f.app.db.Update(func(tx *bolt.Tx) error {
				if key == original.ID {
					return putBoundJSON(tx, draftBucket, draftMetaBucket, key, original)
				}
				if err := tx.Bucket([]byte(draftBucket)).Delete([]byte(key)); err != nil {
					return err
				}
				return tx.Bucket([]byte(draftMetaBucket)).Delete([]byte(key))
			}); err != nil {
				t.Fatal(err)
			}
			reopened := f.checkReopened(t, true)
			if draft, err := reopened.Draft(secondActor, secondProject, original.ID); err != nil || !reflect.DeepEqual(original, draft) {
				t.Errorf("repair did not restore the exact approved-source draft: %v", err)
			}
		})
	}
}
