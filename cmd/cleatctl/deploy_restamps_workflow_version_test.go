package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/wasm"
)

// coreModuleHeader is the smallest thing wasm.ReadMetadata/WriteMetadata accept:
// the core-module magic and version. The tests here are about the metadata
// section, not about executing anything, so nothing else is needed.
var coreModuleHeader = []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}

// artifactWithMetadata returns a core module carrying cleat.metadata, plus the
// bytes of a trailing custom section so a caller can produce two artifacts that
// differ in their BODY while sharing a metadata stamp -- which is what a rebuild
// at an unchanged `cleat build --version` actually produces.
func artifactWithMetadata(t *testing.T, meta *wasm.Metadata, bodyMarker string) []byte {
	t.Helper()
	b, err := wasm.WriteMetadata(coreModuleHeader, meta)
	if err != nil {
		t.Fatalf("WriteMetadata: %v", err)
	}
	if bodyMarker == "" {
		return b
	}
	return append(b, customSectionBytes(bodyMarker)...)
}

// customSectionBytes encodes a custom section named bodyMarker with no payload.
func customSectionBytes(name string) []byte {
	out := []byte{0x00} // custom section id
	body := append(uleb128(uint32(len(name))), []byte(name)...)
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
// The subtlety this test is built around: the input must differ from the stored
// row in its BODY, not just its version. An identical file is caught by the
// dedup guard and never reaches the insert -- which is a different behaviour,
// covered by TestDeployWorkflow_SkipsARedeployOfTheSameArtifactAfterRestamping.
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
					WASMBytes: deployedV1, CreatedAt: time.Now().Add(-24 * time.Hour)},
			}, nil
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

// TestDeployWorkflow_SkipsARedeployOfTheSameArtifactAfterRestamping pins the
// other half of cleat#2944: the guard in deployWorkflow's doc comment -- "If an
// exact version already exists with the same SHA256 hash, the deployment is
// skipped" -- is documented behaviour, and restamping gives the stored bytes a
// version the file on disk does not have.
//
// Compared on raw bytes, those two never match again and the guard silently
// dies, turning an accidental double-deploy into a spurious new version. The
// comparison is normalised for exactly this case: the row below holds the
// artifact as a fixed deploy WOULD store it (stamped v2), the input is the same
// artifact as built (stamped v1).
func TestDeployWorkflow_SkipsARedeployOfTheSameArtifactAfterRestamping(t *testing.T) {
	dir := t.TempDir()

	meta := &wasm.Metadata{
		WorkflowName:         "provision",
		WorkflowVersion:      1,
		ABIVersion:           wasm.CurrentABIVersion,
		MinCompatibleVersion: wasm.CurrentABIVersion,
		Language:             "go",
	}
	asBuilt := artifactWithMetadata(t, meta, "")
	path := writeWASM(t, dir, asBuilt)

	stampedV2, err := restampWorkflowVersion(asBuilt, 2)
	if err != nil {
		t.Fatalf("restampWorkflowVersion: %v", err)
	}
	if stampedV2 == nil || string(stampedV2) == string(asBuilt) {
		t.Fatal("restampWorkflowVersion did not change the artifact, so this test is not exercising the stamped case")
	}

	store := &mockStore{
		listWorkflowDefsFn: func(_ context.Context, name string) ([]engine.WorkflowDef, error) {
			return []engine.WorkflowDef{
				{Name: name, Version: 2, ABIVersion: wasm.CurrentABIVersion,
					WASMBytes: stampedV2, CreatedAt: time.Now()},
			}, nil
		},
		deployWorkflowDefFn: func(_ context.Context, def *engine.WorkflowDef) error {
			t.Errorf("DeployWorkflowDef called for v%d: the same artifact was already deployed as "+
				"v2 and the doc comment says that is skipped. Comparing raw bytes breaks the "+
				"moment stored bytes carry a version the file does not. cleat#2944.", def.Version)
			return nil
		},
	}

	stdout := captureStdout(t, func() {
		deployWorkflow(context.Background(), store, nil, []string{"provision", path})
	})
	if !strings.Contains(stdout, "WASM unchanged") {
		t.Errorf("expected 'WASM unchanged' in stdout, got: %s", stdout)
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

// TestContentFingerprintIgnoresOnlyTheWorkflowVersion bounds the normalisation:
// it must ignore the version and nothing else, or the dedup guard would start
// treating genuinely different artifacts as the same one.
func TestContentFingerprintIgnoresOnlyTheWorkflowVersion(t *testing.T) {
	base := func(version int, marker string) []byte {
		return artifactWithMetadata(t, &wasm.Metadata{
			WorkflowName:         "provision",
			WorkflowVersion:      version,
			ABIVersion:           wasm.CurrentABIVersion,
			MinCompatibleVersion: wasm.CurrentABIVersion,
			Language:             "go",
		}, marker)
	}
	renamed := func(version int, marker string) []byte {
		return artifactWithMetadata(t, &wasm.Metadata{
			WorkflowName:         "something-else",
			WorkflowVersion:      version,
			ABIVersion:           wasm.CurrentABIVersion,
			MinCompatibleVersion: wasm.CurrentABIVersion,
			Language:             "go",
		}, marker)
	}

	if contentFingerprint(base(1, "")) != contentFingerprint(base(9, "")) {
		t.Error("two artifacts differing only in workflow_version must fingerprint equal, " +
			"or the dedup guard dies the moment a deploy restamps")
	}
	if contentFingerprint(base(1, "")) == contentFingerprint(base(1, "body")) {
		t.Error("two artifacts with different bodies must fingerprint differently")
	}
	if contentFingerprint(base(1, "")) == contentFingerprint(renamed(1, "")) {
		t.Error("two artifacts with different metadata must fingerprint differently")
	}
	if contentFingerprint(base(1, "")) == contentFingerprint(coreModuleHeader) {
		t.Error("an artifact with metadata must fingerprint differently from one without")
	}
}
