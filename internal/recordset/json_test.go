package recordset_test

import (
	"encoding/json"
	"errors"
	"testing"

	bolt "go.etcd.io/bbolt"
	"release-evidence-board/internal/recordset"
)

const hash123 = "a665a45920422f9d417e4867efdc4fb8a04a1f3fff1fa07e998e86f7f7a27ae3"
const hash456 = "b3a8e0e1f9ab1bfe3a36f231f676f78bb30a519d2b21e6c530c0eee8ebb4a5d0"

func TestEncodeJSONUsesExactIndependentGoldenAndRejectsUnsupportedValue(t *testing.T) {
	for _, v := range []struct {
		value     int
		raw, hash string
	}{{123, "123", hash123}, {456, "456", hash456}} {
		raw, hash, err := recordset.EncodeJSON(v.value)
		if err != nil || string(raw) != v.raw || string(hash) != v.hash {
			t.Fatalf("codec differs from literal JSON/Python hashlib golden: %q %q %v", raw, hash, err)
		}
	}
	raw, hash, err := recordset.EncodeJSON(make(chan int))
	var unsupported *json.UnsupportedTypeError
	if !errors.As(err, &unsupported) || raw != nil || hash != nil {
		t.Fatal("unsupported value produced usable bytes or binding")
	}
}

func TestWriteBoundJSONCommitsPairInSameBucketAndPreservesNeighbor(t *testing.T) {
	db := openRecords(t)
	if err := db.Update(func(tx *bolt.Tx) error {
		return recordset.WriteBoundJSON(tx, recordset.Key{Bucket: "records", Key: "evidence"}, recordset.Key{Bucket: "records", Key: "binding"}, 123)
	}); err != nil {
		t.Fatal(err)
	}
	checkValue(t, db, "evidence", "123")
	checkValue(t, db, "binding", hash123)
	checkValue(t, db, "sentinel", "untouched")
	if err := db.Update(func(tx *bolt.Tx) error {
		err := recordset.WriteBoundJSON(tx, recordset.Key{Bucket: "records", Key: "evidence"}, recordset.Key{Bucket: "records", Key: "binding"}, make(chan int))
		bucket := tx.Bucket([]byte("records"))
		if string(bucket.Get([]byte("evidence"))) != "123" || string(bucket.Get([]byte("binding"))) != hash123 {
			t.Error("serialization failure wrote before returning its error")
		}
		return err
	}); err == nil {
		t.Fatal("unsupported value was accepted")
	} else {
		var unsupported *json.UnsupportedTypeError
		if !errors.As(err, &unsupported) {
			t.Fatal(err)
		}
	}
	checkValue(t, db, "evidence", "123")
	checkValue(t, db, "binding", hash123)
	if err := db.View(func(tx *bolt.Tx) error {
		return recordset.WriteBoundJSON(tx, recordset.Key{Bucket: "records", Key: "evidence"}, recordset.Key{Bucket: "records", Key: "binding"}, 456)
	}); !errors.Is(err, bolt.ErrTxNotWritable) {
		t.Fatalf("readonly write: %v", err)
	}
	checkValue(t, db, "evidence", "123")
	checkValue(t, db, "binding", hash123)
}

func TestWriteBoundJSONStopsOnPrimaryErrorBeforeLookingUpBinding(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "nested-primary", true: "missing-primary"}[missing], func(t *testing.T) {
			db := openRecords(t)
			primary := recordset.Key{Bucket: "records", Key: "nested"}
			want := bolt.ErrIncompatibleValue
			if missing {
				primary.Bucket = "missing"
				want = bolt.ErrBucketNotFound
			}
			err := db.Update(func(tx *bolt.Tx) error {
				return recordset.WriteBoundJSON(tx, primary, recordset.Key{Bucket: "also-missing", Key: "binding"}, 123)
			})
			if !errors.Is(err, want) {
				t.Fatalf("first storage error lost: %v", err)
			}
			checkValue(t, db, "evidence", "retained evidence")
			checkValue(t, db, "sentinel", "untouched")
		})
	}
}

func TestWriteBoundJSONBindingRefusalRollsBackEarlierPrimaryWrite(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "nested-binding", true: "missing-binding"}[missing], func(t *testing.T) {
			db := openRecords(t)
			binding := recordset.Key{Bucket: "records", Key: "nested"}
			want := bolt.ErrIncompatibleValue
			if missing {
				binding.Bucket = "missing"
				want = bolt.ErrBucketNotFound
			}
			observed := false
			err := db.Update(func(tx *bolt.Tx) error {
				err := recordset.WriteBoundJSON(tx, recordset.Key{Bucket: "records", Key: "evidence"}, binding, 456)
				observed = string(tx.Bucket([]byte("records")).Get([]byte("evidence"))) == "456"
				if string(tx.Bucket([]byte("records")).Get([]byte("sentinel"))) != "untouched" {
					t.Error("neighbor changed")
				}
				return err
			})
			if !errors.Is(err, want) || !observed {
				t.Fatalf("binding failure not reached after primary: %v, %v", err, observed)
			}
			checkValue(t, db, "evidence", "retained evidence")
			checkValue(t, db, "sentinel", "untouched")
			if err := db.Update(func(tx *bolt.Tx) error {
				return recordset.WriteBoundJSON(tx, recordset.Key{Bucket: "records", Key: "evidence"}, recordset.Key{Bucket: "records", Key: "binding"}, 456)
			}); err != nil {
				t.Fatal(err)
			}
			checkValue(t, db, "evidence", "456")
			checkValue(t, db, "binding", hash456)
		})
	}
}
