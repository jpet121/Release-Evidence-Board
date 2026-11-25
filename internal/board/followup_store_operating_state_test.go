package board

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func TestFollowupCheckAndHealthRefuseInvalidSavedOperatingConfiguration(t *testing.T) {
	f := newIntakeCustodyFixture(t)
	if err := f.app.SetOperatingDuration(secondActor, secondProject, backupInterval, 12*time.Hour); err != nil {
		t.Fatal(err)
	}
	if check := f.app.Check(); !check.DatabaseOK || check.ConsecutiveFailures != 0 {
		t.Fatalf("initial valid operating configuration failed its health check: %+v", check)
	}
	key := []byte(operatingConfigSHA)
	var saved []byte
	if err := f.app.db.Update(func(tx *bolt.Tx) error {
		meta := tx.Bucket([]byte(metaBucket))
		saved = bytes.Clone(meta.Get(key))
		if len(saved) == 0 {
			return errors.New("fixture did not persist a real operating configuration binding")
		}
		return meta.Put(key, []byte(strings.Repeat("0", 64)))
	}); err != nil {
		t.Fatal(err)
	}
	for count := 1; count <= 2; count++ {
		check := f.app.Check()
		if check.DatabaseOK || check.ConsecutiveFailures != count || !check.CheckedAt.Equal(f.at) {
			t.Fatalf("invalid operating configuration reported a healthy sample: %+v", check)
		}
	}
	if _, err := f.app.Health(secondActor, secondProject); !errors.Is(err, ErrIntegrity) ||
		!strings.Contains(err.Error(), "operating configuration does not match its stored record") {
		t.Fatalf("operator health accepted malformed saved operating configuration: %v", err)
	}
	if held, err := f.app.AssessmentSuspended(secondActor, secondProject); !held || err != nil {
		t.Fatalf("operator health did not keep the known fault held: held=%t %v", held, err)
	}
	if err := f.app.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(metaBucket)).Put(key, saved)
	}); err != nil {
		t.Fatal(err)
	}
	reopened := f.checkReopened(t, true)
	if check := reopened.Check(); !check.DatabaseOK || check.ConsecutiveFailures != 0 {
		t.Fatalf("repaired operating configuration did not recover health: %+v", check)
	}
}

func TestFollowupAuditRefusesOrphanBindingAndZeroSequence(t *testing.T) {
	t.Run("unowned_next_binding", func(t *testing.T) {
		f := newIntakeCustodyFixture(t)
		before, err := f.app.Audit(secondActor, secondProject)
		if err != nil || len(before) == 0 {
			t.Fatalf("fixture lacks committed audit events: %v", err)
		}
		key := []byte(auditKey(before[len(before)-1].Sequence + 1))
		if err := f.app.db.Update(func(tx *bolt.Tx) error {
			return tx.Bucket([]byte(auditMetaBucket)).Put(key, []byte(digest([]byte("unowned companion"))))
		}); err != nil {
			t.Fatal(err)
		}
		const id = "synthetic-audit-destination"
		if err := f.app.SetAccount(secondActor, id, "synthetic-account-password", []Role{Viewer}); !errors.Is(err, ErrIntegrity) ||
			!strings.Contains(err.Error(), "audit event sequence already exists") {
			t.Fatalf("append did not refuse the next event's occupied binding: %v", err)
		}
		if _, err := f.app.Account(id); !errors.Is(err, ErrNotFound) {
			t.Fatalf("refused append exposed the associated account update: %v", err)
		}
		if audit, err := f.app.Audit(secondActor, secondProject); len(audit) != 0 || !errors.Is(err, ErrIntegrity) ||
			!strings.Contains(err.Error(), "stored evidence companion lacks a primary record") {
			t.Fatalf("audit read accepted an unowned event binding: entries=%d %v", len(audit), err)
		}
		if err := f.app.db.Update(func(tx *bolt.Tx) error {
			return tx.Bucket([]byte(auditMetaBucket)).Delete(key)
		}); err != nil {
			t.Fatal(err)
		}
		if after, err := f.app.Audit(secondActor, secondProject); err != nil || !reflect.DeepEqual(after, before) {
			t.Fatalf("rejected append changed the prior audit sequence: %v", err)
		}
		f.checkReopened(t, false)
	})
	t.Run("zero_sequence", func(t *testing.T) {
		f := newIntakeCustodyFixture(t)
		key := auditKey(0)
		if err := f.app.db.Update(func(tx *bolt.Tx) error {
			event := AuditEvent{Time: f.at, Actor: secondActor, Action: "access.set", Outcome: "success"}
			return putBoundJSON(tx, auditBucket, auditMetaBucket, key, event)
		}); err != nil {
			t.Fatal(err)
		}
		if audit, err := f.app.Audit(secondActor, secondProject); len(audit) != 0 || !errors.Is(err, ErrIntegrity) ||
			!strings.Contains(err.Error(), "retained audit event disagrees with its sequence key") {
			t.Fatalf("audit read exposed an event with reserved sequence zero: entries=%d %v", len(audit), err)
		}
		if err := f.app.db.Update(func(tx *bolt.Tx) error {
			if err := tx.Bucket([]byte(auditBucket)).Delete([]byte(key)); err != nil {
				return err
			}
			return tx.Bucket([]byte(auditMetaBucket)).Delete([]byte(key))
		}); err != nil {
			t.Fatal(err)
		}
		f.checkReopened(t, true)
	})
}

func TestFollowupOpenRefusesMissingRequiredSectionAndInvalidProduct(t *testing.T) {
	rejected := filepath.Join(t.TempDir(), "invalid-product.db")
	invalid, err := Open(rejected, Settings{ProjectID: secondProject, ProductID: "invalid product"})
	if invalid != nil {
		t.Cleanup(func() { _ = invalid.Close() })
	}
	if invalid != nil || !errors.Is(err, ErrInvalid) {
		t.Fatalf("valid project and invalid product did not fail before opening: %v", err)
	}
	if _, err := os.Stat(rejected); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid independent product ID created a store: %v", err)
	}
	f := newIntakeCustodyFixture(t)
	path := f.app.db.Path()
	if err := f.app.db.Update(func(tx *bolt.Tx) error { return tx.DeleteBucket([]byte(previewCSV)) }); err != nil {
		t.Fatal(err)
	}
	if err := f.app.Close(); err != nil {
		t.Fatal(err)
	}
	settings := Settings{ProjectID: secondProject, ProductID: "DEMO", Clock: func() time.Time { return f.at }}
	partial, err := Open(path, settings)
	if partial != nil {
		t.Cleanup(func() { _ = partial.Close() })
	}
	if partial != nil || !errors.Is(err, ErrIntegrity) || !strings.Contains(err.Error(), "storage section missing") {
		t.Fatalf("previously initialized store without a required nonlegacy section entered service: %v", err)
	}
	db, err := bolt.Open(path, 0600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		if tx.Bucket([]byte(previewCSV)) != nil {
			return errors.New("failed Open did not leave the isolated missing section unchanged")
		}
		_, err := tx.CreateBucket([]byte(previewCSV))
		return err
	})
	if err := errors.Join(err, db.Close()); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, settings)
	if err != nil {
		t.Fatalf("repaired required section could not reopen the named store: %v", err)
	}
	t.Cleanup(func() {
		if err := reopened.Close(); err != nil {
			t.Error(err)
		}
	})
	if csv, err := reopened.SourceCSV(secondActor, secondProject, f.receipt.ID); err != nil || !bytes.Equal(csv, f.source) {
		t.Fatalf("failed startup altered the retained original CSV: %v", err)
	}
	if report, html, err := reopened.ReportHTML(secondActor, secondProject, f.report.ID); err != nil || report != f.report || !bytes.Equal(html, f.html) {
		t.Fatalf("failed startup altered the retained frozen report: %v", err)
	}
}
