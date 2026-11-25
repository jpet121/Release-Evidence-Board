package board

import (
	"encoding/json"
	"errors"
	"time"

	"release-evidence-board/internal/recordset"

	bolt "go.etcd.io/bbolt"
)

const (
	evidenceRetention = "evidence-retention"
	auditRetention    = "audit-retention"
	backupExpiryAge   = "backup-expiry"
	backupInterval    = "backup-interval"
	backupOverdueAge  = "backup-overdue"

	defaultBackupExpiry  = 30 * 24 * time.Hour
	defaultBackupOverdue = 26 * time.Hour

	operatingConfigKey = "operating_config"
	operatingConfigSHA = "operating_config_sha256"
)

func defaultOperatingConfig() map[string]time.Duration {
	return map[string]time.Duration{
		evidenceRetention: 180 * 24 * time.Hour,
		auditRetention:    180 * 24 * time.Hour,
		backupExpiryAge:   defaultBackupExpiry,
		backupInterval:    24 * time.Hour,
		backupOverdueAge:  defaultBackupOverdue,
	}
}

func readOperatingConfig(tx *bolt.Tx) (map[string]time.Duration, error) {
	config, _, err := readOperatingConfigSnapshot(tx)
	return config, err
}

func readOperatingConfigSnapshot(tx *bolt.Tx) (map[string]time.Duration, ImportRatePolicy, error) {
	policy, err := readImportRatePolicy(tx)
	if err != nil {
		return nil, ImportRatePolicy{}, err
	}
	config, err := readOperatingDurations(tx)
	if err != nil {
		return nil, ImportRatePolicy{}, err
	}
	return config, policy, nil
}

func readOperatingDurations(tx *bolt.Tx) (map[string]time.Duration, error) {
	meta := tx.Bucket([]byte(metaBucket))
	key, sha := []byte(operatingConfigKey), []byte(operatingConfigSHA)
	raw, binding := meta.Get(key), meta.Get(sha)
	defaults := defaultOperatingConfig()
	if raw == nil && binding == nil && meta.Bucket(key) == nil && meta.Bucket(sha) == nil {
		return defaults, nil
	}
	var config map[string]time.Duration
	if !recordset.MatchesBinding(raw, binding) ||
		json.Unmarshal(raw, &config) != nil || len(config) != len(defaults) {
		return nil, integrity("operating configuration does not match its stored record")
	}
	for setting := range defaults {
		if config[setting] <= 0 {
			return nil, integrity("operating configuration contains an invalid duration")
		}
	}
	return config, nil
}

// OperatingConfig returns the effective, store-wide durations to an Operator.
func (b *Board) OperatingConfig(actor, project string) (map[string]time.Duration, error) {
	config, _, err := b.OperatingConfigSnapshot(actor, project)
	return config, err
}

// OperatingConfigSnapshot reads operator durations and the import rate from one
// authorized storage snapshot. Failure exposes neither partial record.
func (b *Board) OperatingConfigSnapshot(actor, project string) (map[string]time.Duration, ImportRatePolicy, error) {
	var config map[string]time.Duration
	var policy ImportRatePolicy
	err := b.db.View(func(tx *bolt.Tx) error {
		if err := b.authorize(tx, actor, project, operate); err != nil {
			return err
		}
		var err error
		config, policy, err = readOperatingConfigSnapshot(tx)
		return err
	})
	if err != nil {
		return nil, ImportRatePolicy{}, b.recordIntegrityIssue(actor, err)
	}
	return config, policy, nil
}

var errSameOperatingSetting = errors.New("operating setting already configured")

// SetOperatingDuration updates a named operating duration and its attributed
// audit in one transaction. Reapplying the current value leaves the store intact.
func (b *Board) SetOperatingDuration(actor, project, setting string, duration time.Duration) error {
	return b.changeOperatingSetting(actor, project, func(tx *bolt.Tx) ([]string, error) {
		return updateOperatingDuration(tx, setting, duration)
	})
}

func updateOperatingDuration(tx *bolt.Tx, setting string, duration time.Duration) ([]string, error) {
	config, err := readOperatingConfig(tx)
	if err != nil {
		return nil, err
	}
	old, supported := config[setting]
	if !supported || duration <= 0 {
		return nil, invalid("use a supported operating setting and a positive duration")
	}
	if old == duration {
		return nil, errSameOperatingSetting
	}
	config[setting] = duration
	if err := recordset.WriteBoundJSON(tx, recordset.Key{Bucket: metaBucket, Key: operatingConfigKey}, recordset.Key{Bucket: metaBucket, Key: operatingConfigSHA}, config); err != nil {
		return nil, err
	}
	return []string{setting, "old=" + old.String(), "new=" + duration.String()}, nil
}

func (b *Board) changeOperatingSetting(actor, project string, update func(*bolt.Tx) ([]string, error)) error {
	const action = "config.change"
	b.recoveryMu.Lock()
	defer b.recoveryMu.Unlock()
	err := b.db.Update(func(tx *bolt.Tx) error {
		if err := b.authorizeCustody(tx, actor, project); err != nil {
			return err
		}
		references, err := update(tx)
		if err != nil {
			return err
		}
		if _, err := increment(tx, "data_revision"); err != nil {
			return err
		}
		return b.audit(tx, actor, action, "success", references...)
	})
	if err == errSameOperatingSetting {
		return nil
	}
	return b.recordIntegrityIssue(actor, b.failedChange(actor, action, err))
}
