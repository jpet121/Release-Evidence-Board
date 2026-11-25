package recordset

import (
	"errors"
	"math"

	bolt "go.etcd.io/bbolt"
)

var ErrSequenceExhausted = errors.New("record sequence capacity is exhausted")

// AdvanceSequence allocates without wrapping an existing bucket's sequence.
// exhausted preserves the caller's domain error; nil uses ErrSequenceExhausted.
// The caller owns the transaction and must return errors to roll back its writes.
func AdvanceSequence(bucket *bolt.Bucket, exhausted error) (uint64, error) {
	if bucket.Sequence() == math.MaxUint64 {
		if exhausted == nil {
			exhausted = ErrSequenceExhausted
		}
		return 0, exhausted
	}
	return bucket.NextSequence()
}
