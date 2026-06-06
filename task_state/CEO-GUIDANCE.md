# CEO Guidance — cleat Engine (Next Lap)

**Date:** 2026-05-25
**Budget:** ~$100 (~10 engineer-days)
**Target:** WASM workflow debugger is complete and shipped. Engine is production-polished. No new features — just complete the last of the 2026-05-22 guidance and fix any remaining rough edges.
**Repo:** `/localssd/rcownie/cleat` (Apache 2.0)

## TL;DR

The critical items (checksums, RLS, signal wake-up, OTel, structured logging, divergence errors) are all deployed. The WASM debugger is 40% done (228a engine plumbing complete, 228b CLI ready for implementation). This lap finishes the debugger and cleans up. Shortest cleat lap yet — the engine is nearly done.

## What's Already Built

- Default checksum verification (#40)
- RLS fail-closed (#42)
- Max workflow duration flag + signal wake-up (#44)
- OTel exporter initialization + structured logging migration (#43)
- Divergence error payload enrichment (#42)
- WASM debugger engine plumbing — centralized `advanceReplayStep` (cleat-228a, $35)
- Multi-language wasmtime support (#46)
- Standalone clew-service binary
- engine.go.bak committed (0664fec)

**Still missing:**
- WASM debugger CLI — the `cleatctl debug <workflow-id>` command (cleat-228b, $55)
- Engine reliability polish — any remaining race conditions or edge cases found during dogfood

## This Lap: 2 Items

### 1. WASM Debugger CLI — cleat-228b ($80, ~8 days)

The debugger engine plumbing is done (cleat-228a). This item implements the CLI. Full contract in `projects/cleat/cleat-228b/CONTRACT.md`.

- [ ] **`cleatctl debug <workflow-id>` command:**
  - Load event history from DB for the given workflow (read-only connection)
  - Execute replay one event at a time, pausing after each event
  - At each pause, display: step number, event type, service/op, query_state snapshot
  - Support commands: `next` (advance one event), `continue` (run to end), `state` (dump full query_state), `events` (list remaining events), `quit`
  - Connect to same DB as worker (read-only, same connection string)
  - ~5 days.
- [ ] **`--watch` flag:** Tails event_history, auto-advances as new events arrive (live debugging). ~1.5 days.
- [ ] **Tests:** Step-through replay on a known workflow, watch mode with new events, error handling for missing workflow, read-only enforcement. ~1 day.
- [ ] **Documentation:** `docs/how-to/debug-workflows.md` — usage guide, example session, troubleshooting. ~0.5 day.

**Files:** `cmd/cleatctl/debug.go` (new), `internal/host/engine.go` (expose single-step replay API), `docs/how-to/debug-workflows.md` (new)
**Risk:** Medium — new engine API surface for single-step replay. Engine already replays for crash recovery; this adds pause-and-inspect on top.
**Dependencies:** cleat-228a (engine plumbing) — DONE

---

### 2. Engine Reliability Polish ($20, ~2 days)

Dogfood operations have surfaced a few rough edges. Fix them while the engine is fresh in mind.

- [ ] **Race condition audit:** The race-safe backend execution fix (7f7558f) and mutex protection (257d59a) addressed specific races found during dogfood. Audit the remaining hot paths for similar patterns — concurrent goroutine map access, shared state without synchronization. ~1 day.
- [ ] **Log cleanup:** Verify no remaining `log.Printf` in hot paths missed by cleat-226. Clean up any debug logging that shouldn't be in production output. ~0.5 day.
- [ ] **Error message quality:** Review error messages surfaced to clew users through the dashboard. Ensure they're actionable — "workflow failed: WASM trap" should include which trap and what the workflow was doing. ~0.5 day.

**Files:** `internal/host/engine.go`, `cmd/cleat-worker/main.go`
**Risk:** Low — polish work, not new features. Fixes found issues, doesn't introduce new behavior.

---

## Dependencies

```
Item 1 (WASM debugger CLI) — depends on cleat-228a (DONE)
Item 2 (reliability polish) — independent, can start immediately
```

Both items start in parallel.

---

## Budget

| # | Item | Budget | Days | Priority |
|---|------|--------|------|----------|
| 1 | WASM debugger CLI (228b) | $80 | ~8 | 1 — last remaining 2026-05-22 item |
| 2 | Engine reliability polish | $20 | ~2 | 2 — production readiness |
| **Total** | | **$100** | **~10 days** | |

---

## Success Criteria

1. **`cleatctl debug <id>` works.** Step through a workflow's event history interactively. Inspect query_state at each step. `--watch` tails live events.
2. **Debugger documented.** `docs/how-to/debug-workflows.md` with usage guide and example session.
3. **No known race conditions.** Audit complete, any found races fixed.
4. **Error messages are actionable.** User-facing errors include context: what was happening, which component, what to do.

## What NOT to Do This Lap

- **Python SDK CI/publishing.** Ecosystem work. Not needed for clew-service.
- **Plugin maturity audit.** 22 plugins work. Catalog them when someone asks.
- **Sharding, partitioning, canary deploys.** Scale work. clew-service will have tens of workflows, not millions.
- **Snapshot recovery / replay optimization.** Research project.
- **New plugins or integrations.** Engine is feature-complete for clew MVP.
- **TinyGo compatibility hardening.** Standard Go compilation is the default (#36). TinyGo is deprecated.

---

## Looking Ahead

The engine's next lap after this depends on clew-service operational experience. If workflow volume grows, consider partitioning. If operations needs more tools, consider an admin repair API. But the most likely path is: nothing. The engine is infrastructure. Once it's safe, observable, and debuggable, the right thing is to leave it alone and focus on clew.
