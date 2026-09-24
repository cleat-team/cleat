package llm

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
		Name:        "llm",
		Version:     "0.1.0",
		Description: "Unified LLM provider interface (OpenAI, Anthropic, Groq, Ollama)",
		Author:      "cleat",
	}, func() plugin.Plugin {
		return &Plugin{}
	})
}

// New creates a new Plugin instance.
func New() plugin.Plugin {
	return &Plugin{}
}

// ProviderConfig holds configuration for a single LLM provider.
//
// APIKey lived here until cleat#1992 part 1 moved it to a deployment secret
// ("llm.providers.<provider>.api_key") so it can be rotated with
// `cleatctl set-deployment-secret` and take effect without a worker restart.
// See (*Plugin).providerAPIKey (host_functions.go) for the per-call lookup
// that replaced this field. ollama needs no key at all -- it is excluded
// there rather than here, since this struct still describes every provider
// uniformly.
type ProviderConfig struct {
	BaseURL      string `json:"base_url,omitempty"`
	DefaultModel string `json:"default_model,omitempty"`
	Enabled      bool   `json:"enabled"`
}

// Config holds the full plugin configuration.
type Config struct {
	Providers map[string]ProviderConfig `json:"providers"`
}

// Plugin implements the LLM provider plugin.
type Plugin struct {
	db                plugin.PluginDB
	logger            *slog.Logger
	httpClient        *http.Client
	config            Config
	deploymentSecrets plugin.DeploymentSecrets
}

// Info returns plugin metadata.
func (p *Plugin) Info() plugin.PluginInfo {
	return plugin.PluginInfo{
		Name:        "llm",
		Version:     "0.1.0",
		Description: "Unified LLM provider interface (OpenAI, Anthropic, Groq, Ollama)",
		Author:      "cleat",
	}
}

// Init initializes the plugin.
func (p *Plugin) Init(ctx context.Context, env *plugin.Environment) error {
	if env.Logger != nil {
		p.logger = env.Logger
	} else {
		p.logger = slog.Default()
	}

	p.db = env.DB
	// cleat#1565: every outbound request goes through the egress guard.
	p.httpClient = &http.Client{Timeout: 60 * time.Second, Transport: env.HTTPTransport}
	p.deploymentSecrets = env.DeploymentSecrets

	if len(env.Config) > 0 {
		if err := json.Unmarshal(env.Config, &p.config); err != nil {
			return fmt.Errorf("llm: invalid config: %w", err)
		}
	}

	p.logger.Info("llm: initialized", "providers", len(p.config.Providers))
	return nil
}

// RequiredDeploymentSecrets implements plugin.HasRequiredDeploymentSecrets:
// one "llm.providers.<provider>.api_key" per ENABLED provider, excluding
// ollama, which providerAPIKey (host_functions.go) never looks up because
// OllamaChat/OllamaChatStream take no key at all.
func (p *Plugin) RequiredDeploymentSecrets(config []byte) ([]string, error) {
	var cfg Config
	if len(config) > 0 {
		if err := json.Unmarshal(config, &cfg); err != nil {
			return nil, fmt.Errorf("llm: invalid config: %w", err)
		}
	}
	var names []string
	for provider, pc := range cfg.Providers {
		if !pc.Enabled || provider == "ollama" {
			continue
		}
		names = append(names, "llm.providers."+provider+".api_key")
	}
	return names, nil
}
