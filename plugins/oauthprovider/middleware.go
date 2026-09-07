package oauthprovider

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/cleat-team/cleat/plugin"
	"github.com/google/uuid"
)

// SessionInfo holds user identity extracted from an OAuth session.
type SessionInfo struct {
	TenantID  uuid.UUID
	SessionID uuid.UUID
	UserEmail string
}

type sessionContextKey struct{}

// SessionFromContext extracts the OAuth session info from the request context.
func SessionFromContext(ctx context.Context) (*SessionInfo, bool) {
	info, ok := ctx.Value(sessionContextKey{}).(*SessionInfo)
	return info, ok
}

// Middleware validates the OAuth session token from the Authorization header
// and injects session info into the request context. It skips /oauth/ and
// /healthz paths.
func (p *Plugin) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if strings.HasPrefix(path, "/oauth/") || path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}

		auth := r.Header.Get("Authorization")
		if auth == "" || !strings.HasPrefix(auth, "Bearer ") {
			next.ServeHTTP(w, r)
			return
		}

		token := strings.TrimPrefix(auth, "Bearer ")
		if token == "" {
			next.ServeHTTP(w, r)
			return
		}

		// Only claim tokens shaped like our own.
		//
		// This middleware cannot ask the database "is this token mine?" -- it
		// can only ask "is this token a live session of mine?", and a NO to the
		// second was treated as a NO to the first until IMPROVEMENT-PLAN 3.246.
		// It therefore answered 401 for every bearer token belonging to another
		// scheme. A cleat API key is `Authorization: Bearer cleat_sk_…`, so once
		// #891 linked this plugin -- and cmd/cleat-worker/main.go wraps the whole
		// handler chain in every plugin implementing HasMiddleware -- no API key
		// worked anywhere (#912). --require-auth defaults to true, so that is a
		// default deployment serving an API nobody can authenticate against.
		//
		// The shape test is what makes the two questions separable, and it is
		// available precisely because the two schemes do not collide:
		// generateSessionToken emits 64 lowercase hex characters and an API key
		// carries a `cleat_sk_` prefix.
		//
		// Falling through is not a loosening. This middleware only ever ADDS
		// SessionInfo; it grants nothing on its own, and it sits OUTSIDE
		// auth.Middleware, which still refuses a request carrying no valid
		// credential.
		if !looksLikeSessionToken(token) {
			next.ServeHTTP(w, r)
			return
		}

		// Hash the incoming token and look up by token_hash.
		tokenHashBytes := sha256.Sum256([]byte(token))
		tokenHash := hex.EncodeToString(tokenHashBytes[:])

		var sessionID uuid.UUID
		var tenantID uuid.UUID
		var userEmail sql.NullString
		var expiresAt sql.NullTime

		err := p.db.QueryRow(r.Context(), plugin.Rebind(`
				SELECT id, tenant_id, user_email, expires_at
				FROM oauth_sessions
				WHERE token_hash = $1 AND (expires_at IS NULL OR expires_at > now())
			`, p.dialect), tokenHash).Scan(&sessionID, &tenantID, &userEmail, &expiresAt)
		if err != nil {
			// Reaching here means the token LOOKS like one of ours (see
			// looksLikeSessionToken above) and is not a live session -- unknown,
			// revoked or expired. That is this plugin's business to refuse, and
			// it still does.
			p.logger.Warn("oauth: invalid session token", "error", err)
			http.Error(w, `{"error":"invalid session"}`, http.StatusUnauthorized)
			return
		}

		info := &SessionInfo{
			TenantID:  tenantID,
			SessionID: sessionID,
			UserEmail: userEmail.String,
		}
		ctx := context.WithValue(r.Context(), sessionContextKey{}, info)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// looksLikeSessionToken reports whether a bearer token has the shape this
// plugin issues: the hex encoding of 32 random bytes, i.e. 64 lowercase hex
// characters.
//
// Kept beside generateSessionToken deliberately, and pinned by
// TestAFreshlyGeneratedSessionTokenLooksLikeOne -- a predicate describing a
// format defined elsewhere is exactly the kind of thing that rots silently, and
// if it ever stops matching, this plugin stops recognising its own sessions.
func looksLikeSessionToken(token string) bool {
	if len(token) != 64 {
		return false
	}
	for i := 0; i < len(token); i++ {
		c := token[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
