# cleat-230-logse — Log Cleanup Explorer

**Parent:** cleat-230 (Engine Reliability Polish)
**Budget:** $5 (~0.5 engineer-day)
**Type:** Exploration — survey and report, no code changes

## Task

Verify no remaining `log.Printf` in hot paths missed by cleat-226. Clean up any debug
logging that shouldn't be in production output.

### Scope

1. Search all Go source for `log.Printf`, `log.Println`, `fmt.Printf` in engine and worker code
2. Classify each call site: hot path (per-event/per-workflow), medium path (periodic),
   startup/one-time, CLI/tool, acceptable
3. Identify which hot-path calls should be migrated to `slog`
4. Identify debug `fmt.Printf` calls that should be removed or gated behind flags
5. Report findings — implementation is handled by a separate developer agent

### Out of scope

- Example code, benchmarks, tests (these are fine using log.Printf)
- Plugin implementations (each plugin manages its own logging)
- SDK runtime.go warnings (these are developer-error messages, not hot path)
