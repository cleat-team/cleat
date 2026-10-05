package main

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/wasm"
)

// cleat#1986, the deploy half: slice 2a made `--exposure` the way to set a
// definition's class and refused `public`; slice 2c-ii added the artifact's
// SOURCE declaration and the tighten-only rule around it.
//
// The two halves are tested in this one file because they are one decision at
// one call site: `--exposure` is now an OPINION that may tighten the declared
// class, and the empty default is what lets the declaration stand.
//
// Driven through deployWorkflow directly with a nil *sql.DB, which is what the
// neighbouring deploy tests do -- the `--db` / CLEAT_DB_URL check happens in the
// command dispatcher BEFORE this function, so a `cleatctl deploy ...` invocation
// with a bad --exposure exits on the missing database and never reaches the
// validation. Measured the hard way: a `go run` of exactly that printed the
// database error, which reads like the validation passing when it means the
// validation never ran.

const bareWASMExposureTest = "\x00asm\x01\x00\x00\x00"

func deploymentFor(t *testing.T, args ...string) *engine.WorkflowDef {
	t.Helper()
	dir := t.TempDir()
	path := writeWASM(t, dir, []byte(bareWASMExposureTest))

	var captured *engine.WorkflowDef
	store := &mockStore{
		deployWorkflowDefFn: func(_ context.Context, def *engine.WorkflowDef) error {
			captured = def
			return nil
		},
	}
	withExitPanic(t, func() {
		deployWorkflow(context.Background(), store, nil, append([]string{"exposure-wf", path}, args...))
	})
	if captured == nil {
		t.Fatalf("DeployWorkflowDef was never called for args %v", args)
	}
	return captured
}

// The default is the class whose meaning is "what the server already does", so a
// deploy that says nothing changes nothing.
func TestDeployWorkflowDefaultsToAuthExposure(t *testing.T) {
	if got := deploymentFor(t).Exposure; got != engine.ExposureAuth {
		t.Errorf("Exposure = %q, want %q for a deploy with no --exposure", got, engine.ExposureAuth)
	}
}

// The flag reaches the definition -- asserted on the struct the store is handed,
// not on the flag variable, so a wiring break between them is visible.
func TestDeployWorkflowCarriesTheRequestedExposure(t *testing.T) {
	if got := deploymentFor(t, "--exposure", "internal").Exposure; got != engine.ExposureInternal {
		t.Errorf("Exposure = %q, want %q -- --exposure did not reach the definition", got, engine.ExposureInternal)
	}
}

// 'public' is refused, and the refusal must NAME what is missing. A definition
// stored as public today becomes world-readable the moment enforcement lands,
// so accepting it would be a time bomb rather than a permissive default.
func TestDeployWorkflowRefusesPublicExposure(t *testing.T) {
	dir := t.TempDir()
	path := writeWASM(t, dir, []byte(bareWASMExposureTest))

	deployed := false
	store := &mockStore{
		deployWorkflowDefFn: func(_ context.Context, _ *engine.WorkflowDef) error {
			deployed = true
			return nil
		},
	}
	stderr := withExitPanic(t, func() {
		deployWorkflow(context.Background(), store, nil, []string{"wf", path, "--exposure", "public"})
	})
	if deployed {
		t.Error("the definition was deployed despite --exposure public being refused")
	}
	if !strings.Contains(stderr, "opt-in") {
		t.Errorf("the refusal does not name the missing per-tenant opt-in, so a reader cannot tell why:\n%s", stderr)
	}
}

// An unknown class is refused too, and the message lists the accepted values --
// otherwise the database's CHECK rejects it later with a message about a
// constraint rather than about the flag the user typed.
func TestDeployWorkflowRefusesAnUnknownExposure(t *testing.T) {
	dir := t.TempDir()
	path := writeWASM(t, dir, []byte(bareWASMExposureTest))

	stderr := withExitPanic(t, func() {
		deployWorkflow(context.Background(), &mockStore{}, nil, []string{"wf", path, "--exposure", "sideways"})
	})
	for _, want := range []string{"auth", "public", "internal"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the refusal does not list the accepted value %q:\n%s", want, stderr)
		}
	}
}

// ---- cleat#1986 slice 2c-ii: the artifact's declaration, and tighten-only ----

// artifactDeclaring returns the bytes of a valid module whose cleat.metadata
// carries the given exposure class, which is what `cleat build` now stamps for
// a source that declares one.
func artifactDeclaring(t *testing.T, declared string) []byte {
	t.Helper()
	out, err := wasm.WriteMetadata([]byte(bareWASMExposureTest), &wasm.Metadata{Exposure: declared})
	if err != nil {
		t.Fatalf("WriteMetadata(exposure=%q): %v", declared, err)
	}
	// The control for every test below: if the stamp is not readable back, the
	// "declared" half of each assertion is vacuous and the tests would be
	// exercising the no-declaration path while claiming the opposite.
	back, err := wasm.ReadMetadata(out)
	if err != nil {
		t.Fatalf("the artifact this helper built has no readable metadata: %v", err)
	}
	if back.Exposure != declared {
		t.Fatalf("the artifact declares %q, want %q -- the helper is not building what the tests assume",
			back.Exposure, declared)
	}
	return out
}

// deployArtifact runs deployWorkflow over an artifact declaring `declared`, and
// reports what reached the store (nil when nothing did) plus stderr.
func deployArtifact(t *testing.T, declared string, args ...string) (*engine.WorkflowDef, string) {
	t.Helper()
	path := writeWASM(t, t.TempDir(), artifactDeclaring(t, declared))

	var captured *engine.WorkflowDef
	store := &mockStore{
		deployWorkflowDefFn: func(_ context.Context, def *engine.WorkflowDef) error {
			captured = def
			return nil
		},
	}
	stderr := withExitPanic(t, func() {
		deployWorkflow(context.Background(), store, nil, append([]string{"wf", path}, args...))
	})
	return captured, stderr
}

// THE CENTRAL CASE OF 2c-ii. An artifact whose source declares `internal`
// deploys as internal with no flag at all. Before this slice it deployed as
// `auth` -- reachable from the HTTP API -- while its own source said otherwise,
// which is the fail-open the whole feature exists to close.
func TestDeployWorkflowHonoursAnArtifactsDeclaredExposure(t *testing.T) {
	def, _ := deployArtifact(t, string(engine.ExposureInternal))
	if def == nil {
		t.Fatal("nothing was deployed, but this declaration is legal and needs no flag")
	}
	if def.Exposure != engine.ExposureInternal {
		t.Errorf("Exposure = %q, want %q: the artifact declares internal and the deploy expressed no "+
			"opinion, so the declaration is the answer", def.Exposure, engine.ExposureInternal)
	}
}

// The default is empty rather than `auth`, and this is why: with `auth` as the
// default, an OMITTED flag is indistinguishable from `--exposure auth`, so the
// test above would read as a request to loosen and be refused. Asserted on the
// default itself so a revert to `auth` fails here rather than only there.
func TestDeployWorkflowOmittedFlagIsNotARequestForAuth(t *testing.T) {
	def, stderr := deployArtifact(t, string(engine.ExposureInternal))
	if def == nil {
		t.Fatalf("an omitted --exposure was treated as a request to loosen a declared `internal`, "+
			"which is what a default of `auth` does:\n%s", stderr)
	}
	if def.Exposure == engine.ExposureAuth {
		t.Error("Exposure = auth for a deploy that passed no --exposure on an artifact declaring internal")
	}
}

// Loosening is REFUSED, and the refusal names both classes. Clamping to the
// declared class instead would leave the operator believing their own command
// line, and the next thing they do is debug why the workflow is unreachable.
func TestDeployWorkflowRefusesToLoosenADeclaredClass(t *testing.T) {
	def, stderr := deployArtifact(t, string(engine.ExposureInternal), "--exposure", "auth")
	if def != nil {
		t.Errorf("--exposure auth deployed over a declared `internal`, as %q", def.Exposure)
	}
	for _, want := range []string{"internal", "auth"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the refusal does not name %q, so the operator cannot see which two classes "+
				"disagree:\n%s", want, stderr)
		}
	}
}

// Tightening is allowed, in both directions of the order. `public` cannot be
// DEPLOYED here (no per-tenant opt-in), so the declaration is built by hand --
// which is exactly the artifact `cleat build` produces from
// `//cleat:exposure public`, and the one this path has to tighten rather than
// refuse.
func TestDeployWorkflowMayTightenADeclaredClass(t *testing.T) {
	for _, want := range []engine.ExposureClass{engine.ExposureAuth, engine.ExposureInternal} {
		def, stderr := deployArtifact(t, string(engine.ExposurePublic), "--exposure", string(want))
		if def == nil {
			t.Fatalf("tightening a declared `public` to %q was refused:\n%s", want, stderr)
		}
		if def.Exposure != want {
			t.Errorf("Exposure = %q, want %q", def.Exposure, want)
		}
	}
}

// A DECLARED `public` is refused too, with no flag involved. This is wider than
// slice 2a, which checked only the flag: `//cleat:exposure public` is legal to
// BUILD (the build knows no tenant), so the flag-only check let an artifact that
// declared it be stored, and a definition stored as public becomes
// world-readable the moment enforcement ships.
func TestDeployWorkflowRefusesADeclaredPublic(t *testing.T) {
	def, stderr := deployArtifact(t, string(engine.ExposurePublic))
	if def != nil {
		t.Errorf("an artifact declaring `public` was deployed as %q with no opt-in in existence", def.Exposure)
	}
	if !strings.Contains(stderr, "opt-in") {
		t.Errorf("the refusal does not name the missing per-tenant opt-in:\n%s", stderr)
	}
}

// A malformed stamp is refused, NOT read as an absence. The class comes out of
// the artifact's metadata, which is untrusted at deploy time, so `"Internal"`
// must not quietly become "no declaration" and deploy as `auth` the very
// workflow whose source asked for protection.
func TestDeployWorkflowRefusesAMalformedDeclaredClass(t *testing.T) {
	for _, bad := range []string{"Internal", "internal ", "secrets"} {
		def, stderr := deployArtifact(t, bad)
		if def != nil {
			t.Errorf("a metadata stamp of %q was accepted and deployed as %q", bad, def.Exposure)
		}
		if !strings.Contains(stderr, "not one of") {
			t.Errorf("a stamp of %q was refused, but not as a malformed class:\n%s", bad, stderr)
		}
	}
}

// ---- cleat#1986, Python half: the <wasm>.schema.json sidecar fallback ----
//
// Python has no wasm.Metadata write path of its own (see
// wasm/metadata_carries_no_entry_point_parameters_test.go), so its
// declaration rides the same sidecar cleatctl already reads for
// EntryPointSchemas. These tests use bareWASMExposureTest UNDECLARED (no
// wasm.Metadata.Exposure stamped at all) so only the sidecar can be the
// source of the declaration -- the same "control" discipline
// artifactDeclaring's own self-check applies to the metadata path.

// deployArtifactWithSchema runs deployWorkflow over an artifact with no
// wasm.Metadata declaration, next to a hand-written .schema.json sidecar, and
// reports what reached the store (nil when nothing did) plus stderr.
func deployArtifactWithSchema(t *testing.T, schemaJSON string, args ...string) (*engine.WorkflowDef, string) {
	t.Helper()
	dir := t.TempDir()
	path := writeWASM(t, dir, []byte(bareWASMExposureTest))
	if err := os.WriteFile(path+".schema.json", []byte(schemaJSON), 0o644); err != nil {
		t.Fatalf("writing sidecar: %v", err)
	}

	var captured *engine.WorkflowDef
	store := &mockStore{
		deployWorkflowDefFn: func(_ context.Context, def *engine.WorkflowDef) error {
			captured = def
			return nil
		},
	}
	stderr := withExitPanic(t, func() {
		deployWorkflow(context.Background(), store, nil, append([]string{"wf", path}, args...))
	})
	return captured, stderr
}

// THE CENTRAL CASE OF THE PYTHON SLICE, mirroring
// TestDeployWorkflowHonoursAnArtifactsDeclaredExposure for the sidecar instead
// of wasm.Metadata. Before this, a Python artifact's exposure declaration had
// nowhere to be read from at deploy at all -- every Python workflow deployed
// as `auth` regardless of what its source declared.
func TestDeployWorkflowHonoursAPythonSidecarsDeclaredExposure(t *testing.T) {
	def, stderr := deployArtifactWithSchema(t, `{"InventorySync":{"params":{},"result":{},"exposure":"internal"}}`)
	if def == nil {
		t.Fatalf("nothing was deployed, but this declaration is legal and needs no flag:\n%s", stderr)
	}
	if def.Exposure != engine.ExposureInternal {
		t.Errorf("Exposure = %q, want %q: the sidecar declares internal and the deploy expressed no "+
			"opinion, so the declaration is the answer", def.Exposure, engine.ExposureInternal)
	}
}

// An entry with no "exposure" key at all is the same as no declaration --
// omitted, never read as a literal "auth" that would then look like a real
// declaration to a reader comparing it against wasm.Metadata's own absence.
func TestDeployWorkflowSidecarWithNoExposureKeyIsNoDeclaration(t *testing.T) {
	def, stderr := deployArtifactWithSchema(t, `{"PlainOne":{"params":{},"result":{}}}`)
	if def == nil {
		t.Fatalf("a sidecar with no exposure key should deploy exactly as if there were no sidecar "+
			"at all:\n%s", stderr)
	}
	if def.Exposure != engine.ExposureAuth {
		t.Errorf("Exposure = %q, want %q (the default)", def.Exposure, engine.ExposureAuth)
	}
}

// Two entry points disagreeing on their declared class is refused, not
// merged -- the sidecar equivalent of Go's "two different classes in one
// package are refused" rule for //cleat:exposure (slice 2c-i). No live
// Python build produces this (exactly one entry point per sidecar), but the
// deploy path must not silently pick one if a hand-edited or future
// multi-entry sidecar ever does.
func TestDeployWorkflowRefusesConflictingSidecarExposures(t *testing.T) {
	def, stderr := deployArtifactWithSchema(t,
		`{"A":{"params":{},"result":{},"exposure":"internal"},"B":{"params":{},"result":{},"exposure":"auth"}}`)
	if def != nil {
		t.Errorf("deployed despite two entry points disagreeing on exposure, as %q", def.Exposure)
	}
	if !strings.Contains(stderr, "disagree") {
		t.Errorf("the refusal does not say the entries disagree:\n%s", stderr)
	}
}

// A malformed value in the sidecar is refused, not read as an absence -- same
// posture as TestDeployWorkflowRefusesAMalformedDeclaredClass for
// wasm.Metadata. The sidecar is as untrusted at deploy time as the binary's
// own metadata: anyone who can hand this path a .wasm chooses the
// .schema.json beside it too.
func TestDeployWorkflowRefusesAMalformedSidecarExposure(t *testing.T) {
	def, stderr := deployArtifactWithSchema(t, `{"A":{"params":{},"result":{},"exposure":"sideways"}}`)
	if def != nil {
		t.Errorf("a sidecar exposure of %q was accepted and deployed as %q", "sideways", def.Exposure)
	}
	if !strings.Contains(stderr, "none of") {
		t.Errorf("a stamp of %q was refused, but not as a malformed class:\n%s", "sideways", stderr)
	}
}

// wasm.Metadata wins when present, even with a sidecar also in play -- today
// this cannot happen in practice (Go stamps metadata and has no emitter that
// writes "exposure" into its own sidecar), but the precedence must hold if it
// ever does, so the fallback is pinned as a FALLBACK rather than a merge.
func TestDeployWorkflowMetadataWinsOverSidecarWhenBothDeclare(t *testing.T) {
	dir := t.TempDir()
	path := writeWASM(t, dir, artifactDeclaring(t, string(engine.ExposureInternal)))
	if err := os.WriteFile(path+".schema.json",
		[]byte(`{"A":{"params":{},"result":{},"exposure":"auth"}}`), 0o644); err != nil {
		t.Fatalf("writing sidecar: %v", err)
	}

	var captured *engine.WorkflowDef
	store := &mockStore{
		deployWorkflowDefFn: func(_ context.Context, def *engine.WorkflowDef) error {
			captured = def
			return nil
		},
	}
	withExitPanic(t, func() {
		deployWorkflow(context.Background(), store, nil, []string{"wf", path})
	})
	if captured == nil {
		t.Fatal("nothing was deployed")
	}
	if captured.Exposure != engine.ExposureInternal {
		t.Errorf("Exposure = %q, want %q: wasm.Metadata declares internal and must win over the "+
			"sidecar's auth", captured.Exposure, engine.ExposureInternal)
	}
}
