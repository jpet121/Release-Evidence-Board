package board_test

import (
	"bytes"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
	"release-evidence-board/internal/board"
)

func TestTicket80SettingsBindingRetainsAuthenticCompanionAndBlocksDamagedPrimary(t *testing.T) {
	for _, damage := range []string{"missing", "altered"} {
		for _, section := range []string{"rate", "durations", "both"} {
			t.Run(damage+"/"+section, func(t *testing.T) {
				app, path := openBoard(t, func() time.Time { return fixtureTime })
				candidate := fixtureCandidate(t, app, vectorBaseline())
				at := board.FormatUTC(fixtureTime)
				raw := csvBytes(t, resultRow(candidate, "T1", "1", "BINDING-RUN", "BINDING-RESULT", at, at, "PASS"))
				receipt := importBytes(t, app, candidate.ID, raw)
				frozen, err := app.Freeze(supervisor, project, candidate.ID)
				if err != nil {
					t.Fatal(err)
				}
				metadata, html, err := app.ReportHTML(supervisor, project, frozen.ID)
				if err != nil {
					t.Fatal(err)
				}
				wantPolicy := board.ImportRatePolicy{Count: 7, Window: 3 * time.Minute}
				if err := app.SetImportRate(supervisor, project, wantPolicy); err != nil {
					t.Fatal(err)
				}
				if err := app.SetOperatingDuration(supervisor, project, "backup-interval", 48*time.Hour); err != nil {
					t.Fatal(err)
				}
				if err := app.Close(); err != nil {
					t.Fatal(err)
				}
				original := make(map[string][]byte)
				companions := make(map[string][]byte)
				editClosedStore := func(restoreAuthenticBytes bool) {
					t.Helper()
					db, err := bolt.Open(path, 0600, nil)
					if err != nil {
						t.Fatal(err)
					}
					defer func() {
						if err := db.Close(); err != nil {
							t.Error(err)
						}
					}()
					err = db.Update(func(tx *bolt.Tx) error {
						meta := tx.Bucket([]byte("meta"))
						for _, record := range []struct{ section, key, binding, before, after string }{
							{"rate", "import_rate_policy", "import_rate_policy_sha256", `"count":7`, `"count":8`},
							{"durations", "operating_config", "operating_config_sha256", `"backup-interval":` + strconv.FormatInt(int64(48*time.Hour), 10), `"backup-interval":` + strconv.FormatInt(int64(49*time.Hour), 10)},
						} {
							if section != "both" && section != record.section {
								continue
							}
							key, binding := []byte(record.key), []byte(record.binding)
							if restoreAuthenticBytes {
								if !bytes.Equal(meta.Get(binding), companions[record.binding]) {
									return errors.New("the authentic companion changed during the custody check")
								}
								if err := meta.Put(key, original[record.key]); err != nil {
									return err
								}
								continue
							}
							original[record.key] = bytes.Clone(meta.Get(key))
							companions[record.binding] = bytes.Clone(meta.Get(binding))
							if len(original[record.key]) == 0 || len(companions[record.binding]) != 64 {
								return errors.New("public setters did not create the required authentic pair")
							}
							if damage == "missing" {
								if err := meta.Delete(key); err != nil {
									return err
								}
							} else {
								if bytes.Count(original[record.key], []byte(record.before)) != 1 {
									return errors.New("stored setting does not have the expected independently chosen value")
								}
								changed := bytes.Replace(original[record.key], []byte(record.before), []byte(record.after), 1)
								if err := meta.Put(key, changed); err != nil {
									return err
								}
							}
							if !bytes.Equal(meta.Get(binding), companions[record.binding]) {
								return errors.New("fixture altered the authentic companion")
							}
						}
						return nil
					})
					if err != nil {
						t.Fatal(err)
					}
				}
				editClosedStore(false) // Inject the native primary fault.
				app = ticket80Reopen(t, path)
				config, policy, err := app.OperatingConfigSnapshot(supervisor, project)
				if !errors.Is(err, board.ErrIntegrity) || config != nil || policy != (board.ImportRatePolicy{}) {
					t.Fatalf("damaged setting escaped custody or exposed partial output: %v %+v %v", config, policy, err)
				}
				want := "import rate policy"
				if section == "durations" {
					want = "operating configuration"
				}
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("rate-before-duration error precedence changed: %v", err)
				}
				if held, err := app.AssessmentSuspended(supervisor, project); err != nil || !held {
					t.Fatalf("damaged settings did not hold new assessments: %t %v", held, err)
				}
				if assessment, err := app.Assess(supervisor, project, candidate.ID); !errors.Is(err, board.ErrIntegrity) || assessment.Status == board.Sufficient {
					t.Fatalf("damaged settings exposed a sufficient assessment: %s %v", assessment.Status, err)
				}
				checkMetadata, checkHTML, err := app.ReportHTML(supervisor, project, frozen.ID)
				if err != nil || !reflect.DeepEqual(checkMetadata, metadata) || !bytes.Equal(checkHTML, html) {
					t.Fatalf("settings damage changed the frozen report: %v", err)
				}
				if err := app.Close(); err != nil {
					t.Fatal(err)
				}
				editClosedStore(true) // Restore the exact authentic primary bytes.
				app = ticket80Reopen(t, path)
				if held, err := app.AssessmentSuspended(supervisor, project); err != nil || !held {
					t.Fatalf("reopening repaired bytes lost the durable assessment hold: %t %v", held, err)
				}
				if err := app.Revalidate(supervisor, project); err != nil {
					t.Fatal(err)
				}
				if held, err := app.AssessmentSuspended(supervisor, project); err != nil || held {
					t.Fatalf("successful revalidation did not release the assessment hold: %t %v", held, err)
				}
				config, policy, err = app.OperatingConfigSnapshot(supervisor, project)
				if err != nil || config["backup-interval"] != 48*time.Hour || policy != wantPolicy {
					t.Fatalf("restoring authentic bytes did not recover the original settings: %v %+v %v", config, policy, err)
				}
				stored, err := app.SourceCSV(supervisor, project, receipt.ID)
				if err != nil || !bytes.Equal(stored, raw) {
					t.Fatalf("settings fault changed unrelated source evidence: %v", err)
				}
			})
		}
	}
}
