package board_test

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestAC010OfflineFrozenReportVectors(t *testing.T) {
	for _, vector := range []struct {
		name, digest string
		gap, tie     bool
	}{
		{"V03", "fb52986d99a49aeae9fe164cf1079a986629c61f5c7349bbd6943d8263ebdb40", false, false},
		{"V07", "73a57b0f6c7940c6535f2de38be12e9c308d58810c0bdf82e5c94308413e80c0", true, false},
		{"V08", "1bc6f827fe618ff74a46e21f30e34182ccf99eb7683f520ef308d580c7a96f55", true, true},
	} {
		t.Run(vector.name, func(t *testing.T) {
			now := fixtureTime
			app, _ := openBoard(t, func() time.Time { return now })
			c := fixtureCandidate(t, app, vectorBaseline())
			at := "2026-11-17T12:00:00Z"
			pass := csvBytes(t, resultRow(c, "T1", "1", "RUN1", "RES1", at, at, "PASS"))
			switch {
			case vector.tie:
				importBytes(t, app, c.ID, csvBytes(t, runOnly(c, "RUN2", at)))
				importBytes(t, app, c.ID, pass)
			default:
				importBytes(t, app, c.ID, pass)
				if vector.gap {
					importBytes(t, app, c.ID, csvBytes(t, runOnly(c, "RUN2", "2026-11-17T12:01:00Z")))
				}
			}
			if vector.gap {
				now = now.Add(time.Minute)
			}
			report, err := app.Freeze(supervisor, project, c.ID)
			if err != nil || report.EvidenceDigest != vector.digest || report.SHA256 == report.EvidenceDigest {
				t.Fatalf("report = %#v, %v; want evidence SHA-256 %s", report, err, vector.digest)
			}
			_, html, err := app.ReportHTML(supervisor, project, report.ID)
			if err != nil {
				t.Fatal(err)
			}
			fields := []string{"reb-rules/1.5", "RES1", report.EvidenceDigest, "Historical assessment", "No deployment authority", "RUN1", "run-1"}
			if vector.gap {
				fields = append(fields, "RUN_ONLY", "MISSING_SLOT", ">none<", "RUN2")
			}
			for _, field := range fields {
				if !bytes.Contains(html, []byte(field)) {
					t.Errorf("HTML lacks %q", field)
				}
			}
			if strings.Contains(strings.ToLower(string(html)), "<script") {
				t.Fatal("HTML includes executable script")
			}
			now = now.Add(25 * time.Hour)
			frozen, sameHTML, err := app.ReportHTML(supervisor, project, report.ID)
			if err != nil || frozen.SHA256 != report.SHA256 || !bytes.Equal(html, sameHTML) {
				t.Fatalf("historical HTML changed: %#v, %v", frozen, err)
			}
		})
	}
}
