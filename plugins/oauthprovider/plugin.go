// Package oauthprovider provides OAuth2/OIDC authentication with support for
// Google, GitHub, and Okta as identity providers.
package oauthprovider

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/cleat-team/cleat/plugin"
)

func init() {
	plugin.Register(plugin.PluginInfo{
		Name:        "oauth-provider",
		Version:     "0.1.0",
		Description: "OAuth2/OIDC authentication provider",
		Author:      "cleat",
	}, func() plugin.Plugin {
		return &Plugin{}
	})
}

// New creates a new Plugin instance.
func New() plugin.Plugin {
	return &Plugin{}
}

// Plugin implements OAuth2/OIDC authentication with Google, GitHub, and Okta.
type Plugin struct {
	db         plugin.PluginDB
	mux        *http.ServeMux
	logger     *slog.Logger
	httpClient *http.Client
	dialect    plugin.Dialect
	secrets    plugin.Secrets

	// hostResolver and requireHostMatch back handleLogin's Host-bound tenant
	// check (cleat#2340) -- see plugin.Environment.HostResolver's doc comment
	// for why both are needed (a nil resolver alone can't distinguish "host
	// binding is off" from "nothing to check against").
	hostResolver     plugin.DomainResolver
	requireHostMatch bool

	// OIDC discovery + JWKS cache for the generic `oidc` provider (cleat#1582).
	// Reached through p.cache() rather than directly: several tests construct a
	// Plugin without calling Init, and a nil map there would panic inside a
	// login rather than fail a check.
	oidcOnce sync.Once
	oidc     *oidcCache
}

// cache returns the OIDC discovery cache, creating it on first use.
func (p *Plugin) cache() *oidcCache {
	p.oidcOnce.Do(func() {
		if p.oidc == nil {
			p.oidc = newOIDCCache()
		}
	})
	return p.oidc
}

// Info returns plugin metadata for discovery and documentation.
func (p *Plugin) Info() plugin.PluginInfo {
	return plugin.PluginInfo{
		Name:        "oauth-provider",
		Version:     "0.1.0",
		Description: "OAuth2/OIDC authentication provider",
		Author:      "cleat",
	}
}

// Init initializes the plugin with the given environment. No config is parsed
// since OAuth provider settings are stored in the oauth_config table.
func (p *Plugin) Init(ctx context.Context, env *plugin.Environment) error {
	if env.Logger != nil {
		p.logger = env.Logger
	} else {
		p.logger = slog.Default()
	}

	p.db = env.DB
	p.mux = env.Mux
	p.dialect = env.Dialect
	p.secrets = env.Secrets
	p.hostResolver = env.HostResolver
	p.requireHostMatch = env.RequireHostMatch
	p.httpClient = &http.Client{
		// cleat#1565: every outbound request goes through the egress guard.
		// Nil in tests that build an Environment directly, which falls back to
		// the default transport -- TestEveryPluginRoutesItsEgressThroughTheGuard
		// is what keeps that from being how production works.
		Transport: env.HTTPTransport,
		Timeout:   30 * time.Second,
	}

	// cleat#2340: OAuth login is Postgres-only for 0.3.0 (a minted credential
	// can't be revoked on mysql/mssql yet -- auth.RevokeAPIKeyByHash doesn't
	// exist there, same limitation auth.TenantStore.RevokeAPIKey already has).
	// Logged once here, at the dialect the WORKER is running, not per tenant
	// config -- oauth_config rows can be added after this without a restart,
	// so this can only ever say "this deployment can't", never "nobody uses
	// this yet". handleLogin/handleCallback refuse per-request rather than
	// failing Init: every bundled plugin initializes on every boot against
	// one flat --plugin-config with no reliable "am I configured" signal, and
	// an Init-time refusal would stop every mysql/mssql worker from starting
	// whether or not anyone uses OAuth (see #2202's email-plugin incident).
	if p.dialect != plugin.DialectPostgres {
		p.logger.Info("oauth-provider: OAuth login is Postgres-only in this release; "+
			"/login and /callback will refuse on this dialect",
			"dialect", p.dialect)
	}

	p.logger.Info("oauth-provider: initialized")
	return nil
}

// pgOnly refuses the request with 501 if the worker isn't running Postgres,
// and reports whether it did -- callers return immediately when true. See
// Init's dialect-log comment for why this refuses per-request instead of at
// Init.
func (p *Plugin) pgOnly(w http.ResponseWriter) bool {
	if p.dialect == plugin.DialectPostgres {
		return false
	}
	p.writeError(w, http.StatusNotImplemented,
		"OAuth login is only supported on the postgres dialect in this release")
	return true
}
