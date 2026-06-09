// Package auth provides tenant-aware API key authentication for cleat.
//
// It implements Bearer token and header-based auth, tenant ID extraction
// from API keys via PostgreSQL lookup, and context propagation of tenant IDs.
//
// Key types:
//   - TenantStore — PostgreSQL-backed API key to tenant ID resolution
//   - AuthMiddleware — HTTP middleware that validates and propagates tenant context
package auth
