package engine

import (
	"context"
	"testing"
)

// cleat#3516: GetWASMDigest is computed by the database, and Worker.loadWASM
// compares it with the digest of the bytes it holds. The two must be the same
// string or every cache entry reads as stale (the worker reloads each time) or,
// worse, a wrong implementation that agrees with itself serves the wrong bytes.
//
// The payloads include one past 8000 bytes because SQL Server's HASHBYTES took
// no more than that before 2016, and a real module is megabytes.
func TestAStoredWASMDigestMatchesTheBytes(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()

			d, ok := store.(WASMDigester)
			if !ok {
				t.Fatalf("%T does not implement WASMDigester, so a worker on this dialect falls back to downloading the bytes to compare them", store)
			}

			big := make([]byte, 300_000)
			for i := range big {
				big[i] = byte(i*131 + i>>8)
			}
			for name, payload := range map[string][]byte{
				"small": {0x00, 0x61, 0x73, 0x6d, 0x01, 0x0a},
				"large": big,
			} {
				def := "digest-" + name
				if err := store.DeployWorkflowDef(ctx, &WorkflowDef{Name: def, Version: 1, WASMBytes: payload, ABIVersion: 1, MinVersion: 1}); err != nil {
					t.Fatalf("UNMEASURED: deploy %s: %v", def, err)
				}
				got, err := d.GetWASMDigest(ctx, def, 1)
				if err != nil {
					t.Fatalf("GetWASMDigest(%s): %v", def, err)
				}
				if want := WASMDigest(payload); got != want {
					t.Errorf("%s: database digest %q, local digest %q", def, got, want)
				}

				// Replace with bytes of the SAME length: the digest has to move.
				other := append([]byte(nil), payload...)
				other[len(other)-1] ^= 0xff
				if err := store.DeployWorkflowDef(ctx, &WorkflowDef{Name: def, Version: 1, WASMBytes: other, ABIVersion: 1, MinVersion: 1}); err != nil {
					t.Fatalf("UNMEASURED: replace %s: %v", def, err)
				}
				got2, err := d.GetWASMDigest(ctx, def, 1)
				if err != nil {
					t.Fatalf("GetWASMDigest after replace: %v", err)
				}
				if got2 == got || got2 != WASMDigest(other) {
					t.Errorf("%s: after a same-length replacement the digest is %q (before %q, want %q)", def, got2, got, WASMDigest(other))
				}
			}
		})
	}
}
