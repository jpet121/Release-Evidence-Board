package board

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"slices"
	"strconv"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
	"release-evidence-board/internal/recordset"
)

type Report struct {
	ID, CandidateID, Creator, EvidenceDigest, SHA256, Status string
	AssessedAt, EarliestExpiry                               time.Time
	HasExpiry                                                bool
	LedgerHighWater                                          uint64
	IntegrityWarning                                         bool `json:"-"`
}

type frozenReportBinding = recordset.FrozenHTMLBinding

//go:embed report.html
var reportSource string

var reportTemplate = template.Must(template.New("assessment").Funcs(template.FuncMap{"utc": FormatUTC}).Parse(reportSource))

type reportPage struct {
	Report
	Assessment Assessment
	Rule       string
	Evidence   string
}

func (b *Board) Freeze(actor, project, candidateID string) (Report, error) {
	var report Report
	err := b.db.Update(func(tx *bolt.Tx) error {
		if err := b.authorize(tx, actor, project, review); err != nil {
			return err
		}
		if err := b.assessmentAvailable(tx); err != nil {
			return err
		}
		assessment, err := makeAssessment(tx, candidateID, b.now(), nil)
		if err != nil {
			return err
		}
		report, err = b.publishFrozenAssessment(tx, actor, assessment)
		return err
	})
	if err != nil {
		err = b.recordIntegrityIssue(actor, err)
		if errors.Is(err, ErrForbidden) || errors.Is(err, ErrNotFound) {
			err = b.failedChange(actor, "report.freeze", err)
		} else {
			err = errors.Join(err, b.recordReportFailure(actor))
		}
	}
	return report, err
}

// publishFrozenAssessment binds evidence, rendered HTML and metadata before the
// success audit, all inside the caller-owned transaction.
func (b *Board) publishFrozenAssessment(tx *bolt.Tx, actor string, assessment Assessment) (report Report, err error) {
	evidence, err := evidenceDescriptor(assessment)
	if err != nil {
		return report, err
	}
	metadataStore := tx.Bucket([]byte(reportBucket))
	htmlStore := tx.Bucket([]byte(htmlBucket))
	bindingStore := tx.Bucket([]byte(reportMetaBucket))
	if metadataStore == nil || htmlStore == nil || bindingStore == nil {
		return report, integrity("frozen report publication storage is missing")
	}
	sequence, err := recordset.AdvanceSequence(metadataStore, invalid("frozen report identifier capacity is exhausted"))
	if err != nil {
		return report, err
	}
	id := "RPT-" + strconv.FormatUint(sequence, 10)
	key := []byte(id)
	for _, store := range []*bolt.Bucket{metadataStore, htmlStore, bindingStore} {
		if store.Get(key) != nil || store.Bucket(key) != nil {
			return report, integrity("frozen report identifier already exists")
		}
	}
	report = Report{
		ID: id, CandidateID: assessment.Candidate.ID, Creator: actor, AssessedAt: assessment.AssessedAt,
		EvidenceDigest: evidence.Digest, EarliestExpiry: assessment.EarliestExpiry, HasExpiry: assessment.HasExpiry,
		LedgerHighWater: assessment.LedgerHighWater, Status: assessment.Status,
	}
	var html bytes.Buffer
	page := reportPage{Report: report, Assessment: assessment, Rule: RuleVersion, Evidence: evidence.Text}
	if err := reportTemplate.Execute(&html, page); err != nil {
		return report, fmt.Errorf("render frozen report: %w", err)
	}
	report.SHA256, err = recordset.WriteFrozenHTML(tx,
		recordset.FrozenHTMLStores{HTML: htmlBucket, Metadata: reportBucket, Binding: reportMetaBucket}, report.ID, html.Bytes(), func(sha string) any {
			metadata := report
			metadata.SHA256 = sha
			return metadata
		})
	if err != nil {
		return report, err
	}
	return report, b.audit(tx, actor, "report.freeze", "success", assessment.Candidate.ID, report.ID)
}

func (b *Board) recordReportFailure(actor string) error {
	return b.db.Update(func(tx *bolt.Tx) error {
		if _, err := increment(tx, "report_failures"); err != nil {
			return err
		}
		return b.audit(tx, actor, "report.freeze", "failure")
	})
}

func (b *Board) Reports(actor, project, candidateID string) ([]Report, error) {
	var reports []Report
	err := b.db.View(func(tx *bolt.Tx) error {
		if err := b.authorize(tx, actor, project, read); err != nil {
			return err
		}
		if _, err := getJSON[Candidate](tx, candidateBucket, candidateID); err != nil {
			return err
		}
		err := visitFrozenReports(tx, func(report Report) error {
			if report.CandidateID == candidateID {
				if _, err := verifiedReportHTML(tx, report); err != nil {
					return err
				}
				reports = append(reports, report)
			}
			return nil
		})
		if err != nil {
			return err
		}
		slices.SortFunc(reports, func(a, b Report) int { return b.AssessedAt.Compare(a.AssessedAt) })
		return nil
	})
	if err != nil {
		return nil, b.recordIntegrityIssue(actor, err)
	}
	return reports, nil
}

func (b *Board) ReportHTML(actor, project, id string) (Report, []byte, error) {
	var report Report
	var html []byte
	var metadataIssue error
	err := b.db.View(func(tx *bolt.Tx) error {
		if err := b.authorize(tx, actor, project, read); err != nil {
			return err
		}
		var err error
		report, err = retainedReport(tx, id)
		if errors.Is(err, ErrIntegrity) {
			metadataIssue = err
			binding, bindErr := getJSON[frozenReportBinding](tx, reportMetaBucket, id)
			if bindErr != nil || !digestPattern.MatchString(binding.HTMLSHA256) {
				return err
			}
			// Independently bound original bytes are still usable for
			// historical review. The altered metadata is omitted.
			report = Report{ID: id, SHA256: binding.HTMLSHA256, IntegrityWarning: true}
		} else if err != nil {
			return err
		}
		raw, err := verifiedReportHTML(tx, report)
		if err != nil {
			return err
		}
		html = slices.Clone(raw)
		return nil
	})
	if err != nil {
		return Report{}, nil, b.recordIntegrityIssue(actor, err)
	}
	if metadataIssue != nil {
		// recordIntegrityIssue sets the in-process hold before its
		// persistence attempt. Keep verified historical bytes available.
		_ = b.recordIntegrityIssue(actor, metadataIssue)
	}
	return report, html, nil
}

func retainedReport(tx *bolt.Tx, id string) (Report, error) {
	key := []byte(id)
	metadata := tx.Bucket([]byte(reportBucket)).Get(key)
	if metadata == nil {
		if tx.Bucket([]byte(reportMetaBucket)).Get(key) != nil || tx.Bucket([]byte(htmlBucket)).Get(key) != nil {
			return Report{}, integrity("frozen report lacks its retained metadata")
		}
		return Report{}, ErrNotFound
	}
	return verifiedReportMetadata(tx, id, metadata)
}

func visitFrozenReports(tx *bolt.Tx, visit func(Report) error) error {
	return tx.Bucket([]byte(reportBucket)).ForEach(func(key, metadata []byte) error {
		report, err := verifiedReportMetadata(tx, string(key), metadata)
		if err != nil {
			return err
		}
		return visit(report)
	})
}

func validateFrozenReports(tx *bolt.Tx, ledger uint64) error {
	last := tx.Bucket([]byte(reportBucket)).Sequence()
	return visitFrozenReports(tx, func(report Report) error {
		number, err := strconv.ParseUint(strings.TrimPrefix(report.ID, "RPT-"), 10, 64)
		if err != nil || number == 0 || report.ID != "RPT-"+strconv.FormatUint(number, 10) {
			return integrity("retained frozen report identifier is not canonical")
		}
		if number > last {
			return integrity("frozen report identifier sequence precedes retained reports")
		}
		if err := checkLedgerWitness(ledger, report.LedgerHighWater); err != nil {
			return err
		}
		_, err = verifiedReportHTML(tx, report)
		return err
	})
}

func verifiedReportMetadata(tx *bolt.Tx, id string, metadata []byte) (Report, error) {
	binding, err := getJSON[frozenReportBinding](tx, reportMetaBucket, id)
	if err != nil || binding.MetadataSHA256 != digest(metadata) {
		return Report{}, integrity("frozen report metadata hash differs")
	}
	var report Report
	if err := json.Unmarshal(metadata, &report); err != nil || report.ID != id || report.SHA256 != binding.HTMLSHA256 ||
		tx.Bucket([]byte(candidateBucket)).Get([]byte(report.CandidateID)) == nil {
		return Report{}, integrity("frozen report metadata or candidate identity is inconsistent")
	}
	return report, nil
}

func verifiedReportHTML(tx *bolt.Tx, report Report) ([]byte, error) {
	raw := tx.Bucket([]byte(htmlBucket)).Get([]byte(report.ID))
	if raw == nil || digest(raw) != report.SHA256 {
		return nil, integrity("stored HTML hash differs")
	}
	return raw, nil
}

func FormatUTC(value time.Time) string {
	timeUTC := value.UTC().Truncate(time.Microsecond)
	if timeUTC.Nanosecond() == 0 {
		return timeUTC.Format("2006-01-02T15:04:05Z")
	}
	return timeUTC.Format("2006-01-02T15:04:05.000000Z")
}

func evidenceDescriptor(assessment Assessment) (Descriptor, error) {
	var runs []Run
	for _, behavior := range assessment.Behaviors {
		for _, run := range behavior.Runs {
			runs = append(runs, run.Run)
		}
	}
	slices.SortFunc(runs, compareSequence)
	var evidence []any
	for _, run := range runs {
		rows := slices.Clone(run.Rows)
		slices.SortFunc(rows, compareSourceSlots)
		for _, source := range rows {
			row := make([]any, 0, columnCount+1)
			for _, value := range source.Fields {
				row = append(row, value)
			}
			evidence = append(evidence, append(row, run.Sequence))
		}
	}
	return describe([]any{"reb-evidence/1", assessment.Candidate.ManifestDigest, assessment.Candidate.BaselineDigest,
		RuleVersion, assessment.LedgerHighWater, FormatUTC(assessment.AssessedAt), evidence})
}

func compareSourceSlots(a, b SourceRow) int {
	if byTest := strings.Compare(a.TestID(), b.TestID()); byTest != 0 {
		return byTest
	}
	left, _ := positiveDecimal(a.Attempt())
	right, _ := positiveDecimal(b.Attempt())
	return left - right
}
