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

	// RequiresDeploymentKey is a pointer so an OMITTED field defaults to
	// true (a required key, today's behaviour for every enabled provider
	// except ollama) and an EXPLICIT `"requires_deployment_key": false`
	// opts out -- a plain bool cannot carry that distinction, since its zero
	// value and "explicitly false" are the same value.
	//
	// Without this, a keyless self-hosted base_url (vLLM, LM Studio) or a
	// BYOK-only deployment (cleat#1988, where req.APIKey wins over anything
	// looked up here) could never boot: RequiredDeploymentSecrets required
	// llm.providers.<provider>.api_key for every enabled non-ollama
	// provider unconditionally, so enabling one without ALSO writing a
	// deployment secret it will never use refused the worker at startup.
	// Found in cleat-review's #2202 pass.
	RequiresDeploymentKey *bool `json:"requires_deployment_key,omitempty"`
}

// requiresDeploymentKey is the read side of ProviderConfig.RequiresDeploymentKey's
// nil-means-true default -- see that field's doc comment.
func (pc ProviderConfig) requiresDeploymentKey() bool {
	return pc.RequiresDeploymentKey == nil || *pc.RequiresDeploymentKey
}

// Config holds the full plugin configuration.
type Config struct {
	Providers map[string]ProviderConfig `json:"providers"`
}

// legacyProviderConfig catches api_key left over in --plugin-config from
// before cleat#1992 part 1 moved it to a deployment secret. Same reasoning
// as email's legacyEmailConfig: json.Unmarshal silently drops a field
// ProviderConfig no longer declares, so a leftover key here does nothing and
// says nothing unless something goes looking for it on purpose.
//
// plugin.Secret, not string, for the same reason as legacyEmailConfig:
// TestPluginCredentialFieldsUseTheSecretType is name-driven and flags any
// credential-shaped field held as a plain string. Found in cleat-review's
// #2202 re-check.
type legacyProviderConfig struct {
	APIKey plugin.Secret `json:"api_key"`
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
		var legacy struct {
			Providers map[string]legacyProviderConfig `json:"providers"`
		}
		if err := json.Unmarshal(env.Config, &legacy); err == nil {
			for name, pc := range legacy.Providers {
				if pc.APIKey != "" {
					p.logger.Warn("llm: providers." + name + ".api_key in --plugin-config is no longer read " +
						"(cleat#1992 part 1); it has no effect. Use " +
						"`cleatctl set-deployment-secret --name llm.providers." + name + ".api_key` instead.")
				}
			}
		}
	}

	p.logger.Info("llm: initialized", "providers", len(p.config.Providers))
	return nil
}

// RequiredDeploymentSecrets implements plugin.HasRequiredDeploymentSecrets:
// one "llm.providers.<provider>.api_key" per ENABLED provider that requires
// one, excluding ollama (providerAPIKey never looks one up for it -- neither
// OllamaChat nor OllamaChatStream take a key at all) and excluding any
// provider explicitly marked "requires_deployment_key": false -- a keyless
// self-hosted base_url or a BYOK-only provider (see ProviderConfig.RequiresDeploymentKey's
// doc comment).
func (p *Plugin) RequiredDeploymentSecrets(config []byte) ([]string, error) {
	var cfg Config
	if len(config) > 0 {
		if err := json.Unmarshal(config, &cfg); err != nil {
			return nil, fmt.Errorf("llm: invalid config: %w", err)
		}
	}
	var names []string
	for provider, pc := range cfg.Providers {
		if !pc.Enabled || provider == "ollama" || !pc.requiresDeploymentKey() {
			continue
		}
		names = append(names, "llm.providers."+provider+".api_key")
	}
	return names, nil
}

// DeploymentSecretPrefix implements plugin.HasDeploymentSecretPrefix: every
// name llm ever reads is "llm.providers.<provider>.api_key".
func (p *Plugin) DeploymentSecretPrefix() string {
	return "llm.providers."
}
