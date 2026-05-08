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
	ID        uuid.UUID
	TenantID  uuid.UUID
	Provider  string
	UserEmail driver.Value // nil or string
	TokenHash driver.Value // nil or string (sha256 hex of session token)
	CreatedAt time.Time
	ExpiresAt driver.Value // nil or time.Time
}

type fakeDBStore struct {
	mu       sync.RWMutex
	sessions map[uuid.UUID]*fakeSession
	now      func() time.Time
}

func newFakeDBStore() *fakeDBStore {
	return &fakeDBStore{
		sessions: make(map[uuid.UUID]*fakeSession),
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
