package recordset

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	bolt "go.etcd.io/bbolt"
)

// EncodeJSON returns standard JSON bytes and their lowercase SHA-256 binding.
// Serialization failure returns neither bytes nor a plausible binding.
func EncodeJSON(value any) ([]byte, []byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, nil, err
	}
	return raw, jsonBinding(raw), nil
}

func jsonBinding(raw []byte) []byte {
	hash := sha256.Sum256(raw)
	return []byte(hex.EncodeToString(hash[:]))
}

// MatchesBinding checks a present value against its exact lowercase SHA-256
// binding. A present zero-length value is distinct from an absent value.
// Both byte slices remain owned by the caller and unchanged by verification.
func MatchesBinding(raw, binding []byte) bool {
	return raw != nil && bytes.Equal(binding, jsonBinding(raw))
}

// WriteBoundJSON writes a JSON value before its binding in the caller's open
// transaction. The coordinates must be distinct; they may share a bucket.
// It stops at the first error. Return that error from Update to roll back any
// earlier write. Missing buckets return bbolt.ErrBucketNotFound.
func WriteBoundJSON(tx *bolt.Tx, valueKey, bindingKey Key, value any) error {
	return WriteJSON(tx, JSONRecord{Key: valueKey, Binding: &bindingKey, Value: value})
}
