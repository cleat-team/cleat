//go:build cgo

package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Compiling a component in a child process (cleat-internal/component-precompile-design-2026-11-23.md, Option A).
//
// The test binary plays the worker's role: an exec of it with SerializeComponentArg as its first argument runs the real child (the init below), exactly as an exec of cleat-worker does in main.
// FAULTS are injected AFTER the real child has run, by the child's own test-only wrapper, so every fault is a real artifact that was really produced and then damaged.
//
// What "served" means here, and why a parent-side zero is not it: componentCache.compiles is per backend, so compiles==0 is also what a bug that loads nothing would show. A component is served from
// a child only when the PARENT's compiles is 0 AND its childCompiles is 1 AND the child's own record names another process that compiled it exactly once.

const precompileFaultEnv = "CLEAT_PRECOMPILE_TEST_FAULT"

func init() {
	if len(os.Args) > 1 && os.Args[1] == SerializeComponentArg {
		os.Exit(runPrecompileTestChild(os.Args[2:]))
	}
}

// runPrecompileTestChild is the real child plus two test-only behaviours that the production entry point does not have.
func runPrecompileTestChild(args []string) int {
	// The worker's environment holds database credentials and the child needs none of it. The parent sets t.Setenv on a probe variable; a child that can see it was handed the parent's environment.
	if os.Getenv("CLEAT_PRECOMPILE_ENV_PROBE") != "" {
		os.Stderr.WriteString("test child: it inherited the worker's environment\n")
		return 3
	}
	// A child that finds another child running exits nonzero. The parent runs one at a time, so this never fires; a parent that ran two would see fallbacks.
	if dir := os.Getenv("CLEAT_PRECOMPILE_TEST_EXCLUSIVE_DIR"); dir != "" {
		mine := filepath.Join(dir, "running-"+strconv.Itoa(os.Getpid()))
		if others, _ := filepath.Glob(filepath.Join(dir, "running-*")); len(others) > 0 {
			os.Stderr.WriteString("test child: another child is running\n")
			return 8
		}
		if err := os.WriteFile(mine, nil, 0o600); err != nil {
			return 9
		}
		defer os.Remove(mine)
		time.Sleep(150 * time.Millisecond)
	}
	fault := os.Getenv(precompileFaultEnv)
	if fault == "" {
		return RunSerializeComponentChild(args, os.Stdout, os.Stderr)
	}
	var out bytes.Buffer
	if rc := RunSerializeComponentChild(args, &out, os.Stderr); rc != 0 {
		return rc
	}
	var rec componentChildRecord
	if err := json.Unmarshal(out.Bytes(), &rec); err != nil {
		return 4
	}
	artifact := args[1]
	data, err := os.ReadFile(artifact)
	if err != nil {
		return 5
	}
	switch fault {
	case "truncate-keep-record": // the file is shorter than the record says
		data = data[:len(data)/2]
	case "flip-keep-record": // the same size, one byte different
		data[len(data)/2] ^= 0xff
	case "truncate-rehash": // a record that agrees with the damaged file, so only the engine can refuse it
		data = data[:len(data)/2]
	case "kill-self": // what the kernel's out-of-memory killer does to the child
		syscall.Kill(os.Getpid(), syscall.SIGKILL)
		return 10
	case "ppid-record": // a child that claims the compile was done by the worker itself
		rec.PID = os.Getppid()
	case "garbage-rehash":
		for i := range data {
			data[i] = byte(i * 7)
		}
	default:
		return 6
	}
	if err := os.WriteFile(artifact, data, 0o600); err != nil {
		return 7
	}
	if strings.HasSuffix(fault, "-rehash") {
		rec.Bytes = int64(len(data))
		rec.SHA256 = sha256Hex(data)
	}
	_ = json.NewEncoder(os.Stdout).Encode(rec)
	return 0
}

// capturedLog collects a backend's own log records so a test can say what the worker reported.
type capturedLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *capturedLog) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

func (c *capturedLog) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

func newPrecompileBackend(t *testing.T, extra ...WasmtimeOption) (*wasmtimeBackend, *capturedLog) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	logs := &capturedLog{}
	opts := append([]WasmtimeOption{
		WithWasmtimeComponentPrecompile(ComponentPrecompile{Executable: exe}),
		WithWasmtimeLogger(slog.New(slog.NewTextHandler(logs, nil))),
	}, extra...)
	b := newComponentBackend(t, opts...)
	t.Cleanup(func() { _ = b.Close(context.Background()) })
	if b.precompiler == nil {
		t.Fatalf("the backend has no precompiler, so every test below would measure the in-process path:\n%s", logs.String())
	}
	return b, logs
}

// 1. SERVED. The component is compiled by another process, once, and the worker never compiles it.
func TestAComponentIsServedFromAChildsArtifactAndTheWorkerNeverCompilesIt(t *testing.T) {
	t.Setenv("CLEAT_PRECOMPILE_ENV_PROBE", "secret-the-child-must-not-see")
	b, logs := newPrecompileBackend(t)
	wasm := distinctComponent(t, 0)

	runComponent(t, b, wasm)
	runComponent(t, b, wasm) // a hit: nothing more is compiled by anyone

	if got := compilesOf(b); got != 0 {
		t.Fatalf("the worker compiled the component %d time(s) in-process, want 0 (served from a child's artifact)\n%s", got, logs.String())
	}
	if got := b.precompiler.childCompiles.Load(); got != 1 {
		t.Fatalf("childCompiles = %d, want 1: a parent-side zero alone proves nothing\n%s", got, logs.String())
	}
	if got := b.precompiler.fallbacks.Load(); got != 0 {
		t.Errorf("fallbacks = %d, want 0\n%s", got, logs.String())
	}
	rec := b.precompiler.lastRecord.Load()
	if rec == nil {
		t.Fatal("no child record was kept")
	}
	if rec.PID <= 0 || rec.PID == os.Getpid() {
		t.Errorf("the record names pid %d, this process is %d: it must be ANOTHER process", rec.PID, os.Getpid())
	}
	if rec.Compiled != 1 || rec.Bytes <= 0 || rec.SHA256 == "" {
		t.Errorf("record = %+v, want compiled=1 and a size and a hash", rec)
	}
	if rec.Parallel {
		t.Errorf("the child compiled with parallel_compilation on though the option left ParallelCompilation unset (its zero value is the low-memory compile)")
	}
	if !rec.Epoch || rec.Fuel {
		t.Errorf("record epoch=%v fuel=%v, want epoch on and fuel off for a worker with no instruction limit", rec.Epoch, rec.Fuel)
	}
	if !strings.Contains(logs.String(), "compiled in a child process") {
		t.Errorf("the worker did not log that a child compiled it:\n%s", logs.String())
	}

	// CONTROL: with no precompiler the same component is compiled here once, so "compiles == 0" above is the child's doing and not a counter that never counts.
	plain := newComponentBackend(t)
	t.Cleanup(func() { _ = plain.Close(context.Background()) })
	runComponent(t, plain, wasm)
	if got := compilesOf(plain); got != 1 {
		t.Errorf("CONTROL: a backend with no precompiler compiled %d time(s), want 1", got)
	}
	if plain.precompiler != nil {
		t.Errorf("CONTROL: a backend built without the option has a precompiler")
	}
}

// The setting reaches the child: ParallelCompilation true is what the record reports, and the artifact is accepted either way (parallel_compilation does not bind it).
func TestTheParallelCompilationSettingReachesTheChildAndBothArtifactsLoad(t *testing.T) {
	for _, parallel := range []bool{true, false} {
		b, logs := newPrecompileBackend(t)
		b.precompiler.parallel = parallel
		runComponent(t, b, distinctComponent(t, 0))
		rec := b.precompiler.lastRecord.Load()
		if rec == nil || rec.Parallel != parallel {
			t.Errorf("parallel=%v: record = %+v\n%s", parallel, rec, logs.String())
		}
		if b.precompiler.childCompiles.Load() != 1 || compilesOf(b) != 0 {
			t.Errorf("parallel=%v: the artifact was not loaded (child=%d compiles=%d)\n%s", parallel, b.precompiler.childCompiles.Load(), compilesOf(b), logs.String())
		}
	}
}

// The served component RUNS and answers what the in-process one does.
func TestAComponentServedFromAChildsArtifactAnswersAsTheInProcessOneDoes(t *testing.T) {
	b, _ := newPrecompileBackend(t)
	plain := newComponentBackend(t)
	t.Cleanup(func() { _ = plain.Close(context.Background()) })
	wasm := distinctComponent(t, 3)

	run := func(be *wasmtimeBackend) string {
		res, err := be.PerExecution(0).Execute(context.Background(), wasm, "run", json.RawMessage(`"x"`), &mockHostHandler{})
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		return mustJSON(t, res)
	}
	if served, local := run(b), run(plain); served != local {
		t.Errorf("a component loaded from a child's artifact answered %s, the in-process one %s", served, local)
	}
	if got := b.precompiler.childCompiles.Load(); got != 1 {
		t.Errorf("the comparison did not go through a child (childCompiles=%d)", got)
	}
}

// 2. FALLBACKS. Every way the child's output can be wrong ends in an in-process compile and a workflow that still runs, with the reason logged. These are the known positives for the integrity
// checks: each was a real artifact, damaged after the child wrote it.
func TestAnArtifactTheWorkerWillNotAcceptFallsBackToACompileInThisProcess(t *testing.T) {
	for _, tc := range []struct{ fault, stage string }{
		{"truncate-keep-record", "bytes on disk"}, // size on disk differs from the record: refused by the SIZE check, before the file is hashed
		{"flip-keep-record", "sha256"},            // same size, different bytes
		{"truncate-rehash", "deserialize"},        // size and hash agree with the file: only the engine can refuse it
		{"garbage-rehash", "deserialize"},
		{"ppid-record", "not a compile by another process"}, // not a serialized component at all
	} {
		tc := tc
		t.Run(tc.fault, func(t *testing.T) {
			b, logs := newPrecompileBackend(t)
			b.precompiler.childEnv = []string{precompileFaultEnv + "=" + tc.fault}
			runComponent(t, b, distinctComponent(t, 0)) // must still complete

			if got := compilesOf(b); got != 1 {
				t.Errorf("compiles = %d, want 1: a refused artifact is replaced by an in-process compile\n%s", got, logs.String())
			}
			if got := b.precompiler.fallbacks.Load(); got != 1 {
				t.Errorf("fallbacks = %d, want 1", got)
			}
			if got := b.precompiler.childCompiles.Load(); got != 0 {
				t.Errorf("childCompiles = %d, want 0: a refused artifact was counted as served", got)
			}
			if out := logs.String(); !strings.Contains(out, "component precompile in a child failed") || !strings.Contains(out, tc.stage) {
				t.Errorf("the fallback did not say why (want a warning containing %q):\n%s", tc.stage, out)
			}
		})
	}
}

// 3. FUEL BINDS THE ARTIFACT. An artifact compiled with fuel will not load into an engine without it, nor the reverse. The worker REFUSES it (and compiles its own); it does not load it.
func TestAnArtifactCompiledWithADifferentFuelSettingIsRefusedNotLoaded(t *testing.T) {
	const limit = 1_000_000
	zero := uint64(0)
	nonzero := uint64(limit)

	t.Run("the worker has fuel and the child is told the same", func(t *testing.T) {
		b, logs := newPrecompileBackend(t, WithWasmtimeInstructionLimit(limit))
		runComponent(t, b, distinctComponent(t, 0))
		if b.precompiler.childCompiles.Load() != 1 || compilesOf(b) != 0 {
			t.Fatalf("a fuel worker was not served from a fuel artifact (child=%d, compiles=%d)\n%s", b.precompiler.childCompiles.Load(), compilesOf(b), logs.String())
		}
		if rec := b.precompiler.lastRecord.Load(); rec == nil || !rec.Fuel {
			t.Errorf("record = %+v, want fuel on", rec)
		}
	})
	t.Run("the worker has fuel and the artifact does not", func(t *testing.T) {
		b, logs := newPrecompileBackend(t, WithWasmtimeInstructionLimit(limit))
		b.precompiler.childInstructionLimit = &zero
		runComponent(t, b, distinctComponent(t, 0))
		if compilesOf(b) != 1 || b.precompiler.fallbacks.Load() != 1 || b.precompiler.childCompiles.Load() != 0 {
			t.Errorf("a fuel-off artifact was not refused by a fuel-on worker (compiles=%d fallbacks=%d child=%d)\n%s",
				compilesOf(b), b.precompiler.fallbacks.Load(), b.precompiler.childCompiles.Load(), logs.String())
		}
	})
	t.Run("the worker has no fuel and the artifact does", func(t *testing.T) {
		b, logs := newPrecompileBackend(t)
		b.precompiler.childInstructionLimit = &nonzero
		runComponent(t, b, distinctComponent(t, 0))
		if compilesOf(b) != 1 || b.precompiler.fallbacks.Load() != 1 || b.precompiler.childCompiles.Load() != 0 {
			t.Errorf("a fuel-on artifact was not refused by a fuel-off worker (compiles=%d fallbacks=%d child=%d)\n%s",
				compilesOf(b), b.precompiler.fallbacks.Load(), b.precompiler.childCompiles.Load(), logs.String())
		}
	})
}

// 4. THE CHILD ITSELF FAILS. Every one of these is a worker that keeps working.
func TestAChildThatCannotCompileLeavesTheWorkerCompilingInProcess(t *testing.T) {
	t.Run("the executable does not exist", func(t *testing.T) {
		b, logs := newPrecompileBackend(t)
		b.precompiler.exe = filepath.Join(t.TempDir(), "no-such-binary")
		runComponent(t, b, distinctComponent(t, 0))
		if compilesOf(b) != 1 || b.precompiler.fallbacks.Load() != 1 {
			t.Errorf("compiles=%d fallbacks=%d, want 1 and 1\n%s", compilesOf(b), b.precompiler.fallbacks.Load(), logs.String())
		}
	})
	t.Run("the child exceeds its timeout", func(t *testing.T) {
		b, logs := newPrecompileBackend(t)
		b.precompiler.timeout = time.Nanosecond
		runComponent(t, b, distinctComponent(t, 0))
		if compilesOf(b) != 1 || b.precompiler.fallbacks.Load() != 1 || !strings.Contains(logs.String(), "timed out") {
			t.Errorf("compiles=%d fallbacks=%d, want 1 and 1 and a 'timed out' reason\n%s", compilesOf(b), b.precompiler.fallbacks.Load(), logs.String())
		}
	})
	t.Run("the child exits nonzero", func(t *testing.T) {
		b, logs := newPrecompileBackend(t)
		b.precompiler.childEnv = []string{precompileFaultEnv + "=unknown-fault"}
		runComponent(t, b, distinctComponent(t, 0))
		if compilesOf(b) != 1 || b.precompiler.fallbacks.Load() != 1 {
			t.Errorf("compiles=%d fallbacks=%d, want 1 and 1\n%s", compilesOf(b), b.precompiler.fallbacks.Load(), logs.String())
		}
	})
}

// 5. SETUP AND CLEANUP. A relative executable is refused (it would be a PATH-style lookup), and the private directory holds nothing between components and is gone at Close.
func TestThePrecompilerRefusesARelativeExecutableAndLeavesNothingBehind(t *testing.T) {
	b := newComponentBackend(t, WithWasmtimeComponentPrecompile(ComponentPrecompile{Executable: "cleat-worker"}))
	t.Cleanup(func() { _ = b.Close(context.Background()) })
	if b.precompiler != nil {
		t.Fatalf("a relative executable was accepted: the child would be found by lookup, not by the path the worker resolved once")
	}
	runComponent(t, b, distinctComponent(t, 0)) // and the worker still works
	if compilesOf(b) != 1 {
		t.Errorf("a backend whose precompile setup was refused compiled %d time(s), want 1 (in-process)", compilesOf(b))
	}

	pb, _ := newPrecompileBackend(t)
	dir := pb.precompiler.dir
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("the private directory is %v (err %v), want mode 0700", fi, err)
	}
	for i := 0; i < 3; i++ {
		runComponent(t, pb, distinctComponent(t, i))
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Errorf("the private directory holds %d leftover entries after three served components, want 0", len(ents))
	}
	_ = pb.Close(context.Background())
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the private directory survives Close (stat err %v)", err)
	}
}

// 6. ONE CHILD AT A TIME, NO DEADLOCK. Several distinct components missing together are all served, one child after another.
func TestSeveralComponentsMissingTogetherAreAllServedByChildren(t *testing.T) {
	b, logs := newPrecompileBackend(t)
	b.precompiler.childEnv = []string{"CLEAT_PRECOMPILE_TEST_EXCLUSIVE_DIR=" + t.TempDir()}
	const n = 4
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			runComponent(t, b, distinctComponent(t, i))
		}()
	}
	wg.Wait()
	if got := b.precompiler.childCompiles.Load(); got != n || compilesOf(b) != 0 || b.precompiler.fallbacks.Load() != 0 {
		t.Errorf("childCompiles=%d compiles=%d fallbacks=%d, want %d, 0 and 0\n%s", got, compilesOf(b), b.precompiler.fallbacks.Load(), n, logs.String())
	}
}

// 7. THE CHILD'S ENTRY POINT refuses what it should. These run the real function.
func TestTheChildEntryPointRefusesBadInvocations(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.wasm")
	if err := os.WriteFile(in, distinctComponent(t, 0), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out.cwasm")
	run := func(args ...string) (int, string) {
		var so, se bytes.Buffer
		rc := RunSerializeComponentChild(args, &so, &se)
		return rc, se.String()
	}
	if rc, _ := run(); rc == 0 {
		t.Error("no arguments exited 0")
	}
	if rc, _ := run(in); rc == 0 {
		t.Error("one argument exited 0")
	}
	if rc, _ := run(filepath.Join(dir, "missing.wasm"), out); rc == 0 {
		t.Error("a missing input exited 0")
	}
	if rc, _ := run(in, out, "--no-such-flag"); rc == 0 {
		t.Error("an unknown flag exited 0")
	}
	if rc, _ := run(in, out, "stray"); rc == 0 {
		t.Error("a stray argument exited 0")
	}
	if rc, msg := run(in, out); rc != 0 {
		t.Fatalf("a valid invocation exited %d: %s", rc, msg)
	}
	// O_EXCL: the child never overwrites a file that is already where the parent told it to write.
	if rc, _ := run(in, out); rc == 0 {
		t.Error("the child overwrote an existing output file")
	}
	if err := os.WriteFile(in, []byte("not wasm"), 0o600); err != nil {
		t.Fatal(err)
	}
	if rc, _ := run(in, filepath.Join(dir, "out2.cwasm")); rc == 0 {
		t.Error("an input that is not a component exited 0")
	}
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// The child asks to be the kernel's first choice for an out-of-memory kill. The path is redirected to a file, so this asserts the wiring (RunSerializeComponentChild calls it) and the value,
// without needing Linux; the known negative is a path that cannot be written, which must not stop a compile.
func TestTheChildAsksToBeTheFirstChoiceForAnOutOfMemoryKill(t *testing.T) {
	dir := t.TempDir()
	adj := filepath.Join(dir, "oom_score_adj")
	old := oomScoreAdjPath
	oomScoreAdjPath = adj
	t.Cleanup(func() { oomScoreAdjPath = old })

	in, out := filepath.Join(dir, "in.wasm"), filepath.Join(dir, "out.cwasm")
	if err := os.WriteFile(in, distinctComponent(t, 0), 0o600); err != nil {
		t.Fatal(err)
	}
	var so, se bytes.Buffer
	if rc := RunSerializeComponentChild([]string{in, out}, &so, &se); rc != 0 {
		t.Fatalf("child exited %d: %s", rc, se.String())
	}
	if got, err := os.ReadFile(adj); err != nil || string(got) != "1000" {
		t.Errorf("oom_score_adj = %q (err %v), want 1000", got, err)
	}

	oomScoreAdjPath = filepath.Join(dir, "no-such-dir", "oom_score_adj")
	out2 := filepath.Join(dir, "out2.cwasm")
	if rc := RunSerializeComponentChild([]string{in, out2}, &so, &se); rc != 0 {
		t.Errorf("an unwritable oom_score_adj stopped the compile (rc %d): %s", rc, se.String())
	}
}

// A child killed by SIGKILL (the out-of-memory killer, on a container) is NOT answered by compiling in the worker: that needs more memory than the child did and would kill the worker and every run
// it holds. The execution is refused with the reason, the worker lives, and the next execution works once the child does. Measured on a 512 MiB container: the child is killed, the worker falls back,
// and the container dies at 531 MiB.
func TestAChildKilledBySIGKILLIsRefusedNotAnsweredWithACompileInTheWorker(t *testing.T) {
	b, logs := newPrecompileBackend(t)
	b.precompiler.childEnv = []string{precompileFaultEnv + "=kill-self"}

	_, err := b.PerExecution(0).Execute(context.Background(), distinctComponent(t, 0), "run", json.RawMessage(`"x"`), &mockHostHandler{})
	if err == nil || !strings.Contains(err.Error(), "compile child was killed") || !strings.Contains(err.Error(), "memory limit") {
		t.Fatalf("a killed child's execution returned %v, want an error naming the kill and the memory limit\n%s", err, logs.String())
	}
	if compilesOf(b) != 0 {
		t.Errorf("the worker compiled in-process %d time(s) after its child was killed: that is the compile that kills a container", compilesOf(b))
	}
	if b.precompiler.refusals.Load() != 1 || b.precompiler.fallbacks.Load() != 0 || b.precompiler.childCompiles.Load() != 0 {
		t.Errorf("refusals=%d fallbacks=%d child=%d, want 1, 0, 0", b.precompiler.refusals.Load(), b.precompiler.fallbacks.Load(), b.precompiler.childCompiles.Load())
	}

	// Not sticky: nothing was cached, and a healthy child serves the same component.
	b.precompiler.childEnv = nil
	runComponent(t, b, distinctComponent(t, 0))
	if b.precompiler.childCompiles.Load() != 1 || compilesOf(b) != 0 {
		t.Errorf("after the kill, child=%d compiles=%d, want 1 and 0\n%s", b.precompiler.childCompiles.Load(), compilesOf(b), logs.String())
	}
}
