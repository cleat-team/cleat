package engine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// buildJavaWasm compiles the Java saga workflow to WASM via TeaVM/Gradle and
// returns the path to the compiled module.
//
// Java is best-effort: if the toolchain or compilation fails, the test skips
// gracefully. The WASM output from saga-java-port is at:
//
//	build/wasm/wasm/workflow.wasm
func buildJavaWasm(t *testing.T) string {
	t.Helper()

	repoRoot := findRepoRoot(t)
	javaDir := filepath.Join(repoRoot, "examples", "saga-java-port")
	wasmPath := filepath.Join(javaDir, "build", "wasm", "wasm", "workflow.wasm")

	// Try to build with Gradle. We need either "gradle" on PATH or a
	// "gradlew" wrapper in the project directory.
	hasGradle := false
	if _, err := exec.LookPath("gradle"); err == nil {
		hasGradle = true
	}
	if _, err := os.Stat(filepath.Join(javaDir, "gradlew")); err == nil {
		hasGradle = true
	}
	// Same policy as buildRustWasm: a job that declares it provides the
	// toolchain must fail when the toolchain is missing, because that means its
	// own setup step failed silently. e2e-cross-language.yml runs
	// -run "...|TestJava" with Java 17 and Gradle installed, so for that job the
	// absence of either is a broken runner, not an absent prerequisite.
	if !hasGradle {
		if toolchainRequired("java") {
			t.Fatalf("gradle/gradlew not found, but %s declares java, so this job installs Java + Gradle -- its setup step must have failed silently", requireToolchainEnv)
		}
		t.Skip("gradle/gradlew not found -- skipping Java WASM integration test (only e2e-cross-language.yml provisions Java for this test)")
	}
	if _, err := exec.LookPath("java"); err != nil {
		if toolchainRequired("java") {
			t.Fatalf("java not installed, but %s declares java, so this job installs it -- its setup step must have failed silently: %v", requireToolchainEnv, err)
		}
		t.Skip("java not installed -- skipping Java WASM integration test (only e2e-cross-language.yml provisions Java for this test)")
	}

	// Use the Gradle wrapper if the project provides one.
	gradleBin := "gradle"
	if _, err := os.Stat(filepath.Join(javaDir, "gradlew")); err == nil {
		gradleBin = filepath.Join(javaDir, "gradlew")
	}

	// generateWasm, not wasm. The TeaVM Gradle plugin has never registered a
	// task called `wasm` -- `gradle tasks --all` lists generateWasm under "TeaVM
	// tasks" -- so this invocation failed with "Task 'wasm' not found" on every
	// machine that had the toolchain installed. Because the failure was then
	// degraded to a skip, the test reported the same thing whether Java worked
	// or not, and the SDK was assessed as unverified on that evidence.
	cmd := exec.Command(gradleBin, "generateWasm", "--no-daemon")
	cmd.Dir = javaDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gradle generateWasm failed:\n%s\n%v", string(out), err)
	}

	if _, err := os.Stat(wasmPath); os.IsNotExist(err) {
		t.Fatalf("gradle generateWasm completed but WASM not found at %s", wasmPath)
	}

	t.Logf("Java WASM built: %s", wasmPath)
	return wasmPath
}

// TestJavaWorkflowExecute compiles the Java MoneyTransfer saga workflow to
// WASM, loads it into the Go host runtime, executes the transfer_money entry
// point, and verifies that the event history contains the expected saga steps
// (accounts.Withdraw -> accounts.Deposit).
func TestJavaWorkflowExecute(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping Java WASM integration test in short mode")
	}

	wasmPath := buildJavaWasm(t)
	wasmBytes, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatalf("read Java WASM: %v", err)
	}

	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	caller := &mockCaller{}
	engine := NewEngine(rt, caller)

	// The MoneyTransfer.transferMoney entry point expects JSON with
	// source/destination accounts and an amount.
	input := []byte(`{"from":"accountA","to":"accountB","amount":100,"currency":"USD"}`)
	result, history, suspended, deferrals, queryState, err := engine.Execute(ctx, wasmBytes, "transfer_money", input)
	if err != nil {
		t.Fatalf("Execute Java workflow: %v", err)
	}
	if suspended != nil {
		t.Errorf("unexpected workflow suspension: %v", suspended.Reason)
	}
	if result == "" {
		t.Error("expected non-empty result from Java workflow")
	}
	if len(history) == 0 {
		t.Error("expected non-empty history from Java workflow")
	}

	// The saga should have made two external calls:
	//  accounts.Withdraw -> accounts.Deposit
	// Select the calls, rather than deselecting durable_log. The saga registers
	// a compensation before it withdraws, so history[0] is a `defer` with no
	// Service or Op -- under an exclusion filter it landed in callHistory and
	// shifted every index by one, which read as "step 0: expected
	// accounts.Withdraw, got .". Naming the event type this test is about means
	// a new event kind cannot silently rejoin the list.
	var callHistory []EventRecord
	for _, rec := range history {
		if rec.EventType == EventTypeCall {
			callHistory = append(callHistory, rec)
		}
	}

	if len(callHistory) < 2 {
		t.Errorf("expected at least 2 external calls, got %d", len(callHistory))
	} else {
		if callHistory[0].Service != "accounts" || callHistory[0].Op != "Withdraw" {
			t.Errorf("step 0: expected accounts.Withdraw, got %s.%s",
				callHistory[0].Service, callHistory[0].Op)
		}
		if callHistory[1].Service != "accounts" || callHistory[1].Op != "Deposit" {
			t.Errorf("step 1: expected accounts.Deposit, got %s.%s",
				callHistory[1].Service, callHistory[1].Op)
		}
	}

	// Verify the result JSON indicates success.
	if !strings.Contains(result, `"status":"completed"`) &&
		!strings.Contains(result, `"status"`) {
		t.Logf("Warning: result may not indicate success: %s", result)
	}

	t.Logf("Java workflow result: %s", result)
	t.Logf("History: %d events, deferrals: %v, queryState: %v",
		len(history), deferrals, queryState)
	for i, rec := range history {
		t.Logf("  step %d: %s.%s => %s (err=%s)", i, rec.Service, rec.Op, rec.Response, rec.Err)
	}
}
