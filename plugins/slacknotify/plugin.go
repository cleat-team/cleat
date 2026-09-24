// Package slacknotify provides Slack webhook integration for workflows.
// It lets workflows send Slack messages via incoming webhooks, and exposes
// HTTP CRUD routes for managing Slack configs (tenant-scoped).
package slacknotify

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
		Name:        "slack-notify",
		Version:     "0.1.0",
		Description: "Send Slack messages from workflows",
		Author:      "cleat",
	}, func() plugin.Plugin {
		return &Plugin{}
	})
}

// New creates a new Plugin instance.
func New() plugin.Plugin {
	return &Plugin{}
}

// Plugin implements Slack webhook message sending for workflows.
type Plugin struct {
	db         plugin.PluginDB
	logger     *slog.Logger
	httpClient *http.Client
	config     Config
	dialect    plugin.Dialect

	signalWorkflow    func(ctx context.Context, workflowID, signalName, payload string) error
	deploymentSecrets plugin.DeploymentSecrets
}

// Config holds optional configuration for the slack-notify plugin.
//
// SlackSigningSecret lived here until cleat#2172 moved it to a deployment
// secret ("slacknotify.signing_secret", cleat#1992 part 1) so it can be
// rotated with `cleatctl set-deployment-secret` and take effect without a
// worker restart. See handleInteractiveCallback (interactive.go) for the
// per-request lookup that replaced the cached slackSigningSecret field this
// struct used to carry.
type Config struct{}

// legacySlackConfig catches slack_signing_secret left over in
// --plugin-config from before cleat#2172. json.Unmarshal ignores fields a
// target struct does not declare, so once Config dropped the field a
// leftover value there silently stopped doing anything -- no error, no log,
// just quietly wrong. This is unmarshaled from the same bytes purely to
// detect that and WARN; Config above no longer has anywhere to put the
// value even if this found one.
//
// plugin.Secret, not string: TestPluginCredentialFieldsUseTheSecretType
// flags any credential-shaped field held as a plain string. This field
// never round-trips through a handler -- it is read once, at Init, purely
// to decide whether to WARN -- but the guard is name-driven rather than
// reachability-driven, and Secret costs nothing here.
type legacySlackConfig struct {
	SlackSigningSecret plugin.Secret `json:"slack_signing_secret"`
}

// Info returns plugin metadata for discovery and documentation.
func (p *Plugin) Info() plugin.PluginInfo {
	return plugin.PluginInfo{
		Name:        "slack-notify",
		Version:     "0.1.0",
		Description: "Send Slack messages from workflows",
		Author:      "cleat",
	}
}

// Init initializes the plugin with the given environment. It creates an HTTP
// client with a 10-second timeout for Slack webhook requests.
func (p *Plugin) Init(ctx context.Context, env *plugin.Environment) error {
	if env.Logger != nil {
		p.logger = env.Logger
	} else {
		p.logger = slog.Default()
	}

	p.db = env.DB
	p.dialect = env.Dialect
	p.httpClient = &http.Client{
		// cleat#1565: every outbound request goes through the egress guard.
		// Nil in tests that build an Environment directly, which falls back to
		// the default transport -- TestEveryPluginRoutesItsEgressThroughTheGuard
		// is what keeps that from being how production works.
		Transport: env.HTTPTransport,
		Timeout:   10 * time.Second,
	}

	// Parse optional config.
	if len(env.Config) > 0 {
		if err := json.Unmarshal(env.Config, &p.config); err != nil {
			return fmt.Errorf("slack-notify: invalid config: %w", err)
		}
		var legacy legacySlackConfig
		if err := json.Unmarshal(env.Config, &legacy); err == nil && legacy.SlackSigningSecret != "" {
			p.logger.Warn("slack-notify: slack_signing_secret in --plugin-config is no longer read " +
				"(cleat#2172); it has no effect. Use " +
				"`cleatctl set-deployment-secret --name slacknotify.signing_secret` instead.")
		}
	}

	p.signalWorkflow = env.SignalWorkflow
	p.deploymentSecrets = env.DeploymentSecrets

	p.logger.Info("slack-notify: initialized")
	return nil
}

// DeploymentSecretPrefix implements plugin.HasDeploymentSecretPrefix:
// slack-notify only ever reads "slacknotify.signing_secret".
func (p *Plugin) DeploymentSecretPrefix() string {
	return "slacknotify."
}
