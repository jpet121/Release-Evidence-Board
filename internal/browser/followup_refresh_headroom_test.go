package browser_test

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func TestBrowserAssessmentRefreshLeavesRequestHeadroom(t *testing.T) {
	_, base, id := startTestBrowser(t)
	reviewer, _ := loginSession(t, base, "reviewer")
	candidate := "/p/pilot/candidates/" + id
	refresh := regexp.MustCompile(`<meta http-equiv="refresh" content="([0-9]+)">`)
	for _, page := range []struct{ path, paused string }{
		{candidate, candidate + "?refresh=off"},
		{candidate + "/behaviors?id=BEH1", candidate + "/behaviors?id=BEH1&refresh=off"},
	} {
		body, status := response(t, reviewer.Get, base+page.path)
		directives := refresh.FindAllStringSubmatch(body, -1)
		if status != http.StatusOK || len(directives) != 1 || !strings.Contains(body, id) {
			t.Fatalf("live assessment %s did not render the assigned candidate with one refresh directive: HTTP %d", page.path, status)
		}
		seconds, err := strconv.Atoi(directives[0][1])
		if err != nil || seconds <= 0 || seconds >= 60 {
			t.Fatalf("live assessment %s scheduled %ds, leaving no request headroom within the 60s limit: %v", page.path, seconds, err)
		}
		body, status = response(t, reviewer.Get, base+page.paused)
		if status != http.StatusOK || refresh.MatchString(body) || !strings.Contains(body, "Historical display") {
			t.Fatalf("paused assessment %s did not render historical context without automatic refresh: HTTP %d", page.paused, status)
		}
	}
}
