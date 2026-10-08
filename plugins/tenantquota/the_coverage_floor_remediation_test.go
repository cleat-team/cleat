package tenantquota

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/plugin"
)

// cleat#2487. The three functions below were 0% covered: statusRecorder.Write
// (only Flush and WriteHeader had their own test), isDuplicateKey (the
// collision check that guards the only INSERT this plugin issues), and
// Init (both the happy path and the nil-DB refusal #1588's reasoning asks
// for).

func TestTheQuotaStatusRecorderWritesThroughAndCountsAsTheImplicitHeader(t *testing.T) {
	inner := httptest.NewRecorder()
	rec := &statusRecorder{ResponseWriter: inner, status: http.StatusOK}

	n, err := rec.Write([]byte("hello"))
	if err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	if n != 5 {
		t.Errorf("Write returned n=%d, want 5", n)
	}
	if inner.Body.String() != "hello" {
		t.Errorf("inner writer got %q, want %q -- Write did not pass through", inner.Body.String(), "hello")
	}
	if !rec.wroteHeader {
		t.Error("a Write with no prior WriteHeader sends an implicit 200, so it counts as the header written")
	}
}

func TestIsDuplicateKey(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"postgres", errors.New(`pq: duplicate key value violates unique constraint "bucket_pkey"`), true},
		{"postgres code", errors.New("ERROR: 23505: ..."), true},
		{"mysql", errors.New("Error 1062: Duplicate entry '1' for key 'PRIMARY'"), true},
		{"mysql code", errors.New("Error 1062 (23000)"), true},
		{"mssql", errors.New("Violation of PRIMARY KEY constraint"), true},
		{"mssql code", errors.New("The statement has been terminated., 2627"), true},
		{"unrelated", errors.New("connection refused"), false},
		{"wrapped unrelated", sql.ErrNoRows, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isDuplicateKey(c.err); got != c.want {
				t.Errorf("isDuplicateKey(%v) = %t, want %t", c.err, got, c.want)
			}
		})
	}
}

func TestTenantQuotaInit(t *testing.T) {
	p := &Plugin{}
	env := &plugin.Environment{
		DB:     &engine.SQLDBAdapter{DB: &sql.DB{}},
		Logger: slog.Default(),
	}
	if err := p.Init(context.Background(), env); err != nil {
		t.Fatalf("Init() returned error: %v", err)
	}
	if p.db == nil {
		t.Error("expected db to be set after Init")
	}
	if p.logger == nil {
		t.Error("expected logger to be set after Init")
	}
}

func TestTenantQuotaInitWithNilLoggerDefaults(t *testing.T) {
	p := &Plugin{}
	env := &plugin.Environment{
		DB: &engine.SQLDBAdapter{DB: &sql.DB{}},
	}
	if err := p.Init(context.Background(), env); err != nil {
		t.Fatalf("Init() returned error: %v", err)
	}
	if p.logger == nil {
		t.Error("expected logger to default to slog.Default() when env.Logger is nil")
	}
}

// A missing database is refused, not degraded -- #1588's reasoning: a quota
// is meaningless per-process, so starting without a database would leave
// every quota silently unenforced while the operator's configuration said
// otherwise.
func TestTenantQuotaInitRefusesANilDatabase(t *testing.T) {
	p := &Plugin{}
	env := &plugin.Environment{Logger: slog.Default()}
	err := p.Init(context.Background(), env)
	if !errors.Is(err, errNoDatabase) {
		t.Fatalf("Init() with a nil DB returned %v, want errNoDatabase", err)
	}
}
