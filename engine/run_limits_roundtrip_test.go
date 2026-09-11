package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// A per-run override is written by the start and read back by the resolver.
//
// cleat#1187. The arithmetic is pinned exhaustively by
// TestLimitPrecedenceOperatorTenantRun; this is the plumbing on either side of
// it -- that StartOptions reaches the columns, and that GetRunLimits returns
// what was written. Both halves are needed because either could be right alone
// and the feature still do nothing.
func TestARunsOwnLimitsSurviveAStartAndAreReadBack(t *testing.T) {
	for _, backend := range registeredBackends {
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)
			truncateAll(t, store)
			ctx := context.Background()

			starter, ok := store.(interface {
				StartNewRunWithOptions(context.Context, string, string, int, json.RawMessage, string, string, int, StartOptions) (string, bool, error)
			})
			if !ok {
				t.Fatalf("%T cannot record per-run start options", store)
			}
			reader, ok := store.(RunLimitsReader)
			if !ok {
				t.Fatalf("%T cannot read per-run limits", store)
			}

			want := TenantSettings{
				WasmInstanceTimeout:  7 * time.Second,
				WasmWallClockCeiling: 11 * time.Second,
				HostRetryBudget:      13 * time.Second,
			}
			id, _, err := starter.StartNewRunWithOptions(ctx,
				fmt.Sprintf("rl-%d", time.Now().UnixNano()), "test-workflow", 1,
				json.RawMessage(`{}`), "", DefaultTenantUUID, 0,
				StartOptions{RunLimits: want})
			if err != nil {
				t.Fatalf("StartNewRunWithOptions: %v", err)
			}

			got, err := reader.GetRunLimits(ctx, id)
			if err != nil {
				t.Fatalf("GetRunLimits: %v", err)
			}
			if got != want {
				t.Errorf("run limits round-tripped as %+v, want %+v.\n\n"+
					"Three distinct values are used so a mix-up between the columns is "+
					"visible; equal values would pass against any permutation of them.", got, want)
			}
		})

		t.Run(backend.Name()+"/a start with no overrides reads back as unset", func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)
			truncateAll(t, store)
			ctx := context.Background()

			// The ordinary case, and the one that decides whether every run on
			// the deployment suddenly has overrides. Unset must mean unset --
			// ClampToCeiling reads a non-positive value as "no override", so a
			// zero here resolves to the tenant's setting, which is correct, but
			// a NON-zero here would silently bound every run.
			id, _, err := store.StartNewRun(ctx, fmt.Sprintf("rl-none-%d", time.Now().UnixNano()),
				"test-workflow", 1, json.RawMessage(`{}`), "", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}
			got, err := store.(RunLimitsReader).GetRunLimits(ctx, id)
			if err != nil {
				t.Fatalf("GetRunLimits: %v", err)
			}
			if got != (TenantSettings{}) {
				t.Errorf("a run started with no overrides reports %+v, want the zero value", got)
			}
		})
	}
}
