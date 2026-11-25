package recordset_test

import (
	"errors"
	"testing"

	bolt "go.etcd.io/bbolt"
	"release-evidence-board/internal/recordset"
)

func TestWriteJSONPublishesOnePlainRecordAndEmptyBatchIsNoop(t *testing.T) {
	db := openRecords(t)
	if err := db.Update(func(tx *bolt.Tx) error {
		return recordset.WriteJSON(tx, recordset.JSONRecord{Key: recordset.Key{Bucket: "records", Key: "evidence"}, Value: 123})
	}); err != nil {
		t.Fatal(err)
	}
	checkValue(t, db, "evidence", "123")
	checkValue(t, db, "sentinel", "untouched")
	checkValue(t, db, "binding", "")
	if err := db.Update(func(tx *bolt.Tx) error { return recordset.WriteJSON(tx) }); err != nil {
		t.Fatal(err)
	}
	checkValue(t, db, "evidence", "123")
}

func TestWriteJSONStopsBeforeLaterEncodingAndRollsBackCompletedPairs(t *testing.T) {
	for _, boundFailure := range []bool{false, true} {
		t.Run(map[bool]string{false: "primary", true: "binding"}[boundFailure], func(t *testing.T) {
			db := openRecords(t)
			binding := recordset.Key{Bucket: "records", Key: "binding"}
			fault := recordset.JSONRecord{Key: recordset.Key{Bucket: "records", Key: "nested"}, Value: 456}
			if boundFailure {
				fault.Key.Key = "later-primary"
				fault.Binding = &recordset.Key{Bucket: "records", Key: "nested"}
			}
			laterCalls := 0
			err := db.Update(func(tx *bolt.Tx) error {
				err := recordset.WriteJSON(tx,
					recordset.JSONRecord{Key: recordset.Key{Bucket: "records", Key: "evidence"}, Value: 123, Binding: &binding},
					fault,
					recordset.JSONRecord{Key: recordset.Key{Bucket: "records", Key: "never-encoded"}, Value: observedJSON{calls: &laterCalls}},
					recordset.JSONRecord{Key: recordset.Key{Bucket: "records", Key: "sentinel"}, Value: make(chan int)})
				bucket := tx.Bucket([]byte("records"))
				if string(bucket.Get([]byte("evidence"))) != "123" || string(bucket.Get([]byte("binding"))) != hash123 {
					t.Error("earlier pair was not written before native failure")
				}
				if boundFailure && string(bucket.Get([]byte("later-primary"))) != "456" {
					t.Error("binding was attempted before its primary")
				}
				if string(bucket.Get([]byte("sentinel"))) != "untouched" {
					t.Error("later write continued after refusal")
				}
				return err
			})
			if !errors.Is(err, bolt.ErrIncompatibleValue) {
				t.Fatalf("first native refusal lost to later encoding: %v", err)
			}
			if laterCalls != 0 {
				t.Fatalf("serialized later value %d times", laterCalls)
			}
			checkValue(t, db, "evidence", "retained evidence")
			checkValue(t, db, "binding", "")
			checkValue(t, db, "later-primary", "")
			checkValue(t, db, "sentinel", "untouched")
		})
	}
}

// A valid JSON Marshaler records when real encoding happens; it does not
// manufacture an error or replace the production serializer/storage adapter.
type observedJSON struct{ calls *int }

func (v observedJSON) MarshalJSON() ([]byte, error) { *v.calls++; return []byte("123"), nil }

func TestWriteJSONMissingUnboundBucketKeepsNativeCauseAndEvidence(t *testing.T) {
	db := openRecords(t)
	calls := 0
	err := db.Update(func(tx *bolt.Tx) error {
		return recordset.WriteJSON(tx,
			recordset.JSONRecord{Key: recordset.Key{Bucket: "absent", Key: "plain"}, Value: observedJSON{calls: &calls}},
			recordset.JSONRecord{Key: recordset.Key{Bucket: "records", Key: "sentinel"}, Value: 456})
	})
	if !errors.Is(err, bolt.ErrBucketNotFound) || calls != 1 {
		t.Fatalf("plain missing bucket cause/encoding = %v calls=%d", err, calls)
	}
	checkValue(t, db, "evidence", "retained evidence")
	checkValue(t, db, "sentinel", "untouched")
}

func TestWriteJSONMixedBatchPersistsEveryRecord(t *testing.T) {
	db := openRecords(t)
	binding := recordset.Key{Bucket: "records", Key: "candidate-binding"}
	if err := db.Update(func(tx *bolt.Tx) error {
		return recordset.WriteJSON(tx,
			recordset.JSONRecord{Key: recordset.Key{Bucket: "records", Key: "manifest"}, Value: 123},
			recordset.JSONRecord{Key: recordset.Key{Bucket: "records", Key: "candidate"}, Value: 456, Binding: &binding},
			recordset.JSONRecord{Key: recordset.Key{Bucket: "records", Key: "material"}, Value: "material"})
	}); err != nil {
		t.Fatal(err)
	}
	checkValue(t, db, "manifest", "123")
	checkValue(t, db, "candidate", "456")
	checkValue(t, db, "candidate-binding", hash456)
	checkValue(t, db, "material", `"material"`)
	checkValue(t, db, "evidence", "retained evidence")
	checkValue(t, db, "sentinel", "untouched")
}
