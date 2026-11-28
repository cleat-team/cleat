package engine

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

// cleat#4275: a run held in memory through a wait is reclaimed once its heartbeat is HELD-stale instead of timeout-stale, and only a run the holder marked held at its current
// generation. Every reclaim statement carries that arm (ReapStaleInstances and ReapStaleInstancesExcept on each dialect), so each case runs through both.
//
// The numbers are the product's defaults (a 47 s window, a 17 s held window) but nothing here waits: heartbeats are backdated by the database's own clock.
const (
	heldTestTimeout = 47 * time.Second
	heldTestHeld    = 17 * time.Second
)

// heldTestRun starts a run and claims it for workerID, so it is running, assigned, at a known generation.
func heldTestRun(t *testing.T, ctx context.Context, store WorkflowStore, workerID string) *WorkflowInstance {
	t.Helper()
	wfID := startTerminableWorkflow(t, ctx, store, "held-reclaim", false)
	return claimByID(t, ctx, store, workerID, wfID)
}

// heldGenerationOf reads the marker straight from the row, which is the only way to tell "cleared" from "refused".
func heldGenerationOf(t *testing.T, store WorkflowStore, id string) sql.NullInt64 {
	t.Helper()
	var db *sql.DB
	var q string
	switch s := store.(type) {
	case *PostgresStore:
		db, q = s.db, `SELECT held_generation FROM workflow_leases WHERE id = $1`
	case *MySQLStore:
		db, q = s.db, `SELECT held_generation FROM workflow_instances WHERE id = ?`
	case *MSSQLStore:
		db, q = s.db, `SELECT held_generation FROM workflow_instances WHERE id = @p1`
	default:
		t.Fatalf("heldGenerationOf: unknown store type %T", store)
	}
	var g sql.NullInt64
	if err := db.QueryRow(q, id).Scan(&g); err != nil {
		t.Fatalf("reading held_generation of %s: %v", id, err)
	}
	return g
}

// heldReaps is the two statements every case runs through. The Except path is the one a worker with the holder veto configured uses; arming only ReapStaleInstances would
// silently turn the fast path off for it.
func heldReaps(t *testing.T, store WorkflowStore) []struct {
	name string
	reap func(ctx context.Context, timeout time.Duration) (int, error)
} {
	t.Helper()
	reaper, ok := store.(StaleHolderReaper)
	if !ok {
		t.Fatalf("%T does not implement StaleHolderReaper", store)
	}
	return []struct {
		name string
		reap func(ctx context.Context, timeout time.Duration) (int, error)
	}{
		{"ReapStaleInstances", func(ctx context.Context, timeout time.Duration) (int, error) {
			return store.ReapStaleInstances(ctx, timeout, 100)
		}},
		{"ReapStaleInstancesExcept", func(ctx context.Context, timeout time.Duration) (int, error) {
			return reaper.ReapStaleInstancesExcept(ctx, timeout, 100, nil)
		}},
	}
}

func TestAHeldRunIsReclaimedSoonerThanARunningOne(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			marker, ok := store.(HeldRunMarker)
			if !ok {
				t.Fatalf("%T does not implement HeldRunMarker", store)
			}

			for _, r := range heldReaps(t, store) {
				r := r
				armed := WithHeldReclaimAfter(context.Background(), heldTestHeld)
				for _, tc := range []struct {
					name      string
					marked    bool
					staleSecs int
					ctx       context.Context
					want      int // 1: this run is reclaimed; 0: it is left running
				}{
					{"a marked run stale by the held window is reclaimed", true, 20, armed, 1},
					{"an UNMARKED run stale by the held window is not", false, 20, armed, 0},
					{"a marked run that is fresher than the held window is not", true, 10, armed, 0},
					{"a marked run stale by the ordinary window is reclaimed", true, 60, armed, 1},
					{"an unmarked run stale by the ordinary window is reclaimed as it always was", false, 60, armed, 1},
					{"a marked run stale by the held window is NOT reclaimed when nothing armed the held arm", true, 20, context.Background(), 0},
				} {
					tc := tc
					t.Run(r.name+"/"+tc.name, func(t *testing.T) {
						ctx := context.Background()
						wf := heldTestRun(t, ctx, store, "held-worker")
						if tc.marked {
							if err := marker.MarkRunHeld(ctx, wf.ID, "held-worker", wf.Generation); err != nil {
								t.Fatalf("MarkRunHeld: %v", err)
							}
							if g := heldGenerationOf(t, store, wf.ID); !g.Valid || g.Int64 != wf.Generation {
								t.Fatalf("after MarkRunHeld, held_generation = %+v, want %d", g, wf.Generation)
							}
						}
						backdateHeartbeat(t, store, wf.ID, tc.staleSecs)

						// The count is not asserted: the database is shared by every case in this backend and a run an earlier case left stale and marked is
						// legitimately reclaimed by this call too. What each case asserts is what happened to ITS run.
						if _, err := r.reap(tc.ctx, heldTestTimeout); err != nil {
							t.Fatalf("%s: %v", r.name, err)
						}
						got := mustGetWorkflow(t, ctx, store, wf.ID)
						if tc.want == 1 && (got.Status != "ready" || got.Generation != wf.Generation+1) {
							t.Errorf("a reclaimed run is status %q generation %d, want ready and %d", got.Status, got.Generation, wf.Generation+1)
						}
						if tc.want == 0 && (got.Status != "running" || got.Generation != wf.Generation) {
							t.Errorf("a run that was not reclaimed is status %q generation %d, want running and %d", got.Status, got.Generation, wf.Generation)
						}
					})
				}
			}
		})
	}
}

// A run falsely reclaimed while held: its holder's clear is refused (the generation moved), so the old marker survives. The next owner must NOT be reclaimable at the held window
// because of it: the marker names the old generation and the arm asks for the CURRENT one. With `held_generation IS NOT NULL` in place of `held_generation = generation` this
// goes red, which is the point of it.
func TestAMarkerLeftByAReclaimedHolderDoesNotMakeTheNextOwnerReclaimable(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			marker, ok := store.(HeldRunMarker)
			if !ok {
				t.Fatalf("%T does not implement HeldRunMarker", store)
			}
			for _, r := range heldReaps(t, store) {
				r := r
				t.Run(r.name, func(t *testing.T) {
					ctx := context.Background()
					armed := WithHeldReclaimAfter(ctx, heldTestHeld)
					wf := heldTestRun(t, ctx, store, "old-holder")
					if err := marker.MarkRunHeld(ctx, wf.ID, "old-holder", wf.Generation); err != nil {
						t.Fatalf("MarkRunHeld: %v", err)
					}

					// The holder is falsely reclaimed (here by the ordinary window), which bumps the generation.
					backdateHeartbeat(t, store, wf.ID, 60)
					if n, err := r.reap(armed, heldTestTimeout); err != nil || n != 1 {
						t.Fatalf("setup: the held run was not reclaimed: n=%d err=%v", n, err)
					}

					// Its clear is refused: the claim has moved. Nothing is written.
					if err := marker.ClearRunHeld(ctx, wf.ID, "old-holder", wf.Generation); !errors.Is(err, ErrFenceLost) {
						t.Fatalf("the old holder's ClearRunHeld = %v, want ErrFenceLost", err)
					}
					if g := heldGenerationOf(t, store, wf.ID); !g.Valid || g.Int64 != wf.Generation {
						t.Fatalf("the refused clear changed the marker: %+v, want it left at %d", g, wf.Generation)
					}

					// A new owner claims it and is executing: heartbeat old enough for the held window, but not for the ordinary one.
					next := claimByID(t, ctx, store, "new-owner", wf.ID)
					if next.Generation == wf.Generation {
						t.Fatalf("setup: the next claim kept generation %d; the marker cannot be told from a current one", wf.Generation)
					}
					backdateHeartbeat(t, store, wf.ID, 20)
					if _, err := r.reap(armed, heldTestTimeout); err != nil {
						t.Fatalf("%s: %v", r.name, err)
					}
					if got := mustGetWorkflow(t, ctx, store, wf.ID); got.Status != "running" || got.Generation != next.Generation {
						t.Errorf("%s reclaimed the NEXT owner's executing run at the held window (status %q generation %d, want running and %d) because the old holder's marker (generation %d) was left behind", r.name, got.Status, got.Generation, next.Generation, wf.Generation)
					}
				})
			}
		})
	}
}

// The two marker writes are fenced on (assigned_to, generation) and the clear is idempotent for a holder that still owns the run. The second clear is the MySQL trap: it
// changes no row, MySQL reports zero rows, and a zero count must not be read as a lost claim (which would suspend a healthy run at its wake).
func TestTheHeldMarkerWritesAreFencedAndAClearIsIdempotent(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()
			marker, ok := store.(HeldRunMarker)
			if !ok {
				t.Fatalf("%T does not implement HeldRunMarker", store)
			}
			wf := heldTestRun(t, ctx, store, "owner")

			if err := marker.MarkRunHeld(ctx, wf.ID, "someone-else", wf.Generation); !errors.Is(err, ErrFenceLost) {
				t.Errorf("MarkRunHeld by a worker that does not own the run = %v, want ErrFenceLost", err)
			}
			if err := marker.MarkRunHeld(ctx, wf.ID, "owner", wf.Generation+1); !errors.Is(err, ErrFenceLost) {
				t.Errorf("MarkRunHeld at a generation the run is not at = %v, want ErrFenceLost", err)
			}
			if g := heldGenerationOf(t, store, wf.ID); g.Valid {
				t.Fatalf("a refused MarkRunHeld wrote the marker: %+v", g)
			}

			if err := marker.MarkRunHeld(ctx, wf.ID, "owner", wf.Generation); err != nil {
				t.Fatalf("MarkRunHeld: %v", err)
			}
			if err := marker.MarkRunHeld(ctx, wf.ID, "owner", wf.Generation); err != nil {
				t.Errorf("a second MarkRunHeld for the same hold = %v, want nil", err)
			}
			if err := marker.ClearRunHeld(ctx, wf.ID, "someone-else", wf.Generation); !errors.Is(err, ErrFenceLost) {
				t.Errorf("ClearRunHeld by a worker that does not own the run = %v, want ErrFenceLost", err)
			}
			if g := heldGenerationOf(t, store, wf.ID); !g.Valid {
				t.Fatalf("a refused ClearRunHeld cleared the marker")
			}
			if err := marker.ClearRunHeld(ctx, wf.ID, "owner", wf.Generation); err != nil {
				t.Fatalf("ClearRunHeld: %v", err)
			}
			if g := heldGenerationOf(t, store, wf.ID); g.Valid {
				t.Errorf("after ClearRunHeld, held_generation = %+v, want NULL", g)
			}
			if err := marker.ClearRunHeld(ctx, wf.ID, "owner", wf.Generation); err != nil {
				t.Errorf("ClearRunHeld of a marker that is already clear, by the owner = %v, want nil (on MySQL this changes no row, which is not a lost claim)", err)
			}
		})
	}
}

// ListStaleHolders is the population ReapStaleInstances WOULD reclaim, which the worker's veto channel asks about first (staleRowsToAsk), so it must carry the same held arm: armed, a marked
// run stale by the held window is listed; unarmed, or unmarked, it is not.
func TestListStaleHoldersListsAMarkedRunAtTheHeldWindow(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			marker, ok := store.(HeldRunMarker)
			reaper, ok2 := store.(StaleHolderReaper)
			if !ok || !ok2 {
				t.Fatalf("%T must implement HeldRunMarker and StaleHolderReaper", store)
			}
			ctx := context.Background()
			marked := heldTestRun(t, ctx, store, "marked-worker")
			if err := marker.MarkRunHeld(ctx, marked.ID, "marked-worker", marked.Generation); err != nil {
				t.Fatalf("MarkRunHeld: %v", err)
			}
			unmarked := heldTestRun(t, ctx, store, "unmarked-worker")
			backdateHeartbeat(t, store, marked.ID, 20)
			backdateHeartbeat(t, store, unmarked.ID, 20)

			listed := func(ctx context.Context) map[string]bool {
				holders, err := reaper.ListStaleHolders(ctx, heldTestTimeout, 100)
				if err != nil {
					t.Fatalf("ListStaleHolders: %v", err)
				}
				m := map[string]bool{}
				for _, h := range holders {
					m[h.Key.WorkflowID] = true
				}
				return m
			}
			if got := listed(WithHeldReclaimAfter(ctx, heldTestHeld)); !got[marked.ID] || got[unmarked.ID] {
				t.Errorf("armed: marked listed=%v unmarked listed=%v, want true and false", got[marked.ID], got[unmarked.ID])
			}
			if got := listed(ctx); got[marked.ID] || got[unmarked.ID] {
				t.Errorf("unarmed: marked listed=%v unmarked listed=%v, want false and false (20s is inside the 47s window)", got[marked.ID], got[unmarked.ID])
			}
		})
	}
}
