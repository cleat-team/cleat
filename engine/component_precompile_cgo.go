//go:build cgo

package engine

// #cgo CFLAGS: -I${SRCDIR}/wasmtimeinc
// #include <stdlib.h>
// #include <wasmtime.h>
// #include <wasmtime/component/component.h>
//
// static wasmtime_error_t *precompile_serialize(const wasmtime_component_t *c, wasm_byte_vec_t *out) {
//     return wasmtime_component_serialize(c, out);
// }
//
// static wasmtime_error_t *precompile_deserialize_file(const wasm_engine_t *e, const char *path, wasmtime_component_t **out) {
//     return wasmtime_component_deserialize_file(e, path, out);
// }
import "C"

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/bytecodealliance/wasmtime-go/v48"
)

// Compiling a component in a child process (cleat-internal/component-precompile-design-2026-11-23.md, Option A).
//
// THE TRADE, so nobody reads this as more than it is. A component compile raises a process's resident memory by ~490 MiB of Cranelift scratch (Python), and the engine keeps ~145 MiB of the result.
// Done in a child, the scratch goes back to the OS when the child exits and the worker loads the serialized artifact for ~27 MiB (73 ms). That lowers the worker's STEADY STATE. It does NOT lower the
// compile PEAK: the child runs in the worker's container, under the same cgroup limit, so the peak is the worker's baseline plus the child's, at once. parallel_compilation=false in the child roughly
// halves the child's high-water. These figures are the measurement harness's, on one machine; see the PR for what the worker itself measured.
//
// THE ARTIFACT IS NATIVE CODE. wasmtime says deserializing is not safe for arbitrary input. So the worker loads ONLY a file its own child just wrote, into a directory the worker created 0700, at a path
// the worker chose, after checking the size and sha-256 the child reported against the file. No option points deserialize anywhere else.

// componentChildRecord is the one JSON line the child prints on stdout: the evidence that a compile happened in ANOTHER process, and with what settings. The parent's own counters cannot show
// that (a parent-side compiles==0 is also what a bug that loads nothing looks like), so a test asserts on this.
type componentChildRecord struct {
	PID         int    `json:"pid"`
	Compiled    int    `json:"compiled"`
	CompileMs   int64  `json:"compile_ms"`
	SerializeMs int64  `json:"serialize_ms"`
	Bytes       int64  `json:"bytes"`
	SHA256      string `json:"sha256"`
	Fuel        bool   `json:"fuel"`
	Epoch       bool   `json:"epoch"`
	Parallel    bool   `json:"parallel_compilation"`
	MaxRSSKiB   int64  `json:"max_rss_kib"`
}

func precompileErrString(err *C.wasmtime_error_t) string {
	var msg C.wasm_byte_vec_t
	C.wasmtime_error_message(err, &msg)
	s := C.GoStringN(msg.data, C.int(msg.size))
	C.wasm_byte_vec_delete(&msg)
	C.wasmtime_error_delete(err)
	return s
}

// maxRSSKiB is this process's peak resident size in KiB. getrusage reports KiB on Linux and BYTES on macOS.
func maxRSSKiB() int64 {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	if runtime.GOOS == "darwin" {
		return int64(ru.Maxrss) / 1024 //nolint:unconvert // Maxrss is int32 on 32-bit Linux, int64 here
	}
	return int64(ru.Maxrss) //nolint:unconvert // as above
}

// oomScoreAdjPath is where the child asks to be the kernel's first choice for an out-of-memory kill. A var so a test can point it at a file.
var oomScoreAdjPath = "/proc/self/oom_score_adj"

// lowerOOMPriority makes this process the preferred victim of a cgroup out-of-memory kill. The child exists to be thrown away: if the container runs out of memory during a compile, the kernel should
// kill the child, which the worker survives (it logs and compiles in-process), rather than choose the worker that holds every in-flight run. Raising one's own score needs no privilege. A kernel
// without the file (not Linux) is not an error.
func lowerOOMPriority(path string) error {
	return os.WriteFile(path, []byte("1000"), 0o644)
}

// RunSerializeComponentChild is the child: `<in.wasm> <out.cwasm> [--instruction-limit N] [--parallel-compilation=false]`. It compiles the component with the engine config the worker would
// use, serializes it to out, prints one componentChildRecord line on stdout and returns the process exit code. Nothing else is read from the environment or the filesystem.
func RunSerializeComponentChild(args []string, stdout, stderr io.Writer) int {
	fail := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "serialize-component: "+format+"\n", a...)
		return 2
	}
	if len(args) < 2 {
		return fail("usage: %s <in.wasm> <out.cwasm> [--instruction-limit N] [--parallel-compilation=BOOL]", SerializeComponentArg)
	}
	in, out := args[0], args[1]
	_ = lowerOOMPriority(oomScoreAdjPath) // best effort: absent off Linux
	fs := flag.NewFlagSet(SerializeComponentArg, flag.ContinueOnError)
	fs.SetOutput(stderr)
	limit := fs.Uint64("instruction-limit", 0, "fuel is enabled iff this is > 0")
	parallel := fs.Bool("parallel-compilation", true, "wasmtime parallel_compilation")
	if err := fs.Parse(args[2:]); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		return fail("unexpected arguments: %v", fs.Args())
	}

	wasm, err := os.ReadFile(in)
	if err != nil {
		return fail("read input: %v", err)
	}
	eng := newWasmtimeEngine(*limit > 0, *parallel)
	defer eng.Close()

	t0 := time.Now()
	comp, err := componentCompile(eng, wasm)
	if err != nil {
		return fail("%v", err)
	}
	defer C.wasmtime_component_delete(comp)
	compileMs := time.Since(t0).Milliseconds()

	t1 := time.Now()
	var vec C.wasm_byte_vec_t
	if cerr := C.precompile_serialize(comp, &vec); cerr != nil {
		return fail("serialize: %s", precompileErrString(cerr))
	}
	defer C.wasm_byte_vec_delete(&vec)
	data := unsafe.Slice((*byte)(unsafe.Pointer(vec.data)), int(vec.size))

	// O_EXCL: the parent created an empty directory and chose this name; a file already there is not the child's to overwrite.
	f, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fail("create output: %v", err)
	}
	sum := sha256.Sum256(data)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fail("write output: %v", err)
	}
	if err := f.Close(); err != nil {
		return fail("close output: %v", err)
	}
	rec := componentChildRecord{
		PID: os.Getpid(), Compiled: 1, CompileMs: compileMs, SerializeMs: time.Since(t1).Milliseconds(),
		Bytes: int64(len(data)), SHA256: hex.EncodeToString(sum[:]),
		Fuel: *limit > 0, Epoch: true, Parallel: *parallel, MaxRSSKiB: maxRSSKiB(),
	}
	if err := json.NewEncoder(stdout).Encode(rec); err != nil {
		return fail("write record: %v", err)
	}
	return 0
}

// componentPrecompiler is the parent's side: it runs one child per cache miss, one at a time, and loads what the child wrote.
type componentPrecompiler struct {
	exe              string
	dir              string
	timeout          time.Duration
	instructionLimit uint64
	parallel         bool
	log              *slog.Logger
	sem              chan struct{} // one child at a time: a second child would double the peak this exists to bound

	// childEnv is the child's ENTIRE environment. nil means empty, never "inherit": the child needs nothing from the worker's environment, which holds database credentials.
	childEnv []string
	// childInstructionLimit overrides what the child is told, for a test that makes the child's fuel setting disagree with the worker's.
	childInstructionLimit *uint64

	childCompiles atomic.Int64 // components served from a child's artifact
	fallbacks     atomic.Int64 // misses where the child path failed and the worker compiled in-process
	refusals      atomic.Int64 // misses refused because the child was killed (see ComponentCompileRefusedError)
	spawns        atomic.Int64 // children started

	memLimit uint64
	window   time.Duration
	// now is the clock the refusal window reads; a test replaces it.
	now          func() time.Time
	mu           sync.Mutex
	refusedUntil map[string]time.Time // component key -> when this worker will try it again
	lastRecord   atomic.Pointer[componentChildRecord]
}

func newComponentPrecompiler(cfg ComponentPrecompile, instructionLimit uint64, logger *slog.Logger) (*componentPrecompiler, error) {
	if !filepath.IsAbs(cfg.Executable) {
		return nil, fmt.Errorf("component precompile: executable %q is not an absolute path", cfg.Executable)
	}
	if logger == nil {
		logger = slog.Default()
	}
	dir, err := os.MkdirTemp("", "cleat-precompile-") // 0700
	if err != nil {
		return nil, fmt.Errorf("component precompile: private directory: %w", err)
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultComponentPrecompileTimeout
	}
	window := cfg.RefusalWindow
	if window <= 0 {
		window = DefaultComponentPrecompileRefusalWindow
	}
	return &componentPrecompiler{
		exe: cfg.Executable, dir: dir, timeout: timeout, instructionLimit: instructionLimit,
		parallel: cfg.ParallelCompilation, log: logger, sem: make(chan struct{}, 1),
		memLimit: cfg.MemoryLimitBytes, window: window, now: time.Now, refusedUntil: map[string]time.Time{},
	}, nil
}

// close removes the private directory. Every artifact is already deleted on every path of compile; this removes the directory itself.
func (p *componentPrecompiler) close() {
	if p != nil && p.dir != "" {
		_ = os.RemoveAll(p.dir)
	}
}

// limitedBuffer keeps at most max bytes, so a child that prints without end cannot grow the worker.
type limitedBuffer struct {
	bytes.Buffer
	max int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.Len(); room > 0 {
		if len(p) > room {
			b.Buffer.Write(p[:room])
		} else {
			b.Buffer.Write(p)
		}
	}
	return len(p), nil
}

// compile has a child compile and serialize wasm, verifies what it wrote and loads it. A non-nil error means the caller must compile in-process; the stage names what failed. The artifact and its
// directory are removed on every path.
func (p *componentPrecompiler) compile(engine *wasmtime.Engine, wasm []byte) (*C.wasmtime_component_t, componentChildRecord, error) {
	var zero componentChildRecord
	p.sem <- struct{}{}
	defer func() { <-p.sem }()

	work, err := os.MkdirTemp(p.dir, "c-")
	if err != nil {
		return nil, zero, fmt.Errorf("workdir: %w", err)
	}
	defer os.RemoveAll(work)
	in, out := filepath.Join(work, "in.wasm"), filepath.Join(work, "out.cwasm")
	if err := os.WriteFile(in, wasm, 0o600); err != nil {
		return nil, zero, fmt.Errorf("write input: %w", err)
	}

	limit := p.instructionLimit
	if p.childInstructionLimit != nil {
		limit = *p.childInstructionLimit
	}
	ctx, cancel := context.WithTimeout(context.Background(), p.timeout)
	defer cancel()
	p.spawns.Add(1)
	cmd := exec.CommandContext(ctx, p.exe, SerializeComponentArg, in, out,
		"--instruction-limit", fmt.Sprint(limit), fmt.Sprintf("--parallel-compilation=%t", p.parallel))
	cmd.Env = append([]string{}, p.childEnv...) // non-nil: an empty environment, not the worker's
	cmd.Dir = work
	var stdout, stderr limitedBuffer
	stdout.max, stderr.max = 16<<10, 16<<10
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, zero, fmt.Errorf("child: timed out after %s: %w", p.timeout, err)
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() && ws.Signal() == syscall.SIGKILL {
				return nil, zero, &errPrecompileChildKilled{cause: err}
			}
		}
		return nil, zero, fmt.Errorf("child: %w: %s", err, strings.TrimSpace(stderr.String()))
	}

	var rec componentChildRecord
	line := strings.TrimSpace(stdout.String())
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		return nil, zero, fmt.Errorf("record: %w (stdout %q)", err, line)
	}
	if rec.Compiled != 1 || rec.PID == os.Getpid() || rec.PID <= 0 {
		return nil, zero, fmt.Errorf("record: not a compile by another process (compiled=%d pid=%d)", rec.Compiled, rec.PID)
	}

	// The worker loads a file only if it is the one its child just wrote: same path, the size and hash the child reported.
	fi, err := os.Lstat(out)
	if err != nil || !fi.Mode().IsRegular() {
		return nil, rec, fmt.Errorf("artifact: not a regular file where the child was told to write it (%v)", err)
	}
	if fi.Size() != rec.Bytes {
		return nil, rec, fmt.Errorf("artifact: %d bytes on disk, the child reported %d", fi.Size(), rec.Bytes)
	}
	f, err := os.Open(out)
	if err != nil {
		return nil, rec, fmt.Errorf("artifact: %w", err)
	}
	h := sha256.New()
	_, cerr := io.Copy(h, f)
	f.Close()
	if cerr != nil {
		return nil, rec, fmt.Errorf("artifact: hash: %w", cerr)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != rec.SHA256 {
		return nil, rec, fmt.Errorf("artifact: sha256 %s does not match the child's %s", got, rec.SHA256)
	}

	cpath := C.CString(out)
	defer C.free(unsafe.Pointer(cpath))
	var comp *C.wasmtime_component_t
	if cerr := C.precompile_deserialize_file(getEnginePtr(engine), cpath, &comp); cerr != nil {
		return nil, rec, fmt.Errorf("deserialize: %s", precompileErrString(cerr))
	}
	return comp, rec, nil
}

// errPrecompileChildKilled is a child that died of SIGKILL, which in a container is nearly always the out-of-memory killer. compileComponentMiss turns it into a ComponentCompileRefusedError.
type errPrecompileChildKilled struct{ cause error }

func (e *errPrecompileChildKilled) Error() string {
	return "the precompile child was killed: " + e.cause.Error()
}
func (e *errPrecompileChildKilled) Unwrap() error { return e.cause }

// refusalFor returns how long key is still refused on this worker, and whether it is.
func (p *componentPrecompiler) refusalFor(key string) (time.Duration, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	until, ok := p.refusedUntil[key]
	if !ok {
		return 0, false
	}
	if left := until.Sub(p.now()); left > 0 {
		return left, true
	}
	delete(p.refusedUntil, key) // expired: forget it, so the map holds only live refusals
	return 0, false
}

// refuse records that key's child was killed and returns the error that releases the run. repeat is true when no child was started.
func (p *componentPrecompiler) refuse(key string, cause error, left time.Duration, repeat bool) error {
	if !repeat {
		p.mu.Lock()
		p.refusedUntil[key] = p.now().Add(p.window)
		p.mu.Unlock()
		left = p.window
	}
	p.refusals.Add(1)
	return &ComponentCompileRefusedError{MemoryLimitBytes: p.memLimit, ParallelCompilation: p.parallel, RetryAfter: left, Window: p.window, Repeat: repeat, Cause: cause}
}

// compileComponentMiss is acquireComponent's compile step. It returns the component and whether it was compiled IN THIS PROCESS, which is what componentCache.compiles counts. With a precompiler
// configured it tries the child first and falls back to the in-process compile on any failure, logged and counted, never returned as an error.
func (b *wasmtimeBackend) compileComponentMiss(key string, wasmBytes []byte) (*C.wasmtime_component_t, bool, error) {
	if p := b.precompiler; p != nil {
		if left, refused := p.refusalFor(key); refused {
			// One attempt per component per window: a run released for retry must not respawn a child that was just killed.
			err := p.refuse(key, nil, left, true)
			b.log().Warn("component compile refused without trying: its precompile child was killed within the refusal window", "retry_after", left.Round(time.Second))
			return nil, false, err
		}
		comp, rec, err := p.compile(b.engine, wasmBytes)
		if err == nil {
			p.childCompiles.Add(1)
			p.lastRecord.Store(&rec)
			b.log().Info("component compiled in a child process", "child_pid", rec.PID, "compile_ms", rec.CompileMs, "artifact_bytes", rec.Bytes,
				"child_max_rss_kib", rec.MaxRSSKiB, "parallel_compilation", rec.Parallel)
			return comp, false, nil
		}
		var killed *errPrecompileChildKilled
		if errors.As(err, &killed) {
			// SIGKILL is, on a container, almost always the kernel's out-of-memory kill (the child asks to be its first choice, see lowerOOMPriority). Compiling in THIS process needs more memory than
			// the child did, so it would be killed too, with every run it holds, and a restart would replay into the same compile. Refuse this one execution with the reason instead.
			refusal := p.refuse(key, err, 0, false)
			b.log().Error("component precompile child was killed; not compiling in this process, which would need more memory",
				"memory_limit_mib", p.memLimit>>20, "parallel_compilation", p.parallel, "retry_after", p.window.Round(time.Second), "error", refusal)
			return nil, false, refusal
		}
		p.fallbacks.Add(1)
		b.log().Warn("component precompile in a child failed; compiling in this process", "error", err)
	}
	comp, err := componentCompile(b.engine, wasmBytes)
	return comp, true, err
}
