package main

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// registerTestStores installs the fixtures these tests share, exactly once per PROCESS.
//
// Both halves of that are load-bearing and both were measured. Without the Once, `go test -count=2` panics
// on the duplicate registration that registerStore is right to refuse. Registering each store inside the
// test that uses it makes the tests order-dependent: the refusal test passed in a full run because another
// test had already registered `test-alpha`, and FAILED when run alone.
var registerTestStores = sync.OnceFunc(func() {
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
})

func writeFixture(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// The registry's whole purpose is that TWO STORES CAN BE ADDED INDEPENDENTLY, so the test that matters is
// two registered side by side, each reached only through its own name and fixture. A single-store test
// would pass with a registry that hard-coded one name.
func TestTwoRegisteredStoresAreIndependent(t *testing.T) {
	registerTestStores()

	dir := t.TempDir()
	alpha := writeFixture(t, dir, "alpha.json", `{"from":"alpha"}`)
	beta := writeFixture(t, dir, "beta.json", `{"from":"beta"}`)

	opts, specs, err := resolveScripts([]string{"test-alpha=" + alpha, "test-beta=" + beta})
	if err != nil {
		t.Fatalf("two independent stores were refused: %v", err)
	}
	if len(opts) != 2 {
		t.Fatalf("got %d engine options, want one per store", len(opts))
	}
	if len(specs) != 2 {
		t.Fatalf("got %d specs, want one per store", len(specs))
	}

	extras := extrasFrom(specs)
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
	if _, err := storeRegistry["test-alpha"](alpha); err != nil {
		t.Fatalf("alpha factory on alpha's own fixture: %v", err)
	}
	if _, err := storeRegistry["test-beta"](beta); err != nil {
		t.Fatalf("beta factory on beta's own fixture: %v", err)
	}
}

// The registered closure outlives a single run of the test -- the registry is process-global and a second
// -count iteration reuses it -- so the state it reads is package-level and RESET at the start of each run.
// Capturing per-run locals instead made the second iteration read 0, which is the same defect one level up.
var testCalls = struct {
	mu    sync.Mutex
	calls []string
	reads int
}{}

func resetTestCalls() {
	testCalls.mu.Lock()
	defer testCalls.mu.Unlock()
	testCalls.calls = nil
	testCalls.reads = 0
}

// EXTRA IS READ AFTER THE RUN. This is a regression test for a defect that was measured, not imagined: when
// resolveScripts called Extra during fixture loading, a store that copied its own call list reported `[]`
// for a run that had made `[update/approve]`. The reads counter below is the direct statement of the
// ordering -- zero reads during resolve, one afterwards -- so a change that moves the call back fails HERE
// rather than in whichever group's assertion happens to notice.
func TestExtraIsNotCalledUntilTheRunIsOver(t *testing.T) {
	resetTestCalls()
	registerStoreOnce(t, "test-calls", func(path string) (storeSpec, error) {
		if _, err := os.ReadFile(path); err != nil {
			return storeSpec{}, err
		}
		return storeSpec{
			Option: engine.WithPromiseStore(newMemoryPromises(nil, nil)),
			Extra: func() (string, any) {
				testCalls.mu.Lock()
				defer testCalls.mu.Unlock()
				testCalls.reads++
				return "calls", append([]string(nil), testCalls.calls...)
			},
		}, nil
	})

	f := writeFixture(t, t.TempDir(), "calls.json", `{}`)
	_, specs, err := resolveScripts([]string{"test-calls=" + f})
	if err != nil {
		t.Fatalf("resolveScripts: %v", err)
	}
	testCalls.mu.Lock()
	reads := testCalls.reads
	testCalls.mu.Unlock()
	if reads != 0 {
		t.Fatalf("Extra was called %d time(s) while loading the fixture; the run has not happened yet", reads)
	}

	// Stand in for the run: this is what the engine does to a store between construction and return.
	testCalls.mu.Lock()
	testCalls.calls = append(testCalls.calls, "update/approve")
	testCalls.mu.Unlock()

	extras := extrasFrom(specs)
	got, ok := extras["calls"].([]string)
	if !ok {
		t.Fatalf("extras[calls] = %v, want the store's call list", extras["calls"])
	}
	if len(got) != 1 || got[0] != "update/approve" {
		t.Fatalf("extras[calls] = %v, want the call the run made -- pre-run state is the bug this guards", got)
	}
	testCalls.mu.Lock()
	defer testCalls.mu.Unlock()
	if testCalls.reads != 1 {
		t.Errorf("Extra read %d times, want exactly once", testCalls.reads)
	}
}

func TestResolveScriptsRefusesAnUnknownNameAndListsTheKnownOnes(t *testing.T) {
	registerTestStores()

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
	registerTestStores()

	// REAL FILES, because the duplicate is checked before the factory runs but the FIRST pair still has to
	// get past it. With a nonexistent path the factory errors first and the duplicate check is never
	// reached -- deleting it left the package `ok`, which is how this was found.
	dir := t.TempDir()
	a := writeFixture(t, dir, "a.json", `{}`)
	b := writeFixture(t, dir, "b.json", `{}`)

	_, _, err := resolveScripts([]string{"test-alpha=" + a, "test-alpha=" + b})
	if err == nil {
		t.Fatal("the same store twice was accepted; one store takes one fixture")
	}
	if got := err.Error(); !contains(got, "given twice") {
		t.Fatalf("error = %q, want it to name the duplicate rather than fail somewhere else", got)
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

// registerStoreOnce is registerTestStores for a store only one test needs: the registry is process-global,
// so a second run of the same test under -count=2 would otherwise panic on the duplicate.
func registerStoreOnce(t *testing.T, name string, f storeFactory) {
	t.Helper()
	onceFor(name, f)()
}

var (
	storeOnceMu sync.Mutex
	storeOnces  = map[string]*sync.Once{}
)

func onceFor(name string, f storeFactory) func() {
	storeOnceMu.Lock()
	defer storeOnceMu.Unlock()
	o, ok := storeOnces[name]
	if !ok {
		o = &sync.Once{}
		storeOnces[name] = o
	}
	return func() { o.Do(func() { registerStore(name, f) }) }
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
