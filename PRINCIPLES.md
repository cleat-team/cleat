# Cleat Design Principles

Cleat is a durable workflow engine on PostgreSQL: write workflows in Go, compile
to WASM, deploy via INSERT. These principles guide every design decision. Before
proposing a change, ask: *does this align with cleat's principles?*

---

## 1. PostgreSQL is the source of truth

Workflow state, event history, schedules, signals, and deployment metadata all
live in PostgreSQL. There are no external coordination services, no message
queues, no separate databases for different concerns. This means you can
back up, restore, inspect, and migrate your entire workflow system with
standard PostgreSQL tooling -- `pg_dump`, `pg_restore`, `psql`, and any
PostgreSQL-compatible ORM or dashboard.

**Do this:** Store workflow state, event history, and schedules in PostgreSQL
tables. Use `SELECT ... FOR UPDATE SKIP LOCKED` for work claiming. Ensure a
single `pg_dump` captures the entire system state.

**Not that:** Introduce Redis for queue management, etcd for leader election,
Kafka for event streaming, or any auxiliary data store that requires separate
backup procedures, operational expertise, or failure modes.

**How it guides contributions:** A proposed feature that requires a new service
(e.g., "add RabbitMQ for async delivery") must first exhaust what can be done
with PostgreSQL features (LISTEN/NOTIFY, pg_notify, polling with SKIP LOCKED).
If the feature genuinely requires an external service, it belongs in a separate
plugin or integration -- not in cleat core.

---

## 2. WASM-first sandboxing

User workflow code runs inside WebAssembly, not natively. WASM provides
deterministic execution (no undefined behavior), language flexibility (Go,
Rust, AssemblyScript, and more targets), and a security boundary (user code
cannot access the host system except through explicit HostCalls). No user code
ever runs in the worker process address space.

**Do this:** Compile workflow code to WASM, execute it in a sandboxed wazero
runtime, and route all external interactions through the 15 defined HostCall
imports on the `env` module.

**Not that:** Load user code as a native Go plugin (`plugin.Open`), execute it
via shared library FFI, or embed a scripting language interpreter in the worker
process.

**How it guides contributions:** Any change that bypasses the WASM boundary,
adds host-side execution paths for user code, or weakens the sandbox must be
rejected. New HostCall imports must be carefully designed for determinism
and auditability (recorded in event history). Performance improvements to
WASM execution are welcome; shortcuts around it are not.

---

## 3. Boring infrastructure

If you have PostgreSQL and can run a Go binary, you can run cleat. No
ZooKeeper, no etcd, no message queue, no Kubernetes operators. The worker is
a single stateless Go binary that polls PostgreSQL. Scale by running more
copies. Deploy by shipping a binary, not by operating a control plane.

**Do this:** Run `cleat-worker --db postgres://...` on a VM, a container, or
a bare-metal server. Add more workers for capacity. The only runtime dependency
is a reachable PostgreSQL database.

**Not that:** Require Kubernetes CRDs, a service mesh, a dedicated operator,
or any orchestration layer beyond what the user already has for their own
applications.

**How it guides contributions:** New features should not introduce new runtime
dependencies. If a feature needs state coordination, it must use PostgreSQL --
not a consensus protocol, not a distributed cache, not a sidecar. Features
that *optionally* integrate with external systems (e.g., a Helm chart for
Kubernetes deployment, a Terraform provider) belong in the ecosystem, not in
the core.

---

## 4. Explicit over magic

The transformer pipeline makes everything visible: call graph analysis, closure
computation, parameter auto-threading, WASM export generation. You can inspect
the generated code at every stage with `cleat vet`. There is no code generation
that runs invisibly during build, no reflection-based runtime magic, no
unnamed goroutines doing implicit work.

**Do this:** Run `cleat vet` to see entry points, call graphs, threading
analysis, and closure errors. Use `cleat build --verbose` to examine each
pipeline stage. The generated source is in the output directory for review.

**Not that:** Use `go:generate` comments, code generation macros, or build
tags that produce output the developer cannot easily inspect. Avoid runtime
reflection to wire up calls that the transformer could resolve statically.

**How it guides contributions:** New pipeline stages must produce human-readable
intermediate output. Tooling that obscures what the compiler does ("trust us,
it works") is not acceptable. Error messages from the pipeline must point to
specific source locations. If an optimization makes the pipeline harder to
debug, it should be opt-in.

---

## 5. Workflows as data

Workflows are compiled WASM blobs stored as rows in PostgreSQL tables. You
deploy a new version with `INSERT INTO workflow_defs ...`. You roll back by
updating a version pointer. The workflow catalog is a database table you can
query, join, and inspect with standard SQL. Versions are immutable rows, not
mutable tags -- every deployment creates a new row with an incremented version.

**Do this:** `cleat deploy` produces an INSERT. `cleat rollback` produces
an UPDATE on a version pointer. You can query `SELECT * FROM workflow_defs
WHERE name = 'order_processor' ORDER BY version DESC` to see all versions.

**Not that:** Store WASM blobs on a filesystem, in an S3 bucket, or in a
separate artifact registry. Manage versions through git tags, file paths,
or a separate deployment tool.

**How it guides contributions:** Any feature that needs workflow metadata
(version history, deployment timestamps, entry point lists) must read from
PostgreSQL, not from a sidecar file store. The `workflow_defs` table is the
canonical deployment catalog. If the schema needs to grow, it grows in
PostgreSQL -- not in a manifest file, a registry API, or a configuration
language.

---

## 6. Determinism through replay

Workflow execution is event-sourced. The event history (`event_history` table)
is the source of truth for what happened during execution. On replay -- after
a worker crash, a restart, or a migration -- the host replays the event
history and calls return cached responses instead of re-executing. This means
replay always produces bit-identical state. It also enables debugging ("what
was the state when this call was made?"), migration ("copy event history to
new cluster and replay"), and audit ("prove this workflow ran with these
inputs").

**Do this:** Record every `DurableCall`, `DurableSleep`, `AwaitSignals`, defer,
and child workflow invocation in the event history. On replay, serve cached
responses. Never re-execute a completed step.

**Not that:** Execute workflow code nondeterministically (using `time.Now()`,
`rand.Int()`, or `os.Getpid()` inside workflow code). Skip event recording for
"fast" paths. Allow users to opt out of event sourcing for performance.

**How it guides contributions:** New HostCalls must be designed for
deterministic replay: they must produce the same result when replayed with
the same event history. Non-deterministic features (e.g., "random timeout
jitter") must be implemented on the host side with deterministic seed
derivation. The replay path must be tested and benchmarked -- it is not a
rare edge case, it is the primary execution mode after restarts.

---

## 7. Meet developers where they are

Cleat's native SDK is Go -- write standard Go functions, no DSL, no special
annotations, no framework inheritance. The `cleat build` pipeline handles WASM
compilation transparently. Rust support is first-class via `cleat-sdk` crate
and `#[cleat_entry]` proc-macros. Community SDKs cover Python, Java, and
AssemblyScript. The developer writes idiomatic code in their language of
choice; cleat handles the rest.

**Do this:** Write `func MyWorkflow(h cleat.HostCalls, input string) error`
in Go, or annotate a Rust function with `#[cleat_entry]`. Run `cleat build`
to produce a WASM binary. Test with `cleattest.TestEnv` without compiling to
WASM.

**Not that:** Require developers to learn a DSL, inherit from a framework base
class, annotate every function with decorators, or manually manage the WASM
compilation pipeline (writing imports, exports, memory layout).

**How it guides contributions:** SDK changes should reduce ceremony, not add
it. A new feature should fit naturally into the host language's idioms (e.g.,
Go errors, Rust Results, Python exceptions). If a feature requires an
annotation or decorator in one language, it should require the same in all
languages (or be handled by the build pipeline). New languages for the SDK
are welcome as community contributions, following the same HostCall boundary.

---

## 8. Observability is not optional

Every workflow execution is observable by default. The worker exports
Prometheus metrics (`/metrics`) covering throughput, latency, error rates,
and queue depth. Structured logging through HostCalls (`LogKV`) is recorded
in event history. The embedded Svelte web UI provides workflow list/detail
views, schedule management, and live execution state. You should never need
to wonder what your workflows are doing.

**Do this:** Export latency histograms, error counters, and instance-state
gauges as Prometheus metrics. Log structured key-value pairs through HostCalls
that appear in the event history and web UI. Ship the web UI embedded in the
worker binary -- one process, one port, everything visible.

**Not that:** Make observability opt-in ("add `--enable-metrics`"), require
a separate observability stack, log only to stdout and expect users to grep,
or rely on external APM agents for basic visibility.

**How it guides contributions:** Every new feature must include observability
considerations: What Prometheus metrics does it need? What log messages should
appear in the event history? What does the web UI need to show? A feature
that ships without observability hooks is incomplete. Conversely, metrics dashboards
and UI components should be maintained alongside the feature code.

---

## Applying these principles

These principles are not absolute laws -- every project faces tradeoffs. But
they define the default answer: **if a proposal violates a principle, it
carries the burden of proof.** The proposal must explain which principle is
being violated, why the violation is justified, and what compensating
mechanisms exist.

When reviewing contributions, ask:

- Does this require a new runtime dependency?
- Does this bypass the WASM sandbox?
- Does this make the system harder to back up with pg_dump?
- Does this add invisible behavior the developer cannot inspect?
- Does this reduce observability?
- Does this add ceremony to the developer experience?

If the answer to any of these is "yes," the contribution needs strong
justification.
