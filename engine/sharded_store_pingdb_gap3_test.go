package engine

import (
	"context"
	"errors"
	"testing"
)

// pingingMockShardStore adds a controllable DBPinger to mockShardStore.
// mockShardStore itself deliberately does NOT implement DBPinger -- that is
// the case this file's test is about -- so this is its own type, used only
// here.
type pingingMockShardStore struct {
	*mockShardStore
	pingDBFn func(ctx context.Context) error
}

func (p *pingingMockShardStore) PingDB(ctx context.Context) error {
	if p.pingDBFn != nil {
		return p.pingDBFn(ctx)
	}
	return nil
}

// TestShardedStorePingDBFailsClosedOnAShardWithoutADBPinger is GAP3 from
// cleat#2005's third review round: ShardedStore.PingDB used to silently
// return nil for a shard whose underlying store did not implement DBPinger,
// on the theory that this only happens with a test double. It is called from
// the reaper's idle-worker path (see reapingIsSafe / heartbeatAndFenceInFlight
// in cmd/cleat-worker), where "nil" means "confirmed reachable" -- so a shard
// that cannot even attempt to answer was being read as proof of health. A
// shard that cannot confirm or deny its own reachability is not evidence of
// reachability, so it must fail the same way an unreachable shard would.
//
// Falsify by reverting the `!ok` branch in ShardedStore.PingDB to `return
// nil`: this test must then fail, since it asserts an error, not nil.
func TestShardedStorePingDBFailsClosedOnAShardWithoutADBPinger(t *testing.T) {
	ss, mocks := makeShardedStore(t, 3)

	// Wrap every shard's store so it DOES implement DBPinger and reports
	// healthy, except the middle one, which is left as a bare *mockShardStore
	// -- the case that used to be silently skipped.
	ss.mu.Lock()
	for i, shard := range ss.shards {
		if i == 1 {
			continue
		}
		shard.Store = &pingingMockShardStore{mockShardStore: mocks[i]}
	}
	ss.mu.Unlock()

	err := ss.PingDB(context.Background())
	if err == nil {
		t.Fatal("PingDB returned nil for a store group containing a shard with no DBPinger; " +
			"want an error, since that shard proved nothing about its own reachability")
	}

	// KNOWN-POSITIVE: when every shard genuinely implements DBPinger and is
	// healthy, PingDB must return nil -- otherwise the assertion above would
	// also pass against a PingDB that always errors.
	ss2, mocks2 := makeShardedStore(t, 3)
	ss2.mu.Lock()
	for i, shard := range ss2.shards {
		shard.Store = &pingingMockShardStore{mockShardStore: mocks2[i]}
	}
	ss2.mu.Unlock()
	if err := ss2.PingDB(context.Background()); err != nil {
		t.Fatalf("PingDB with every shard reachable = %v, want nil", err)
	}

	// And a genuinely unreachable shard (one that DOES implement DBPinger but
	// errors) must still be reported, unchanged from before this fix.
	ss3, mocks3 := makeShardedStore(t, 2)
	wantErr := errors.New("shard-1 is down")
	ss3.mu.Lock()
	ss3.shards[0].Store = &pingingMockShardStore{mockShardStore: mocks3[0]}
	ss3.shards[1].Store = &pingingMockShardStore{
		mockShardStore: mocks3[1],
		pingDBFn:       func(context.Context) error { return wantErr },
	}
	ss3.mu.Unlock()
	if err := ss3.PingDB(context.Background()); !errors.Is(err, wantErr) {
		t.Fatalf("PingDB with an unreachable shard = %v, want %v", err, wantErr)
	}
}
