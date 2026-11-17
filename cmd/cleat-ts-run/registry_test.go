package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// The registry's whole purpose is that TWO STORES CAN BE ADDED INDEPENDENTLY, so the test that matters is
// two registered side by side, each reached only through its own name and fixture. A single-store test
// would pass with a registry that hard-coded one name.
func TestTwoRegisteredStoresAreIndependent(t *testing.T) {
	// Both are registered from THIS test rather than from a group's init(): the registry is process-global,
	// and the production names belong to files this change does not touch.
	registerStore("test-alpha", func(path string) (storeSpec, error) {
		b, err := os.ReadFile(path)
		if err != nil {
			return storeSpec{}, err
		}
		return storeSpec{
			Option: engine.WithPromiseStore(newMemoryPromises(map[string]string{"alpha": string(b)}, nil)),
			Extra:  func() (string, any) { return "alpha", string(b) },
		}, nil
	})
	registerStore("test-beta", func(path string) (storeSpec, error) {
		b, err := os.ReadFile(path)
		if err != nil {
			return storeSpec{}, err
		}
		return storeSpec{
			Option: engine.WithChildWorkflowStore(newMemoryChildren(map[string]engine.ChildOutcome{
				"beta": {Completed: true, Result: string(b)},
			})),
			// No Extra: a store with nothing to report must not be forced to invent one.
		}, nil
	})

	dir := t.TempDir()
	alpha := filepath.Join(dir, "alpha.json")
	beta := filepath.Join(dir, "beta.json")
	if err := os.WriteFile(alpha, []byte(`{"from":"alpha"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(beta, []byte(`{"from":"beta"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	opts, extras, err := resolveScripts([]string{"test-alpha=" + alpha, "test-beta=" + beta})
	if err != nil {
		t.Fatalf("two independent stores were refused: %v", err)
	}
	if len(opts) != 2 {
		t.Fatalf("got %d engine options, want one per store", len(opts))
	}
	if len(extras) != 1 {
		t.Fatalf("extras = %v, want only the store that declared one", extras)
	}
	if got := extras["alpha"]; got != `{"from":"alpha"}` {
		t.Errorf("extras[alpha] = %v, want its own fixture content", got)
	}
	if _, ok := extras["test-beta"]; ok {
		t.Error("a store that declared no Extra contributed one anyway")
	}

	// Each factory saw only its OWN file: the cross-wiring this is here to catch. Called directly, so a
	// registry that resolved names by position rather than by name would show up here.
	alphaSpec, err := storeRegistry["test-alpha"](alpha)
	if err != nil {
		t.Fatalf("alpha factory on alpha's own fixture: %v", err)
	}
	if k, v := alphaSpec.Extra(); k != "alpha" || v != `{"from":"alpha"}` {
		t.Errorf("alpha factory read the wrong fixture: %s=%v", k, v)
	}
	if _, err := storeRegistry["test-beta"](beta); err != nil {
		t.Fatalf("beta factory on beta's own fixture: %v", err)
	}
}

func TestResolveScriptsRefusesAnUnknownNameAndListsTheKnownOnes(t *testing.T) {
	// A typo must not configure nothing silently. For the update store that would mean the engine reports
	// no pending update and an update test passes having delivered nothing (engine/updater.go:98).
	_, _, err := resolveScripts([]string{"nosuchstore=/tmp/x.json"})
	if err == nil {
		t.Fatal("an unregistered store name was accepted")
	}
	if got := err.Error(); !contains(got, "not a registered store") || !contains(got, "test-alpha") {
		t.Fatalf("error = %q, want it to name the problem and list the known stores", got)
	}
}

func TestResolveScriptsRefusesDuplicatesAndMissingFiles(t *testing.T) {
	if _, _, err := resolveScripts([]string{"test-alpha=/a.json", "test-alpha=/b.json"}); err == nil {
		t.Error("the same store twice was accepted; one store takes one fixture")
	}
	if _, _, err := resolveScripts([]string{"test-alpha="}); err == nil {
		t.Error("-script with no file was accepted")
	}
}

// The flag must accept repeats: a single-value flag would silently keep only the last, and every earlier
// store would configure nothing while the run looked fine.
func TestScriptFlagKeepsEveryOccurrence(t *testing.T) {
	var f scriptFlag
	for _, v := range []string{"a=/1.json", "b=/2.json", "c=/3.json"} {
		if err := f.Set(v); err != nil {
			t.Fatalf("Set(%q): %v", v, err)
		}
	}
	if len(f.pairs) != 3 {
		t.Fatalf("kept %d of 3 values: %v", len(f.pairs), f.pairs)
	}
	if err := f.Set("no-equals-sign"); err == nil {
		t.Error("a value without '=' was accepted")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
