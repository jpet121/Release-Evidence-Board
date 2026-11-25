package board

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"release-evidence-board/internal/recordset"

	bolt "go.etcd.io/bbolt"
)

var (
	ErrNotFound  = errors.New("record is unavailable")
	ErrForbidden = errors.New("this role cannot perform the action")
	ErrConflict  = errors.New("records changed; reopen and try again")
	ErrInvalid   = errors.New("the submission is invalid")
	ErrIntegrity = errors.New("evidence integrity requires revalidation")
)

const (
	metaBucket          = "meta"
	accountBucket       = "accounts"
	expiryBucket        = "backup_expiry"
	auditBucket         = "audit"
	auditMetaBucket     = "audit_metadata_sha256"
	draftBucket         = "drafts"
	draftMetaBucket     = "draft_metadata_sha256"
	baselineBucket      = "baselines"
	baselineMetaBucket  = "baseline_metadata_sha256"
	baselineRevision    = "baseline_revisions"
	behaviorRevision    = "behavior_revisions"
	policyRevision      = "policy_revisions"
	testOwners          = "test_owners"
	manifestBucket      = "manifests"
	candidateBucket     = "candidates"
	candidateMetaBucket = "candidate_metadata_sha256"
	materialBucket      = "material"
	tombstoneBucket     = "purged_candidates"
	runBucket           = "runs"
	purgedRunBucket     = "purged_runs"
	candidateRun        = "candidate_runs"
	resultBucket        = "results"
	receiptBucket       = "receipts"
	csvBucket           = "csv_bytes"
	previewBucket       = "previews"
	previewCSV          = "preview_csv"
	reportBucket        = "reports"
	reportMetaBucket    = "report_metadata_sha256"
	htmlBucket          = "report_html"
)

var buckets = []string{
	metaBucket, accountBucket, expiryBucket, auditBucket, auditMetaBucket, draftBucket, draftMetaBucket, baselineBucket, baselineMetaBucket,
	baselineRevision, behaviorRevision, policyRevision, testOwners, manifestBucket,
	candidateBucket, candidateMetaBucket, materialBucket, tombstoneBucket, runBucket, purgedRunBucket, candidateRun,
	resultBucket, receiptBucket, csvBucket, previewBucket, previewCSV, reportBucket, reportMetaBucket, htmlBucket,
}

type Settings struct {
	ProjectID string
	ProductID string
	Clock     func() time.Time
}

type Board struct {
	db            *bolt.DB
	project       string
	product       string
	clock         func() time.Time
	expiryLstat   func(string) (os.FileInfo, error)
	healthMu      sync.Mutex
	recoveryMu    sync.Mutex
	integrityHold atomic.Bool
	check         HealthCheck
}

type storeOpenMode bool

const (
	openOrCreateStore    storeOpenMode = false
	requireExistingStore storeOpenMode = true
)

func Open(path string, settings Settings) (*Board, error) {
	return openStore(path, settings, openOrCreateStore)
}

func openStore(path string, settings Settings, mode storeOpenMode) (*Board, error) {
	if !idPattern.MatchString(settings.ProjectID) || !idPattern.MatchString(settings.ProductID) {
		return nil, invalid("project and product require valid identifiers")
	}
	db, err := bolt.Open(path, 0600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("open evidence store: %w", err)
	}
	clock := settings.Clock
	if clock == nil {
		clock = time.Now
	}
	app := &Board{db: db, project: settings.ProjectID, product: settings.ProductID, clock: clock, expiryLstat: os.Lstat}
	err = db.Update(func(tx *bolt.Tx) error {
		if err := app.initialize(tx, mode); err != nil {
			return err
		}
		return removeAbandonedPreviews(tx, app.now())
	})
	if err == nil {
		err = db.View(app.validateCurrentStore)
		if errors.Is(err, ErrIntegrity) {
			// Only validation damage is recoverable as a readable, held store.
			// Record the in-process hold before attempting its durable write.
			_ = app.recordIntegrityIssue("startup", err)
			err = nil
		}
	}
	if err != nil {
		return nil, errors.Join(err, db.Close())
	}
	return app, nil
}

func (b *Board) initialize(tx *bolt.Tx, mode storeOpenMode) error {
	if meta := tx.Bucket([]byte(metaBucket)); meta != nil {
		if string(meta.Get([]byte("project"))) != b.project || string(meta.Get([]byte("product"))) != b.product {
			return invalid("project and product do not match this evidence store")
		}
		if string(meta.Get([]byte("schema"))) != "reb-store/1" {
			return invalid("unsupported evidence store schema")
		}
		for _, name := range buckets {
			if (name == expiryBucket || name == reportMetaBucket || name == candidateMetaBucket || name == auditMetaBucket ||
				name == draftMetaBucket || name == baselineMetaBucket) &&
				tx.Bucket([]byte(name)) == nil {
				if _, err := tx.CreateBucket([]byte(name)); err != nil {
					return err
				}
			}
			if tx.Bucket([]byte(name)) == nil {
				return fmt.Errorf("%w: storage section missing", ErrIntegrity)
			}
		}
		return nil
	}
	if mode == requireExistingStore {
		return invalid("restore source lacks a stored Board identity")
	}
	for _, name := range buckets {
		if _, err := tx.CreateBucket([]byte(name)); err != nil {
			return err
		}
	}
	return b.writeStoreIdentity(tx.Bucket([]byte(metaBucket)))
}

// writeStoreIdentity publishes the three identity values in the initialization
// transaction; bucket creation and open/restore mode checks stay with initialize.
func (b *Board) writeStoreIdentity(meta *bolt.Bucket) error {
	for key, value := range map[string]string{"project": b.project, "product": b.product, "schema": "reb-store/1"} {
		if err := meta.Put([]byte(key), []byte(value)); err != nil {
			return err
		}
	}
	return nil
}

func (b *Board) Close() error      { return b.db.Close() }
func (b *Board) ProjectID() string { return b.project }
func (b *Board) ProductID() string { return b.product }

func (b *Board) now() time.Time { return b.clock().UTC().Truncate(time.Microsecond) }

func invalid(message string) error { return fmt.Errorf("%w: %s", ErrInvalid, message) }

func putJSON(tx *bolt.Tx, bucket, key string, value any) error {
	return recordset.WriteJSON(tx, recordset.JSONRecord{Key: recordset.Key{Bucket: bucket, Key: key}, Value: value})
}

func putBoundJSON(tx *bolt.Tx, records, bindings, key string, value any) error {
	return recordset.WriteBoundJSON(tx, recordset.Key{Bucket: records, Key: key}, recordset.Key{Bucket: bindings, Key: key}, value)
}

func checkRecordBinding(tx *bolt.Tx, records, bindings, key string) error {
	rawKey := []byte(key)
	data := tx.Bucket([]byte(records)).Get(rawKey)
	bound := tx.Bucket([]byte(bindings)).Get(rawKey)
	if !recordset.MatchesBinding(data, bound) {
		return integrity("retained metadata differs from its integrity binding")
	}
	return nil
}

func getJSON[T any](tx *bolt.Tx, bucket, key string) (T, error) {
	var value T
	data := tx.Bucket([]byte(bucket)).Get([]byte(key))
	if data == nil {
		return value, ErrNotFound
	}
	if err := json.Unmarshal(data, &value); err != nil {
		return value, fmt.Errorf("%w: invalid storage record in %s", ErrIntegrity, bucket)
	}
	return value, nil
}

func listJSON[T any](tx *bolt.Tx, bucket string) ([]T, error) {
	var result []T
	err := visitJSON(tx, bucket, func(_ string, item T) error {
		result = append(result, item)
		return nil
	})
	return result, err
}

func visitJSON[T any](tx *bolt.Tx, bucket string, visit func(string, T) error) error {
	return tx.Bucket([]byte(bucket)).ForEach(func(key, data []byte) error {
		var item T
		if err := json.Unmarshal(data, &item); err != nil {
			return fmt.Errorf("%w: invalid storage record in %s", ErrIntegrity, bucket)
		}
		return visit(string(key), item)
	})
}

func counter(tx *bolt.Tx, key string) (uint64, error) {
	meta := tx.Bucket([]byte(metaBucket))
	raw := meta.Get([]byte(key))
	if raw == nil {
		if meta.Bucket([]byte(key)) != nil {
			return 0, integrity("storage counter is a nested bucket")
		}
		return 0, nil
	}
	value, err := strconv.ParseUint(string(raw), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: invalid storage counter", ErrIntegrity)
	}
	return value, nil
}

func increment(tx *bolt.Tx, key string) (uint64, error) {
	current, err := counter(tx, key)
	if err != nil {
		return 0, err
	}
	if current == math.MaxUint64 {
		return 0, invalid("storage counter capacity is exhausted")
	}
	next := current + 1
	return next, tx.Bucket([]byte(metaBucket)).Put([]byte(key), strconv.AppendUint(nil, next, 10))
}

type AuditEvent struct {
	Sequence   uint64
	Time       time.Time
	Actor      string
	Action     string
	References []string
	Outcome    string
}

func (b *Board) change(actor, action string, update func(*bolt.Tx) error) error {
	return b.failedChange(actor, action, b.db.Update(update))
}

func (b *Board) failedChange(actor, action string, operationError error) error {
	if operationError == nil {
		return nil
	}
	outcome := "failure"
	if errors.Is(operationError, ErrForbidden) || errors.Is(operationError, ErrNotFound) {
		outcome = "denied"
	}
	auditError := b.db.Update(func(tx *bolt.Tx) error {
		attributed := "anonymous"
		if tx.Bucket([]byte(accountBucket)).Get([]byte(actor)) != nil {
			attributed = actor
		}
		return b.audit(tx, attributed, action, outcome)
	})
	return errors.Join(operationError, auditError)
}

func (b *Board) AuditRejectedRequest(actor string, denied bool) error {
	outcome := "failure"
	if denied {
		outcome = "denied"
	}
	return b.db.Update(func(tx *bolt.Tx) error {
		attributed := "anonymous"
		if tx.Bucket([]byte(accountBucket)).Get([]byte(actor)) != nil {
			attributed = actor
		}
		return b.audit(tx, attributed, "request.reject", outcome)
	})
}

func (b *Board) audit(tx *bolt.Tx, actor, action, outcome string, references ...string) error {
	bucket := tx.Bucket([]byte(auditBucket))
	sequence, err := recordset.AdvanceSequence(bucket, invalid("audit sequence capacity is exhausted"))
	if err != nil {
		return err
	}
	event := AuditEvent{Sequence: sequence, Time: b.now(), Actor: actor, Action: action, References: references, Outcome: outcome}
	key := auditKey(sequence)
	if bucket.Get([]byte(key)) != nil || tx.Bucket([]byte(auditMetaBucket)).Get([]byte(key)) != nil {
		return integrity("audit event sequence already exists")
	}
	// Audit keys use fixed-width decimal numbers, in sequence order.
	if latest, _ := bucket.Cursor().Last(); bytes.Compare([]byte(key), latest) <= 0 {
		return integrity("audit event sequence does not follow retained events")
	}
	return putBoundJSON(tx, auditBucket, auditMetaBucket, key, event)
}

func auditKey(sequence uint64) string { return fmt.Sprintf("%020d", sequence) }

func visitAuditEvents(tx *bolt.Tx, visit func(AuditEvent) error) error {
	if err := verifyCompanionKeys(tx, auditMetaBucket, auditBucket); err != nil {
		return err
	}
	return visitJSON(tx, auditBucket, func(key string, event AuditEvent) error {
		if event.Sequence == 0 || key != auditKey(event.Sequence) {
			return integrity("retained audit event disagrees with its sequence key")
		}
		if err := checkRecordBinding(tx, auditBucket, auditMetaBucket, key); err != nil {
			return err
		}
		return visit(event)
	})
}

func (b *Board) Audit(actor, project string) ([]AuditEvent, error) {
	var result []AuditEvent
	err := b.db.View(func(tx *bolt.Tx) error {
		if err := b.authorize(tx, actor, project, operate); err != nil {
			return err
		}
		return visitAuditEvents(tx, func(event AuditEvent) error {
			result = append(result, event)
			return nil
		})
	})
	return result, b.recordIntegrityIssue(actor, err)
}
