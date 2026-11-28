package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine/testutil"
)

// A sharded store answers "is this tenant suspended" (cleat#3596).
//
// Both start paths decide whether to refuse a suspended tenant with a type assertion on the store they
// hold, and ShardedStore used not to implement the interface, so on a sharded deployment the assertion
// failed and the check was skipped without a word. These tests assert on the same thing the start paths
// do -- the interface, not a method called by name -- because a method that exists but is not the
// interface's would still leave the assertion failing.

// suspendingShard is a shard whose answer the test controls.
type suspendingShard struct {
	*mockShardStore
	suspended map[string]bool
	err       error
}

func (s *suspendingShard) IsTenantSuspended(_ context.Context, tenantID string) (bool, error) {
	if s.err != nil {
		return false, s.err
	}
	return s.suspended[tenantID], nil
}

// asReader asserts the way the start paths do. Calling the method directly would not compile until the
// method exists, which hides the difference between "the interface is missing" and "the answer is wrong".
func asReader(t *testing.T, s WorkflowStore) TenantSuspensionReader {
	t.Helper()
	r, ok := s.(TenantSuspensionReader)
	if !ok {
		t.Fatalf("%T does not implement TenantSuspensionReader, so both start paths skip the suspension check", s)
	}
	return r
}

func shardedOf(t *testing.T, stores ...WorkflowStore) *ShardedStore {
	t.Helper()
	configs := make([]ShardConfig, len(stores))
	closers := make([]func() error, len(stores))
	for i := range stores {
		configs[i] = ShardConfig{Name: "shard" + string(rune('0'+i))}
		closers[i] = func() error { return nil }
	}
	ss, err := NewShardedStore(configs, stores, closers)
	if err != nil {
		t.Fatalf("NewShardedStore: %v", err)
	}
	return ss
}

func TestAShardedStoreAnswersTheSuspensionQuestionStartPathsAsk(t *testing.T) {
	held, live := uuid.NewString(), uuid.NewString()
	ctx := context.Background()

	// WHICH shard carries the tenant's row is not something the store can know -- every shard is its own
	// database with its own admin.tenants -- so the answer is "suspended on ANY shard". Each position is
	// tried so a store that only asks the first shard, or only the last, is caught.
	for _, tc := range []struct {
		name   string
		shards func() []WorkflowStore
	}{
		{"flagged on the first shard", func() []WorkflowStore {
			return []WorkflowStore{
				&suspendingShard{mockShardStore: &mockShardStore{}, suspended: map[string]bool{held: true}},
				&suspendingShard{mockShardStore: &mockShardStore{}},
			}
		}},
		{"flagged on the last shard", func() []WorkflowStore {
			return []WorkflowStore{
				&suspendingShard{mockShardStore: &mockShardStore{}},
				&suspendingShard{mockShardStore: &mockShardStore{}},
				&suspendingShard{mockShardStore: &mockShardStore{}, suspended: map[string]bool{held: true}},
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The store as the start paths hold it: scoped to the tenant.
			var scoped WorkflowStore = shardedOf(t, tc.shards()...).WithTenant(held)
			susp, ok := scoped.(TenantSuspensionReader)
			if !ok {
				t.Fatalf("%T does not implement TenantSuspensionReader, so both start paths skip the "+
					"suspension check on a sharded deployment", scoped)
			}
			got, err := susp.IsTenantSuspended(ctx, held)
			if err != nil {
				t.Fatalf("IsTenantSuspended(held): %v", err)
			}
			if !got {
				t.Errorf("a tenant suspended on one shard was reported live")
			}
			// CONTROLS. Without them an implementation that answers true for everyone passes above.
			if got, err := susp.IsTenantSuspended(ctx, live); err != nil || got {
				t.Errorf("IsTenantSuspended(live) = %v, %v; want false, nil", got, err)
			}
			if got, err := susp.IsTenantSuspended(ctx, uuid.NewString()); err != nil || got {
				t.Errorf("a tenant with no row anywhere = %v, %v; want false, nil (absent is not suspended)", got, err)
			}
		})
	}
}

// An unreadable flag is not a clear one. A shard that errors, and a shard whose store cannot answer at
// all, both make the store's answer an error rather than "not suspended" -- the start paths already turn
// an error into a refusal.
func TestAShardedStoreThatCannotReadTheFlagRefuses(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("shard unreachable")

	t.Run("a shard returns an error", func(t *testing.T) {
		ss := shardedOf(t,
			&suspendingShard{mockShardStore: &mockShardStore{}},
			&suspendingShard{mockShardStore: &mockShardStore{}, err: boom})
		got, err := asReader(t, ss).IsTenantSuspended(ctx, uuid.NewString())
		if err == nil || !errors.Is(err, boom) {
			t.Errorf("IsTenantSuspended = %v, %v; want the shard's error, so the start is refused", got, err)
		}
	})

	t.Run("a shard cannot answer", func(t *testing.T) {
		// A bare mock has no IsTenantSuspended.
		ss := shardedOf(t,
			&suspendingShard{mockShardStore: &mockShardStore{}},
			&mockShardStore{})
		got, err := asReader(t, ss).IsTenantSuspended(ctx, uuid.NewString())
		if err == nil || !errors.Is(err, ErrTenantSuspensionUnanswerable) {
			t.Errorf("IsTenantSuspended = %v, %v; want ErrTenantSuspensionUnanswerable for a shard that neither answers nor opts out", got, err)
		}
	})

	t.Run("an error does not hide a tenant that IS suspended", func(t *testing.T) {
		held := uuid.NewString()
		ss := shardedOf(t,
			&suspendingShard{mockShardStore: &mockShardStore{}, suspended: map[string]bool{held: true}},
			&suspendingShard{mockShardStore: &mockShardStore{}, err: boom})
		got, err := asReader(t, ss).IsTenantSuspended(ctx, held)
		// Either answer refuses the start; the one that must not happen is (false, nil).
		if err == nil && !got {
			t.Errorf("a suspended tenant read as live because a sibling shard errored")
		}
	})
}

// The real implementer, on a real database: a *PostgresStore shard is found by the sharded store's
// delegation, so the unit tests above are not only satisfied by their own fakes.
func TestAShardedStoreOverARealShardSeesAnAdminTenantsRow(t *testing.T) {
	owner := testutil.TestDB(t, testutil.DialectPostgres)
	testutil.SetupFullSchema(t, owner, testutil.DialectPostgres)
	applyPostgresProcedures(t, owner)
	defer owner.Close()

	ctx := context.Background()
	held, live := uuid.NewString(), uuid.NewString()
	t.Cleanup(func() {
		_, _ = owner.Exec(`DELETE FROM admin.tenants WHERE tenant_id = ANY($1)`, "{"+held+","+live+"}")
	})
	for _, tc := range []struct {
		id        string
		name      string
		suspended bool
	}{
		{live, "shard-susp-live-" + live[:8], false},
		{held, "shard-susp-held-" + held[:8], true},
	} {
		if _, err := owner.ExecContext(ctx,
			`INSERT INTO admin.tenants (tenant_id, name, suspended) VALUES ($1, $2, $3)
			 ON CONFLICT (tenant_id) DO UPDATE SET suspended = EXCLUDED.suspended`,
			tc.id, tc.name, tc.suspended); err != nil {
			t.Fatalf("seeding %s: %v", tc.name, err)
		}
	}

	ss := shardedOf(t,
		&suspendingShard{mockShardStore: &mockShardStore{}},
		NewPostgresStore(owner))
	var scoped WorkflowStore = ss.WithTenant(held)
	susp, ok := scoped.(TenantSuspensionReader)
	if !ok {
		t.Fatalf("%T does not implement TenantSuspensionReader", scoped)
	}
	if got, err := susp.IsTenantSuspended(ctx, held); err != nil || !got {
		t.Errorf("IsTenantSuspended(held) = %v, %v; want true, nil", got, err)
	}
	if got, err := susp.IsTenantSuspended(ctx, live); err != nil || got {
		t.Errorf("IsTenantSuspended(live) = %v, %v; want false, nil", got, err)
	}
}
