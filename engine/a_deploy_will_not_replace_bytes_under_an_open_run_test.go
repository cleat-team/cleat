package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// cleat#3497: a deploy that would replace a version's WASM while runs of that
// version are still open is refused, on every dialect. See
// engine/workflow_def_replacement.go for the rule and why.
//
// EVERY CASE ASSERTS A PRECONDITION FIRST, because the refusal and the no-op are
// indistinguishable from "the fixture never made an open run": with no open run
// the deploy is allowed, which is also what a broken guard does. So each case that
// expects a refusal first reads the run back and checks it is not settled, and
// each case that expects an allowance after a settle checks the run IS settled.
func TestADeployWillNotReplaceBytesUnderAnOpenRun(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()

			bytesA := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x0a}
			bytesB := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x0b}
			bytesC := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x0c}
			bytesD := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x0d}

			deploy := func(name string, version int, wasm []byte, force bool) error {
				return store.DeployWorkflowDef(ctx, &WorkflowDef{
					Name: name, Version: version, WASMBytes: wasm,
					ABIVersion: 1, MinVersion: 1, ReplaceUnderOpenRuns: force,
				})
			}
			stored := func(t *testing.T, name string, version int) []byte {
				t.Helper()
				got, err := store.LoadWASM(ctx, name, version)
				if err != nil {
					t.Fatalf("LoadWASM(%s v%d): %v", name, version, err)
				}
				return got
			}
			startRun := func(t *testing.T, name string, version int) string {
				t.Helper()
				id, existed, err := store.StartNewRun(ctx, "", name, version, json.RawMessage(`{}`), "", DefaultTenantUUID, 0)
				if err != nil || existed {
					t.Fatalf("UNMEASURED: could not start a run on %s v%d (existed=%v): %v", name, version, existed, err)
				}
				return id
			}
			requireStatusSettled := func(t *testing.T, id string, wantSettled bool) {
				t.Helper()
				inst, err := store.GetWorkflowByID(ctx, id)
				if err != nil || inst == nil {
					t.Fatalf("UNMEASURED: cannot read run %s back: %v", id, err)
				}
				settled := map[string]bool{"done": true, "failed": true, "dead_lettered": true, "terminated": true, "cancelled": true}[inst.Status]
				if settled != wantSettled {
					t.Fatalf("UNMEASURED: run %s has status %q, but this case needs it settled=%v, "+
						"so the deploy below would not be testing the rule", id, inst.Status, wantSettled)
				}
			}

			t.Run("different bytes on a version with an open run are refused and nothing is written", func(t *testing.T) {
				const name = "replace-refused"
				if err := deploy(name, 1, bytesA, false); err != nil {
					t.Fatalf("first deploy: %v", err)
				}
				id := startRun(t, name, 1)
				requireStatusSettled(t, id, false)

				err := deploy(name, 1, bytesB, false)
				if !errors.Is(err, ErrWorkflowDefInUse) {
					t.Fatalf("deploying different bytes onto v1 with an open run returned %v, want ErrWorkflowDefInUse", err)
				}
				var inUse *WorkflowDefInUseError
				if !errors.As(err, &inUse) || inUse.OpenRuns != 1 || inUse.Name != name || inUse.Version != 1 {
					t.Fatalf("the refusal does not name what it refused: %+v (err %v), want %s v1 with 1 open run", inUse, err, name)
				}
				if got := stored(t, name, 1); !bytes.Equal(got, bytesA) {
					t.Fatalf("a refused deploy still changed v1's bytes: have %x, want %x", got, bytesA)
				}
			})

			t.Run("identical bytes stay an idempotent upsert even with an open run", func(t *testing.T) {
				const name = "replace-idempotent"
				if err := deploy(name, 1, bytesA, false); err != nil {
					t.Fatalf("first deploy: %v", err)
				}
				requireStatusSettled(t, startRun(t, name, 1), false)
				if err := deploy(name, 1, bytesA, false); err != nil {
					t.Fatalf("redeploying the SAME bytes with an open run was refused: %v", err)
				}
			})

			t.Run("a new version is unaffected by the old one's open runs", func(t *testing.T) {
				const name = "replace-new-version"
				if err := deploy(name, 1, bytesA, false); err != nil {
					t.Fatalf("first deploy: %v", err)
				}
				requireStatusSettled(t, startRun(t, name, 1), false)
				if err := deploy(name, 2, bytesC, false); err != nil {
					t.Fatalf("deploying v2 while v1 has an open run was refused: %v", err)
				}
				if got := stored(t, name, 1); !bytes.Equal(got, bytesA) {
					t.Fatalf("deploying v2 changed v1's bytes: %x", got)
				}
			})

			t.Run("another version's open run does not block replacing this one", func(t *testing.T) {
				const name = "replace-other-version-run"
				if err := deploy(name, 1, bytesA, false); err != nil {
					t.Fatalf("deploy v1: %v", err)
				}
				if err := deploy(name, 2, bytesA, false); err != nil {
					t.Fatalf("deploy v2: %v", err)
				}
				requireStatusSettled(t, startRun(t, name, 2), false) // an open run on v2 only
				if err := deploy(name, 1, bytesB, false); err != nil {
					t.Fatalf("replacing v1, which has no runs, was refused because v2 has one: %v", err)
				}
			})

			t.Run("the force option replaces the bytes under an open run", func(t *testing.T) {
				const name = "replace-forced"
				if err := deploy(name, 1, bytesA, false); err != nil {
					t.Fatalf("first deploy: %v", err)
				}
				requireStatusSettled(t, startRun(t, name, 1), false)
				if err := deploy(name, 1, bytesB, true); err != nil {
					t.Fatalf("a forced replacement was refused: %v", err)
				}
				if got := stored(t, name, 1); !bytes.Equal(got, bytesB) {
					t.Fatalf("a forced replacement did not write the new bytes: have %x, want %x", got, bytesB)
				}
			})

			t.Run("a settled run no longer blocks the replacement", func(t *testing.T) {
				const name = "replace-after-settle"
				if err := deploy(name, 1, bytesA, false); err != nil {
					t.Fatalf("first deploy: %v", err)
				}
				id := startRun(t, name, 1)
				requireStatusSettled(t, id, false)
				if err := deploy(name, 1, bytesB, false); !errors.Is(err, ErrWorkflowDefInUse) {
					t.Fatalf("UNMEASURED: the refusal this case depends on did not happen first: %v", err)
				}
				if err := store.CancelWorkflow(ctx, id, "settle for the replacement test"); err != nil {
					t.Fatalf("UNMEASURED: could not settle the run: %v", err)
				}
				requireStatusSettled(t, id, true)
				if err := deploy(name, 1, bytesD, false); err != nil {
					t.Fatalf("replacing the bytes after the only run settled was refused: %v", err)
				}
				if got := stored(t, name, 1); !bytes.Equal(got, bytesD) {
					t.Fatalf("the replacement after settle did not write: have %x, want %x", got, bytesD)
				}
			})

			// Two overlapping FIRST deploys of one version. The check reads the
			// existing row FOR UPDATE, and on MySQL at REPEATABLE READ an absent
			// key makes that a gap lock that both sessions hold, so each one's
			// INSERT waited on the other and one was killed with error 1213
			// (cleat#3509 review). A client retrying a deploy that is still in
			// flight does exactly this, with identical bytes. Neither is a case
			// the rule refuses: with no row there is no run, so every call must
			// succeed. The race is milliseconds wide, so one round proves little;
			// each round uses a fresh name so every round is a first deploy.
			for _, c := range []struct {
				label string
				same  bool
			}{{"identical bytes", true}, {"different bytes", false}} {
				c := c
				t.Run("overlapping first deploys with "+c.label+" all succeed", func(t *testing.T) {
					const rounds, racers = 8, 4
					for r := 0; r < rounds; r++ {
						name := fmt.Sprintf("replace-concurrent-first-%t-%d-%d", c.same, r, time.Now().UnixNano())
						start := make(chan struct{})
						errs := make([]error, racers)
						var wg sync.WaitGroup
						for i := 0; i < racers; i++ {
							wasm := bytesA
							if !c.same {
								wasm = []byte{0x00, 0x61, 0x73, 0x6d, 0x01, byte(0x20 + i)}
							}
							wg.Add(1)
							go func(i int, wasm []byte) {
								defer wg.Done()
								<-start
								errs[i] = deploy(name, 1, wasm, false)
							}(i, wasm)
						}
						close(start)
						wg.Wait()
						for i, err := range errs {
							if err != nil {
								t.Fatalf("round %d: racer %d of %d deploying %s v1 for the first time failed: %v", r, i, racers, name, err)
							}
						}
						if got := stored(t, name, 1); len(got) == 0 {
							t.Fatalf("round %d: %s v1 has no bytes after %d successful deploys", r, name, racers)
						}
					}
				})
			}

			t.Run("different bytes with no run at all are allowed", func(t *testing.T) {
				const name = "replace-no-runs"
				if err := deploy(name, 1, bytesA, false); err != nil {
					t.Fatalf("first deploy: %v", err)
				}
				if err := deploy(name, 1, bytesB, false); err != nil {
					t.Fatalf("replacing the bytes of a version nothing is running was refused: %v", err)
				}
			})
		})
	}
}
