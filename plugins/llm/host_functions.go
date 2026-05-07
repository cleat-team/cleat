package llm

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/rcownie/durable/internal/plugin"
	"github.com/rcownie/durable/plugins/llm/providers"
)

// RegisterHostFunctions registers workflow-callable functions.
func (p *Plugin) RegisterHostFunctions(scope plugin.FuncRegistry) error {
	if scope == nil {
		return fmt.Errorf("llm: nil function registry")
	}
	if err := scope.Register(plugin.FuncOptions{Name: "chat"}, p.chat); err != nil {
		return err
	}
	if err := scope.Register(plugin.FuncOptions{Name: "embed", Idempotent: true}, p.embed); err != nil {
		return err
	}
	if err := scope.Register(plugin.FuncOptions{Name: "list_models", Idempotent: true}, p.listModels); err != nil {
		return err
	}
	return nil
}

type chatRequest struct {
	Provider    string             `json:"provider"`
	Model       string             `json:"model"`
	Messages    []providers.Message `json:"messages"`
	Temperature float64            `json:"temperature,omitempty"`
	MaxTokens   int                `json:"max_tokens,omitempty"`
	Tools       []providers.Tool   `json:"tools,omitempty"`
	ToolChoice  string             `json:"tool_choice,omitempty"`
	System      string             `json:"system,omitempty"`
}

type embedRequest struct {
	Provider string   `json:"provider"`
	Model    string   `json:"model"`
	Input    []string `json:"input"`
}

func (p *Plugin) chat(ctx context.Context, inputJSON string) (string, error) {
	cc := plugin.CallContextFromContext(ctx)
	if cc == nil || cc.TenantID == uuid.Nil {
		return "", fmt.Errorf("llm: no tenant context")
	}

	var req chatRequest
	if err := json.Unmarshal([]byte(inputJSON), &req); err != nil {
		return "", fmt.Errorf("llm: invalid input: %w", err)
	}
	if req.Provider == "" {
		return "", fmt.Errorf("llm: provider is required")
	}

	cfg, ok := p.config.Providers[req.Provider]
	if !ok || !cfg.Enabled {
		return "", fmt.Errorf("llm: provider %q not configured or disabled", req.Provider)
	}

	if req.Model == "" {
		req.Model = cfg.DefaultModel
	}

	input := providers.ChatInput{
		Model:       req.Model,
		Messages:    req.Messages,
		Temperature: req.Temperature,
		MaxTokens:   req.MaxTokens,
		Tools:       req.Tools,
		ToolChoice:  req.ToolChoice,
		System:      req.System,
	}

	var output providers.ChatOutput
	var err error

	switch req.Provider {
	case "openai":
		output, err = providers.OpenAIChat(ctx, p.httpClient, cfg.APIKey, cfg.BaseURL, input)
	case "anthropic":
		output, err = providers.AnthropicChat(ctx, p.httpClient, cfg.APIKey, cfg.BaseURL, input)
	case "groq":
		output, err = providers.GroqChat(ctx, p.httpClient, cfg.APIKey, cfg.BaseURL, input)
	case "ollama":
		output, err = providers.OllamaChat(ctx, p.httpClient, cfg.BaseURL, input)
	default:
		return "", fmt.Errorf("llm: unknown provider: %s", req.Provider)
	}
	if err != nil {
		output.Error = err.Error()
	}

	outJSON, err := json.Marshal(output)
	if err != nil {
		return "", fmt.Errorf("llm: marshal output: %w", err)
	}
	return string(outJSON), nil
}

func (p *Plugin) embed(ctx context.Context, inputJSON string) (string, error) {
	cc := plugin.CallContextFromContext(ctx)
	if cc == nil || cc.TenantID == uuid.Nil {
		return "", fmt.Errorf("llm: no tenant context")
	}

	var req embedRequest
	if err := json.Unmarshal([]byte(inputJSON), &req); err != nil {
		return "", fmt.Errorf("llm: invalid embed input: %w", err)
	}
	if req.Provider == "" {
		return "", fmt.Errorf("llm: provider is required")
	}

	cfg, ok := p.config.Providers[req.Provider]
	if !ok || !cfg.Enabled {
		return "", fmt.Errorf("llm: provider %q not configured or disabled", req.Provider)
	}

	if req.Model == "" {
		req.Model = cfg.DefaultModel
	}

	input := providers.EmbedInput{Model: req.Model, Input: req.Input}

	var output providers.EmbedOutput
	var err error

	switch req.Provider {
	case "openai":
		output, err = providers.OpenAIEmbed(ctx, p.httpClient, cfg.APIKey, cfg.BaseURL, input)
	default:
		// Try OpenAI-compatible path for other providers
		output, err = providers.OpenAIEmbed(ctx, p.httpClient, cfg.APIKey, cfg.BaseURL, input)
	}
	if err != nil {
		output.Error = err.Error()
	}

	outJSON, err := json.Marshal(output)
	if err != nil {
		return "", fmt.Errorf("llm: marshal embed output: %w", err)
	}
	return string(outJSON), nil
}

func (p *Plugin) listModels(ctx context.Context, inputJSON string) (string, error) {
	var req struct {
		Provider string `json:"provider"`
	}
	if err := json.Unmarshal([]byte(inputJSON), &req); err != nil {
		return "", fmt.Errorf("llm: invalid input: %w", err)
	}

	type modelInfo struct {
		Name   string  `json:"name"`
		Cost1K float64 `json:"cost_per_1k_tokens"`
	}

	models := map[string][]modelInfo{
		"openai": {
			{"gpt-4o", 0.0125},
			{"gpt-4o-mini", 0.00075},
			{"gpt-4-turbo", 0.040},
			{"text-embedding-3-small", 0.00002},
		},
		"anthropic": {
			{"claude-opus-4-7", 0.090},
			{"claude-sonnet-4-6", 0.018},
			{"claude-haiku-4-5", 0.0048},
		},
		"groq": {
			{"llama-3.3-70b", 0.001},
			{"mixtral-8x7b", 0.0005},
		},
		"ollama": {
			{"llama3.2", 0},
			{"mistral", 0},
			{"codellama", 0},
		},
	}

	if req.Provider != "" {
		result, ok := models[req.Provider]
		if !ok {
			result = []modelInfo{}
		}
		outJSON, _ := json.Marshal(map[string]any{"models": result, "provider": req.Provider})
		return string(outJSON), nil
	}

	all := map[string][]modelInfo{}
	for _, p := range []string{"openai", "anthropic", "groq", "ollama"} {
		if cfg, ok := p.config.Providers[p]; ok && cfg.Enabled {
			all[p] = models[p]
		}
	}
	outJSON, _ := json.Marshal(map[string]any{"providers": all})
	return string(outJSON), nil
}
