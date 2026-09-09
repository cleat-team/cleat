package engine

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/wasm"
)

// stampABI returns a minimal module declaring the given host ABI version.
//
// The fixture is checked rather than trusted: a WriteMetadata that silently
// failed to stamp would leave every assertion below passing for the wrong
// reason, since the warning's absence is what half of them look for.
func stampABI(t *testing.T, abi int, lang string) []byte {
	t.Helper()
	out, err := wasm.WriteMetadata([]byte("\x00asm\x01\x00\x00\x00"), &wasm.Metadata{
		WorkflowName: "orders", WorkflowVersion: 7, ABIVersion: abi, Language: lang,
	})
	if err != nil {
		t.Fatalf("WriteMetadata: %v", err)
	}
	back, err := wasm.ReadMetadata(out)
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	if back.ABIVersion != abi {
		t.Fatalf("fixture stamped ABIVersion %d, want %d", back.ABIVersion, abi)
	}
	return out
}

// engineLoggingTo returns an Engine whose warnings land in buf.
func engineLoggingTo(buf *bytes.Buffer) *Engine {
	return NewEngine(nil, &mockCaller{}, WithLogger(slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn}))))
}

// cleat#1054: abi_version was stamped at build, stored per definition, returned
// by the API and never compared against the running worker. The field that
// would name a mismatch was in the row, read, and discarded.
//
// This pins the comparison, and pins that it WARNS RATHER THAN REFUSES. Both
// halves matter and the second is the one a future change is likely to break:
// #1054 proposed refusing, and turning this into a refusal is a behaviour
// change for every guest, not a tightening of a message.
func TestAModuleBuiltAgainstANewerABIIsReportedAndStillRuns(t *testing.T) {
	var buf bytes.Buffer
	e := engineLoggingTo(&buf)

	// No backends registered: resolveBackend returns (nil, nil), the
	// "this engine does no routing" case. That is deliberate -- it isolates
	// the version check from backend selection, so a failure here cannot be
	// a routing failure wearing a version warning's clothes.
	backend, err := e.resolveBackend(stampABI(t, wasm.CurrentABIVersion+1, "go"))
	if err != nil {
		t.Fatalf("resolveBackend refused a module built against a newer ABI: %v\n\n"+
			"The decision on cleat#1054 is WARN, NOT REJECT. A refusal here breaks "+
			"every guest that declares a version this worker does not know, which "+
			"is the opposite of what a version field is for while there is exactly "+
			"one ABI.", err)
	}
	if backend != nil {
		t.Fatalf("expected no backend from an engine with none registered, got %T", backend)
	}

	line := buf.String()
	if !strings.Contains(line, "newer host ABI") {
		t.Fatalf("a module declaring ABI %d produced no warning against a worker at %d.\n\ngot: %s",
			wasm.CurrentABIVersion+1, wasm.CurrentABIVersion, line)
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &rec); err != nil {
		t.Fatalf("warning was not one JSON record: %v\n%s", err, line)
	}
	// Both numbers must be named. A warning that says "mismatch" without them
	// sends the reader to find what it already knew.
	if got := rec["module_abi_version"]; got != float64(wasm.CurrentABIVersion+1) {
		t.Errorf("module_abi_version = %v, want %d", got, wasm.CurrentABIVersion+1)
	}
	if got := rec["worker_abi_version"]; got != float64(wasm.CurrentABIVersion) {
		t.Errorf("worker_abi_version = %v, want %d", got, wasm.CurrentABIVersion)
	}
	if got := rec["workflow_name"]; got != "orders" {
		t.Errorf("workflow_name = %v, want \"orders\" -- without it the operator cannot tell which definition to rebuild", got)
	}
}

// The other half, and the one that makes the check worth having rather than
// merely present: a module at the CURRENT version must be silent. A warning
// that fires for everything is noise an operator learns to skip, which is the
// same end state as not warning at all.
func TestAModuleAtTheCurrentABIIsSilent(t *testing.T) {
	var buf bytes.Buffer
	e := engineLoggingTo(&buf)
	if _, err := e.resolveBackend(stampABI(t, wasm.CurrentABIVersion, "go")); err != nil {
		t.Fatalf("resolveBackend: %v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("a module at the current ABI warned:\n%s", buf.String())
	}
}

// An OLDER module must also be silent. This is the case a naive `!=` comparison
// gets wrong, and it is the realistic one: after a second ABI ships, every
// guest built before it is older, so a `!=` would warn about the entire
// installed base on every execution.
func TestAModuleBuiltAgainstAnOlderABIIsSilent(t *testing.T) {
	if wasm.CurrentABIVersion <= 1 {
		// Manufacture the case rather than skipping: a module stamped 0 is
		// older than 1, and 0 is also what an unstamped build path leaves
		// behind (cleat#1079), so this doubles as a guard that the check does
		// not shout about those.
		var buf bytes.Buffer
		e := engineLoggingTo(&buf)
		if _, err := e.resolveBackend(stampABI(t, 0, "go")); err != nil {
			t.Fatalf("resolveBackend: %v", err)
		}
		if buf.Len() != 0 {
			t.Fatalf("a module stamped ABI 0 warned; an older or unstamped module is not a newer one:\n%s", buf.String())
		}
		return
	}
	var buf bytes.Buffer
	e := engineLoggingTo(&buf)
	if _, err := e.resolveBackend(stampABI(t, wasm.CurrentABIVersion-1, "go")); err != nil {
		t.Fatalf("resolveBackend: %v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("a module at an older ABI warned:\n%s", buf.String())
	}
}
