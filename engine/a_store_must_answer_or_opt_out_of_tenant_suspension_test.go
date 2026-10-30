package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// A store must either answer "is this tenant suspended" or declare that it has no such notion
// (cleat#3552).
//
// The start paths used to ask with a bare type assertion on TenantSuspensionReader and skip the check when
// it failed, so "this store cannot answer" and "this tenant is not suspended" were one outcome. Two real
// holes came out of that -- the plugin start path had no check at all (cleat#3594) and a sharded store
// failed the assertion (cleat#3596) -- and neither made anything fail. TenantSuspended is the one
// question both paths now ask, and it has a third answer.

// optedOutStore declares NoTenantSuspensionMarker and nothing else.
type optedOutStore struct{ *mockShardStore }

func (optedOutStore) NoTenantSuspension() {}

func TestTenantSuspendedRefusesAStoreThatNeitherAnswersNorOptsOut(t *testing.T) {
	ctx := context.Background()
	tid := uuid.NewString()

	t.Run("a bare store is unanswerable and the error names its type", func(t *testing.T) {
		got, err := TenantSuspended(ctx, &mockShardStore{}, tid)
		if !errors.Is(err, ErrTenantSuspensionUnanswerable) {
			t.Fatalf("TenantSuspended = %v, %v; want ErrTenantSuspensionUnanswerable", got, err)
		}
		if !strings.Contains(err.Error(), "mockShardStore") {
			t.Errorf("error %q does not name the store type, so an operator cannot tell which store to fix", err)
		}
		for _, want := range []string{"IsTenantSuspended(", "NoTenantSuspension()"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not say to add %s, so it names the problem and not the fix", err, want)
			}
		}
		if got {
			t.Errorf("an unanswerable store reported suspended=true")
		}
	})

	t.Run("a declared opt-out is not suspended, on purpose", func(t *testing.T) {
		got, err := TenantSuspended(ctx, optedOutStore{&mockShardStore{}}, tid)
		if err != nil || got {
			t.Errorf("TenantSuspended = %v, %v; want false, nil for a store that declares it has no tenants to suspend", got, err)
		}
	})

	t.Run("a reader answers, yes and no", func(t *testing.T) {
		held := uuid.NewString()
		st := &suspendingShard{mockShardStore: &mockShardStore{}, suspended: map[string]bool{held: true}}
		if got, err := TenantSuspended(ctx, st, held); err != nil || !got {
			t.Errorf("suspended tenant: TenantSuspended = %v, %v; want true, nil", got, err)
		}
		if got, err := TenantSuspended(ctx, st, tid); err != nil || got {
			t.Errorf("live tenant: TenantSuspended = %v, %v; want false, nil", got, err)
		}
	})

	t.Run("a reader's error is returned, never read as not suspended", func(t *testing.T) {
		boom := errors.New("db down")
		got, err := TenantSuspended(ctx, &suspendingShard{mockShardStore: &mockShardStore{}, err: boom}, tid)
		if !errors.Is(err, boom) || got {
			t.Errorf("TenantSuspended = %v, %v; want false and the reader's error", got, err)
		}
	})

	t.Run("a reader that also opts out is believed", func(t *testing.T) {
		// The more specific answer wins; opting out must not mask a store that can in fact say yes.
		held := uuid.NewString()
		st := struct {
			*suspendingShard
			NoTenantSuspensionMarker
		}{&suspendingShard{mockShardStore: &mockShardStore{}, suspended: map[string]bool{held: true}}, optedOutStore{}}
		if got, err := TenantSuspended(ctx, st, held); err != nil || !got {
			t.Errorf("TenantSuspended = %v, %v; want true, nil from the reader", got, err)
		}
	})
}

// The real MySQL store is the one production type that opts out. Asserted on the type itself, through a
// wrapper that embeds it as well, since the declaration has to survive embedding.
func TestMySQLStoreDeclaresItHasNoTenantSuspension(t *testing.T) {
	var st any = &MySQLStore{}
	if _, ok := st.(NoTenantSuspensionMarker); !ok {
		t.Fatalf("*MySQLStore does not implement NoTenantSuspensionMarker: every start on MySQL would be refused")
	}
	if _, ok := st.(TenantSuspensionReader); ok {
		t.Errorf("*MySQLStore implements TenantSuspensionReader as well: it has no admin.tenants to read")
	}
	got, err := TenantSuspended(context.Background(), wrappedMySQLStore{&MySQLStore{}}, uuid.NewString())
	if err != nil || got {
		t.Errorf("TenantSuspended(wrapped MySQLStore) = %v, %v; want false, nil", got, err)
	}
}

// The other production types answer. A new store type that does neither is refused at the first start, but
// this pins the three that exist so that losing the reader from one of them fails here and not in
// production.
func TestEveryProductionStoreAnswersOrOptsOut(t *testing.T) {
	cases := []struct {
		name  string
		store any
	}{
		{"PostgresStore", &PostgresStore{}},
		{"MSSQLStore", &MSSQLStore{}},
		{"ShardedStore", &ShardedStore{}},
		{"MySQLStore", &MySQLStore{}},
	}
	for _, c := range cases {
		_, reader := c.store.(TenantSuspensionReader)
		_, optsOut := c.store.(NoTenantSuspensionMarker)
		if !reader && !optsOut {
			t.Errorf("%s neither implements TenantSuspensionReader nor NoTenantSuspensionMarker; the start paths would refuse every start on it", c.name)
		}
	}
}

// A shard that opts out has no flag to read: it counts as not suspended, and a sibling that says yes still wins.
func TestAShardedStoreAcceptsAShardThatOptsOut(t *testing.T) {
	ctx := context.Background()
	held := uuid.NewString()
	ss := shardedOf(t,
		optedOutStore{&mockShardStore{}},
		&suspendingShard{mockShardStore: &mockShardStore{}, suspended: map[string]bool{held: true}})
	if got, err := ss.IsTenantSuspended(ctx, uuid.NewString()); err != nil || got {
		t.Errorf("live tenant: IsTenantSuspended = %v, %v; want false, nil (an opted-out shard is not an error)", got, err)
	}
	if got, err := ss.IsTenantSuspended(ctx, held); err != nil || !got {
		t.Errorf("suspended tenant: IsTenantSuspended = %v, %v; want true, nil", got, err)
	}
}
