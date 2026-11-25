package board

import bolt "go.etcd.io/bbolt"

// MutateIndependentFixtureStore changes a synthetic, isolated board that the
// test already opened. The method exists only in the Go test binary.
func MutateIndependentFixtureStore(b *Board, fault func(*bolt.Tx) error) error {
	return b.db.Update(fault)
}
