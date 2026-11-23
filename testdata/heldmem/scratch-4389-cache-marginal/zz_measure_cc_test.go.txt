//go:build cgo && linux && zzmeasure

// A throwaway measurement, not a test of the engine. Meant to be deleted.
//
// Question: when a worker's compiled-component cache already holds one
// distinct artifact, what does a SECOND and THIRD distinct artifact cost in
// process memory? The design docs charge "one compiled component" at the
// +493-510 MiB (Python) / +81 MiB (C#) process delta measured after the first
// warm-up execution. That delta includes an execution; this measures the
// compile-and-cache half alone, and then the free half (Close), so the parts
// can be told apart.
//
// Distinct artifacts are made by appending a unique custom section to the same
// component bytes, so the compiled code is identical and only the cache key
// (sha256 of the bytes) differs. That isolates the marginal cost of a cache
// entry from the cost of compiling different code.
//
// Run one language per process: MEAS_LANG=python|csharp.
package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"testing"
	"time"
)

type measMem struct {
	rss, anon, file, pss, hwm int64 // KiB
}

func (m measMem) String() string {
	return fmt.Sprintf("rss=%.1f anon=%.1f file=%.1f pss=%.1f hwm=%.1f MiB",
		ki(m.rss), ki(m.anon), ki(m.file), ki(m.pss), ki(m.hwm))
}

func ki(k int64) float64 { return float64(k) / 1024.0 }

func measReadMem(t *testing.T) measMem {
	t.Helper()
	var m measMem
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		t.Fatalf("read /proc/self/status: %v", err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		v, _ := strconv.ParseInt(f[1], 10, 64)
		switch f[0] {
		case "VmRSS:":
			m.rss = v
		case "RssAnon:":
			m.anon = v
		case "RssFile:":
			m.file = v
		case "VmHWM:":
			m.hwm = v
		}
	}
	if roll, err := os.ReadFile("/proc/self/smaps_rollup"); err == nil {
		for _, line := range strings.Split(string(roll), "\n") {
			if strings.HasPrefix(line, "Pss:") {
				f := strings.Fields(line)
				if len(f) >= 2 {
					m.pss, _ = strconv.ParseInt(f[1], 10, 64)
				}
			}
		}
	}
	return m
}

func measSettle() {
	runtime.GC()
	debug.FreeOSMemory()
	time.Sleep(1500 * time.Millisecond)
}

// appendCustom appends a well-formed wasm custom section carrying name.
func appendCustom(src []byte, name string) []byte {
	body := appendLEB(nil, uint32(len(name)))
	body = append(body, name...)
	body = append(body, []byte("cleat-component-memory-measurement")...)
	out := append([]byte{}, src...)
	out = append(out, 0x00) // custom section id, in modules and components alike
	out = appendLEB(out, uint32(len(body)))
	out = append(out, body...)
	return out
}

func appendLEB(dst []byte, v uint32) []byte {
	for {
		b := byte(v & 0x7f)
		v >>= 7
		if v != 0 {
			dst = append(dst, b|0x80)
		} else {
			return append(dst, b)
		}
	}
}

func TestMeasureComponentLiveCost(t *testing.T) {
	path := os.Getenv("MEAS_WASM")
	if path == "" {
		t.Fatal("MEAS_WASM must name a component")
	}
	k, _ := strconv.Atoi(os.Getenv("MEAS_K"))
	if k == 0 {
		k = 5
	}
	base, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	ctx := context.Background()
	b, err := NewWasmtimeBackend(ctx, WithWasmtimeModuleCacheMaxEntries(100), WithWasmtimeModuleCacheMaxBytes(4<<30))
	if err != nil {
		t.Fatalf("NewWasmtimeBackend: %v", err)
	}
	// instrument check
	measSettle()
	pre := measReadMem(t)
	buf := make([]byte, 64<<20)
	for i := 0; i < len(buf); i += 4096 {
		buf[i] = byte(i)
	}
	mid := measReadMem(t)
	runtime.KeepAlive(buf)
	if d := ki(mid.anon - pre.anon); d < 48 || d > 80 {
		t.Fatalf("known-positive: 64 MiB read as %+.1f MiB", d)
	}
	buf = nil
	measSettle()
	measureMallocTrim()
	time.Sleep(300 * time.Millisecond)
	prev := measReadMem(t).anon
	est := CompiledSizeEstimate(len(base))
	for i := 0; i < k; i++ {
		data := base
		if i > 0 {
			data = appendCustom(base, fmt.Sprintf("d%d", i))
		}
		start := time.Now()
		if err := measureCompileOnce(b, data); err != nil {
			t.Fatalf("compile %d: %v", i, err)
		}
		el := time.Since(start)
		measSettle()
		raw := measReadMem(t)
		measureMallocTrim()
		time.Sleep(300 * time.Millisecond)
		tr := measReadMem(t)
		fmt.Printf("LIVE wasm=%s bytes=%d est_MiB=%.1f entry=%d elapsed_ms=%d raw_delta_MiB=%.1f live_delta_MiB=%.1f live_total_MiB=%.1f compiles=%d\n",
			filepath.Base(path), len(base), float64(est)/1048576, i+1, el.Milliseconds(), ki(raw.anon-prev), ki(tr.anon-prev), ki(tr.anon), b.components.compiles.Load())
		prev = tr.anon
	}
	_ = b.Close(ctx)
}

func TestMeasureSerialized(t *testing.T) {
	path := os.Getenv("MEAS_WASM")
	base, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewWasmtimeBackend(context.Background(), WithWasmtimeModuleCacheMaxEntries(10), WithWasmtimeModuleCacheMaxBytes(4<<30))
	if err != nil {
		t.Fatal(err)
	}
	n, err := measureSerializedLen(b, base)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("SERIALIZED wasm=%s bytes=%d serialized=%d ratio=%.2f\n", filepath.Base(path), len(base), n, float64(n)/float64(len(base)))
}
