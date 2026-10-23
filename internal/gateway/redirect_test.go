package gateway

import (
	"net/http"
	"strings"
	"testing"
)

// An upstream that redirects is a misconfigured endpoint: the redirect is not
// followed (the target sees nothing), the attempt counts as unreachable and
// the request fails over to the next target.
func TestRedirectingUpstreamFailsOverWithoutFollowing(t *testing.T) {
	for _, code := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		elsewhere := newFake(t, okJSON("from the redirect target"))
		a := newFake(t, func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, elsewhere.srv.URL+r.URL.Path, code)
		})
		b := newFake(t, okJSON("from b"))
		e := newEnv(t, "", a, b)
		resp, body := e.post(t, simple)
		if resp.StatusCode != 200 || !strings.Contains(body, "from b") {
			t.Fatalf("%d: %d %s", code, resp.StatusCode, body)
		}
		if got := resp.Header.Get("X-Cbrt-Attempts"); got != "a:m1:unreachable,b:m2:ok" {
			t.Errorf("%d: attempts %q", code, got)
		}
		if n := elsewhere.calls.Load(); n != 0 {
			t.Errorf("%d: redirect target received %d requests", code, n)
		}
		found := false
		for _, ev := range e.hub.Recent(10) {
			if strings.Contains(ev.Detail, "redirects are not followed") {
				found = true
			}
		}
		if !found {
			t.Errorf("%d: no event explains the redirect: %+v", code, e.hub.Recent(10))
		}
	}
}
