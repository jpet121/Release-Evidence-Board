package browser

import (
	"bytes"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"release-evidence-board/internal/board"
)

func TestTicket80MultipartNativeFileRefusalsNeverStageEvidence(t *testing.T) {
	for _, fault := range []string{"missing-file", "directory-read"} {
		t.Run(fault, func(t *testing.T) {
			temp := t.TempDir()
			for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
				t.Setenv(name, temp)
			}
			app := newSessionBoard(t)
			draft, err := app.CreateDraft("reviewer", "pilot", board.Baseline{ProductID: "DEMO", ID: "B1", Revision: 1,
				Behaviors: []board.Behavior{{ID: "BEH1", Revision: 1, Required: true, Text: "Reject unauthorized reads",
					Policy: board.Policy{ID: "POL1", Revision: 1, MinPassBasisPoints: 10000, MaxAgeHours: 24},
					Tests:  []board.TestPlan{{ID: "T1", ExpectedOutcome: "Access denied", MustPass: true, PlannedAttempts: 1}}}}}, board.DraftOptions{})
			if err != nil {
				t.Fatal(err)
			}
			approved, err := app.Approve("reviewer", "pilot", draft.ID, draft.Revision)
			if err != nil {
				t.Fatal(err)
			}
			candidate, reused, err := app.CreateCandidate("reviewer", "pilot", board.CandidateInput{
				BaselineDigest: approved.Descriptor.Digest, Manifest: board.Manifest{ProductID: "DEMO", ReleaseID: "R1", ApplicationVersion: "1.0.0",
					BuildDigest: strings.Repeat("a", 64), PromptDigest: "not_applicable", ModelConfigDigest: "not_applicable", KnowledgeDigest: "not_applicable", EnvironmentID: "E1"}})
			if err != nil || reused {
				t.Fatalf("real candidate setup failed: %v", err)
			}
			server := New(app, false)
			t.Cleanup(server.Close)
			web := httptest.NewServer(server)
			t.Cleanup(web.Close)
			jar, err := cookiejar.New(nil)
			if err != nil {
				t.Fatal(err)
			}
			client := &http.Client{Jar: jar, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			login := sessionRequest(t, client, web.URL+"/login", nil)
			if signed := sessionRequest(t, client, web.URL+"/login", url.Values{"actor": {"reviewer"}, "password": {"synthetic-session-password"}, "_csrf": {sessionCSRF(t, login.body)}}); signed.status != http.StatusSeeOther {
				t.Fatal("real login failed")
			}
			base := web.URL + "/p/pilot/candidates/" + candidate.ID
			intake := sessionRequest(t, client, base+"/import", nil)
			if intake.status != http.StatusOK {
				t.Fatal("real intake unavailable")
			}
			var payload bytes.Buffer
			writer := multipart.NewWriter(&payload)
			if err := writer.WriteField("_csrf", sessionCSRF(t, intake.body)); err != nil {
				t.Fatal(err)
			}
			part, err := writer.CreateFormFile("results", "synthetic.csv")
			if err != nil {
				t.Fatal(err)
			}
			stamp := time.Now().UTC().Format("2006-01-02T15:04:05.000000Z")
			csv := "schema_version,record_type,result_id,product_id,release_id,manifest_digest,baseline_digest,behavior_id,behavior_revision,policy_id,policy_revision,test_id,run_id,attempt_index,finished_at,run_finished_at,outcome,source_uri,failure_excerpt\n" +
				"reb-results/1.3,RESULT,RES1,DEMO,R1," + candidate.ManifestDigest + "," + candidate.BaselineDigest + ",BEH1,1,POL1,1,T1,RUN1,1," + stamp + "," + stamp + ",PASS,,\n"
			if _, err := part.Write([]byte(csv)); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, base+"/preview", bytes.NewReader(payload.Bytes()))
			t.Cleanup(func() {
				if err := request.Body.Close(); err != nil {
					t.Error(err)
				}
			})
			request.Header.Set("Content-Type", writer.FormDataContentType())
			request.Header.Set("Origin", web.URL)
			origin, err := url.Parse(web.URL)
			if err != nil {
				t.Fatal(err)
			}
			for _, cookie := range jar.Cookies(origin) {
				request.AddCookie(cookie)
			}
			// A real upstream parser may use a smaller memory budget than the
			// Board. Preserve its genuine FileHeader and form data for handling.
			if err := request.ParseMultipartForm(0); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := request.MultipartForm.RemoveAll(); err != nil {
					t.Error(err)
				}
			})
			file, _, err := request.FormFile("results")
			if err != nil {
				t.Fatal(err)
			}
			native, ok := file.(*os.File)
			if !ok {
				_ = file.Close()
				t.Fatal("standard multipart parser did not spill the real file")
			}
			path := native.Name()
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			info, err := os.Lstat(path)
			if err != nil || !info.Mode().IsRegular() || !filepath.IsAbs(path) ||
				filepath.Dir(path) != filepath.Clean(os.TempDir()) || !strings.HasPrefix(filepath.Base(path), "multipart-") {
				t.Fatal("refusing to fault a path outside the parser-owned native temporary file")
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if fault == "directory-read" {
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			probe, _, openErr := request.FormFile("results")
			var nativeErr *os.PathError
			if fault == "missing-file" {
				if !errors.As(openErr, &nativeErr) || nativeErr.Op != "open" || nativeErr.Path != path || !errors.Is(openErr, os.ErrNotExist) {
					if probe != nil {
						_ = probe.Close()
					}
					t.Fatal("missing spool did not fail at native open")
				}
			} else {
				if openErr != nil {
					t.Fatal("directory fault occurred before native read")
				}
				_, readErr := io.CopyN(io.Discard, probe, 1)
				closeErr := probe.Close()
				if !errors.As(readErr, &nativeErr) || nativeErr.Op != "read" || nativeErr.Path != path || closeErr != nil {
					t.Fatal("directory did not isolate native read refusal with successful close")
				}
			}
			t.Logf("qualified native spool %s refusal: %v", nativeErr.Op, nativeErr.Err)
			response := httptest.NewRecorder()
			server.ServeHTTP(response, request)
			if response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), "The CSV could not be read.") ||
				strings.Contains(response.Body.String(), `name="preview"`) {
				t.Fatalf("native file refusal did not yield the read-error response: HTTP%d", response.Code)
			}
			assessment, err := app.Assess("reviewer", "pilot", candidate.ID)
			if err != nil || assessment.Status != board.Blocked || assessment.LedgerHighWater != 0 || len(assessment.Behaviors[0].Runs) != 0 {
				t.Fatalf("failed upload changed evidence: %v", err)
			}
			if fault == "directory-read" {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(path, []byte(csv), 0600); err != nil {
				t.Fatal(err)
			}
			retry := httptest.NewRecorder()
			server.ServeHTTP(retry, request)
			if retry.Code != http.StatusOK || !strings.Contains(retry.Body.String(), `name="preview"`) ||
				!strings.Contains(retry.Body.String(), "Projected evidence status") {
				t.Fatal("intact same-auth retry did not produce a genuine preview")
			}
		})
	}
}
