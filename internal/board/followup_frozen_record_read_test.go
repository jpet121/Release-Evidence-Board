package board

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	bolt "go.etcd.io/bbolt"
)

func TestFollowupFrozenReadsSeparateMetadataAndOriginalCustody(t *testing.T) {
	for _, fault := range []string{"original only", "malformed body hash", "invalid metadata JSON", "metadata points elsewhere", "metadata lacks candidate"} {
		t.Run(fault, func(t *testing.T) {
			f := newIntakeCustodyFixture(t)
			const id = "RPT-2"
			metadata := f.report
			metadata.ID = id
			hash := f.report.SHA256
			switch fault {
			case "malformed body hash":
				hash = "invalid-hash"
			case "metadata points elsewhere":
				metadata.ID = f.report.ID
			case "metadata lacks candidate":
				metadata.CandidateID = "C-" + strings.Repeat("f", 64)
			}
			data, err := json.Marshal(metadata)
			if err != nil {
				t.Fatal(err)
			}
			if fault == "invalid metadata JSON" {
				data = []byte("{")
			}
			if fault == "original only" {
				data = nil
			}
			writeFrozenReadFixture(t, f, id, data, hash)
			got, raw, err := f.app.ReportHTML(secondActor, secondProject, id)
			if fault == "original only" || fault == "malformed body hash" {
				if !errors.Is(err, ErrIntegrity) || got.ID != "" || len(raw) != 0 {
					t.Errorf("report read trusted an unverifiable original companion: %q, %d bytes, %v", got.ID, len(raw), err)
				}
			} else {
				want := Report{ID: id, SHA256: f.report.SHA256, IntegrityWarning: true}
				if err != nil || got != want || !bytes.Equal(raw, f.html) {
					t.Errorf("report read trusted damaged metadata or lost hash-verified original: %+v, %v", got, err)
				}
			}
			if report, err := f.app.Freeze(secondActor, secondProject, f.candidate.ID); !errors.Is(err, ErrIntegrity) || report.ID != "" {
				t.Errorf("known report fault left new publication available: %q, %v", report.ID, err)
			}
			if prior, original, err := f.app.ReportHTML(secondActor, secondProject, f.report.ID); err != nil || prior != f.report || !bytes.Equal(original, f.html) {
				t.Errorf("report-custody failure changed independently retained evidence: %v", err)
			}
		})
	}
}

func TestFollowupRevalidationRefusesFrozenIDsOutsidePositiveDecimal(t *testing.T) {
	for _, id := range []string{"RPT-0", "RPT-not-a-number"} {
		t.Run(id, func(t *testing.T) {
			f := newIntakeCustodyFixture(t)
			if err := f.app.SuspendAssessments(secondActor, secondProject); err != nil {
				t.Fatal(err)
			}
			metadata := f.report
			metadata.ID = id
			data, err := json.Marshal(metadata)
			if err != nil {
				t.Fatal(err)
			}
			writeFrozenReadFixture(t, f, id, data, f.report.SHA256)
			if err := f.app.Revalidate(secondActor, secondProject); !errors.Is(err, ErrIntegrity) ||
				!strings.Contains(err.Error(), "retained frozen report identifier is not canonical") {
				t.Errorf("review improperly accepted a nonpositive or nondecimal frozen ID: %v", err)
			}
			if prior, raw, err := f.app.ReportHTML(secondActor, secondProject, f.report.ID); err != nil || prior != f.report || !bytes.Equal(raw, f.html) {
				t.Errorf("invalid report ID damaged previous verified history: %v", err)
			}
		})
	}
}

func writeFrozenReadFixture(t *testing.T, f intakeCustodyFixture, id string, metadata []byte, htmlHash string) {
	t.Helper()
	if err := f.app.db.Update(func(tx *bolt.Tx) error {
		if err := tx.Bucket([]byte(htmlBucket)).Put([]byte(id), f.html); err != nil || metadata == nil {
			return err
		}
		if err := tx.Bucket([]byte(reportBucket)).Put([]byte(id), metadata); err != nil {
			return err
		}
		return putJSON(tx, reportMetaBucket, id, frozenReportBinding{MetadataSHA256: digest(metadata), HTMLSHA256: htmlHash})
	}); err != nil {
		t.Fatal(err)
	}
}
