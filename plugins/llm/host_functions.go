package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
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
	if err := scope.Register(plugin.FuncOptions{
		Name: "chat",
		// cleat#2043: api_key's raw value must be exactly a ${secret:NAME}
		// reference when present, never a literal -- a literal reaches
		// event_history otherwise (cleat#1988/#2023). Neither Idempotent nor
		// SameValueOnReplay is set, so this is not combined with a
		// re-invoke-on-replay policy; see FuncOptions.SecretOnlyFields.
		SecretOnlyFields: []string{"api_key"},
	}, p.chat); err != nil {
		return err
	}
	if streamScope, ok := scope.(plugin.StreamFuncRegistry); ok {
		if err := streamScope.RegisterStream(plugin.FuncOptions{
			Name:             "chat_stream",
			SecretOnlyFields: []string{"api_key"},
		}, p.chatStream); err != nil {
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

type listModelsRequest struct {
	Provider string `json:"provider"`
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
func (p *Plugin) effectiveAPIKey(ctx context.Context, requestKey, provider string) (string, error) {
	if requestKey == "" {
		return p.providerAPIKey(ctx, provider)
	}
	if strings.Contains(requestKey, "${secret:") {
		return "", fmt.Errorf("llm: api_key contains an unresolved secret reference " +
			"(no tenant context, or no master key configured on the worker)")
	}
	return requestKey, nil
}

// providerAPIKey fetches the current API key for one provider from
// deployment secrets, at the moment of use rather than cached at Init
// (cleat#1992 part 1), so a key rotated with `cleatctl set-deployment-secret`
// takes effect on the next call without a worker restart. ollama needs no
// key at all -- OllamaChat/OllamaChatStream take none -- so it is excluded
// here rather than made to look up a secret that will never be set. A
// provider explicitly marked "requires_deployment_key": false is treated the
// same way: a keyless self-hosted base_url has nothing to look up, and a
// BYOK-only provider is reached through effectiveAPIKey's req.APIKey branch
// before providerAPIKey is ever called for it in the first place -- this
// only matters for the caller that supplies no request-level key at all.
func (p *Plugin) providerAPIKey(ctx context.Context, provider string) (string, error) {
	if provider == "ollama" || !p.config.Providers[provider].requiresDeploymentKey() {
		return "", nil
	}
	if p.deploymentSecrets == nil {
		return "", fmt.Errorf("llm: no deployment secret store configured")
	}
	key, err := p.deploymentSecrets.Get(ctx, "llm.providers."+provider+".api_key")
	if err != nil {
		return "", fmt.Errorf("llm: providers.%s.api_key: %w", provider, err)
	}
	return key, nil
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

	apiKey, err := p.effectiveAPIKey(ctx, req.APIKey, req.Provider)
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
	if output.EstimatedCost && p.logger != nil {
		// A field nothing reads is not a signal (cleat#2572) -- log it so an
		// operator running a model cleat has no rate for learns that from
		// the run, not just from a JSON field a workflow may never inspect.
		// p.logger is nil for a Plugin built without Init (every test in
		// this package does), same as the unguarded slog.Default() fallback
		// Init itself applies -- guarded here rather than defaulted, since
		// this package has no constructor test code is required to use.
		p.logger.Warn("llm: priced by fallback rate, not an exact one",
			"provider", req.Provider, "model", output.Model, "cost", output.Cost)
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

	apiKey, err := p.providerAPIKey(ctx, req.Provider)
	if err != nil {
		return "", err
	}

	input := providers.EmbedInput{Model: req.Model, Input: req.Input}

	var output providers.EmbedOutput

	switch req.Provider {
	case "openai":
		output, err = providers.OpenAIEmbed(ctx, p.httpClient, apiKey, cfg.BaseURL, input)
	default:
		// Try OpenAI-compatible path for other providers
		output, err = providers.OpenAIEmbed(ctx, p.httpClient, apiKey, cfg.BaseURL, input)
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
	var req listModelsRequest
	if err := json.Unmarshal([]byte(inputJSON), &req); err != nil {
		return "", fmt.Errorf("llm: invalid input: %w", err)
	}

	type modelInfo struct {
		Name   string  `json:"name"`
		Cost1K float64 `json:"cost_per_1k_tokens"`
	}

	models := map[string][]modelInfo{
		// ollama is deliberately not derived from providers.prices: it is not
		// a priced table at all, every model is free, and it carries no
		// entry there for that reason (see providers/pricing.go).
		"ollama": {
			{"llama3.2", 0},
			{"mistral", 0},
			{"codellama", 0},
		},
	}
	for _, provider := range []string{"openai", "anthropic", "groq", "gemini", "mistral"} {
		rates := providers.ProviderModels(provider)
		names := make([]string, 0, len(rates))
		for name := range rates {
			names = append(names, name)
		}
		sort.Strings(names)
		list := make([]modelInfo, 0, len(names))
		for _, name := range names {
			rate := rates[name]
			// Cost1K blends prompt and completion into one $/1000-token
			// figure for display -- cleat#2572: before this it was a
			// separately hand-maintained number that only agreed with the
			// split prompt/completion rate providers actually bill at (see
			// CostFor) at a 1:1 prompt:completion ratio, which a real call
			// never has. It is now derived from that same rate, so the two
			// cannot drift, but it is still only an approximation of what a
			// specific call will cost.
			list = append(list, modelInfo{
				Name:   name,
				Cost1K: (rate.PromptPerMillion + rate.CompletionPerMillion) / 2 / 1000,
			})
		}
		models[provider] = list
	}
	// text-embedding-3-small is priced by OpenAIEmbed, not the chat table
	// CostFor draws on, so it is listed here rather than in
	// providers/pricing.go.
	models["openai"] = append(models["openai"], modelInfo{Name: "text-embedding-3-small", Cost1K: 0.00002})

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

	apiKey, err := p.effectiveAPIKey(ctx, req.APIKey, req.Provider)
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
