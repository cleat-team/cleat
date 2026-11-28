//go:build cgo

package engine

import (
	"context"
	"os"
	"testing"
)

// A compiled component is charged to the cache at 3.5x its SERIALIZED artifact, not 4x its wasm (cleat#4389).
//
// The wasm-based figure under-counted every component measured: Python by 1.35 to 1.5x and C# by up to 2.2x, because a component's wasm is mostly a
// memory snapshot, which does not compile to anything; and it over-counted the densified Python component, whose wasm grew 11.6 MB of zero-filled
// gaps and whose live cost did not move. The table and the method are on CompiledComponentSizeEstimate. The live costs are noisy and are not asserted
// here (they are a measurement of whatever machine runs this); what is pinned is that the charge follows the serialized artifact, and that for the
// smallest checked-in component, where it is cheap to compile, it differs from the old charge in the measured direction.

func TestTheComponentEstimateIsThreeAndAHalfTimesTheSerializedArtifact(t *testing.T) {
	for _, tc := range []struct {
		serialized int
		want       int64
	}{{0, 0}, {-5, 0}, {2, 7}, {1000, 3500}, {45790232, 160265812}} {
		if got := CompiledComponentSizeEstimate(tc.serialized); got != tc.want {
			t.Errorf("CompiledComponentSizeEstimate(%d) = %d, want %d", tc.serialized, got, tc.want)
		}
	}
}

func TestACompiledComponentIsChargedItsSerializedSizeNotItsWasmLength(t *testing.T) {
	wasm, err := os.ReadFile("testdata/csharp_defer_order/defer_order.wasm")
	if err != nil {
		t.Fatalf("UNMEASURED: the checked-in C# component is missing: %v", err)
	}
	b := newComponentBackend(t)
	t.Cleanup(func() { _ = b.Close(context.Background()) })

	c, err := b.acquireComponent(wasm)
	if err != nil {
		t.Fatalf("UNMEASURED: compiling the component: %v", err)
	}
	serialized, ok := componentSerializedLen(c)
	cgotestReleaseComponent(c)
	if !ok || serialized <= 0 {
		t.Fatalf("UNMEASURED: the compiled component would not serialize (%d, %v)", serialized, ok)
	}

	got := b.moduleCache.estimatedBytes()
	if want := CompiledComponentSizeEstimate(serialized); got != want {
		t.Fatalf("the cache charges %d bytes for the component, want %d (3.5x its %d-byte serialized artifact)", got, want, serialized)
	}
	// The negative control: the old charge was a different number, so this test can tell the two apart. For this component the old one was 4x its
	// 1.06 MB wasm = 4.0 MiB against about 10.7 MiB now, the direction of the measured 8.9 MiB live.
	if old := CompiledSizeEstimate(len(wasm)); got <= old {
		t.Fatalf("the cache charges %d bytes, no more than the old wasm-based %d, so it would still under-count this component by the measured 2.2x", got, old)
	}
}
