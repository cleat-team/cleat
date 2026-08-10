package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cleat-team/cleat/plugin"
	"github.com/cleat-team/cleat/plugins/llm/providers"
	"github.com/google/uuid"
)

// RegisterHostFunctions registers workflow-callable functions.
func (p *Plugin) RegisterHostFunctions(scope plugin.FuncRegistry) error {
	if scope == nil {
		return fmt.Errorf("llm: nil function registry")
	}
	if err := scope.Register(plugin.FuncOptions{Name: "chat"}, p.chat); err != nil {
		return err
	}
	if streamScope, ok := scope.(plugin.StreamFuncRegistry); ok {
		if err := streamScope.RegisterStream(plugin.FuncOptions{Name: "chat_stream"}, p.chatStream); err != nil {
			return err
		}
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
	Provider    string              `json:"provider"`
	Model       string              `json:"model"`
	Messages    []providers.Message `json:"messages"`
	Temperature float64             `json:"temperature,omitempty"`
	MaxTokens   int                 `json:"max_tokens,omitempty"`
	Tools       []providers.Tool    `json:"tools,omitempty"`
	ToolChoice  string              `json:"tool_choice,omitempty"`
	System      string              `json:"system,omitempty"`
}

type embedRequest struct {
	Provider string   `json:"provider"`
	Model    string   `json:"model"`
	Input    []string `json:"input"`
}

// normalizeOutput ensures consistent ChatOutput structure across all providers.
func normalizeOutput(out *providers.ChatOutput) {
	for i := range out.Choices {
		ch := &out.Choices[i]
		ch.FinishReason = strings.ToLower(ch.FinishReason)
		switch ch.FinishReason {
		case "tool_calls", "stop", "length", "content_filter":
		case "":
			ch.FinishReason = "stop"
		default:
			ch.FinishReason = "stop"
		}
		for j := range ch.Message.ToolCalls {
			tc := &ch.Message.ToolCalls[j]
			if tc.ID == "" {
				tc.ID = "call_" + strings.ReplaceAll(uuid.New().String(), "-", "")[:24]
			}
		}
	}
	if out.Choices == nil {
		out.Choices = []providers.Choice{}
	}
}

func (p *Plugin) chat(ctx context.Context, inputJSON string) (string, error) {
	cc := plugin.CallContextFromContext(ctx)
	if cc == nil || cc.TenantID == "" {
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
	case "gemini":
		output, err = providers.GeminiChat(ctx, p.httpClient, cfg.APIKey, cfg.BaseURL, input)
	case "mistral":
		output, err = providers.MistralChat(ctx, p.httpClient, cfg.APIKey, cfg.BaseURL, input)
	default:
		return "", fmt.Errorf("llm: unknown provider: %s", req.Provider)
	}
	if err != nil {
		output.Error = err.Error()
	}

	normalizeOutput(&output)

	outJSON, err := json.Marshal(output)
	if err != nil {
		return "", fmt.Errorf("llm: marshal output: %w", err)
	}
	return string(outJSON), nil
}

func (p *Plugin) embed(ctx context.Context, inputJSON string) (string, error) {
	cc := plugin.CallContextFromContext(ctx)
	if cc == nil || cc.TenantID == "" {
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
		"gemini": {
			{"gemini-2.5-flash", 0.00075},
			{"gemini-2.5-pro", 0.00625},
			{"gemini-2.0-flash", 0.00050},
			{"gemini-2.0-flash-lite", 0.000375},
		},
		"mistral": {
			{"mistral-large-latest", 0.008},
			{"mistral-medium-latest", 0.005},
			{"mistral-small-latest", 0.004},
			{"open-mistral-nemo", 0.0006},
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
	for _, provider := range []string{"openai", "anthropic", "groq", "ollama", "gemini", "mistral"} {
		if cfg, ok := p.config.Providers[provider]; ok && cfg.Enabled {
			all[provider] = models[provider]
		}
	}
	outJSON, _ := json.Marshal(map[string]any{"providers": all})
	return string(outJSON), nil
}

func (p *Plugin) chatStream(ctx context.Context, inputJSON string) (<-chan plugin.StreamEvent, error) {
	cc := plugin.CallContextFromContext(ctx)
	if cc == nil || cc.TenantID == "" {
		return nil, fmt.Errorf("llm: no tenant context")
	}

	var req chatRequest
	if err := json.Unmarshal([]byte(inputJSON), &req); err != nil {
		return nil, fmt.Errorf("llm: invalid input: %w", err)
	}
	if req.Provider == "" {
		return nil, fmt.Errorf("llm: provider is required")
	}

	cfg, ok := p.config.Providers[req.Provider]
	if !ok || !cfg.Enabled {
		return nil, fmt.Errorf("llm: provider %q not configured or disabled", req.Provider)
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

	var chunkCh <-chan providers.StreamChunk
	var err error

	switch req.Provider {
	case "openai":
		chunkCh, err = providers.OpenAIChatStream(ctx, p.httpClient, cfg.APIKey, cfg.BaseURL, input)
	case "anthropic":
		chunkCh, err = providers.AnthropicChatStream(ctx, p.httpClient, cfg.APIKey, cfg.BaseURL, input)
	case "groq":
		chunkCh, err = providers.GroqChatStream(ctx, p.httpClient, cfg.APIKey, cfg.BaseURL, input)
	case "ollama":
		chunkCh, err = providers.OllamaChatStream(ctx, p.httpClient, cfg.BaseURL, input)
	default:
		return nil, fmt.Errorf("llm: unknown provider: %s", req.Provider)
	}
	if err != nil {
		return nil, err
	}

	out := make(chan plugin.StreamEvent)
	go func() {
		defer close(out)
		for chunk := range chunkCh {
			out <- plugin.StreamEvent{
				Index:   chunk.Index,
				Content: chunk.Content,
				Finish:  chunk.Done,
			}
		}
	}()

	return out, nil
}
