package board

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func TestFollowupExpiryRefusesUnverifiedStoredAuthorization(t *testing.T) {
	for _, fault := range []struct {
		name            string
		approved, retry time.Duration
	}{
		{"shortened", 3 * time.Hour, 2 * time.Hour},
		{"null", 48 * 24 * time.Hour, 31 * 24 * time.Hour},
		{"explicit_zero", 48 * 24 * time.Hour, 31 * 24 * time.Hour},
		{"unbound_positive", 3 * time.Hour, 3 * time.Hour},
	} {
		t.Run(fault.name, func(t *testing.T) {
			f := newIntakeCustodyFixture(t)
			file := f.app.db.Path()
			directory := t.TempDir()
			backup, err := f.app.Backup(secondActor, secondProject, directory)
			if err != nil {
				t.Fatal(err)
			}
			companion := backup.Path + ".sha256"
			original, err := os.ReadFile(companion)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.app.SetOperatingDuration(secondActor, secondProject, "backup-expiry", fault.approved); err != nil {
				t.Fatal(err)
			}
			if err := f.app.Close(); err != nil {
				t.Fatal(err)
			}
			openAt := func(at time.Time) *Board {
				t.Helper()
				b, err := Open(file, Settings{ProjectID: secondProject, ProductID: "DEMO", Clock: func() time.Time { return at }})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := b.Close(); err != nil {
						t.Error(err)
					}
				})
				return b
			}
			// Interrupt only the second physical file removal, after the real
			// expiry authorizes and persists a replayable intent.
			parked := filepath.Join(directory, "original-checksum")
			if err := os.Rename(companion, parked); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(companion, 0700); err != nil {
				t.Fatal(err)
			}
			staged := openAt(f.at.Add(fault.approved))
			count, err := staged.ExpireBackups(secondActor, secondProject, directory)
			if count != 0 || err == nil || !strings.Contains(err.Error(), "non-file companion") {
				t.Fatalf("did not leave a genuinely authorized interrupted expiry: count=%d, %v", count, err)
			}
			if _, err := os.Stat(backup.Path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("pending expiry did not reach physical removal: %v", err)
			}
			if err := staged.Close(); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(companion); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(parked, companion); err != nil {
				t.Fatal(err)
			}
			storage, err := bolt.Open(file, 0600, &bolt.Options{Timeout: 2 * time.Second})
			if err != nil {
				t.Fatal(err)
			}
			err = storage.Update(func(tx *bolt.Tx) error {
				bucket := tx.Bucket([]byte(expiryBucket))
				cursor := bucket.Cursor()
				key, raw := cursor.First()
				if key == nil || raw == nil {
					return errors.New("fixture did not produce a stored expiry intent")
				}
				priorKey := bytes.Clone(key)
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(raw, &fields); err != nil {
					return err
				}
				var approved time.Duration
				if err := json.Unmarshal(fields["MinimumAge"], &approved); err != nil || approved != fault.approved {
					return errors.New("fixture did not journal the authorized duration")
				}
				if more, _ := cursor.Next(); more != nil {
					return errors.New("fixture should have one pending expiry")
				}
				switch fault.name {
				case "shortened":
					changed, err := json.Marshal(time.Hour)
					if err != nil {
						return err
					}
					fields["MinimumAge"] = changed
				case "null":
					fields["MinimumAge"] = json.RawMessage("null")
				case "explicit_zero":
					fields["MinimumAge"] = json.RawMessage("0")
				}
				if fault.name != "shortened" {
					// An explicitly present duration is not a legacy record,
					// even if stored under the genuine format's key.
					legacyKey := []byte(digest([]byte(directory + "\x00" + filepath.Base(backup.Path))))
					if err := bucket.Delete(priorKey); err != nil {
						return err
					}
					priorKey = legacyKey
				}
				changed, err := json.Marshal(fields)
				if err != nil {
					return err
				}
				return bucket.Put(priorKey, changed)
			})
			if err := errors.Join(err, storage.Close()); err != nil {
				t.Fatal(err)
			}
			app := openAt(f.at.Add(fault.retry))
			if held, err := app.AssessmentSuspended(secondActor, secondProject); err != nil || !held {
				t.Errorf("unverified expiry authorization did not hold the reopened board: held=%t, %v", held, err)
			}
			count, err = app.ExpireBackups(secondActor, secondProject, directory)
			if count != 0 || !errors.Is(err, ErrIntegrity) {
				t.Errorf("unverified expiry authorization replay reached deletion: count=%d, %v", count, err)
			}
			raw, err := os.ReadFile(companion)
			if err != nil || !bytes.Equal(raw, original) {
				t.Errorf("unverified expiry authorization lost the exact retained companion: %v", err)
			}
			report, html, err := app.ReportHTML(secondActor, secondProject, f.report.ID)
			if err != nil || report != f.report || !bytes.Equal(html, f.html) {
				t.Errorf("expiry authorization fault changed independently verified report bytes: %v", err)
			}
		})
	}
}
