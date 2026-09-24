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

	signalWorkflow     func(ctx context.Context, workflowID, signalName, payload string) error
	slackSigningSecret string
}

// Config holds optional configuration for the slack-notify plugin.
type Config struct {
	SlackSigningSecret string `json:"slack_signing_secret,omitempty"`
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
	}

	p.signalWorkflow = env.SignalWorkflow
	p.slackSigningSecret = p.config.SlackSigningSecret

	if p.slackSigningSecret == "" {
		// interactive.go only verifies the Slack signature when this is set --
		// existing behaviour, not new here. Flagged at WARN rather than left
		// silent so an operator who forgot the secret can see it at boot.
		//
		// States the PRESENT truth, not only the future one: an earlier
		// version of this wording said only what cleat#2172 will do, which a
		// reader could take as "nothing is wrong yet". What is true right
		// now is the worse half -- /slack/interactive accepts every request
		// unsigned, verifying nothing -- and cleat#2172 (owner decision,
		// option A) is what changes that to a refusal. Found in
		// cleat-review's #2202 pass.
		p.logger.Warn("slack-notify: no signing secret configured -- /slack/interactive currently ACCEPTS unsigned requests; cleat#2172 will make it refuse them")
	}

	p.logger.Info("slack-notify: initialized")
	return nil
}
