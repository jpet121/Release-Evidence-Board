package browser_test

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"release-evidence-board/internal/board"
)

func TestBrowserCandidateRejectsFileTypedManifestInput(t *testing.T) {
	t.Run("AC-003-04.P003", func(t *testing.T) {
		app, visit, oldID := startTestBrowser(t)
		if err := app.SetAccount("reviewer", "contributor", browserPassword, []board.Role{board.Contributor}); err != nil {
			t.Fatal(err)
		}
		client, csrf := loginSession(t, visit, "contributor")
		_, _, approved, err := app.Candidate("reviewer", "pilot", oldID)
		if err != nil {
			t.Fatal(err)
		}
		before, err := app.Candidates("reviewer", "pilot")
		if err != nil || len(before) != 1 || before[0].ID != oldID {
			t.Fatalf("candidate fixture is not isolated: %v %v", before, err)
		}
		build := strings.Repeat("b", 64)
		submit := func(scalar, file bool) (string, int) {
			t.Helper()
			var body bytes.Buffer
			form := multipart.NewWriter(&body)
			fields := map[string]string{
				"_csrf": csrf, "release": "R-WIRE", "version": "2.0", "baseline": approved.Descriptor.Digest,
				"environment": "E2", "prompt": "not_applicable", "model": "not_applicable", "knowledge": "not_applicable",
			}
			if scalar {
				fields["build"] = build
			}
			for key, field := range fields {
				if err := form.WriteField(key, field); err != nil {
					t.Fatal(err)
				}
			}
			if file {
				part, err := form.CreateFormFile("build", "digest.txt")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := part.Write([]byte(build)); err != nil {
					t.Fatal(err)
				}
			}
			if err := form.Close(); err != nil {
				t.Fatal(err)
			}
			return response(t, func(endpoint string) (*http.Response, error) {
				return client.Post(endpoint, form.FormDataContentType(), &body)
			}, visit+"/p/pilot/candidates")
		}

		for _, scalar := range []bool{false, true} {
			_, status := submit(scalar, true)
			after, err := app.Candidates("reviewer", "pilot")
			if status != http.StatusUnprocessableEntity || err != nil || !reflect.DeepEqual(after, before) {
				t.Fatalf("file-typed build with scalar=%t: HTTP %d, catalog rows %d (want 422 and unchanged %d rows): %v",
					scalar, status, len(after), len(before), err)
			}
		}
		if _, status := submit(true, false); status != http.StatusSeeOther {
			t.Fatalf("same valid scalar-only multipart candidate returned HTTP %d, want 303", status)
		}
		candidates, err := app.Candidates("reviewer", "pilot")
		if err != nil || len(candidates) != 2 {
			t.Fatalf("valid scalar control did not add one candidate: %v %v", candidates, err)
		}
		newID := candidates[0].ID
		if newID == oldID {
			newID = candidates[1].ID
		}
		candidate, manifest, newBaseline, err := app.Candidate("reviewer", "pilot", newID)
		if err != nil || candidate.ID == oldID || manifest.Manifest.BuildDigest != build ||
			manifest.Manifest.ReleaseID != "R-WIRE" || newBaseline.Descriptor.Digest != approved.Descriptor.Digest {
			t.Fatalf("accepted scalar candidate did not retain its exact descriptor fields: %+v %v", manifest, err)
		}
		recordThresholdHTTPInstance(t, "AC-003-04.P003", map[string]any{"defect": "wrong type"}, "Rejected")
	})
}
