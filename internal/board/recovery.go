package board

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"release-evidence-board/internal/backuppair"

	bolt "go.etcd.io/bbolt"
)

type HealthCheck struct {
	CheckedAt           time.Time
	DatabaseOK          bool
	ConsecutiveFailures int
	BackupOverdue       bool
	BackupOverdueNotice string
}

type Health struct {
	HealthCheck
	LastBackup, LastRestore, LastDrill time.Time
	ImportRejections, ReportFailures   uint64
	Hold                               bool
	BackupDue                          bool
	Alert                              string
	HistoricalAI                       *HistoricalAI
}

type Backup struct {
	Path, SHA256 string
	CreatedAt    time.Time
}

type RestoreResult struct {
	IntegrityOK bool
	RestoredAt  time.Time
}

const unverifiedBackupHold = "corrupt_backup"

func requiresVerifiedBackup(meta *bolt.Bucket) bool {
	return string(meta.Get([]byte("assessment_hold"))) == unverifiedBackupHold
}

func (b *Board) AssessmentSuspended(actor, project string) (bool, error) {
	var held bool
	err := b.db.View(func(tx *bolt.Tx) error {
		if err := b.authorize(tx, actor, project, read); err != nil {
			return err
		}
		held = b.assessmentAvailable(tx) != nil
		return nil
	})
	return held, err
}

func (b *Board) Check() HealthCheck {
	now := b.now()
	var overdueNotice string
	// Mapped reads can still succeed after the storage handle stops accepting
	// writes. Commit a real transaction before reporting the database available.
	err := b.db.Update(func(tx *bolt.Tx) error {
		if _, err := counter(tx, "ledger"); err != nil {
			return err
		}
		config, err := readOperatingConfig(tx)
		if err != nil {
			return err
		}
		backup, err := storedRecoveryTime(tx.Bucket([]byte(metaBucket)), "last_backup")
		if err != nil {
			return err
		}
		overdueNotice = backupOverdueNotice(now, backup, config[backupOverdueAge])
		return nil
	})
	b.healthMu.Lock()
	defer b.healthMu.Unlock()
	b.check.CheckedAt, b.check.DatabaseOK = now, err == nil
	b.check.BackupOverdue, b.check.BackupOverdueNotice = overdueNotice != "", overdueNotice
	if err == nil {
		b.check.ConsecutiveFailures = 0
	} else {
		b.check.ConsecutiveFailures++
	}
	return b.check
}

func backupOverdueNotice(now, last time.Time, age time.Duration) string {
	if last.IsZero() || !now.After(last.Add(age)) {
		return ""
	}
	label := age.String()
	if age == defaultBackupOverdue {
		label = "26 hours"
	}
	return "most recent successful backup is more than " + label + " old"
}

func (b *Board) Health(actor, project string) (Health, error) {
	b.healthMu.Lock()
	health := Health{HealthCheck: b.check}
	b.healthMu.Unlock()
	now := b.now()
	err := b.db.View(func(tx *bolt.Tx) error {
		if err := b.authorize(tx, actor, project, operate); err != nil {
			return err
		}
		meta := tx.Bucket([]byte(metaBucket))
		health.Hold = b.assessmentAvailable(tx) != nil
		config, err := readOperatingConfig(tx)
		if err != nil {
			return err
		}
		health.ImportRejections, err = counter(tx, "import_rejections")
		if err != nil {
			return err
		}
		health.ReportFailures, err = counter(tx, "report_failures")
		if err != nil {
			return err
		}
		times := map[string]*time.Time{"last_backup": &health.LastBackup, "last_restore": &health.LastRestore, "last_drill": &health.LastDrill}
		for key, target := range times {
			*target, err = storedRecoveryTime(meta, key)
			if err != nil {
				return err
			}
		}
		health.BackupDue = health.LastBackup.IsZero() || now.After(health.LastBackup.Add(config[backupInterval]))
		health.BackupOverdueNotice = backupOverdueNotice(now, health.LastBackup, config[backupOverdueAge])
		health.BackupOverdue = health.BackupOverdueNotice != ""
		health.HistoricalAI, err = readHistoricalAI(tx, now)
		return err
	})
	if err != nil {
		return health, b.recordIntegrityIssue(actor, err)
	}
	if health.CheckedAt.IsZero() {
		check := b.Check()
		health.CheckedAt = check.CheckedAt
		health.DatabaseOK = check.DatabaseOK
		health.ConsecutiveFailures = check.ConsecutiveFailures
	}
	switch {
	case health.ConsecutiveFailures >= 3:
		health.Alert = "Three consecutive application health checks failed. Inspect storage."
	case health.LastBackup.IsZero():
		health.Alert = "No successful backup recorded."
	case health.BackupOverdue:
		health.Alert = "The " + health.BackupOverdueNotice + "."
	case health.LastDrill.IsZero() || now.After(health.LastDrill.AddDate(0, 1, 0)):
		health.Alert = "A monthly restore drill is due."
	}
	return health, nil
}

func (b *Board) SuspendAssessments(actor, project string) error {
	return b.change(actor, "assessment.suspend", func(tx *bolt.Tx) error {
		if err := b.authorize(tx, actor, project, operate); err != nil {
			return err
		}
		meta := tx.Bucket([]byte(metaBucket))
		if !requiresVerifiedBackup(meta) {
			if err := meta.Put([]byte("assessment_hold"), []byte("incident")); err != nil {
				return err
			}
		}
		return b.audit(tx, actor, "assessment.suspend", "success")
	})
}

func (b *Board) Revalidate(actor, project string) error {
	err := b.change(actor, "assessment.revalidate", func(tx *bolt.Tx) error {
		return b.revalidateTransaction(tx, actor, project)
	})
	if err == nil {
		b.integrityHold.Store(false)
	}
	return b.recordIntegrityIssue(actor, err)
}

// revalidateTransaction releases the durable hold only after complete validation.
// The outer operation releases its in-process hold only after commit succeeds.
func (b *Board) revalidateTransaction(tx *bolt.Tx, actor, project string) error {
	if err := b.authorize(tx, actor, project, review); err != nil {
		return err
	}
	meta := tx.Bucket([]byte(metaBucket))
	if requiresVerifiedBackup(meta) {
		return fmt.Errorf("%w: restore a verified backup before reviewer revalidation", ErrIntegrity)
	}
	if err := b.validateCurrentStore(tx); err != nil {
		return err
	}
	if err := meta.Delete([]byte("assessment_hold")); err != nil {
		return err
	}
	return b.audit(tx, actor, "assessment.revalidate", "success")
}

func validateStore(tx *bolt.Tx) error {
	return validateStoreAt(tx, time.Time{})
}

func (b *Board) validateCurrentStore(tx *bolt.Tx) error {
	return validateStoreAt(tx, b.aiSnapshotTime(tx))
}

func (b *Board) aiSnapshotTime(tx *bolt.Tx) time.Time {
	meta := tx.Bucket([]byte(metaBucket))
	if meta != nil && meta.Get([]byte(aiSnapshotKey)) != nil {
		return b.now()
	}
	return time.Time{}
}

func validateStoreAt(tx *bolt.Tx, now time.Time) error {
	var pageError error
	for problem := range tx.Check() {
		pageError = errors.Join(pageError, problem)
	}
	if pageError != nil {
		return fmt.Errorf("%w: inconsistent database pages", ErrIntegrity)
	}
	return validateStoreRecords(tx, now)
}

func validateStoreRecords(tx *bolt.Tx, now time.Time) error {
	for _, name := range buckets {
		if tx.Bucket([]byte(name)) == nil {
			return fmt.Errorf("%w: storage section missing", ErrIntegrity)
		}
	}
	for _, key := range []string{"data_revision", "import_rejections", "report_failures"} {
		if _, err := counter(tx, key); err != nil {
			return err
		}
	}
	meta := tx.Bucket([]byte(metaBucket))
	if err := validateAssessmentHoldType(meta); err != nil {
		return err
	}
	for _, key := range []string{"last_backup", "last_restore", "last_drill"} {
		if _, err := storedRecoveryTime(meta, key); err != nil {
			return err
		}
	}
	if _, err := readOperatingConfig(tx); err != nil {
		return err
	}
	if err := visitJSON(tx, expiryBucket, checkBackupExpiry); err != nil {
		return err
	}
	lastAudit := tx.Bucket([]byte(auditBucket)).Sequence()
	if err := visitAuditEvents(tx, func(event AuditEvent) error {
		if event.Sequence > lastAudit {
			return integrity("audit allocator sequence precedes retained events")
		}
		return nil
	}); err != nil {
		return err
	}
	if err := validateDescriptorIndexes(tx); err != nil {
		return err
	}
	expected, ledger, err := expectedStoredRuns(tx, "")
	if err != nil {
		return err
	}
	var runs []Run
	err = visitJSON(tx, runBucket, func(key string, run Run) error {
		if err := validateRunCustody(tx, run, expected[key]); err != nil {
			return err
		}
		runs = append(runs, run)
		return nil
	})
	if err != nil {
		return err
	}
	if len(runs) != len(expected) {
		return integrity("stored run inventory differs from CSV evidence")
	}
	if err := validateRunIndexes(tx, runs); err != nil {
		return err
	}
	if err := verifyCompanionKeys(tx, csvBucket, receiptBucket); err != nil {
		return err
	}
	if err := verifyCompanionKeys(tx, htmlBucket, reportBucket); err != nil {
		return err
	}
	if err := verifyCompanionKeys(tx, reportMetaBucket, reportBucket); err != nil {
		return err
	}
	if _, err := readHistoricalAI(tx, now); err != nil {
		return err
	}
	return validateFrozenReports(tx, ledger)
}

func storedRecoveryTime(meta *bolt.Bucket, key string) (time.Time, error) {
	raw := meta.Get([]byte(key))
	if raw == nil {
		if meta.Bucket([]byte(key)) != nil {
			return time.Time{}, integrity("recovery timestamp is a nested bucket")
		}
		return time.Time{}, nil
	}
	when, ok := parseTimestamp(string(raw))
	if !ok {
		return time.Time{}, ErrIntegrity
	}
	return when, nil
}

func (b *Board) authorizeCustody(tx *bolt.Tx, actor, project string) error {
	if err := b.authorize(tx, actor, project, operate); err != nil {
		return err
	}
	if err := b.assessmentAvailable(tx); err != nil {
		return err
	}
	// bbolt's page check reads the shared freelist. On a live read snapshot,
	// other writers can change that list before this transaction finishes.
	if tx.Writable() {
		return b.validateCurrentStore(tx)
	}
	return validateStoreRecords(tx, b.aiSnapshotTime(tx))
}

func (b *Board) Backup(actor, project, dir string) (result Backup, operationError error) {
	defer func() {
		operationError = b.failedChange(actor, "backup.create", b.recordIntegrityIssue(actor, operationError))
	}()
	if err := b.db.View(func(tx *bolt.Tx) error { return b.authorize(tx, actor, project, operate) }); err != nil {
		return Backup{}, err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return Backup{}, err
	}
	created := b.now()
	staged, err := os.CreateTemp(dir, "reb-stage-*.db")
	if err != nil {
		return Backup{}, err
	}
	stagePath := staged.Name()
	hash := sha256.New()
	err = b.db.View(func(tx *bolt.Tx) error {
		_, err := tx.WriteTo(io.MultiWriter(staged, hash))
		return err
	})
	err = errors.Join(err, staged.Sync(), staged.Close())
	if err == nil {
		err = checkBackup(stagePath)
	}
	if err != nil {
		return Backup{}, errors.Join(err, os.Remove(stagePath))
	}
	random := make([]byte, 6)
	// crypto/rand.Read fills the buffer or terminates the process.
	rand.Read(random)
	name := "reb-backup-" + created.UTC().Format("20060102T150405.000000Z") + "-" + hex.EncodeToString(random) + ".db"
	path := filepath.Join(dir, name)
	record := Backup{Path: path, CreatedAt: created, SHA256: hex.EncodeToString(hash.Sum(nil))}
	if err := backuppair.Publish(stagePath, path, record.SHA256); err != nil {
		return Backup{}, err
	}
	err = b.db.Update(func(tx *bolt.Tx) error {
		if err := b.authorize(tx, actor, project, operate); err != nil {
			return err
		}
		meta := tx.Bucket([]byte(metaBucket))
		earlier, _ := parseTimestamp(string(meta.Get([]byte("last_backup"))))
		if earlier.Before(record.CreatedAt) {
			if err := meta.Put([]byte("last_backup"), []byte(FormatUTC(record.CreatedAt))); err != nil {
				return err
			}
		}
		return b.audit(tx, actor, "backup.create", "success", name, record.SHA256)
	})
	return record, err
}

func checkBackup(path string) error {
	db, err := bolt.Open(path, 0600, &bolt.Options{ReadOnly: true, Timeout: 2 * time.Second})
	if err != nil {
		return err
	}
	return errors.Join(db.View(validateStore), db.Close())
}

func RestoreBackup(source, destination string, settings Settings) (result RestoreResult, restoreErr error) {
	contents, err := os.Open(source)
	if err != nil {
		return RestoreResult{}, err
	}
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			err = &os.PathError{Op: "restore", Path: destination, Err: os.ErrExist}
		}
		return RestoreResult{}, errors.Join(err, contents.Close())
	}
	stageDir, err := os.MkdirTemp(filepath.Dir(destination), ".reb-restore-")
	if err != nil {
		return RestoreResult{}, errors.Join(err, contents.Close())
	}
	defer func() { restoreErr = errors.Join(restoreErr, os.Remove(stageDir)) }()

	stagePath := filepath.Join(stageDir, "store.db")
	target, err := os.OpenFile(stagePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return RestoreResult{}, errors.Join(err, contents.Close())
	}
	defer func() { restoreErr = errors.Join(restoreErr, os.Remove(stagePath)) }()
	hash := sha256.New()
	_, copyError := io.Copy(io.MultiWriter(target, hash), contents)
	err = errors.Join(copyError, contents.Close(), target.Sync(), target.Close())
	if err != nil {
		return RestoreResult{}, err
	}
	sidecar, sideError := os.ReadFile(source + ".sha256")
	expectedHash := strings.TrimSuffix(strings.TrimSuffix(string(sidecar), "\n"), "\r")
	checksumBad := sideError != nil || !digestPattern.MatchString(expectedHash) || expectedHash != hex.EncodeToString(hash.Sum(nil))
	if sideError != nil && !errors.Is(sideError, os.ErrNotExist) {
		return RestoreResult{}, sideError
	}
	app, err := openStore(stagePath, settings, requireExistingStore)
	if err != nil {
		return RestoreResult{}, err
	}
	result = RestoreResult{RestoredAt: app.now()}
	err = app.db.Update(func(tx *bolt.Tx) error {
		check := app.validateCurrentStore(tx)
		meta := tx.Bucket([]byte(metaBucket))
		// A checksum of a derived copy verifies its bytes. Its prior failed
		// backup verification still requires independently verified recovery.
		result.IntegrityOK = check == nil && !checksumBad && !requiresVerifiedBackup(meta)
		reason := "restored"
		if !result.IntegrityOK {
			reason = unverifiedBackupHold
		}
		if err := meta.Put([]byte("assessment_hold"), []byte(reason)); err != nil {
			return err
		}
		if err := meta.Put([]byte("last_restore"), []byte(FormatUTC(result.RestoredAt))); err != nil {
			return err
		}
		outcome := "success"
		if !result.IntegrityOK {
			outcome = "failure"
		}
		return app.audit(tx, "operator-procedure", "backup.restore", outcome)
	})
	if err == nil {
		err = app.db.Sync()
	}
	if err := errors.Join(err, app.Close()); err != nil {
		return RestoreResult{}, err
	}
	// Linking publishes the complete held store in one operation without
	// replacing another writer's destination. The stage is on the same
	// filesystem and its name is removed independently of publication.
	if err := os.Link(stagePath, destination); err != nil {
		return RestoreResult{}, fmt.Errorf("publish prepared restore: %w", err)
	}
	return result, nil
}

func (b *Board) RecordRestoreDrill(actor, project, restoredPath string) error {
	if err := checkBackup(restoredPath); err != nil {
		return b.failedChange(actor, "backup.drill", err)
	}
	return b.change(actor, "backup.drill", func(tx *bolt.Tx) error {
		if err := b.authorize(tx, actor, project, operate); err != nil {
			return err
		}
		if err := tx.Bucket([]byte(metaBucket)).Put([]byte("last_drill"), []byte(FormatUTC(b.now()))); err != nil {
			return err
		}
		return b.audit(tx, actor, "backup.drill", "success")
	})
}

func (b *Board) ExpireBackups(actor, project, dir string) (int, error) {
	b.recoveryMu.Lock()
	defer b.recoveryMu.Unlock()
	location, err := filepath.Abs(dir)
	if err != nil {
		return 0, err
	}
	var pending []backupExpiry
	var minimumAge time.Duration
	if err := b.db.View(func(tx *bolt.Tx) error {
		if err := b.authorizeCustody(tx, actor, project); err != nil {
			return err
		}
		var err error
		minimumAge, pending, err = readBackupExpiryState(tx)
		return err
	}); err != nil {
		return 0, b.failedChange(actor, "backup.expire", b.recordIntegrityIssue(actor, err))
	}
	count := 0
	for _, intent := range pending {
		if intent.Directory != location {
			continue
		}
		if err := b.finishExpiry(actor, project, intent); err != nil {
			return count, err
		}
		count++
	}
	files, err := os.ReadDir(location)
	if err != nil {
		return count, b.failedChange(actor, "backup.expire", err)
	}
	cutoff := b.now().Add(-minimumAge)
	for _, file := range files {
		name := file.Name()
		created, ok := backupFileTime(name)
		if !file.Type().IsRegular() || !ok || created.After(cutoff) {
			continue
		}
		path := filepath.Join(location, name)
		if _, err := os.Stat(path + ".sha256"); err != nil {
			return count, b.failedChange(actor, "backup.expire", err)
		}
		intent := backupExpiry{Directory: location, Name: name, MinimumAge: minimumAge}
		if err := b.change(actor, "backup.expire_intent", func(tx *bolt.Tx) error {
			if err := b.authorizeCustody(tx, actor, project); err != nil {
				return err
			}
			return b.publishBackupExpiryIntent(tx, actor, intent)
		}); err != nil {
			return count, b.recordIntegrityIssue(actor, err)
		}
		if err := b.finishExpiry(actor, project, intent); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}

// readBackupExpiryState captures policy and durable work in one native snapshot.
func readBackupExpiryState(tx *bolt.Tx) (time.Duration, []backupExpiry, error) {
	config, err := readOperatingConfig(tx)
	if err != nil {
		return 0, nil, err
	}
	pending, err := listJSON[backupExpiry](tx, expiryBucket)
	return config[backupExpiryAge], pending, err
}

func (b *Board) publishBackupExpiryIntent(tx *bolt.Tx, actor string, intent backupExpiry) error {
	if err := putJSON(tx, expiryBucket, intent.key(), intent); err != nil {
		return err
	}
	return b.audit(tx, actor, "backup.expire_intent", "success", intent.Name)
}

type backupExpiry struct {
	Directory, Name string
	MinimumAge      time.Duration `json:",omitempty"`
}

func (e *backupExpiry) UnmarshalJSON(raw []byte) error {
	var record struct {
		Directory, Name string
		MinimumAge      json.RawMessage
	}
	if err := json.Unmarshal(raw, &record); err != nil {
		return err
	}
	var minimumAge time.Duration
	if record.MinimumAge != nil {
		if err := json.Unmarshal(record.MinimumAge, &minimumAge); err != nil {
			return err
		}
		if minimumAge <= 0 {
			return integrity("recorded backup expiry duration must be positive")
		}
	}
	*e = backupExpiry{Directory: record.Directory, Name: record.Name, MinimumAge: minimumAge}
	return nil
}

func (e backupExpiry) key() string {
	location := e.Directory + "\x00" + e.Name
	if e.MinimumAge == 0 {
		return digest([]byte(location))
	}
	// Bind current intents to the approved duration in integer nanoseconds.
	return digest([]byte("reb-backup-expiry/2\x00" + location + "\x00" + strconv.FormatInt(int64(e.MinimumAge), 10)))
}

func checkBackupExpiry(key string, intent backupExpiry) error {
	if _, ok := backupFileTime(intent.Name); !ok || !filepath.IsAbs(intent.Directory) ||
		filepath.Clean(intent.Directory) != intent.Directory || intent.key() != key {
		return integrity("backup expiry journal does not match its location and authorized duration")
	}
	return nil
}

func backupFileTime(name string) (time.Time, bool) {
	const prefix = "reb-backup-"
	if len(name) != len(prefix)+39 || !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".db") {
		return time.Time{}, false
	}
	parts := name[len(prefix):]
	_, keyError := hex.DecodeString(parts[24:36])
	created, err := time.Parse("20060102T150405.000000Z", parts[:23])
	return created, err == nil && keyError == nil && parts[23] == '-'
}

func (b *Board) finishExpiry(actor, project string, intent backupExpiry) error {
	err := b.change(actor, "backup.expire", func(tx *bolt.Tx) error {
		if err := b.authorizeCustody(tx, actor, project); err != nil {
			return err
		}
		return b.finishExpiryTransaction(tx, actor, intent)
	})
	return b.recordIntegrityIssue(actor, err)
}

// finishExpiryTransaction consumes a previously durable exact intent. Failed
// metadata deletion leaves that intent available for a later verified retry.
func (b *Board) finishExpiryTransaction(tx *bolt.Tx, actor string, intent backupExpiry) error {
	stored, err := getJSON[backupExpiry](tx, expiryBucket, intent.key())
	if err != nil || stored != intent {
		return integrity("pending backup expiry no longer matches its recorded intent")
	}
	created, _ := backupFileTime(intent.Name)
	age := stored.MinimumAge
	if age == 0 {
		// Legacy intents used a fixed 30-day eligibility rule.
		age = defaultBackupExpiry
	}
	if created.After(b.now().Add(-age)) {
		return invalid("pending backup is not old enough to expire")
	}
	// The intent is already durable. If removal or this transaction fails,
	// the next verified attempt can finish removing the two files.
	path := filepath.Join(intent.Directory, intent.Name)
	for _, file := range []string{path, path + ".sha256"} {
		if err := b.removeBackupFile(file); err != nil {
			return err
		}
	}
	if err := tx.Bucket([]byte(expiryBucket)).Delete([]byte(intent.key())); err != nil {
		return err
	}
	return b.audit(tx, actor, "backup.expire", "success", intent.Name)
}

func (b *Board) removeBackupFile(path string) error {
	info, err := b.expiryLstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("backup expiry found a non-file companion")
	}
	return os.Remove(path)
}
