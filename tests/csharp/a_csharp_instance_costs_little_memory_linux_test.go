//go:build cgo && linux

package csharp

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
)

// The per-instance private-memory half of the pin described in a_csharp_instance_costs_little_memory_test.go (cleat#4378). Linux only, by build
// tag and not by skip: it reads /proc/self/smaps_rollup. The startup-cap test in the other file pins the same cause on every host.

// parkingCaller holds every service call until released, so N instances are held inside a host call at once.
type parkingCaller struct {
	arrived atomic.Int32
	release chan struct{}
}

func (c *parkingCaller) Call(ctx context.Context, _, _, _ string) (string, error) {
	c.arrived.Add(1)
	select {
	case <-c.release:
	case <-ctx.Done():
	}
	return "{}", nil
}

// privateKiB reads the process's proportional set size (physical memory, shared pages divided among their users) from /proc.
func privateKiB(t *testing.T) uint64 {
	t.Helper()
	b, err := os.ReadFile("/proc/self/smaps_rollup")
	if err != nil {
		t.Fatalf("UNMEASURED: %v", err)
	}
	for _, l := range strings.Split(string(b), "\n") {
		if f := strings.Fields(l); len(f) >= 2 && f[0] == "Pss:" {
			v, err := strconv.ParseUint(f[1], 10, 64)
			if err != nil {
				t.Fatalf("UNMEASURED: Pss %q: %v", f[1], err)
			}
			return v
		}
	}
	t.Fatalf("UNMEASURED: no Pss line in /proc/self/smaps_rollup")
	return 0
}

// instanceMemoryBudgetMiB bounds the marginal physical memory of one parked C# instance. The SDK build (8 MiB segments) measures 16.7 to 17.1
// MiB; a build with 16 MiB segments measures 32.8 and one without the setting 48.7. 25 sits between the first and the others, with about 8 MiB
// either side.
const instanceMemoryBudgetMiB = 25

func TestCSharpAnInstanceCostsUnderTheMemoryBudget(t *testing.T) {
	b := conformanceComponent(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rt, err := engine.NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	t.Cleanup(func() { rt.Close(ctx) })
	wt, err := engine.NewWasmtimeBackend(ctx)
	if err != nil {
		t.Fatalf("NewWasmtimeBackend: %v", err)
	}
	t.Cleanup(func() { wt.Close(ctx) })

	const input = `{"service":"svc","operation":"op","request":{},"maxAttempts":1,"initialMs":1,"backoff":1,"maxMs":1,"nonRetryable":[]}`
	// hold parks n instances inside their one service call, reads the memory they add, and releases them.
	hold := func(n int, tag string) uint64 {
		c := &parkingCaller{release: make(chan struct{})}
		runtime.GC()
		debug.FreeOSMemory()
		time.Sleep(300 * time.Millisecond)
		before := privateKiB(t)
		var wg sync.WaitGroup
		var failed atomic.Int32
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				id := fmt.Sprintf("wf-%s-%d", tag, i)
				eng := engine.NewEngine(rt, c, engine.WithBackends(engine.WasmtimeLanguages, wt), engine.WithWorkflowID(id))
				if _, _, _, _, _, err := eng.Execute(ctx, b, "a_call_retry", []byte(input)); err != nil {
					failed.Add(1)
				}
			}(i)
		}
		deadline := time.Now().Add(120 * time.Second)
		for int(c.arrived.Load()) < n && time.Now().Before(deadline) && failed.Load() == 0 {
			time.Sleep(10 * time.Millisecond)
		}
		if int(c.arrived.Load()) != n {
			t.Fatalf("UNMEASURED: %d of %d instances reached their service call (%d failed), so the memory read below would not be of %d parked instances",
				c.arrived.Load(), n, failed.Load(), n)
		}
		time.Sleep(300 * time.Millisecond)
		held := privateKiB(t)
		close(c.release)
		wg.Wait()
		if failed.Load() != 0 {
			t.Fatalf("UNMEASURED: %d instances failed after release", failed.Load())
		}
		return held - before
	}

	hold(1, "warm") // compiles and caches the component, so no later reading carries the compile
	small, large := hold(4, "a"), hold(16, "b")
	// The two-point slope removes whatever the first hold costs once (threads, runtime structures), leaving the cost of one more instance.
	perInstanceKiB := float64(int64(large)-int64(small)) / 12
	t.Logf("held 4 instances: +%.1f MiB; held 16: +%.1f MiB; marginal %.2f MiB per instance (budget %d MiB)",
		float64(small)/1024, float64(large)/1024, perInstanceKiB/1024, instanceMemoryBudgetMiB)
	if perInstanceKiB/1024 > instanceMemoryBudgetMiB {
		t.Errorf("one more parked C# instance costs %.1f MiB of physical memory, over the %d MiB budget: the GC segment setting in "+
			"packages/cleat-csharp/build/Cleat.Sdk.props is not reaching the build (without it the cost is about 49 MiB; cleat#4378)",
			perInstanceKiB/1024, instanceMemoryBudgetMiB)
	}
	if perInstanceKiB <= 0 {
		t.Errorf("the marginal cost came out as %.2f MiB; a negative or zero cost means the readings were not of parked instances", perInstanceKiB/1024)
	}
}
