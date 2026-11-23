package engine

// MEASUREMENT, not a test: what does one HELD instance cost, and how much of it is shared (cleat#4377)? Gated on HELDMEM_LANG, so it skips everywhere else. Linux only (reads /proc).
//
// Env: HELDMEM_LANG=go|python|csharp|javascript  HELDMEM_N  HELDMEM_CALLS  HELDMEM_MB  HELDMEM_MODE=hold|block  HELDMEM_OUT=<file to append one JSON line to>  HELDMEM_WASM_OUT=<dir to save the guest in>
//
//	hold  - the REAL residency path: the guest makes a durable sleep, the resident set admits it, and the run is held (the execution never returns to the worker).
//	block - the guest is parked inside a blocking host call (design 9a's condition), with no resident set.
//
// One process per configuration, so the allocator's retention from one cannot leak into the next.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"sort"
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
	info     atomic.Value  // the first request of an "info" call: the guest describing its own memory (C# only)
	info2    atomic.Value  // the first request of an "info2" call: the same, after a collection (C# only)
	release  chan struct{}
	resp     string
}

func (c *parkCaller) Call(ctx context.Context, service, op, req string) (string, error) {
	if op == "info" {
		c.info.CompareAndSwap(nil, req)
	}
	if op == "info2" {
		c.info2.CompareAndSwap(nil, req)
	}
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

// snap is one reading of the process, in KiB except the counts.
type snap struct {
	Anon, File, Shmem, Rss     uint64 // /proc/self/status
	Threads                    uint64
	Pss, PssAnon, PssFile      uint64 // smaps_rollup
	PssShmem                   uint64
	SharedClean, SharedDirty   uint64
	PrivateClean, PrivateDirty uint64
	MemfdMaps, MemfdRss        uint64   // mappings backed by a memfd: what a copy-on-write memory image looks like in smaps
	ExecAnonRss                uint64   // anonymous executable mappings: JIT-compiled code
	TopMaps                    []string // the six largest mappings by resident size: "rssKiB sizeKiB perms path"
	StackMaps                  uint64   // mappings of about 8 MiB with a stack-like shape
	StackRss                   uint64
	Cgroup                     uint64
	GoHeap, GoSys, GoStackSys  uint64
}

var stackSize = regexp.MustCompile(`^Size:\s+(\d+) kB`)
var mapHeader = regexp.MustCompile(`^[0-9a-f]+-[0-9a-f]+ (\S+) \S+ \S+ \S+\s*(.*)$`)

func readSnap(t *testing.T) snap {
	t.Helper()
	var m snap
	kv := func(path string, set func(k string, v uint64)) {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("UNMEASURED: %s: %v", path, err)
		}
		for _, l := range strings.Split(string(b), "\n") {
			f := strings.Fields(l)
			if len(f) < 2 || !strings.HasSuffix(f[0], ":") {
				continue
			}
			if v, err := strconv.ParseUint(f[1], 10, 64); err == nil {
				set(strings.TrimSuffix(f[0], ":"), v)
			}
		}
	}
	kv("/proc/self/status", func(k string, v uint64) {
		switch k {
		case "RssAnon":
			m.Anon = v
		case "RssFile":
			m.File = v
		case "RssShmem":
			m.Shmem = v
		case "VmRSS":
			m.Rss = v
		case "Threads":
			m.Threads = v
		}
	})
	kv("/proc/self/smaps_rollup", func(k string, v uint64) {
		switch k {
		case "Pss":
			m.Pss = v
		case "Pss_Anon":
			m.PssAnon = v
		case "Pss_File":
			m.PssFile = v
		case "Pss_Shmem":
			m.PssShmem = v
		case "Shared_Clean":
			m.SharedClean = v
		case "Shared_Dirty":
			m.SharedDirty = v
		case "Private_Clean":
			m.PrivateClean = v
		case "Private_Dirty":
			m.PrivateDirty = v
		}
	})
	if m.Anon == 0 || m.Pss == 0 {
		t.Fatalf("UNMEASURED: RssAnon=%d Pss=%d", m.Anon, m.Pss)
	}
	// Thread stacks: anonymous mappings of 8 MiB (the glibc default, ulimit -s). A shape, not an identity: it is the sum of their RESIDENT part that matters.
	b, err := os.ReadFile("/proc/self/smaps")
	if err != nil {
		t.Fatalf("UNMEASURED: smaps: %v", err)
	}
	var size uint64
	var perms, path string
	type mapRow struct {
		rss, size   uint64
		perms, path string
	}
	var rows []mapRow
	for _, l := range strings.Split(string(b), "\n") {
		if hm := mapHeader.FindStringSubmatch(l); hm != nil {
			perms, path = hm[1], hm[2]
			continue
		}
		if mm := stackSize.FindStringSubmatch(l); mm != nil {
			size, _ = strconv.ParseUint(mm[1], 10, 64)
			continue
		}
		if strings.HasPrefix(l, "Rss:") {
			f := strings.Fields(l)
			v, _ := strconv.ParseUint(f[1], 10, 64)
			rows = append(rows, mapRow{v, size, perms, path})
			if size >= 8180 && size <= 8200 && path == "" {
				m.StackMaps++
				m.StackRss += v
			}
			if strings.Contains(path, "memfd") {
				m.MemfdMaps++
				m.MemfdRss += v
			}
			if path == "" && strings.Contains(perms, "x") {
				m.ExecAnonRss += v
			}
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].rss > rows[j].rss })
	for i := 0; i < len(rows) && i < 6; i++ {
		m.TopMaps = append(m.TopMaps, fmt.Sprintf("%d %d %s %s", rows[i].rss, rows[i].size, rows[i].perms, rows[i].path))
	}
	if c, err := os.ReadFile("/sys/fs/cgroup/memory.current"); err == nil {
		v, _ := strconv.ParseUint(strings.TrimSpace(string(c)), 10, 64)
		m.Cgroup = v / 1024
	}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	m.GoHeap, m.GoSys, m.GoStackSys = ms.HeapAlloc/1024, ms.Sys/1024, ms.StackSys/1024
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
	mode := os.Getenv("HELDMEM_MODE")
	if mode == "" {
		mode = "hold"
	}
	if mode != "hold" && mode != "block" {
		t.Fatalf("HELDMEM_MODE must be hold or block, got %q", mode)
	}
	const holdMs = 600000
	sleepMs := 0
	if mode == "hold" {
		sleepMs = holdMs
	}
	input := func(sleep int) string { return fmt.Sprintf(`{"calls":%d,"mb":%d,"sleep_ms":%d}`, calls, mb, sleep) }

	var wasmBytes []byte
	var entry string
	var mkInput func(sleep int) string = input
	switch lang {
	case "go":
		b, err := os.ReadFile(buildFixtureWasm(t, "heldmem"))
		if err != nil {
			t.Fatal(err)
		}
		// The Go fixture reads the first three digit runs as calls, mb, sleep_ms.
		wasmBytes, entry = b, "held_mem"
	case "python":
		if v := os.Getenv("HELDMEM_PY_WASM"); v != "" { // a prebuilt (possibly rewritten) component instead of compiling the guest
			b, err := os.ReadFile(v)
			if err != nil {
				t.Fatalf("UNMEASURED: %v", err)
			}
			wasmBytes, entry = b, "run"
			break
		}
		h := newPythonWasmTestHelper(t)
		if !h.toolsAvailable() {
			t.Fatalf("UNMEASURED: %s", h.missingTools())
		}
		b, err := os.ReadFile(h.compileWorkflow(t, "held_mem_workflow.py", "held_mem"))
		if err != nil {
			t.Fatal(err)
		}
		wasmBytes, entry = b, "run"
	case "javascript":
		b, err := os.ReadFile(filepath.Join("..", "testdata", "heldmem_ts.wasm"))
		if err != nil {
			t.Fatalf("UNMEASURED: %v", err)
		}
		wasmBytes, entry = b, "held_mem"
	case "csharp":
		path := filepath.Join("..", "testdata", "heldmem_cs.wasm")
		if v := os.Getenv("HELDMEM_CS_WASM"); v != "" {
			path = v
		}
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("UNMEASURED: %v (the held-memory C# guest is built separately)", err)
		}
		b, err = wasm.WriteMetadata(b, &wasm.Metadata{Language: "csharp"})
		if err != nil {
			t.Fatal(err)
		}
		wasmBytes, entry = b, "run"
		mkInput = func(sleep int) string {
			return fmt.Sprintf(`{"mode":"heldmem","calls":"%d","mb":"%d","sleep_ms":"%d"}`, calls, mb, sleep)
		}
	default:
		t.Fatalf("unknown HELDMEM_LANG %q", lang)
	}
	if got := wasm.DetectLanguage(wasmBytes); got != lang {
		t.Fatalf("PRECONDITION FAILED: detected as %q, want %q", got, lang)
	}
	if d := os.Getenv("HELDMEM_WASM_OUT"); d != "" {
		_ = os.WriteFile(filepath.Join(d, lang+".wasm"), wasmBytes, 0o644)
	}

	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rt.Close(ctx) })
	var backendOpts []WasmtimeOption
	if mb := atoi("HELDMEM_MEMLIMIT_MB", 0); mb > 0 { // an explicit per-store linear-memory cap: the least cap a guest still STARTS under is its start-up linear memory
		backendOpts = append(backendOpts, WithWasmtimeMemoryLimits(int64(mb)<<20, 0, 0))
	}
	wt, err := NewWasmtimeBackend(ctx, backendOpts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { wt.Close(ctx) })
	resp := `{"ok":true,"pad":"` + string(bytes.Repeat([]byte("x"), 2000)) + `"}`

	// The resident set (hold mode): every language charged ONE byte, so chargedBytes(set) is the number of runs held. The knobs are set far above anything measured.
	var set *ResidentSet
	if mode == "hold" {
		cfg := DefaultResidencyConfig()
		cfg.MaxRuns = 100000
		cfg.MemoryBudgetBytes = 1 << 40
		cfg.InstanceBytes = map[string]uint64{"go": 1, "python": 1, "csharp": 1, "javascript": 1}
		cfg.WindowFloor, cfg.WindowCap = 20*time.Minute, 30*time.Minute
		set, err = NewResidentSet(cfg)
		if err != nil {
			t.Fatal(err)
		}
	}

	run := func(ctx context.Context, c *parkCaller, id string, sleep int) (string, error) {
		opts := []EngineOption{WithBackends(WasmtimeLanguages, wt), WithWorkflowID(id)}
		if set != nil {
			opts = append(opts, WithResidency(set.ForRun(ResidentRunInfo{WorkflowID: id, Language: lang, ClaimCheck: func(context.Context) error { return nil }})))
		}
		eng := NewEngine(rt, c, opts...)
		res, _, susp, _, _, err := eng.Execute(ctx, wasmBytes, entry, []byte(mkInput(sleep)))
		if err == nil && susp != nil {
			return res, fmt.Errorf("suspended")
		}
		return res, err
	}

	pre := readSnap(t)

	// Warm-up: compiles and caches the module and finishes (a held 300 ms in hold mode; an already-released park in block mode).
	warm := &parkCaller{release: make(chan struct{}), resp: resp}
	close(warm.release)
	warmSleep := 0
	if mode == "hold" {
		warmSleep = 300
	}
	if res, err := run(ctx, warm, "heldmem-warm", warmSleep); err != nil {
		t.Fatalf("PRECONDITION FAILED: the warm-up run failed: %v (res=%q)", err, res)
	}
	if mode == "block" && warm.arrived.Load() != 1 {
		t.Fatalf("PRECONDITION FAILED: the warm-up reached the park call %d times, want 1", warm.arrived.Load())
	}
	if set != nil && chargedBytes(set) != 0 {
		t.Fatalf("PRECONDITION FAILED: the set is still charged %d after the warm-up", chargedBytes(set))
	}

	runtime.GC()
	debug.FreeOSMemory()
	time.Sleep(700 * time.Millisecond)
	base := readSnap(t)
	compBase := compilesOf(wt)

	c := &parkCaller{release: make(chan struct{}), resp: resp}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	tStart := time.Now()
	var ended atomic.Int32
	var otherErrs atomic.Int32
	var lastErr atomic.Value
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := run(runCtx, c, fmt.Sprintf("heldmem-%d", i), sleepMs)
			ended.Add(1)
			if err != nil && runCtx.Err() == nil {
				otherErrs.Add(1)
				lastErr.Store(err.Error())
			}
		}(i)
	}
	reached := func() int {
		if mode == "hold" {
			return int(chargedBytes(set))
		}
		return int(c.arrived.Load())
	}
	deadline := time.Now().Add(240 * time.Second)
	for reached() < n && time.Now().Before(deadline) && otherErrs.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	msToHeld := float64(time.Since(tStart).Microseconds()) / 1000
	if reached() != n {
		t.Fatalf("UNMEASURED: %d of %d guests are held (errors=%d, last=%v)", reached(), n, otherErrs.Load(), lastErr.Load())
	}
	time.Sleep(700 * time.Millisecond)
	if ended.Load() != 0 {
		t.Fatalf("UNMEASURED: %d executions had already ended while %d were meant to be held", ended.Load(), n)
	}
	held := readSnap(t)
	heldCheck := reached()

	if mode == "hold" {
		cancel()
	} else {
		close(c.release)
	}
	wg.Wait()
	if otherErrs.Load() != 0 {
		t.Fatalf("UNMEASURED: %d guests failed other than by the release: %v", otherErrs.Load(), lastErr.Load())
	}

	out := map[string]any{
		"lang": lang, "mode": mode, "n": n, "calls": calls, "mb": mb, "wasm_bytes": len(wasmBytes),
		"held_check": heldCheck, "pre": pre, "base": base, "held": held,
		"compiles_component": compilesOf(wt), "compiles_component_before_hold": compBase, "compiles_core": heldMemCoreCompiles.Load(),
		"guest_sys_kib_sum": c.guestSys.Load() / 1024, "ms_to_held": msToHeld, "guest_info": c.info.Load(), "guest_info2": c.info2.Load(),
	}
	line, _ := json.Marshal(out)
	if f := os.Getenv("HELDMEM_OUT"); f != "" {
		fh, err := os.OpenFile(f, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		fh.Write(append(line, '\n'))
		fh.Close()
	}
	fmt.Printf("HELDMEM %s\n", line)
}
