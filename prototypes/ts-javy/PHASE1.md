# Phase 1 — host calls and suspension (branch `proto/ts-javy`, remote `feature/proto-ts-javy`)

Prototype only. No PR to `develop`, no `tiers.yaml` entry, no support claim anywhere. Plan:
`cleat-internal/typescript-javy-prototype-plan-2026-11-08.md`, Q1–Q2 and §4.3/§4.4.

## Result against the Phase 1 exit (Q2)

> sleep, signal and child workflows suspend and resume across a worker restart, with identical history across 3 replays.

**Met, with the limits listed under "Not measured".** One command re-derives all of it, on the final tree:

    prototypes/ts-javy/phase1-verify.sh        # exits 0; 2 would mean a check could not run

| What | Result | Raw output |
|---|---|---|
| Sleep, signal, child, and all three in one run, each killed with `kill -9` while suspended, then restarted | 9 of 9 checks pass; every run completes with the correct result | `evidence/phase1/q2-restart.txt` |
| Persisted history rows are never rewritten by later segments (full rows, every column, snapshotted in flight) | A→B→C→D each an unchanged prefix of the next, across two SIGKILLs | same file, S4 |
| 3 replays of an in-flight history, wasmtime backend | **IDENTICAL x3: true** — see the exact meaning of "identical" below | `evidence/phase1/q2-replay-3x.txt` |
| `finally` does not run when a segment suspends | 0 trace lines while suspended, 1 at completion; **red control** (suspension-by-throw build) runs it during suspension | `q2-restart.txt`, S5 |
| `Promise.race` / `Promise.all` over a suspended and a ready promise | same result on 3 runs, each suspending twice | `q2-restart.txt`, S6 |
| Error and exception paths | all five terminate `failed` in ≤ 2.1 s, none hangs, **0** WASI-policy log lines | `evidence/phase1/errors.txt` |

### What "identical" means in the replay test, exactly

The test (`replay/replay_test.go`) loads a live in-flight run's history **through the engine's own
`PostgresStore.LoadEventHistory`** (no hand-written row mapping) and replays it three times on the wasmtime backend.

- Every **persisted** event (4 of them) is compared exactly, timestamps included.
- The replay appends one event of its own (5 in the output history). That event carries the replay's wall clock in
  `timestamp_ms` — `TimestampMs` is documented as "the wall-clock time when the event was recorded" — and the first
  run of this test **failed on exactly that field**: `event 4 field timestamp_ms: 1794589855008 vs 1794589855009`
  (then `…011`). The test now zeroes `timestamp_ms`/`created_at` only for events at index ≥ the length of the input
  history, and says why in a comment. Nothing else is normalised.

**Red controls, both detected** (a control must show a detected tamper — an error or an outcome that differs from the
genuine one):

- A: step 1 `signal_received` retyped as `child_workflow` → the module fails with a JS error through the ABI.
- B: one event removed from the middle → the engine's density refusal, `replay history is incomplete: event 1 of 3
  carries step 2`.

Recorded and **not asserted**: renaming the recorded child (`ChildName`) is "detected" only in the sense that the outcome
differs (`err=""`); it is not an engine refusal. Whether the engine checks a child's name on replay is a property of the
engine's child guard and is the same for every guest language.

## Decisions from the coordinator's "Go for Phase 1"

1. **Branch name** kept: remote `feature/proto-ts-javy`. No workflow triggers on pushes to `feature/**`
   (`gh run list --branch feature/proto-ts-javy` → 0 runs; control: `develop` → 3).
2. **`javascript` label.** Added to `engine.WasmtimeLanguages` on this branch only (`engine/engine.go`). Inventory of
   what a language label selects, from reading the code (`git grep -nE 'WasmtimeLanguages|deferSegmentLanguages'`,
   non-test):
   - `Engine.resolveBackend` (`engine/executor.go:75`): the label picks the backend. An unlisted label is refused with
     "no WASM backend registered for guest language". `WithBackends(WasmtimeLanguages, …)` registers wasmtime for every
     listed label, in the worker (`cmd/cleat-worker/backend_routing.go:17`) and in `cleat/wasmtest`.
   - `engine/backend_wasmtime.go:826` and `:1030`: only `go` (runs `_start`, poll-work dispatcher) and `java` (runs
     `_start` for runtime init) are special-cased. **Every other label, `javascript` included, takes the direct-export
     path** — the one `assemblyscript` and `rust` take.
   - `deferSegmentLanguages` (`engine/engine.go`, enforced at `engine/executor.go:349`): a defer segment for a label not
     in the map **fails closed**. `javascript` is deliberately **not** in it, and this prototype binds no defer host
     call, so a JavaScript guest never has outstanding defers. A productised JS guest that wants defers needs the SDK to
     decode the suspend sentinel *and* an entry in that map: a second, separate change.
   - Q1 is therefore restated as the coordinator asked: **no change to engine mechanisms**. The label is a one-line
     registration and is recorded as a required productisation change.
3. **Static vs dynamic.** Static measured (Phase 0, ≈ 1.095 MB per module). Dynamic: **blocked**, estimate below.
4. **Error and exception paths**: table below. `fd_seek` was **not reached** on any of them.
5. **Exit criterion**: the table at the top.

## Error and exception paths (`evidence/phase1/errors.txt`)

Each is a separate entry point of `workflows/errors/errors.ts`; the input names it via `__entry_point` (a module with
several entry points needs that — the first version of this driver omitted it, all five runs "failed" for that reason,
and the driver now exits 2, UNMEASURED, when it sees that refusal).

| entry | ends as | what the API error contains |
|---|---|---|
| `thrower` (throw in the workflow) | `failed`, `error_code='unknown'` | `wasm trap: host: export "thrower": {"error":"Error: thrower: deliberate failure\n    at …"}` |
| `rejecter` (`Promise.reject("a string")`) | `failed` | `{"error":"rejecter: a string, not an Error"}` |
| `late_thrower` (sleep 1500, then throw) | `failed` after 2.1 s, i.e. on the **resumed** segment | `Error: lateThrower: failed after a resume` |
| `raw_throw` (no `workflow()` wrapper; nothing catches it) | `failed` | a bare wasm backtrace ending `wasm \`unreachable\` instruction executed` — **no JS message**; the message is on the worker's stderr (`Error: rawThrow: nothing catches this`, one line in the worker log) |
| `oom` (allocate until refused) | `failed` in 0.3 s | `InternalError: out of memory` — QuickJS refuses cleanly and the SDK reports it; no trap, no host OOM |

Notes: guest failures end as `error_code='unknown'` where the engine's own entry-point refusal ends `permanent`; I
observed the runs `failed` at the poll and did not observe them beyond that with a worker running.

## Observations that are not findings yet

1. **`WARN replay history is shorter than the instance's recorded event count` on `journey`.** `recorded_event_count=5
   loaded_events=4`, final `event_count=6` against 4 persisted rows. Every such run completed with the correct result.
   **Corrected 2026-11-13 (Phase 2):** this section first said the rate varied (5 of 5, then 3 of 5) and called that
   unexplained. It was my harness: worker processes from earlier scripts survived and claimed runs whose log lines went to
   files the counting script did not read. With exactly one worker alive for the whole run, the same bisect gives **5 of 5**
   (`evidence/phase1/warn-bisect.txt`), including the two variants with no kill, so it is not a restart artifact. The Go control
   that this section said had not been run **was** run in Phase 2: a Go guest logs it 10 of 10, the JS guest 10 of 10, so it is
   an engine behaviour, filed as cleat#3779. My reading that re-suspending at a child wait counts an event that is never
   persisted is still **not measured**. Related, closed: cleat#3391.
2. **The build was not byte-reproducible (fixed in Phase 2: see PHASE2.md).** Two consecutive builds of identical source and an identical esbuild bundle
   (same `js` hash) differ at the Javy stage in 3 bytes of one 7-byte data segment (`cmp -l` → 3). The cause is not
   established; a Wizer-snapshotted heap value is the obvious place to look. A deployed module's bytes are fixed, so
   replay is unaffected; it matters for content-addressed dedupe and for the plan's engine-pin item.
3. **`Math.random`, `Date` and the other nondeterministic globals are not shimmed yet.** That is Phase 2 (§4.5), with
   its red controls. `racer` covers only promise-ordering determinism and must not be read as more.

## Dynamic mode — the engine change it needs (estimate by reading; not run)

A dynamic-mode `sleeper` module is **4,051 bytes** and defines no host imports of its own: it imports `cabi_realloc`,
`invoke` and `memory` from a module named `cleat-js-plugin-v0` and exports `_start` plus the entry. Against the stock
engine:

- `engine/backend_wasmtime.go:785–815` builds one linker, registers the `env`/WASI imports for the module being
  instantiated, instantiates **one** module, then reads `instance.GetExport(store, "memory")` (line 811). A dynamic
  guest has no exported memory (it imports one), so it would be refused with "module has no exported memory".
- What the engine would need: (a) a **host-supplied module** concept — a pinned, content-hashed plugin artefact stored
  with the deployment, which the stock engine has no notion of; (b) instantiate it first with the `env` and WASI imports,
  then `DefineInstance("cleat-js-plugin-v0", …)` on the linker before the guest; (c) take `memory` from the plugin
  instance for scratch-buffer placement (`scratchBaseFor`); (d) the entry trampoline (my `cleat-js-link` step) must move
  into the 4 KB guest as imports from the plugin; that part is tooling, not engine.
- This is a change to engine mechanisms, so by the coordinator's rule it needs a go before it is built. It is the only
  route to Q5's dynamic-size line.

## Not measured (limits of this phase)

- `kill -9` was applied **while the run was suspended**, not in the middle of a guest executing.
- The 3 replays are three calls in one process against one history, not three fresh processes.
- (Both listed here originally are resolved in Phase 2: the Go control was run, and the reproducibility cause was found.)
- Failed runs were not observed beyond the first terminal poll with a live worker.
- Timing and size thresholds (§5) are Phase 4. Phase 1 recorded none.

## Harness defects found and fixed while producing this (so they are not repeated)

- A helper stripped trailing tabs from `psql` output, so rows with empty trailing columns came back short (`IndexError`).
- A "history prefix unchanged" snapshot was taken after the run completed (a completed run's `event_history` is
  deleted); it now asserts the run is still in flight at each snapshot.
- A check named "reclaim_count shows no failure path" was written `… == 0 or True` and could not fail; it is now an
  `info` line.
- A first red control compared a tampered outcome to the genuine one while every outcome differed in a timestamp, so it
  could not distinguish a detected tamper from noise; it now normalises only replay-recorded stamps (above).
- The error driver reported `hung: none`, exit 0, over five runs that never reached the code under test; it now exits 2.
- `tools/tsmeta` lived in the root Go module and was in its build; both Go programs are now in the nested module
  `prototypes/ts-javy/go.mod` (always run with `GOWORK=off`): `go list ./... | grep -c prototypes` → 0 from the root.

- **Worker processes outlived the scripts that started them.** `phase1_verify.py` and `phase1_warn_bisect.py` never stopped their
  workers, so a later script's `/healthz` answered from the old worker, two workers shared the database, and every count taken
  from a log file was short. It produced the wrong "3 of 5" above and, in Phase 2, a Go control that read 0 of 10. Found because
  an end-to-end run left two `cleat-worker` processes alive. Every driver now stops stale workers first, refuses to run if one
  survives, and stops its own at exit; `phase2-verify.sh` flags a leak between steps.

## Layout

    prototypes/ts-javy/
      PHASE0.md  PHASE1.md  toolchain.lock
      phase1-verify.sh  phase1_verify.py  phase1_replay.py  phase1_errors.py  phase1_warn_bisect.py  phase1_lib.py
      build.sh                      workflow.ts -> esbuild -> javy (static, cleat-js plugin) -> cleat-js-link -> tsmeta
      plugin/  link/                Rust: the Javy plugin and the walrus linker
      packages/cleat-ts/            the SDK
      workflows/{sleeper,signaler,parent,racer,finalizer,journey,errors}/
      go.mod  tools/tsmeta/  replay/   nested Go module (tools and the replay test)
      evidence/phase1/              raw output of the final run
