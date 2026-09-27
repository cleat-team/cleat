package engine

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// ShardedStore implemented WorkflowStore and neither TenantSettingsReader nor
// RunLimitsReader, which all three dialect stores implement. Both are reached
// by a type assertion that returns silently on failure -- see
// tenantSettings in tenant_settings.go and runLimits in engine.go -- so on a
// sharded deployment every workflow got the tier-above fallback with no log
// at all. That is worse than the write-ahead-intent gap cleat#1857 fixed,
// which at least refused loudly: here nothing said the capability was
// missing, and the fallback direction (safe, but wider than the tenant or
// run intended) went unnoticed.

// tenantLimitsCapableShardStore is a shard store that CAN answer both.
type tenantLimitsCapableShardStore struct {
	stubWorkflowStore
	name         string
	tenant       TenantSettings
	tenantErr    error
	runLimits    map[string]TenantSettings
	getCallCount int
}

func (s *tenantLimitsCapableShardStore) GetTenantSettings(_ context.Context) (TenantSettings, error) {
	s.getCallCount++
	return s.tenant, s.tenantErr
}

func (s *tenantLimitsCapableShardStore) GetRunLimits(_ context.Context, workflowID string) (TenantSettings, error) {
	return s.runLimits[workflowID], nil
}

func twoTenantLimitsShards() (*ShardedStore, *tenantLimitsCapableShardStore, *tenantLimitsCapableShardStore) {
	a := &tenantLimitsCapableShardStore{name: "shard-a", runLimits: map[string]TenantSettings{}}
	b := &tenantLimitsCapableShardStore{name: "shard-b", runLimits: map[string]TenantSettings{}}
	ss, err := NewShardedStore(
		[]ShardConfig{{Name: "shard-a"}, {Name: "shard-b"}},
		[]WorkflowStore{a, b},
		[]func() error{func() error { return nil }, func() error { return nil }},
	)
	if err != nil {
		panic(err)
	}
	return ss, a, b
}

// TestShardedStore_HonoursTenantSettings is the known-positive: before the
// delegation existed, ShardedStore did not implement TenantSettingsReader at
// all, so the type assertion in tenantSettings (tenant_settings.go) failed
// and every tenant silently got the operator's flags.
func TestShardedStore_HonoursTenantSettings(t *testing.T) {
	ss, a, _ := twoTenantLimitsShards()
	a.tenant = TenantSettings{HostRetryBudget: 5 * time.Second}

	reader, ok := WorkflowStore(ss).(TenantSettingsReader)
	if !ok {
		t.Fatal("ShardedStore does not implement TenantSettingsReader: every tenant on a sharded " +
			"deployment silently gets the operator's flags instead of its own override")
	}
	got, err := reader.GetTenantSettings(context.Background())
	if err != nil {
		t.Fatalf("GetTenantSettings: %v", err)
	}
	if got.HostRetryBudget != 5*time.Second {
		t.Errorf("HostRetryBudget = %v, want 5s: the tenant's override did not survive sharding", got.HostRetryBudget)
	}
}

// TestShardedStore_TenantSettingsFallsThroughToAShardThatHasARow is the
// fallback behaviour a shard-0-only delegation would get wrong: an operator
// wrote the override to shard-b (whichever shard happens to hold index 1) and
// not shard-a, because cleat#1187's writer takes one *sql.DB and is run per
// shard by hand.
func TestShardedStore_TenantSettingsFallsThroughToAShardThatHasARow(t *testing.T) {
	ss, _, b := twoTenantLimitsShards()
	// shard-a stays at the zero value -- no override written there.
	b.tenant = TenantSettings{MaxWorkflowDuration: 90 * time.Minute}

	got, err := ss.GetTenantSettings(context.Background())
	if err != nil {
		t.Fatalf("GetTenantSettings: %v", err)
	}
	if got.MaxWorkflowDuration != 90*time.Minute {
		t.Fatalf("MaxWorkflowDuration = %v, want 90m: a delegation pinned to one shard would miss "+
			"an override an operator wrote to a different one", got.MaxWorkflowDuration)
	}
}

// TestShardedStore_TenantSettingsAllZeroIsNotAnError is GetTenantSettings'
// own documented contract: "a tenant with no row is not an error".
func TestShardedStore_TenantSettingsAllZeroIsNotAnError(t *testing.T) {
	ss, _, _ := twoTenantLimitsShards()
	got, err := ss.GetTenantSettings(context.Background())
	if err != nil {
		t.Fatalf("GetTenantSettings with no override anywhere: %v, want nil -- absence is not an error", err)
	}
	if got != (TenantSettings{}) {
		t.Errorf("got %+v, want the zero value", got)
	}
}

// TestShardedStore_TenantSettingsDisagreementUsesFirstShardAndIsVisible covers
// the case TestShardedStoreHonoursTenantSettingsWithDocumentedSemantics
// (tenant_settings_wiring_test.go) requires: two shards with DIFFERENT
// non-zero overrides is an operator error (nothing fans a tenant_settings
// write out to every shard), and the chosen semantics is "use the first
// shard found, and make the disagreement visible" rather than silently
// preferring one with no signal at all.
//
// "Visible" is checked through slog's own test hook rather than by
// re-implementing a formatter: a text handler writing to a buffer, installed
// only for this test's default logger and restored after.
func TestShardedStore_TenantSettingsDisagreementUsesFirstShardAndIsVisible(t *testing.T) {
	ss, a, b := twoTenantLimitsShards()
	a.tenant = TenantSettings{HostRetryBudget: 5 * time.Second}
	b.tenant = TenantSettings{HostRetryBudget: 9 * time.Second}

	var logBuf strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	defer slog.SetDefault(prev)

	got, err := ss.GetTenantSettings(context.Background())
	if err != nil {
		t.Fatalf("GetTenantSettings: %v", err)
	}
	if got.HostRetryBudget != 5*time.Second {
		t.Errorf("HostRetryBudget = %v, want 5s (shard-a's, the first found): the chosen "+
			"semantics is deterministic first-found, not last-found or a merge", got.HostRetryBudget)
	}
	if !strings.Contains(logBuf.String(), "disagree") {
		t.Errorf("no warning logged for disagreeing shards; log was: %q\n"+
			"The whole point of choosing first-found over silent preference is that the "+
			"disagreement stops being invisible.", logBuf.String())
	}
}

// TestShardedStore_HonoursRunLimits is the known-positive for GetRunLimits,
// and it uses two workflow ids -- one per shard -- for the reason recorded on
// the sibling call-intent test: a single-id sample cannot distinguish correct
// routing from a router that ignores the workflow id.
func TestShardedStore_HonoursRunLimits(t *testing.T) {
	ctx := context.Background()
	ss, a, b := twoTenantLimitsShards()

	perShard := map[*tenantLimitsCapableShardStore]string{}
	for _, id := range []string{
		"11111111-1111-1111-1111-111111111111",
		"22222222-2222-2222-2222-222222222222",
		"33333333-3333-3333-3333-333333333333",
		"44444444-4444-4444-4444-444444444444",
		"55555555-5555-5555-5555-555555555555",
	} {
		owner := ss.getShard(id).Store.(*tenantLimitsCapableShardStore)
		if _, seen := perShard[owner]; !seen {
			perShard[owner] = id
		}
	}
	if len(perShard) != 2 {
		// Deterministic (sha256 over these literals), so this can only be
		// always- or never-true -- Fatal rather than Skip, per the sibling
		// test's note on why a skip here would silently stop measuring.
		t.Fatalf("the sampled ids cover %d of 2 shards: a one-shard sample cannot tell correct "+
			"routing from a constant", len(perShard))
	}

	idA, idB := perShard[a], perShard[b]
	a.runLimits[idA] = TenantSettings{WasmInstanceTimeout: 10 * time.Second}
	b.runLimits[idB] = TenantSettings{WasmInstanceTimeout: 20 * time.Second}

	gotA, err := ss.GetRunLimits(ctx, idA)
	if err != nil {
		t.Fatalf("GetRunLimits(%s): %v", idA, err)
	}
	if gotA.WasmInstanceTimeout != 10*time.Second {
		t.Errorf("GetRunLimits(%s) = %v, want 10s", idA, gotA.WasmInstanceTimeout)
	}

	gotB, err := ss.GetRunLimits(ctx, idB)
	if err != nil {
		t.Fatalf("GetRunLimits(%s): %v", idB, err)
	}
	if gotB.WasmInstanceTimeout != 20*time.Second {
		t.Fatalf("GetRunLimits(%s) = %v, want 20s: routing to the wrong shard would return the "+
			"other run's override, or none", idB, gotB.WasmInstanceTimeout)
	}
}

// TestShardedStore_IncapableShardDegradesInsteadOfErroring matches the
// documented fallback posture: a shard that cannot answer resolves to the
// zero value (the safe, wider-than-intended direction), not an error that
// would fail the workflow outright. This is the property that makes the
// missing warning in the pre-fix code dangerous -- the code path never
// crashed or even returned an error, so nothing about its behaviour pointed
// at the gap.
func TestShardedStore_IncapableShardDegradesInsteadOfErroring(t *testing.T) {
	ctx := context.Background()
	incapable := &stubWorkflowStore{}
	ss, err := NewShardedStore(
		[]ShardConfig{{Name: "incapable"}},
		[]WorkflowStore{incapable},
		[]func() error{func() error { return nil }},
	)
	if err != nil {
		t.Fatalf("NewShardedStore: %v", err)
	}

	settings, err := ss.GetTenantSettings(ctx)
	if err != nil {
		t.Errorf("GetTenantSettings over an incapable shard returned an error %v, want nil: "+
			"the documented fallback is the operator's flags, not a failure", err)
	}
	if settings != (TenantSettings{}) {
		t.Errorf("got %+v, want the zero value", settings)
	}

	limits, err := ss.GetRunLimits(ctx, "11111111-1111-1111-1111-111111111111")
	if err != nil {
		t.Errorf("GetRunLimits over an incapable shard returned an error %v, want nil", err)
	}
	if limits != (TenantSettings{}) {
		t.Errorf("got %+v, want the zero value", limits)
	}
}
