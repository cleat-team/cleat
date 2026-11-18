package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// cleat#4078. The engine shadows wasi:clocks/wall-clock and wasi:random/* with the durable clock and the seeded source (cleat#1410), but the
// linker matches an import name EXACTLY, version included, and the shadows used to be registered only at @0.2.9 -- the version componentize-py
// emits. A component whose toolchain imports another 0.2 minor (a C# component built with componentize-dotnet imports @0.2.6) was handed the
// host's REAL clock and entropy, with no error, and its DateTime.UtcNow, Random and Guid differed on every replay.
//
// Python is guarded by TestAPythonGuestGetsTheDurableClock, which needs a Python toolchain and so cannot cover a version no Python build emits.
// This test needs no toolchain: testdata/wasi_minor holds one tiny checked-in component per minor, each importing wall-clock and random at
// exactly that version and returning what it read. build.sh regenerates them (wasm-tools).
//
// WHAT IS ASSERTED IS THE EFFECT, as in the Python test: the guest's clock reads the pinned durable clock, and its entropy is the same across two
// runs of one workflow id. Two controls make that mean something:
//
//   - 0.2.9, the version the engine already served, is in the set and must pass: if it does not, the harness is broken, not the shadows.
//   - a DIFFERENT workflow id must produce DIFFERENT entropy: an "entropy" the guest reads as a constant would otherwise agree with itself.
//
// Measured before the fix (shadows at @0.2.9 only), the other four minors failed; after it, all five pass.

var wasiMinors = []string{"0.2.0", "0.2.3", "0.2.6", "0.2.9", "0.2.10"}

type wasiProbe struct {
	Seconds uint64 `json:"s"`
	Random  uint64 `json:"r"`
}

func runWasiMinorProbe(t *testing.T, minor, wfID string) wasiProbe {
	t.Helper()
	wasm, err := os.ReadFile(filepath.Join("testdata", "wasi_minor", "probe-"+minor+".wasm"))
	if err != nil {
		t.Fatalf("read the probe component for %s: %v (build.sh regenerates it)", minor, err)
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
		t.Fatalf("execute the %s probe: %v", minor, err)
	}
	var p wasiProbe
	if err := json.Unmarshal([]byte(res), &p); err != nil {
		t.Fatalf("the %s probe returned %q, which is not the {\"s\":..,\"r\":..} it should: %v", minor, res, err)
	}
	return p
}

func TestAComponentGetsTheDurableClockAndEntropyAtEveryWasiMinor(t *testing.T) {
	// The probe's wall-clock read in seconds, from the pinned durable clock.
	wantSeconds := uint64(pinnedClockMs / 1000)

	for _, minor := range wasiMinors {
		minor := minor
		t.Run(minor, func(t *testing.T) {
			a := runWasiMinorProbe(t, minor, "wf-wasi-minor")
			b := runWasiMinorProbe(t, minor, "wf-wasi-minor")
			other := runWasiMinorProbe(t, minor, "wf-wasi-minor-other")

			// CONTROL: entropy exists and is keyed on the workflow id.
			if a.Random == other.Random {
				t.Fatalf("PRECONDITION FAILED: two DIFFERENT workflow ids gave the same entropy %d, so this probe's random is not "+
					"entropy and 'the same across replays' below would pass for nothing.", a.Random)
			}
			if a.Seconds != wantSeconds {
				t.Errorf("the guest's wasi:clocks/wall-clock@%s reads %d seconds, want %d (the pinned durable clock).\n\n"+
					"The linker matches the import name EXACTLY, version included. If the shadows are registered at another minor "+
					"this guest got the host's real clock, and a replay diverges. cleat#4078, cleat#1410. If the value is about "+
					"the real current time, that is what happened.", minor, a.Seconds, wantSeconds)
			}
			if a != b {
				t.Errorf("two runs of ONE workflow id disagree for @%s: %+v vs %+v. A replay would diverge: this guest is getting the host's real "+
					"entropy or clock. cleat#4078.", minor, a, b)
			}
		})
	}
}

// wasiDeterminismVersionsOf has to find what a component imports, and has to be harmless on what it is not given.
func TestWasiDeterminismVersionsOf(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "wasi_minor", "probe-*.wasm"))
	if err != nil {
		t.Fatal(err)
	}
	// A glob that matches nothing makes every loop below pass vacuously.
	if len(files) != len(wasiMinors) {
		t.Fatalf("found %d probe components %v, want %d (%v)", len(files), files, len(wasiMinors), wasiMinors)
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		minor := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(f), "probe-"), ".wasm")
		got := wasiDeterminismVersionsOf(b)
		has := func(v string) bool {
			for _, g := range got {
				if g == v {
					return true
				}
			}
			return false
		}
		if !has("@"+minor) || !has(wasiDeterminismVersion) {
			t.Errorf("%s: versions %v must contain the component's own @%s and the default %s", f, got, minor, wasiDeterminismVersion)
		}
		if minor != "0.2.9" && len(got) != 2 {
			t.Errorf("%s: versions %v, want exactly the default and @%s", f, got, minor)
		}
	}

	// A later WASI generation is a different set of function shapes: shadowing it with 0.2 callbacks would be wrong, so
	// the pattern must not match it.
	for _, imp := range []string{"wasi:clocks/wall-clock@0.3.0", "wasi:random/random@1.0.0"} {
		if m := wasiDeterminismImportRE.FindSubmatch([]byte(imp)); m != nil {
			t.Errorf("%s matched as version %q: only 0.2.x may be shadowed", imp, m[1])
		}
	}
	if m := wasiDeterminismImportRE.FindSubmatch([]byte("wasi:clocks/wall-clock@0.2.17")); m == nil || string(m[1]) != "0.2.17" {
		t.Errorf("a 0.2.17 import was not matched: %v", m)
	}

	// Not a component, empty, truncated, and a lying section length: the default alone, and no panic.
	for name, in := range map[string][]byte{
		"nil":           nil,
		"empty":         {},
		"core module":   []byte("\x00asm\x01\x00\x00\x00"),
		"truncated":     []byte("\x00asm\x0d\x00\x01\x00\x0a"),
		"lying length":  []byte("\x00asm\x0d\x00\x01\x00\x0a\xff\xff\xff\xff\x0f"),
		"garbage bytes": []byte("not wasm at all"),
	} {
		got := wasiDeterminismVersionsOf(in)
		if len(got) != 1 || got[0] != wasiDeterminismVersion {
			t.Errorf("%s: got %v, want only the default %s", name, got, wasiDeterminismVersion)
		}
	}
}
