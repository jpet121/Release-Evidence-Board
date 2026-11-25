package browser

import (
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"testing"
)

type sessionReply struct {
	status         int
	location, body string
}

func sessionRequest(t *testing.T, client *http.Client, address string, form url.Values) sessionReply {
	t.Helper()
	var response *http.Response
	var err error
	if form == nil {
		response, err = client.Get(address)
	} else {
		response, err = client.PostForm(address, form)
	}
	if err != nil {
		t.Fatal(err)
	}
	raw, readErr := io.ReadAll(response.Body)
	if err := errors.Join(readErr, response.Body.Close()); err != nil {
		t.Fatal(err)
	}
	return sessionReply{response.StatusCode, response.Header.Get("Location"), string(raw)}
}

func sessionCSRF(t *testing.T, body string) string {
	t.Helper()
	match := regexp.MustCompile(`name="_csrf" value="([^"]+)"`).FindStringSubmatch(body)
	if len(match) != 2 {
		t.Fatal("public form lacks CSRF")
	}
	return match[1]
}

func sessionCookie(t *testing.T, jar http.CookieJar, origin string) string {
	t.Helper()
	address, err := url.Parse(origin)
	if err != nil {
		t.Fatal(err)
	}
	for _, cookie := range jar.Cookies(address) {
		if cookie.Name == "reb_session" {
			return cookie.Value
		}
	}
	return ""
}
