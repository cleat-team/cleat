// Package auth provides tenant-aware API key authentication for cleat's
// cleat execution framework.
//
// It implements Bearer token and header-based auth, tenant ID extraction
// from API keys via PostgreSQL lookup, and context propagation of tenant IDs.
package auth

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"net/http"
	"strings"

	"github.com/google/uuid"
)

type tenantIDKey struct{}

// WithTenantID sets the tenant ID in the context. Primarily for testing.
func WithTenantID(ctx context.Context, tenantID uuid.UUID) context.Context {
	return context.WithValue(ctx, tenantIDKey{}, tenantID)
}

// TenantIDFromContext extracts the tenant ID from the request context.
func TenantIDFromContext(ctx context.Context) (uuid.UUID, bool) {
	tid, ok := ctx.Value(tenantIDKey{}).(uuid.UUID)
	return tid, ok
}

// TenantFromAPIKey looks up a tenant by API key hash.
func TenantFromAPIKey(ctx context.Context, db *sql.DB, keyHash []byte) (uuid.UUID, error) {
	var tenantID uuid.UUID
	err := db.QueryRowContext(ctx,
		`SELECT tenant_id FROM tenant_api_keys
		 WHERE key_hash = $1 AND revoked_at IS NULL`, keyHash).Scan(&tenantID)
	if err != nil {
		return uuid.Nil, err
	}
	return tenantID, nil
}

// Middleware authenticates requests using a cleat API key.
// Supports: Authorization: Bearer cleat_sk_<key>
// Also supports: X-Cleat-API-Key: <key>
func Middleware(db *sql.DB) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := extractAPIKey(r)
			if key == "" {
				// If no key, proceed without tenant — handler decides if that's OK
				// (e.g., /healthz, /metrics don't need auth)
				next.ServeHTTP(w, r)
				return
			}
			keyHash := sha256Hash(key)
			tenantID, err := TenantFromAPIKey(r.Context(), db, keyHash)
			if err != nil {
				http.Error(w, `{"error":"invalid or revoked API key"}`, http.StatusUnauthorized)
				return
			}
			ctx := context.WithValue(r.Context(), tenantIDKey{}, tenantID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func extractAPIKey(r *http.Request) string {
	// Support Authorization: Bearer <key>
	auth := r.Header.Get("Authorization")
	if auth != "" {
		parts := strings.SplitN(auth, " ", 2)
		if len(parts) == 2 && strings.EqualFold(parts[0], "bearer") {
			return parts[1]
		}
	}
	// Support X-Cleat-API-Key: <key>
	return r.Header.Get("X-Cleat-API-Key")
}

func sha256Hash(s string) []byte {
	h := sha256.Sum256([]byte(s))
	return h[:]
}
