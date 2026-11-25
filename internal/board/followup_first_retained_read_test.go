package board

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	bolt "go.etcd.io/bbolt"
)

func TestFollowupFirstRetainedReadsRefuseMalformedEvidence(t *testing.T) {
	for _, field := range []string{"draft record", "receipt record", "prospective run record", "existing run original", "saved data revision", "saved ledger"} {
		t.Run(field, func(t *testing.T) {
			f := newIntakeCustodyFixture(t)
			_, _, approval, err := f.app.Candidate(secondActor, secondProject, f.candidate.ID)
			if err != nil {
				t.Fatal(err)
			}
			var section, key string
			switch field {
			case "draft record":
				section, key = draftBucket, approval.DraftID
			case "receipt record":
				section, key = receiptBucket, f.receipt.Hash
			case "prospective run record":
				section, key = runBucket, "PENDING"
			case "existing run original":
				section, key = runBucket, "PRIOR"
			case "saved data revision":
				section, key = metaBucket, "data_revision"
			case "saved ledger":
				section, key = metaBucket, "ledger"
			}
			var prior []byte
			if err := f.app.db.Update(func(tx *bolt.Tx) error {
				bucket := tx.Bucket([]byte(section))
				prior = bytes.Clone(bucket.Get([]byte(key)))
				if field == "existing run original" {
					run, err := getJSON[Run](tx, section, key)
					if err != nil {
						return err
					}
					run.FileHash = strings.Repeat("f", 64)
					return putJSON(tx, section, key, run)
				}
				return bucket.Put([]byte(key), []byte("{"))
			}); err != nil {
				t.Fatal(err)
			}
			var observed error
			switch field {
			case "draft record":
				_, observed = f.app.Draft(secondActor, secondProject, key)
			case "receipt record":
				_, observed = f.app.Receipt(secondActor, secondProject, f.receipt.ID)
			default:
				raw := f.pending
				if field == "existing run original" {
					raw = f.source
				}
				var p Preview
				p, observed = f.app.PreviewImport(secondActor, secondProject, f.candidate.ID, raw)
				if p.Token != "" || p.Projected != nil {
					t.Error("malformed retained input yielded a confirmable preview")
				}
			}
			if !errors.Is(observed, ErrIntegrity) {
				t.Errorf("malformed first retained read did not fail integrity: %v", observed)
			}
			if field == "existing run original" && (observed == nil || !strings.Contains(observed.Error(), "an existing run lacks its original receipt")) {
				t.Errorf("missing retained run receipt was not located in replanning: %v", observed)
			}
			if err := f.app.db.Update(func(tx *bolt.Tx) error {
				bucket := tx.Bucket([]byte(section))
				if prior == nil {
					return bucket.Delete([]byte(key))
				}
				return bucket.Put([]byte(key), prior)
			}); err != nil {
				t.Fatal(err)
			}
			f.checkReopened(t, true)
		})
	}
}
