package pagerdutyalert

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/internal/plugin"
	"github.com/cleat-team/cleat/internal/host"
)

func TestInfo(t *testing.T) {
	p := &Plugin{}
	info := p.Info()
	if info.Name != "pagerduty-alert" {
		t.Errorf("expected Name 'pagerduty-alert', got %q", info.Name)
	}
	if info.Version != "0.1.0" {
		t.Errorf("expected Version '0.1.0', got %q", info.Version)
	}
	if info.Description == "" {
		t.Error("expected non-empty Description")
	}
}

func TestInit(t *testing.T) {
	p := &Plugin{}
	env := &plugin.Environment{}
	err := p.Init(context.Background(), env)
	if err != nil {
		t.Fatalf("Init() returned error: %v", err)
	}
	if p.httpClient == nil {
		t.Error("expected httpClient to be set")
	}
	if p.httpClient.Timeout != 30e9 {
		t.Errorf("expected httpClient timeout 30s, got %v", p.httpClient.Timeout)
	}
	if p.logger == nil {
		t.Error("expected logger to be set")
	}
}

func TestInitWithConfig(t *testing.T) {
	p := &Plugin{}
	env := &plugin.Environment{
		Config: []byte(`{}`),
	}
	err := p.Init(context.Background(), env)
	if err != nil {
		t.Fatalf("Init() returned error: %v", err)
	}
}

func TestInitInvalidConfig(t *testing.T) {
	p := &Plugin{}
	env := &plugin.Environment{
		Config: []byte(`not valid json`),
	}
	err := p.Init(context.Background(), env)
	if err == nil {
		t.Fatal("expected error for invalid config, got nil")
	}
}

func TestHealthExists(t *testing.T) {
	// Verify Health method exists on the type by calling it and checking that
	// nil db causes a panic (the framework always provides a valid db after Init).
	p := &Plugin{}
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic from Health() with nil db (nil *sql.DB dereference)")
		}
	}()
	p.Health()
}

func TestRegisterRoutes(t *testing.T) {
	p := &Plugin{}
	mux := http.NewServeMux()
	err := p.RegisterRoutes(mux)
	if err != nil {
		t.Fatalf("RegisterRoutes() returned error: %v", err)
	}

	// Verify routes are registered by making requests.
	tests := []struct {
		method string
		path   string
	}{
		{"POST", "/pagerduty/configs"},
		{"GET", "/pagerduty/configs"},
		{"GET", "/pagerduty/configs/11111111-1111-1111-1111-111111111111"},
		{"PUT", "/pagerduty/configs/11111111-1111-1111-1111-111111111111"},
		{"DELETE", "/pagerduty/configs/11111111-1111-1111-1111-111111111111"},
	}
	for _, tt := range tests {
		req := httptest.NewRequest(tt.method, tt.path, nil)
		_, pattern := mux.Handler(req)
		if pattern == "" {
			t.Errorf("no handler matched %s %s", tt.method, tt.path)
		}
	}
}

func TestRegisterRoutesNilMux(t *testing.T) {
	p := &Plugin{}
	err := p.RegisterRoutes(nil)
	if err == nil {
		t.Error("expected error for nil mux, got nil")
	}
}

func TestMigrations(t *testing.T) {
	p := &Plugin{}
	migrations := p.Migrations()
	if len(migrations) == 0 {
		t.Fatal("expected at least one migration")
	}

	// Verify the migration creates the pd_config table.
	up := migrations[0].Up
	if up == "" {
		t.Error("expected non-empty Up SQL")
	}
	if migrations[0].Version != 1 {
		t.Errorf("expected version 1, got %d", migrations[0].Version)
	}

	// Check for key SQL keywords.
	if !contains(up, "pd_config") {
		t.Error("migration should reference pd_config table")
	}
	if !contains(up, "tenant_id") {
		t.Error("migration should include tenant_id")
	}
	if !contains(up, "routing_key") {
		t.Error("migration should include routing_key")
	}
}

func TestRegisterHostFunctions(t *testing.T) {
	p := &Plugin{}
	scope := &mockRegistry{}
	err := p.RegisterHostFunctions(scope)
	if err != nil {
		t.Fatalf("RegisterHostFunctions() returned error: %v", err)
	}

	// Verify the expected functions were registered.
	expected := map[string]bool{"trigger_incident": false, "resolve_incident": false}
	for _, f := range scope.functions {
		expected[f] = true
	}
	for name, found := range expected {
		if !found {
			t.Errorf("expected host function %q not registered", name)
		}
	}
}

func TestRegisterHostFunctionsNilScope(t *testing.T) {
	p := &Plugin{}
	err := p.RegisterHostFunctions(nil)
	if err == nil {
		t.Error("expected error for nil scope, got nil")
	}
}

// ---- mocks ----

type mockRegistry struct {
	functions []string
}

func (m *mockRegistry) Register(opts plugin.FuncOptions, fn plugin.PluginFunc) error {
	m.functions = append(m.functions, opts.Name)
	return nil
}

// callCountRegistry fails Register on the Nth call (1-indexed).
type callCountRegistry struct {
	count  int
	failOn int
	err    error
}

func (r *callCountRegistry) Register(_ plugin.FuncOptions, _ plugin.PluginFunc) error {
	r.count++
	if r.count >= r.failOn {
		return r.err
	}
	return nil
}

// ---- helpers ----

func contains(s, substr string) bool {
	return len(s) >= len(substr) && containsStr(s, substr)
}

func containsStr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Health test — minimal fake SQL driver returning COUNT(*) = 1
// ---------------------------------------------------------------------------

type healthFakeConn struct{}

func (*healthFakeConn) Prepare(_ string) (driver.Stmt, error) {
	return nil, fmt.Errorf("healthFakeConn: unexpected Prepare")
}
func (*healthFakeConn) Close() error                                     { return nil }
func (*healthFakeConn) Begin() (driver.Tx, error)                        { return nil, fmt.Errorf("healthFakeConn: no tx") }

func (c *healthFakeConn) QueryContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Rows, error) {
	return &healthFakeRows{}, nil
}

var _ driver.QueryerContext = (*healthFakeConn)(nil)

type healthFakeRows struct{ pos int }

func (r *healthFakeRows) Columns() []string { return []string{"count"} }
func (r *healthFakeRows) Close() error      { return nil }
func (r *healthFakeRows) Next(dest []driver.Value) error {
	if r.pos > 0 {
		return io.EOF
	}
	r.pos++
	dest[0] = int64(1)
	return nil
}

type healthFakeConnector struct{}

func (*healthFakeConnector) Connect(_ context.Context) (driver.Conn, error) { return &healthFakeConn{}, nil }
func (*healthFakeConnector) Driver() driver.Driver                          { return &healthFakeDrv{} }

type healthFakeDrv struct{}

func (*healthFakeDrv) Open(_ string) (driver.Conn, error) { return nil, fmt.Errorf("use sql.OpenDB") }

func TestInitWithEnvLogger(t *testing.T) {
	p := &Plugin{}
	env := &plugin.Environment{
		Logger: slog.Default(),
	}
	err := p.Init(context.Background(), env)
	if err != nil {
		t.Fatalf("Init() returned error: %v", err)
	}
	if p.httpClient == nil {
		t.Error("expected httpClient to be set")
	}
	if p.logger == nil {
		t.Error("expected logger to be set after Init")
	}
}

func TestRegisterHostFunctionsSecondError(t *testing.T) {
	p := &Plugin{}
	scope := &callCountRegistry{failOn: 2, err: fmt.Errorf("second register failed")}
	err := p.RegisterHostFunctions(scope)
	if err == nil {
		t.Fatal("expected error from second register call")
	}
	if !strings.Contains(err.Error(), "second register failed") {
		t.Errorf("expected 'second register failed', got: %v", err)
	}
}

func TestHealthSuccess(t *testing.T) {
	fakeDB := sql.OpenDB(&healthFakeConnector{})
	defer fakeDB.Close()

	p := &Plugin{db: &host.SQLDBAdapter{DB: fakeDB}, logger: slog.Default()}
	err := p.Health()
	if err != nil {
		t.Errorf("Health() returned unexpected error: %v", err)
	}
}

type healthErrorFakeConn struct{}

func (*healthErrorFakeConn) Prepare(_ string) (driver.Stmt, error) { return nil, fmt.Errorf("unexpected Prepare") }
func (*healthErrorFakeConn) Close() error                          { return nil }
func (*healthErrorFakeConn) Begin() (driver.Tx, error)             { return nil, fmt.Errorf("no tx") }

func (c *healthErrorFakeConn) QueryContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Rows, error) {
	return nil, fmt.Errorf("db connection failed")
}

var _ driver.QueryerContext = (*healthErrorFakeConn)(nil)

type healthErrorFakeConnector struct{}

func (*healthErrorFakeConnector) Connect(_ context.Context) (driver.Conn, error) { return &healthErrorFakeConn{}, nil }
func (*healthErrorFakeConnector) Driver() driver.Driver                          { return &healthErrorFakeDrv{} }

type healthErrorFakeDrv struct{}

func (*healthErrorFakeDrv) Open(_ string) (driver.Conn, error) { return nil, fmt.Errorf("use sql.OpenDB") }

func TestHealthDBError(t *testing.T) {
	fakeDB := sql.OpenDB(&healthErrorFakeConnector{})
	defer fakeDB.Close()

	p := &Plugin{db: &host.SQLDBAdapter{DB: fakeDB}, logger: slog.Default()}
	err := p.Health()
	if err == nil {
		t.Fatal("expected error from Health() with failing DB")
	}
	if !strings.Contains(err.Error(), "health check") {
		t.Errorf("expected health check error, got: %v", err)
	}
}
