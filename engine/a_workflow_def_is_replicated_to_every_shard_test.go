package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// cleat#3545, beyond the failing run in a_run_on_any_shard_finds_its_workflow_def_test.go: what
// replicating a definition to every shard has to get right. Two real databases (three where a
// shard must fail while the others do not). See that file for why not one database.
//
// Every case asserts the precondition that makes its verdict mean something, and says UNMEASURED
// when the fixture did not take, because a refusal and a fixture that never made the situation look
// the same.

var (
	defBytesA = []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x0a}
	defBytesB = []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x0b}
)

func (rs *realShards) deploy(ctx context.Context, name string, version int, wasm []byte, force bool) error {
	return rs.store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: name, Version: version, WASMBytes: wasm, ABIVersion: 1, MinVersion: 1, ReplaceUnderOpenRuns: force,
	})
}

// bytesOn reads a definition's bytes straight from shard i's database.
func (rs *realShards) bytesOn(t *testing.T, i int, name string, version int) []byte {
	t.Helper()
	var b []byte
	if err := rs.dbs[i].QueryRow(`SELECT wasm_bytes FROM workflow_defs WHERE name = $1 AND version = $2`, name, version).Scan(&b); err != nil {
		t.Fatalf("UNMEASURED: read %s v%d on shard %q: %v", name, version, rs.names[i], err)
	}
	return b
}

// startRunOn starts an open run of name v1 on shard i and checks it landed there.
func (rs *realShards) startRunOn(ctx context.Context, t *testing.T, i int, name string) string {
	t.Helper()
	id := rs.idOnShard(t, i, "open-run")
	if _, _, err := rs.store.StartNewRun(ctx, id, name, 1, json.RawMessage(`{}`), "", DefaultTenantUUID, 0); err != nil {
		t.Fatalf("UNMEASURED: could not start a run on shard %q: %v", rs.names[i], err)
	}
	var n int
	if err := rs.dbs[i].QueryRow(`SELECT COUNT(*) FROM workflow_payloads WHERE id = $1`, id).Scan(&n); err != nil || n != 1 {
		t.Fatalf("UNMEASURED: the run is not on shard %q (count %d): %v", rs.names[i], n, err)
	}
	return id
}

// otherThan returns the index of a shard that is not i.
func otherThan(i, total int) int { return (i + 1) % total }

func TestADeployReachesEveryShardAndRunningItAgainRepairsAMissingCopy(t *testing.T) {
	rs := newRealShards(t, "cleat_shard_defs_deploy", "a", "b", "c")
	ctx := context.Background()
	const name = "shard-defs-deploy"

	if err := rs.deploy(ctx, name, 1, defBytesA, false); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	for i := range rs.names {
		if got := rs.defCount(t, i, name); got != 1 {
			t.Fatalf("shard %q holds %d copies of the definition after a deploy, want 1", rs.names[i], got)
		}
	}

	// A copy goes missing, as a deploy that stopped half way would leave it.
	missing := otherThan(rs.shardOf(name), len(rs.names))
	if _, err := rs.dbs[missing].Exec(`DELETE FROM workflow_defs WHERE name = $1`, name); err != nil {
		t.Fatalf("UNMEASURED: remove the copy on shard %q: %v", rs.names[missing], err)
	}
	if got := rs.defCount(t, missing, name); got != 0 {
		t.Fatalf("UNMEASURED: the copy on shard %q is still there", rs.names[missing])
	}

	if err := rs.deploy(ctx, name, 1, defBytesA, false); err != nil {
		t.Fatalf("deploying the same bytes again: %v", err)
	}
	if got := rs.defCount(t, missing, name); got != 1 {
		t.Errorf("deploying again did not restore the copy on shard %q: %d", rs.names[missing], got)
	}
}

func TestAPartialDeployNamesTheShardsItWroteAndTheOnesItDid(t *testing.T) {
	rs := newRealShards(t, "cleat_shard_defs_partial", "a", "b", "c")
	ctx := context.Background()
	const name = "shard-defs-partial"

	// One non-designated shard refuses this definition at write time. A CHECK that is NOT VALID is
	// still enforced for new rows, and unlike a closed connection it does not also fail the
	// read-only pass that runs first, so the failure lands in the write, which is what is under test.
	broken := otherThan(rs.shardOf(name), len(rs.names))
	if _, err := rs.dbs[broken].Exec(`ALTER TABLE workflow_defs ADD CONSTRAINT refuse_this_name CHECK (name <> '` + name + `') NOT VALID`); err != nil {
		t.Fatalf("UNMEASURED: make shard %q refuse the definition: %v", rs.names[broken], err)
	}
	if _, err := rs.dbs[broken].Exec(`INSERT INTO workflow_defs (name, version, wasm_bytes, abi_version, min_version, tenant_id)
		VALUES ($1, 1, $2, 1, 1, $3)`, name, defBytesA, DefaultTenantUUID); err == nil {
		t.Fatalf("UNMEASURED: shard %q accepted a row the constraint should refuse", rs.names[broken])
	}

	err := rs.deploy(ctx, name, 1, defBytesA, false)
	var partial *ShardedDefWriteError
	if !errors.As(err, &partial) {
		t.Fatalf("a deploy that failed on one shard returned %T %v, want a *ShardedDefWriteError", err, err)
	}
	if len(partial.Failed) != 1 || partial.Failed[0] != rs.names[broken] {
		t.Errorf("Failed = %v, want [%s]", partial.Failed, rs.names[broken])
	}
	if len(partial.Succeeded) != 2 {
		t.Errorf("Succeeded = %v, want the other two shards", partial.Succeeded)
	}
	if !strings.Contains(err.Error(), `"`+rs.names[broken]+`"`) || !strings.Contains(err.Error(), "refuse_this_name") {
		t.Errorf("the message does not name the failing shard and why: %v", err)
	}
	for i := range rs.names {
		want := 1
		if i == broken {
			want = 0
		}
		if got := rs.defCount(t, i, name); got != want {
			t.Errorf("shard %q holds %d copies after the partial deploy, want %d", rs.names[i], got, want)
		}
	}

	// Repair: the shard stops refusing and the same command is run again.
	if _, err := rs.dbs[broken].Exec(`ALTER TABLE workflow_defs DROP CONSTRAINT refuse_this_name`); err != nil {
		t.Fatalf("UNMEASURED: lift the constraint: %v", err)
	}
	if err := rs.deploy(ctx, name, 1, defBytesA, false); err != nil {
		t.Fatalf("the repairing deploy: %v", err)
	}
	for i := range rs.names {
		if got := rs.defCount(t, i, name); got != 1 {
			t.Errorf("shard %q holds %d copies after the repair, want 1", rs.names[i], got)
		}
	}
}

func TestADeployThatReplacesBytesIsRefusedWhenAnyShardHasAnOpenRun(t *testing.T) {
	rs := newRealShards(t, "cleat_shard_defs_guard", "a", "b")
	ctx := context.Background()
	const name = "shard-defs-guard"
	designated := rs.shardOf(name)
	other := otherThan(designated, len(rs.names))

	if err := rs.deploy(ctx, name, 1, defBytesA, false); err != nil {
		t.Fatalf("first deploy: %v", err)
	}
	// The open run is on the shard that is NOT the designated one. A guard that looked only at the
	// designated shard would see none.
	rs.startRunOn(ctx, t, other, name)
	if n, err := rs.store.shards[designated].Store.CountActiveInstances(ctx, name, 1); err != nil || n != 0 {
		t.Fatalf("UNMEASURED: the designated shard %q should have no open run, has %d: %v", rs.names[designated], n, err)
	}

	err := rs.deploy(ctx, name, 1, defBytesB, false)
	var inUse *WorkflowDefInUseError
	if !errors.Is(err, ErrWorkflowDefInUse) || !errors.As(err, &inUse) {
		t.Fatalf("replacing bytes under a run open on shard %q gave %v, want ErrWorkflowDefInUse", rs.names[other], err)
	}
	if inUse.OpenRuns != 1 {
		t.Errorf("OpenRuns = %d, want 1", inUse.OpenRuns)
	}
	// Nothing was written anywhere. This is the half a per-shard check cannot give: the designated
	// shard has no open run, so it alone would have been replaced before the other refused.
	for i := range rs.names {
		if got := rs.bytesOn(t, i, name, 1); string(got) != string(defBytesA) {
			t.Errorf("shard %q holds %x after a refused deploy, want the original %x", rs.names[i], got, defBytesA)
		}
	}

	// A run on the designated shard too: the refusal counts both.
	rs.startRunOn(ctx, t, designated, name)
	err = rs.deploy(ctx, name, 1, defBytesB, false)
	if !errors.As(err, &inUse) || inUse.OpenRuns != 2 {
		t.Errorf("with an open run on each shard the refusal reports %v, want OpenRuns 2", err)
	}

	// Identical bytes stay an idempotent upsert under open runs, on every shard.
	if err := rs.deploy(ctx, name, 1, defBytesA, false); err != nil {
		t.Errorf("redeploying identical bytes under open runs: %v", err)
	}
	// The deliberate replacement goes to every shard.
	if err := rs.deploy(ctx, name, 1, defBytesB, true); err != nil {
		t.Fatalf("forced replacement: %v", err)
	}
	for i := range rs.names {
		if got := rs.bytesOn(t, i, name, 1); string(got) != string(defBytesB) {
			t.Errorf("shard %q holds %x after a forced replacement, want %x", rs.names[i], got, defBytesB)
		}
	}
}

func TestDeprecatingAndPurgingAVersionReachEveryShard(t *testing.T) {
	rs := newRealShards(t, "cleat_shard_defs_lifecycle", "a", "b")
	ctx := context.Background()
	const name = "shard-defs-lifecycle"

	if err := rs.deploy(ctx, name, 1, defBytesA, false); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	disabled := func(i int) (bool, bool) {
		var at *string
		var gc bool
		if err := rs.dbs[i].QueryRow(`SELECT disabled_at::text, gc_eligible FROM workflow_defs WHERE name = $1 AND version = 1`, name).Scan(&at, &gc); err != nil {
			t.Fatalf("UNMEASURED: read the flags on shard %q: %v", rs.names[i], err)
		}
		return at != nil, gc
	}
	for i := range rs.names {
		if d, gc := disabled(i); d || gc {
			t.Fatalf("UNMEASURED: shard %q starts deprecated", rs.names[i])
		}
	}
	if err := rs.store.MarkVersionDeprecated(ctx, name, 1, true); err != nil {
		t.Fatalf("deprecate: %v", err)
	}
	for i := range rs.names {
		if d, gc := disabled(i); !d || !gc {
			t.Errorf("shard %q: disabled=%v gc_eligible=%v after deprecating, want both true", rs.names[i], d, gc)
		}
	}

	if err := rs.store.PurgeWorkflowDef(ctx, name, 1); err != nil {
		t.Fatalf("purge: %v", err)
	}
	for i := range rs.names {
		if got := rs.defCount(t, i, name); got != 0 {
			t.Errorf("shard %q still holds %d copies after a purge", rs.names[i], got)
		}
	}
}

// A purge is refused by any shard that still holds a run of the version, FINISHED ones included:
// workflow_payloads_def_fkey is NO ACTION and a finished run keeps its row until retention. Version GC
// and `cleatctl versions purge` gate only on runs that are not settled, so this is the ROUTINE case, not
// a race (cleat#3546 review). With two shards it can hide, because the refusing shard is the only
// non-designated one and the designated copy is written last; it takes a third shard for a purge to
// remove the definition from one shard and then be refused by the next. So: three shards, and the
// history on each non-designated shard in turn.
func TestAPurgeRefusedByAnyShardWritesNothingAnywhere(t *testing.T) {
	for _, historyOn := range []int{0, 1} {
		historyOn := historyOn
		t.Run(fmt.Sprintf("history_on_the_%s_other_shard", []string{"first", "second"}[historyOn]), func(t *testing.T) {
			rs := newRealShards(t, fmt.Sprintf("cleat_shard_defs_purge%d", historyOn), "a", "b", "c")
			ctx := context.Background()
			const name = "shard-defs-purge"
			designated := rs.shardOf(name)
			var others []int
			for i := range rs.names {
				if i != designated {
					others = append(others, i)
				}
			}
			refusing := others[historyOn]

			if err := rs.deploy(ctx, name, 1, defBytesA, false); err != nil {
				t.Fatalf("deploy: %v", err)
			}
			// A run that has FINISHED, on the refusing shard only.
			id := rs.startRunOn(ctx, t, refusing, name)
			if _, err := rs.dbs[refusing].Exec(`UPDATE workflow_leases SET status = 'done' WHERE id = $1`, id); err != nil {
				t.Fatalf("UNMEASURED: settle the run: %v", err)
			}
			// The gate version GC uses calls this version unused. That is the precondition: without it
			// this would be the unusual case, not the routine one.
			if n, err := rs.store.CountActiveInstances(ctx, name, 1); err != nil || n != 0 {
				t.Fatalf("UNMEASURED: CountActiveInstances = %d, %v, want 0 (the run is settled)", n, err)
			}

			err := rs.store.PurgeWorkflowDef(ctx, name, 1)
			if err == nil {
				t.Fatal("a purge of a version a shard still holds a finished run of succeeded")
			}
			var partial *ShardedDefWriteError
			if errors.As(err, &partial) {
				t.Errorf("the purge was refused only after writing: written to %v, failed on %v", partial.Succeeded, partial.Failed)
			}
			if !strings.Contains(err.Error(), "no shard was written") || !strings.Contains(err.Error(), `"`+rs.names[refusing]+`"`) ||
				!strings.Contains(err.Error(), "workflow_payloads_def_fkey") {
				t.Errorf("the error does not say nothing was written and name the shard and the constraint: %v", err)
			}
			for i := range rs.names {
				if got := rs.defCount(t, i, name); got != 1 {
					t.Errorf("shard %q holds %d copies after a refused purge, want 1", rs.names[i], got)
				}
			}
			// The symptom: a run can still start on every shard.
			for i := range rs.names {
				runID := rs.idOnShard(t, i, "after-refused-purge")
				if _, _, err := rs.store.StartNewRun(ctx, runID, name, 1, json.RawMessage(`{}`), "", DefaultTenantUUID, 0); err != nil {
					t.Errorf("a run on shard %q cannot start after a refused purge: %v", rs.names[i], err)
				}
			}
		})
	}
}

func TestListingAndCountingSeeEachDefinitionOnceAndEveryRun(t *testing.T) {
	rs := newRealShards(t, "cleat_shard_defs_read", "a", "b", "c")
	ctx := context.Background()
	const name = "shard-defs-read"

	for _, v := range []int{1, 2} {
		if err := rs.deploy(ctx, name, v, defBytesA, false); err != nil {
			t.Fatalf("deploy v%d: %v", v, err)
		}
	}
	for i := range rs.names {
		if got := rs.defCount(t, i, name); got != 2 {
			t.Fatalf("UNMEASURED: shard %q holds %d rows, want 2 (one per version)", rs.names[i], got)
		}
	}

	defs, err := rs.store.ListWorkflowDefs(ctx, name)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(defs) != 2 || defs[0].Version != 2 || defs[1].Version != 1 {
		t.Errorf("listing a name gave %d definitions %+v, want versions [2 1] once each", len(defs), defs)
	}
	all, err := rs.store.ListWorkflowDefs(ctx, "")
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("listing everything gave %d definitions, want 2", len(all))
	}

	// One open run of v1 on each shard: the count is the total, not one shard's.
	for i := range rs.names {
		rs.startRunOn(ctx, t, i, name)
	}
	got, err := rs.store.CountActiveInstances(ctx, name, 1)
	if err != nil || got != len(rs.names) {
		t.Errorf("CountActiveInstances = %d, %v, want %d: one open run on each of %d shards", got, err, len(rs.names), len(rs.names))
	}
}
