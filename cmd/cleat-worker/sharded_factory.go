package main

import (
	"context"
	"fmt"
	"io"

	"github.com/cleat-team/cleat/engine"
)

// shardedStoreFactory opens a tenant-scoped store that spans every shard.
//
// It exists because the sharded startup path builds two things that do not
// agree about how much data they cover: `store` is a ShardedStore over all
// shards, while `factory` was assigned the *first shard's* factory only. Handing
// that factory to the HTTP layer would scope each request correctly by tenant
// and then silently narrow it to shard 0 -- every list, get and admin action
// would miss the other shards' workflows and report success. That is a data
// correctness bug rather than a security one, but it is the kind that reads as
// "the workflow isn't there" and sends the reader somewhere else entirely.
//
// Construction is cheap and does no I/O: engine.NewShardedStore only builds
// structs, and each per-shard OpenStore reuses that shard's existing *sql.DB
// rather than dialling. So this runs per request without a connection cost.
//
// This lives in cmd/cleat-worker rather than engine/ deliberately: engine's
// sharded files belong to another workstream, and nothing here needs to be
// inside the engine to work.
type shardedStoreFactory struct {
	configs   []engine.ShardConfig
	factories []engine.StoreFactory
}

// OpenStore opens one store per shard for the given tenant and wraps them in a
// ShardedStore, so routing behaves exactly as the process-wide store does.
func (f *shardedStoreFactory) OpenStore(ctx context.Context, tenantID string, taskQueues ...string) (engine.WorkflowStore, io.Closer, error) {
	if len(f.factories) == 0 {
		return nil, nil, fmt.Errorf("sharded store factory has no shards")
	}

	stores := make([]engine.WorkflowStore, len(f.factories))
	closers := make([]func() error, len(f.factories))
	for i, sf := range f.factories {
		s, shardCloser, err := sf.OpenStore(ctx, tenantID, taskQueues...)
		if err != nil {
			return nil, nil, fmt.Errorf("open shard %d for tenant %s: %w", i, tenantID, err)
		}
		stores[i] = s
		// THE SHARD'S OWN CLOSER, kept rather than dropped. It used to be
		// replaced with a no-op, on the reasoning that these stores share
		// process-wide per-shard *sql.DB pools this factory does not own and
		// must not close when one request finishes -- true of what the closer
		// was then, which was nothing at all.
		//
		// cleat#1928 gave it a meaning: it is a LEASE on the tenant's pool, and
		// closing it says this caller is finished, not that the pool should
		// shut. Dropping it now would pin every shard's pool for every tenant
		// the API has ever served, which is the leak this is all about, one
		// level up. Every shard here is a PostgresStoreFactory, which shares
		// one pool and hands out a no-op, so today the two behave identically
		// -- the point is which of them stays right if a per-tenant-pool
		// dialect is ever sharded.
		closers[i] = shardCloser.Close
	}

	ss, err := engine.NewShardedStore(f.configs, stores, closers)
	if err != nil {
		return nil, nil, fmt.Errorf("build sharded store for tenant %s: %w", tenantID, err)
	}
	// Closing this closes each shard's, which releases each shard's lease.
	// The ShardedStore is per-caller -- built fresh above -- so closing it
	// tears down nothing shared.
	return ss, shardedStoreCloser{ss}, nil
}

// shardedStoreCloser releases the per-shard leases a ShardedStore was built
// with. See OpenStore.
type shardedStoreCloser struct{ ss *engine.ShardedStore }

func (c shardedStoreCloser) Close() error { c.ss.Close(); return nil }

// DriverName reports the first shard's driver. All shards share a driver.
func (f *shardedStoreFactory) DriverName() string {
	if len(f.factories) == 0 {
		return ""
	}
	return f.factories[0].DriverName()
}

// Dialect reports the first shard's dialect. All shards share a dialect.
func (f *shardedStoreFactory) Dialect() engine.Dialect {
	if len(f.factories) == 0 {
		return engine.DialectPostgres
	}
	return f.factories[0].Dialect()
}
