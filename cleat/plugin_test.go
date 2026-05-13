package cleat

import (
	"errors"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// CallTyped
// ---------------------------------------------------------------------------

func TestCallTypedSuccess(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{
		DurableCall: func(service, operation, requestJSON string) (string, error) {
			if service != "my_plugin" || operation != "my_op" {
				t.Errorf("unexpected service/operation: %s.%s", service, operation)
			}
			if requestJSON != `{"key":"hello","val":42}` {
				t.Errorf("unexpected request JSON: %s", requestJSON)
			}
			return `{"result":"ok","count":7}`, nil
		},
	})

	type MyRequest struct {
		Key string `json:"key"`
		Val int    `json:"val"`
	}
	type MyResponse struct {
		Result string `json:"result"`
		Count  int    `json:"count"`
	}

	resp, err := CallTyped[MyResponse](h, "my_plugin", "my_op", MyRequest{Key: "hello", Val: 42})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Result != "ok" {
		t.Errorf("expected result 'ok', got %q", resp.Result)
	}
	if resp.Count != 7 {
		t.Errorf("expected count 7, got %d", resp.Count)
	}
}

func TestCallTypedMarshalingError(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{})
	// An unmarshalable channel type should produce a marshaling error.
	_, err := CallTyped[string](h, "svc", "op", make(chan int))
	if err == nil {
		t.Fatal("expected marshaling error, got nil")
	}
	if !strings.Contains(err.Error(), "marshaling") {
		t.Errorf("expected 'marshaling' in error, got: %v", err)
	}
}

func TestCallTypedPropagatesCallError(t *testing.T) {
	callErr := errors.New("service unavailable")
	h := NewHostCalls(HostCallsOptions{
		DurableCall: func(_, _, _ string) (string, error) {
			return "", callErr
		},
	})

	_, err := CallTyped[string](h, "svc", "op", map[string]string{"k": "v"})
	if err != callErr {
		t.Errorf("expected original error %v, got %v", callErr, err)
	}
}

func TestCallTypedUnmarshalError(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{
		DurableCall: func(_, _, _ string) (string, error) {
			return "{bad json}", nil
		},
	})

	type MyResponse struct {
		Result string `json:"result"`
	}
	_, err := CallTyped[MyResponse](h, "svc", "op", map[string]string{"k": "v"})
	if err == nil {
		t.Fatal("expected unmarshaling error, got nil")
	}
	if !strings.Contains(err.Error(), "unmarshaling") {
		t.Errorf("expected 'unmarshaling' in error, got: %v", err)
	}
}

func TestCallTypedDurableCallNotInitialized(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{})
	_, err := CallTyped[string](h, "svc", "op", map[string]string{})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "not initialized") {
		t.Errorf("expected 'not initialized' error, got: %v", err)
	}
}

func TestCallTypedEmptyResponse(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{
		DurableCall: func(_, _, _ string) (string, error) {
			return ``, nil
		},
	})

	// Empty string should unmarshal into the zero value of the target type.
	resp, err := CallTyped[string](h, "svc", "op", map[string]string{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp != "" {
		t.Errorf("expected empty string, got %q", resp)
	}
}

// ---------------------------------------------------------------------------
// CallTypedEphemeral
// ---------------------------------------------------------------------------

func TestCallTypedEphemeralSuccess(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{
		PluginCall: func(plugin, function, inputJSON string) (string, error) {
			if plugin != "llm" || function != "chat" {
				t.Errorf("unexpected plugin/function: %s.%s", plugin, function)
			}
			if inputJSON != `{"prompt":"hello"}` {
				t.Errorf("unexpected input JSON: %s", inputJSON)
			}
			return `{"reply":"hi there"}`, nil
		},
	})

	type ChatRequest struct {
		Prompt string `json:"prompt"`
	}
	type ChatResponse struct {
		Reply string `json:"reply"`
	}

	resp, err := CallTypedEphemeral[ChatResponse](h, "llm", "chat", ChatRequest{Prompt: "hello"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Reply != "hi there" {
		t.Errorf("expected reply 'hi there', got %q", resp.Reply)
	}
}

func TestCallTypedEphemeralMarshalingError(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{})
	_, err := CallTypedEphemeral[string](h, "llm", "chat", make(chan int))
	if err == nil {
		t.Fatal("expected marshaling error, got nil")
	}
	if !strings.Contains(err.Error(), "marshaling") {
		t.Errorf("expected 'marshaling' in error, got: %v", err)
	}
}

func TestCallTypedEphemeralPropagatesCallError(t *testing.T) {
	callErr := errors.New("plugin error")
	h := NewHostCalls(HostCallsOptions{
		PluginCall: func(_, _, _ string) (string, error) {
			return "", callErr
		},
	})

	_, err := CallTypedEphemeral[string](h, "llm", "chat", map[string]string{"k": "v"})
	if err != callErr {
		t.Errorf("expected original error %v, got %v", callErr, err)
	}
}

func TestCallTypedEphemeralUnmarshalError(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{
		PluginCall: func(_, _, _ string) (string, error) {
			return "{bad json}", nil
		},
	})

	type MyResponse struct {
		Result string `json:"result"`
	}
	_, err := CallTypedEphemeral[MyResponse](h, "llm", "chat", map[string]string{})
	if err == nil {
		t.Fatal("expected unmarshaling error, got nil")
	}
	if !strings.Contains(err.Error(), "unmarshaling") {
		t.Errorf("expected 'unmarshaling' in error, got: %v", err)
	}
}

func TestCallTypedEphemeralPluginCallNotInitialized(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{})
	_, err := CallTypedEphemeral[string](h, "llm", "chat", map[string]string{})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "not initialized") {
		t.Errorf("expected 'not initialized' error, got: %v", err)
	}
}

func TestCallTypedEphemeralEmptyResponse(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{
		PluginCall: func(_, _, _ string) (string, error) {
			return ``, nil
		},
	})

	resp, err := CallTypedEphemeral[string](h, "llm", "chat", map[string]string{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp != "" {
		t.Errorf("expected empty string, got %q", resp)
	}
}

// ---------------------------------------------------------------------------
// CallTyped with struct embedding (demonstrates type safety)
// ---------------------------------------------------------------------------

func TestCallTypedWithStructTypes(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{
		DurableCall: func(_, _, requestJSON string) (string, error) {
			return `{"user_id":"u1","display_name":"Alice"}`, nil
		},
	})

	type UserInfo struct {
		UserID      string `json:"user_id"`
		DisplayName string `json:"display_name"`
	}

	user, err := CallTyped[UserInfo](h, "users", "get", map[string]string{"id": "u1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if user.UserID != "u1" {
		t.Errorf("expected UserID 'u1', got %q", user.UserID)
	}
	if user.DisplayName != "Alice" {
		t.Errorf("expected DisplayName 'Alice', got %q", user.DisplayName)
	}
}

func TestCallTypedEphemeralWithStructTypes(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{
		PluginCall: func(_, _, inputJSON string) (string, error) {
			return `{"id":42,"label":"test"}`, nil
		},
	})

	type Item struct {
		ID    int    `json:"id"`
		Label string `json:"label"`
	}

	item, err := CallTypedEphemeral[Item](h, "db", "get", map[string]string{"key": "test"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if item.ID != 42 {
		t.Errorf("expected ID 42, got %d", item.ID)
	}
	if item.Label != "test" {
		t.Errorf("expected Label 'test', got %q", item.Label)
	}
}
