package engine

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/wasm"
)

// uleb encodes n the way a WASM section header does.
func uleb(n int) []byte {
	var out []byte
	for {
		b := byte(n & 0x7f)
		n >>= 7
		if n != 0 {
			out = append(out, b|0x80)
			continue
		}
		return append(out, b)
	}
}

// stampJSEnginePin appends a cleat.js-engine custom section to a minimal
// module, building the section bytes DIRECTLY rather than through any writer in
// this tree. cmd/cleat-ts-meta is the only writer, and it is in package main, so
// the alternative would be to test the reader against a copy of itself -- a
// fixture that agrees with the parser about a format neither may match.
//
// The fixture is read back before use: a builder that silently produced a
// module with no section would make every "silent" assertion below pass for the
// wrong reason.
func stampJSEnginePin(t *testing.T, pin map[string]any) []byte {
	t.Helper()
	payload, err := json.Marshal(pin)
	if err != nil {
		t.Fatalf("marshaling the pin fixture: %v", err)
	}
	name := []byte("cleat.js-engine")
	body := append(uleb(len(name)), name...)
	body = append(body, payload...)

	out := append([]byte("\x00asm\x01\x00\x00\x00"), 0)
	out = append(out, uleb(len(body))...)
	out = append(out, body...)

	back, found, err := wasm.ReadJSEnginePin(out)
	if err != nil || !found || back == nil {
		t.Fatalf("fixture carries no readable pin (found=%v err=%v); every assertion below would be vacuous", found, err)
	}
	return out
}

// cleat#3830: a built TypeScript module records the JS engine it was built
// against in a cleat.js-engine section, and nothing anywhere read it. This pins
// the comparison, and pins that it WARNS RATHER THAN REFUSES -- the same choice
// cleat#1054 made for abi_version, and the half a future change is most likely
// to break.
//
// It also pins the LIMIT of the check: only pin_version is compared, because
// every other field names something the worker does not carry. If someone later
// adds a comparison against module_javy or plugin_sha256, they will have had to
// introduce a worker-side value for it, and this comment is where to say so.
func TestATypeScriptModuleWithANewerJSEnginePinIsReportedAndStillRuns(t *testing.T) {
	var buf bytes.Buffer
	e := engineLoggingTo(&buf)

	module := stampJSEnginePin(t, map[string]any{
		"pin_version":         wasm.CurrentJSEnginePinVersion + 1,
		"javy":                "javy 9.1.0",
		"rquickjs_core":       "0.12.2",
		"plugin_sha256":       strings.Repeat("a", 64),
		"sdk_sha256":          strings.Repeat("b", 64),
		"deterministic_build": true,
	})

	// No backends registered, so resolveBackend returns (nil, nil): the
	// "this engine does no routing" case. That isolates the pin check from
	// backend selection, exactly as the ABI test does, so a failure here
	// cannot be a routing failure wearing a version warning's clothes.
	backend, err := e.resolveBackend(module)
	if err != nil {
		t.Fatalf("resolveBackend refused a module with a newer JS engine pin: %v\n\n"+
			"The decision on cleat#3830 is WARN, NOT REJECT.", err)
	}
	if backend != nil {
		t.Fatalf("resolveBackend returned a backend with none registered: %v", backend)
	}

	logged := buf.String()
	if !strings.Contains(logged, "newer cleat.js-engine pin") {
		t.Fatalf("no warning about the JS engine pin. Logged: %q", logged)
	}
	// The warning must name both sides of the pair; a message that says only
	// "newer" cannot be acted on by whoever reads it.
	for _, want := range []string{
		`"module_pin_version":2`,
		`"worker_pin_version":1`,
		`"module_javy":"javy 9.1.0"`,
		`"module_rquickjs_core":"0.12.2"`,
	} {
		if !strings.Contains(logged, want) {
			t.Errorf("warning does not carry %s. Logged: %q", want, logged)
		}
	}
}

// The three silences matter as much as the warning: a check that fires on every
// module is noise, and noise is how a real warning gets missed.
func TestJSEnginePinIsSilentWhenThereIsNothingToReport(t *testing.T) {
	cases := []struct {
		name string
		mod  func(t *testing.T) []byte
	}{
		{
			// Every non-TypeScript module, and every TypeScript module built
			// before the pin existed.
			name: "no pin section at all",
			mod:  func(*testing.T) []byte { return []byte("\x00asm\x01\x00\x00\x00") },
		},
		{
			name: "a pin at the version this worker parses",
			mod: func(t *testing.T) []byte {
				return stampJSEnginePin(t, map[string]any{
					"pin_version": wasm.CurrentJSEnginePinVersion,
					"javy":        "javy 9.1.0",
				})
			},
		},
		{
			// Present but unreadable: the reader reports an error and this
			// check stays quiet.
			//
			// NOTHING ELSE REPORTS IT -- cleat.js-engine has exactly one reader,
			// this one, so a module whose pin is corrupt gets no signal at all.
			// That is the price of warn-only, and it is stated rather than
			// justified: the alternative, warning on an unparseable pin, would
			// fire on every module built by a future toolchain whose payload
			// this worker cannot decode -- the exact case pin_version exists to
			// report deliberately, with the version the reader CAN see.
			name: "a pin section that is not JSON",
			mod: func(t *testing.T) []byte {
				name := []byte("cleat.js-engine")
				body := append(uleb(len(name)), name...)
				body = append(body, []byte("not json")...)
				out := append([]byte("\x00asm\x01\x00\x00\x00"), 0)
				out = append(out, uleb(len(body))...)
				return append(out, body...)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			e := engineLoggingTo(&buf)
			if _, err := e.resolveBackend(tc.mod(t)); err != nil {
				t.Fatalf("resolveBackend: %v", err)
			}
			if s := buf.String(); s != "" {
				t.Fatalf("warned when it should have been silent: %q", s)
			}
		})
	}
}
