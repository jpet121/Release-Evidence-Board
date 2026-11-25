package recordset_test

import (
	"errors"
	"math"
	"testing"

	bolt "go.etcd.io/bbolt"
	"release-evidence-board/internal/recordset"
)

func TestAdvanceSequenceCommitsFinalIdentifierAndNeverWraps(t *testing.T) {
	db := openRecords(t)
	capacity := errors.New("caller-specific capacity diagnostic")
	if err := db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte("records"))
		if err := bucket.SetSequence(math.MaxUint64 - 1); err != nil {
			return err
		}
		got, err := recordset.AdvanceSequence(bucket, capacity)
		if err != nil || got != math.MaxUint64 || bucket.Sequence() != math.MaxUint64 {
			t.Fatalf("final identifier = %d, sequence=%d: %v", got, bucket.Sequence(), err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, supplied := range []error{capacity, nil} {
		want := supplied
		if want == nil {
			want = recordset.ErrSequenceExhausted
		}
		if err := db.Update(func(tx *bolt.Tx) error {
			bucket := tx.Bucket([]byte("records"))
			got, err := recordset.AdvanceSequence(bucket, supplied)
			if got != 0 || err != want || bucket.Sequence() != math.MaxUint64 {
				t.Fatalf("exhausted allocation = %d, sequence=%d: %v", got, bucket.Sequence(), err)
			}
			return err
		}); err != want {
			t.Fatalf("caller capacity error changed: %v", err)
		}
	}
	if err := db.View(func(tx *bolt.Tx) error {
		got, err := recordset.AdvanceSequence(tx.Bucket([]byte("records")), capacity)
		if got != 0 || err != capacity {
			t.Fatalf("capacity must precede native writability refusal: %d, %v", got, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	checkValue(t, db, "evidence", "retained evidence")
}

func TestAdvanceSequencePreservesNativeReadOnlyAndClosedTransactionErrors(t *testing.T) {
	db := openRecords(t)
	if err := db.View(func(tx *bolt.Tx) error {
		got, err := recordset.AdvanceSequence(tx.Bucket([]byte("records")), nil)
		if got != 0 || !errors.Is(err, bolt.ErrTxNotWritable) {
			t.Fatalf("native read-only refusal changed: %d, %v", got, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin(true)
	if err != nil {
		t.Fatal(err)
	}
	bucket := tx.Bucket([]byte("records"))
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if got, err := recordset.AdvanceSequence(bucket, nil); got != 0 || !errors.Is(err, bolt.ErrTxClosed) {
		t.Fatalf("native closed-transaction refusal changed: %d, %v", got, err)
	}
}

func TestAdvanceSequenceRollsBackWithLaterNativeWriteFailureAndRetries(t *testing.T) {
	db := openRecords(t)
	observedAllocation := false
	if err := db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte("records"))
		got, err := recordset.AdvanceSequence(bucket, nil)
		if err != nil {
			return err
		}
		observedAllocation = got == 1 && bucket.Sequence() == 1
		return bucket.Put([]byte("nested"), []byte("cannot replace a real bucket"))
	}); !errors.Is(err, bolt.ErrIncompatibleValue) || !observedAllocation {
		t.Fatalf("native refusal after allocation was not observed: allocated=%t, %v", observedAllocation, err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte("records"))
		if bucket.Sequence() != 0 || bucket.Bucket([]byte("nested")) == nil {
			t.Fatal("rollback consumed an identifier or damaged existing evidence")
		}
		got, err := recordset.AdvanceSequence(bucket, nil)
		if err != nil || got != 1 {
			t.Fatalf("retry did not reuse the rolled-back identifier: %d, %v", got, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.View(func(tx *bolt.Tx) error {
		if tx.Bucket([]byte("records")).Sequence() != 1 {
			t.Fatal("successful retry did not persist its identifier")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	checkValue(t, db, "evidence", "retained evidence")
	checkValue(t, db, "sentinel", "untouched")
}
