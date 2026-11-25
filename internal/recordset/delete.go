// Package recordset applies ordered scalar-record operations to real storage.
package recordset

import bolt "go.etcd.io/bbolt"

// Key identifies a scalar record, including bindings, hashes and composite keys.
type Key struct {
	Bucket, Key string
}

// Delete removes keys in order within the caller's open bbolt transaction.
// It stops at the first storage error. The caller must return that error from
// Update to roll back earlier removals; Delete neither commits nor rolls back.
func Delete(tx *bolt.Tx, keys ...Key) error {
	for _, key := range keys {
		bucket := tx.Bucket([]byte(key.Bucket))
		if bucket == nil {
			return bolt.ErrBucketNotFound
		}
		if err := bucket.Delete([]byte(key.Key)); err != nil {
			return err
		}
	}
	return nil
}
