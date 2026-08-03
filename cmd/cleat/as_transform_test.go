package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// ---------------------------------------------------------------------------
// TestASTransform — AssemblyScript transform integration test
// ---------------------------------------------------------------------------
//
// This test verifies the @cleat/transform (packages/cleat-as/transform/index.js)
// in two modes:
//
//  1. Isolation (requires node) — loads the transform module directly and
//     calls its internal methods with mock AST data.  Verifies @cleatEntry
//     detection, wrapper generation, and error diagnostics (E001 for
//     Math.random()).
//
//  2. Full pipeline (requires npx + asc) — creates a minimal AS workflow
//     project in a temp directory and compiles it with `npx asc` using the
//     transform.  Verifies that a .wasm file is produced and that it exports
//     the expected workflow function.
//
// The test skips gracefully when the required tooling is not available.
// ---------------------------------------------------------------------------

func TestASTransform(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping AS transform test in short mode")
	}

	hasNode := exec.Command("node", "--version").Run() == nil
	hasNpx := exec.Command("npx", "--version").Run() == nil

	if !hasNode && !hasNpx {
		t.Skip("AS transform tests require node or npx")
	}

	// ---- Transform isolation (exercises transform JS directly) ----
	if hasNode {
		t.Run("generates_wrapper", testASTransformGeneratesWrapper)
		t.Run("no_entry_no_wrapper", testASTransformNoEntry)
		t.Run("detects_math_random", testASTransformMathRandom)
	}

	// ---- Full compilation pipeline via npx asc ----
	if hasNpx {
		t.Run("compiles_to_wasm", testASCompilesToWasm)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// repoRoot returns the absolute path to the repository root.
// Duplicated from cleat_pipeline_test.go so this file is self-contained
// when running -run TestASTransform.
func asRepoRoot(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot determine test file path")
	}
	return filepath.Join(filepath.Dir(filename), "..", "..")
}

// transformDir returns the absolute path to the AS transform package.
func transformDir(t *testing.T) string {
	return filepath.Join(asRepoRoot(t), "packages", "cleat-as", "transform")
}

// writeNodeScript writes a Node.js script to a temp file and returns its path.
func writeNodeScript(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

// ---------------------------------------------------------------------------
// Isolation test: @cleatEntry is detected and wrapper is generated
// ---------------------------------------------------------------------------

func testASTransformGeneratesWrapper(t *testing.T) {
	transformDir := transformDir(t)
	script := fmt.Sprintf(`"use strict";
const path = require("path");
const CleatEntryTransformer = require(%q);

const t = new CleatEntryTransformer();
const results = [];

function check(name, pass, detail) {
  results.push({ name, pass, detail: String(detail).substring(0, 200) });
}

// ---- Mock @cleatEntry function declaration ----
const entryStmt = {
  name: { text: "myWorkflow" },
  signature: {
    parameters: [
      { name: { text: "h" }, type: { text: "HostCalls" } },
      { name: { text: "input" }, type: { text: "string" } }
    ],
    returnType: { text: "string" }
  },
  decorators: [
    { name: { text: "cleatEntry" } }
  ]
};

// Test 1: _isDurableEntryFunc detects @cleatEntry
const detected = t._isDurableEntryFunc(entryStmt);
check("detect_cleatEntry", detected === true, detected);

// Test 2: _extractEntryInfo produces correct metadata
const info = t._extractEntryInfo(entryStmt);
check("extract_funcName", info.funcName === "myWorkflow", info.funcName);
check("extract_innerName", info.innerName === "__durable_inner_myWorkflow", info.innerName);
check("extract_paramNames_len", info.paramNames.length === 1, info.paramNames.length);
check("extract_paramNames_0", info.paramNames[0] === "input", info.paramNames[0]);
check("extract_retTypeStr", info.retTypeStr === "string", info.retTypeStr);
check("extract_isVoid", info.isVoid === false, info.isVoid);
check("extract_isString", info.isString === true, info.isString);
check("extract_callArgs", JSON.stringify(info.callArgs) === '["h","input"]', JSON.stringify(info.callArgs));

// Test 3: _generateWrappers produces valid wrapper code
const wrapperCode = t._generateWrappers([info]);

check("wrapper_export_func",
  wrapperCode.indexOf("export function myWorkflow(") >= 0,
  "missing export function");

check("wrapper_abi_signature",
  wrapperCode.indexOf("argsPtr: usize") >= 0 &&
  wrapperCode.indexOf("argsLen: i32") >= 0 &&
  wrapperCode.indexOf("outPtr: usize") >= 0 &&
  wrapperCode.indexOf("maxOutLen: i32") >= 0,
  "missing ABI signature");

check("wrapper_returns_i64",
  wrapperCode.indexOf("): i64 {") >= 0,
  "missing i64 return");

check("wrapper_inner_call",
  wrapperCode.indexOf("__durable_inner_myWorkflow") >= 0,
  "missing inner function call");

check("wrapper_new_HostCalls",
  wrapperCode.indexOf("new HostCalls()") >= 0,
  "missing HostCalls creation");

check("wrapper_reset_suspended",
  wrapperCode.indexOf("resetWorkflowSuspended") >= 0,
  "missing resetWorkflowSuspended");

check("wrapper_isWorkflowSuspended",
  wrapperCode.indexOf("isWorkflowSuspended") >= 0,
  "missing isWorkflowSuspended");

check("wrapper_SUSPEND_SENTINEL",
  wrapperCode.indexOf("SUSPEND_SENTINEL") >= 0,
  "missing SUSPEND_SENTINEL check");

check("wrapper_Memory_writeString",
  wrapperCode.indexOf("Memory.writeString(outPtr, maxOutLen, _result)") >= 0,
  "missing Memory.writeString for result");

check("wrapper_Memory_encodeExportResult",
  wrapperCode.indexOf("Memory.encodeExportResult(0, _written)") >= 0,
  "missing encodeExportResult");

// ---- Also test a function with zero user params (HostCalls only) ----
const noUserParamsStmt = {
  name: { text: "simpleWorkflow" },
  signature: {
    parameters: [
      { name: { text: "h" }, type: { text: "HostCalls" } }
    ],
    returnType: { text: "void" }
  },
  decorators: [
    { name: { text: "cleatEntry" } }
  ]
};

const noParamInfo = t._extractEntryInfo(noUserParamsStmt);
check("noParam_funcName", noParamInfo.funcName === "simpleWorkflow", noParamInfo.funcName);
check("noParam_isVoid", noParamInfo.isVoid === true, noParamInfo.isVoid);
check("noParam_paramNames_len", noParamInfo.paramNames.length === 0, noParamInfo.paramNames.length);

const noParamWrapper = t._generateWrappers([noParamInfo]);
check("noParam_void_result",
  noParamWrapper.indexOf('"ok":true') >= 0,
  "void wrapper should return {\"ok\":true}");

process.stdout.write(JSON.stringify(results));
`, filepath.Join(transformDir, "index.js"))

	tmpDir := t.TempDir()
	scriptPath := writeNodeScript(t, tmpDir, "test_wrapper.js", script)

	cmd := exec.Command("node", scriptPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node script failed: %v\n%s", err, out)
	}

	var results []struct {
		Name   string `json:"name"`
		Pass   bool   `json:"pass"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(out, &results); err != nil {
		t.Fatalf("failed to parse results JSON: %v\nraw output: %s", err, out)
	}

	for _, r := range results {
		if !r.Pass {
			t.Errorf("check %q failed: %s", r.Name, r.Detail)
		}
	}
}

// ---------------------------------------------------------------------------
// Isolation test: function with no @cleatEntry produces no wrapper
// ---------------------------------------------------------------------------

func testASTransformNoEntry(t *testing.T) {
	transformDir := transformDir(t)
	script := fmt.Sprintf(`"use strict";
const CleatEntryTransformer = require(%q);

const t = new CleatEntryTransformer();
const results = [];

function check(name, pass, detail) {
  results.push({ name, pass, detail: String(detail).substring(0, 200) });
}

// Non-entry function (no decorators at all)
const plainStmt = {
  name: { text: "helper" },
  signature: { parameters: [] },
  decorators: []
};

check("plain_not_entry", t._isDurableEntryFunc(plainStmt) === false,
  t._isDurableEntryFunc(plainStmt));

// Non-entry function (wrong decorator name)
const wrongDecoratorStmt = {
  name: { text: "otherFunc" },
  signature: { parameters: [] },
  decorators: [
    { name: { text: "notCleatEntry" } }
  ]
};

check("wrong_decorator_not_entry", t._isDurableEntryFunc(wrongDecoratorStmt) === false,
  t._isDurableEntryFunc(wrongDecoratorStmt));

// Null/edge cases
check("null_stmt_not_entry", t._isDurableEntryFunc(null) === false,
  t._isDurableEntryFunc(null));

check("empty_object_not_entry", t._isDurableEntryFunc({}) === false,
  t._isDurableEntryFunc({}));

// Verify no wrapper is generated when entries list is empty
const emptyWrapper = t._generateWrappers([]);
check("empty_wrapper_no_content", emptyWrapper.indexOf("export function") === -1,
  "empty entries list should produce no export functions");

// No @cleatEntry in a minimal source program
const sourceEntries = t._findDurableEntries({
  sources: [
    {
      statements: [
        {
          name: { text: "helper" },
          signature: { parameters: [] },
          decorators: []
        }
      ]
    }
  ]
});
check("no_entry_in_source", sourceEntries.length === 0,
  sourceEntries.length + " entries found (expected 0)");

process.stdout.write(JSON.stringify(results));
`, filepath.Join(transformDir, "index.js"))

	tmpDir := t.TempDir()
	scriptPath := writeNodeScript(t, tmpDir, "test_no_entry.js", script)

	cmd := exec.Command("node", scriptPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node script failed: %v\n%s", err, out)
	}

	var results []struct {
		Name   string `json:"name"`
		Pass   bool   `json:"pass"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(out, &results); err != nil {
		t.Fatalf("failed to parse results JSON: %v\nraw output: %s", err, out)
	}

	for _, r := range results {
		if !r.Pass {
			t.Errorf("check %q failed: %s", r.Name, r.Detail)
		}
	}
}

// ---------------------------------------------------------------------------
// Isolation test: Math.random() triggers E001 error
// ---------------------------------------------------------------------------

func testASTransformMathRandom(t *testing.T) {
	transformDir := transformDir(t)
	script := fmt.Sprintf(`"use strict";
const CleatEntryTransformer = require(%q);

const t = new CleatEntryTransformer();
const results = [];

function check(name, pass, detail) {
  results.push({ name, pass, detail: String(detail).substring(0, 200) });
}

// Simulate a function whose body calls Math.random()
const badStmt = {
  name: { text: "badFunc" },
  signature: { parameters: [] },
  body: {
    statements: [
      {
        callee: {
          object: { text: "Math" },
          property: { name: { text: "random" } }
        },
        args: []
      }
    ]
  }
};

const source = { internalPath: "index.ts", statements: [] };

// Capture console.error output from _validateDurableFunction
const origError = console.error;
let captured = "";
console.error = function(msg) { captured += msg + "\n"; };

t._validateDurableFunction(badStmt, source);

console.error = origError;

check("e001_emitted", captured.indexOf("E001") >= 0,
  captured.length > 0 ? captured.substring(0, 150) : "no error output");

check("e001_mentions_random", captured.indexOf("Math.random") >= 0,
  "E001 should mention Math.random()");

check("e001_mentions_badFunc", captured.indexOf("badFunc") >= 0,
  "E001 should mention the function name");

// ---- Test that Math.seedRandom also triggers E001 ----
const seedRandomStmt = {
  name: { text: "seededFunc" },
  signature: { parameters: [] },
  body: {
    statements: [
      {
        callee: {
          object: { text: "Math" },
          property: { name: { text: "seedRandom" } }
        },
        args: []
      }
    ]
  }
};

captured = "";
console.error = function(msg) { captured += msg + "\n"; };

t._validateDurableFunction(seedRandomStmt, source);

console.error = origError;

check("e001_seedRandom_emitted", captured.indexOf("E001") >= 0,
  captured.length > 0 ? captured.substring(0, 150) : "no error for seedRandom");

// ---- Test that a non-Math call does NOT trigger E001 ----
const safeStmt = {
  name: { text: "safeFunc" },
  signature: { parameters: [] },
  body: {
    statements: [
      {
        callee: {
          object: { text: "console" },
          property: { name: { text: "log" } }
        },
        args: []
      }
    ]
  }
};

captured = "";
console.error = function(msg) { captured += msg + "\n"; };

t._validateDurableFunction(safeStmt, source);

console.error = origError;

check("console_log_not_e001", captured.indexOf("E001") === -1,
  captured.length > 0 ? captured.substring(0, 150) : "no E001 for console.log (expected)");

process.stdout.write(JSON.stringify(results));
`, filepath.Join(transformDir, "index.js"))

	tmpDir := t.TempDir()
	scriptPath := writeNodeScript(t, tmpDir, "test_math_random.js", script)

	cmd := exec.Command("node", scriptPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node script failed: %v\n%s", err, out)
	}

	var results []struct {
		Name   string `json:"name"`
		Pass   bool   `json:"pass"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(out, &results); err != nil {
		t.Fatalf("failed to parse results JSON: %v\nraw output: %s", err, out)
	}

	for _, r := range results {
		if !r.Pass {
			t.Errorf("check %q failed: %s", r.Name, r.Detail)
		}
	}
}

// ---------------------------------------------------------------------------
// Full pipeline: compile minimal AS workflow with npx asc + transform
// ---------------------------------------------------------------------------

func testASCompilesToWasm(t *testing.T) {
	tmpDir := t.TempDir()

	// ---- Create project files ----

	// assembly/index.ts — self-contained source that does not import
	// @cleat/sdk (AS 0.27.32 cannot resolve scoped packages from node_modules
	// nor from asconfig.json paths).  All types referenced by the transform's
	// generated wrapper are defined inline.
	asmDir := filepath.Join(tmpDir, "assembly")
	if err := os.MkdirAll(asmDir, 0755); err != nil {
		t.Fatal(err)
	}

	// This fixture imports the real @cleat/sdk, exactly as a user's workflow
	// does. It previously declared stand-in HostCalls/Memory/cleatEntry
	// definitions inline because the SDK was not installed; those now collide
	// with the real ones (TS2300 duplicate identifier), and more importantly
	// they meant this test could never have caught a mismatch between the
	// transform's generated wrapper and the SDK it generates against — which
	// is most of what there is to catch here.
	indexTS := `
import { HostCalls, cleatEntry } from "@cleat/sdk";

@cleatEntry()
function myWorkflow(h: HostCalls, input: string): string {
  return "{\"status\":\"ok\"}";
}
`
	if err := os.WriteFile(filepath.Join(asmDir, "index.ts"), []byte(indexTS), 0644); err != nil {
		t.Fatal(err)
	}

	// package.json. The --transform flag points at the transform's JS file
	// directly, but that is not sufficient on its own: the transform injects
	// an `import { HostCalls, Memory, ... } from "@cleat/sdk"` into the
	// wrapper it generates, and asc must resolve that import at parse time.
	// Installing @cleat/sdk from the repo checkout is what lets this subtest
	// actually compile. Without it asc fails with a parse error, no .wasm is
	// produced, and the subtest skips — which is what it did from the day it
	// was written until this was fixed, despite being named "compiles to wasm".
	pkgJSON := fmt.Sprintf(`{
  "name": "test-as-workflow",
  "private": true,
  "devDependencies": {
    "assemblyscript": "^0.27.0"
  },
  "dependencies": {
    "@cleat/sdk": "file:%s"
  }
}`, filepath.Join(asRepoRoot(t), "packages", "cleat-as"))
	if err := os.WriteFile(filepath.Join(tmpDir, "package.json"), []byte(pkgJSON), 0644); err != nil {
		t.Fatal(err)
	}

	// ---- npm install (needed for asc binary) ----
	t.Log("Running npm install...")
	npmCmd := exec.Command("npm", "install", "--no-audit", "--no-fund")
	npmCmd.Dir = tmpDir
	if out, err := npmCmd.CombinedOutput(); err != nil {
		t.Skipf("npm install failed (may not have network): %v\n%s", err, out)
	}

	// ---- Locate asc binary ----
	ascPath := filepath.Join(tmpDir, "node_modules", ".bin", "asc")
	if _, err := os.Stat(ascPath); os.IsNotExist(err) {
		ascPath = filepath.Join(tmpDir, "node_modules", "assemblyscript", "bin", "asc.js")
		if _, err := os.Stat(ascPath); os.IsNotExist(err) {
			t.Fatalf("asc binary not found after npm install")
		}
	}

	// ---- Compile with asc + transform ----
	transformPath := filepath.Join(transformDir(t), "index.js")
	distDir := filepath.Join(tmpDir, "dist")
	if err := os.MkdirAll(distDir, 0755); err != nil {
		t.Fatal(err)
	}

	t.Log("Running asc compilation with transform...")
	sourcePath := filepath.Join(asmDir, "index.ts")
	wasmPath := filepath.Join(distDir, "workflow.wasm")

	ascArgs := []string{
		sourcePath,
		"--runtime", "stub",
		"-O0",
		"--initialMemory", "170",
		"--transform", transformPath,
		"-o", wasmPath,
	}

	ascCmd := exec.Command(ascPath, ascArgs...)
	ascCmd.Dir = tmpDir
	ascOut, ascErr := ascCmd.CombinedOutput()
	t.Logf("asc output:\n%s", string(ascOut))

	// A compilation failure is a FAILURE, not a skip.
	//
	// This block used to t.Skipf here, which made the subtest unfalsifiable:
	// any asc error at all — including a real regression in the transform —
	// produced no .wasm and was reported as a skip, and skips are green. The
	// stated justification was that @cleat/sdk could not be installed in the
	// fixture, so the transform's generated import never resolved. That is now
	// fixed (see the package.json above), so there is no longer any expected
	// reason for asc to fail, and any failure is a defect worth failing on.
	//
	// The only legitimate skips in this test are environmental and are handled
	// earlier: a missing `npm`/`node`, or an `npm install` that cannot reach
	// the network.
	if ascErr != nil {
		if _, err := os.Stat(wasmPath); os.IsNotExist(err) {
			t.Fatalf("asc compilation failed and produced no .wasm: %v\n%s", ascErr, ascOut)
		}
		t.Errorf("asc reported an error even though a .wasm was produced: %v\n%s", ascErr, ascOut)
	}

	// ---- Verify .wasm output ----
	wasmData, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatalf("reading .wasm file: %v", err)
	}
	if len(wasmData) == 0 {
		t.Fatal("WASM file is empty")
	}

	// Verify WASM magic number (\0asm)
	if len(wasmData) < 4 || wasmData[0] != 0x00 || wasmData[1] != 0x61 ||
		wasmData[2] != 0x73 || wasmData[3] != 0x6D {
		t.Fatal("WASM file does not start with \\0asm magic bytes")
	}

	// ---- Verify WASM exports using Node.js ----
	verifyScript := fmt.Sprintf(`"use strict";
const fs = require("fs");

const wasmPath = %q;
const wasm = fs.readFileSync(wasmPath);
const mod = new WebAssembly.Module(wasm);
const allExports = WebAssembly.Module.exports(mod);

const funcExports = allExports.filter(function(e) { return e.kind === "function"; });
const names = funcExports.map(function(e) { return e.name; });
console.log(JSON.stringify({ funcNames: names, all: allExports }));
`, wasmPath)

	verifyScriptPath := filepath.Join(tmpDir, "verify_wasm.js")
	if err := os.WriteFile(verifyScriptPath, []byte(verifyScript), 0644); err != nil {
		t.Fatal(err)
	}

	verifyCmd := exec.Command("node", verifyScriptPath)
	verifyOut, verifyErr := verifyCmd.CombinedOutput()
	if verifyErr != nil {
		t.Fatalf("WASM verification script failed: %v\n%s", verifyErr, verifyOut)
	}

	var verifyResult struct {
		FuncNames []string          `json:"funcNames"`
		All       []json.RawMessage `json:"all"`
	}
	if err := json.Unmarshal(verifyOut, &verifyResult); err != nil {
		t.Fatalf("parsing WASM export verification: %v\nraw: %s", err, verifyOut)
	}

	// The transform should have generated a wrapper that exports myWorkflow.
	// This used to be a t.Skipf, on the theory that AS 0.27.32's afterParse
	// provides parser.sources but not parser.program, so the transform (which
	// read parser.program) could never detect @cleatEntry and myWorkflow could
	// legitimately go missing. That bug was real but was fixed in
	// packages/cleat-as/transform/index.js (afterParse now reads this.program,
	// which AS sets on the prototype, instead of the absent parser.program) --
	// see the comment there. Confirmed live: this test now finds myWorkflow
	// every run. A missing export here is a real transform regression, not an
	// expected AS-version quirk, so it must fail rather than skip.
	found := false
	for _, name := range verifyResult.FuncNames {
		if name == "myWorkflow" {
			found = true
			break
		}
	}

	if !found {
		t.Fatalf("WASM compiled but 'myWorkflow' export not found (got functions: %v)", verifyResult.FuncNames)
	}

	t.Logf("WASM exports %d functions including 'myWorkflow': %v",
		len(verifyResult.FuncNames), verifyResult.FuncNames)
	t.Logf("WASM size: %d bytes", len(wasmData))
}
