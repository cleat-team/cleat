package main

import (
	"testing"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/monitoring/prometheus"
)

// cleat#2311. A store-level counter (cleat_decryption_errors_total) is only a
// counter if the store holds the metrics instance, and the process-wide store
// and the shard stores are built before the worker has one. The single-store
// path was a silent no-op until wireStoreMetrics; the sharded path was the
// second place the same gap lived.
func TestWireStoreMetricsReachesTheSingleStoreAndEveryShard(t *testing.T) {
	m := &prometheus.Metrics{}

	single := engine.NewPostgresStore(nil)
	wireStoreMetrics(single, nil, m)
	if single.Metrics != m {
		t.Error("the process-wide PostgresStore did not receive the metrics instance")
	}

	shards := []engine.WorkflowStore{engine.NewPostgresStore(nil), engine.NewPostgresStore(nil)}
	configs := []engine.ShardConfig{{Name: "a"}, {Name: "b"}}
	closers := []func() error{func() error { return nil }, func() error { return nil }}
	sharded, err := engine.NewShardedStore(configs, shards, closers)
	if err != nil {
		t.Fatal(err)
	}
	// Control: before wiring, no shard has one, so the assertions below cannot
	// pass by the shards having been born with it.
	for i, s := range shards {
		if s.(*engine.PostgresStore).Metrics != nil {
			t.Fatalf("PRECONDITION FAILED: shard %d already has metrics", i)
		}
	}
	wireStoreMetrics(sharded, nil, m)
	for i, s := range shards {
		if s.(*engine.PostgresStore).Metrics != m {
			t.Errorf("shard %d did not receive the metrics instance", i)
		}
	}

	// A store that already has its own is left alone.
	own := &prometheus.Metrics{}
	keep := engine.NewPostgresStore(nil)
	keep.Metrics = own
	wireStoreMetrics(keep, nil, m)
	if keep.Metrics != own {
		t.Error("a store's own metrics instance was replaced")
	}
}
