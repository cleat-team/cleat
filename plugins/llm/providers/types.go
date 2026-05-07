// Package providers defines shared types for LLM provider implementations.
package providers

// ChatInput is the JSON input for a chat completion request.
type ChatInput struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Temperature float64   `json:"temperature,omitempty"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
	Tools       []Tool    `json:"tools,omitempty"`
	ToolChoice  string    `json:"tool_choice,omitempty"`
	System      string    `json:"system,omitempty"`
}

// Message represents a conversation message.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

// Tool defines a function tool available to the model.
type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

// ToolFunction describes a callable function.
type ToolFunction struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Parameters  any    `json:"parameters"`
}

// ToolCall represents a model's request to call a tool.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

// FunctionCall contains the name and arguments for a tool invocation.
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ChatOutput is the JSON output from a chat completion.
type ChatOutput struct {
	Choices []Choice `json:"choices"`
	Usage   Usage    `json:"usage"`
	Cost    float64  `json:"cost"`
	Model   string   `json:"model"`
	Error   string   `json:"error,omitempty"`
}

// Choice represents a single completion choice.
type Choice struct {
	Message      ChoiceMessage `json:"message"`
	FinishReason string        `json:"finish_reason"`
}

// ChoiceMessage is the message within a choice.
type ChoiceMessage struct {
	Role      string     `json:"role"`
	Content   string     `json:"content"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
}

// Usage tracks token consumption.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// StreamChunk represents a single chunk of content from a streaming LLM response.
type StreamChunk struct {
	Content string `json:"content"`
	Index   int    `json:"index"`
	Done    bool   `json:"done"`
}

// EmbedInput is the JSON input for an embedding request.
type EmbedInput struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

// EmbedOutput is the JSON output from an embedding request.
type EmbedOutput struct {
	Data  []EmbeddingData `json:"data"`
	Usage Usage           `json:"usage"`
	Cost  float64         `json:"cost"`
	Error string          `json:"error,omitempty"`
}

// EmbeddingData holds a single embedding vector.
type EmbeddingData struct {
	Embedding []float64 `json:"embedding"`
	Index     int       `json:"index"`
}
