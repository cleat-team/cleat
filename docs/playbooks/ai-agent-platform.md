# Playbook 1 — AI agent platform with per-tenant budgets

**Status:** engineering reference. Drafted 2026-09-14 against `develop` at `654d6f84`.
Nothing here has been built end to end; see [What was verified](#what-was-verified) at the end.

**Who this is for:** you are building a product where a user's request kicks off an agent that
thinks for anywhere from ten seconds to ten minutes, calls tools, retrieves documents, sometimes
waits for a human, and costs real money at every step. You have many customers and you need each
one's spend and blast radius bounded separately.

This is the use case where durable execution stops being a nice property and becomes the only way
the thing works at all.

---

## Why agents are a durability problem in disguise

A conventional request-response service can afford to fail: the client retries and you are back
where you started. An agent run cannot. By step nine it has spent real money on eight model calls,
written to two external systems, and accumulated context that cost tokens to build. **Losing it is
not a retry, it is a refund.**

So the properties an agent platform needs are, almost exactly, the properties a durable workflow
engine already has:

| Agent platform needs | Durable engine calls it |
|---|---|
| Resume a run mid-flight after a crash | replay from event history |
| Never repeat a paid side effect | recorded steps not re-invoked |
| Pause for human approval, for hours | a signal, or a durable promise |
| Prove afterwards what the model was asked and answered | the event history, which already exists |
| Bound one customer's spend without bounding another's | per-tenant ceilings |

The interesting claim of this playbook is that **you do not build any of those.** You tie your
model provider and your vector index to the cleat, and they come with the boat.

---

## What ties to the cleat, and what stays rope

**Ties to the cleat:**

- Agent run state and step sequencing — the workflow itself
- Every model call, recorded with its inputs and its answer
- Retrieval results, recorded
- Tool invocations and their side effects
- Human approval gates
- Per-tenant execution ceilings
- The audit trail, which is the same artifact as the replay log

**Stays rope:**

- Your model provider. `plugins/llm/` already speaks Anthropic, OpenAI, Gemini, Groq, Mistral and
  Ollama (`plugins/llm/providers/`).
- Your front-end. A chat UI is a chat UI; build it however you build it, serve it from a CDN.
- Your embedding pipeline's source documents, in object storage.
- Your evaluation harness, your prompt management, your model choice.

---

## The assembly

| Concern | Component | Hitch point |
|---|---|---|
| Model calls | `llm`: `chat`, `chat_stream`, `embed`, `list_models` | host functions |
| Retrieval | `pgvector`: `search`, `upsert`, `delete` | host functions |
| Documents and artifacts | `blobstore`: `get`, `put` (S3-backed) | host functions |
| Model / prompt rollout | `featureflags`: `evaluate_flag` | host function |
| Human approval | workflow signals, or `eventtriggers`: `await_event` | host function |
| Per-tenant request limits | `ratelimiter` | edge middleware |
| User login | `oauthprovider` (OIDC; GitHub, Okta) | edge middleware |
| Who-did-what | `auditlog` | edge middleware |
| Long-run notification | `email`, `slacknotify`, `notifications` | host functions |

---

## The part worth reading twice: replay and paid calls

This is where an agent platform built on a generic queue goes wrong, and where cleat's design has
already done the thinking.

A host function declares **two independent properties** (`plugin/plugin.go:221-243`), and the
engine re-invokes on replay only when both are true:

- `Idempotent` — is calling this again *safe*? Says nothing about what it returns.
- `SameValueOnReplay` — would re-invoking during a replay yield what the original call yielded?
  The doc comment is emphatic that this is "a statement about the WORLD, not about the function:
  a perfectly deterministic function fails it if its inputs can change between the original run and
  the replay."

Look at how the shipped plugins actually declare themselves:

| Call | `Idempotent` | `SameValueOnReplay` | Consequence on replay |
|---|---|---|---|
| `llm.chat` | false | false | **Never re-invoked.** The recorded answer is returned. |
| `llm.embed` | true | true | May be re-invoked — near-deterministic for a fixed model and input |
| `pgvector.search` | true | false | Not re-invoked: the index is mutable, so a later insert would change the result set |
| `pgvector.upsert` | false | false | Not re-invoked — it is a write |

`llm.chat` is registered as a bare `FuncOptions{Name: "chat"}`
(`plugins/llm/host_functions.go:19`), which means both properties are false, which is exactly
right and is the single most important line in this playbook. **A replayed agent run does not
re-ask the model.** It reads back what the model said the first time. That is simultaneously the
cost control, the determinism guarantee, and the audit trail — one mechanism, three benefits.

Contrast what you would otherwise build: a cache keyed on the prompt, which is wrong whenever the
prompt contains a timestamp; or a "replay mode" flag threaded through your agent loop, which is
wrong the first time someone forgets it.

Two honest caveats, both already written down in the tree rather than by me:

- `llm.embed`'s registration carries its own warning (`plugins/llm/host_functions.go:29-35`): the
  "near" in near-deterministic is load-bearing, a hosted model can change behind a stable name, and
  re-invoking costs money — "a real cost, though not a workflow side effect. Both halves are
  asserted here rather than derived from the code."
- `pgvector.search` returning `SameValueOnReplay: false` means a long agent run that retrieves,
  then pauses for a day, then resumes, replays against **recorded** retrieval results rather than
  the index as it stands. That is correct for determinism and may be surprising to a product
  manager who expects the agent to "see" newly indexed documents. If you want fresh retrieval after
  a pause, the retrieval has to happen after the pause, in a later step.

---

## Per-tenant budgets

Three execution ceilings are settable per tenant, with a clamp rule that is the right way round:
the operator's flag is the maximum, and **a tenant may lower it and can never raise it**. A larger
value is clamped at execution time rather than rejected (`cmd/cleatctl/settenantsetting.go:205-216`,
schema from `migrations/postgres/039_tenant_settings.sql`):

    cleatctl --db <dsn> set-tenant-setting <tenant-uuid> \
      --wasm-instance-timeout-ms N \     # guest EXECUTION time ceiling
      --wasm-wall-clock-ceiling-ms N \   # WALL CLOCK ceiling for one invocation
      --host-retry-budget-ms N           # worst-case host retry backoff ceiling

Note the read-modify-write with an `updated_at` precondition: a concurrent change by another
operator is **refused and reported** rather than silently overwritten. That matters more than it
sounds when budget changes are being made by an automated system reacting to spend.

For request-rate limits rather than execution ceilings, `plugins/ratelimiter/` provides per-tenant
token buckets as edge middleware, backed by a config table reloaded on an interval.

**What is not here, and you will want it:** none of these is a *token* or *dollar* budget. They
bound time, not spend. A cost ceiling means recording token counts per call and enforcing against
an accumulated total — see [What you still have to build](#what-you-still-have-to-build-or-buy).

**Set the plugin to `db` mode, and know why.** `ratelimiter` has two modes (`plugin.go:52`). The
default is `memory`: in-process token buckets, so with N workers a tenant gets N times the
configured rate. Mode `db` is genuinely cluster-wide — `checkDBRateLimit`
(`plugins/ratelimiter/middleware.go:211-268`) keeps per-second buckets in a `rate_counter` table and
sums them over a sliding window, working across all three dialects.

For a spend-sensitive product the default is the wrong one. Set `mode: "db"` in the plugin config.

**You no longer have to check the startup log for this.** This paragraph used to say `Init`
*silently falls back to memory when no DB is available*, and told you to read the log line to find
out — which put the burden on an operator noticing a `Warn` among everything else a worker prints
at startup. Since cleat#1581 a `mode: "db"` that cannot be honoured **refuses to start**, naming
the reason, and so does an unrecognised mode: `"DB"`, `"database"` and every other near-miss used
to select per-process limiting, because the middleware asks `p.mode == "db"` and treats anything
else as memory.

The default is still `memory`, and a config that does not mention `mode` still gets it — the
refusal fires only where a deployment asked for something it was not getting.

Two properties of the DB path to design around: it **fails open** on a database error
(`middleware.go:186`), which is the right default for availability and the wrong one if the limiter
is your spend control; and the read-then-increment is not one atomic statement, so concurrent
workers can slightly overshoot a limit at its boundary. It is a cluster-wide limiter, not a
cluster-wide semaphore.

---

## Cost

**Where this wins.**

*You do not pay twice for a resumed run.* A conventional agent harness that loses its process
re-runs the agent from the top, re-paying for every model call already made. Cleat replays from
history and re-asks nothing. On a platform where a meaningful fraction of runs are interrupted by
deploys, scaling events or transient failures, this is the dominant cost term, and it is
structural rather than something you tune.

*One system instead of five.* The conventional assembly is an agent framework, a vector database,
a queue or task runner, a cache, and a separate audit store. Here it is Postgres plus workers, with
`pgvector` living in the same database as the workflow state — so retrieval and run state are one
backup, one failover, one connection budget.

*Idle agents cost nothing.* A run waiting on human approval is a database row, not a held process
or a parked container. Conventional agent frameworks that keep a process alive across an approval
pause pay for the wait.

**Where this loses, and you should concede it.**

*Token streaming to a browser: SHIPPED, with one limit worth knowing before you design around it.*
`GET /api/workflows/{id}/stream` serves a run's streamed tokens as server-sent events — see
[Streaming tokens to a client](../how-to/stream-tokens-to-a-client.md). A browser reconnecting
after a dropped connection is served exactly what it missed, from `event_history`, because every
chunk is persisted as it arrives.

**The limit: the live tail is worker-local.** A request landing on a worker that is not executing
the run gets the durable history and then nothing live, and says so (`"live": false`). Behind a
load balancer with N workers, expect that on a proportionate share of connections. Routing a
reader to the right worker is not solved — cleat#1639.

(This section said flatly that nothing bridged the two halves, and before that, that the worker had
no SSE support at all. The second came from a grep scoped to `cmd/` and `engine/`, which is where
the worker's own handlers live and not where plugin routes do. The first was true when written and
was the filing of cleat#1572, which then found the issue's own central premise wrong: partial
output *is* durable. Both corrected; the history is on the issue because the reasoning is more
instructive than the conclusion.)

*Every model call is written to the database.* That is the audit trail you wanted, and it is also
write volume proportional to agent activity, with prompt and response payloads in it. Budget for
it, set retention deliberately, and note that `--dead-letter-retention-days` defaults to 0 meaning
off.

**Measure rather than assume:** bytes of event history per average agent run; the fraction of runs
that are interrupted and resumed, which is what sizes the first win; and database growth per
thousand runs at your prompt sizes.

For the infrastructure half, `cleatctl cost` estimates monthly spend from workload parameters —
workflows per second, average duration, events per workflow, retention, replication, and provider
(`cmd/cleatctl/cost.go`). Read the note it prints about retention before trusting the storage line:
the `--retention-days` flag is an *assumption you supply*, explicitly "NOT what the engine does"
(cleat#1295). It estimates infrastructure only and knows nothing about model spend.

---

## Operations

The operational story is one deploy pipeline instead of three. An agent is a workflow version — a
row — promoted by routing rules and rolled back by a pointer flip. A prompt change, a model change
and a tool change ship as one artifact and roll back as one artifact.

The conventional equivalent is a container image for the agent service, a separate prompt store
with its own versioning, and a feature-flag vendor to canary between them — three systems whose
versions can disagree, and which cannot be rolled back atomically.

`featureflags.evaluate_flag` covers gradual model rollout inside a workflow. Note it is registered
as workflow-callable, so a flag evaluation is *recorded* — the run's history shows which model it
was routed to, which is exactly what you want when explaining a bad answer six weeks later.

**The operational cost you take on:** model provider credentials, quota and failover across
providers are yours. `plugins/llm/providers/` gives you the client surface, not the vendor
relationship.

---

## Observability

**The strong claim, and it is genuinely strong here.** Because durability requires recording every
external interaction for replay, the record *is* the audit trail. For an AI product that means, at
no extra cost and with no instrumentation code:

- every prompt sent and every answer received, per run, in order
- which retrieved documents were in context for a given answer
- which model version answered, and which flag routed it there
- where a human intervened, and what they approved

This is not a logging strategy you adopt; it is a by-product of the execution model. For any
product facing an AI-governance conversation — "show me what the model was asked and what it said,
for this decision, on this date" — the answer is a query rather than a project.

**The honest limits.** There is no token or cost metric emitted today, so spend dashboards are on
you. There is no eval harness, no prompt-regression tooling, no drift detection. The event history
tells you what happened; it does not tell you whether it was any good.

---

## What you still have to build or buy

1. **Token and cost accounting.** Record per-call token counts, accumulate per tenant, enforce a
   ceiling. The `tenant_settings` clamp pattern is the model to copy; the metric plumbing does not
   exist.
2. **Nothing — but configure it.** Fleet-wide rate limiting already exists as `ratelimiter` in
   `db` mode. What you have to do is turn it on and verify it took, since the default is `memory`
   and the fallback is silent. Listed here because an unconfigured default looks identical to a
   missing feature.
3. **Nothing, for token streaming — it shipped.** `GET /api/workflows/{id}/stream`, cleat#1572.
   What is still yours: routing a reader to the worker running the job, if `"live": false` on a
   share of connections is not acceptable to your front end. Tracked as cleat#1639.
4. **An eval and prompt-regression harness.** Entirely absent, and entirely your problem.
5. **The front-end.** As designed.

---

## Failure modes to design against

**A paused run resumes against stale retrieval.** By design — see the `pgvector.search` note
above. Retrieve after the pause, not before, if freshness matters.

**A run is interrupted mid-tool-call.** The tool call is a host function; whether it re-invokes is
governed by its two declared properties. If you register your own tool, get them right — an
idempotent *write* reads as safe and is still a live write issued during a reconstruction of a past
execution. That exact confusion is what `cleat#1318` was filed about.

**A tenant's ceiling is lower than a long agent run needs.** The clamp means the tenant's value
wins when it is lower. An agent that needs ninety seconds under a tenant-set thirty-second wall
clock fails, and it fails per-tenant, which is hard to reproduce centrally. Report the effective
ceiling in the run's error.

**Prompt payloads carry secrets into event history.** `engine.Redact` is applied on some paths
(the update payload path, `cmd/cleat-worker/server.go`). Whether it covers plugin host-function
arguments was **not verified** — check before putting user PII in a prompt.

---

## What was verified

**Read from the tree at `654d6f84`:** `FuncOptions` and its two properties
(`plugin/plugin.go:208-243`); the registration options for `llm.chat`, `llm.embed`,
`pgvector.search` and `pgvector.upsert`; the llm provider list; the streaming registry interface;
`cleatctl set-tenant-setting`'s three ceilings and its clamp and precondition semantics; the
plugin extension-point taxonomy; and every `text/event-stream` site in the tree, repo-wide rather
than under `cmd/` and `engine/` only — which is what corrected the streaming claim above.

**Asserted, not measured:** every cost claim. The "resumed runs do not re-pay" argument follows
from the replay semantics above, not from a measurement of a running system.

**Not verified:** whether `engine.Redact` reaches plugin host-function arguments. Listed as a
failure mode deliberately rather than presented as a property.
