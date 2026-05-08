package oauthprovider

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rcownie/cleat/internal/plugin"
)

func TestInfo(t *testing.T) {
	p := &Plugin{}
	info := p.Info()
	if info.Name != "oauth-provider" {
		t.Errorf("expected Name 'oauth-provider', got %q", info.Name)
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
}

func TestRegisterRoutes(t *testing.T) {
	p := &Plugin{}
	mux := http.NewServeMux()
	err := p.RegisterRoutes(mux)
	if err != nil {
		t.Fatalf("RegisterRoutes() returned error: %v", err)
	}

	tests := []struct {
		method string
		path   string
	}{
		{"GET", "/oauth/google/login"},
		{"GET", "/oauth/github/login"},
		{"GET", "/oauth/okta/login"},
		{"GET", "/oauth/google/callback"},
		{"GET", "/oauth/github/callback"},
		{"GET", "/oauth/okta/callback"},
		{"GET", "/oauth/sessions"},
		{"DELETE", "/oauth/sessions/550e8400-e29b-41d4-a716-446655440000"},
	}
	for _, tt := range tests {
		req := httptest.NewRequest(tt.method, tt.path, nil)
		_, pattern := mux.Handler(req)
		if pattern == "" {
			t.Errorf("no handler matched %s %s", tt.method, tt.path)
		}
	}
}

// ---- Exported interface tests ----

func TestMigrations(t *testing.T) {
	p := &Plugin{}
	migrations := p.Migrations()
	if len(migrations) != 2 {
		t.Fatalf("expected 2 migrations, got %d", len(migrations))
	}
	if migrations[0].Version != 1 {
		t.Errorf("expected version 1, got %d", migrations[0].Version)
	}
	if !strings.Contains(migrations[0].Up, "oauth_config") {
		t.Error("expected migration 1 to mention oauth_config")
	}
	if !strings.Contains(migrations[1].Up, "token_hash") {
		t.Error("expected migration 2 to mention token_hash")
	}
}

func TestRegisterRoutesNilMux(t *testing.T) {
	p := &Plugin{}
	err := p.RegisterRoutes(nil)
	if err == nil {
		t.Fatal("expected error for nil mux, got nil")
	}
}

// ---- Utility function tests ----

func TestSHA256Hex(t *testing.T) {
	input := "hello"
	// Just verify it returns a 64-char hex string.
	result := sha256Hex(input)
	if len(result) != 64 {
		t.Errorf("sha256Hex(%q) has length %d, want 64", input, len(result))
	}
	// Verify determinism.
	a := sha256Hex("test-value")
	b := sha256Hex("test-value")
	if a != b {
		t.Errorf("sha256Hex not deterministic: %q != %q", a, b)
	}
}

func TestGenerateSessionToken(t *testing.T) {
	token, err := generateSessionToken()
	if err != nil {
		t.Fatalf("generateSessionToken() returned error: %v", err)
	}
	// Should be a 64-char hex string (32 bytes).
	if len(token) != 64 {
		t.Errorf("expected 64-char hex token, got %d chars: %q", len(token), token)
	}
}

func TestGenerateSessionTokenUnique(t *testing.T) {
	t1, _ := generateSessionToken()
	t2, _ := generateSessionToken()
	if t1 == t2 {
		t.Error("expected consecutive tokens to be different")
	}
}

func TestTenantIDNoSession(t *testing.T) {
	p := &Plugin{}
	req := httptest.NewRequest("GET", "/test", nil)
	tid := p.tenantID(req)
	if tid != uuid.Nil {
		t.Errorf("expected nil UUID when no session in context, got %v", tid)
	}
}

func TestTenantIDWithSession(t *testing.T) {
	p := &Plugin{}
	session := &SessionInfo{
		TenantID:  uuid.MustParse("00000000-0000-0000-0000-000000000001"),
		SessionID: uuid.New(),
		UserEmail: "test@example.com",
	}
	ctx := context.WithValue(context.Background(), sessionContextKey{}, session)
	req := httptest.NewRequest("GET", "/test", nil).WithContext(ctx)
	tid := p.tenantID(req)
	if tid != session.TenantID {
		t.Errorf("expected tenant %v, got %v", session.TenantID, tid)
	}
}

func TestSessionFromContext(t *testing.T) {
	info := &SessionInfo{
		TenantID:  uuid.MustParse("00000000-0000-0000-0000-000000000001"),
		SessionID: uuid.MustParse("00000000-0000-0000-0000-000000000002"),
		UserEmail: "test@example.com",
	}

	// Store and retrieve from context.
	ctx := context.WithValue(context.Background(), sessionContextKey{}, info)
	retrieved, ok := SessionFromContext(ctx)
	if !ok {
		t.Fatal("SessionFromContext returned ok=false for valid context")
	}
	if retrieved.TenantID != info.TenantID {
		t.Errorf("TenantID: got %v, want %v", retrieved.TenantID, info.TenantID)
	}
	if retrieved.SessionID != info.SessionID {
		t.Errorf("SessionID: got %v, want %v", retrieved.SessionID, info.SessionID)
	}
	if retrieved.UserEmail != info.UserEmail {
		t.Errorf("UserEmail: got %q, want %q", retrieved.UserEmail, info.UserEmail)
	}

	// Empty context should return ok=false.
	_, ok = SessionFromContext(context.Background())
	if ok {
		t.Error("SessionFromContext should return ok=false for empty context")
	}
}

func TestFormatProviderURL(t *testing.T) {
	tests := []struct {
		template string
		domain   string
		want     string
	}{
		{"https://%s/oauth2/v1/authorize", "myokta.example.com", "https://myokta.example.com/oauth2/v1/authorize"},
		{"https://accounts.google.com/o/oauth2/v2/auth", "ignored", "https://accounts.google.com/o/oauth2/v2/auth"},
		{"", "", ""},
	}
	for _, tt := range tests {
		got := formatProviderURL(tt.template, tt.domain)
		if got != tt.want {
			t.Errorf("formatProviderURL(%q, %q) = %q, want %q", tt.template, tt.domain, got, tt.want)
		}
	}
}

// ---- extractSession tests ----

// fakeSessionDB provides a minimal fake DB for testing extractSession.
type fakeSessionStore struct {
	mu     sync.Mutex
	sessions map[string]*testSessionRow // token_hash -> row
}

type testSessionRow struct {
	id        uuid.UUID
	tenantID  uuid.UUID
	userEmail sql.NullString
	expiresAt sql.NullTime
}

func newFakeSessionStore() *fakeSessionStore {
	return &fakeSessionStore{sessions: make(map[string]*testSessionRow)}
}

type fakeSessionConnector struct {
	store *fakeSessionStore
}

func (c *fakeSessionConnector) Connect(_ context.Context) (driver.Conn, error) {
	return &fakeSessionConn{store: c.store}, nil
}

func (c *fakeSessionConnector) Driver() driver.Driver {
	return &fakeSessionDriver{}
}

type fakeSessionDriver struct{}

func (*fakeSessionDriver) Open(_ string) (driver.Conn, error) {
	return nil, fmt.Errorf("fakeSessionDriver: use sql.OpenDB")
}

type fakeSessionConn struct {
	store *fakeSessionStore
}

func (*fakeSessionConn) Prepare(_ string) (driver.Stmt, error) {
	return nil, fmt.Errorf("fakeSessionConn: unexpected Prepare call")
}

func (*fakeSessionConn) Close() error                        { return nil }
func (*fakeSessionConn) Begin() (driver.Tx, error)           { return &fakeSessionTx{}, nil }

type fakeSessionTx struct{}

func (*fakeSessionTx) Commit() error   { return nil }
func (*fakeSessionTx) Rollback() error { return nil }

func (c *fakeSessionConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.store.mu.Lock()
	defer c.store.mu.Unlock()

	if strings.Contains(query, "oauth_sessions") {
		tokenHash, err := argStringSession(args, 1)
		if err != nil {
			return &fakeSessionRows{cols: []string{"id", "tenant_id", "user_email", "expires_at"}}, nil
		}
		row, ok := c.store.sessions[tokenHash]
		if !ok {
			return &fakeSessionRows{cols: []string{"id", "tenant_id", "user_email", "expires_at"}}, nil
		}
		var expiresAt driver.Value
		if row.expiresAt.Valid {
			expiresAt = row.expiresAt.Time
		}
		var email driver.Value
		if row.userEmail.Valid {
			email = row.userEmail.String
		}
		return &fakeSessionRows{
			cols: []string{"id", "tenant_id", "user_email", "expires_at"},
			data: [][]driver.Value{{row.id.String(), row.tenantID.String(), email, expiresAt}},
		}, nil
	}
	return &fakeSessionRows{cols: []string{}, data: [][]driver.Value{}}, nil
}

func argStringSession(args []driver.NamedValue, ordinal int) (string, error) {
	for _, a := range args {
		if a.Ordinal == ordinal {
			switch v := a.Value.(type) {
			case string:
				return v, nil
			case []byte:
				return string(v), nil
			default:
				return "", fmt.Errorf("arg %d: want string, got %T", ordinal, a.Value)
			}
		}
	}
	return "", fmt.Errorf("arg %d not found", ordinal)
}

type fakeSessionRows struct {
	cols []string
	data [][]driver.Value
	pos  int
}

func (r *fakeSessionRows) Columns() []string              { return r.cols }
func (r *fakeSessionRows) Close() error                    { return nil }
func (r *fakeSessionRows) Next(dest []driver.Value) error {
	if r.pos >= len(r.data) {
		return io.EOF
	}
	copy(dest, r.data[r.pos])
	r.pos++
	return nil
}

func TestExtractSessionValid(t *testing.T) {
	store := newFakeSessionStore()
	db := sql.OpenDB(&fakeSessionConnector{store: store})
	defer db.Close()

	p := &Plugin{db: db}
	sessionID := uuid.New()
	tenantID := uuid.New()
	token := "valid-session-token-123"
	tokenHash := sha256Hex(token)

	store.mu.Lock()
	store.sessions[tokenHash] = &testSessionRow{
		id:       sessionID,
		tenantID: tenantID,
		userEmail: sql.NullString{String: "user@example.com", Valid: true},
		expiresAt: sql.NullTime{Time: time.Now().Add(1 * time.Hour), Valid: true},
	}
	store.mu.Unlock()

	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	session := p.extractSession(req)
	if session == nil {
		t.Fatal("extractSession returned nil for valid token")
	}
	if session.TenantID != tenantID {
		t.Errorf("TenantID: got %v, want %v", session.TenantID, tenantID)
	}
	if session.SessionID != sessionID {
		t.Errorf("SessionID: got %v, want %v", session.SessionID, sessionID)
	}
	if session.UserEmail != "user@example.com" {
		t.Errorf("UserEmail: got %q, want %q", session.UserEmail, "user@example.com")
	}
}

func TestExtractSessionNoAuthHeader(t *testing.T) {
	p := &Plugin{}
	req := httptest.NewRequest("GET", "/test", nil)
	session := p.extractSession(req)
	if session != nil {
		t.Error("expected nil session for missing auth header")
	}
}

func TestExtractSessionEmptyToken(t *testing.T) {
	p := &Plugin{}
	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "Bearer ")
	session := p.extractSession(req)
	if session != nil {
		t.Error("expected nil session for empty token")
	}
}

func TestExtractSessionInvalidToken(t *testing.T) {
	store := newFakeSessionStore()
	db := sql.OpenDB(&fakeSessionConnector{store: store})
	defer db.Close()

	p := &Plugin{db: db}
	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "Bearer nonexistent-token")

	session := p.extractSession(req)
	if session != nil {
		t.Error("expected nil session for invalid token")
	}
}

// ---- Route handler error path tests (pre-DB) ----

func TestHandleLoginInvalidProvider(t *testing.T) {
	p := &Plugin{}
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	req := httptest.NewRequest("GET", "/oauth/invalid/login", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Errorf("expected 400 for invalid provider, got %d", rec.Code)
	}
}

func TestHandleLoginMissingTenant(t *testing.T) {
	p := &Plugin{}
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	req := httptest.NewRequest("GET", "/oauth/google/login", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Errorf("expected 400 for missing tenant, got %d", rec.Code)
	}
}

func TestHandleLoginInvalidTenantQuery(t *testing.T) {
	p := &Plugin{}
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	req := httptest.NewRequest("GET", "/oauth/google/login?tenant_id=not-a-uuid", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Errorf("expected 400 for invalid tenant_id query, got %d", rec.Code)
	}
}

func TestHandleCallbackInvalidProvider(t *testing.T) {
	p := &Plugin{}
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	req := httptest.NewRequest("GET", "/oauth/invalid/callback?code=abc&state=def", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Errorf("expected 400 for invalid provider, got %d", rec.Code)
	}
}

func TestHandleCallbackMissingCode(t *testing.T) {
	p := &Plugin{}
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	req := httptest.NewRequest("GET", "/oauth/google/callback?state=def", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Errorf("expected 400 for missing code, got %d", rec.Code)
	}
}

func TestHandleCallbackMissingState(t *testing.T) {
	p := &Plugin{}
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	req := httptest.NewRequest("GET", "/oauth/google/callback?code=abc", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Errorf("expected 400 for missing state, got %d", rec.Code)
	}
}

func TestHandleListSessionsUnauthorized(t *testing.T) {
	p := &Plugin{}
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	req := httptest.NewRequest("GET", "/oauth/sessions", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Errorf("expected 401 for unauthorized, got %d", rec.Code)
	}
}

func TestHandleDeleteSessionUnauthorized(t *testing.T) {
	p := &Plugin{}
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	req := httptest.NewRequest("DELETE", "/oauth/sessions/11111111-1111-1111-1111-111111111111", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Errorf("expected 401 for unauthorized, got %d", rec.Code)
	}
}

func TestHandleDeleteSessionInvalidID(t *testing.T) {
	store := newFakeSessionStore()
	db := sql.OpenDB(&fakeSessionConnector{store: store})
	defer db.Close()

	p := &Plugin{db: db}
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	// Pre-populate a valid session so extractSession succeeds.
	tenantID := uuid.New()
	token := "valid-session-token"
	tokenHash := sha256Hex(token)
	store.mu.Lock()
	store.sessions[tokenHash] = &testSessionRow{
		id:        uuid.New(),
		tenantID:  tenantID,
		userEmail: sql.NullString{String: "test@example.com", Valid: true},
		expiresAt: sql.NullTime{Time: time.Now().Add(1 * time.Hour), Valid: true},
	}
	store.mu.Unlock()

	// Request with valid Authorization header but invalid session ID in path.
	req := httptest.NewRequest("DELETE", "/oauth/sessions/not-a-uuid", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Errorf("expected 400 for invalid session id, got %d", rec.Code)
	}
}