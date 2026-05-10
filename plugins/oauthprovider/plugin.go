// Package oauthprovider provides OAuth2/OIDC authentication with support for
// Google, GitHub, and Okta as identity providers.
package oauthprovider

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/rcownie/cleat/internal/plugin"
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

// Plugin implements OAuth2/OIDC authentication with Google, GitHub, and Okta.
type Plugin struct {
	db         plugin.PluginDB
	mux        *http.ServeMux
	logger     *slog.Logger
	httpClient *http.Client
	dialect    plugin.Dialect
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
	p.httpClient = &http.Client{
		Timeout: 30 * time.Second,
	}

	p.logger.Info("oauth-provider: initialized")
	return nil
}
