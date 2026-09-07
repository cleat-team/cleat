package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/plugin"
)

// fakeListPlugin is a minimal Plugin so the test can put a name into the
// registry that no production package uses.
type fakeListPlugin struct{ name string }

func (f *fakeListPlugin) Info() plugin.PluginInfo {
	return plugin.PluginInfo{Name: f.name, Version: "9.9.9", Description: "registered by list_plugins_test"}
}
func (f *fakeListPlugin) Init(context.Context, *plugin.Environment) error { return nil }

// TestListPluginsReportsTheRegistryAndNotAConstant is written against the
// failure mode this flag exists to prevent: a diagnostic that prints something
// plausible without consulting the thing it claims to report.
//
// It therefore asserts in both directions, per CLAUDE.md -- a name that IS
// registered must appear (known-positive), and a name that is NOT registered
// must not (negative control). A function returning a hardcoded list passes the
// first and fails the second; a function returning an empty list fails the
// first. Neither alone is sufficient.
func TestListPluginsReportsTheRegistryAndNotAConstant(t *testing.T) {
	const registered = "zz-list-plugins-test-fixture"
	const neverRegistered = "zz-this-plugin-does-not-exist"

	plugin.Register(plugin.PluginInfo{
		Name:        registered,
		Version:     "9.9.9",
		Description: "registered by list_plugins_test",
	}, func() plugin.Plugin { return &fakeListPlugin{name: registered} })

	var buf bytes.Buffer
	if code := runListPlugins(&buf); code != 0 {
		t.Fatalf("runListPlugins returned %d, want 0; output:\n%s", code, buf.String())
	}
	out := buf.String()

	if !strings.Contains(out, registered) {
		t.Errorf("a plugin registered moments ago is missing from the output -- this "+
			"function is not reading the registry.\ngot:\n%s", out)
	}
	if strings.Contains(out, neverRegistered) {
		t.Errorf("output names %q, which was never registered -- the function is "+
			"printing something other than the registry.\ngot:\n%s", neverRegistered, out)
	}
}

// TestListPluginsReportsTheBinarysActualImportBlock asserts the product fact
// that IMPROVEMENT-PLAN.md 3.315 recorded: a plugin appears here only if
// cmd/cleat-worker/main.go blank-imports it.
//
// It deliberately does NOT assert a count. The count is a product decision that
// will change as plugins are wired in; what must stay true is the mechanism --
// llm is linked, so it is listed, and event-triggers is not linked, so it is
// not. Delete the llm import and this test goes red, which is the point: the
// import block is the feature set and nothing else was checking it.
func TestListPluginsReportsTheBinarysActualImportBlock(t *testing.T) {
	var buf bytes.Buffer
	runListPlugins(&buf)
	out := buf.String()

	if !strings.Contains(out, "llm") {
		t.Errorf("llm is blank-imported by main.go but is not listed:\n%s", out)
	}
	// Not linked by cmd/cleat-worker. If this ever fails, the subsystem was
	// wired in -- update 3.315 and the event-routing design's P0 rather than
	// this assertion.
	if strings.Contains(out, "event-triggers") {
		t.Errorf("event-triggers is listed, so it is now linked into cleat-worker. "+
			"That is a real change: its migrations will now run. Update "+
			"IMPROVEMENT-PLAN.md 3.315 and docs/contributor/design/event-routing-design.md P0.\n%s", out)
	}
}
