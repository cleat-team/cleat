package engine

import (
	"math"
	"testing"
)

// TestScratchBaseFor covers the arithmetic all three execution paths used to do
// inline, in uint32, one page above a guest heap that can reach 4 GiB.
//
// The interesting case is not "does it overflow" but what the overflow *did*:
// `uint32(currentSize + wasmPageSize)` wrapped to a small number, fell below
// the 10 MiB legacy floor, and was then clamped UP to 10 MiB -- an address
// comfortably inside the guest's own heap. Every bounds check downstream then
// passed, because 10 MiB really is a valid offset in a 4 GiB memory, so the
// host would have written its scratch buffers over guest data and returned
// success. A test that only asserted "no overflow" would miss why that matters.
func TestScratchBaseFor(t *testing.T) {
	const oneMiB = uint32(1024 * 1024)

	tests := []struct {
		name        string
		currentSize uint64
		outBufSz    uint32
		want        uint32
		wantErr     bool
	}{
		{
			// Below the floor: a fresh module with one page of memory.
			name:        "small heap clamps to the legacy floor",
			currentSize: wasmPageSize,
			outBufSz:    oneMiB,
			want:        legacyScratchOffset,
		},
		{
			name:        "just below the floor still clamps",
			currentSize: uint64(legacyScratchOffset) - 2*wasmPageSize,
			outBufSz:    oneMiB,
			want:        legacyScratchOffset,
		},
		{
			// Above the floor: one guard page past the heap.
			name:        "large heap sits one page above it",
			currentSize: 64 * 1024 * 1024,
			outBufSz:    oneMiB,
			want:        64*1024*1024 + wasmPageSize,
		},
		{
			// The regression. Pre-fix this wrapped to 0 and was clamped to
			// 10 MiB -- inside the guest heap, silently.
			name:        "heap one page short of 4 GiB is refused, not wrapped",
			currentSize: math.MaxUint32 - wasmPageSize,
			outBufSz:    oneMiB,
			wantErr:     true,
		},
		{
			name:        "heap at the 4 GiB ceiling is refused",
			currentSize: math.MaxUint32,
			outBufSz:    oneMiB,
			wantErr:     true,
		},
		{
			// Room for the base but not for both buffers above it. Checking
			// only the base would let this through and put the output buffer
			// past the addressable range.
			name:        "base fits but the buffers do not",
			currentSize: math.MaxUint32 - uint64(oneMiB) - 2*wasmPageSize,
			outBufSz:    oneMiB,
			wantErr:     true,
		},
		{
			// A configured --wasm-output-buffer-size big enough to matter.
			name:        "large out buffer shifts the ceiling down",
			currentSize: math.MaxUint32 - 600*1024*1024,
			outBufSz:    512 * 1024 * 1024,
			wantErr:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := scratchBaseFor(tt.currentSize, tt.outBufSz)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("scratchBaseFor(%d, %d) = %d, nil; want an error. "+
						"Silently returning a base here is how the host ends up "+
						"writing into the guest's heap", tt.currentSize, tt.outBufSz, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("scratchBaseFor(%d, %d): unexpected error: %v", tt.currentSize, tt.outBufSz, err)
			}
			if got != tt.want {
				t.Errorf("scratchBaseFor(%d, %d) = %d, want %d", tt.currentSize, tt.outBufSz, got, tt.want)
			}
			// Whatever it returns must leave room for both buffers, which is
			// the property every caller relies on to do its own uint32 adds.
			if uint64(got)+2*uint64(tt.outBufSz) > math.MaxUint32 {
				t.Errorf("base %d + 2x%d exceeds uint32; callers add these in uint32 and would wrap",
					got, tt.outBufSz)
			}
			// And it must never land below the floor the SDKs hardcode.
			if got < legacyScratchOffset {
				t.Errorf("base %d is below the %d legacy floor", got, legacyScratchOffset)
			}
		})
	}
}
