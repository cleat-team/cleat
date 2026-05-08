// Package oauthprovider tests the OAuth provider plugin with an in-memory fake
// database, avoiding any need for PostgreSQL.
package oauthprovider

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// In-memory fake database (replaces PostgreSQL entirely for testing)
// ---------------------------------------------------------------------------

type fakeSession struct {
	ID           uuid.UUID
	TenantID     uuid.UUID
	Provider     string
	State        driver.Value // nil or string (for callback flow)
	CodeVerifier driver.Value // nil or string
	UserEmail    driver.Value // nil or string
	TokenHash    driver.Value // nil or string (sha256 hex of session token)
	CreatedAt    time.Time
	ExpiresAt    driver.Value // nil or time.Time
}

type fakeDBStore struct {
	mu       sync.RWMutex
	sessions map[uuid.UUID]*fakeSession
	configs  map[string]*oauthConfigRow // key: "tenantID:provider"
	now      func() time.Time
}

func newFakeDBStore() *fakeDBStore {
	return &fakeDBStore{
		sessions: make(map[uuid.UUID]*fakeSession),
		configs:  make(map[string]*oauthConfigRow),
		now:      time.Now,
	}
}

// AddSession inserts a session. If expiresIn > 0, ExpiresAt = now + expiresIn.
// If expiresIn < 0, ExpiresAt = now + expiresIn (past time = expired).
// If expiresIn == 0, ExpiresAt is nil (never expires).
func (s *fakeDBStore) AddSession(id, tenantID uuid.UUID, token string, email string, expiresIn time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tokenHash := sha256Hex(token)
	var userEmail driver.Value
	if email != "" {
		userEmail = email
	}
	var expiresAt driver.Value
	if expiresIn != 0 {
		t := s.now().Add(expiresIn)
		expiresAt = t
	}

	s.sessions[id] = &fakeSession{
		ID:        id,
		TenantID:  tenantID,
		Provider:  "google",
		UserEmail: userEmail,
		TokenHash: tokenHash,
		CreatedAt: s.now(),
		ExpiresAt: expiresAt,
	}
}

// RemoveSession deletes a session from the store by ID.
func (s *fakeDBStore) RemoveSession(id uuid.UUID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, id)
}

// ---------------------------------------------------------------------------
// Fake SQL driver (replaces PostgreSQL entirely for testing)
// ---------------------------------------------------------------------------

type fakeConnector struct {
	store *fakeDBStore
}

func (c *fakeConnector) Connect(_ context.Context) (driver.Conn, error) {
	return &fakeConn{store: c.store}, nil
}

func (c *fakeConnector) Driver() driver.Driver {
	return &fakeDrv{}
}

type fakeDrv struct{}

func (*fakeDrv) Open(_ string) (driver.Conn, error) {
	return nil, fmt.Errorf("fakeDriver: use sql.OpenDB")
}

type fakeConn struct {
	store *fakeDBStore
}

func (*fakeConn) Prepare(_ string) (driver.Stmt, error) {
	return nil, fmt.Errorf("fakeConn: unexpected Prepare call")
}

func (*fakeConn) Close() error      { return nil }
func (*fakeConn) Begin() (driver.Tx, error) { return &fakeTx{}, nil }

type fakeTx struct{}

func (*fakeTx) Commit() error   { return nil }
func (*fakeTx) Rollback() error { return nil }

// --- driver.ExecerContext ---

func (c *fakeConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.store.mu.Lock()
	defer c.store.mu.Unlock()

	switch {
	case strings.Contains(query, "DELETE FROM oauth_sessions"):
		return c.execDeleteSession(args)
	case strings.Contains(query, "INSERT INTO oauth_sessions"):
		return c.execInsertSession(args)
	case strings.Contains(query, "UPDATE oauth_sessions"):
		return c.execUpdateSession(args)
	default:
		return nil, fmt.Errorf("fakeConn: unexpected Exec query: %s", query)
	}
}

func (c *fakeConn) execDeleteSession(args []driver.NamedValue) (driver.Result, error) {
	// DELETE FROM oauth_sessions WHERE id = $1 AND tenant_id = $2
	idStr, err := argString(args, 1)
	if err != nil {
		return nil, err
	}
	id, err := uuid.Parse(idStr)
	if err != nil {
		return nil, err
	}
	tenantStr, err := argString(args, 2)
	if err != nil {
		return nil, err
	}
	tenantID, err := uuid.Parse(tenantStr)
	if err != nil {
		return nil, err
	}

	s, ok := c.store.sessions[id]
	if !ok || s.TenantID != tenantID {
		return &fakeResult{rowsAffected: 0}, nil
	}

	delete(c.store.sessions, id)
	return &fakeResult{rowsAffected: 1}, nil
}

// --- driver.QueryerContext ---

func (c *fakeConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.store.mu.RLock()
	defer c.store.mu.RUnlock()

	switch {
	case strings.Contains(query, "FROM oauth_config"):
		return c.queryOAuthConfig(args)
	case strings.Contains(query, "WHERE state = $1"):
		return c.querySessionByState(args)
	case strings.Contains(query, "token_hash = $1"):
		return c.queryByTokenHash(args)
	case strings.Contains(query, "ORDER BY created_at DESC"):
		return c.queryListSessions(args)
	default:
		return nil, fmt.Errorf("fakeConn: unexpected Query query: %s", query)
	}
}

func (c *fakeConn) queryByTokenHash(args []driver.NamedValue) (driver.Rows, error) {
	// SELECT id, tenant_id, user_email, expires_at
	// FROM oauth_sessions
	// WHERE token_hash = $1 AND (expires_at IS NULL OR expires_at > now())
	tokenHash, err := argString(args, 1)
	if err != nil {
		return nil, err
	}

	now := c.store.now()
	for _, s := range c.store.sessions {
		th, ok := s.TokenHash.(string)
		if !ok || th != tokenHash {
			continue
		}
		// Check (expires_at IS NULL OR expires_at > now())
		if s.ExpiresAt != nil {
			expTime, ok := s.ExpiresAt.(time.Time)
			if ok && !expTime.After(now) {
				continue // expired
			}
		}
		return &fakeRows{
			columns: []string{"id", "tenant_id", "user_email", "expires_at"},
			data: [][]driver.Value{{
				s.ID.String(),
				s.TenantID.String(),
				s.UserEmail,
				s.ExpiresAt,
			}},
		}, nil
	}
	return &fakeRows{columns: []string{"id", "tenant_id", "user_email", "expires_at"}}, nil
}

func (c *fakeConn) queryListSessions(args []driver.NamedValue) (driver.Rows, error) {
	// SELECT id, provider, user_email, created_at, expires_at
	// FROM oauth_sessions
	// WHERE tenant_id = $1
	// ORDER BY created_at DESC
	tenantStr, err := argString(args, 1)
	if err != nil {
		return nil, err
	}
	tenantID, err := uuid.Parse(tenantStr)
	if err != nil {
		return nil, err
	}

	columns := []string{"id", "provider", "user_email", "created_at", "expires_at"}
	var data [][]driver.Value
	for _, s := range c.store.sessions {
		if s.TenantID != tenantID {
			continue
		}
		data = append(data, []driver.Value{
			s.ID.String(),
			s.Provider,
			s.UserEmail,
			s.CreatedAt,
			s.ExpiresAt,
		})
	}
	// Sort by created_at DESC (most recent first).
	for i := 0; i < len(data); i++ {
		for j := i + 1; j < len(data); j++ {
			ti := data[i][3].(time.Time)
			tj := data[j][3].(time.Time)
			if tj.After(ti) {
				data[i], data[j] = data[j], data[i]
			}
		}
	}

	return &fakeRows{columns: columns, data: data}, nil
}

// ---------------------------------------------------------------------------
// Argument extractors
// ---------------------------------------------------------------------------

func argString(args []driver.NamedValue, ordinal int) (string, error) {
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

func argAny(args []driver.NamedValue, ordinal int) (driver.Value, error) {
	for _, a := range args {
		if a.Ordinal == ordinal {
			return a.Value, nil
		}
	}
	return nil, fmt.Errorf("arg %d not found", ordinal)
}

// ---------------------------------------------------------------------------
// driver.Result / driver.Rows stubs
// ---------------------------------------------------------------------------

type fakeResult struct {
	rowsAffected int64
}

func (r *fakeResult) LastInsertId() (int64, error) { return 0, nil }
func (r *fakeResult) RowsAffected() (int64, error) { return r.rowsAffected, nil }

type fakeRows struct {
	columns []string
	data    [][]driver.Value
	pos     int
}

func (r *fakeRows) Columns() []string { return r.columns }
func (r *fakeRows) Close() error      { return nil }
func (r *fakeRows) Next(dest []driver.Value) error {
	if r.pos >= len(r.data) {
		return io.EOF
	}
	copy(dest, r.data[r.pos])
	r.pos++
	return nil
}

// ---------------------------------------------------------------------------
// Test setup helpers
// ---------------------------------------------------------------------------

var testTenantID = uuid.MustParse("00000000-0000-0000-0000-000000000001")
var testTenantID2 = uuid.MustParse("00000000-0000-0000-0000-000000000002")

// setupTestPlugin creates a Plugin wired to an in-memory fake database and
// registers its routes on a new ServeMux.  The returned http.Handler can be
// used directly with httptest.
func setupTestPlugin(t *testing.T, store *fakeDBStore) (*Plugin, http.Handler) {
	t.Helper()

	db := sql.OpenDB(&fakeConnector{store: store})
	t.Cleanup(func() { db.Close() })

	p := &Plugin{
		db:     db,
		mux:    http.NewServeMux(),
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	if err := p.RegisterRoutes(p.mux); err != nil {
		t.Fatalf("RegisterRoutes: %v", err)
	}

	return p, p.mux
}

// authedRequest creates a request with a Bearer token in the Authorization
// header.
func authedRequest(method, target string, body io.Reader, token string) *http.Request {
	req := httptest.NewRequest(method, target, body)
	req.Header.Set("Authorization", "Bearer "+token)
	return req
}

// ---------------------------------------------------------------------------
// Behavioral tests
// ---------------------------------------------------------------------------

// TestValidTokenListSessions verifies that a valid session token allows
// listing sessions (token creation flow validation).
func TestValidTokenListSessions(t *testing.T) {
	store := newFakeDBStore()
	_, handler := setupTestPlugin(t, store)

	sessionID := uuid.MustParse("00000000-0000-0000-0000-000000000101")
	store.AddSession(sessionID, testTenantID, "valid-session-token", "user@example.com", 0)

	req := authedRequest("GET", "/oauth/sessions", nil, "valid-session-token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var sessions []map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &sessions); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}
	if sessions[0]["provider"] != "google" {
		t.Errorf("expected provider 'google', got %q", sessions[0]["provider"])
	}
}

// TestExpiredToken verifies that a session token with a past expiry is
// rejected with 401.
func TestExpiredToken(t *testing.T) {
	store := newFakeDBStore()
	_, handler := setupTestPlugin(t, store)

	sessionID := uuid.MustParse("00000000-0000-0000-0000-000000000102")
	// expiresIn = -1h means the token expired an hour ago.
	store.AddSession(sessionID, testTenantID, "expired-token", "user@example.com", -1*time.Hour)

	req := authedRequest("GET", "/oauth/sessions", nil, "expired-token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for expired token, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestInvalidToken verifies that a token not matching any session returns 401.
func TestInvalidToken(t *testing.T) {
	store := newFakeDBStore()
	_, handler := setupTestPlugin(t, store)

	req := authedRequest("GET", "/oauth/sessions", nil, "nonexistent-token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for invalid token, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestMissingAuthHeader verifies that a request without an Authorization
// header returns 401.
func TestMissingAuthHeader(t *testing.T) {
	store := newFakeDBStore()
	_, handler := setupTestPlugin(t, store)

	req := httptest.NewRequest("GET", "/oauth/sessions", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for missing auth header, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestMalformedAuthHeader verifies that various forms of malformed
// Authorization headers are rejected with 401.
func TestMalformedAuthHeader(t *testing.T) {
	store := newFakeDBStore()
	_, handler := setupTestPlugin(t, store)

	tests := []struct {
		name  string
		value string
	}{
		{"Basic scheme", "Basic dXNlcjpwYXNz"},
		{"Empty Bearer token", "Bearer "},
		{"No Bearer prefix", "Token abc123"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/oauth/sessions", nil)
			req.Header.Set("Authorization", tt.value)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("expected 401, got %d: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestListSessionsMulti verifies that listing sessions returns all sessions
// for the authenticated tenant and excludes sessions from other tenants.
func TestListSessionsMulti(t *testing.T) {
	store := newFakeDBStore()
	_, handler := setupTestPlugin(t, store)

	authSessionID := uuid.MustParse("00000000-0000-0000-0000-000000000201")
	sessionBID := uuid.MustParse("00000000-0000-0000-0000-000000000202")
	sessionCID := uuid.MustParse("00000000-0000-0000-0000-000000000203")
	otherTenantID := uuid.MustParse("00000000-0000-0000-0000-000000000301")

	// Sessions for the authenticated tenant.
	store.AddSession(authSessionID, testTenantID, "auth-token", "alice@example.com", 0)
	store.AddSession(sessionBID, testTenantID, "session-b-token", "bob@example.com", 0)
	store.AddSession(sessionCID, testTenantID, "session-c-token", "charlie@example.com", 0)

	// Session for a different tenant (should not appear in results).
	store.AddSession(otherTenantID, testTenantID2, "other-tenant-token", "eve@example.com", 0)

	req := authedRequest("GET", "/oauth/sessions", nil, "auth-token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var sessions []map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &sessions); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if len(sessions) != 3 {
		t.Fatalf("expected 3 sessions for tenant 1, got %d", len(sessions))
	}
}

// TestDeleteSession verifies that deleting a session via the DELETE endpoint
// invalidates the corresponding token (token revocation).
func TestDeleteSession(t *testing.T) {
	store := newFakeDBStore()
	_, handler := setupTestPlugin(t, store)

	authSessionID := uuid.MustParse("00000000-0000-0000-0000-000000000401")
	targetSessionID := uuid.MustParse("00000000-0000-0000-0000-000000000402")

	store.AddSession(authSessionID, testTenantID, "auth-token", "admin@example.com", 0)
	store.AddSession(targetSessionID, testTenantID, "target-token", "target@example.com", 0)

	// 1. Delete the target session, authenticated as the admin session.
	req := authedRequest("DELETE", "/oauth/sessions/"+targetSessionID.String(), nil, "auth-token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// 2. The deleted session's token should no longer validate.
	req = authedRequest("GET", "/oauth/sessions", nil, "target-token")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("deleted session token: expected 401, got %d: %s", rec.Code, rec.Body.String())
	}

	// 3. The admin session still works and the list shows only 1 session left.
	req = authedRequest("GET", "/oauth/sessions", nil, "auth-token")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("auth session after delete: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var sessions []map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &sessions); err != nil {
		t.Fatalf("failed to decode list: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 remaining session, got %d", len(sessions))
	}
}

// TestDeleteNonexistentSession verifies that deleting a session that does not
// exist returns 404.
func TestDeleteNonexistentSession(t *testing.T) {
	store := newFakeDBStore()
	_, handler := setupTestPlugin(t, store)

	authSessionID := uuid.MustParse("00000000-0000-0000-0000-000000000501")
	store.AddSession(authSessionID, testTenantID, "auth-token", "admin@example.com", 0)

	fakeID := uuid.MustParse("00000000-0000-0000-0000-000000009999")
	req := authedRequest("DELETE", "/oauth/sessions/"+fakeID.String(), nil, "auth-token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for nonexistent session, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestDeleteSessionUnauthenticated verifies that DELETE /oauth/sessions/{id}
// without auth returns 401.
func TestDeleteSessionUnauthenticated(t *testing.T) {
	store := newFakeDBStore()
	_, handler := setupTestPlugin(t, store)

	req := httptest.NewRequest("DELETE", "/oauth/sessions/00000000-0000-0000-0000-000000000601", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for unauthenticated DELETE, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestTokenRefreshFlow simulates token refresh: create a new session after
// the old one is invalidated, verify the old token is rejected and the new
// token is accepted.
func TestTokenRefreshFlow(t *testing.T) {
	store := newFakeDBStore()
	_, handler := setupTestPlugin(t, store)

	oldSessionID := uuid.MustParse("00000000-0000-0000-0000-000000000701")
	newSessionID := uuid.MustParse("00000000-0000-0000-0000-000000000702")

	store.AddSession(oldSessionID, testTenantID, "old-token", "user@example.com", 0)
	store.AddSession(newSessionID, testTenantID, "new-token", "user@example.com", 0)

	// 1. Old token still works before refresh.
	req := authedRequest("GET", "/oauth/sessions", nil, "old-token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("old token before refresh: expected 200, got %d", rec.Code)
	}

	// 2. Simulate refresh: remove the old session.
	store.RemoveSession(oldSessionID)

	// 3. Old token no longer works.
	req = authedRequest("GET", "/oauth/sessions", nil, "old-token")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("old token after refresh: expected 401, got %d", rec.Code)
	}

	// 4. New token works.
	req = authedRequest("GET", "/oauth/sessions", nil, "new-token")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("new token after refresh: expected 200, got %d", rec.Code)
	}
}

// ===========================================================================
// Middleware skip paths and passthrough (no DB needed)
// ===========================================================================

func TestOA_Middleware_SkipsOAuthPaths(t *testing.T) {
	p := &Plugin{}
	nextCalled := false
	handler := p.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalled = true
	}))
	for _, path := range []string{"/oauth/login", "/oauth/callback", "/healthz"} {
		nextCalled = false
		req := httptest.NewRequest("GET", path, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if !nextCalled {
			t.Errorf("path %s: expected next handler", path)
		}
	}
}

func TestOA_Middleware_PassthroughNoAuth(t *testing.T) {
	p := &Plugin{}
	nextCalled := false
	handler := p.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalled = true
	}))
	tests := []struct{ name, auth string }{
		{"no header", ""},
		{"Basic auth", "Basic dGVzdDp0ZXN0"},
	}
	for _, tc := range tests {
		nextCalled = false
		req := httptest.NewRequest("GET", "/api/x", nil)
		if tc.auth != "" {
			req.Header.Set("Authorization", tc.auth)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if !nextCalled {
			t.Errorf("%s: expected passthrough", tc.name)
		}
	}
}

func TestOA_FormatProviderURL_WithSubstitution(t *testing.T) {
	got := formatProviderURL("https://%s.example.com/auth", "mycompany")
	if got != "https://mycompany.example.com/auth" {
		t.Errorf("expected substitution, got: %s", got)
	}
}

func TestOA_FormatProviderURL_NoPlaceholder(t *testing.T) {
	got := formatProviderURL("https://accounts.google.com/o/oauth2/auth", "ignored")
	if got != "https://accounts.google.com/o/oauth2/auth" {
		t.Errorf("expected no change, got: %s", got)
	}
}

func TestOA_GenerateSessionToken(t *testing.T) {
	t1, err := generateSessionToken()
	if err != nil {
		t.Fatalf("generateSessionToken: %v", err)
	}
	if t1 == "" {
		t.Error("token should not be empty")
	}
	t2, err := generateSessionToken()
	if err != nil {
		t.Fatalf("second token: %v", err)
	}
	if t1 == t2 {
		t.Error("two tokens should be different")
	}
}

func TestOA_TenantID_NotPresent(t *testing.T) {
	p := &Plugin{}
	req := httptest.NewRequest("GET", "/", nil)
	tid := p.tenantID(req)
	if tid != uuid.Nil {
		t.Errorf("expected nil UUID, got %s", tid)
	}
}

func TestOA_SessionFromContext_Roundtrip(t *testing.T) {
	info := &SessionInfo{
		TenantID:  uuid.MustParse("00000000-0000-0000-0000-000000000001"),
		SessionID: uuid.New(),
		UserEmail: "user@example.com",
	}
	ctx := context.WithValue(context.Background(), sessionContextKey{}, info)
	got, ok := SessionFromContext(ctx)
	if !ok {
		t.Fatal("expected session to be present")
	}
	if got.UserEmail != info.UserEmail || got.TenantID != info.TenantID {
		t.Errorf("session info mismatch: %+v", got)
	}
}

func TestOA_SessionFromContext_NotPresent(t *testing.T) {
	_, ok := SessionFromContext(context.Background())
	if ok {
		t.Error("expected session to not be present")
	}
}

func TestOA_Migrations(t *testing.T) {
	p := &Plugin{}
	migrations := p.Migrations()
	if len(migrations) == 0 {
		t.Error("expected migrations")
	}
	for _, m := range migrations {
		if m.Version == 0 {
			t.Error("version must be non-zero")
		}
	}
}

func TestOA_Sha256Hex_Deterministic(t *testing.T) {
	a := sha256Hex("hello")
	b := sha256Hex("hello")
	if a != b || len(a) != 64 {
		t.Errorf("sha256Hex inconsistent: a=%s len=%d", a, len(a))
	}
}

func TestOA_Info(t *testing.T) {
	p := &Plugin{}
	info := p.Info()
	if info.Name != "oauth-provider" {
		t.Errorf("want oauth-provider, got %s", info.Name)
	}
}

// ---------------------------------------------------------------------------
// oauth_config helpers
// ---------------------------------------------------------------------------

func (s *fakeDBStore) AddOAuthConfig(tenantID uuid.UUID, provider, clientID, clientSecret, redirectURL, domain string, enabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := tenantID.String() + ":" + provider
	s.configs[key] = &oauthConfigRow{
		TenantID:     tenantID,
		Provider:     provider,
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURL:  redirectURL,
		Domain:       domain,
		Enabled:      enabled,
	}
}

func (c *fakeConn) execInsertSession(args []driver.NamedValue) (driver.Result, error) {
	idStr, err := argString(args, 1)
	if err != nil {
		return nil, err
	}
	id, err := uuid.Parse(idStr)
	if err != nil {
		return nil, err
	}
	tenantStr, err := argString(args, 2)
	if err != nil {
		return nil, err
	}
	tenantID, err := uuid.Parse(tenantStr)
	if err != nil {
		return nil, err
	}
	provider, err := argString(args, 3)
	if err != nil {
		return nil, err
	}
	state, err := argString(args, 4)
	if err != nil {
		return nil, err
	}
	codeVerifier, err := argString(args, 5)
	if err != nil {
		return nil, err
	}
	expiresAt, err := argAny(args, 6)
	if err != nil {
		return nil, err
	}

	c.store.sessions[id] = &fakeSession{
		ID:           id,
		TenantID:     tenantID,
		Provider:     provider,
		State:        state,
		CodeVerifier: codeVerifier,
		CreatedAt:    c.store.now(),
		ExpiresAt:    expiresAt,
	}
	return &fakeResult{rowsAffected: 1}, nil
}

func (c *fakeConn) execUpdateSession(args []driver.NamedValue) (driver.Result, error) {
	idStr, err := argString(args, 7)
	if err != nil {
		return nil, err
	}
	id, err := uuid.Parse(idStr)
	if err != nil {
		return nil, err
	}

	s, ok := c.store.sessions[id]
	if !ok {
		return &fakeResult{rowsAffected: 0}, nil
	}

	if tokenHash, err := argString(args, 2); err == nil {
		s.TokenHash = tokenHash
	}
	if userEmail, err := argAny(args, 3); err == nil {
		s.UserEmail = userEmail
	}
	if expiresAt, err := argAny(args, 6); err == nil {
		s.ExpiresAt = expiresAt
	}
	s.State = nil
	s.CodeVerifier = nil

	return &fakeResult{rowsAffected: 1}, nil
}

func (c *fakeConn) queryOAuthConfig(args []driver.NamedValue) (driver.Rows, error) {
	tenantStr, err := argString(args, 1)
	if err != nil {
		return nil, err
	}
	provider, err := argString(args, 2)
	if err != nil {
		return nil, err
	}

	key := tenantStr + ":" + provider
	cfg, ok := c.store.configs[key]
	if !ok || !cfg.Enabled {
		return &fakeRows{
			columns: []string{"tenant_id", "provider", "client_id", "client_secret", "redirect_url", "domain", "enabled"},
		}, nil
	}

	return &fakeRows{
		columns: []string{"tenant_id", "provider", "client_id", "client_secret", "redirect_url", "domain", "enabled"},
		data: [][]driver.Value{{
			cfg.TenantID.String(),
			cfg.Provider,
			cfg.ClientID,
			cfg.ClientSecret,
			cfg.RedirectURL,
			cfg.Domain,
			cfg.Enabled,
		}},
	}, nil
}

func (c *fakeConn) querySessionByState(args []driver.NamedValue) (driver.Rows, error) {
	state, err := argString(args, 1)
	if err != nil {
		return nil, err
	}

	now := c.store.now()
	for _, s := range c.store.sessions {
		st, ok := s.State.(string)
		if !ok || st != state {
			continue
		}
		if s.ExpiresAt != nil {
			if expTime, ok := s.ExpiresAt.(time.Time); ok && !expTime.After(now) {
				continue
			}
		}
		return &fakeRows{
			columns: []string{"id", "tenant_id", "provider", "code_verifier"},
			data: [][]driver.Value{{
				s.ID.String(),
				s.TenantID.String(),
				s.Provider,
				s.CodeVerifier,
			}},
		}, nil
	}
	return &fakeRows{columns: []string{"id", "tenant_id", "provider", "code_verifier"}}, nil
}

func TestOA_Login_Google_Redirect(t *testing.T) {
	store := newFakeDBStore()
	_, handler := setupTestPlugin(t, store)
	store.AddOAuthConfig(testTenantID, "google", "g-client-id", "g-secret", "http://localhost/callback", "", true)

	req := httptest.NewRequest("GET", "/oauth/google/login?tenant_id="+testTenantID.String(), nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("expected 302 redirect, got %d: %s", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	if !strings.Contains(loc, "accounts.google.com") {
		t.Errorf("expected redirect to accounts.google.com, got: %s", loc)
	}
	if !strings.Contains(loc, "client_id=g-client-id") {
		t.Errorf("expected client_id in redirect, got: %s", loc)
	}
	if !strings.Contains(loc, "code_challenge=") {
		t.Errorf("expected code_challenge in redirect, got: %s", loc)
	}
}

func TestOA_Login_GitHub_Redirect(t *testing.T) {
	store := newFakeDBStore()
	_, handler := setupTestPlugin(t, store)
	store.AddOAuthConfig(testTenantID, "github", "gh-client-id", "gh-secret", "http://localhost/callback", "", true)

	req := httptest.NewRequest("GET", "/oauth/github/login?tenant_id="+testTenantID.String(), nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("expected 302 redirect, got %d: %s", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	if !strings.Contains(loc, "github.com/login/oauth/authorize") {
		t.Errorf("expected GitHub auth URL, got: %s", loc)
	}
	if !strings.Contains(loc, "scope=read%3Auser") {
		t.Errorf("expected scope=read:user, got: %s", loc)
	}
}

func TestOA_Login_Okta_Redirect(t *testing.T) {
	store := newFakeDBStore()
	_, handler := setupTestPlugin(t, store)
	store.AddOAuthConfig(testTenantID, "okta", "okta-client-id", "okta-secret", "http://localhost/callback", "mycompany.okta.com", true)

	req := httptest.NewRequest("GET", "/oauth/okta/login?tenant_id="+testTenantID.String(), nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("expected 302 redirect, got %d: %s", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	if !strings.Contains(loc, "mycompany.okta.com") {
		t.Errorf("expected Okta domain in redirect, got: %s", loc)
	}
}

func TestOA_Login_MissingTenant(t *testing.T) {
	store := newFakeDBStore()
	_, handler := setupTestPlugin(t, store)

	req := httptest.NewRequest("GET", "/oauth/google/login", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestOA_Login_InvalidTenantID(t *testing.T) {
	store := newFakeDBStore()
	_, handler := setupTestPlugin(t, store)

	req := httptest.NewRequest("GET", "/oauth/google/login?tenant_id=not-a-uuid", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestOA_Login_InvalidProvider(t *testing.T) {
	store := newFakeDBStore()
	_, handler := setupTestPlugin(t, store)

	req := httptest.NewRequest("GET", "/oauth/bad/login?tenant_id="+testTenantID.String(), nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestOA_Login_ConfigNotFound(t *testing.T) {
	store := newFakeDBStore()
	_, handler := setupTestPlugin(t, store)

	req := httptest.NewRequest("GET", "/oauth/google/login?tenant_id="+testTenantID.String(), nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestOA_Callback_InvalidProvider(t *testing.T) {
	store := newFakeDBStore()
	_, handler := setupTestPlugin(t, store)
	req := httptest.NewRequest("GET", "/oauth/bad/callback?code=x&state=y", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestOA_Callback_MissingCode(t *testing.T) {
	store := newFakeDBStore()
	_, handler := setupTestPlugin(t, store)
	req := httptest.NewRequest("GET", "/oauth/google/callback?state=abc", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestOA_Callback_MissingState(t *testing.T) {
	store := newFakeDBStore()
	_, handler := setupTestPlugin(t, store)
	req := httptest.NewRequest("GET", "/oauth/google/callback?code=xyz", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestOA_Callback_InvalidState(t *testing.T) {
	store := newFakeDBStore()
	_, handler := setupTestPlugin(t, store)
	req := httptest.NewRequest("GET", "/oauth/google/callback?code=x&state=nonexistent", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestOA_Callback_ExpiredState(t *testing.T) {
	store := newFakeDBStore()
	_, handler := setupTestPlugin(t, store)

	sessionID := uuid.MustParse("00000000-0000-0000-0000-000000000801")
	store.mu.Lock()
	store.sessions[sessionID] = &fakeSession{
		ID: sessionID, TenantID: testTenantID, Provider: "google",
		State: "expired-state", CodeVerifier: "verifier",
		CreatedAt: time.Now().Add(-10 * time.Minute),
		ExpiresAt: time.Now().Add(-5 * time.Minute),
	}
	store.mu.Unlock()

	req := httptest.NewRequest("GET", "/oauth/google/callback?code=x&state=expired-state", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestOA_Callback_ProviderMismatch(t *testing.T) {
	store := newFakeDBStore()
	p, handler := setupTestPlugin(t, store)
	p.httpClient = &http.Client{Timeout: time.Second}

	sessionID := uuid.MustParse("00000000-0000-0000-0000-000000000802")
	store.mu.Lock()
	store.sessions[sessionID] = &fakeSession{
		ID: sessionID, TenantID: testTenantID, Provider: "github",
		State: "mismatch-state", CodeVerifier: "verifier",
		CreatedAt: time.Now(), ExpiresAt: time.Now().Add(5 * time.Minute),
	}
	store.mu.Unlock()
	store.AddOAuthConfig(testTenantID, "github", "gh-id", "gh-secret", "http://localhost/cb", "", true)

	req := httptest.NewRequest("GET", "/oauth/google/callback?code=x&state=mismatch-state", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestOA_Callback_ConfigNotFound(t *testing.T) {
	store := newFakeDBStore()
	p, handler := setupTestPlugin(t, store)
	p.httpClient = &http.Client{Timeout: time.Second}

	sessionID := uuid.MustParse("00000000-0000-0000-0000-000000000803")
	store.mu.Lock()
	store.sessions[sessionID] = &fakeSession{
		ID: sessionID, TenantID: testTenantID, Provider: "google",
		State: "no-cfg-state", CodeVerifier: "verifier",
		CreatedAt: time.Now(), ExpiresAt: time.Now().Add(5 * time.Minute),
	}
	store.mu.Unlock()

	req := httptest.NewRequest("GET", "/oauth/google/callback?code=x&state=no-cfg-state", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestOA_Callback_Success(t *testing.T) {
	store := newFakeDBStore()
	p, handler := setupTestPlugin(t, store)

	mockMux := http.NewServeMux()
	mockMux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token":"mock-at","expires_in":3600}`))
	})
	mockMux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"email":"user@example.com"}`))
	})
	mockSrv := httptest.NewServer(mockMux)
	defer mockSrv.Close()

	origEndpoints := endpoints
	endpoints = map[string]providerEndpoints{
		"google": {
			tokenURL:    mockSrv.URL + "/token",
			userinfoURL: mockSrv.URL + "/userinfo",
		},
	}
	defer func() { endpoints = origEndpoints }()

	p.httpClient = mockSrv.Client()

	sessionID := uuid.MustParse("00000000-0000-0000-0000-000000000900")
	store.mu.Lock()
	store.sessions[sessionID] = &fakeSession{
		ID: sessionID, TenantID: testTenantID, Provider: "google",
		State: "callback-valid-state", CodeVerifier: "test-code-verifier",
		CreatedAt: time.Now(), ExpiresAt: time.Now().Add(5 * time.Minute),
	}
	store.mu.Unlock()
	store.AddOAuthConfig(testTenantID, "google", "test-client-id", "test-secret", "http://localhost/cb", "", true)

	req := httptest.NewRequest("GET", "/oauth/google/callback?code=mock-code&state=callback-valid-state", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var result map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &result)
	if result["user_email"] != "user@example.com" {
		t.Errorf("expected useremail, got %q", result["user_email"])
	}
}

func TestOA_Callback_TokenExchangeError(t *testing.T) {
	store := newFakeDBStore()
	p, handler := setupTestPlugin(t, store)

	mockMux := http.NewServeMux()
	mockMux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"error":"invalid_grant"}`))
	})
	mockSrv := httptest.NewServer(mockMux)
	defer mockSrv.Close()

	origEndpoints := endpoints
	endpoints = map[string]providerEndpoints{"google": {tokenURL: mockSrv.URL + "/token"}}
	defer func() { endpoints = origEndpoints }()

	p.httpClient = mockSrv.Client()

	sessionID := uuid.MustParse("00000000-0000-0000-0000-000000000901")
	store.mu.Lock()
	store.sessions[sessionID] = &fakeSession{
		ID: sessionID, TenantID: testTenantID, Provider: "google",
		State: "token-err-state", CodeVerifier: "verifier",
		CreatedAt: time.Now(), ExpiresAt: time.Now().Add(5 * time.Minute),
	}
	store.mu.Unlock()
	store.AddOAuthConfig(testTenantID, "google", "cid", "cs", "http://localhost/cb", "", true)

	req := httptest.NewRequest("GET", "/oauth/google/callback?code=x&state=token-err-state", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestOA_Callback_UserInfoError(t *testing.T) {
	store := newFakeDBStore()
	p, handler := setupTestPlugin(t, store)

	mockMux := http.NewServeMux()
	mockMux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token":"at","expires_in":3600}`))
	})
	mockMux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	mockSrv := httptest.NewServer(mockMux)
	defer mockSrv.Close()

	origEndpoints := endpoints
	endpoints = map[string]providerEndpoints{
		"google": {tokenURL: mockSrv.URL + "/token", userinfoURL: mockSrv.URL + "/userinfo"},
	}
	defer func() { endpoints = origEndpoints }()

	p.httpClient = mockSrv.Client()

	sessionID := uuid.MustParse("00000000-0000-0000-0000-000000000903")
	store.mu.Lock()
	store.sessions[sessionID] = &fakeSession{
		ID: sessionID, TenantID: testTenantID, Provider: "google",
		State: "uierr-state", CodeVerifier: "v",
		CreatedAt: time.Now(), ExpiresAt: time.Now().Add(5 * time.Minute),
	}
	store.mu.Unlock()
	store.AddOAuthConfig(testTenantID, "google", "cid", "cs", "http://localhost/cb", "", true)

	req := httptest.NewRequest("GET", "/oauth/google/callback?code=x&state=uierr-state", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestOA_Middleware_TokenHashMismatch(t *testing.T) {
	store := newFakeDBStore()
	db := sql.OpenDB(&fakeConnector{store: store})
	t.Cleanup(func() { db.Close() })

	p := &Plugin{db: db, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	nextCalled := false
	handler := p.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalled = true
	}))

	req := httptest.NewRequest("GET", "/api/protected", nil)
	req.Header.Set("Authorization", "Bearer nonexistent-token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if nextCalled {
		t.Error("next should not be called for invalid token")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestOA_Middleware_ValidTokenInjectsSession(t *testing.T) {
	store := newFakeDBStore()
	sessionID := uuid.MustParse("00000000-0000-0000-0000-000000000950")
	store.AddSession(sessionID, testTenantID, "valid-mw-token", "mw@example.com", 0)

	db := sql.OpenDB(&fakeConnector{store: store})
	t.Cleanup(func() { db.Close() })

	p := &Plugin{db: db, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	var gotSession *SessionInfo
	handler := p.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		info, ok := SessionFromContext(r.Context())
		if ok {
			gotSession = info
		}
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/api/protected", nil)
	req.Header.Set("Authorization", "Bearer valid-mw-token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if gotSession == nil {
		t.Fatal("expected session in context")
	}
	if gotSession.TenantID != testTenantID {
		t.Errorf("expected tenant %s, got %s", testTenantID, gotSession.TenantID)
	}
}

func TestOA_Middleware_ExpiredTokenRejected(t *testing.T) {
	store := newFakeDBStore()
	sessionID := uuid.MustParse("00000000-0000-0000-0000-000000000951")
	store.AddSession(sessionID, testTenantID, "expired-mw-token", "old@example.com", -1*time.Hour)

	db := sql.OpenDB(&fakeConnector{store: store})
	t.Cleanup(func() { db.Close() })

	p := &Plugin{db: db, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	nextCalled := false
	handler := p.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalled = true
	}))

	req := httptest.NewRequest("GET", "/api/protected", nil)
	req.Header.Set("Authorization", "Bearer expired-mw-token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if nextCalled {
		t.Error("next should not be called for expired token")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
}

func TestOA_ExtractSession_ValidToken(t *testing.T) {
	store := newFakeDBStore()
	p, handler := setupTestPlugin(t, store)
	_ = handler

	sessionID := uuid.MustParse("00000000-0000-0000-0000-000000000960")
	store.AddSession(sessionID, testTenantID, "extract-valid-token", "extract@example.com", 0)

	session := p.extractSession(authedRequest("GET", "/oauth/sessions", nil, "extract-valid-token"))
	if session == nil {
		t.Fatal("expected non-nil session")
	}
	if session.TenantID != testTenantID {
		t.Errorf("expected tenant %s, got %s", testTenantID, session.TenantID)
	}
}

func TestOA_ExtractSession_NoAuthHeader(t *testing.T) {
	store := newFakeDBStore()
	p, handler := setupTestPlugin(t, store)
	_ = handler

	req := httptest.NewRequest("GET", "/oauth/sessions", nil)
	if s := p.extractSession(req); s != nil {
		t.Error("expected nil session for no auth header")
	}
}

func TestOA_ExtractSession_EmptyBearer(t *testing.T) {
	store := newFakeDBStore()
	p, handler := setupTestPlugin(t, store)
	_ = handler

	req := httptest.NewRequest("GET", "/oauth/sessions", nil)
	req.Header.Set("Authorization", "Bearer ")
	if s := p.extractSession(req); s != nil {
		t.Error("expected nil for empty Bearer")
	}
}

func TestOA_ExtractSession_BasicAuth(t *testing.T) {
	store := newFakeDBStore()
	p, handler := setupTestPlugin(t, store)
	_ = handler

	req := httptest.NewRequest("GET", "/oauth/sessions", nil)
	req.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	if s := p.extractSession(req); s != nil {
		t.Error("expected nil for Basic auth")
	}
}

