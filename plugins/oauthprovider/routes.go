package oauthprovider

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cleat-team/cleat/plugin"
	"github.com/google/uuid"
)

// OAuth provider endpoints.
type providerEndpoints struct {
	authURL     string
	tokenURL    string
	userinfoURL string
	scope       string
}

var endpoints = map[string]providerEndpoints{
	"google": {
		authURL:     "https://accounts.google.com/o/oauth2/v2/auth",
		tokenURL:    "https://oauth2.googleapis.com/token",
		userinfoURL: "https://www.googleapis.com/oauth2/v2/userinfo",
		scope:       "openid email profile",
	},
	"github": {
		authURL:     "https://github.com/login/oauth/authorize",
		tokenURL:    "https://github.com/login/oauth/access_token",
		userinfoURL: "https://api.github.com/user",
		scope:       "read:user",
	},
	"okta": {
		authURL:     "https://%s/oauth2/v1/authorize",
		tokenURL:    "https://%s/oauth2/v1/token",
		userinfoURL: "https://%s/oauth2/v1/userinfo",
		scope:       "openid email profile",
	},
}

var validProviders = map[string]bool{
	"google": true,
	"github": true,
	"okta":   true,
}

// oauthConfigRow represents a row from the oauth_config table.
type oauthConfigRow struct {
	TenantID     uuid.UUID
	Provider     string
	ClientID     string
	ClientSecret string
	RedirectURL  string
	Domain       string
	Enabled      bool
}

// RegisterRoutes registers HTTP handlers for the OAuth flow and session
// management.
func (p *Plugin) RegisterRoutes(mux *http.ServeMux) error {
	if mux == nil {
		return fmt.Errorf("oauth-provider: nil mux")
	}
	mux.HandleFunc("GET /oauth/{provider}/login", p.handleLogin)
	mux.HandleFunc("GET /oauth/{provider}/callback", p.handleCallback)
	mux.HandleFunc("GET /oauth/sessions", p.handleListSessions)
	mux.HandleFunc("DELETE /oauth/sessions/{id}", p.handleDeleteSession)
	return nil
}

// ---- helpers ----

func (p *Plugin) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (p *Plugin) writeError(w http.ResponseWriter, status int, msg string) {
	p.writeJSON(w, status, map[string]string{"error": msg})
}

// tenantID extracts the tenant UUID from the OAuth session in the request
// context. Returns the zero UUID if no session is set.
func (p *Plugin) tenantID(r *http.Request) uuid.UUID {
	info, ok := SessionFromContext(r.Context())
	if !ok {
		return uuid.Nil
	}
	return info.TenantID
}

func (p *Plugin) getConfig(ctx context.Context, tenantID uuid.UUID, provider string) (*oauthConfigRow, error) {
	var cfg oauthConfigRow
	err := p.db.QueryRow(ctx, plugin.Rebind(`
			SELECT tenant_id, provider, client_id, client_secret, redirect_url,
			       COALESCE(domain, '') AS domain, enabled
			FROM oauth_config
			WHERE tenant_id = $1 AND provider = $2 AND enabled = true
		`, p.dialect), tenantID, provider).Scan(
		&cfg.TenantID, &cfg.Provider, &cfg.ClientID, &cfg.ClientSecret,
		&cfg.RedirectURL, &cfg.Domain, &cfg.Enabled,
	)
	if err != nil {
		return nil, err
	}
	return &cfg, nil
}

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func generateSessionToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", b), nil
}

// extractSession validates the Bearer token from the request and returns the
// session info. Returns nil if the token is missing or invalid.
func (p *Plugin) extractSession(r *http.Request) *SessionInfo {
	auth := r.Header.Get("Authorization")
	if auth == "" || !strings.HasPrefix(auth, "Bearer ") {
		return nil
	}
	token := strings.TrimPrefix(auth, "Bearer ")
	if token == "" {
		return nil
	}

	var sessionID uuid.UUID
	var tenantID uuid.UUID
	var userEmail sql.NullString
	var expiresAt sql.NullTime

	tokenHash := sha256Hex(token)

	err := p.db.QueryRow(r.Context(), plugin.Rebind(`
			SELECT id, tenant_id, user_email, expires_at
			FROM oauth_sessions
			WHERE token_hash = $1 AND (expires_at IS NULL OR expires_at > now())
		`, p.dialect), tokenHash).Scan(&sessionID, &tenantID, &userEmail, &expiresAt)
	if err != nil {
		return nil
	}

	return &SessionInfo{
		TenantID:  tenantID,
		SessionID: sessionID,
		UserEmail: userEmail.String,
	}
}

// formatProviderURL substitutes the Okta domain into endpoint templates that
// contain a %s placeholder.
func formatProviderURL(template, domain string) string {
	if strings.Contains(template, "%s") {
		return fmt.Sprintf(template, domain)
	}
	return template
}

// ---- GET /oauth/{provider}/login ----

func (p *Plugin) handleLogin(w http.ResponseWriter, r *http.Request) {
	provider := r.PathValue("provider")
	if !validProviders[provider] {
		p.writeError(w, http.StatusBadRequest, "invalid provider")
		return
	}

	// Get tenant ID from context (main auth middleware) or query param.
	tid := p.tenantID(r)
	if tid == uuid.Nil {
		tidStr := r.URL.Query().Get("tenant_id")
		if tidStr != "" {
			var err error
			tid, err = uuid.Parse(tidStr)
			if err != nil {
				p.writeError(w, http.StatusBadRequest, "invalid tenant_id")
				return
			}
		}
	}
	if tid == uuid.Nil {
		p.writeError(w, http.StatusBadRequest, "tenant_id required")
		return
	}

	cfg, err := p.getConfig(r.Context(), tid, provider)
	if err != nil {
		p.logger.Error("oauth: config lookup", "provider", provider, "error", err)
		p.writeError(w, http.StatusInternalServerError, "oauth config not found")
		return
	}

	// Generate CSRF state nonce.
	stateBytes := make([]byte, 32)
	if _, err := rand.Read(stateBytes); err != nil {
		p.writeError(w, http.StatusInternalServerError, "failed to generate state")
		return
	}
	state := hex.EncodeToString(stateBytes)

	// Generate PKCE code_verifier and code_challenge (RFC 7636).
	verifierBytes := make([]byte, 32)
	if _, err := rand.Read(verifierBytes); err != nil {
		p.writeError(w, http.StatusInternalServerError, "failed to generate code verifier")
		return
	}
	codeVerifier := base64.RawURLEncoding.EncodeToString(verifierBytes)
	h := sha256.Sum256([]byte(codeVerifier))
	codeChallenge := base64.RawURLEncoding.EncodeToString(h[:])

	// Store state + code_verifier in oauth_sessions with 5-minute expiry.
	sessionID := uuid.New()
	sessionExpiresAt := time.Now().Add(5 * time.Minute)
	_, err = p.db.Exec(r.Context(), plugin.Rebind(`
			INSERT INTO oauth_sessions (id, tenant_id, provider, state, code_verifier, expires_at)
			VALUES ($1, $2, $3, $4, $5, $6)
		`, p.dialect), sessionID, tid, provider, state, codeVerifier, sessionExpiresAt)
	if err != nil {
		p.logger.Error("oauth: store state", "error", err)
		p.writeError(w, http.StatusInternalServerError, "failed to initialize login")
		return
	}

	ep := endpoints[provider]
	authURL := formatProviderURL(ep.authURL, cfg.Domain)

	v := url.Values{}
	v.Set("client_id", cfg.ClientID)
	v.Set("redirect_uri", cfg.RedirectURL)
	v.Set("state", state)
	v.Set("response_type", "code")
	v.Set("scope", ep.scope)
	v.Set("code_challenge", codeChallenge)
	v.Set("code_challenge_method", "S256")

	http.Redirect(w, r, authURL+"?"+v.Encode(), http.StatusFound)
}

// ---- GET /oauth/{provider}/callback ----

func (p *Plugin) handleCallback(w http.ResponseWriter, r *http.Request) {
	provider := r.PathValue("provider")
	if !validProviders[provider] {
		p.writeError(w, http.StatusBadRequest, "invalid provider")
		return
	}

	code := r.URL.Query().Get("code")
	state := r.URL.Query().Get("state")
	if code == "" {
		p.writeError(w, http.StatusBadRequest, "missing code")
		return
	}
	if state == "" {
		p.writeError(w, http.StatusBadRequest, "missing state")
		return
	}

	// Look up state in oauth_sessions — verify it exists and hasn't expired.
	var tid uuid.UUID
	var storedProvider string
	var codeVerifier sql.NullString
	var sessionID uuid.UUID

	err := p.db.QueryRow(r.Context(), plugin.Rebind(`
			SELECT id, tenant_id, provider, code_verifier
			FROM oauth_sessions
			WHERE state = $1 AND expires_at > now()
		`, p.dialect), state).Scan(&sessionID, &tid, &storedProvider, &codeVerifier)
	if err != nil {
		p.logger.Error("oauth: state lookup", "error", err)
		p.writeError(w, http.StatusBadRequest, "invalid or expired state")
		return
	}

	if !codeVerifier.Valid || codeVerifier.String == "" {
		p.writeError(w, http.StatusBadRequest, "missing code verifier")
		return
	}

	// Verify the provider in the stored row matches the URL path.
	if storedProvider != provider {
		p.writeError(w, http.StatusBadRequest, "provider mismatch")
		return
	}

	cfg, err := p.getConfig(r.Context(), tid, provider)
	if err != nil {
		p.logger.Error("oauth: config lookup", "provider", provider, "error", err)
		p.writeError(w, http.StatusInternalServerError, "oauth config not found")
		return
	}

	ep := endpoints[provider]
	tokenURL := formatProviderURL(ep.tokenURL, cfg.Domain)

	// Exchange authorization code for tokens with PKCE code_verifier.
	data := url.Values{}
	data.Set("code", code)
	data.Set("client_id", cfg.ClientID)
	data.Set("client_secret", cfg.ClientSecret)
	data.Set("redirect_uri", cfg.RedirectURL)
	data.Set("grant_type", "authorization_code")
	data.Set("code_verifier", codeVerifier.String)

	tokenReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, tokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		p.writeError(w, http.StatusInternalServerError, "failed to create token request")
		return
	}
	tokenReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	tokenReq.Header.Set("Accept", "application/json")

	tokenResp, err := p.httpClient.Do(tokenReq)
	if err != nil {
		p.logger.Error("oauth: token exchange", "error", err)
		p.writeError(w, http.StatusBadGateway, "token exchange failed")
		return
	}
	defer tokenResp.Body.Close()

	var tokenResult struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		Error        string `json:"error"`
		ErrorDesc    string `json:"error_description"`
	}
	if err := json.NewDecoder(tokenResp.Body).Decode(&tokenResult); err != nil {
		p.writeError(w, http.StatusBadGateway, "failed to parse token response")
		return
	}
	if tokenResult.Error != "" {
		msg := tokenResult.Error
		if tokenResult.ErrorDesc != "" {
			msg += ": " + tokenResult.ErrorDesc
		}
		p.logger.Error("oauth: token error", "error", msg)
		p.writeError(w, http.StatusBadGateway, "oauth error: "+tokenResult.Error)
		return
	}

	// Fetch user info from the provider.
	userinfoURL := formatProviderURL(ep.userinfoURL, cfg.Domain)
	userReq, err := http.NewRequestWithContext(r.Context(), http.MethodGet, userinfoURL, nil)
	if err != nil {
		p.writeError(w, http.StatusInternalServerError, "failed to create userinfo request")
		return
	}
	userReq.Header.Set("Authorization", "Bearer "+tokenResult.AccessToken)

	userResp, err := p.httpClient.Do(userReq)
	if err != nil {
		p.logger.Error("oauth: userinfo", "error", err)
		p.writeError(w, http.StatusBadGateway, "userinfo request failed")
		return
	}
	defer userResp.Body.Close()

	var userInfo struct {
		Email string `json:"email"`
		Login string `json:"login"` // GitHub uses "login" instead of "email"
	}
	if err := json.NewDecoder(userResp.Body).Decode(&userInfo); err != nil {
		p.writeError(w, http.StatusBadGateway, "failed to parse userinfo response")
		return
	}

	email := userInfo.Email
	if email == "" {
		email = userInfo.Login
	}

	// Generate a 32-byte hex session token.
	sessionToken, err := generateSessionToken()
	if err != nil {
		p.writeError(w, http.StatusInternalServerError, "failed to generate session token")
		return
	}

	// Hash the session token for at-rest storage.
	tokenHash := sha256Hex(sessionToken)

	var expiresAt *time.Time
	if tokenResult.ExpiresIn > 0 {
		t := time.Now().Add(time.Duration(tokenResult.ExpiresIn) * time.Second)
		expiresAt = &t
	}

	// Update the pre-inserted state row with the actual session data and clear
	// the PKCE fields.
	_, err = p.db.Exec(r.Context(), plugin.Rebind(`
			UPDATE oauth_sessions
			SET session_token = $1, token_hash = $2, user_email = $3,
			    access_token = $4, refresh_token = $5, expires_at = $6,
			    state = NULL, code_verifier = NULL
			WHERE id = $7
		`, p.dialect), sessionToken, tokenHash, email, tokenResult.AccessToken,
		tokenResult.RefreshToken, expiresAt, sessionID)
	if err != nil {
		p.logger.Error("oauth: create session", "error", err)
		p.writeError(w, http.StatusInternalServerError, "failed to create session")
		return
	}

	p.logger.Info("oauth: session created",
		"provider", provider,
		"tenant", tid,
		"email", email,
	)

	p.writeJSON(w, http.StatusOK, map[string]any{
		"session_token": sessionToken,
		"user_email":    email,
		"expires_at":    expiresAt,
	})
}

// ---- GET /oauth/sessions ----

func (p *Plugin) handleListSessions(w http.ResponseWriter, r *http.Request) {
	session := p.extractSession(r)
	if session == nil {
		p.writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	rows, err := p.db.Query(r.Context(), plugin.Rebind(`
			SELECT id, provider, user_email, created_at, expires_at
			FROM oauth_sessions
			WHERE tenant_id = $1
			ORDER BY created_at DESC
		`, p.dialect), session.TenantID)
	if err != nil {
		p.logger.Error("oauth: list sessions", "error", err)
		p.writeError(w, http.StatusInternalServerError, "failed to list sessions")
		return
	}
	defer rows.Close()

	type sessionEntry struct {
		ID        uuid.UUID  `json:"id"`
		Provider  string     `json:"provider"`
		UserEmail string     `json:"user_email"`
		CreatedAt time.Time  `json:"created_at"`
		ExpiresAt *time.Time `json:"expires_at,omitempty"`
	}

	var sessions []sessionEntry
	for rows.Next() {
		var entry sessionEntry
		var userEmail sql.NullString
		var expiresAt sql.NullTime
		if err := rows.Scan(&entry.ID, &entry.Provider, &userEmail, &entry.CreatedAt, &expiresAt); err != nil {
			p.logger.Error("oauth: scan session row", "error", err)
			continue
		}
		entry.UserEmail = userEmail.String
		if expiresAt.Valid {
			entry.ExpiresAt = &expiresAt.Time
		}
		sessions = append(sessions, entry)
	}
	if sessions == nil {
		sessions = []sessionEntry{}
	}

	p.writeJSON(w, http.StatusOK, sessions)
}

// ---- DELETE /oauth/sessions/{id} ----

func (p *Plugin) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	session := p.extractSession(r)
	if session == nil {
		p.writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	idStr := r.PathValue("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		p.writeError(w, http.StatusBadRequest, "invalid session id")
		return
	}

	rows, err := p.db.Exec(r.Context(), plugin.Rebind(`
			DELETE FROM oauth_sessions
			WHERE id = $1 AND tenant_id = $2
		`, p.dialect), id, session.TenantID)
	if err != nil {
		p.logger.Error("oauth: delete session", "error", err)
		p.writeError(w, http.StatusInternalServerError, "failed to delete session")
		return
	}

	if rows == 0 {
		p.writeError(w, http.StatusNotFound, "session not found")
		return
	}

	p.writeJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
}
