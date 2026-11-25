package recordset_test

import (
	"errors"
	"path/filepath"
	"testing"

	bolt "go.etcd.io/bbolt"
	"release-evidence-board/internal/recordset"
)

func TestDeleteFailureStopsAndCallerTransactionRestoresEarlierEvidence(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "nested-value", true: "missing-bucket"}[missing], func(t *testing.T) {
			db := openRecords(t)
			observedEarlierRemoval := false
			err := db.Update(func(tx *bolt.Tx) error {
				fault := recordset.Key{Bucket: "records", Key: "nested"}
				if missing {
					fault.Bucket = "missing"
				}
				err := recordset.Delete(tx, recordset.Key{Bucket: "records", Key: "evidence"}, fault,
					recordset.Key{Bucket: "records", Key: "sentinel"})
				observedEarlierRemoval = tx.Bucket([]byte("records")).Get([]byte("evidence")) == nil
				if string(tx.Bucket([]byte("records")).Get([]byte("sentinel"))) != "untouched" {
					t.Error("execution continued past the failed deletion")
				}
				return err
			})
			want := bolt.ErrIncompatibleValue
			if missing {
				want = bolt.ErrBucketNotFound
			}
			if !errors.Is(err, want) || !observedEarlierRemoval {
				t.Fatalf("native fault or pre-error deletion not observed: %v, removed=%v", err, observedEarlierRemoval)
			}
			checkValue(t, db, "evidence", "retained evidence")
			checkValue(t, db, "sentinel", "untouched")
		})
	}
}

func TestDeleteRequiresWritableTransactionAndCanCommitACompleteGroup(t *testing.T) {
	db := openRecords(t)
	if err := db.View(func(tx *bolt.Tx) error {
		return recordset.Delete(tx, recordset.Key{Bucket: "records", Key: "evidence"})
	}); !errors.Is(err, bolt.ErrTxNotWritable) {
		t.Fatalf("read-only transaction did not preserve native refusal: %v", err)
	}
	checkValue(t, db, "evidence", "retained evidence")
	if err := db.Update(func(tx *bolt.Tx) error {
		return recordset.Delete(tx, recordset.Key{Bucket: "records", Key: "evidence"},
			recordset.Key{Bucket: "records", Key: "sentinel"})
	}); err != nil {
		t.Fatal(err)
	}
	checkValue(t, db, "evidence", "")
	checkValue(t, db, "sentinel", "")
	if err := db.View(func(tx *bolt.Tx) error { return recordset.Delete(tx) }); err != nil {
		t.Fatal(err)
	}
}

func openRecords(t *testing.T) *bolt.DB {
	t.Helper()
	db, err := bolt.Open(filepath.Join(t.TempDir(), "records.db"), 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := db.Update(func(tx *bolt.Tx) error {
		bucket, err := tx.CreateBucket([]byte("records"))
		if err != nil {
			return err
		}
		if err := bucket.Put([]byte("evidence"), []byte("retained evidence")); err != nil {
			return err
		}
		if err := bucket.Put([]byte("sentinel"), []byte("untouched")); err != nil {
			return err
		}
		_, err = bucket.CreateBucket([]byte("nested"))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return db
}

func checkValue(t *testing.T, db *bolt.DB, key, expected string) {
	t.Helper()
	if err := db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket([]byte("records")).Get([]byte(key))
		if expected == "" && (raw != nil || tx.Bucket([]byte("records")).Bucket([]byte(key)) != nil) {
			t.Errorf("record %s still exists after deletion", key)
		}
		if got := string(raw); got != expected {
			t.Errorf("record %s = %q, want %q", key, got, expected)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
