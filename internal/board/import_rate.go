package board

import (
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"release-evidence-board/internal/recordset"

	bolt "go.etcd.io/bbolt"
)

const importRateKey = "import_rate_policy"
const importRateSHA = "import_rate_policy_sha256"

// ImportRatePolicy is the per-account request count and rolling import window.
type ImportRatePolicy struct {
	Count  int           `json:"count"`
	Window time.Duration `json:"window"`
}

func readImportRatePolicy(tx *bolt.Tx) (ImportRatePolicy, error) {
	meta := tx.Bucket([]byte(metaBucket))
	key, sha := []byte(importRateKey), []byte(importRateSHA)
	raw, binding := meta.Get(key), meta.Get(sha)
	if raw == nil && binding == nil && meta.Bucket(key) == nil && meta.Bucket(sha) == nil {
		return ImportRatePolicy{Count: 10, Window: time.Minute}, nil
	}
	var policy ImportRatePolicy
	if !recordset.MatchesBinding(raw, binding) ||
		json.Unmarshal(raw, &policy) != nil || policy.Count <= 0 || policy.Window <= 0 {
		return ImportRatePolicy{}, integrity("import rate policy does not match a valid stored record")
	}
	return policy, nil
}

// ImportRate returns the active policy to an Operator or authorized contributor.
func (b *Board) ImportRate(actor, project string) (ImportRatePolicy, error) {
	var policy ImportRatePolicy
	err := b.db.View(func(tx *bolt.Tx) error {
		err := b.authorize(tx, actor, project, operate)
		if errors.Is(err, ErrForbidden) {
			err = b.authorize(tx, actor, project, contribute)
		}
		if err != nil {
			return err
		}
		policy, err = readImportRatePolicy(tx)
		return err
	})
	return policy, b.recordIntegrityIssue(actor, err)
}

// SetImportRate atomically sets both policy limits and their attributed audit.
func (b *Board) SetImportRate(actor, project string, policy ImportRatePolicy) error {
	return b.changeOperatingSetting(actor, project, func(tx *bolt.Tx) ([]string, error) {
		return updateImportRate(tx, policy)
	})
}

// updateImportRate returns attributed changes only after the bound policy write.
// The caller owns authorization, revision, audit and the transaction commit.
func updateImportRate(tx *bolt.Tx, policy ImportRatePolicy) ([]string, error) {
	if policy.Count <= 0 || policy.Window <= 0 {
		return nil, invalid("import rate requires a positive request count and duration")
	}
	old, err := readImportRatePolicy(tx)
	if err != nil {
		return nil, err
	}
	if old == policy {
		return nil, errSameOperatingSetting
	}
	if err := recordset.WriteBoundJSON(tx, recordset.Key{Bucket: metaBucket, Key: importRateKey}, recordset.Key{Bucket: metaBucket, Key: importRateSHA}, policy); err != nil {
		return nil, err
	}
	return []string{"import-rate", "old_count=" + strconv.Itoa(old.Count), "old_window=" + old.Window.String(),
		"new_count=" + strconv.Itoa(policy.Count), "new_window=" + policy.Window.String()}, nil
}
