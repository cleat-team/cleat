package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// cleat#4396. wasi:random/insecure and wasi:random/insecure-seed are in wasiDeterminismInterfaces, and until this test NOTHING exercised them: no
// test named either interface, and the only evidence that they are shadowed was that they are in a table. A name in a table is a statement about the
// table. The linker matches an import by name AND version, a callback is registered by name only (its signature is whatever the component imports), and
// insecure-seed returns a tuple<u64,u64> where the neighbouring entropy calls return a u64, so "registered" and "shadowed" are different claims, which
// is cleat#4078's lesson about versions arriving a second time.
//
// This is the WIT-level probe the issue asked for, because no standard-library path of the guests that were measured calls these two (Python imports
// both and nothing in the measured paths reaches them): a guest cannot be used to find out. testdata/wasi_minor/insecure-probe-<minor>.wasm is one tiny
// checked-in component per WASI 0.2 minor that calls get-insecure-random-u64, get-insecure-random-bytes(8) and insecure-seed and returns what it read;
// build.sh regenerates them (wasm-tools).
//
// WHAT IS ASSERTED IS THE EFFECT, as in the neighbouring tests: one workflow id reads the same entropy on every run, on every interface. Controls:
//   - a DIFFERENT workflow id must read DIFFERENT entropy on each field, so an interface that returns a constant would not agree with itself;
//   - every field of one run must differ from every other field of it, so one source cannot be served to all four calls.

type insecureProbe struct {
	U64   uint64 `json:"i"`
	Seed0 uint64 `json:"s"`
	Seed1 uint64 `json:"t"`
	Bytes uint64 `json:"b"`
}

func runInsecureProbe(t *testing.T, minor, wfID string) (insecureProbe, error) {
	t.Helper()
	wasm, err := os.ReadFile(filepath.Join("testdata", "wasi_minor", "insecure-probe-"+minor+".wasm"))
	if err != nil {
		t.Fatalf("read the insecure probe component for %s: %v (build.sh regenerates it)", minor, err)
	}
	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	t.Cleanup(func() { rt.Close(ctx) })
	wt, err := NewWasmtimeBackend(ctx)
	if err != nil {
		t.Fatalf("NewWasmtimeBackend: %v", err)
	}
	t.Cleanup(func() { wt.Close(ctx) })

	eng := NewEngine(rt, &mockCaller{},
		WithBackends(WasmtimeLanguages, wt),
		WithWorkflowID(wfID),
		WithWorkflowStartTime(pinnedClockMs),
		WithClock(func() int64 { return pinnedClockMs }),
	)
	res, _, _, _, _, err := eng.Execute(ctx, wasm, "run", []byte(`{}`))
	if err != nil {
		return insecureProbe{}, err
	}
	var p insecureProbe
	if err := json.Unmarshal([]byte(res), &p); err != nil {
		t.Fatalf("the %s probe returned %q, which is not the {\"i\":..,\"s\":..,\"t\":..,\"b\":..} it should: %v", minor, res, err)
	}
	return p, nil
}

func TestAComponentGetsTheWorkflowsInsecureEntropyAtEveryWasiMinor(t *testing.T) {
	for _, minor := range wasiMinors {
		minor := minor
		t.Run(minor, func(t *testing.T) {
			a, err := runInsecureProbe(t, minor, "wf-insecure")
			if err != nil {
				t.Fatalf("the %s probe did not run: %v", minor, err)
			}
			b, err := runInsecureProbe(t, minor, "wf-insecure")
			if err != nil {
				t.Fatalf("the %s probe did not run on its second run: %v", minor, err)
			}
			other, err := runInsecureProbe(t, minor, "wf-insecure-other")
			if err != nil {
				t.Fatalf("the %s probe did not run for the other workflow id: %v", minor, err)
			}

			for _, f := range []struct {
				name           string
				x, y, otherVal uint64
			}{
				{"get-insecure-random-u64", a.U64, b.U64, other.U64},
				{"insecure-seed (first u64)", a.Seed0, b.Seed0, other.Seed0},
				{"insecure-seed (second u64)", a.Seed1, b.Seed1, other.Seed1},
				{"get-insecure-random-bytes(8)", a.Bytes, b.Bytes, other.Bytes},
			} {
				if f.x != f.y {
					t.Errorf("%s read %d on one run of the workflow and %d on the next: it is the host's entropy and not the workflow's", f.name, f.x, f.y)
				}
				if f.x == f.otherVal {
					t.Errorf("%s read %d for two different workflow ids: a constant would agree with itself, so this does not show it is seeded", f.name, f.x)
				}
			}
			// One source must not be served to every call.
			seen := map[uint64]string{}
			for name, v := range map[string]uint64{"u64": a.U64, "seed0": a.Seed0, "seed1": a.Seed1, "bytes": a.Bytes} {
				if prev, dup := seen[v]; dup {
					t.Errorf("%s and %s read the same value %d in one run", prev, name, v)
				}
				seen[v] = name
			}
		})
	}
}
