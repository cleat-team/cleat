package engine

// MEASUREMENT, not a test (cleat#4378): what does a small GC segment cost a GC-heavy C# guest? Runs the "gcheavy" mode K times in one process (a fresh instance each)
// and prints what the guest reports about its own GC work. Env: HELDMEM_CS_WASM  HELDMEM_K  HELDMEM_ITERS

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/cleat-team/cleat/wasm"
)

func TestMeasureGCHeavy(t *testing.T) {
	path := os.Getenv("HELDMEM_CS_WASM")
	if path == "" {
		t.Skip("measurement: set HELDMEM_CS_WASM")
	}
	k, _ := strconv.Atoi(os.Getenv("HELDMEM_K"))
	if k == 0 {
		k = 7
	}
	iters := os.Getenv("HELDMEM_ITERS")
	if iters == "" {
		iters = "2000000"
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("UNMEASURED: %v", err)
	}
	b, err = wasm.WriteMetadata(b, &wasm.Metadata{Language: "csharp"})
	if err != nil {
		t.Fatal(err)
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
	for i := 0; i < k+1; i++ { // run 0 is the warm-up (compiles the component)
		c := &parkCaller{release: make(chan struct{}), resp: "{}"}
		close(c.release)
		eng := NewEngine(rt, c, WithBackends(WasmtimeLanguages, wt), WithWorkflowID(fmt.Sprintf("gcheavy-%d", i)))
		t0 := time.Now()
		_, _, susp, _, _, err := eng.Execute(ctx, b, "run", []byte(`{"mode":"gcheavy","iters":"`+iters+`"}`))
		wall := time.Since(t0)
		if err != nil || susp != nil {
			t.Fatalf("UNMEASURED: run %d: err=%v suspended=%v", i, err, susp != nil)
		}
		info, _ := c.gcinfo.Load().(string)
		if info == "" {
			t.Fatalf("UNMEASURED: run %d reported no gcinfo", i)
		}
		if i > 0 {
			fmt.Printf("GCHEAVY wall_ms=%.0f %s\n", float64(wall.Microseconds())/1000, info)
		}
	}
}
