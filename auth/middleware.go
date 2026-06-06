// Package auth provides tenant-aware API key authentication for cleat's
// cleat execution framework.
//
// It implements Bearer token and header-based auth, tenant ID extraction
// from API keys via PostgreSQL lookup, and context propagation of tenant IDs.
package auth

import (
	"context"
	"crypto/sha256"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/cleat-team/cleat/engine"
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
func TenantFromAPIKey(ctx context.Context, store engine.WorkflowStore, keyHash []byte) (uuid.UUID, error) {
	return store.ResolveTenantFromAPIKey(ctx, keyHash)
}

// Middleware authenticates requests using a cleat API key.
// Supports: Authorization: Bearer cleat_sk_<key>
// Also supports: X-Cleat-API-Key: <key>
// When requireAuth is true, requests without a valid API key are rejected with 401,
// except for public paths (/healthz, /metrics).
func Middleware(store engine.WorkflowStore, requireAuth bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Public paths are always accessible without authentication.
			path := r.URL.Path
			if path == "/healthz" || path == "/metrics" ||
				path == "/" || path == "/index.html" ||
				strings.HasPrefix(path, "/assets/") ||
				(path == "/favicon.ico") {
				next.ServeHTTP(w, r)
				return
			}

			key := extractAPIKey(r)
			if key == "" {
				if requireAuth {
					http.Error(w, `{"error":"authentication required: provide an API key via Authorization: Bearer <key> or X-Cleat-API-Key: <key>"}`, http.StatusUnauthorized)
					return
				}
				// If not required, proceed without tenant — handler decides if that's OK.
				next.ServeHTTP(w, r)
				return
			}
			keyHash := sha256Hash(key)
			tenantID, err := TenantFromAPIKey(r.Context(), store, keyHash)
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
