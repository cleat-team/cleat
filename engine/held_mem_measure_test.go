package engine

// MEASUREMENT, not a test: what does one HELD instance cost (cleat#4162, residency memory budget)? Gated on HELDMEM_LANG, so it skips everywhere else. Linux only (reads /proc).
// Env: HELDMEM_LANG=go|python|csharp, HELDMEM_N, HELDMEM_CALLS, HELDMEM_MB. One process per configuration, so the allocator's retention from one cannot leak into the next.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cleat-team/cleat/wasm"
)

type parkCaller struct {
	arrived  atomic.Int32
	guestSys atomic.Uint64 // the guest's own runtime Sys at the park call, summed (Go guest only)
	release  chan struct{}
	resp     string
}

func (c *parkCaller) Call(ctx context.Context, service, op, req string) (string, error) {
	if op == "park" || strings.Contains(req, "park") {
		c.arrived.Add(1)
		if i := strings.Index(req, `"sys":`); i >= 0 {
			if v, err := strconv.ParseUint(strings.TrimRight(strings.TrimSpace(req[i+6:]), "}"), 10, 64); err == nil {
				c.guestSys.Add(v)
			}
		}
		select {
		case <-c.release:
		case <-ctx.Done():
		}
	}
	return c.resp, nil
}

type procMem struct{ anon, file, threads, cgroup, goheap, gosys uint64 }

func readProcMem(t *testing.T) procMem {
	t.Helper()
	var m procMem
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		t.Fatalf("UNMEASURED: /proc/self/status: %v", err)
	}
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Fields(l)
		if len(f) < 2 {
			continue
		}
		v, _ := strconv.ParseUint(f[1], 10, 64)
		switch f[0] {
		case "RssAnon:":
			m.anon = v
		case "RssFile:":
			m.file = v
		case "Threads:":
			m.threads = v
		}
	}
	if m.anon == 0 {
		t.Fatalf("UNMEASURED: no RssAnon in /proc/self/status")
	}
	if c, err := os.ReadFile("/sys/fs/cgroup/memory.current"); err == nil {
		v, _ := strconv.ParseUint(strings.TrimSpace(string(c)), 10, 64)
		m.cgroup = v / 1024
	}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	m.goheap, m.gosys = ms.HeapAlloc/1024, ms.Sys/1024
	return m
}

func TestMeasureHeldInstanceMemory(t *testing.T) {
	lang := os.Getenv("HELDMEM_LANG")
	if lang == "" {
		t.Skip("measurement: set HELDMEM_LANG")
	}
	atoi := func(k string, d int) int {
		if v, err := strconv.Atoi(os.Getenv(k)); err == nil {
			return v
		}
		return d
	}
	n, calls, mb := atoi("HELDMEM_N", 1), atoi("HELDMEM_CALLS", 0), atoi("HELDMEM_MB", 0)

	var wasmBytes []byte
	var entry, input string
	switch lang {
	case "go":
		b, err := os.ReadFile(buildFixtureWasm(t, "heldmem"))
		if err != nil {
			t.Fatal(err)
		}
		wasmBytes, entry, input = b, "held_mem", fmt.Sprintf(`{"calls":%d,"mb":%d}`, calls, mb)
	case "python":
		h := newPythonWasmTestHelper(t)
		if !h.toolsAvailable() {
			t.Fatalf("UNMEASURED: %s", h.missingTools())
		}
		b, err := os.ReadFile(h.compileWorkflow(t, "held_mem_workflow.py", "held_mem"))
		if err != nil {
			t.Fatal(err)
		}
		wasmBytes, entry, input = b, "run", fmt.Sprintf(`{"calls":%d,"mb":%d}`, calls, mb)
	case "javascript":
		b, err := os.ReadFile(filepath.Join("..", "testdata", "heldmem_ts.wasm"))
		if err != nil {
			t.Fatalf("UNMEASURED: %v", err)
		}
		wasmBytes, entry, input = b, "held_mem", fmt.Sprintf(`{"calls":%d,"mb":%d}`, calls, mb)
	case "csharp":
		wasmBytes, entry, input = csharpFixtureStamped(t), "run", `{"mode":"hello","stage":"park"}`
		if calls != 0 || mb != 0 {
			t.Fatalf("UNMEASURED: the checked-in C# fixture has no ballast or pre-calls mode (calls=%d mb=%d)", calls, mb)
		}
	default:
		t.Fatalf("unknown HELDMEM_LANG %q", lang)
	}
	if got := wasm.DetectLanguage(wasmBytes); got != lang {
		t.Fatalf("PRECONDITION FAILED: detected as %q, want %q", got, lang)
	}

	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rt.Close(ctx) })
	wt, err := NewWasmtimeBackend(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { wt.Close(ctx) })
	resp := `{"ok":true,"pad":"` + string(bytes.Repeat([]byte("x"), 2000)) + `"}`

	run := func(c *parkCaller, id string) (string, error) {
		eng := NewEngine(rt, c, WithBackends(WasmtimeLanguages, wt), WithWorkflowID(id))
		res, _, susp, _, _, err := eng.Execute(ctx, wasmBytes, entry, []byte(input))
		if err == nil && susp != nil {
			return res, fmt.Errorf("suspended")
		}
		return res, err
	}

	// Warm-up: compiles and caches the module, and finishes without parking (release is already closed).
	warm := &parkCaller{release: make(chan struct{}), resp: resp}
	close(warm.release)
	if res, err := run(warm, "heldmem-warm"); err != nil {
		t.Fatalf("PRECONDITION FAILED: the warm-up run failed: %v (res=%q)", err, res)
	}
	if warm.arrived.Load() != 1 {
		t.Fatalf("PRECONDITION FAILED: the warm-up reached the park call %d times, want 1", warm.arrived.Load())
	}

	runtime.GC()
	debug.FreeOSMemory()
	time.Sleep(700 * time.Millisecond)
	base := readProcMem(t)

	c := &parkCaller{release: make(chan struct{}), resp: resp}
	var wg sync.WaitGroup
	var failures atomic.Int32
	var lastErr atomic.Value
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := run(c, fmt.Sprintf("heldmem-%d", i)); err != nil {
				failures.Add(1)
				lastErr.Store(err.Error())
			}
		}(i)
	}
	deadline := time.Now().Add(180 * time.Second)
	for int(c.arrived.Load()) < n && time.Now().Before(deadline) && failures.Load() == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if int(c.arrived.Load()) != n {
		t.Fatalf("UNMEASURED: %d of %d guests reached the park call (failures=%d, last=%v)", c.arrived.Load(), n, failures.Load(), lastErr.Load())
	}
	time.Sleep(700 * time.Millisecond)
	held := readProcMem(t)

	close(c.release)
	wg.Wait()
	if failures.Load() != 0 {
		t.Fatalf("UNMEASURED: %d guests failed after release: %v", failures.Load(), lastErr.Load())
	}
	// Only the park call may have been reached by every guest: a guest that did not run its ballast measures nothing.
	d := func(a, b uint64) int64 { return int64(a) - int64(b) }
	fmt.Printf("HELDMEM lang=%s calls=%d mb=%d n=%d base_anon_kib=%d held_anon_kib=%d anon_delta_per_inst_kib=%d file_delta_kib=%d goheap_delta_per_inst_kib=%d gosys_delta_kib=%d cgroup_delta_kib=%d threads_delta=%d guest_sys_per_inst_kib=%d\n",
		lang, calls, mb, n, base.anon, held.anon, d(held.anon, base.anon)/int64(n), d(held.file, base.file), d(held.goheap, base.goheap)/int64(n), d(held.gosys, base.gosys),
		d(held.cgroup, base.cgroup), d(held.threads, base.threads), c.guestSys.Load()/1024/uint64(n))
	_ = filepath.Join
}
