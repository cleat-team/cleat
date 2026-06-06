# cleat-230-racee — Race Condition Audit

**Parent:** cleat-230 (Engine Reliability Polish)
**Budget:** $10 (~0.5 engineer-day)
**Type:** Exploration — read-only audit, no code changes

## Task

Audit the engine and worker codebase for potential race conditions. Identify
concurrent access patterns that could lead to data corruption, deadlocks,
or non-deterministic behavior.

### Scope

1. Review all goroutine spawn sites in `engine/`, `cmd/cleat-worker/`, `internal/host/`
2. Identify shared mutable state accessed from multiple goroutines
3. Check synchronization primitives (mutexes, channels, atomics) for correctness
4. Audit database access patterns for transactional safety
5. Review WASM runtime lifecycle for concurrent access issues
6. Check plugin access patterns for thread safety
7. Report findings with severity classification — no code changes

### Out of scope

- Example code, benchmarks, tests (these are not production hot paths)
- Plugin implementations (each plugin manages its own concurrency)
- External service integrations (these are the plugin's responsibility)
