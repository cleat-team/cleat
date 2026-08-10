package engine

import (
	"strings"
	"testing"
)

func TestNewPluginCallGuard_AllowsUnknownCaller(t *testing.T) {
	g := NewPluginCallGuard()
	err := g.Check("any-caller", "any-target")
	if err != nil {
		t.Errorf("new guard should allow unknown callers, got: %v", err)
	}
}

func TestAllow_SpecificTargets(t *testing.T) {
	g := NewPluginCallGuard()
	g.Allow("plugin-a", []string{"plugin-b", "plugin-c"})

	if err := g.Check("plugin-a", "plugin-b"); err != nil {
		t.Errorf("plugin-a should be allowed to call plugin-b: %v", err)
	}
	if err := g.Check("plugin-a", "plugin-c"); err != nil {
		t.Errorf("plugin-a should be allowed to call plugin-c: %v", err)
	}
	if err := g.Check("plugin-a", "plugin-d"); err == nil {
		t.Error("plugin-a should NOT be allowed to call plugin-d")
	}
}

func TestAllow_Wildcard(t *testing.T) {
	g := NewPluginCallGuard()
	g.Allow("plugin-a", []string{"*"})

	if err := g.Check("plugin-a", "anything"); err != nil {
		t.Errorf("wildcard should allow any target: %v", err)
	}
	if err := g.Check("plugin-a", "anything-else"); err != nil {
		t.Errorf("wildcard should allow any target: %v", err)
	}
}

func TestCheck_UnknownCaller(t *testing.T) {
	g := NewPluginCallGuard()
	g.Allow("plugin-a", []string{"plugin-b"})

	// plugin-b is not in the guard at all — always allowed
	if err := g.Check("plugin-b", "plugin-a"); err != nil {
		t.Errorf("unknown caller should be allowed: %v", err)
	}
}

func TestCheck_Denied_ErrorMessage(t *testing.T) {
	g := NewPluginCallGuard()
	g.Allow("plugin-a", []string{"plugin-x"})

	err := g.Check("plugin-a", "plugin-y")
	if err == nil {
		t.Fatal("expected error for denied call")
	}
	msg := err.Error()
	if !strings.Contains(msg, "plugin-a") || !strings.Contains(msg, "plugin-y") {
		t.Errorf("error message should mention both plugins: %q", msg)
	}
	if !strings.Contains(msg, "not allowed") {
		t.Errorf("error message should say 'not allowed': %q", msg)
	}
}

func TestAllow_Overwrite(t *testing.T) {
	g := NewPluginCallGuard()
	g.Allow("plugin-a", []string{"plugin-b"})
	g.Allow("plugin-a", []string{"plugin-c"})

	// old target should no longer be allowed
	if err := g.Check("plugin-a", "plugin-b"); err == nil {
		t.Error("after overwrite, plugin-b should not be allowed")
	}
	// new target should be allowed
	if err := g.Check("plugin-a", "plugin-c"); err != nil {
		t.Errorf("after overwrite, plugin-c should be allowed: %v", err)
	}
}

func TestAllow_EmptyTargets(t *testing.T) {
	g := NewPluginCallGuard()
	g.Allow("plugin-a", []string{})

	// with no targets and no wildcard, nothing should be allowed
	if err := g.Check("plugin-a", "anything"); err == nil {
		t.Error("with empty targets, no calls should be allowed")
	}
}
