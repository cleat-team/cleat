package engine

// MEASUREMENT, not a test (cleat#4377): instantiate ONE core module N times in plain wasmtime-go and report whether each instance got a copy-on-write image.
// Env: HELDMEM_IMAGE_WASM=<core module>  HELDMEM_N  HELDMEM_COW=0 (the measurement switch)  HELDMEM_IMAGE_OUT=<jsonl>

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/bytecodealliance/wasmtime-go/v48"
)

func TestMeasureStandaloneImage(t *testing.T) {
	path := os.Getenv("HELDMEM_IMAGE_WASM")
	if path == "" {
		t.Skip("measurement: set HELDMEM_IMAGE_WASM")
	}
	n, _ := strconv.Atoi(os.Getenv("HELDMEM_N"))
	if n == 0 {
		n = 8
	}
	wasmBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("UNMEASURED: %v", err)
	}
	cfg := wasmtime.NewConfig()
	if os.Getenv("HELDMEM_COW") == "0" {
		cfg.SetMemoryInitCOWSet(false)
	} else if os.Getenv("HELDMEM_COW") == "1" {
		cfg.SetMemoryInitCOWSet(true)
	}
	eng := wasmtime.NewEngineWithConfig(cfg)
	module, err := wasmtime.NewModule(eng, wasmBytes)
	if err != nil {
		t.Fatalf("UNMEASURED: compile: %v", err)
	}
	mk := func() (*wasmtime.Store, *wasmtime.Instance) {
		store := wasmtime.NewStore(eng)
		linker := wasmtime.NewLinker(eng)
		if err := linker.DefineUnknownImportsAsTraps(module); err != nil {
			t.Fatalf("UNMEASURED: stubbing imports: %v", err)
		}
		inst, err := linker.Instantiate(store, module)
		if err != nil {
			t.Fatalf("UNMEASURED: instantiate: %v", err)
		}
		return store, inst
	}
	// Warm-up instance, then released.
	s0, i0 := mk()
	mem0 := i0.GetExport(s0, "memory")
	var pages0 uint64
	if mem0 != nil && mem0.Memory() != nil {
		pages0 = mem0.Memory().Size(s0)
	}
	var memHash string
	if mem0 != nil && mem0.Memory() != nil {
		sum := sha256.Sum256(mem0.Memory().UnsafeData(s0))
		memHash = hex.EncodeToString(sum[:])
	}
	runtime.KeepAlive(i0)
	s0 = nil
	runtime.GC()
	debug.FreeOSMemory()
	time.Sleep(500 * time.Millisecond)
	base := readSnap(t)
	var keep []*wasmtime.Store
	var insts []*wasmtime.Instance
	t0 := time.Now()
	for i := 0; i < n; i++ {
		s, in := mk()
		keep, insts = append(keep, s), append(insts, in)
	}
	elapsed := time.Since(t0)
	time.Sleep(500 * time.Millisecond)
	held := readSnap(t)
	out := map[string]any{"wasm": path, "n": n, "cow": os.Getenv("HELDMEM_COW"), "memory_pages": pages0, "memory_sha256": memHash, "base": base, "held": held, "ms_per_instantiate": float64(elapsed.Microseconds()) / 1000 / float64(n)}
	line, _ := json.Marshal(out)
	if f := os.Getenv("HELDMEM_IMAGE_OUT"); f != "" {
		fh, _ := os.OpenFile(f, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		fh.Write(append(line, '\n'))
		fh.Close()
	}
	fmt.Printf("IMAGE %s\n", line)
	runtime.KeepAlive(keep)
	runtime.KeepAlive(insts)
}

// TestMeasureStandaloneInit runs a core module's `_initialize` export on its own, with every import returning zeros, and reads the linear memory afterwards: its size,
// which pages are resident (mincore) and where, and which are non-zero. cleat#4378 (what a .NET guest writes at start-up).
func TestMeasureStandaloneInit(t *testing.T) {
	path := os.Getenv("HELDMEM_INIT_WASM")
	if path == "" {
		t.Skip("measurement: set HELDMEM_INIT_WASM")
	}
	wasmBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("UNMEASURED: %v", err)
	}
	eng := wasmtime.NewEngine()
	module, err := wasmtime.NewModule(eng, wasmBytes)
	if err != nil {
		t.Fatalf("UNMEASURED: compile: %v", err)
	}
	store := wasmtime.NewStore(eng)
	linker := wasmtime.NewLinker(eng)
	if err := linker.DefineUnknownImportsAsDefaultValues(store, module); err != nil {
		t.Fatalf("UNMEASURED: stubbing imports: %v", err)
	}
	inst, err := linker.Instantiate(store, module)
	if err != nil {
		t.Fatalf("UNMEASURED: instantiate: %v", err)
	}
	mem := inst.GetExport(store, "memory").Memory()
	before := mem.Size(store)
	beforeRSS := readSnap(t)
	initFn := inst.GetFunc(store, "_initialize")
	if initFn == nil {
		t.Fatalf("UNMEASURED: no _initialize export")
	}
	_, callErr := initFn.Call(store)
	after := mem.Size(store)
	afterRSS := readSnap(t)
	// Drive the guest the way the component adapter would: allocate the argument string with cabi_realloc, then call run(ptr, len) (mode "alloc", which makes no host call).
	var steps []string
	steps = append(steps, fmt.Sprintf("after _initialize: %d pages", after))
	if arg := os.Getenv("HELDMEM_INIT_ARG"); arg != "" {
		realloc := inst.GetFunc(store, "cabi_realloc")
		run := inst.GetFunc(store, "run")
		if realloc == nil || run == nil {
			t.Fatalf("UNMEASURED: missing cabi_realloc/run")
		}
		pv, err := realloc.Call(store, int32(0), int32(0), int32(1), int32(len(arg)))
		if err != nil {
			t.Fatalf("UNMEASURED: cabi_realloc: %v", err)
		}
		steps = append(steps, fmt.Sprintf("after cabi_realloc: %d pages", mem.Size(store)))
		ptr := pv.(int32)
		copy(mem.UnsafeData(store)[ptr:], arg)
		_, callErr = run.Call(store, ptr, int32(len(arg)))
		steps = append(steps, fmt.Sprintf("after run(%q): %d pages (call error: %v)", arg, mem.Size(store), callErr))
		after = mem.Size(store)
		afterRSS = readSnap(t)
	}
	for _, st := range steps {
		fmt.Printf("INIT step: %s\n", st)
	}
	data := mem.UnsafeData(store)
	const pg = 4096
	n := (len(data) + pg - 1) / pg
	vec := make([]byte, n)
	_, _, errno := syscall.Syscall(syscall.SYS_MINCORE, uintptr(unsafe.Pointer(&data[0])), uintptr(len(data)), uintptr(unsafe.Pointer(&vec[0])))
	if errno != 0 {
		t.Fatalf("UNMEASURED: mincore: %v", errno)
	}
	type run struct{ start, length int }
	runs := func(pred func(i int) bool) (rs []run, total int) {
		for i := 0; i < n; i++ {
			if pred(i) {
				total++
				if len(rs) > 0 && rs[len(rs)-1].start+rs[len(rs)-1].length == i {
					rs[len(rs)-1].length++
				} else {
					rs = append(rs, run{i, 1})
				}
			}
		}
		return
	}
	nonzero := func(i int) bool {
		end := (i + 1) * pg
		if end > len(data) {
			end = len(data)
		}
		for _, b := range data[i*pg : end] {
			if b != 0 {
				return true
			}
		}
		return false
	}
	resident, resTotal := runs(func(i int) bool { return vec[i]&1 == 1 })
	nz, nzTotal := runs(nonzero)
	fmtRuns := func(rs []run) string {
		s := ""
		for i, r := range rs {
			if i >= 12 {
				s += fmt.Sprintf(" ... (+%d more runs)", len(rs)-12)
				break
			}
			s += fmt.Sprintf(" [%.2f-%.2f MiB]", float64(r.start*pg)/(1<<20), float64((r.start+r.length)*pg)/(1<<20))
		}
		return s
	}
	fmt.Printf("INIT callErr=%v\n", callErr)
	fmt.Printf("INIT linear memory: %d pages (%.2f MiB) before _initialize -> %d pages (%.2f MiB) after\n", before, float64(before)*64/1024, after, float64(after)*64/1024)
	fmt.Printf("INIT process Anon delta over _initialize: %.2f MiB\n", float64(afterRSS.Anon-beforeRSS.Anon)/1024)
	fmt.Printf("INIT resident 4K pages in the linear memory: %d of %d = %.2f MiB, in %d runs:%s\n", resTotal, n, float64(resTotal*pg)/(1<<20), len(resident), fmtRuns(resident))
	fmt.Printf("INIT NON-ZERO 4K pages: %d = %.2f MiB, in %d runs:%s\n", nzTotal, float64(nzTotal*pg)/(1<<20), len(nz), fmtRuns(nz))
}
