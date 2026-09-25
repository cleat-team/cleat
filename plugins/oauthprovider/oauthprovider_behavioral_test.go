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
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/plugin"
	"github.com/cleat-team/cleat/plugins/plugintest"
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
	Nonce        driver.Value // nil or string (OIDC replay check, cleat#1582)
	UserEmail    driver.Value // nil or string
	TokenHash    driver.Value // nil or string (sha256 hex of session token)
	CreatedAt    time.Time
	ExpiresAt    driver.Value // nil or time.Time

	// SessionTokenAtRest/AccessTokenAtRest/RefreshTokenAtRest mirror the
	// session_token/access_token/refresh_token columns finishLogin writes.
	// As of cleat#2295/#2296 finishLogin always writes NULL to all three
	// (nothing reads them back, and an earlier version that sealed them via
	// plugin.Payloads broke every login on a deployment with no
	// --encryption-key-file set) -- these stay nil after every real login.
	SessionTokenAtRest driver.Value
	AccessTokenAtRest  driver.Value
	RefreshTokenAtRest driver.Value
}

type fakeDBStore struct {
	mu       sync.RWMutex
	sessions map[uuid.UUID]*fakeSession
	configs  map[string]*oauthConfigRow // key: "tenantID:provider"; ClientSecret unused, see AddOAuthConfig
	now      func() time.Time

	// allowlist mirrors oauth_allowed_identities (cleat#2340), keyed the same
	// way configs is. A nil/absent key is an EMPTY allowlist, which DENIES;
	// TestOA_Allowlist_EmptyDenies is the case that says so, because "no rows"
	// and "the lookup never ran" are otherwise indistinguishable from a denial.
	//
	// There is no companion flag in this fake. An oauth_config.allowlist_enabled
	// column used to gate the check and defaulted false; cleat#2371 removed it,
	// so every config this fake can hold is allowlist-enforced and the fake has
	// no "off" state to model.
	allowlist map[string][]allowedIdentityRow

	// secrets backs AddOAuthConfig's client secret and must be the SAME
	// instance wired into the Plugin under test as p.secrets -- setupTestPlugin
	// does that. Owned by the store rather than created separately in each
	// test, so the existing AddOAuthConfig(tenantID, provider, ..., clientSecret,
	// ...) call sites (all seven of them, predating cleat#1992) need no change:
	// this seeds both the DB-shaped row and the tenant secret in one call, the
	// same way production's two stores (oauth_config, tenant_secrets) are two
	// separate writes only at the SQL layer.
	secrets *plugintest.FakeSecrets
}

// allowedIdentityRow is one row of oauth_allowed_identities. Deliberately
// EXACTLY the shape the SELECT returns -- identity_type and identity_value, no
// id or created_at -- so the fake cannot satisfy the query with a column the
// production statement does not ask for.
type allowedIdentityRow struct {
	IdentityType string
	Identity     string
}

func newFakeDBStore() *fakeDBStore {
	return &fakeDBStore{
		sessions:  make(map[uuid.UUID]*fakeSession),
		configs:   make(map[string]*oauthConfigRow),
		allowlist: make(map[string][]allowedIdentityRow),
		now:       time.Now,
		secrets:   plugintest.NewFakeSecrets(),
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

func (*fakeConn) Close() error              { return nil }
func (*fakeConn) Begin() (driver.Tx, error) { return &fakeTx{}, nil }

type fakeTx struct{}

func (*fakeTx) Commit() error   { return nil }
func (*fakeTx) Rollback() error { return nil }

// --- driver.ExecerContext ---

func (c *fakeConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.store.mu.Lock()
	defer c.store.mu.Unlock()

	switch {
	case strings.Contains(query, "DELETE FROM oauth_sessions") && strings.Contains(query, "expires_at IS NOT NULL"):
		return c.execDeleteExpiredSessions()
	case strings.Contains(query, "DELETE FROM oauth_sessions") && strings.Contains(query, "expires_at IS NULL"):
		return c.execDeleteOldStateSessions()
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

func (c *fakeConn) execDeleteExpiredSessions() (driver.Result, error) {
	now := c.store.now()
	var affected int64
	for id, s := range c.store.sessions {
		if s.ExpiresAt != nil {
			if expTime, ok := s.ExpiresAt.(time.Time); ok && expTime.Before(now) {
				delete(c.store.sessions, id)
				affected++
			}
		}
	}
	return &fakeResult{rowsAffected: affected}, nil
}

func (c *fakeConn) execDeleteOldStateSessions() (driver.Result, error) {
	now := c.store.now()
	cutoff := now.Add(-24 * time.Hour)
	var affected int64
	for id, s := range c.store.sessions {
		if s.ExpiresAt == nil && s.CreatedAt.Before(cutoff) {
			delete(c.store.sessions, id)
			affected++
		}
	}
	return &fakeResult{rowsAffected: affected}, nil
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
	case strings.Contains(query, "FROM oauth_allowed_identities"):
		return c.queryAllowedIdentities(args)
	case strings.Contains(query, "ORDER BY created_at DESC"):
		return c.queryListSessions(args)
	default:
		return nil, fmt.Errorf("fakeConn: unexpected Query query: %s", query)
	}
}

// queryAllowedIdentities answers identityAllowed's SELECT.
//
// It returns the rows for (tenant_id, provider) WITHOUT filtering on any
// identity value, deliberately: the production statement does not filter
// either -- the comparison happens in Go (see identityAllowed's doc comment) --
// and a fake that pre-filtered would let a bug in that comparison pass the
// tests by never exercising it.
//
// A tenant with no rows returns an empty result, not an error. That is the
// case the migration calls out as the one an unconditional check would turn
// into "deny every login", so it has to be reachable here.
func (c *fakeConn) queryAllowedIdentities(args []driver.NamedValue) (driver.Rows, error) {
	// SELECT identity_type, identity_value
	// FROM oauth_allowed_identities
	// WHERE tenant_id = $1 AND provider = $2
	tenantStr, err := argString(args, 1)
	if err != nil {
		return nil, err
	}
	provider, err := argString(args, 2)
	if err != nil {
		return nil, err
	}

	cols := []string{"identity_type", "identity_value"}
	rows := [][]driver.Value{}
	for _, r := range c.store.allowlist[tenantStr+":"+provider] {
		rows = append(rows, []driver.Value{r.IdentityType, r.Identity})
	}
	return &fakeRows{columns: cols, data: rows}, nil
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

	// dialect is explicit, not incidental: handleLogin and handleCallback
	// begin with p.pgOnly (plugin.go), which refuses with 501 anything that
	// is not literally plugin.DialectPostgres -- and Dialect's zero value is
	// "", not "postgres" (plugin/migration.go). A fixture left as
	// &Plugin{...} without this field exercises the refusal path instead of
	// the handler, and every redirect/validation assertion below reads 501.
	// These tests are about the Postgres login path, so they say so.
	p := &Plugin{
		dialect: plugin.DialectPostgres,
		db:      &engine.SQLDBAdapter{DB: db},
		mux:     http.NewServeMux(),
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		secrets: store.secrets,
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

	var sessions []map[string]any
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

	req := authedRequest("GET", "/oauth/sessions", nil, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa1")
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

	var sessions []map[string]any
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
	var sessions []map[string]any
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
	for _, path := range []string{"/oauth/login", "/oauth/callback", "/healthz", "/livez", "/readyz", "/metrics"} {
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
	tid, ok := p.tenantID(req)
	if ok {
		t.Errorf("expected ok=false with no session in context, got ok=true tid=%s", tid)
	}
}

// TestOA_TenantID_DefaultTenant is the cleat#2183 regression case: a session
// for the seeded default tenant, whose ID IS uuid.Nil, must read as
// ok=true -- not be conflated with "no session" the way comparing the UUID
// to uuid.Nil does.
func TestOA_TenantID_DefaultTenant(t *testing.T) {
	p := &Plugin{}
	info := &SessionInfo{TenantID: uuid.Nil, SessionID: uuid.New()}
	ctx := context.WithValue(context.Background(), sessionContextKey{}, info)
	req := httptest.NewRequest("GET", "/", nil).WithContext(ctx)
	tid, ok := p.tenantID(req)
	if !ok {
		t.Errorf("expected ok=true for the default tenant (uuid.Nil) session, got ok=false")
	}
	if tid != uuid.Nil {
		t.Errorf("expected tid=uuid.Nil, got %s", tid)
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
		TenantID:    tenantID,
		Provider:    provider,
		ClientID:    clientID,
		RedirectURL: redirectURL,
		Domain:      domain,
		Enabled:     enabled,
	}
	// client_secret lives in tenant secrets, not the oauth_config row, from
	// cleat#1992 onward -- Seed bypasses ctx/tenant resolution the way this
	// whole fake bypasses SQL, so no ForTenant-marked context is needed here.
	s.secrets.Seed(tenantID.String(), OAuthClientSecretName(provider), clientSecret)
}

// AddAllowedIdentity inserts one oauth_allowed_identities row. cleat#2340.
//
// Written as the operator would write it -- the raw identity_type and
// identity_value strings, unnormalised -- because the whole point of the
// comparison living in
// Go is that a hand-written row may carry mixed case. A helper that normalised
// here would make TestOA_Allowlist_MixedCaseMatch pass vacuously.
func (s *fakeDBStore) AddAllowedIdentity(tenantID uuid.UUID, provider, identityType, identity string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := tenantID.String() + ":" + provider
	s.allowlist[key] = append(s.allowlist[key], allowedIdentityRow{
		IdentityType: identityType,
		Identity:     identity,
	})
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
	nonce, err := argString(args, 6)
	if err != nil {
		return nil, err
	}
	expiresAt, err := argAny(args, 7)
	if err != nil {
		return nil, err
	}

	c.store.sessions[id] = &fakeSession{
		ID:           id,
		TenantID:     tenantID,
		Provider:     provider,
		State:        state,
		CodeVerifier: codeVerifier,
		Nonce:        nonce,
		CreatedAt:    c.store.now(),
		ExpiresAt:    expiresAt,
	}
	return &fakeResult{rowsAffected: 1}, nil
}

// execUpdateSession backs finishLogin's UPDATE. It supports two argument
// shapes so a wholesale regression back to binding session_token/
// access_token/refresh_token as their OWN parameters is visible to a test
// here, rather than tripping an unrelated id-parse error: the current shape
// (4 named args -- token_hash, user_email, expires_at, id) and the
// pre-cleat#2295 shape (7 named args, with the three bound as parameters 1,
// 4 and 5).
//
// "4 args arrived" is NOT proof the three columns are NULL, and this
// comment claimed otherwise until cleat-review's should-fix on 705e1fcd: a
// statement can reuse an EXISTING placeholder -- `session_token = $1,
// access_token = $2, refresh_token = $2` alongside `token_hash = $1,
// user_email = $2` -- and still bind exactly 4 args while writing real
// values into all three. This fake cannot see that; it only counts. What
// closes the loophole is
// a_real_dialect_login_stores_no_tokens_test.go's
// TestARealLoginStoresNoTokensOnAnyDialect, which reads the columns back
// with a real SELECT against real Postgres/MySQL/SQL Server rather than
// inferring their state from an argument count. Falsified by reintroducing
// exactly that placeholder-reuse shape in finishLogin: this fake stayed
// green, the real-dialect test failed naming all three columns, on all
// three dialects.
func (c *fakeConn) execUpdateSession(args []driver.NamedValue) (driver.Result, error) {
	var idPos int
	switch len(args) {
	case 4:
		idPos = 4
	case 7:
		idPos = 7
	default:
		return nil, fmt.Errorf("execUpdateSession: unexpected arg count %d", len(args))
	}
	idStr, err := argString(args, idPos)
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

	if idPos == 7 {
		// The shape finishLogin used before cleat#2295/#2296: session_token,
		// access_token and refresh_token are bound as real parameters, so
		// whatever the caller passed lands at rest exactly like a real DB
		// row would show it -- this is what lets a falsification of the fix
		// actually be CAUGHT by the "at rest" assertions, not just error out.
		if sessionToken, err := argString(args, 1); err == nil {
			s.SessionTokenAtRest = sessionToken
		}
		if tokenHash, err := argString(args, 2); err == nil {
			s.TokenHash = tokenHash
		}
		if userEmail, err := argAny(args, 3); err == nil {
			s.UserEmail = userEmail
		}
		if accessToken, err := argString(args, 4); err == nil {
			s.AccessTokenAtRest = accessToken
		}
		if refreshToken, err := argString(args, 5); err == nil {
			s.RefreshTokenAtRest = refreshToken
		}
		if expiresAt, err := argAny(args, 6); err == nil {
			s.ExpiresAt = expiresAt
		}
	} else {
		if tokenHash, err := argString(args, 1); err == nil {
			s.TokenHash = tokenHash
		}
		if userEmail, err := argAny(args, 2); err == nil {
			s.UserEmail = userEmail
		}
		if expiresAt, err := argAny(args, 3); err == nil {
			s.ExpiresAt = expiresAt
		}
		s.SessionTokenAtRest = nil
		s.AccessTokenAtRest = nil
		s.RefreshTokenAtRest = nil
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
	// Seven columns, and the fake has to agree with production's getConfig on
	// the COUNT as well as the names: a Scan whose destination count disagrees
	// with the row fails outright ("expected 7 destination arguments in Scan,
	// not 8"), which turns every callback test into a 500 with nothing in the
	// message saying why. This was eight until cleat#2371 dropped
	// allowlist_enabled from the SELECT.
	//
	// The WHERE enabled = true in the production statement is modelled by the
	// empty result below rather than by filtering the row, which is what
	// production does too -- no row comes back at all.
	if !ok || !cfg.Enabled {
		return &fakeRows{
			columns: []string{"tenant_id", "provider", "client_id", "redirect_url", "domain", "issuer", "enabled"},
		}, nil
	}

	return &fakeRows{
		columns: []string{"tenant_id", "provider", "client_id", "redirect_url", "domain", "issuer", "enabled"},
		data: [][]driver.Value{{
			cfg.TenantID.String(),
			cfg.Provider,
			cfg.ClientID,
			cfg.RedirectURL,
			cfg.Domain,
			cfg.Issuer,
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
			columns: []string{"id", "tenant_id", "provider", "code_verifier", "nonce"},
			data: [][]driver.Value{{
				s.ID.String(),
				s.TenantID.String(),
				s.Provider,
				s.CodeVerifier,
				s.Nonce,
			}},
		}, nil
	}
	return &fakeRows{columns: []string{"id", "tenant_id", "provider", "code_verifier", "nonce"}}, nil
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
	// Parse the scope out and compare the WHOLE value. The assertion this
	// replaced was strings.Contains(loc, "scope=read%3Auser"), which passed
	// both before and after cleat#2340 added user:email -- "read%3Auser" is a
	// prefix of "read%3Auser+user%3Aemail", and the encoded form of the new
	// space separator is not the "&" the "expected scope=read:user" message
	// implies. It was a check that could not disagree with the change it was
	// supposed to catch, which is why the fix is the assertion rather than a
	// second one beside it.
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("redirect Location is not a URL: %v", err)
	}
	if got := u.Query().Get("scope"); got != "read:user user:email" {
		t.Errorf("scope = %q, want %q -- user:email is REQUIRED: GitHub's /user\n"+
			"returns an address only when the account has made one public, and the\n"+
			"allowlist (cleat#2340) may only compare a verified address", got, "read:user user:email")
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

	// cleat#2368: this answered 500, which an unauthenticated caller could read
	// against the 302 a configured pair gets -- "this (tenant, provider) pair
	// IS configured", for any tenant id it could guess. It is the same 302 now,
	// at a harmless same-origin path.
	if rec.Code != http.StatusFound {
		t.Fatalf("expected the uniform 302, got %d: %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/" {
		t.Errorf("want the harmless same-origin root, got Location %q", loc)
	}
}

// TestOA_Login_ConfiguredAndUnconfiguredAreIndistinguishable is cleat#2368's
// regression guard, and it deliberately asserts only one thing: the same
// caller on the same route, one tenant with a working (tenant, provider)
// config and one with none at all, must not be separable by HTTP status.
//
// It is falsifiable by construction. Restore the 500 on the not-configured
// branch and this reddens on the status comparison while every functional
// login test in this package stays green -- which is the point. The defect was
// never that a login failed; it was that a failure to log in was readable as
// "this tenant exists and is configured".
func TestOA_Login_ConfiguredAndUnconfiguredAreIndistinguishable(t *testing.T) {
	configured := newFakeDBStore()
	_, configuredHandler := setupTestPlugin(t, configured)
	configured.AddOAuthConfig(testTenantID, "google", "g-client-id", "g-secret", "http://localhost/callback", "", true)

	unconfigured := newFakeDBStore()
	_, unconfiguredHandler := setupTestPlugin(t, unconfigured)

	probe := func(h http.Handler) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/oauth/google/login?tenant_id="+testTenantID.String(), nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	gotConfigured := probe(configuredHandler)
	gotUnconfigured := probe(unconfiguredHandler)

	// The negative control for the comparison below, and it is load-bearing:
	// if the configured arm were not actually reaching the provider, both arms
	// being "the same status" would prove nothing about the oracle.
	if loc := gotConfigured.Header().Get("Location"); !strings.Contains(loc, "accounts.google.com") {
		t.Fatalf("the configured arm did not reach the provider (Location %q), so the status "+
			"comparison below would be vacuous", loc)
	}

	if gotConfigured.Code != gotUnconfigured.Code {
		t.Errorf("status distinguishes configured from unconfigured: configured=%d, unconfigured=%d -- "+
			"an unauthenticated caller can enumerate (tenant, provider) pairs",
			gotConfigured.Code, gotUnconfigured.Code)
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
		w.Write([]byte(`{"email":"user@example.com","email_verified":true}`))
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
	// A successful callback now needs an allowlist row as well as a config, on
	// every deployment (cleat#2371 removed the opt-in that used to gate the
	// check). Stated here rather than hidden in a helper, because it is the
	// precondition this test shares with every other 200 from this endpoint --
	// without it the IdP assertions below all still run and the login is refused
	// 403 before any of them can be observed.
	store.AddAllowedIdentity(testTenantID, "google", identityTypeEmail, "user@example.com")

	req := httptest.NewRequest("GET", "/oauth/google/callback?code=mock-code&state=callback-valid-state", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var result map[string]any
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

// Middleware tokens below are 64 lowercase hex characters, which is what
// generateSessionToken issues. That shape is load-bearing since
// IMPROVEMENT-PLAN 3.246: the middleware only claims tokens shaped like its
// own, so that a bearer token belonging to another scheme -- a cleat API key,
// `Bearer cleat_sk_...` -- falls through instead of being refused as an invalid
// session (#912). A placeholder like "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb2" now falls through and the
// test would assert nothing about the lookup.
func TestOA_Middleware_TokenHashMismatch(t *testing.T) {
	store := newFakeDBStore()
	db := sql.OpenDB(&fakeConnector{store: store})
	t.Cleanup(func() { db.Close() })

	p := &Plugin{db: &engine.SQLDBAdapter{DB: db}, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	nextCalled := false
	handler := p.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalled = true
	}))

	req := httptest.NewRequest("GET", "/api/protected", nil)
	req.Header.Set("Authorization", "Bearer aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa1")
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
	store.AddSession(sessionID, testTenantID, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb2", "mw@example.com", 0)

	db := sql.OpenDB(&fakeConnector{store: store})
	t.Cleanup(func() { db.Close() })

	p := &Plugin{db: &engine.SQLDBAdapter{DB: db}, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	var gotSession *SessionInfo
	handler := p.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		info, ok := SessionFromContext(r.Context())
		if ok {
			gotSession = info
		}
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/api/protected", nil)
	req.Header.Set("Authorization", "Bearer bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb2")
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

// TestOA_Middleware_ValidTokenSetsNeutralSubject covers cleat#1881: a valid
// session now populates auth.SubjectFromContext alongside the OAuth-specific
// SessionInfo, so a plugin auditing "who" (audit-log) does not have to know
// OAuth answered the question.
func TestOA_Middleware_ValidTokenSetsNeutralSubject(t *testing.T) {
	store := newFakeDBStore()
	sessionID := uuid.MustParse("00000000-0000-0000-0000-0000000009a0")
	store.AddSession(sessionID, testTenantID, "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddda0", "subject@example.com", 0)

	db := sql.OpenDB(&fakeConnector{store: store})
	t.Cleanup(func() { db.Close() })

	p := &Plugin{db: &engine.SQLDBAdapter{DB: db}, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	var gotSubject string
	var gotOK bool
	handler := p.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSubject, gotOK = auth.SubjectFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/api/protected", nil)
	req.Header.Set("Authorization", "Bearer dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddda0")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if !gotOK {
		t.Fatal("expected a subject in context for a valid session")
	}
	if gotSubject != "subject@example.com" {
		t.Errorf("subject = %q, want %q", gotSubject, "subject@example.com")
	}
}

// TestOA_Middleware_PassthroughDoesNotSetASubject is the mirror: a request
// this middleware does not recognise as one of its own sessions (no bearer
// token, wrong shape, wrong path) must leave auth.SubjectFromContext exactly
// as it found it -- absent -- so that oauth-provider never claims an identity
// for a request it did not authenticate.
func TestOA_Middleware_PassthroughDoesNotSetASubject(t *testing.T) {
	p := &Plugin{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	var gotOK bool
	handler := p.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, gotOK = auth.SubjectFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/api/protected", nil) // no Authorization header at all
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if gotOK {
		t.Error("a request with no session token got a subject in context; " +
			"oauth-provider must not claim an identity for a request it did not authenticate")
	}
}

func TestOA_Middleware_ExpiredTokenRejected(t *testing.T) {
	store := newFakeDBStore()
	sessionID := uuid.MustParse("00000000-0000-0000-0000-000000000951")
	store.AddSession(sessionID, testTenantID, "ccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc3", "old@example.com", -1*time.Hour)

	db := sql.OpenDB(&fakeConnector{store: store})
	t.Cleanup(func() { db.Close() })

	p := &Plugin{db: &engine.SQLDBAdapter{DB: db}, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	nextCalled := false
	handler := p.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalled = true
	}))

	req := httptest.NewRequest("GET", "/api/protected", nil)
	req.Header.Set("Authorization", "Bearer ccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc3")
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

// ---------------------------------------------------------------------------
// Identity allowlist (cleat#2340 item 2)
// ---------------------------------------------------------------------------

// allowlistCase is the fixture every callback test below shares: a fake store,
// the plugin, the HTTP handler, the callback session row, and a mock IdP the
// callback will call. Built once rather than copied seven times, because the
// tests differ ONLY in what the IdP asserts and what the allowlist holds, and a
// copy-paste divergence in the setup is indistinguishable from a difference in
// the thing under test.
type allowlistCase struct {
	store   *fakeDBStore
	plugin  *Plugin
	handler http.Handler
	session uuid.UUID
	state   string
	srv     *httptest.Server
}

// newAllowlistCase wires a callback test up to a mock IdP.
//
// userinfo is the body served at /userinfo; mount may register further handlers
// on the same mux (the GitHub tests use it for /user/emails). The provider's
// endpoints are swapped to point at the mock for the duration of the test --
// restored on cleanup, the same defer the older callback tests use.
func newAllowlistCase(t *testing.T, provider, state, userinfo string, mount func(*http.ServeMux)) *allowlistCase {
	t.Helper()

	store := newFakeDBStore()
	p, handler := setupTestPlugin(t, store)

	mockMux := http.NewServeMux()
	mockMux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token":"mock-at","expires_in":3600}`))
	})
	mockMux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(userinfo))
	})
	if mount != nil {
		mount(mockMux)
	}
	srv := httptest.NewServer(mockMux)
	t.Cleanup(srv.Close)

	origEndpoints := endpoints
	endpoints = map[string]providerEndpoints{
		provider: {
			tokenURL:      srv.URL + "/token",
			userinfoURL:   srv.URL + "/userinfo",
			userEmailsURL: srv.URL + "/user/emails",
		},
	}
	t.Cleanup(func() { endpoints = origEndpoints })

	p.httpClient = srv.Client()

	sessionID := uuid.New()
	store.mu.Lock()
	store.sessions[sessionID] = &fakeSession{
		ID: sessionID, TenantID: testTenantID, Provider: provider,
		State: state, CodeVerifier: "v",
		CreatedAt: time.Now(), ExpiresAt: time.Now().Add(5 * time.Minute),
	}
	store.mu.Unlock()
	store.AddOAuthConfig(testTenantID, provider, "cid", "cs", "http://localhost/cb", "", true)

	return &allowlistCase{store: store, plugin: p, handler: handler, session: sessionID, state: state, srv: srv}
}

// callback drives the callback for this case's session.
func (c *allowlistCase) callback(t *testing.T, provider string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", "/oauth/"+provider+"/callback?code=x&state="+c.state, nil)
	rec := httptest.NewRecorder()
	c.handler.ServeHTTP(rec, req)
	return rec
}

// sessionRow returns the callback's session row, read under the store lock.
func (c *allowlistCase) sessionRow() *fakeSession {
	c.store.mu.RLock()
	defer c.store.mu.RUnlock()
	return c.store.sessions[c.session]
}

// TestOA_Allowlist_VerifiedEmailAdmits is the positive control for the pair
// below: a verified address that is on the list is admitted.
func TestOA_Allowlist_VerifiedEmailAdmits(t *testing.T) {
	c := newAllowlistCase(t, "google", "al-verified",
		`{"email":"ada@example.com","email_verified":true}`, nil)
	c.store.AddAllowedIdentity(testTenantID, "google", identityTypeEmail, "ada@example.com")

	if rec := c.callback(t, "google"); rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestOA_Allowlist_UnverifiedEmailOnListDenies is THE core invariant, and it is
// written as a near-copy of the test above on purpose.
//
// Same tenant, same provider, same allowlist row, same address. The only
// difference is email_verified: true above, absent here. If this pair ever
// agrees, the verification claim is not being consulted, and the whole feature
// is a decoration -- an address anyone can type into an account being admitted
// because an operator listed it.
func TestOA_Allowlist_UnverifiedEmailOnListDenies(t *testing.T) {
	c := newAllowlistCase(t, "google", "al-unverified",
		`{"email":"ada@example.com"}`, nil)
	c.store.AddAllowedIdentity(testTenantID, "google", identityTypeEmail, "ada@example.com")

	rec := c.callback(t, "google")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for an UNVERIFIED address even when it is on the list, got %d: %s",
			rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("403 body is not JSON: %v", err)
	}
	if body["code"] != "identity_not_allowlisted" {
		t.Errorf("code = %v, want identity_not_allowlisted", body["code"])
	}
	// The refusal must not name a table, a row, or a count. It is served to
	// whoever holds the callback URL, which is not an authenticated operator.
	for _, leak := range []string{"oauth_allowed_identities", "oauth_config", "SELECT"} {
		if strings.Contains(rec.Body.String(), leak) {
			t.Errorf("403 body leaks internal detail %q: %s", leak, rec.Body.String())
		}
	}
}

// TestOA_Allowlist_MixedCaseMatchAdmits covers the reason the comparison is in
// Go rather than in SQL. Rows are hand-written INSERTs and an operator will
// write the address the way they see it; the provider asserts its own casing.
func TestOA_Allowlist_MixedCaseMatchAdmits(t *testing.T) {
	c := newAllowlistCase(t, "google", "al-case",
		`{"email":"ada@example.com","email_verified":true}`, nil)
	c.store.AddAllowedIdentity(testTenantID, "google", identityTypeEmail, "Ada@Example.COM")

	if rec := c.callback(t, "google"); rec.Code != http.StatusOK {
		t.Fatalf("a row differing only in case must match, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestOA_Allowlist_SubjectAdmitsWithoutVerifiedEmail is the case the subject
// rows exist for: an issuer that publishes no email_verified at all.
//
// Provider "google" rather than "oidc" deliberately. The generic oidc provider
// resolves its endpoints through DISCOVERY and validates an ID token against
// the discovered JWKS, so exercising it here would mean standing up a discovery
// document, a JWKS and a signed token -- a fixture that measures the OIDC
// validator, which is not the thing under test. The subject branch in
// handleCallback is provider-independent (it reads userinfo's `sub`), and
// google reaches it on the same code path.
func TestOA_Allowlist_SubjectAdmitsWithoutVerifiedEmail(t *testing.T) {
	c := newAllowlistCase(t, "google", "al-subject",
		`{"email":"ada@example.com","sub":"00u1a2b3c"}`, nil)
	c.store.AddAllowedIdentity(testTenantID, "google", identityTypeSubject, "00u1a2b3c")

	if rec := c.callback(t, "google"); rec.Code != http.StatusOK {
		t.Fatalf("a subject row must admit on its own, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestOA_Allowlist_SubjectRowDoesNotAdmitAnotherSubject is its negative. Without
// it the test above would pass on a plugin that admits every subject.
func TestOA_Allowlist_SubjectRowDoesNotAdmitAnotherSubject(t *testing.T) {
	c := newAllowlistCase(t, "google", "al-subject-other",
		`{"email":"ada@example.com","sub":"00u1a2b3c"}`, nil)
	c.store.AddAllowedIdentity(testTenantID, "google", identityTypeSubject, "99x9z8y7w")

	if rec := c.callback(t, "google"); rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for an unlisted subject, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestOA_Allowlist_SubjectRowIsNotCaseFolded pins normalizeSubject's asymmetry
// with normalizeEmail. OIDC Core §2 defines `sub` as case-sensitive, so a row
// differing only in case is a DIFFERENT account and must not match -- folding
// here would admit one person under another's row.
func TestOA_Allowlist_SubjectRowIsNotCaseFolded(t *testing.T) {
	c := newAllowlistCase(t, "google", "al-subject-case",
		`{"email":"ada@example.com","sub":"00U1A2B3C"}`, nil)
	c.store.AddAllowedIdentity(testTenantID, "google", identityTypeSubject, "00u1a2b3c")

	if rec := c.callback(t, "google"); rec.Code != http.StatusForbidden {
		t.Fatalf("a case-differing subject must NOT match, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestOA_Allowlist_EmptyDenies is the migration's stated contract: a
// (tenant, provider) pair with no rows refuses, rather than falling through to
// "no list configured, admit everyone". An empty table is the state a
// deployment that has just configured OAuth is in, so this is the common case,
// not an edge one.
//
// This case used to be named TestOA_Allowlist_EnabledAndEmptyDenies and its
// setup called a helper setting oauth_config.allowlist_enabled = true. The
// assertion was the same 403, but the same plugin against the same empty table
// admitted everyone whenever that column was false -- which was its default. So
// this test pinned the behaviour of one branch while a second, larger branch
// did the opposite, and only the pair together said what the deployment did.
// cleat#2371 removed the column and the sibling test that covered the other
// branch (TestOA_Allowlist_DisabledIgnoresRows) with it.
func TestOA_Allowlist_EmptyDenies(t *testing.T) {
	c := newAllowlistCase(t, "google", "al-empty",
		`{"email":"ada@example.com","email_verified":true}`, nil)

	if rec := c.callback(t, "google"); rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 when the allowlist has no rows for this pair, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestOA_Allowlist_RefusalLeavesSessionUnclaimed is why the gate sits before the
// UPDATE in finishLogin. A refused login must leave the session row exactly as
// the login start wrote it: no token_hash, no cleared state. Otherwise a refused
// person leaves a claimed-looking row behind, and re-driving the callback -- the
// ordinary thing a user does after a refusal -- finds it already consumed.
func TestOA_Allowlist_RefusalLeavesSessionUnclaimed(t *testing.T) {
	c := newAllowlistCase(t, "google", "al-untouched",
		`{"email":"grace@example.com","email_verified":true}`, nil)
	c.store.AddAllowedIdentity(testTenantID, "google", identityTypeEmail, "ada@example.com")

	rec := c.callback(t, "google")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rec.Code, rec.Body.String())
	}

	row := c.sessionRow()
	if row == nil {
		t.Fatal("the session row was deleted by a refusal")
	}
	if row.TokenHash != nil && row.TokenHash != "" {
		t.Errorf("a refused login wrote a token_hash: %v", row.TokenHash)
	}
	if row.State != c.state {
		t.Errorf("a refused login consumed the state: %q -> %v", c.state, row.State)
	}
	// And the flow is still usable: the same callback, once the person is on
	// the list, must succeed. This is the assertion that would fail if the
	// refusal had taken a shortcut that half-consumed the row.
	c.store.AddAllowedIdentity(testTenantID, "google", identityTypeEmail, "grace@example.com")
	if rec := c.callback(t, "google"); rec.Code != http.StatusOK {
		t.Fatalf("re-driving the callback after a refusal gave %d: %s", rec.Code, rec.Body.String())
	}
}

// TestOA_Allowlist_UnrecognisedIdentityTypeMatchesNothing pins the default
// branch. An identity_type cleat does not know -- a typo, or a kind added later
// -- must not fall through to an unconditional admit.
func TestOA_Allowlist_UnrecognisedIdentityTypeMatchesNothing(t *testing.T) {
	c := newAllowlistCase(t, "google", "al-badtype",
		`{"email":"ada@example.com","email_verified":true}`, nil)
	c.store.AddAllowedIdentity(testTenantID, "google", "username", "ada@example.com")

	if rec := c.callback(t, "google"); rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for an unrecognised identity_type, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// GitHub: the verified-address lookup (cleat#2340 item 1)
// ---------------------------------------------------------------------------

// TestOA_Callback_GitHubUsesVerifiedEmailFromEmailsEndpoint is item 1 of
// cleat#2340. GitHub's /user returns an address only when the account has made
// one public and carries NO verification claim even then, so the address the
// allowlist compares must come from /user/emails, filtered to primary+verified.
func TestOA_Callback_GitHubUsesVerifiedEmailFromEmailsEndpoint(t *testing.T) {
	c := newAllowlistCase(t, "github", "gh-verified",
		`{"email":null,"id":4242}`,
		func(m *http.ServeMux) {
			m.HandleFunc("/user/emails", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`[
					{"email":"old@example.com","primary":false,"verified":true},
					{"email":"unproved@example.com","primary":true,"verified":false},
					{"email":"ada@example.com","primary":true,"verified":true}
				]`))
			})
		})
	c.store.AddAllowedIdentity(testTenantID, "github", identityTypeEmail, "ada@example.com")

	rec := c.callback(t, "github")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	if body["user_email"] != "ada@example.com" {
		t.Errorf("session labelled %v, want the verified primary address ada@example.com", body["user_email"])
	}
}

// TestOA_Callback_GitHubUnverifiedEmailNotAllowlistEligible is the same account
// shape with the allowlist listing the address GitHub will not vouch for.
// Primary but unverified -- the exact case item 1 is about -- must be refused,
// because a person can add an address they do not control and it lands here
// immediately.
func TestOA_Callback_GitHubUnverifiedEmailNotAllowlistEligible(t *testing.T) {
	c := newAllowlistCase(t, "github", "gh-unverified",
		// The public address MUST be present, and MUST equal the allowlist row
		// below. With it null the account resolves no address at all and the
		// login is refused 502 before the allowlist is consulted -- green, and
		// measuring the "no address" guard instead of the comparison this test
		// is named for. That comparison is only reached when an address exists
		// and fails to be verified.
		`{"email":"unproved@example.com","id":4242}`,
		func(m *http.ServeMux) {
			m.HandleFunc("/user/emails", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`[{"email":"unproved@example.com","primary":true,"verified":false}]`))
			})
		})
	c.store.AddAllowedIdentity(testTenantID, "github", identityTypeEmail, "unproved@example.com")

	if rec := c.callback(t, "github"); rec.Code != http.StatusForbidden {
		t.Fatalf("a PRIMARY BUT UNVERIFIED address must not satisfy the allowlist, got %d: %s",
			rec.Code, rec.Body.String())
	}
}

// TestOA_Callback_GitHubSubjectAdmitsWhenNoEmailIsVerifiable is the documented
// escape hatch: an account with no verified address can still be admitted by
// its numeric id, which is GitHub's stable identifier and is not reassignable.
func TestOA_Callback_GitHubSubjectAdmitsWhenNoEmailIsVerifiable(t *testing.T) {
	c := newAllowlistCase(t, "github", "gh-subject",
		// "No email is VERIFIABLE" is the premise, not "no email exists": the
		// session still needs an address to be labelled with, and the pre-existing
		// no-address guard (see TestOA_Callback_GitHubNoAddressAtAllIsRefused)
		// would otherwise fire first and refuse the login for an unrelated reason.
		`{"email":"unproved@example.com","id":4242}`,
		func(m *http.ServeMux) {
			m.HandleFunc("/user/emails", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`[{"email":"unproved@example.com","primary":true,"verified":false}]`))
			})
		})
	c.store.AddAllowedIdentity(testTenantID, "github", identityTypeSubject, "4242")

	if rec := c.callback(t, "github"); rec.Code != http.StatusOK {
		t.Fatalf("expected 200 on a subject match, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestOA_Callback_GitHubEmailsEndpointFailingStillLogsIn covers the failure
// direction identity.go argues for: the lookup upgrades an address, so a lookup
// that cannot run must not be worse for the user than not having tried.
func TestOA_Callback_GitHubEmailsEndpointFailingStillLogsIn(t *testing.T) {
	c := newAllowlistCase(t, "github", "gh-lookup-fails",
		`{"email":"public@example.com","id":4242}`,
		func(m *http.ServeMux) {
			m.HandleFunc("/user/emails", func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
			})
		})
	// A subject row, because the premise of this test is that /user/emails could
	// not be reached -- so public@example.com is never upgraded to verified, and
	// only a `subject` row can admit it. Unconditional since cleat#2371: with the
	// opt-in gone there is no deployment on which this address would be admitted
	// as an email, so the subject row is not a convenience here, it is the only
	// route to the 200 this test asserts.
	c.store.AddAllowedIdentity(testTenantID, "github", identityTypeSubject, "4242")
	if rec := c.callback(t, "github"); rec.Code != http.StatusOK {
		t.Fatalf("expected 200 when /user/emails fails, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestOA_Callback_GitHubNoAddressAtAllIsRefused pins the guard that was
// deliberately NOT widened. An account with no public email and no verified
// address resolves no address at all, and is refused with the pre-existing 502
// -- the subject is an ADDITIONAL allowlist key, never a substitute for having
// an address. Widening this would admit logins that fail today, with no
// operator opt-in.
func TestOA_Callback_GitHubNoAddressAtAllIsRefused(t *testing.T) {
	c := newAllowlistCase(t, "github", "gh-no-address",
		`{"email":null,"id":4242}`,
		func(m *http.ServeMux) {
			m.HandleFunc("/user/emails", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`[]`))
			})
		})
	// A subject row exists AND the allowlist is enabled -- the strongest form
	// of "this person is expected" -- and the login is still refused, because
	// there is no address to label the session with.
	c.store.AddAllowedIdentity(testTenantID, "github", identityTypeSubject, "4242")

	if rec := c.callback(t, "github"); rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 for a GitHub account resolving no address, got %d: %s",
			rec.Code, rec.Body.String())
	}
}

// TestSelectGitHubVerifiedEmail is the pure half of the lookup: the selection
// rule, with no HTTP anywhere. The table is the whole point -- each row is a
// shape GitHub actually returns, and the "want" column is what the allowlist
// is then allowed to compare against.
func TestSelectGitHubVerifiedEmail(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"primary and verified", `[{"email":"a@x.com","primary":true,"verified":true}]`, "a@x.com"},
		{"primary but unverified", `[{"email":"a@x.com","primary":true,"verified":false}]`, ""},
		{"verified but not primary", `[{"email":"a@x.com","primary":false,"verified":true}]`, ""},
		{"neither", `[{"email":"a@x.com","primary":false,"verified":false}]`, ""},
		{"empty list", `[]`, ""},
		{
			"second entry qualifies",
			`[{"email":"a@x.com","primary":false,"verified":true},{"email":"b@x.com","primary":true,"verified":true}]`,
			"b@x.com",
		},
		{
			"first qualifying entry wins",
			`[{"email":"a@x.com","primary":true,"verified":true},{"email":"b@x.com","primary":true,"verified":true}]`,
			"a@x.com",
		},
		{"blank address is not an address", `[{"email":"  ","primary":true,"verified":true}]`, ""},
		{"missing fields read as false", `[{"email":"a@x.com"}]`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := selectGitHubVerifiedEmail([]byte(tc.body))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("selectGitHubVerifiedEmail(%s) = %q, want %q", tc.body, got, tc.want)
			}
		})
	}

	// A body that is not a JSON array is an error, not an empty answer:
	// "no verified address" and "I could not read the response" lead to
	// different follow-ups, and collapsing them would hide a GitHub API change
	// behind a login that simply starts refusing people.
	if _, err := selectGitHubVerifiedEmail([]byte(`{"message":"Not Found"}`)); err == nil {
		t.Error("a non-array body must be an error, not an empty selection")
	}
}

// TestVerificationFlag_UnmarshalJSON pins the claim's decoding. Every row is a
// value an issuer actually emits, and the unrecognised ones must read as false
// rather than erroring: an error here is a 502 on a deployment that may not even
// have an allowlist enabled, and `true` must never be read out of something that
// is not the word.
func TestVerificationFlag_UnmarshalJSON(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want bool
	}{
		{`true`, true},
		{`"true"`, true},
		{`"TRUE"`, true},
		{`"True"`, true},
		{`false`, false},
		{`"false"`, false},
		{`"FALSE"`, false},
		{`null`, false},
		{`1`, false},
		{`0`, false},
		{`""`, false},
		{`"yes"`, false},
		{`" true "`, true},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			var v verificationFlag
			if err := json.Unmarshal([]byte(tc.raw), &v); err != nil {
				t.Fatalf("UnmarshalJSON(%s) errored, which is the 502 this type exists to prevent: %v", tc.raw, err)
			}
			if bool(v) != tc.want {
				t.Errorf("UnmarshalJSON(%s) = %v, want %v", tc.raw, bool(v), tc.want)
			}
		})
	}
}

// TestIDTokenClaims_EmailVerifiedStringForm is the same claim arriving through
// the ID token, which is the door the type was written for: a whole
// idTokenClaims decode must not fail on a string-valued email_verified.
func TestIDTokenClaims_EmailVerifiedStringForm(t *testing.T) {
	var claims idTokenClaims
	body := `{"nonce":"n1","email":"ada@example.com","email_verified":"true","sub":"00u1"}`
	if err := json.Unmarshal([]byte(body), &claims); err != nil {
		t.Fatalf("idTokenClaims decode failed on the string form: %v", err)
	}
	if !bool(claims.EmailVerified) {
		t.Error("string-form \"true\" read as unverified")
	}
	if claims.Subject != "00u1" {
		t.Errorf("sub = %q, want 00u1 -- it comes from jwt.RegisteredClaims", claims.Subject)
	}
}
