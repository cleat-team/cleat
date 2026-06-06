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

	"github.com/sendgrid/sendgrid-go"

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
	logger      *slog.Logger
	httpClient  *http.Client
	client      *sendgrid.Client
	apiKey      string
	defaultFrom string
}

// Config holds optional configuration for the email plugin.
type Config struct {
	SendGridAPIKey string `json:"sendgrid_api_key"`
	DefaultFrom    string `json:"default_from,omitempty"`
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

// Init initializes the plugin with the given environment. It reads the
// SendGrid API key from the plugin config and creates a SendGrid client.
func (p *Plugin) Init(ctx context.Context, env *plugin.Environment) error {
	if env.Logger != nil {
		p.logger = env.Logger
	} else {
		p.logger = slog.Default()
	}

	p.httpClient = &http.Client{
		Timeout: 30 * time.Second,
	}

	// Parse optional config.
	if len(env.Config) > 0 {
		var cfg Config
		if err := json.Unmarshal(env.Config, &cfg); err != nil {
			return fmt.Errorf("email: invalid config: %w", err)
		}
		p.apiKey = cfg.SendGridAPIKey
		p.defaultFrom = cfg.DefaultFrom
	}

	if p.apiKey == "" {
		return fmt.Errorf("email: sendgrid_api_key is required in plugin config")
	}

	p.client = sendgrid.NewSendClient(p.apiKey)

	p.logger.Info("email: initialized", "has_default_from", p.defaultFrom != "")
	return nil
}
