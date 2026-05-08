package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/rcownie/cleat/internal/plugin"
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

// ProviderConfig holds configuration for a single LLM provider.
type ProviderConfig struct {
	APIKey       string `json:"api_key"`
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
db         plugin.DB
	logger     *slog.Logger
	httpClient *http.Client
	config     Config
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
	p.httpClient = &http.Client{Timeout: 60 * time.Second}

	if len(env.Config) > 0 {
		if err := json.Unmarshal(env.Config, &p.config); err != nil {
			return fmt.Errorf("llm: invalid config: %w", err)
		}
	}

	p.logger.Info("llm: initialized", "providers", len(p.config.Providers))
	return nil
}
