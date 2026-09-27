package engine

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// settingsWithLogCapture reads a tenant's settings once through an engine whose
// logger writes to a buffer, and returns whatever was logged.
//
// It drives the real tenantSettings path rather than calling the validator, so
// a validator that is never reached shows up as an absent warning rather than
// as a passing test.
func settingsWithLogCapture(t *testing.T, s TenantSettings) string {
	t.Helper()

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	e := NewEngine(nil, nil,
		WithWorkflowStore(&fakeSettingsStore{settings: s}),
		WithLogger(logger),
	)
	e.tenantID = "tenant-under-test"

	if got := e.tenantSettings(context.Background()); got != s {
		t.Fatalf("tenantSettings returned %+v, want %+v", got, s)
	}
	return buf.String()
}

// TestATenantIsWarnedWhenItsCeilingSitsBelowItsInstanceTimeout is §3.94 step 6.
//
// §3.90 found this in the worker's flags: a wall-clock ceiling below the
// instance timeout means the context deadline expires before the epoch fence
// can, so the guest's execution bound silently becomes the ceiling and the
// instance timeout stops deciding anything. Per-tenant settings let a tenant
// reach that state on its own, and nothing fails when it does -- the wrong
// number simply wins.
func TestATenantIsWarnedWhenItsCeilingSitsBelowItsInstanceTimeout(t *testing.T) {
	out := settingsWithLogCapture(t, TenantSettings{
		WasmWallClockCeiling: 5 * time.Second,
		WasmInstanceTimeout:  30 * time.Second,
	})

	if !strings.Contains(out, "wall-clock ceiling is below its instance timeout") {
		t.Fatalf("no conflict warning was logged for a tenant whose ceiling (5s) "+
			"is below its instance timeout (30s).\n\nlog was: %q", out)
	}
	// The tenant has to be named, or an operator reading this cannot act on it.
	if !strings.Contains(out, "tenant-under-test") {
		t.Errorf("the warning does not name the tenant: %q", out)
	}
}

// TestTheConflictWarningStaysQuietWhenThereIsNoConflict is the control, and it
// is what makes the test above a finding rather than a logger that always
// warns.
//
// Three quiet cases, because there are three distinct reasons not to warn and a
// validator could get any one of them wrong while looking correct on the
// others.
func TestTheConflictWarningStaysQuietWhenThereIsNoConflict(t *testing.T) {
	cases := []struct {
		name string
		s    TenantSettings
	}{
		{
			// The ordinary correct configuration.
			"ceiling above the instance timeout",
			TenantSettings{WasmWallClockCeiling: 300 * time.Second, WasmInstanceTimeout: 30 * time.Second},
		},
		{
			// Equal is not a conflict: the fence can still fire.
			"ceiling equal to the instance timeout",
			TenantSettings{WasmWallClockCeiling: 30 * time.Second, WasmInstanceTimeout: 30 * time.Second},
		},
		{
			// Only one override set. The other comes from the operator, whose
			// instance timeout this code cannot see -- comparing against a
			// value it does not have would be a guess, and a confident wrong
			// warning is worse than none.
			"only the ceiling is overridden",
			TenantSettings{WasmWallClockCeiling: 5 * time.Second},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := settingsWithLogCapture(t, tc.s)
			if strings.Contains(out, "wall-clock ceiling is below its instance timeout") {
				t.Fatalf("warned about a configuration that is not a conflict "+
					"(%+v).\n\nlog was: %q", tc.s, out)
			}
		})
	}
}
