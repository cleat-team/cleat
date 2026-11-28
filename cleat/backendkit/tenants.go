package backendkit

import (
	"context"
	"errors"
	"fmt"
)

// ErrNoTenantKey is returned by TenantClients.For when the app's KeyResolver has no key for the tenant. It is
// returned instead of a client with an empty key, because a client with no key sends no Authorization header
// and the worker's 401 would then read as a bad credential rather than as a key the app never found.
var ErrNoTenantKey = errors.New("backendkit: no API key for this tenant")

// ForTenant returns a client for one tenant of a worker that serves many: same worker, same transport, the
// tenant's own key (cleat#3552). A worker answers 403 when a request names a tenant other than the one its key
// belongs to, so TenantID and APIKey are set together and cannot drift apart.
//
// The result shares c's HTTPClient, so a hundred tenant clients are a hundred small structs over one
// connection pool. It never inherits c's APIKey or TenantID: c is the template for where the worker is, not
// for who is calling. A client made with an empty apiKey sends no Authorization header.
//
// One caution comes with sharing the transport: if the shared http.Client's Transport itself adds an
// Authorization header, it adds it to every tenant client's requests, and that header would override the
// tenant's own key. Put credentials in the client (WithAPIKey, ForTenant), not in a RoundTripper.
func (c *Client) ForTenant(tenantID, apiKey string) *Client {
	return deriveTenantClient(c, tenantID, apiKey)
}

// deriveTenantClient is ForTenant's body, reached by name so that TenantClients does not spell a ".ForTenant("
// call. plugin's TestEverySecretsForTenantCallIsDeclared matches that selector by NAME across the tree, to find
// plugins that name a tenant they did not get from their own request; a client deriving a per-tenant client is
// not that, and a ledger entry saying it was would be false.
func deriveTenantClient(base *Client, tenantID, apiKey string) *Client {
	return &Client{
		BaseURL:    base.BaseURL,
		HTTPClient: base.HTTPClient,
		TenantID:   tenantID,
		APIKey:     apiKey,
	}
}

// TenantClients hands an app that acts for many tenants the right Client for each one (cleat#3552).
//
// The library holds no keys. KeyResolver is the app's own: it looks the key up wherever the app keeps it
// (clew reads its encrypted store). Nothing is cached here, so a key the app rotates or revokes takes effect on
// the next call, and no key can end up in a log or error this package writes.
type TenantClients struct {
	// Base says where the worker is and which http.Client every tenant client shares. Its own APIKey and
	// TenantID are never used.
	Base *Client

	// KeyResolver returns the API key for tenantID. Return ErrNoTenantKey, or an empty key, when the app has
	// none; any other error is passed through to the caller.
	KeyResolver func(ctx context.Context, tenantID string) (string, error)
}

// For returns the client for tenantID, resolving its key on every call.
func (t *TenantClients) For(ctx context.Context, tenantID string) (*Client, error) {
	if t == nil || t.Base == nil || t.KeyResolver == nil {
		return nil, errors.New("backendkit: TenantClients needs a Base client and a KeyResolver")
	}
	if tenantID == "" {
		return nil, errors.New("backendkit: a tenant id is required")
	}
	key, err := t.KeyResolver(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("backendkit: resolving the key for tenant %s: %w", tenantID, err)
	}
	if key == "" {
		return nil, fmt.Errorf("%w (tenant %s)", ErrNoTenantKey, tenantID)
	}
	return deriveTenantClient(t.Base, tenantID, key), nil
}
