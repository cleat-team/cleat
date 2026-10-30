package main

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/wasm"
)

// recordingRegistry is a plugin registry that knows exactly the names in known and keeps the
// overrides written to it, or refuses to when recordErr is set.
type recordingRegistry struct {
	known     map[string]bool
	overrides []engine.PluginDepOverride
	recordErr error
}

func (r *recordingRegistry) KnownPlugins(_ context.Context, names []string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, n := range names {
		if r.known[n] {
			out[n] = true
		}
	}
	return out, nil
}

func (r *recordingRegistry) RecordPluginDepOverride(_ context.Context, o engine.PluginDepOverride) error {
	if r.recordErr != nil {
		return r.recordErr
	}
	r.overrides = append(r.overrides, o)
	return nil
}

// knownPlugins is a registry that knows the names in the map and accepts any override.
type knownPlugins map[string]bool

func (k knownPlugins) KnownPlugins(ctx context.Context, names []string) (map[string]bool, error) {
	return (&recordingRegistry{known: k}).KnownPlugins(ctx, names)
}

func (k knownPlugins) RecordPluginDepOverride(context.Context, engine.PluginDepOverride) error {
	return nil
}

func withPluginSource(t *testing.T, reg engine.PluginRegistryAccess) {
	t.Helper()
	prev := deployPluginSource
	deployPluginSource = func(*sql.DB) engine.PluginRegistryAccess { return reg }
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
	withPluginSource(t, &recordingRegistry{})

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

func TestCleatctlDeployAllowMissingPluginsRecordsTheOverrideThenDeploys(t *testing.T) {
	path := binaryDeclaring(t, map[string]string{"bootstrap-plugin": ">=1"})
	reg := &recordingRegistry{}
	withPluginSource(t, reg)

	var got *engine.WorkflowDef
	var recordedBeforeWrite bool
	store := &mockStore{deployWorkflowDefFn: func(_ context.Context, d *engine.WorkflowDef) error {
		got = d
		recordedBeforeWrite = len(reg.overrides) == 1
		return nil
	}}
	captureStdout(t, func() {
		deployWorkflow(context.Background(), store, nil, []string{"dep-wf", path, "--allow-missing-plugins"})
	})
	if got == nil {
		t.Fatal("the overridden deploy did not reach the store")
	}
	if !recordedBeforeWrite {
		t.Error("the override was not recorded before the definition was written")
	}
	rec := reg.overrides[0]
	if rec.Definition != "dep-wf" || rec.Via != "cleatctl" || rec.Version != got.Version || rec.TenantID != engine.DefaultTenantUUID || rec.Actor == "" {
		t.Errorf("record = %+v, want definition dep-wf, via cleatctl, the deployed version, the default tenant and an actor", rec)
	}
	if len(rec.Missing) != 1 || rec.Missing[0].Name != "bootstrap-plugin" {
		t.Errorf("record's missing = %+v, want bootstrap-plugin", rec.Missing)
	}
}

// An override whose record cannot be written is not honoured: nothing is deployed.
func TestCleatctlDeployRefusesTheOverrideWhenItsRecordCannotBeWritten(t *testing.T) {
	path := binaryDeclaring(t, map[string]string{"bootstrap-plugin": ">=1"})
	withPluginSource(t, &recordingRegistry{recordErr: errors.New("audit table unwritable")})

	wrote := false
	store := &mockStore{deployWorkflowDefFn: func(context.Context, *engine.WorkflowDef) error { wrote = true; return nil }}
	stderr := withExitPanic(t, func() {
		deployWorkflow(context.Background(), store, nil, []string{"dep-wf", path, "--allow-missing-plugins"})
	})
	if wrote {
		t.Error("a deploy whose override could not be recorded wrote a definition")
	}
	if !strings.Contains(stderr, "could not be recorded") {
		t.Errorf("stderr = %q, want it to say the use could not be recorded", stderr)
	}
}

func TestCleatctlDeployDeploysWhenEveryDeclaredPluginIsRegistered(t *testing.T) {
	path := binaryDeclaring(t, map[string]string{"registered": ">=1"})
	withPluginSource(t, &recordingRegistry{known: map[string]bool{"registered": true}})

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
