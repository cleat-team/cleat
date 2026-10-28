package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// cleat#3545. A ShardedStore wrote each workflow definition to ONE shard, getShard(def.Name), and
// routed each RUN by getShard(runID). The two hashes disagree for most (name, run) pairs, so a run
// whose shard was not its definition's shard failed workflow_payloads_def_fkey: the foreign key is
// inside one database and the definition was in another.
//
// Owner decision D42: nobody runs a sharded store for real work yet, so the definitions are
// replicated to every shard rather than the runs being made to follow the name.
//
// These tests use TWO REAL DATABASES, not one database behind several pools. The existing
// DB-backed sharded test (sharded_overclaim_db_test.go) builds its shards from a single database,
// where a definition written through any shard is visible through all of them, so this defect cannot
// occur in it by construction. That is why it went unmeasured until two databases were used.

// realShards is a ShardedStore over len(names) separate PostgreSQL databases, each built from the
// shipped migrations, plus the pieces a test needs to look behind the store.
type realShards struct {
	store *ShardedStore
	dbs   []*sql.DB
	names []string
}

// newRealShards builds one database per name. The database names carry the test's own prefix so two
// tests never share one, and bootstrapScratchDB drops any leftover of the same name first.
func newRealShards(t *testing.T, prefix string, names ...string) *realShards {
	t.Helper()
	rs := &realShards{names: names}
	configs := make([]ShardConfig, len(names))
	stores := make([]WorkflowStore, len(names))
	closers := make([]func() error, len(names))
	for i, n := range names {
		db := bootstrapScratchDB(t, fmt.Sprintf("%s_%s", prefix, n))
		rs.dbs = append(rs.dbs, db)
		configs[i] = ShardConfig{Name: n}
		stores[i] = NewPostgresStore(db)
		closers[i] = func() error { return nil }
	}
	st, err := NewShardedStore(configs, stores, closers)
	if err != nil {
		t.Fatalf("NewShardedStore: %v", err)
	}
	rs.store = st
	return rs
}

// shardOf returns the index of the shard a key routes to.
func (rs *realShards) shardOf(key string) int {
	name := rs.store.getShard(key).Config.Name
	for i, n := range rs.names {
		if n == name {
			return i
		}
	}
	panic("shard not in the list it was built from: " + name)
}

// idOnShard returns a run id that routes to shard i. Hashing is deterministic, so this is a search,
// not a guess.
func (rs *realShards) idOnShard(t *testing.T, i int, stem string) string {
	t.Helper()
	for n := 0; n < 10000; n++ {
		id := fmt.Sprintf("%s-%d", stem, n)
		if rs.shardOf(id) == i {
			return id
		}
	}
	t.Fatalf("no id with prefix %q routes to shard %d", stem, i)
	return ""
}

// defCount counts the definition rows named name on shard i, read straight from that database.
func (rs *realShards) defCount(t *testing.T, i int, name string) int {
	t.Helper()
	var n int
	if err := rs.dbs[i].QueryRow(`SELECT COUNT(*) FROM workflow_defs WHERE name = $1`, name).Scan(&n); err != nil {
		t.Fatalf("count %q on shard %q: %v", name, rs.names[i], err)
	}
	return n
}

func TestARunOnAnyShardFindsItsWorkflowDefinition(t *testing.T) {
	rs := newRealShards(t, "cleat_shard_defs_run", "a", "b")
	ctx := context.Background()

	const name = "shard-defs-run"
	if err := rs.store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: name, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("deploy: %v", err)
	}

	// One run on each shard. Exactly one of the two is on a shard that is not the one getShard(name)
	// picks, which is the run that failed workflow_payloads_def_fkey before the definition was
	// replicated. Both are started so the test does not depend on which way the name hashes.
	for i := range rs.names {
		runID := rs.idOnShard(t, i, "run-on")
		if _, _, err := rs.store.StartNewRun(ctx, runID, name, 1, json.RawMessage(`{}`), "", DefaultTenantUUID, 0); err != nil {
			t.Errorf("a run on shard %q (the definition's designated shard is %q) could not start: %v",
				rs.names[i], rs.names[rs.shardOf(name)], err)
			if !strings.Contains(err.Error(), "workflow_payloads_def_fkey") {
				t.Errorf("...and not by workflow_payloads_def_fkey, which is the failure #3545 measured")
			}
		}
	}
}
