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
	if err := scope.Register(plugin.FuncOptions{
		Name: "embed",
		// Near-deterministic for a fixed model and input, which is the property
		// replay needs rather than mere absence of side effects.
		//
		// CAVEAT: "near". A hosted model can change behind a stable name, and
		// re-invoking costs money -- a real cost, though not a workflow side
		// effect. Both halves are asserted here rather than derived from the
		// code, which is what makes this the second entry to re-examine.
		Idempotent:        true,
		SameValueOnReplay: true,
	}, p.embed); err != nil {
		return err
	}
	if err := scope.Register(plugin.FuncOptions{
		Name: "list_models",
		// Idempotent -- listing has no effect. NOT stable: a provider's model
		// list is not constant over a workflow's lifetime, so a replay can
		// return a set the workflow never branched on. cleat#1318.
		Idempotent:        true,
		SameValueOnReplay: false,
	}, p.listModels); err != nil {
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
	// APIKey overrides the configured provider key for this call (cleat#1988).
	// See effectiveAPIKey for what this field does and does not guarantee.
	APIKey string `json:"api_key,omitempty"`
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

// effectiveAPIKey returns the key to use for one call: the request's own
// (cleat#1988) when the workflow supplied one, falling back to the operator's
// configured key otherwise.
//
// The only case refused is a value that still contains the literal
// "${secret:" text. withSecrets and withSecretsStream
// (cmd/cleat-worker/setup.go) pass a call through UNCHANGED when there is no
// tenant in context or no master key configured -- the one case that boundary
// lets escape as a string rather than an error -- so that text reaching here
// means resolution was never attempted. Sending it to a provider as a
// credential is never correct; a rejected reference is at least legible,
// where the alternative is an opaque 401 from whichever provider was asked to
// authenticate with the literal string "${secret:openai}".
//
// WHAT THIS DOES NOT DO, AND CANNOT: tell a genuinely resolved secret apart
// from a key a workflow author typed directly into the call argument. Both
// arrive here as the same plain string. engine.ResolveSecretRefs
// (engine/tenant_secrets.go) is a whole-document text substitution with no
// per-field record of what it touched -- grep the tree for callers of it and
// there are exactly three, none of which keep one -- so there is no
// information available at this boundary to distinguish the two. The
// property this system actually has is the one engine/tenant_secrets.go's
// SecretStore doc comment already states: a workflow author writing
// ${secret:NAME} keeps the reference in event history rather than the value,
// which is a fact about what THEY write, not something enforced against a
// workflow that chooses not to.
func effectiveAPIKey(requestKey string, cfg ProviderConfig) (string, error) {
	if requestKey == "" {
		return cfg.APIKey, nil
	}
	if strings.Contains(requestKey, "${secret:") {
		return "", fmt.Errorf("llm: api_key contains an unresolved secret reference " +
			"(no tenant context, or no master key configured on the worker)")
	}
	return requestKey, nil
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

	apiKey, err := effectiveAPIKey(req.APIKey, cfg)
	if err != nil {
		return "", err
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

	switch req.Provider {
	case "openai":
		output, err = providers.OpenAIChat(ctx, p.httpClient, apiKey, cfg.BaseURL, input)
	case "anthropic":
		output, err = providers.AnthropicChat(ctx, p.httpClient, apiKey, cfg.BaseURL, input)
	case "groq":
		output, err = providers.GroqChat(ctx, p.httpClient, apiKey, cfg.BaseURL, input)
	case "ollama":
		output, err = providers.OllamaChat(ctx, p.httpClient, cfg.BaseURL, input)
	case "gemini":
		output, err = providers.GeminiChat(ctx, p.httpClient, apiKey, cfg.BaseURL, input)
	case "mistral":
		output, err = providers.MistralChat(ctx, p.httpClient, apiKey, cfg.BaseURL, input)
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

	apiKey, err := effectiveAPIKey(req.APIKey, cfg)
	if err != nil {
		return nil, err
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

	switch req.Provider {
	case "openai":
		chunkCh, err = providers.OpenAIChatStream(ctx, p.httpClient, apiKey, cfg.BaseURL, input)
	case "anthropic":
		chunkCh, err = providers.AnthropicChatStream(ctx, p.httpClient, apiKey, cfg.BaseURL, input)
	case "groq":
		chunkCh, err = providers.GroqChatStream(ctx, p.httpClient, apiKey, cfg.BaseURL, input)
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
		plugin.RecoverGoroutine("llm", nil, func() {
			for chunk := range chunkCh {
				out <- plugin.StreamEvent{
					Index:   chunk.Index,
					Content: chunk.Content,
					Finish:  chunk.Done,
				}
			}
		})
	}()

	return out, nil
}
