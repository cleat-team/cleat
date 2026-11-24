package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// A run's own wasm_instance_timeout_ms (StartOptions.RunLimits.WasmInstanceTimeout, set by the start request) is held to the same
// floor as the worker's --wasm-instance-timeout and a tenant's setting (cleat#3922, owner decision 2026-11-18).
//
// None of these tests needs a database: the refusal happens before any store work, which is also what lets them run, unskipped, on
// every job, and lets the three dialect stores and the sharded store be exercised as zero values.

func TestStartOptionsRefuseAPerRunInstanceTimeoutBelowTheFloor(t *testing.T) {
	for _, tc := range []struct {
		d       time.Duration
		refused bool
	}{
		{time.Millisecond, true},
		{100 * time.Millisecond, true},
		{249 * time.Millisecond, true},
		{249*time.Millisecond + 900*time.Microsecond, true},
		{MinWasmInstanceTimeout, false},
		{30 * time.Second, false},
		{0, false}, // no override
	} {
		err := StartOptions{RunLimits: TenantSettings{WasmInstanceTimeout: tc.d}}.Validate()
		if tc.refused && !errors.Is(err, ErrWasmInstanceTimeoutBelowFloor) {
			t.Errorf("Validate with %v = %v, want ErrWasmInstanceTimeoutBelowFloor", tc.d, err)
		}
		if !tc.refused && err != nil {
			t.Errorf("Validate with %v = %v, want nil", tc.d, err)
		}
	}
	// The floor judges the instance timeout only: the other three per-run bounds are not subject to it.
	if err := (StartOptions{RunLimits: TenantSettings{
		WasmWallClockCeiling: 10 * time.Millisecond, HostRetryBudget: 10 * time.Millisecond, MaxWorkflowDuration: 10 * time.Millisecond,
	}}).Validate(); err != nil {
		t.Errorf("a low wall-clock ceiling, retry budget or workflow duration was refused by the instance-timeout floor: %v", err)
	}
	err := StartOptions{RunLimits: TenantSettings{WasmInstanceTimeout: 100 * time.Millisecond}}.Validate()
	for _, want := range []string{"per-run wasm_instance_timeout_ms", "100ms", "250ms", "defers", "ABI.md"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not contain %q: %v", want, err)
		}
	}
}

// Every store's StartNewRunWithOptions refuses, before touching its connection. Zero-value stores have no database, so a refusal
// that DID reach the database would panic on the nil handle rather than return an error: the refusal and its position are one claim.
func TestEveryStoreRefusesAPerRunInstanceTimeoutBelowTheFloorBeforeItTouchesTheDatabase(t *testing.T) {
	opts := StartOptions{RunLimits: TenantSettings{WasmInstanceTimeout: 100 * time.Millisecond}}
	in := json.RawMessage(`{}`)
	for name, start := range map[string]func() (string, bool, error){
		"PostgresStore": func() (string, bool, error) {
			return (&PostgresStore{}).StartNewRunWithOptions(context.Background(), "", "d", 1, in, "", "", 0, opts)
		},
		"MySQLStore": func() (string, bool, error) {
			return (&MySQLStore{}).StartNewRunWithOptions(context.Background(), "", "d", 1, in, "", "", 0, opts)
		},
		"MSSQLStore": func() (string, bool, error) {
			return (&MSSQLStore{}).StartNewRunWithOptions(context.Background(), "", "d", 1, in, "", "", 0, opts)
		},
		"ShardedStore": func() (string, bool, error) {
			return (&ShardedStore{}).StartNewRunWithOptions(context.Background(), "", "d", 1, in, "", "", 0, opts)
		},
	} {
		t.Run(name, func(t *testing.T) {
			// A store that does not refuse reaches its nil connection and panics; report that as the failure it is, for THIS store,
			// rather than letting it take the other stores' subtests down with it.
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("%s.StartNewRunWithOptions(100ms) reached its database instead of refusing first: %v", name, r)
				}
			}()
			id, existed, err := start()
			if !errors.Is(err, ErrWasmInstanceTimeoutBelowFloor) {
				t.Fatalf("%s.StartNewRunWithOptions(100ms) = (%q, %v, %v), want ErrWasmInstanceTimeoutBelowFloor", name, id, existed, err)
			}
			if id != "" || existed {
				t.Errorf("a refused start returned an id %q / existed=%v", id, existed)
			}
		})
	}
}

// The per-run limit has ONE place that builds it from a request, cmd/cleat-worker/server.go, and the floor is checked there and in the
// stores. This keeps it so: a second place that builds StartOptions.RunLimits (a Go client option, a child-workflow option, a schedule,
// an MCP or cleatctl start) fails here until it is shown to reach StartOptions.Validate, instead of being found by a caller that hits
// the cliff. A TEXT scan of a format it does not model, so it has a known-positive: it must find the one site that exists.
func TestEveryBuilderOfPerRunLimitsIsKnown(t *testing.T) {
	root := ".."
	build := regexp.MustCompile(`\bRunLimits\s*(:|=[^=])`)
	var sites []string
	for _, dir := range []string{"engine", "cmd", "plugins", "plugin", "auth", "cleat", "internal", "web", "examples", "tests"} {
		_ = filepath.Walk(filepath.Join(root, dir), func(p string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			b, rerr := os.ReadFile(p)
			if rerr != nil {
				return nil
			}
			if build.Match(b) {
				rel, _ := filepath.Rel(root, p)
				sites = append(sites, filepath.ToSlash(rel))
			}
			return nil
		})
	}
	const known = "cmd/cleat-worker/server.go"
	if len(sites) != 1 || sites[0] != known {
		t.Errorf("StartOptions.RunLimits is built in %v, want exactly [%s]. A new builder must reach engine.StartOptions.Validate (the "+
			"stores' StartNewRunWithOptions already do) and gets an entry here; an EMPTY list means this scan stopped seeing the file it "+
			"exists for.", sites, known)
	}
}
