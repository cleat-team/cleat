package engine

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/cleat-team/cleat/plugin"
)

// ---------------------------------------------------------------------------
// PluginCall tests
// ---------------------------------------------------------------------------

func TestPluginCall_ReplaySuccess(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{{
		Step: 0, EventType: EventTypePluginCall,
		PluginName: "test-plugin", PluginFunc: "my-func",
		PluginInput: `{"key":"val"}`, PluginOutput: `{"result":"ok"}`,
	}}

	result := s.PluginCall(context.Background(), nil, "test-plugin", "my-func", `{"key":"val"}`, 0, 0)

	errCode := byte(result & 0xFF)
	callErrorCode := byte((result >> 8) & 0xFF)
	if errCode != 0 {
		t.Errorf("expected errCode 0, got %d", errCode)
	}
	if callErrorCode != 0 {
		t.Errorf("expected callErrorCode 0, got %d", callErrorCode)
	}
	if s.stepCount != 1 {
		t.Errorf("expected stepCount=1, got %d", s.stepCount)
	}
}

func TestPluginCall_ReplayCachedError(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{{
		Step: 0, EventType: EventTypePluginCall,
		PluginName: "test-plugin", PluginFunc: "my-func",
		PluginInput: `{"key":"val"}`, PluginError: "something went wrong",
	}}

	result := s.PluginCall(context.Background(), nil, "test-plugin", "my-func", `{"key":"val"}`, 0, 0)

	errCode := byte(result & 0xFF)
	callErrorCode := byte((result >> 8) & 0xFF)
	if errCode != 1 {
		t.Errorf("expected errCode 1, got %d", errCode)
	}
	if callErrorCode != callFailureCode {
		t.Errorf("callErrorCode = %d, want %d -- a recorded plugin failure, classified the same as on the fresh path", callErrorCode, callFailureCode)
	}
}

func TestPluginCall_ReplayDivergentEventType(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{{
		Step: 0, EventType: EventTypeCall, // wrong type
	}}

	result := s.PluginCall(context.Background(), nil, "test-plugin", "my-func", `{}`, 0, 0)

	errCode := byte(result & 0xFF)
	callErrorCode := byte((result >> 8) & 0xFF)
	if errCode != 1 {
		t.Errorf("expected errCode 1 (divergence), got %d", errCode)
	}
	if callErrorCode != callErrorUnknown {
		t.Errorf("callErrorCode = %d, want %d -- a divergence is a bug in the workflow code, not something to retry", callErrorCode, callErrorUnknown)
	}
}

func TestPluginCall_ReplayDivergentPluginName(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{{
		Step: 0, EventType: EventTypePluginCall,
		PluginName: "other-plugin", PluginFunc: "other-func",
		PluginInput: `{"key":"val"}`, PluginOutput: `{"result":"ok"}`,
	}}

	result := s.PluginCall(context.Background(), nil, "test-plugin", "my-func", `{"key":"val"}`, 0, 0)

	errCode := byte(result & 0xFF)
	if errCode != 1 {
		t.Errorf("expected errCode 1 (divergence), got %d", errCode)
	}
}

func TestPluginCall_ReplayIdempotentReinvokes(t *testing.T) {
	callCount := 0
	pr := NewPluginRegistry()
	pr.RegisterIdempotent("test-plugin", "my-func", func(ctx context.Context, inputJSON string) (string, error) {
		callCount++
		return `{"reinvoked":true}`, nil
	})

	s := newTestExecSession()
	s.engine.pluginRegistry = pr
	s.isReplay = true
	s.history = []EventRecord{{
		Step: 0, EventType: EventTypePluginCall,
		PluginName: "test-plugin", PluginFunc: "my-func",
		PluginInput: `{"key":"val"}`, PluginOutput: `{"cached":"old"}`,
		Idempotent: true,
	}}

	buf := make([]byte, 256)
	ctx := contextWithRawMemBuf(context.Background(), buf)
	result := s.PluginCall(ctx, nil, "test-plugin", "my-func", `{"key":"val"}`, 0, 255)

	errCode := byte(result & 0xFF)
	if errCode != 0 {
		t.Errorf("expected errCode 0, got %d", errCode)
	}
	if callCount != 1 {
		t.Errorf("expected 1 re-invocation, got %d", callCount)
	}
	// Should NOT append to newEvents/history since recordEvent=false.
	if len(s.history) != 1 {
		t.Errorf("expected history unchanged (len=1), got %d", len(s.history))
	}
}

func TestPluginCall_ReplayIdempotentFallbackLookup(t *testing.T) {
	callCount := 0
	pr := NewPluginRegistry()
	pr.RegisterIdempotent("test-plugin", "my-func", func(ctx context.Context, inputJSON string) (string, error) {
		callCount++
		return `{"reinvoked":true}`, nil
	})

	s := newTestExecSession()
	s.engine.pluginRegistry = pr
	s.isReplay = true
	s.history = []EventRecord{{
		Step: 0, EventType: EventTypePluginCall,
		PluginName: "test-plugin", PluginFunc: "my-func",
		PluginInput: `{"key":"val"}`, PluginOutput: `{"cached":"old"}`,
		Idempotent: false, // not persisted, but registry has it as idempotent
	}}

	result := s.PluginCall(context.Background(), nil, "test-plugin", "my-func", `{"key":"val"}`, 0, 0)

	errCode := byte(result & 0xFF)
	if errCode != 0 {
		t.Errorf("expected errCode 0, got %d", errCode)
	}
	if callCount != 1 {
		t.Errorf("expected 1 re-invocation from fallback, got %d", callCount)
	}
}

func TestPluginCall_ReplayPastEndExitsReplay(t *testing.T) {
	pr := NewPluginRegistry()
	pr.Register("test-plugin", "my-func", func(ctx context.Context, inputJSON string) (string, error) {
		return `{"fresh":true}`, nil
	})

	s := newTestExecSession()
	s.engine.pluginRegistry = pr
	s.isReplay = true
	s.history = nil

	result := s.PluginCall(context.Background(), nil, "test-plugin", "my-func", `{"key":"val"}`, 0, 0)

	if s.isReplay {
		t.Error("expected isReplay=false after exitReplay")
	}
	errCode := byte(result & 0xFF)
	if errCode != 0 {
		t.Errorf("expected errCode 0, got %d", errCode)
	}
}

func TestPluginCall_FreshSuccess(t *testing.T) {
	pr := NewPluginRegistry()
	pr.Register("test-plugin", "my-func", func(ctx context.Context, inputJSON string) (string, error) {
		if inputJSON != `{"key":"val"}` {
			return "", fmt.Errorf("unexpected input: %s", inputJSON)
		}
		return `{"result":"ok"}`, nil
	})

	s := newTestExecSession()
	s.engine.pluginRegistry = pr

	buf := make([]byte, 256)
	ctx := contextWithRawMemBuf(context.Background(), buf)
	result := s.PluginCall(ctx, nil, "test-plugin", "my-func", `{"key":"val"}`, 0, 255)

	errCode := byte(result & 0xFF)
	if errCode != 0 {
		t.Errorf("expected errCode 0, got %d", errCode)
	}
	if len(s.history) != 1 {
		t.Errorf("expected 1 history entry, got %d", len(s.history))
	}
	if s.history[0].EventType != EventTypePluginCall {
		t.Errorf("expected EventTypePluginCall, got %q", s.history[0].EventType)
	}
	if s.history[0].PluginOutput != `{"result":"ok"}` {
		t.Errorf("expected PluginOutput %q, got %q", `{"result":"ok"}`, s.history[0].PluginOutput)
	}
}

func TestPluginCall_FreshPluginError(t *testing.T) {
	pr := NewPluginRegistry()
	pr.Register("test-plugin", "my-func", func(ctx context.Context, inputJSON string) (string, error) {
		return "", errors.New("plugin error: invalid input")
	})

	s := newTestExecSession()
	s.engine.pluginRegistry = pr

	result := s.PluginCall(context.Background(), nil, "test-plugin", "my-func", `{"bad":"input"}`, 0, 0)

	errCode := byte(result & 0xFF)
	callErrorCode := byte((result >> 8) & 0xFF)
	if errCode != 1 {
		t.Errorf("expected errCode 1, got %d", errCode)
	}
	if callErrorCode != callFailureCode {
		t.Errorf("callErrorCode = %d, want %d -- the plugin function failed; the engine cannot classify it further", callErrorCode, callFailureCode)
	}
	if s.history[0].PluginError != "plugin error: invalid input" {
		t.Errorf("expected PluginError %q, got %q", "plugin error: invalid input", s.history[0].PluginError)
	}
}

func TestPluginCall_FreshUnknownPlugin(t *testing.T) {
	pr := NewPluginRegistry()
	// Register a different function to prove lookup fails.
	pr.Register("other-plugin", "other-func", func(ctx context.Context, inputJSON string) (string, error) {
		return "ok", nil
	})

	s := newTestExecSession()
	s.engine.pluginRegistry = pr

	result := s.PluginCall(context.Background(), nil, "test-plugin", "my-func", `{}`, 0, 0)

	errCode := byte(result & 0xFF)
	callErrorCode := byte((result >> 8) & 0xFF)
	if errCode != 1 {
		t.Errorf("expected errCode 1, got %d", errCode)
	}
	if callErrorCode != callFailureCode {
		t.Errorf("callErrorCode = %d, want %d -- an unregistered plugin arrives as a plugin function error", callErrorCode, callFailureCode)
	}
	if s.history[0].PluginName != "test-plugin" {
		t.Errorf("expected PluginName %q, got %q", "test-plugin", s.history[0].PluginName)
	}
}

func TestPluginCall_FreshNoRegistry(t *testing.T) {
	s := newTestExecSession()
	// pluginRegistry is nil.

	result := s.PluginCall(context.Background(), nil, "test-plugin", "my-func", `{}`, 0, 0)

	errCode := byte(result & 0xFF)
	callErrorCode := byte((result >> 8) & 0xFF)
	if errCode != 1 {
		t.Errorf("expected errCode 1, got %d", errCode)
	}
	if callErrorCode != callErrorUnknown {
		t.Errorf("callErrorCode = %d, want %d -- no plugin registry is a deployment problem; retrying supplies none", callErrorCode, callErrorUnknown)
	}
}

func TestPluginCall_FreshWithCallGuardBlocked(t *testing.T) {
	pr := NewPluginRegistry()
	pr.Register("target-plugin", "my-func", func(ctx context.Context, inputJSON string) (string, error) {
		return "ok", nil
	})

	s := newTestExecSession()
	s.engine.pluginRegistry = pr
	s.engine.pluginCallGuard = newDenyAllCallGuard()
	s.callerPluginName = "caller-plugin"

	result := s.PluginCall(context.Background(), nil, "target-plugin", "my-func", `{}`, 0, 0)

	errCode := byte(result & 0xFF)
	if errCode != 1 {
		t.Errorf("expected errCode 1 (guard blocked), got %d", errCode)
	}
}

// newDenyAllCallGuard returns a PluginCallGuard that denies all cross-plugin calls.
func newDenyAllCallGuard() *PluginCallGuard {
	g := NewPluginCallGuard()
	g.Allow("caller-plugin", nil)
	return g
}

func TestPluginCall_FreshWithTenantAndWorkflowID(t *testing.T) {
	var capturedCtx context.Context
	pr := NewPluginRegistry()
	pr.Register("test-plugin", "my-func", func(ctx context.Context, inputJSON string) (string, error) {
		capturedCtx = ctx
		return `{"result":"ok"}`, nil
	})

	s := newTestExecSession()
	s.engine.pluginRegistry = pr
	s.tenantID = "tenant-abc"
	s.workflowID = "wf-123"

	buf := make([]byte, 256)
	ctx := contextWithRawMemBuf(context.Background(), buf)
	result := s.PluginCall(ctx, nil, "test-plugin", "my-func", `{}`, 0, 255)

	errCode := byte(result & 0xFF)
	if errCode != 0 {
		t.Errorf("expected errCode 0, got %d", errCode)
	}
	cc := plugin.CallContextFromContext(capturedCtx)
	if cc == nil {
		t.Fatal("expected CallContext in captured context")
	}
	if cc.TenantID != "tenant-abc" {
		t.Errorf("expected TenantID %q, got %q", "tenant-abc", cc.TenantID)
	}
	if cc.WorkflowID != "wf-123" {
		t.Errorf("expected WorkflowID %q, got %q", "wf-123", cc.WorkflowID)
	}
}

func TestPluginCall_ReplayIdempotentPluginWithHistory(t *testing.T) {
	callCount := 0
	pr := NewPluginRegistry()
	pr.RegisterIdempotent("test-plugin", "my-func", func(ctx context.Context, inputJSON string) (string, error) {
		callCount++
		return `{"live":true}`, nil
	})

	s := newTestExecSession()
	s.engine.pluginRegistry = pr
	s.isReplay = true
	s.history = []EventRecord{{
		Step: 0, EventType: EventTypePluginCall,
		PluginName: "test-plugin", PluginFunc: "my-func",
		PluginInput: `{"key":"val"}`, PluginOutput: `{"cached":"old"}`,
	}}

	result := s.replayPluginCall(context.Background(), nil, "test-plugin", "my-func", `{"key":"val"}`, 0, 0)

	errCode := byte(result & 0xFF)
	if errCode != 0 {
		t.Errorf("expected errCode 0, got %d", errCode)
	}
	// Should re-invoke because the step is within history but idempotent.
	if callCount != 1 {
		t.Errorf("expected 1 re-invocation, got %d", callCount)
	}
}
