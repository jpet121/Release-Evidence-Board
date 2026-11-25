package board

import (
	"bytes"
	"errors"
	"html/template"
	"strings"
	"testing"
	texttemplate "text/template"
)

func TestTicket80FrozenRendererFailureKeepsPriorReportAndRetries(t *testing.T) {
	f := newIntakeCustodyFixture(t)
	original := reportTemplate
	t.Cleanup(func() { reportTemplate = original })
	broken, err := template.New("assessment").Parse(`partial layout {{.MissingField}}`)
	if err != nil {
		t.Fatal(err)
	}
	reportTemplate = broken
	_, err = f.app.Freeze(secondActor, secondProject, f.candidate.ID)
	var execution texttemplate.ExecError
	if !errors.As(err, &execution) || !strings.Contains(err.Error(), "render frozen report") || !strings.Contains(err.Error(), "MissingField") {
		t.Fatalf("actual template execution failure was not returned: %v", err)
	}
	reportTemplate = original
	reports, err := f.app.Reports(secondActor, secondProject, f.candidate.ID)
	if err != nil || len(reports) != 1 || reports[0] != f.report {
		t.Fatalf("failed renderer published a partial report: %v", err)
	}
	prior, html, err := f.app.ReportHTML(secondActor, secondProject, f.report.ID)
	if err != nil || prior != f.report || !bytes.Equal(html, f.html) {
		t.Fatalf("render refusal changed prior report bytes: %v", err)
	}
	health, err := f.app.Health(secondActor, secondProject)
	if err != nil || health.ReportFailures != 1 {
		t.Fatalf("render refusal was not counted: %v", err)
	}
	audit, err := f.app.Audit(secondActor, secondProject)
	if err != nil || len(audit) == 0 || audit[len(audit)-1].Action != "report.freeze" || audit[len(audit)-1].Outcome != "failure" {
		t.Fatalf("render refusal lacked durable failure audit: %v", err)
	}
	fresh, err := f.app.Freeze(secondActor, secondProject, f.candidate.ID)
	if err != nil || fresh.ID != "RPT-2" || fresh.Status != Sufficient || fresh.LedgerHighWater != 1 {
		t.Fatalf("renderer recovery lost allocator/evidence state: %+v, %v", fresh, err)
	}
}
