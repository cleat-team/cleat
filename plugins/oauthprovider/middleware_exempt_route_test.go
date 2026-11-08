package oauthprovider

// cleat#3654. A request that core auth let through because its route is exempt carries no cleat credential
// by design, and the Authorization header on it is not this plugin's to judge. Before this, a session-shaped
// Bearer on POST /ingest/{id} or POST /slack/interactive was answered 401 "invalid session" here while the
// same request with no header, or with a Bearer of any other shape, reached the handler.
//
// The flag is auth.MatchedExemptRoute, and it is set only by auth.MiddlewareWithMux, so the requests below
// go through that layer first, as they do in cmd/cleat-worker.

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/plugin"
)

// countingDB records how many session lookups were attempted and answers each as a miss.
type countingDB struct {
	plugin.PluginDB
	lookups *int
}

func (d countingDB) QueryRow(_ context.Context, _ string, _ ...any) plugin.RowScanner {
	*d.lookups++
	return errRow{sql.ErrNoRows}
}

func TestASessionShapedBearerOnAnExemptRouteIsLeftAlone(t *testing.T) {
	session := strings.Repeat("ab", 32) // 64 lowercase hex: the shape looksLikeSessionToken claims

	mux := http.NewServeMux()
	ok := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	mux.Handle("POST /ingest/{source_id}", ok)
	mux.Handle("POST /slack/interactive", ok)
	mux.Handle("GET /oauth/{provider}/login", ok)
	mux.Handle("GET /api/workflows", ok)

	var lookups int
	p := &Plugin{db: countingDB{lookups: &lookups}, logger: slog.Default()}
	var reached bool
	var gotSession bool
	// core auth OUTSIDE, the plugin inside it: the order cmd/cleat-worker builds.
	chain := auth.MiddlewareWithMux(nil, true, mux,
		"POST /ingest/{source_id}", "POST /slack/interactive", "GET /oauth/{provider}/login")(
		p.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			reached = true
			_, gotSession = SessionFromContext(r.Context())
		})))

	do := func(method, target, bearer string) *httptest.ResponseRecorder {
		reached, gotSession, lookups = false, false, 0
		req := httptest.NewRequest(method, target, nil)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		w := httptest.NewRecorder()
		chain.ServeHTTP(w, req)
		return w
	}

	for _, tc := range []struct{ name, method, target, bearer string }{
		{"ingest, no credential (control)", "POST", "/ingest/s1", ""},
		{"ingest, session-shaped bearer", "POST", "/ingest/s1", session},
		{"slack interactive, session-shaped bearer", "POST", "/slack/interactive", session},
		// The /oauth/ prefix was skipped by path before this change and still is, flag or not.
		{"oauth login, session-shaped bearer", "GET", "/oauth/github/login", session},
	} {
		w := do(tc.method, tc.target, tc.bearer)
		if !reached || w.Code != http.StatusOK || lookups != 0 || gotSession {
			t.Errorf("%s: reached=%v code=%d lookups=%d session=%v; want reached, 200, no session lookup, no SessionInfo",
				tc.name, reached, w.Code, lookups, gotSession)
		}
	}

	// The flag is the ONLY thing that changed. Without it this plugin still looks the token up and still
	// refuses a session-shaped bearer it does not know, so the check has not been deleted.
	direct := p.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
	reached, lookups = false, 0
	req := httptest.NewRequest("GET", "/api/workflows", nil)
	req.Header.Set("Authorization", "Bearer "+session)
	w := httptest.NewRecorder()
	direct.ServeHTTP(w, req)
	if reached || lookups != 1 || w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "invalid session") {
		t.Errorf("an unflagged request: reached=%v lookups=%d code=%d body=%q; want refused 401 invalid session after one lookup",
			reached, lookups, w.Code, w.Body.String())
	}

	// And the /oauth/ skip holds with no flag at all, which is what makes it independent of this change.
	reached, lookups = false, 0
	req = httptest.NewRequest("GET", "/oauth/github/callback", nil)
	req.Header.Set("Authorization", "Bearer "+session)
	direct.ServeHTTP(httptest.NewRecorder(), req)
	if !reached || lookups != 0 {
		t.Errorf("/oauth/ with no flag: reached=%v lookups=%d; want reached with no lookup", reached, lookups)
	}
}
