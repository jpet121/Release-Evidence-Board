package recordset

import (
	"encoding/json"

	bolt "go.etcd.io/bbolt"
)

// JSONRecord publishes a primary JSON value and, when Binding is nonnil, its
// exact lowercase SHA-256 companion. All coordinates must be distinct.
type JSONRecord struct {
	Key     Key
	Value   any
	Binding *Key
}

// WriteJSON writes records in supplied order in the caller's transaction.
// Return its error from Update to roll back any earlier writes.
func WriteJSON(tx *bolt.Tx, records ...JSONRecord) error {
	for _, record := range records {
		raw, err := json.Marshal(record.Value)
		if err != nil {
			return err
		}
		if err := put(tx, record.Key, raw); err != nil {
			return err
		}
		if record.Binding != nil {
			if err := put(tx, *record.Binding, jsonBinding(raw)); err != nil {
				return err
			}
		}
	}
	return nil
}

func put(tx *bolt.Tx, key Key, raw []byte) error {
	bucket := tx.Bucket([]byte(key.Bucket))
	if bucket == nil {
		return bolt.ErrBucketNotFound
	}
	return bucket.Put([]byte(key.Key), raw)
}
