// Package email provides SendGrid transactional email integration for workflows.
// It lets workflows send transactional emails via SendGrid, and exposes
// HostCall functions callable from WASM workflow code.
package email

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/cleat-team/cleat/plugin"
)

func init() {
	plugin.Register(plugin.PluginInfo{
		Name:        "email-notify",
		Version:     "0.1.0",
		Description: "Send transactional email via SendGrid",
		Author:      "cleat",
	}, func() plugin.Plugin {
		return &Plugin{}
	})
}

// New creates a new Plugin instance.
func New() plugin.Plugin {
	return &Plugin{}
}

// Plugin implements SendGrid email sending for workflows.
type Plugin struct {
	logger            *slog.Logger
	httpClient        *http.Client
	deploymentSecrets plugin.DeploymentSecrets
	defaultFrom       string
}

// Config holds optional configuration for the email plugin.
//
// SendGridAPIKey lived here until cleat#1992 part 1 moved it to a deployment
// secret ("email.sendgrid_api_key") so it can be rotated with
// `cleatctl set-deployment-secret` and take effect without a worker restart.
// See sendGridAPIKey (host_functions.go) for the per-call lookup that
// replaced the cached apiKey field this struct used to carry.
type Config struct {
	DefaultFrom string `json:"default_from,omitempty"`
}

// Info returns plugin metadata for discovery and documentation.
func (p *Plugin) Info() plugin.PluginInfo {
	return plugin.PluginInfo{
		Name:        "email-notify",
		Version:     "0.1.0",
		Description: "Send transactional email via SendGrid",
		Author:      "cleat",
	}
}

// Init initializes the plugin with the given environment.
//
// It no longer reads a SendGrid API key: that moved to a deployment secret
// (cleat#1992 part 1), fetched fresh on every call by sendGridAPIKey in
// host_functions.go rather than cached here. Enablement is still decided by
// config-section presence, exactly as before -- a worker with no "email"
// section in its plugin config never touches this plugin, and
// plugin.ErrNotConfigured is how it says so quietly rather than logging
// ERROR on every stock start. Whether the deployment secret itself is set is
// checked separately, at worker boot, by the fail-closed check setup.go
// runs over every enabled plugin's RequiredDeploymentSecrets.
func (p *Plugin) Init(ctx context.Context, env *plugin.Environment) error {
	if env.Logger != nil {
		p.logger = env.Logger
	} else {
		p.logger = slog.Default()
	}

	p.httpClient = &http.Client{
		// cleat#1565: every outbound request goes through the egress guard.
		// Nil in tests that build an Environment directly, which falls back to
		// the default transport -- TestEveryPluginRoutesItsEgressThroughTheGuard
		// is what keeps that from being how production works.
		Transport: env.HTTPTransport,
		Timeout:   30 * time.Second,
	}

	p.deploymentSecrets = env.DeploymentSecrets

	if len(env.Config) == 0 {
		// No config section at all -- most deployments never touch this
		// plugin. Disable it quietly rather than logging ERROR on every
		// stock worker start.
		return fmt.Errorf("email: %w", plugin.ErrNotConfigured)
	}
	var cfg Config
	if err := json.Unmarshal(env.Config, &cfg); err != nil {
		return fmt.Errorf("email: invalid config: %w", err)
	}
	p.defaultFrom = cfg.DefaultFrom

	p.logger.Info("email: initialized", "has_default_from", p.defaultFrom != "")
	return nil
}

// sendGridAPIKey fetches the current SendGrid API key. Called at the moment
// of use -- send, sendTemplate, checkStatus -- rather than cached, so a key
// rotated with `cleatctl set-deployment-secret` takes effect on the next
// call without a worker restart (plugin.DeploymentSecrets' own doc comment,
// "PER-USE, NOT PER-Init").
func (p *Plugin) sendGridAPIKey(ctx context.Context) (string, error) {
	if p.deploymentSecrets == nil {
		return "", fmt.Errorf("email: no deployment secret store configured")
	}
	key, err := p.deploymentSecrets.Get(ctx, "email.sendgrid_api_key")
	if err != nil {
		return "", fmt.Errorf("email: sendgrid_api_key: %w", err)
	}
	return key, nil
}

// RequiredDeploymentSecrets implements plugin.HasRequiredDeploymentSecrets.
// Unconditional: this is only consulted for a plugin Init already accepted
// (a config section is present), and every call this plugin serves needs the
// SendGrid key, so there is no enabled-but-key-optional case to encode here.
func (p *Plugin) RequiredDeploymentSecrets(config []byte) ([]string, error) {
	return []string{"email.sendgrid_api_key"}, nil
}
