package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// cleat#3922's cliff: --wasm-instance-timeout in (0, 250ms) is refused at startup, and the values around the boundary are not.
func TestWasmInstanceTimeoutBelowTheCliffIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name    string
		d       time.Duration
		refused bool
	}{
		{"100ms, where the soft kill reached the guest 0 of 20 times", 100 * time.Millisecond, true},
		{"249ms, one under the floor", 249 * time.Millisecond, true},
		{"249.999ms, the last refused value", 249*time.Millisecond + 999*time.Microsecond + 999*time.Nanosecond, true},
		{"1ns, the first refused value", 1, true},
		{"negative", -time.Second, true},
		{"250ms, the floor itself", 250 * time.Millisecond, false},
		{"0, which disables the timeout and keeps its meaning", 0, false},
		{"the 30s default", 30 * time.Second, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateWasmInstanceTimeout(tc.d)
			if tc.refused && err == nil {
				t.Fatalf("validateWasmInstanceTimeout(%v) = nil, want a refusal", tc.d)
			}
			if !tc.refused && err != nil {
				t.Fatalf("validateWasmInstanceTimeout(%v) = %v, want nil", tc.d, err)
			}
		})
	}
}

// The refusal must say what was asked for, the floor, and why, so an operator is not left to guess.
func TestWasmInstanceTimeoutRefusalNamesTheValueTheFloorAndTheReason(t *testing.T) {
	err := validateWasmInstanceTimeout(100 * time.Millisecond)
	if err == nil {
		t.Fatal("100ms was accepted")
	}
	msg := err.Error()
	for _, want := range []string{"--wasm-instance-timeout", "100ms", "250ms", "defers", "ABI.md"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not contain %q: %v", want, err)
		}
	}
}

// The flag's default and the floor must agree, or the worker would refuse to start with no flags at all.
func TestTheDefaultWasmInstanceTimeoutPassesTheFloor(t *testing.T) {
	if err := validateWasmInstanceTimeout(*wasmInstanceTimeout); err != nil {
		t.Fatalf("the flag's default value is refused by its own validator: %v", err)
	}
}

// The validator's own tests pass whether or not main() calls it, which is how a guard becomes dead code with green tests
// (CLAUDE.md, "A mechanism that exists and is wired to nothing reads as done"). So run main() itself, in a child process, with the
// flag under the floor, and require that it exits non-zero with the refusal before anything else.
//
// The child is this test binary re-executed with CLEAT_TEST_WORKER_MAIN set, which makes TestWorkerMainHelperProcess hand control to
// main() with os.Args replaced.
func TestWorkerMainHelperProcess(t *testing.T) {
	if os.Getenv("CLEAT_TEST_WORKER_MAIN") != "1" {
		t.Skip("helper process for TestWorkerRefusesToStartBelowTheInstanceTimeoutFloor; not a test on its own")
	}
	os.Args = append([]string{"cleat-worker"}, strings.Fields(os.Getenv("CLEAT_TEST_WORKER_ARGS"))...)
	main()
	os.Exit(0)
}

func runWorkerMainHelper(t *testing.T, args string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestWorkerMainHelperProcess$", "-test.count=1")
	cmd.Env = append(os.Environ(), "CLEAT_TEST_WORKER_MAIN=1", "CLEAT_TEST_WORKER_ARGS="+args, "CLEAT_DATABASE_URL=")
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("could not run the helper process: %v", err)
		}
		code = ee.ExitCode()
	}
	return code, string(out)
}

func TestWorkerRefusesToStartBelowTheInstanceTimeoutFloor(t *testing.T) {
	const refusal = "--wasm-instance-timeout 100ms is below the 250ms minimum"

	code, out := runWorkerMainHelper(t, "--wasm-instance-timeout=100ms")
	if code == 0 {
		t.Fatalf("main() exited 0 with --wasm-instance-timeout=100ms:\n%s", out)
	}
	if !strings.Contains(out, refusal) {
		t.Fatalf("main() exited %d but not with the refusal (want %q):\n%s", code, refusal, out)
	}

	// The control that makes the refusal mean something: the same child at the floor does NOT print it. It will still exit, because
	// no database is configured, but for a different reason.
	_, out = runWorkerMainHelper(t, "--wasm-instance-timeout=250ms")
	if strings.Contains(out, "is below the 250ms minimum") {
		t.Fatalf("main() refused --wasm-instance-timeout=250ms, the floor itself:\n%s", out)
	}
	if !strings.Contains(out, "starting worker") {
		t.Fatalf("the control child never logged `starting worker`, so it may not have run main() past flag parsing:\n%s", out)
	}
}
