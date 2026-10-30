package main

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/wasm"
)

// knownPlugins is a plugin source that knows exactly the names in the map.
type knownPlugins map[string]bool

func (k knownPlugins) KnownPlugins(_ context.Context, names []string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, n := range names {
		if k[n] {
			out[n] = true
		}
	}
	return out, nil
}

func withPluginSource(t *testing.T, src engine.PluginSource) {
	t.Helper()
	prev := deployPluginSource
	deployPluginSource = func(*sql.DB) engine.PluginSource { return src }
	t.Cleanup(func() { deployPluginSource = prev })
}

// A binary that embeds the given plugin_deps, written to a file.
func binaryDeclaring(t *testing.T, deps map[string]string) string {
	t.Helper()
	b, err := wasm.WriteMetadata([]byte("\x00asm\x01\x00\x00\x00"),
		&wasm.Metadata{WorkflowName: "dep-wf", WorkflowVersion: 1, ABIVersion: 1, MinCompatibleVersion: 1, PluginDeps: deps})
	if err != nil {
		t.Fatalf("WriteMetadata: %v", err)
	}
	return writeWASM(t, t.TempDir(), b)
}

// cleat#3600 (owner D52): `cleatctl deploy workflow` refuses a binary naming a plugin no
// worker has registered, before the store is written, and says how to override.
func TestCleatctlDeployRefusesAnUnregisteredPluginAndWritesNothing(t *testing.T) {
	path := binaryDeclaring(t, map[string]string{"never-registered": ">=1", "also-never": ""})
	withPluginSource(t, knownPlugins{})

	wrote := false
	store := &mockStore{deployWorkflowDefFn: func(context.Context, *engine.WorkflowDef) error { wrote = true; return nil }}
	stderr := withExitPanic(t, func() {
		deployWorkflow(context.Background(), store, nil, []string{"dep-wf", path})
	})
	if wrote {
		t.Error("a refused deploy wrote a definition")
	}
	for _, want := range []string{"never-registered", "also-never", "the WASM binary", "--allow-missing-plugins"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q; got: %s", want, stderr)
		}
	}
}

func TestCleatctlDeployAllowMissingPluginsDeploysAndSaysSo(t *testing.T) {
	path := binaryDeclaring(t, map[string]string{"bootstrap-plugin": ">=1"})
	withPluginSource(t, knownPlugins{})

	var got *engine.WorkflowDef
	store := &mockStore{deployWorkflowDefFn: func(_ context.Context, d *engine.WorkflowDef) error { got = d; return nil }}
	captureStdout(t, func() {
		deployWorkflow(context.Background(), store, nil, []string{"dep-wf", path, "--allow-missing-plugins"})
	})
	if got == nil {
		t.Fatal("the overridden deploy did not reach the store")
	}
}

func TestCleatctlDeployDeploysWhenEveryDeclaredPluginIsRegistered(t *testing.T) {
	path := binaryDeclaring(t, map[string]string{"registered": ">=1"})
	withPluginSource(t, knownPlugins{"registered": true})

	var got *engine.WorkflowDef
	store := &mockStore{deployWorkflowDefFn: func(_ context.Context, d *engine.WorkflowDef) error { got = d; return nil }}
	captureStdout(t, func() {
		deployWorkflow(context.Background(), store, nil, []string{"dep-wf", path})
	})
	if got == nil {
		t.Fatal("a deploy whose plugins are all registered did not reach the store")
	}
}

// With nothing to ask, a binary that declares dependencies is refused, and the override does
// not turn "could not check" into "allowed".
func TestCleatctlDeployWithNoRegistryRefusesEvenWithTheOverride(t *testing.T) {
	path := binaryDeclaring(t, map[string]string{"x": ""})
	withPluginSource(t, nil)

	wrote := false
	store := &mockStore{deployWorkflowDefFn: func(context.Context, *engine.WorkflowDef) error { wrote = true; return nil }}
	stderr := withExitPanic(t, func() {
		deployWorkflow(context.Background(), store, nil, []string{"dep-wf", path, "--allow-missing-plugins"})
	})
	if wrote {
		t.Error("a deploy that could not check its plugins wrote a definition")
	}
	if !strings.Contains(stderr, "cannot check plugin dependencies") {
		t.Errorf("stderr = %q, want it to say the check could not be made", stderr)
	}
}
