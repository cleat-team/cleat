package main

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/wasm"
)

// coreModuleHeader is the smallest thing wasm.ReadMetadata/WriteMetadata accept:
// the core-module magic and version. The tests here are about the metadata
// section, not about executing anything, so nothing else is needed.
var coreModuleHeader = []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}

// customSectionWithPayload encodes a custom section named `name` carrying
// `payload` (nil for none).
func customSectionWithPayload(name string, payload []byte) []byte {
	out := []byte{0x00} // custom section id
	body := append(uleb128(uint32(len(name))), []byte(name)...)
	body = append(body, payload...)
	out = append(out, uleb128(uint32(len(body)))...)
	return append(out, body...)
}

func uleb128(v uint32) []byte {
	var out []byte
	for {
		b := byte(v & 0x7f)
		v >>= 7
		if v != 0 {
			out = append(out, b|0x80)
			continue
		}
		return append(out, b)
	}
}

// artifactWithMetadata returns a core module carrying cleat.metadata written
// through the Go struct, plus a trailing custom section so a caller can produce
// two artifacts that differ in their BODY while sharing a metadata stamp --
// which is what a rebuild at an unchanged `cleat build --version` produces.
func artifactWithMetadata(t *testing.T, meta *wasm.Metadata, bodyMarker string) []byte {
	t.Helper()
	b, err := wasm.WriteMetadata(coreModuleHeader, meta)
	if err != nil {
		t.Fatalf("WriteMetadata: %v", err)
	}
	if bodyMarker == "" {
		return b
	}
	return append(b, customSectionWithPayload(bodyMarker, nil)...)
}

// artifactWithRawMetadata returns a core module whose cleat.metadata payload is
// exactly `payload`. A test needs this to include keys wasm.Metadata does not
// model, which WriteMetadata cannot produce by construction.
func artifactWithRawMetadata(t *testing.T, payload string) []byte {
	t.Helper()
	b := append([]byte{}, coreModuleHeader...)
	return append(b, customSectionWithPayload("cleat.metadata", []byte(payload))...)
}

// TestDeployWorkflow_RestampsTheBinaryToTheVersionItRecords is the regression
// test for cleat#2944.
//
// `cleatctl deploy workflow` is documented as "deploys a new version"
// (docs/explanation/workflow-versioning.md:251) and assigns MAX(version)+1. It
// used to store the binary byte-for-byte, so the cleat.metadata stamp inside it
// kept whatever `cleat build --version` wrote -- 1 by default. A rebuilt
// artifact therefore produced a v2 row carrying a v1 stamp, and
// cmd/cleat-worker's pre-flight releases a run whose binary reports a different
// workflow_version from the row it was queued against, on every claim, so the
// run looped between claim and release and never executed.
//
// The input must differ from the stored row in its BODY, not just its version:
// an identical file is caught by the dedup guard and never reaches the
// insert. (Until cleat#2947 that guard compared against
// WorkflowDef.WASMBytes, which no dialect's ListWorkflowDefs selects -- so it
// never fired against a real store and this input reached the insert for a
// reason other than the one stated here.)
func TestDeployWorkflow_RestampsTheBinaryToTheVersionItRecords(t *testing.T) {
	dir := t.TempDir()

	meta := &wasm.Metadata{
		WorkflowName:         "provision",
		WorkflowVersion:      1,
		ABIVersion:           wasm.CurrentABIVersion,
		MinCompatibleVersion: wasm.CurrentABIVersion,
		Language:             "go",
	}
	// v1 as an old deploy stored it: the same stamp, a different body.
	deployedV1 := artifactWithMetadata(t, meta, "body-v1")
	// What a rebuild at the default --version 1 produces: same stamp, new body.
	rebuilt := artifactWithMetadata(t, meta, "body-v2")
	path := writeWASM(t, dir, rebuilt)

	var capturedDef *engine.WorkflowDef
	store := &mockStore{
		listWorkflowDefsFn: func(_ context.Context, name string) ([]engine.WorkflowDef, error) {
			return []engine.WorkflowDef{
				{Name: name, Version: 1, ABIVersion: wasm.CurrentABIVersion,
					CreatedAt: time.Now().Add(-24 * time.Hour)},
			}, nil
		},
		// Where the dedup guard reads the stored bytes since cleat#2947.
		// Supplied so the guard genuinely compares and genuinely finds a
		// difference -- the premise stated above. Left out, the deploy would
		// still proceed, but because LoadWASM returned nothing rather than
		// because the bodies differ.
		loadWASMFn: func(_ context.Context, _ string, version int) ([]byte, error) {
			if version != 1 {
				t.Errorf("LoadWASM asked for v%d, want the latest (v1)", version)
			}
			return deployedV1, nil
		},
		deployWorkflowDefFn: func(_ context.Context, def *engine.WorkflowDef) error {
			capturedDef = def
			return nil
		},
	}

	captureStdout(t, func() {
		deployWorkflow(context.Background(), store, nil, []string{"provision", path})
	})

	if capturedDef == nil {
		t.Fatal("expected DeployWorkflowDef to be called: the bodies differ, so this is not a redeploy of the same artifact")
	}
	if capturedDef.Version != 2 {
		t.Fatalf("Version = %d, want 2: MAX(version)+1 is the documented contract", capturedDef.Version)
	}

	got, err := wasm.ReadMetadata(capturedDef.WASMBytes)
	if err != nil {
		t.Fatalf("the stored binary has no readable metadata: %v", err)
	}
	if got.WorkflowVersion != capturedDef.Version {
		t.Errorf("stored binary reports workflow_version %d but the row is v%d.\n\n"+
			"cmd/cleat-worker's pre-flight compares exactly these two values and releases "+
			"the run for another worker when they differ -- on every claim, so the run never "+
			"executes. Restamp the binary to the version this command assigns. cleat#2944.",
			got.WorkflowVersion, capturedDef.Version)
	}

	// The restamp must touch only the stamp: everything else about the artifact,
	// including its own trailing sections, is what the user built.
	rest := capturedDef.WASMBytes
	if !bytes.Contains(rest, []byte("body-v2")) {
		t.Error("the stored binary lost its body section; the restamp rewrote more than cleat.metadata")
	}
	if bytes.Contains(rest, []byte("body-v1")) {
		t.Error("the stored binary carries the OLD body section; the wrong artifact was stored")
	}
}

// TestRestampWorkflowVersionPreservesKeysItDoesNotModel is the regression test
// for the second defect cleat-review found on this PR: the restamp originally
// round-tripped through wasm.Metadata, which models only the keys the engine
// reads. stamp_metadata.py writes sdk_language, sdk_version and created_at as
// well, and Rust/Java/AssemblyScript inject sdk_version, so every restamp
// dropped them -- "restamps workflow_version" rewrote the whole section.
func TestRestampWorkflowVersionPreservesKeysItDoesNotModel(t *testing.T) {
	payload := `{"workflow_name":"provision","workflow_version":1,"abi_version":1,` +
		`"min_compatible_version":1,"plugin_deps":{},"entry_points":["place_order"],` +
		`"sdk_language":"python","sdk_version":"0.4.0","created_at":"2026-10-02T00:00:00Z"}`
	built := artifactWithRawMetadata(t, payload)

	got, err := restampWorkflowVersion(built, 2)
	if err != nil {
		t.Fatalf("restampWorkflowVersion: %v", err)
	}

	meta, err := wasm.ReadMetadata(got)
	if err != nil {
		t.Fatalf("the restamped artifact has no readable metadata: %v", err)
	}
	if meta.WorkflowVersion != 2 {
		t.Errorf("workflow_version = %d, want 2", meta.WorkflowVersion)
	}

	// Asserted as raw key:value pairs in the stored payload, because
	// wasm.ReadMetadata cannot see them -- which is precisely why they were
	// being lost. Values survive SEMANTICALLY, not byte-for-byte: the payload is
	// re-encoded, so key ORDER changes and interior whitespace is compacted
	// (which is why every expected string below is written without spaces and a
	// whole-payload comparison would be a false failure).
	for _, want := range []string{
		`"sdk_language":"python"`,
		`"sdk_version":"0.4.0"`,
		`"created_at":"2026-10-02T00:00:00Z"`,
		`"entry_points":["place_order"]`,
		`"workflow_name":"provision"`,
		// Modelled by the struct, but tagged omitempty -- so a round-trip drops
		// it when it is present-and-empty, a different mechanism from the three
		// above and worth its own case.
		`"plugin_deps":{}`,
	} {
		if !bytes.Contains(got, []byte(want)) {
			t.Errorf("the restamped artifact lost %s.\n\n"+
				"wasm.Metadata models only the keys the engine reads; rebuilding the payload "+
				"from that struct rewrites the whole section and drops every key it does not "+
				"carry. Patch the one key instead. cleat#2944.", want)
		}
	}
}

// TestRestampWorkflowVersionLeavesAnUnstampedArtifactAlone covers the case that
// makes this safe to land independently of cleat#2941: a binary with no readable
// cleat.metadata is stored exactly as it is. There is no stamp to disagree with,
// and the worker's pre-flight reads that same metadata, so it cannot fire.
//
// A Python component on a tree without #2941 is precisely this case --
// wasm.ReadMetadata rejects it -- which is why this fix does not need #2941 to
// be correct, only to extend to Python.
func TestRestampWorkflowVersionLeavesAnUnstampedArtifactAlone(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []byte
	}{
		{"a core module with no metadata", coreModuleHeader},
		{"a Component Model header", []byte{0x00, 0x61, 0x73, 0x6d, 0x0d, 0x00, 0x01, 0x00}},
		{"a core module with a non-metadata section", append(append([]byte{}, coreModuleHeader...), customSectionWithPayload("other", nil)...)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := restampWorkflowVersion(tc.in, 7)
			if err != nil {
				t.Fatalf("restampWorkflowVersion returned an error for an unstamped artifact: %v", err)
			}
			if string(out) != string(tc.in) {
				t.Errorf("the artifact was modified although it carries no readable metadata:\ngot  %x\nwant %x", out, tc.in)
			}
		})
	}
}

// The version is written as a JSON number, not a string -- wasm.Metadata's
// WorkflowVersion is an int, and a quoted value would fail to unmarshal and
// take the whole definition's metadata with it.
func TestRestampWorkflowVersionWritesANumberNotAString(t *testing.T) {
	built := artifactWithRawMetadata(t, `{"workflow_name":"provision","workflow_version":1}`)

	got, err := restampWorkflowVersion(built, 12)
	if err != nil {
		t.Fatalf("restampWorkflowVersion: %v", err)
	}
	if !bytes.Contains(got, []byte(`"workflow_version":12`)) {
		t.Errorf("expected an unquoted 12 in the payload; got %s", got[8:])
	}
	meta, err := wasm.ReadMetadata(got)
	if err != nil {
		t.Fatalf("ReadMetadata on the restamped artifact: %v", err)
	}
	if meta.WorkflowVersion != 12 {
		t.Errorf("WorkflowVersion = %d, want 12", meta.WorkflowVersion)
	}
}

// TestDeployWorkflow_StoresANonObjectMetadataPayloadUnchanged is the regression
// test for the panic cleat-review found on this PR's second head.
//
// A cleat.metadata payload of the JSON literal `null` parses to a zero
// wasm.Metadata, which reports version 0 -- not the version being assigned -- so
// the restamp proceeded and SetMetadataField assigned into the nil map
// json.Unmarshal leaves behind for `null`. That panics with "assignment to entry
// in nil map", from `cleatctl deploy`, where develop stored the artifact as-is.
//
// The row is still created; what must not happen is a crash.
func TestDeployWorkflow_StoresANonObjectMetadataPayloadUnchanged(t *testing.T) {
	dir := t.TempDir()
	built := artifactWithRawMetadata(t, `null`)
	path := writeWASM(t, dir, built)

	var capturedDef *engine.WorkflowDef
	store := &mockStore{
		listWorkflowDefsFn: func(_ context.Context, _ string) ([]engine.WorkflowDef, error) {
			return nil, nil
		},
		deployWorkflowDefFn: func(_ context.Context, def *engine.WorkflowDef) error {
			capturedDef = def
			return nil
		},
	}

	captureStdout(t, func() {
		deployWorkflow(context.Background(), store, nil, []string{"provision", path})
	})

	if capturedDef == nil {
		t.Fatal("expected DeployWorkflowDef to be called")
	}
	if string(capturedDef.WASMBytes) != string(built) {
		t.Error("a cleat.metadata payload that is not a JSON object has no key to patch, so the " +
			"binary must be stored exactly as built -- which is also what develop did")
	}
}

// artifactWithZeroStamp is what every standalone stamper leaves when it is given
// no version: a well-formed cleat.metadata object whose workflow_version is 0.
// Java's scripts/inject-metadata.sh defaults it with `${CLEAT_WORKFLOW_VERSION:-0}`,
// AssemblyScript's inject-metadata.js with a trailing `: 0`, Rust's
// inject_metadata.rs through an env fallback, and stamp_metadata.py the same way
// (which is cleat#2941's B1). Non-zero abi/min so only the version is degenerate.
func artifactWithZeroStamp(t *testing.T) []byte {
	t.Helper()
	return artifactWithRawMetadata(t, `{"workflow_name":"provision","workflow_version":0,`+
		`"abi_version":1,"min_compatible_version":1,"plugin_deps":{},"sdk_language":"java"}`)
}

// TestRestampWorkflowVersionRestampsAZeroStamp is the regression test for
// cleat-review's B3 on this PR's third head.
//
// An earlier version of restampWorkflowVersion returned early for any stamp that
// was not positive, on the analogy of `cleat deploy` -- which asks whether to
// RECORD the stamp, and rightly declines 0 because a version of 0 cannot be
// recorded. The question here is different: does the stored binary agree with the
// row? A 0 stamp is the case that most needs changing, not the case to skip, and
// skipping it re-opened cleat#2944 for the single commonest unversioned build.
func TestRestampWorkflowVersionRestampsAZeroStamp(t *testing.T) {
	built := artifactWithZeroStamp(t)

	got, err := restampWorkflowVersion(built, 2)
	if err != nil {
		t.Fatalf("restampWorkflowVersion: %v", err)
	}
	if string(got) == string(built) {
		t.Fatal("a 0 stamp was left alone; cmd/cleat-worker's pre-flight compares with `!=` and " +
			"exempts nothing, so this binary is released on every claim and never runs")
	}

	meta, err := wasm.ReadMetadata(got)
	if err != nil {
		t.Fatalf("the restamped artifact has no readable metadata: %v", err)
	}
	if meta.WorkflowVersion != 2 {
		t.Errorf("workflow_version = %d, want 2", meta.WorkflowVersion)
	}
	// The keys a 0-stamped build carries must survive exactly as in the 1 case.
	if !bytes.Contains(got, []byte(`"sdk_language":"java"`)) {
		t.Error("restamping a 0 stamp dropped sdk_language")
	}
}

// The same case through `cleatctl deploy`, which is where cleat-review measured
// it: byte-patch a stamp to 0, deploy, and the stored binary must report the
// version the row records rather than 0.
func TestDeployWorkflow_RestampsAZeroStamp(t *testing.T) {
	dir := t.TempDir()
	zeroed := artifactWithZeroStamp(t)
	path := writeWASM(t, dir, zeroed)

	var capturedDef *engine.WorkflowDef
	store := &mockStore{
		listWorkflowDefsFn: func(_ context.Context, _ string) ([]engine.WorkflowDef, error) {
			return nil, nil
		},
		deployWorkflowDefFn: func(_ context.Context, def *engine.WorkflowDef) error {
			capturedDef = def
			return nil
		},
	}

	captureStdout(t, func() {
		deployWorkflow(context.Background(), store, nil, []string{"provision", path})
	})

	if capturedDef == nil {
		t.Fatal("expected DeployWorkflowDef to be called")
	}
	got, err := wasm.ReadMetadata(capturedDef.WASMBytes)
	if err != nil {
		t.Fatalf("the stored binary has no readable metadata: %v", err)
	}
	if got.WorkflowVersion != capturedDef.Version {
		t.Errorf("stored binary reports workflow_version %d but the row is v%d.\n\n"+
			"A 0 stamp is the default of every standalone stamper given no version, and the "+
			"worker's pre-flight exempts nothing, so this run is released on every claim and "+
			"never executes. cleat#2944.", got.WorkflowVersion, capturedDef.Version)
	}
}
