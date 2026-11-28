package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// cleat#4162. PublishQueryState writes a run's query state WITHOUT ending its segment, so a run HELD through a wait is not invisible for the whole hold. What it must get right, each
// against the real stores:
//
//   - it writes, and changes NOTHING else: the run stays running and assigned to the same worker;
//   - it REPLACES the stored state, as finalize does (a key the guest no longer sets is gone), and a nil map writes nothing;
//   - it is FENCED: a worker that does not hold the run, at the generation it was claimed under, writes nothing and is told ErrFenceLost. The known-positive for that is a REAL lost
//     claim (the run was suspended, which hands it back), not only a made-up worker name;
//   - writing the state the store already holds is NOT a lost fence. MySQL reports rows CHANGED, not rows MATCHED, so an UPDATE that sets query_state to what it already is reports
//     zero rows, which is what a lost fence looks like; a replayed run sets the same state again, so this is the ordinary case and not an edge.
func TestPublishQueryStateIsFencedAndReplacesTheStoredState(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()
			id := newIntentWorkflow(t, ctx, store, "publish-qs")
			wf, err := claimSpecific(t, ctx, store, id, "publisher")
			if err != nil {
				t.Fatalf("claim: %v", err)
			}
			list := func() map[string]string {
				t.Helper()
				got, err := store.ListQueryState(ctx, id)
				if err != nil {
					t.Fatalf("ListQueryState: %v", err)
				}
				return got
			}

			state := map[string]string{"status": "awaiting_approval", "approval_wait": "entered"}
			if err := store.PublishQueryState(ctx, id, "publisher", wf.Generation, state); err != nil {
				t.Fatalf("PublishQueryState: %v", err)
			}
			if got := list(); !reflect.DeepEqual(got, state) {
				t.Fatalf("the store holds %v after the publish, want %v", got, state)
			}
			// It changed NOTHING else.
			after, err := store.GetWorkflowByID(ctx, id)
			if err != nil {
				t.Fatalf("GetWorkflowByID: %v", err)
			}
			if after.Status != "running" || after.AssignedTo != "publisher" || after.Generation != wf.Generation {
				t.Errorf("after the publish the run is status=%q assigned_to=%q generation=%d, want it unchanged (running, publisher, %d): publishing must not end or move the segment", after.Status, after.AssignedTo, after.Generation, wf.Generation)
			}

			// Writing the SAME state again is not a lost fence (the MySQL rows-changed trap).
			if err := store.PublishQueryState(ctx, id, "publisher", wf.Generation, state); err != nil {
				t.Errorf("publishing the state the store already holds = %v, want nil: an unchanged row reported as zero rows was read as a lost fence", err)
			}

			// REPLACES: a key the guest no longer sets is gone.
			next := map[string]string{"status": "approval_received"}
			if err := store.PublishQueryState(ctx, id, "publisher", wf.Generation, next); err != nil {
				t.Fatalf("PublishQueryState (replace): %v", err)
			}
			if got := list(); !reflect.DeepEqual(got, next) {
				t.Errorf("the store holds %v, want %v: the publish merged into the stored state instead of replacing it (finalize replaces)", got, next)
			}

			// A nil map writes nothing and is not an error.
			if err := store.PublishQueryState(ctx, id, "publisher", wf.Generation, nil); err != nil {
				t.Errorf("PublishQueryState(nil) = %v, want nil", err)
			}
			if got := list(); !reflect.DeepEqual(got, next) {
				t.Errorf("a nil publish changed the stored state to %v, want it left as %v", got, next)
			}

			// FENCED, two ways that a made-up claim can be wrong: the worker, and the generation.
			for _, c := range []struct {
				name   string
				worker string
				gen    int64
			}{
				{"another worker", "intruder", wf.Generation},
				{"another generation", "publisher", wf.Generation + 1},
			} {
				err := store.PublishQueryState(ctx, id, c.worker, c.gen, map[string]string{"status": "overwritten-by-" + c.name})
				if !errors.Is(err, ErrFenceLost) {
					t.Errorf("%s: PublishQueryState = %v, want ErrFenceLost", c.name, err)
				}
				if got := list(); !reflect.DeepEqual(got, next) {
					t.Errorf("%s: the store holds %v, want %v: a worker that does not hold the run overwrote its state", c.name, got, next)
				}
			}

			// THE KNOWN-POSITIVE: a REAL lost claim. The run is suspended, which hands it back; the worker that had it must not be able to write over what the suspension recorded.
			if err := store.FinalizeWorkflowSegment(ctx, id, "publisher", wf.Generation, nil, "ready", "", "", "", map[string]string{"status": "recorded-at-the-suspension"}, time.Now().Add(time.Hour)); err != nil {
				t.Fatalf("FinalizeWorkflowSegment (suspend): %v", err)
			}
			err = store.PublishQueryState(ctx, id, "publisher", wf.Generation, map[string]string{"status": "written-after-losing-the-claim"})
			if !errors.Is(err, ErrFenceLost) {
				t.Errorf("a publish after the run was suspended = %v, want ErrFenceLost: this worker no longer holds the run", err)
			}
			if got := list(); got["status"] != "recorded-at-the-suspension" {
				t.Errorf("after losing the claim the store holds %v, want what the suspension recorded: a worker that lost its claim overwrote the state", got)
			}
		})
	}
}

// (c) SEALED. A deployment that seals query_state stores a JSON STRING wrapping ciphertext, not an object (errors.go). The publish must seal exactly as finalize does, and the proof is
// the raw column: not that the write returned no error, and not that a decrypting read works (an unsealed write would read back fine too). The control is the same publish on a store
// that does not seal, whose raw column IS the object with the plaintext in it; without it, "no plaintext in the column" is also what a write that failed silently looks like.
func TestAPublishedQueryStateIsSealedExactlyWhenTheDeploymentSeals(t *testing.T) {
	db := testDB(t)
	t.Cleanup(func() { db.Close() })
	enc, err := NewPayloadEncryption(validKey(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	const secret = "awaiting_approval_SENTINEL"

	for _, c := range []struct {
		name    string
		factory *PostgresStoreFactory
		sealed  bool
	}{
		{"sealing on (--encrypt-sensitive-payloads)", NewPostgresStoreFactory(db, "public").WithEncryption(enc, true), true},
		{"sealing off (the control)", NewPostgresStoreFactory(db, "public"), false},
	} {
		c := c
		t.Run(c.name, func(t *testing.T) {
			store, lease, err := c.factory.OpenStore(ctx, DefaultTenantUUID)
			if err != nil {
				t.Fatalf("OpenStore: %v", err)
			}
			t.Cleanup(func() { _ = lease.Close() })
			id := newIntentWorkflow(t, ctx, store, "publish-sealed")
			wf, err := claimSpecific(t, ctx, store, id, "sealer")
			if err != nil {
				t.Fatalf("claim: %v", err)
			}
			if err := store.PublishQueryState(ctx, id, "sealer", wf.Generation, map[string]string{"status": secret}); err != nil {
				t.Fatalf("PublishQueryState: %v", err)
			}

			var raw string
			if err := db.QueryRowContext(ctx, `SELECT query_state::text FROM workflow_payloads WHERE id = $1`, id).Scan(&raw); err != nil && !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("reading the raw column: %v", err)
			}
			isString := strings.HasPrefix(strings.TrimSpace(raw), `"`)
			hasPlain := strings.Contains(raw, secret)
			if c.sealed && (!isString || hasPlain) {
				t.Errorf("sealing is on and the raw query_state is %q (a JSON string: %v, plaintext present: %v): the publish skipped the encoder finalize uses", raw, isString, hasPlain)
			}
			if !c.sealed && (isString || !hasPlain) {
				t.Errorf("sealing is off and the raw query_state is %q (a JSON string: %v, plaintext present: %v), want the plain object", raw, isString, hasPlain)
			}
			// And it round-trips through the store's own decrypting read in both modes.
			got, err := store.ListQueryState(ctx, id)
			if err != nil || got["status"] != secret {
				t.Errorf("ListQueryState = %v, %v, want status=%s: the sealed value does not decrypt back", got, err, secret)
			}
		})
	}
}

// (g) SHARDED. ShardedStore.PublishQueryState routes by the run id to the shard that owns the run. The failure this guards is the one sharding actually produces, a write that
// lands on the WRONG shard: it would report success, the owning shard would hold nothing, and the client would still see nothing. So this starts one run on EACH shard, publishes
// through the sharded store, and reads the owning shard's own database directly, and also reads the OTHER shard to see that nothing arrived there. It needs real shards:
// a fake store records whatever the router hands it and cannot show where a row went.
func TestAPublishThroughTheShardedStoreLandsOnTheShardThatOwnsTheRun(t *testing.T) {
	rs := newRealShards(t, "cleat_shard_pubqs", "a", "b")
	ctx := context.Background()

	const name = "shard-publish-qs"
	if err := rs.store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: name, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("deploy: %v", err)
	}

	raw := func(shard int, id string) (string, bool) {
		t.Helper()
		var qs sql.NullString
		err := rs.dbs[shard].QueryRowContext(ctx, `SELECT query_state::text FROM workflow_payloads WHERE id = $1`, id).Scan(&qs)
		if errors.Is(err, sql.ErrNoRows) {
			return "", false
		}
		if err != nil {
			t.Fatalf("reading shard %q: %v", rs.names[shard], err)
		}
		return qs.String, qs.Valid
	}

	for owner := range rs.names {
		other := 1 - owner
		id := rs.idOnShard(t, owner, "pubqs-on")
		if _, _, err := rs.store.StartNewRun(ctx, id, name, 1, json.RawMessage(`{}`), "", DefaultTenantUUID, 0); err != nil {
			t.Fatalf("start a run on shard %q: %v", rs.names[owner], err)
		}
		wf, err := claimSpecific(t, ctx, rs.store, id, "sharded-publisher")
		if err != nil {
			t.Fatalf("claim on shard %q: %v", rs.names[owner], err)
		}
		if got := rs.shardOf(id); got != owner {
			t.Fatalf("PRECONDITION FAILED: the run routes to shard %d, want %d, so this measured nothing about routing", got, owner)
		}

		state := map[string]string{"status": "awaiting_approval-on-" + rs.names[owner]}
		if err := rs.store.PublishQueryState(ctx, id, "sharded-publisher", wf.Generation, state); err != nil {
			t.Fatalf("PublishQueryState through the sharded store, run on shard %q: %v", rs.names[owner], err)
		}
		if got, ok := raw(owner, id); !ok || !strings.Contains(got, state["status"]) {
			t.Errorf("run on shard %q: the owning shard holds query_state %q (set: %v), want it to hold %v", rs.names[owner], got, ok, state)
		}
		if got, ok := raw(other, id); ok || got != "" {
			t.Errorf("run on shard %q: the OTHER shard %q holds a row/state %q for it: the publish went to the wrong shard", rs.names[owner], rs.names[other], got)
		}
		if got, err := rs.store.ListQueryState(ctx, id); err != nil || got["status"] != state["status"] {
			t.Errorf("run on shard %q: ListQueryState through the sharded store = %v, %v, want %v", rs.names[owner], got, err, state)
		}
		if err := rs.store.PublishQueryState(ctx, id, "somebody-else", wf.Generation, map[string]string{"status": "overwritten"}); !errors.Is(err, ErrFenceLost) {
			t.Errorf("run on shard %q: a foreign worker's publish through the sharded store = %v, want ErrFenceLost", rs.names[owner], err)
		}
	}
}
