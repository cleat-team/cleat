package tenantquota

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/plugin"
)

// fakeDB implements plugin.PluginDB over canned answers.
//
// Implementing the INTERFACE rather than faking a database/sql driver, which is
// what plugins/ratelimiter does. That machinery exists there because those
// tests exercise SQL; these exercise the decisions this middleware makes around
// the SQL -- soft versus hard, what counts as consumption, what happens when
// the read fails -- and a driver fake would put a SQL parser between the test
// and the behaviour under test.
//
// The SQL itself is exercised against real dialects by the plugin migration
// and harness suites; nothing here claims to have tested it.
type fakeDB struct {
	quotaRow *quota // nil => no quota configured
	quotaErr error
	used     int64
	usageErr error
	execs    int // increments observed
	execErr  error
}

func (f *fakeDB) Begin(context.Context) (plugin.PluginTx, error) { return nil, sql.ErrConnDone }
func (f *fakeDB) Ping(context.Context) error                     { return nil }
func (f *fakeDB) Query(context.Context, string, ...any) (plugin.Rows, error) {
	return nil, sql.ErrConnDone
}

func (f *fakeDB) Exec(_ context.Context, query string, _ ...any) (int64, error) {
	if f.execErr != nil {
		return 0, f.execErr
	}
	// Only the UPDATE is a real increment; the INSERT seeds a zero row.
	if strings.Contains(query, "count = count + 1") {
		f.execs++
	}
	return 1, nil
}

type fakeRow struct {
	vals []any
	err  error
}

func (r *fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	for i := range dest {
		switch d := dest[i].(type) {
		case *int64:
			*d = r.vals[i].(int64)
		case *int:
			*d = r.vals[i].(int)
		case *bool:
			*d = r.vals[i].(bool)
		}
	}
	return nil
}

func (f *fakeDB) QueryRow(_ context.Context, query string, _ ...any) plugin.RowScanner {
	if strings.Contains(query, "FROM tenant_quota ") || strings.Contains(query, "FROM tenant_quota\n") {
		if f.quotaErr != nil {
			return &fakeRow{err: f.quotaErr}
		}
		if f.quotaRow == nil {
			return &fakeRow{err: sql.ErrNoRows}
		}
		return &fakeRow{vals: []any{f.quotaRow.limitCount, f.quotaRow.windowSeconds, f.quotaRow.enforce}}
	}
	if f.usageErr != nil {
		return &fakeRow{err: f.usageErr}
	}
	return &fakeRow{vals: []any{f.used}}
}

func newPlugin(db *fakeDB) *Plugin {
	return &Plugin{db: db, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), dialect: plugin.DialectPostgres}
}

// serve drives the middleware with a start request for tenant tid, with the
// wrapped handler replying `status`.
func serve(t *testing.T, p *Plugin, status int) *httptest.ResponseRecorder {
	t.Helper()
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
	})
	req := httptest.NewRequest(http.MethodPost, "/api/workflows/my-wf/start", nil)
	req = req.WithContext(auth.WithTenantID(req.Context(), uuid.New()))
	rec := httptest.NewRecorder()
	p.Middleware(next).ServeHTTP(rec, req)
	return rec
}

// THE CASE THAT DECIDES WHERE COUNTING GOES: a start the API rejected must not
// consume quota.
//
// The obvious implementation counts before calling the handler, which is one
// statement shorter and wrong: a client looping on a malformed body would
// exhaust a month's allowance without ever starting a workflow. Nothing else in
// this file fails if counting moves before the handler -- every other case
// passes either way -- so this is the test that pins the ordering.
func TestARejectedStartDoesNotConsumeQuota(t *testing.T) {
	db := &fakeDB{quotaRow: &quota{limitCount: 100, windowSeconds: 3600}, used: 0}
	p := newPlugin(db)

	if got := serve(t, p, http.StatusBadRequest).Code; got != http.StatusBadRequest {
		t.Fatalf("handler status %d, want 400", got)
	}
	if db.execs != 0 {
		t.Errorf("a rejected start incremented the counter %d time(s), want 0.\n\n"+
			"Counting before the handler charges a tenant for starts the API "+
			"refused; a client looping on a 400 would burn the quota without "+
			"starting anything.", db.execs)
	}
}

func TestAnAcceptedStartConsumesQuota(t *testing.T) {
	db := &fakeDB{quotaRow: &quota{limitCount: 100, windowSeconds: 3600}, used: 0}
	p := newPlugin(db)

	if got := serve(t, p, http.StatusOK).Code; got != http.StatusOK {
		t.Fatalf("handler status %d, want 200", got)
	}
	if db.execs != 1 {
		t.Errorf("an accepted start incremented %d time(s), want 1", db.execs)
	}
}

// SOFT IS THE DEFAULT. Over the limit with enforce=false must still serve.
func TestOverQuotaWithoutEnforcementStillServes(t *testing.T) {
	db := &fakeDB{quotaRow: &quota{limitCount: 10, windowSeconds: 3600, enforce: false}, used: 999}
	p := newPlugin(db)

	rec := serve(t, p, http.StatusOK)
	if rec.Code != http.StatusOK {
		t.Errorf("soft quota refused a start with %d.\n\n"+
			"enforce=false must record and report without refusing -- the owner "+
			"decision on cleat#1569 is soft-first, so the counting half can be "+
			"trusted before anything refuses on it.", rec.Code)
	}
}

// HARD IS OPT-IN. Over the limit with enforce=true must refuse, and must not
// call the wrapped handler.
func TestOverQuotaWithEnforcementRefuses(t *testing.T) {
	db := &fakeDB{quotaRow: &quota{limitCount: 10, windowSeconds: 3600, enforce: true}, used: 10}
	p := newPlugin(db)

	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodPost, "/api/workflows/my-wf/start", nil)
	req = req.WithContext(auth.WithTenantID(req.Context(), uuid.New()))
	rec := httptest.NewRecorder()
	p.Middleware(next).ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("status %d, want 429", rec.Code)
	}
	if called {
		t.Error("the workflow start ran despite being over an enforced quota")
	}
	if db.execs != 0 {
		t.Errorf("a refused start incremented the counter %d time(s), want 0", db.execs)
	}
	// used == limit refuses: a limit of 10 means ten, not eleven.
}

// A tenant with no quota row is not metered at all.
//
// Counting unconditionally would fill tenant_quota_counter for every
// deployment that never asked for quotas, which is a cost paid by everyone to
// serve the few who configured one.
func TestNoQuotaConfiguredIsNotMetered(t *testing.T) {
	db := &fakeDB{quotaRow: nil}
	p := newPlugin(db)

	if got := serve(t, p, http.StatusOK).Code; got != http.StatusOK {
		t.Fatalf("status %d, want 200", got)
	}
	if db.execs != 0 {
		t.Errorf("a tenant with no quota was metered %d time(s), want 0", db.execs)
	}
}

// FAILING OPEN. An unreadable quota allows the start.
//
// Same direction engine.tenantSettings takes with an unreadable settings row:
// an infrastructure problem in the metering path must not remove the ability to
// start workflows.
func TestAnUnreadableQuotaAllowsTheStart(t *testing.T) {
	for _, tc := range []struct {
		name string
		db   *fakeDB
	}{
		{"quota read fails", &fakeDB{quotaErr: sql.ErrConnDone}},
		{"usage read fails", &fakeDB{quotaRow: &quota{limitCount: 1, windowSeconds: 3600, enforce: true}, usageErr: sql.ErrConnDone}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newPlugin(tc.db)
			if got := serve(t, p, http.StatusOK).Code; got != http.StatusOK {
				t.Errorf("status %d, want 200 -- a metering failure must not refuse a start", got)
			}
		})
	}
}

// Only the start route is metered.
func TestOnlyWorkflowStartIsMetered(t *testing.T) {
	for _, tc := range []struct {
		method, path string
		want         bool
	}{
		{http.MethodPost, "/api/workflows/my-wf/start", true},
		{http.MethodPost, "/api/workflows/my-wf/start/", true},
		{http.MethodGet, "/api/workflows/my-wf/start", false},
		{http.MethodPost, "/api/workflows/my-wf/signal", false},
		{http.MethodPost, "/api/workflows/my-wf/cancel", false},
		{http.MethodPost, "/api/workflows/my-wf", false},
		{http.MethodPost, "/api/workflows/", false},
		{http.MethodPost, "/api/schedules/my-wf/start", false},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, tc.path, nil)
			if got := isWorkflowStart(r); got != tc.want {
				t.Errorf("isWorkflowStart = %v, want %v", got, tc.want)
			}
		})
	}
}
