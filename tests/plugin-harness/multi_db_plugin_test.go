package pluginharness

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/cleat-team/cleat/cleat/wasmtest"
	"github.com/cleat-team/cleat/plugin"
)

// TestPluginCalls_MultiDB executes the Go workflow against each supported
// database backend (PostgreSQL, MySQL, MSSQL) with a fully wired plugin
// environment, verifying that plugin calls work correctly over every dialect.
func TestPluginCalls_MultiDB(t *testing.T) {
	backends := []struct {
		name    string
		dialect plugin.Dialect
		envVar  string
	}{
		{"postgres", plugin.DialectPostgres, "CLEAT_TEST_POSTGRES"},
		{"mysql", plugin.DialectMySQL, "CLEAT_TEST_MYSQL"},
		{"mssql", plugin.DialectMSSQL, "CLEAT_TEST_MSSQL"},
	}

	for _, backend := range backends {
		t.Run(backend.name, func(t *testing.T) {
			connStr := os.Getenv(backend.envVar)
			if connStr == "" {
				t.Skipf("%s not set -- skipping %s backend", backend.envVar, backend.name)
			}

			ctx := context.Background()
			db, schemaName := OpenTestDB(t, backend.dialect, connStr)
			defer CleanupTestDB(t, db, backend.dialect, schemaName)

			env := NewTestPluginEnv(t, ctx, db, backend.dialect, schemaName)
			defer env.Close()

			// Build Go WASM and execute.
			wasmBytes := buildGoWorkflowWasm(t)

			wenv := wasmtest.NewWasmTestEnv(t, wasmtest.WithPluginRegistry(env.Registry))
			defer wenv.Close()

			result, history, err := wenv.Execute(t, wasmBytes, "call_all_plugins", `{}`)
			if err != nil {
				t.Fatalf("workflow execution failed: %v", err)
			}

			// One unmarshal. This ran a GO workflow, and Go's generated wrapper
			// now passes the result through rather than re-encoding it (ABI.md:
			// an entry point returns a string containing a JSON-encoded object).
			// The comment here used to say Go "wraps the result in a
			// JSON-encoded string", which described the bug as though it were
			// the design.
			var results map[string]interface{}
			if err := json.Unmarshal([]byte(result), &results); err != nil {
				t.Fatalf("failed to parse result JSON: %v\nraw: %.500s", err, result)
			}

			expectedKeys := []string{
				"blobstore.put",
				"feature-flags.evaluate_flag",
				"kafka-connect.produce",
				"notifications.send_webhook",
				"llm.chat",
			}
			for _, key := range expectedKeys {
				if _, ok := results[key]; !ok {
					t.Errorf("missing result key: %s", key)
				}
			}

			// TODO: Verify DB side-effects for specific plugins.
			//   - blobstore: check blob_index has our test-key
			//   - feature-flags: check evaluation result was persisted
			//   - notifications: check delivery row was inserted
			// These require inspecting the test database after execution.

			// Replay verification.
			result2, err := wenv.Replay(t, wasmBytes, "call_all_plugins", `{}`, history)
			if err != nil {
				t.Fatalf("replay failed: %v", err)
			}
			if result != result2 {
				t.Errorf("replay mismatch: original result %q != replayed %q", result, result2)
			}

			t.Logf("%s backend: plugin workflow execute and replay produce identical results", backend.name)
		})
	}
}
