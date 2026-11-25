package browser_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
	"testing"
)

func TestCandidateRawWrongTypeAtExistingIngress(t *testing.T) {
	fx := newAuthorityFixture(t)
	base, _, _, err := fx.app.Candidate("reviewer", "pilot", fx.candidateID)
	if err != nil {
		t.Fatal(err)
	}
	const endpoint = "/p/pilot/candidates"
	const fieldValue = `{"sha256":1.25}`
	fields := url.Values{
		"release": {"R49"}, "version": {"2.0"}, "baseline": {base.BaselineDigest},
		"build": {strings.Repeat("b", 64)}, "environment": {"E49"},
		"prompt": {"not_applicable"}, "model": {"not_applicable"}, "knowledge": {"not_applicable"},
	}
	assertOriginal := func(t *testing.T) {
		t.Helper()
		all := fx.state(t).candidates
		if len(all) != 1 || all[0].ID != fx.candidateID {
			t.Fatal("rejected request changed the candidate catalog")
		}
	}
	postRaw := func(t *testing.T, media string, body []byte) authorityResponse {
		t.Helper()
		r, err := http.NewRequestWithContext(t.Context(), http.MethodPost, fx.visit+endpoint, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Content-Type", media)
		resp, err := fx.reviewer.client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		data, readErr := io.ReadAll(resp.Body)
		if err := errors.Join(readErr, resp.Body.Close()); err != nil {
			t.Fatal(err)
		}
		return authorityResponse{status: resp.StatusCode, body: string(data), location: resp.Header.Get("Location")}
	}

	t.Run("raw-JSON-is-outside-the-form-contract", func(t *testing.T) {
		body := map[string]any{"_csrf": fx.reviewer.csrf}
		for key := range fields {
			body[key] = fields.Get(key)
		}
		body["build"] = json.RawMessage(fieldValue)
		wire, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		result := postRaw(t, "application/json", wire)
		if result.status != http.StatusForbidden || result.location != "" ||
			!strings.Contains(result.body, "Your current role or session cannot perform this action.") {
			t.Fatalf("JSON outside the form contract: HTTP %d location=%q", result.status, result.location)
		}
		assertOriginal(t)
		t.Log("authenticated session cookie carried literal object digest; JSON form token was not accepted, so rejection preceded manifest validation")
	})

	t.Run("JSON-valued-scalar-reaches-form-digest-validation", func(t *testing.T) {
		var body bytes.Buffer
		parts := multipart.NewWriter(&body)
		if err := parts.WriteField("_csrf", fx.reviewer.csrf); err != nil {
			t.Fatal(err)
		}
		for key := range fields {
			if key != "build" {
				if err := parts.WriteField(key, fields.Get(key)); err != nil {
					t.Fatal(err)
				}
			}
		}
		digest, err := parts.CreatePart(textproto.MIMEHeader{
			"Content-Disposition": {`form-data; name="build"`}, "Content-Type": {"application/json"},
		})
		if err != nil {
			t.Fatal(err)
		}
		_, err = io.WriteString(digest, fieldValue)
		if err := errors.Join(err, parts.Close()); err != nil {
			t.Fatal(err)
		}
		result := postRaw(t, parts.FormDataContentType(), body.Bytes())
		if result.status != http.StatusUnprocessableEntity || result.location != "" ||
			!strings.Contains(result.body, "build digest must be 64 lowercase hexadecimal characters") {
			t.Fatalf("JSON-valued form scalar: HTTP %d location=%q", result.status, result.location)
		}
		assertOriginal(t)
		t.Log("valid session and form token reached digest validation; multipart parser represented the supplied object bytes as scalar text")
	})

	t.Run("valid-form-control", func(t *testing.T) {
		result := fx.send(t, fx.reviewer, endpoint, fields, nil)
		createdID, valid := strings.CutPrefix(result.location, endpoint+"/")
		if result.status != http.StatusSeeOther || !valid || createdID == fx.candidateID {
			t.Fatalf("valid candidate control: HTTP %d location=%q", result.status, result.location)
		}
		created, manifest, _, err := fx.app.Candidate("reviewer", "pilot", createdID)
		if err != nil || created.BaselineDigest != base.BaselineDigest || manifest.Manifest.BuildDigest != fields.Get("build") ||
			len(fx.state(t).candidates) != 2 {
			t.Fatalf("valid candidate was not stored exactly: %v", err)
		}
	})
}
