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
