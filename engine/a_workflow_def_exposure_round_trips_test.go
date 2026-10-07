package engine

import (
	"context"
	"fmt"
	"testing"
)

// cleat#1986 slice 2a: the exposure class is carried from the caller through
// storage and back, on every dialect.
//
// EVERY read path is exercised, and that is the point rather than thoroughness
// for its own sake: the three handles have three separate column lists and three
// separate Scans, and they drift independently. Measured on this change's first
// pass -- GetWorkflowDef's Scan was given `&def.Exposure` while its SELECT did
// not return the column, which is a runtime Scan-arity error that no compiler
// and no build check can see. ListWorkflowDefs is the one that hides: it has TWO
// selects (by name, and all) behind one function.
//
// The unset case is asserted FIRST and deliberately. A deploy that says nothing
// must still succeed, and it only does because the store writes OrDefault()
// rather than the Go zero value: once the INSERT names the exposure column, the
// database's own DEFAULT 'auth' can no longer apply, and "" is outside the
// closed set the CHECK enforces. Without that, every deploy path breaks -- which
// is exactly what the first run of this change did, on postgres, with
// `violates check constraint "ck_workflow_defs_exposure" (23514)`.
func TestWorkflowDefExposureRoundTrips(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()

			cases := []struct {
				name string
				set  ExposureClass
				want ExposureClass
			}{
				{"unset writes the column default", "", ExposureAuth},
				{"internal round-trips", ExposureInternal, ExposureInternal},
			}

			for i, tc := range cases {
				defName := fmt.Sprintf("exposure-roundtrip-%d", i)
				if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
					Name: defName, Version: 1,
					WASMBytes:  []byte{0x00, 0x61, 0x73, 0x6d},
					ABIVersion: 1, MinVersion: 1,
					Exposure: tc.set,
				}); err != nil {
					t.Fatalf("%s: deploy: %v", tc.name, err)
				}

				// Read path 1: by name and version.
				got, err := store.GetWorkflowDef(ctx, defName, 1)
				if err != nil {
					t.Fatalf("%s: GetWorkflowDef: %v", tc.name, err)
				}
				if got == nil {
					t.Fatalf("%s: GetWorkflowDef returned nil after a successful deploy", tc.name)
				}
				if got.Exposure != tc.want {
					t.Errorf("%s: GetWorkflowDef Exposure = %q, want %q", tc.name, got.Exposure, tc.want)
				}

				// Read path 2: the by-name list.
				byname, err := store.ListWorkflowDefs(ctx, defName)
				if err != nil {
					t.Fatalf("%s: ListWorkflowDefs(name): %v", tc.name, err)
				}
				requireExposure(t, tc.name, "ListWorkflowDefs(name)", byname, defName, tc.want)

				// Read path 3: the all-definitions list.
				all, err := store.ListWorkflowDefs(ctx, "")
				if err != nil {
					t.Fatalf("%s: ListWorkflowDefs(all): %v", tc.name, err)
				}
				requireExposure(t, tc.name, "ListWorkflowDefs(all)", all, defName, tc.want)
			}
		})
	}
}

// requireExposure asserts on the named definition found in defs. Finding NOTHING
// is itself a failure rather than a pass: a read path that returns no rows would
// otherwise satisfy "no definition disagreed".
func requireExposure(t *testing.T, caseName, path string, defs []WorkflowDef, defName string, want ExposureClass) {
	t.Helper()
	for _, d := range defs {
		if d.Name != defName {
			continue
		}
		if d.Exposure != want {
			t.Errorf("%s: %s Exposure = %q, want %q -- this read path's SELECT and Scan disagree, "+
				"or the column is missing from one of them", caseName, path, d.Exposure, want)
		}
		return
	}
	t.Errorf("%s: %s returned no %s at all, so this read path was never exercised", caseName, path, defName)
}
