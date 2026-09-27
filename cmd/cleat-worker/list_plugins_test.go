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
// changes as plugins are wired in -- and it did: 3.315 found one plugin linked,
// and every bundled plugin except pgvector is linked now.
//
// pgvector is the negative control and a real one, not a token. It is excluded
// because its Migrations() creates an `embedding vector(1536)` column and
// plugin.RunMigrations is FATAL at boot, so linking it would stop cleat-worker
// starting on any PostgreSQL without the vector extension. If it ever appears
// here, that decision was reversed and this test should fail until someone
// confirms the migration degrades.
func TestListPluginsReportsTheBinarysActualImportBlock(t *testing.T) {
	var buf bytes.Buffer
	runListPlugins(&buf)
	out := buf.String()

	// Linked, and each was reachable from nothing before 3.315 was acted on.
	for _, name := range []string{"llm", "event-triggers", "eventstore", "webhook-ingest", "kafka-connect"} {
		if !strings.Contains(out, name) {
			t.Errorf("%s is blank-imported by main.go but is not listed:\n%s", name, out)
		}
	}

	// Not linked, deliberately.
	if strings.Contains(out, "pgvector") {
		t.Errorf("pgvector is listed, so it is now linked into cleat-worker. Its "+
			"Migrations() creates a vector(1536) column and plugin.RunMigrations is "+
			"fatal at boot, so this stops the worker starting wherever the extension "+
			"is unavailable. Confirm the migration degrades before allowing this.\n%s", out)
	}
}
