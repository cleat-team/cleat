package oauthprovider

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/plugin"
	"github.com/google/uuid"
)

// OAuth provider endpoints.
type providerEndpoints struct {
	authURL     string
	tokenURL    string
	userinfoURL string
	scope       string
	// userEmailsURL is where to ask whether an address the provider reported
	// is one it VOUCHES for. Empty for every provider whose userinfo answer is
	// already authoritative about that -- see the GitHub entry below, which is
	// the only one that sets it. cleat#2340.
	userEmailsURL string
}

// endpoints holds each provider's PUBLIC OAuth URLs. No secret is stored here:
// the client ID and secret come from tenant configuration at request time.
//
// gosec's G101 reports one HIGH-severity "potential hardcoded credentials"
// finding per entry -- three as of 2026-09-04, one for each provider block. It
// is matching the field NAME `tokenURL` against its credential-name pattern,
// not looking at the value, and every value here is a documented endpoint
// published by Google, GitHub and Okta. Three findings, one cause, no secret.
//
// Verified before this comment was written rather than assumed: the only
// `%s`-bearing entries are Okta's, where the host is the tenant's own Okta
// domain, and nothing in this map is read as a credential.
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
		// user:email is REQUIRED, not optional, and its absence was the
		// first half of the cleat#2340 defect. GitHub's /user returns `email`
		// only when the account has made an address public, and null
		// otherwise -- so for a private-email account (the common case) the
		// only address available is the one /user/emails reports, and reading
		// that endpoint at all needs this scope. Without it the call is a
		// 404/403 and the login has no address it can vouch for.
		userEmailsURL: "https://api.github.com/user/emails",
		scope:         "read:user user:email",
	},
	"okta": {
		authURL:     "https://%s/oauth2/v1/authorize",
		tokenURL:    "https://%s/oauth2/v1/token",
		userinfoURL: "https://%s/oauth2/v1/userinfo",
		scope:       "openid email profile",
	},
}

// validProviders is no longer the list of IdPs cleat supports -- `oidc` covers
// any of them. It is the list of provider KINDS. The three named entries are
// sugar over the same path: their endpoints are known so they need no discovery
// round trip, but nothing else about them is special. cleat#1582.
var validProviders = map[string]bool{
	"google": true,
	"github": true,
	"okta":   true,
	"oidc":   true,
}

// providerOIDC is the generic kind, configured with an issuer URL instead of
// an entry in the endpoints table.
const providerOIDC = "oidc"

// resolveEndpoints produces the endpoint set for a configured provider.
//
// ONE function for both kinds, called by both handlers, because the failure
// this design is avoiding is login and callback disagreeing about where the
// token endpoint is. For google/github/okta it reads the hardcoded table; for
// `oidc` it performs discovery, which is a network call and can fail.
//
// The scope for a discovered issuer is the OIDC minimum plus email. `openid`
// is what makes the request an OIDC one at all and is what causes an ID token
// to be issued.
func (p *Plugin) resolveEndpoints(ctx context.Context, provider string, cfg *oauthConfigRow) (providerEndpoints, error) {
	if provider != providerOIDC {
		ep, ok := endpoints[provider]
		if !ok {
			return providerEndpoints{}, fmt.Errorf("no endpoints for provider %q", provider)
		}
		return providerEndpoints{
			authURL:       formatProviderURL(ep.authURL, cfg.Domain),
			tokenURL:      formatProviderURL(ep.tokenURL, cfg.Domain),
			userinfoURL:   formatProviderURL(ep.userinfoURL, cfg.Domain),
			scope:         ep.scope,
			userEmailsURL: formatProviderURL(ep.userEmailsURL, cfg.Domain),
		}, nil
	}

	if strings.TrimSpace(cfg.Issuer) == "" {
		return providerEndpoints{}, fmt.Errorf("provider %q requires an issuer URL in oauth_config.issuer", providerOIDC)
	}
	doc, _, err := p.discover(ctx, cfg.Issuer)
	if err != nil {
		return providerEndpoints{}, err
	}
	return providerEndpoints{
		authURL:     doc.AuthorizationEndpoint,
		tokenURL:    doc.TokenEndpoint,
		userinfoURL: doc.UserinfoEndpoint,
		scope:       "openid email profile",
	}, nil
}

// oauthConfigRow represents a row from the oauth_config table, plus the
// tenant secret that used to be one of its columns.
type oauthConfigRow struct {
	TenantID     uuid.UUID
	Provider     string
	ClientID     string
	ClientSecret plugin.Secret
	RedirectURL  string
	Domain       string
	Issuer       string
	Enabled      bool
}

// OAuthClientSecretName is the tenant-secret name an oauth_config row's
// client secret is stored under. cleat#1992.
//
// ONE NAME PER (TENANT, PROVIDER), not per config id like
// datadogexport.DatadogAPIKeySecretName: oauth_config's own primary key is
// (tenant_id, provider), so a tenant already has at most one row per
// provider. Keying by provider alone preserves that -- there is no second
// config of the same provider a fixed name could collide with.
//
// EXPORTED for the same reason DatadogAPIKeySecretName is: a caller seeding
// or verifying an oauth_config secret (a test, tests/plugin-harness)
// computes the name the same way rather than reimplementing the scheme.
func OAuthClientSecretName(provider string) string {
	return "oauthprovider.client_secret." + provider
}

// RegisterRoutes registers HTTP handlers for the OAuth flow and session
// management.
func (p *Plugin) RegisterRoutes(mux plugin.Router) error {
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
// context, and whether a session was present. A session's TenantID can
// legitimately be uuid.Nil -- the seeded default tenant -- so callers must
// check ok, not compare the UUID to uuid.Nil, to tell "no session" apart
// from "authenticated as the default tenant".
func (p *Plugin) tenantID(r *http.Request) (uuid.UUID, bool) {
	info, ok := SessionFromContext(r.Context())
	if !ok {
		return uuid.Nil, false
	}
	return info.TenantID, true
}

func (p *Plugin) getConfig(ctx context.Context, tenantID uuid.UUID, provider string) (*oauthConfigRow, error) {
	var cfg oauthConfigRow
	// Scoped by the tenant this call is FOR, not by whatever the request
	// context happens to carry. cleat#1512. Both callers reach here on paths
	// where the request is not necessarily tenant-authenticated -- handleLogin
	// accepts ?tenant_id= precisely because a login is unauthenticated -- so
	// the value in hand is the only reliable one.
	ctx = plugin.ForTenant(ctx, tenantID)
	err := plugin.ScanRow(p.db.QueryRow(ctx, plugin.Rebind(`
			SELECT tenant_id, provider, client_id, redirect_url,
			       COALESCE(domain, '') AS domain, COALESCE(issuer, '') AS issuer, enabled
			FROM oauth_config
			WHERE tenant_id = $1 AND provider = $2 AND enabled = true
		`, p.dialect), tenantID, provider),
		&cfg.TenantID, &cfg.Provider, &cfg.ClientID,
		&cfg.RedirectURL, &cfg.Domain, &cfg.Issuer, &cfg.Enabled,
	)
	if err != nil {
		return nil, err
	}

	// client_secret moved into tenant secrets, cleat#1992. Same ctx: it is
	// already ForTenant-marked above, and plugin.Secrets reads the tenant
	// from ctx the identical way plugin.ForTenant marks it for SQL.
	secret, err := p.secrets.Get(ctx, OAuthClientSecretName(provider))
	if err != nil {
		// errors.Is(err, plugin.ErrSecretNotFound) survives this wrap, so a
		// caller can still tell "this provider has no client secret set" (an
		// operator misconfiguration) apart from "the secret lookup itself
		// failed" (an infrastructure problem) -- see handleLogin/
		// handleCallback, cleat-review's item (4) on cleat#2295.
		return nil, fmt.Errorf("oauth-provider: client secret for %s/%s: %w", tenantID, provider, err)
	}
	cfg.ClientSecret = plugin.Secret(secret)

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

	// CROSS-TENANT, and this one is not a convenience. cleat#1512.
	//
	// The lookup is BY TOKEN HASH and its output is the tenant: this is the
	// call that discovers which tenant the caller belongs to. There is no
	// tenant to scope it by, and there cannot be one, because finding it is the
	// point. A policy calling cleat.assert_tenant_set() would RAISE here and
	// take session authentication with it.
	//
	// Not a leak: the predicate is a SHA-256 token hash, so seeing another
	// tenant's row requires already holding that tenant's session token.
	err := plugin.ScanRow(p.db.QueryRow(
		plugin.AcrossAllTenants(r.Context(), "oauth session lookup: the token hash identifies the tenant, so there is none to scope by"),
		plugin.Rebind(`
			SELECT id, tenant_id, user_email, expires_at
			FROM oauth_sessions
			WHERE token_hash = $1 AND (expires_at IS NULL OR expires_at > now())
		`, p.dialect), tokenHash), &sessionID, &tenantID, &userEmail, &expiresAt)
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
	if p.pgOnly(w) {
		return
	}

	provider := r.PathValue("provider")
	if !validProviders[provider] {
		p.writeError(w, http.StatusBadRequest, "invalid provider")
		return
	}

	// Get tenant ID from context (main auth middleware) or query param.
	tid, ok := p.tenantID(r)
	if !ok {
		tidStr := r.URL.Query().Get("tenant_id")
		if tidStr != "" {
			var err error
			tid, err = uuid.Parse(tidStr)
			if err != nil {
				p.writeError(w, http.StatusBadRequest, "invalid tenant_id")
				return
			}
			ok = true
		}
	}
	if !ok {
		p.writeError(w, http.StatusBadRequest, "tenant_id required")
		return
	}

	// cleat#2340: /login is exempt from auth.HostBindingMiddlewareWithMux
	// (it has to be, cleat#2319 -- an anonymous caller carries no credential
	// for that middleware to bind a tenant from), so nothing else enforces
	// Host against the tenant this handler resolves for itself. Without
	// this, --require-host-match protects every other route but not the one
	// that starts a login: ?tenant_id=<victim> from an attacker-controlled
	// host would otherwise mint a credential for a tenant the request's Host
	// doesn't own. p.hostResolver is built unconditionally by the worker
	// (see plugin.Environment.HostResolver's doc comment), so nil here only
	// happens in a test building an Environment by hand -- treated as "can't
	// check" rather than "check passed", refusing rather than silently
	// skipping the binding --require-host-match promised.
	if p.requireHostMatch {
		if p.hostResolver == nil {
			p.logger.Error("oauth: --require-host-match is set but no host resolver was provided")
			p.writeError(w, http.StatusInternalServerError, "host binding is misconfigured")
			return
		}
		host := auth.NormalizeHost(r.Host)
		bound, err := p.hostResolver.TenantForHost(r.Context(), host, tid)
		if err != nil {
			p.logger.Error("oauth: host binding check", "error", err)
			p.writeError(w, http.StatusInternalServerError, "host binding check failed")
			return
		}
		if !bound {
			p.writeError(w, http.StatusBadRequest, "tenant_id does not match the requested host")
			return
		}
	}

	cfg, err := p.getConfig(r.Context(), tid, provider)
	if err != nil {
		// errors.Is(err, plugin.ErrSecretNotFound) distinguishes "this
		// provider has no client secret set" (an operator setup step was
		// skipped) from every other lookup failure, and that distinction is
		// worth the operator's attention -- but only in the log, not the
		// response: this handler is UNAUTHENTICATED (handleLogin accepts
		// ?tenant_id= from anyone), so an operator-actionable message
		// naming cleat's internal secret scheme and the cleatctl command
		// that fixes it would hand an anonymous caller detail about cleat's
		// own tooling (cleat-review's item (4) on cleat#2295).
		//
		// THE STATUS WAS A SECOND CHANNEL FOR THE SAME BIT, and this branch is
		// where cleat#2368 closed it. It answered 500 here, while a configured
		// pair ended at the redirect this handler closes with (302) -- so an
		// anonymous caller supplying ?tenant_id=<guess> could read "this
		// (tenant, provider) pair IS configured" off the status alone, which
		// is the enumeration surface #2368 is about. It now answers the same
		// 302, at a same-origin relative path so a caller cannot turn this
		// into an open redirect, and harmless for a pair that cannot complete
		// a login.
		//
		// WHAT THIS DOES NOT CLOSE, recorded because it is inherent to the
		// feature rather than an oversight: a login that succeeds for a
		// configured pair must hand the browser the provider's authorize URL,
		// so that response necessarily carries something an unconfigured pair
		// cannot produce, and the Location header still separates the two.
		// Closing that would mean not logging in at all -- cleat#2368's option
		// (B). The owner chose (A), which closes the cheapest channel and
		// leaves this one recorded rather than unnoticed.
		p.logger.Error("oauth: config lookup", "provider", provider, "error", err,
			"secret_not_found", errors.Is(err, plugin.ErrSecretNotFound))
		http.Redirect(w, r, "/", http.StatusFound)
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

	// Resolve endpoints BEFORE writing the session row. For `oidc` this is a
	// network call to the issuer, and a failure here means no login is
	// possible -- writing the row first would leave a usable state value
	// behind for a flow that never started.
	ep, err := p.resolveEndpoints(r.Context(), provider, cfg)
	if err != nil {
		p.logger.Error("oauth: resolve endpoints", "provider", provider, "error", err)
		p.writeError(w, http.StatusBadGateway, "provider discovery failed")
		return
	}

	// Mint a nonce for OIDC. It goes out on the authorize request and must
	// come back inside the signed ID token; storing it here is what lets the
	// callback tell this login apart from a replayed one.
	nonceBytes := make([]byte, 32)
	if _, err := rand.Read(nonceBytes); err != nil {
		p.writeError(w, http.StatusInternalServerError, "failed to generate nonce")
		return
	}
	nonce := hex.EncodeToString(nonceBytes)

	// Store state + code_verifier in oauth_sessions with 5-minute expiry.
	sessionID := uuid.New()
	sessionExpiresAt := time.Now().Add(5 * time.Minute)
	// ForTenant: tid is known here but the request is unauthenticated by
	// definition -- it may have come from ?tenant_id= -- so nothing has put it
	// in the context carrier the policy reads. cleat#1512.
	_, err = p.db.Exec(plugin.ForTenant(r.Context(), tid), plugin.Rebind(`
			INSERT INTO oauth_sessions (id, tenant_id, provider, state, code_verifier, nonce, expires_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
		`, p.dialect), sessionID, tid, provider, state, codeVerifier, nonce, sessionExpiresAt)
	if err != nil {
		p.logger.Error("oauth: store state", "error", err)
		p.writeError(w, http.StatusInternalServerError, "failed to initialize login")
		return
	}

	v := url.Values{}
	v.Set("client_id", cfg.ClientID)
	v.Set("redirect_uri", cfg.RedirectURL)
	v.Set("state", state)
	v.Set("response_type", "code")
	v.Set("scope", ep.scope)
	v.Set("code_challenge", codeChallenge)
	v.Set("code_challenge_method", "S256")
	if provider == providerOIDC {
		v.Set("nonce", nonce)
	}

	// The authorize endpoint may already carry a query string -- a discovered
	// one legitimately can. Appending "?" unconditionally would corrupt it.
	sep := "?"
	if strings.Contains(ep.authURL, "?") {
		sep = "&"
	}
	http.Redirect(w, r, ep.authURL+sep+v.Encode(), http.StatusFound)
}

// ---- GET /oauth/{provider}/callback ----

func (p *Plugin) handleCallback(w http.ResponseWriter, r *http.Request) {
	if p.pgOnly(w) {
		return
	}

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
	var storedNonce sql.NullString
	var sessionID uuid.UUID

	// CROSS-TENANT and deriving, like the token-hash lookups. cleat#1512.
	//
	// An OAuth callback arrives from the identity provider carrying only
	// `state`, on a request that is not authenticated and has no tenant. The
	// row is what says which tenant this flow belongs to, so there is nothing
	// to scope the lookup by. The UPDATE further down, which runs after tid is
	// known, is scoped with ForTenant rather than inheriting this.
	err := plugin.ScanRow(p.db.QueryRow(
		plugin.AcrossAllTenants(r.Context(), "oauth callback: the state parameter identifies the tenant, so there is none to scope by"),
		plugin.Rebind(`
			SELECT id, tenant_id, provider, code_verifier, nonce
			FROM oauth_sessions
			WHERE state = $1 AND expires_at > now()
		`, p.dialect), state), &sessionID, &tid, &storedProvider, &codeVerifier, &storedNonce)
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
		// errors.Is(err, plugin.ErrSecretNotFound) distinguishes "this
		// provider has no client secret set" (an operator setup step was
		// skipped) from every other lookup failure, and that distinction is
		// worth the operator's attention -- but only in the log, not the
		// response, so an operator-actionable message naming cleat's internal
		// secret scheme and the cleatctl command that fixes it does not hand
		// a caller detail about cleat's own tooling (cleat-review's item (4)
		// on cleat#2295).
		//
		// This route is unauthenticated because it is EXEMPT from plugin auth,
		// not because it reads a tenant from the query string -- that
		// justification sat on both handlers and describes only handleLogin.
		// Here tid comes from the oauth_sessions row the state parameter
		// selects (the CROSS-TENANT lookup above), so the caller cannot choose
		// whose config is read, and any pair this branch could probe,
		// handleLogin already probes directly.
		//
		// The uniform TEXT closes the STRING oracle only; the STATUS here
		// still discriminates (500 here, the 200 JSON finishLogin returns on
		// success). This branch is NOT cleat#2368's fix -- that issue closed
		// the login route's status. It is not the same oracle either: a caller
		// reaches this only with a state row, and production mints one only
		// after a login already cleared getConfig (the INSERT in handleLogin),
		// so any pair this branch could probe, handleLogin already answered.
		p.logger.Error("oauth: config lookup", "provider", provider, "error", err,
			"secret_not_found", errors.Is(err, plugin.ErrSecretNotFound))
		p.writeError(w, http.StatusInternalServerError, "oauth config not found")
		return
	}

	ep, err := p.resolveEndpoints(r.Context(), provider, cfg)
	if err != nil {
		p.logger.Error("oauth: resolve endpoints", "provider", provider, "error", err)
		p.writeError(w, http.StatusBadGateway, "provider discovery failed")
		return
	}
	tokenURL := ep.tokenURL

	// Exchange authorization code for tokens with PKCE code_verifier.
	data := url.Values{}
	data.Set("code", code)
	data.Set("client_id", cfg.ClientID)
	data.Set("client_secret", cfg.ClientSecret.Reveal())
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
		IDToken      string `json:"id_token"`
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

	// Validate the ID token whenever one came back.
	//
	// Userinfo remains authoritative for identity -- it is the path every
	// provider here has always used -- but a token that fails validation is a
	// HARD failure, not something to shrug off and fall back from. A validator
	// that can be skipped on error is the permissive validator
	// docs/enterprise-identity-decision.md warns about, and skipping it is
	// indistinguishable from a forged token succeeding.
	//
	// The verification claim and the subject come out of the same validated
	// token, which is the only place either is cryptographically attested:
	// `email_verified` says the issuer proved the address belongs to the
	// account, and `sub` is the account. cleat#2340.
	var identity resolvedIdentity
	if tokenResult.IDToken != "" && provider == providerOIDC {
		claims, err := p.validateIDToken(r.Context(), tokenResult.IDToken, cfg.Issuer, cfg.ClientID, storedNonce.String)
		if err != nil {
			p.logger.Error("oauth: id_token validation", "provider", provider, "error", err)
			p.writeError(w, http.StatusUnauthorized, "id_token validation failed")
			return
		}
		identity.Email = claims.Email
		identity.EmailVerified = bool(claims.EmailVerified)
		identity.Subject = normalizeSubject(claims.Subject)
	}

	// Fetch user info from the provider.
	//
	// userinfo_endpoint is RECOMMENDED rather than required by OIDC Discovery,
	// so a conforming issuer may publish none. When that happens the validated
	// ID token is the only identity available, and it has already passed
	// signature, issuer, audience, expiry and nonce -- so it is a sound source
	// here, unlike the unvalidated case this code is careful never to reach.
	userinfoURL := ep.userinfoURL
	if userinfoURL == "" {
		if identity.Email == "" {
			p.logger.Error("oauth: no identity source", "provider", provider)
			p.writeError(w, http.StatusBadGateway, "issuer publishes no userinfo endpoint and returned no usable id_token")
			return
		}
		p.finishLogin(w, r, tid, provider, sessionID, identity, tokenResult.ExpiresIn)
		return
	}
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
		// EmailVerified is a verificationFlag, not a bool: the claim is
		// optional and some issuers emit it as a string. An unreadable value
		// must read as unverified rather than turning this decode into a 502
		// -- see that type's doc comment for why the direction matters.
		EmailVerified verificationFlag `json:"email_verified"`
		Sub           string           `json:"sub"` // OIDC's stable account id
		ID            int64            `json:"id"`  // GitHub's, which is numeric
	}
	if err := json.NewDecoder(userResp.Body).Decode(&userInfo); err != nil {
		p.writeError(w, http.StatusBadGateway, "failed to parse userinfo response")
		return
	}

	// The address, source by source, with the verification claim that belongs
	// to whichever source supplied it.
	//
	// GitHub is a case of its own, and it is the case cleat#2340 item 1 is
	// about. Until this change the fallback here was `userInfo.Login`, and
	// `login` is a GitHub USERNAME -- user-changeable, not an address, and
	// never proved to belong to anyone. An allowlist keyed on it would have
	// admitted whoever registered the name next. It is deleted rather than
	// demoted, because there is now a real source for a GitHub address:
	// /user/emails, which labels each address verified or not, and the scope
	// that lets us read it (user:email, in the endpoints table).
	if provider == "github" {
		verified, verr := p.githubVerifiedEmail(r.Context(), tokenResult.AccessToken, ep.userEmailsURL)
		if verr != nil {
			// Not fatal: an address we could not confirm is exactly the
			// unverified case below, and the login still has the /user
			// address (if any) to be labelled with. Logged because it is the
			// difference between "this account has no verified address" and
			// "we could not ask", and only one of those is the user's
			// problem.
			p.logger.Warn("oauth: github verified-email lookup", "error", verr)
		}
		switch {
		case verified != "":
			identity.Email = verified
			identity.EmailVerified = true
		case userInfo.Email != "":
			// GitHub returns this only for accounts with a PUBLIC profile
			// email, and publishes no claim about it here. Carried so the
			// session row has a label; not eligible to match an allowlist
			// row, because nothing has vouched for it.
			identity.Email = userInfo.Email
			identity.EmailVerified = false
		}
		if userInfo.ID != 0 {
			identity.Subject = strconv.FormatInt(userInfo.ID, 10)
		}
	} else if userInfo.Email != "" {
		// Source precedence is unchanged from before this change -- userinfo
		// first, the ID token only as a fallback -- and the verification flag
		// travels with the source that supplied the address rather than being
		// OR-ed across both. An issuer that marks the address verified in the
		// userinfo response and unverified in the token (or vice versa) is
		// saying the same thing twice; picking either one is a choice, and
		// picking the source we actually use keeps the answer explainable.
		identity.Email = userInfo.Email
		identity.EmailVerified = bool(userInfo.EmailVerified)
	}

	// The subject ladder for the OIDC-shaped providers. userinfo's `sub` is
	// accepted because it arrives over TLS from the endpoint the operator's own
	// config names, carrying an access token minted moments ago at that same
	// endpoint -- and the validated ID token's `sub`, when there is one, wins,
	// since that is the copy with a signature behind it.
	if identity.Subject == "" {
		identity.Subject = normalizeSubject(userInfo.Sub)
	}

	p.finishLogin(w, r, tid, provider, sessionID, identity, tokenResult.ExpiresIn)
}

// finishLogin turns a verified identity into a session row and a response.
//
// Extracted so the two ways a callback can arrive here -- via userinfo, or via
// a validated ID token when the issuer publishes no userinfo endpoint -- write
// the session exactly the same way. A second copy of this is how the two paths
// would drift on something like clearing the nonce. The allowlist gate below is
// a third reason: a check that ran on one path and not the other would be an
// authentication bypass reachable by choosing an issuer whose discovery
// document omits userinfo_endpoint.
func (p *Plugin) finishLogin(
	w http.ResponseWriter, r *http.Request,
	tid uuid.UUID, provider string, sessionID uuid.UUID,
	id resolvedIdentity, expiresIn int,
) {
	// The guard is on the ADDRESS and stays on the address. An identity with a
	// subject but no address is still refused here, exactly as it was before
	// the subject became an allowlist key, because relaxing this would admit
	// logins that fail today on every deployment -- with no operator opt-in,
	// on a change whose entire purpose is to narrow who gets in. A tenant whose
	// issuer publishes no address must keep using an issuer that does; a tenant
	// whose issuer publishes an address but no email_verified claim can admit
	// people with a subject row (see identityAllowed).
	if id.Email == "" {
		p.logger.Error("oauth: no email resolved", "provider", provider, "tenant", tid)
		p.writeError(w, http.StatusBadGateway, "provider returned no usable identity")
		return
	}

	// ForTenant with the tid the state lookup above derived. The UPDATE below
	// addresses the row BY ID with no tenant predicate, so the policy is what
	// keeps a state collision from writing another tenant's session, and the
	// allowlist read below is scoped the same way for the same reason.
	ctx := plugin.ForTenant(r.Context(), tid)

	// The allowlist gate. cleat#2340 item 2, unconditional since cleat#2371.
	//
	// BEFORE the token is minted and before the UPDATE, so a refused login
	// leaves no trace on the session row: the row keeps its state and its
	// expiry and is swept by the ordinary abandoned-row path. A gate placed
	// after the write would refuse the response while still having minted a
	// usable token_hash, which is the failure mode this ordering exists to
	// make impossible.
	//
	// It FAILS CLOSED, with no opt-in. An oauth_config.allowlist_enabled column
	// used to gate this block, defaulting false; cleat#2371 removed it on the
	// owner's decision, so a (tenant, provider) pair with no rows in
	// oauth_allowed_identities denies every login for that pair until an
	// operator writes one. "There is no list" and "this person is not on the
	// list" are deliberately the same answer: a second column saying which of
	// the two it is can only disagree with the table it is meant to describe,
	// and nothing reading that table could tell the difference either.
	allowed, aerr := p.identityAllowed(ctx, tid, provider, id)
	if aerr != nil {
		p.logger.Error("oauth: allowlist lookup", "provider", provider, "tenant", tid, "error", aerr)
		p.writeError(w, http.StatusInternalServerError, "failed to evaluate the identity allowlist")
		return
	}
	if !allowed {
		// Operator-actionable in the LOG, not the response. The caller may
		// be anyone who reached this callback -- handleLogin accepts
		// ?tenant_id= -- so the response names neither the table nor the
		// row that would fix it (the same reasoning as handleCallback's
		// secret_not_found branch, cleat#2295). The log carries exactly
		// what an operator needs to write the INSERT: the identity, its
		// kind, and whether the address was even eligible to match.
		p.logger.Warn("oauth: identity refused by the tenant's allowlist",
			"provider", provider, "tenant", tid,
			"email", id.Email, "email_verified", id.EmailVerified,
			"subject", id.Subject)
		p.writeJSON(w, http.StatusForbidden, map[string]any{
			"error":   "identity not authorized",
			"code":    "identity_not_allowlisted",
			"message": "this tenant restricts which identities may sign in with " + provider + "; contact your cleat administrator",
		})
		return
	}

	// Generate a 32-byte hex session token.
	sessionToken, err := generateSessionToken()
	if err != nil {
		p.writeError(w, http.StatusInternalServerError, "failed to generate session token")
		return
	}

	// Hash the session token for at-rest storage. token_hash is what
	// middleware/extractSession actually looks a caller up by; session_token
	// itself is not stored at all (see the comment below the ctx line).
	tokenHash := sha256Hex(sessionToken)

	var expiresAt *time.Time
	if expiresIn > 0 {
		t := time.Now().Add(time.Duration(expiresIn) * time.Second)
		expiresAt = &t
	}

	// session_token/access_token/refresh_token are NOT persisted, cleat#2295
	// (closes cleat#2156's ask for these three; client_secret, the fourth
	// field #2156 named, is handled in getConfig via plugin.Secrets and is
	// unaffected by this).
	//
	// cleat-review found that sealing them via plugin.Payloads (the design
	// this replaced) broke every login on any deployment that had not set
	// --encryption-key-file -- which is every deployment shipped so far --
	// because a nil Payloads makes Seal fail closed. The owner's fix (#2295,
	// #2296): since nothing reads these three back (session lookup is by
	// token_hash, below, not by session_token; access_token/refresh_token
	// have no read path at all), storing them in any form -- plaintext or
	// sealed -- protects nothing and only grows the blast radius of a table
	// dump. Write NULL. A future caller that needs to read a provider access
	// or refresh token back must add real storage for it, not resurrect
	// these columns.
	//
	// Update the pre-inserted state row with the actual session data and clear
	// the PKCE fields. The nonce is cleared with them: it is single-use by
	// definition, and a spent nonce left in the row is a replay waiting for a
	// state collision.
	_, err = p.db.Exec(ctx, plugin.Rebind(`
			UPDATE oauth_sessions
			SET session_token = NULL, token_hash = $1, user_email = $2,
			    access_token = NULL, refresh_token = NULL, expires_at = $3,
			    state = NULL, code_verifier = NULL, nonce = NULL
			WHERE id = $4
		`, p.dialect), tokenHash, id.Email, expiresAt, sessionID)
	if err != nil {
		p.logger.Error("oauth: create session", "error", err)
		p.writeError(w, http.StatusInternalServerError, "failed to create session")
		return
	}

	// Reaching this line now means the allowlist admitted this identity: the
	// gate above is unconditional since cleat#2371, and it returns before here
	// on both a lookup error and a refusal. So this line needs no "was there a
	// list" key -- there always was one, and it said yes. The refusal case has
	// its own Warn, which is where the identity an operator must add is
	// recorded.
	p.logger.Info("oauth: session created",
		"provider", provider,
		"tenant", tid,
		"email", id.Email,
	)

	p.writeJSON(w, http.StatusOK, map[string]any{
		"session_token": sessionToken,
		"user_email":    id.Email,
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

	rows, err := p.db.Query(plugin.ForTenant(r.Context(), session.TenantID), plugin.Rebind(`
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
		if err := plugin.ScanRow(rows, &entry.ID, &entry.Provider, &userEmail, &entry.CreatedAt, &expiresAt); err != nil {
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

	rows, err := p.db.Exec(plugin.ForTenant(r.Context(), session.TenantID), plugin.Rebind(`
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
