# Playbook 1 — AI agent platform with per-tenant budgets

**Status:** engineering reference. Drafted 2026-09-14 against `develop` at `654d6f84`; corrected
2026-09-25 against `develop` at `656aced4` (cleat#2053) — see
[What was verified](#what-was-verified) at the end for what changed. Nothing here has been built end
to end.

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

## Three things this page was missing, and two of them were the load-bearing ones

**The agent loop itself is still hand-rolled, and you should not write your own.** As of 2026-09-25
there is no reusable loop: `cleat/ai/agent` exists and **has no importer at all**, and the only
working versions are two hand-copies inside `cleat init` templates
(`cmd/cleat/templates/agent/workflow.go`, `templates/agent-python/agent.py`) — separate from each
other, and untested as loops. cleat#1983 replaces all three with one reusable agent workflow any SDK
starts as a child. **Until it lands: copy a template, and expect to delete it.** Do not build a
product on a loop you are writing yourself — the loop is the part the engine is supposed to own, and
it is the part that is not there yet.

**Model keys are per-tenant now, which this page would have told you was impossible.** It listed
per-tenant model keys under what you still have to build, because the `llm` request had no `api_key`
field — so a `${secret:…}` was resolved and discarded, and **every tenant spent the operator's key**.
That is fixed (cleat#1988): a request's `APIKey` wins over the configured default, and the key is a
*deployment secret* (`llm.providers.<provider>.api_key`) so it can be rotated without a restart
(cleat#1992). For a page whose whole premise is per-tenant budgets, this was the most load-bearing
wrong assumption on it. Check the shape in `plugins/llm/plugin.go` before designing key distribution.

**The field is secret-only, which closes the other half of the same hole.** `api_key` is declared in
`SecretOnlyFields`, so a raw literal in a request is **refused** rather than written to
`event_history` (cleat#2043) — a tenant cannot put its key in the replay log even by trying. Worth
knowing because it is the shape you want for any BYOK field you add: the field is checked, not the
caller's intentions.

**Once #1983 lands, a tenant's own workflow can be an agent TOOL** — a customer supplies the step,
isolated by WASM and scoped by `tenant_id`, exactly as `POST /api/definitions` already allows for a
plain workflow. That is this page's positioning wedge, and it is a **target rather than a feature**:
the tool kind is not in the tree yet. Worth designing toward, not planning around.

---

## The part worth reading twice: replay and paid calls

This is where an agent platform built on a generic queue goes wrong, and where cleat's design has
already done the thinking.

A host function declares **two independent properties** (`FuncOptions`, `plugin/plugin.go`), and the
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

`llm.chat` is registered with neither property set (`plugins/llm/host_functions.go`), which means
both are false — which is exactly right, and is the single most important line in this playbook. **A replayed agent run does not
re-ask the model.** It reads back what the model said the first time. That is simultaneously the
cost control, the determinism guarantee, and the audit trail — one mechanism, three benefits.

Contrast what you would otherwise build: a cache keyed on the prompt, which is wrong whenever the
prompt contains a timestamp; or a "replay mode" flag threaded through your agent loop, which is
wrong the first time someone forgets it.

Two honest caveats, both already written down in the tree rather than by me:

- `llm.embed`'s registration carries its own warning (`plugins/llm/host_functions.go`): the
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
value is clamped at execution time rather than rejected (`settenantsetting.go`,
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

**Set the plugin to `db` mode, and know why.** `ratelimiter` has two modes (`plugins/ratelimiter/plugin.go`). The
default is `memory`: in-process token buckets, so with N workers a tenant gets N times the
configured rate. Mode `db` is genuinely cluster-wide — `checkDBRateLimit`
(`checkDBRateLimit`, `plugins/ratelimiter/middleware.go`) keeps per-second buckets in a `rate_counter` table and
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
(the fail-open branch, `plugins/ratelimiter/middleware.go`), which is the right default for availability and the wrong one if the limiter
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

**Any worker can serve the stream.** The live tail is in memory on the worker executing the run,
so a request landing anywhere else follows the run from `event_history` instead, reading from its
cursor on an interval — which every worker can do, because every worker can read the table. The
`attached` event reports which it got as `transport: "live"` or `"poll"`. Both carry every token;
they differ in latency. cleat#1639.

So a load balancer needs no stickiness for this endpoint, and behind one you should expect
`"live": false` on a share of connections proportional to your worker count **without** those
connections being degraded in content.

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
   `db` mode, and the default is `memory`. Listed here because an unconfigured default looks
   identical to a missing feature. **You no longer have to verify it took by reading a log line**:
   since cleat#1581 a `mode: "db"` that cannot be honoured refuses to start, so a silent downgrade is
   no longer possible — see the note in *Per-tenant budgets*, which this list used to contradict.
3. **Nothing, for token streaming — it shipped.** `GET /api/workflows/{id}/stream`, cleat#1572,
   and it works on every worker as of cleat#1639, so no load-balancer stickiness is yours to
   arrange. What is still yours is a sizing decision: `--stream-poll-interval` is latency your
   users see and `--max-stream-poll-readers` is load your database takes.
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

**Prompt payloads carry secrets into event history.** `engine.Redact` **is** applied to plugin call
payloads before they are persisted — `cmd/cleat-worker/setup.go` redacts `newEvents[i].Request` and
`.Response` on the plugin-event paths, which is exactly where a prompt and its answer land. This page
used to record that as unverified; it is checked now, and the answer is the good one.

What remains is that `Redact` is a **field-name heuristic**, not a content classifier: a secret in a
field it does not recognise still goes to the database. The stronger guarantee is elsewhere —
`${secret:…}` substitution happens *after* the recorder, so the resolved value is never in the event
at all. Put credentials in secrets, not in prompts.

---

## What was verified

**Read from the tree at `654d6f84`:** `FuncOptions` and its two properties
(`FuncOptions`, `plugin/plugin.go`); the registration options for `llm.chat`, `llm.embed`,
`pgvector.search` and `pgvector.upsert`; the llm provider list; the streaming registry interface;
`cleatctl set-tenant-setting`'s three ceilings and its clamp and precondition semantics; the
plugin extension-point taxonomy; and every `text/event-stream` site in the tree, repo-wide rather
than under `cmd/` and `engine/` only — which is what corrected the streaming claim above.

**Asserted, not measured:** every cost claim. The "resumed runs do not re-pay" argument follows
from the replay semantics above, not from a measurement of a running system.

**Resolved since drafting:** whether `engine.Redact` reaches plugin host-function arguments. It does
— the plugin-event paths redact `Request` and `Response` before persisting, in
`cmd/cleat-worker/setup.go`. See the prompt-payload failure mode above for what that covers and what
it does not.

**Corrected since drafting (2026-09-25, cleat#2053).** Three things, and two of them were the page's
load-bearing claims:

- **Per-tenant model keys shipped** (cleat#1988, with rotation via cleat#1992). The page's own
  premise is per-tenant budgets and it had no idea the key was shared — a `${secret:…}` was resolved
  and discarded, so every tenant spent the operator's key. Now documented as shipped, in the section
  above.
- **The agent loop is still hand-rolled and the section saying so did not exist.** `cleat/ai/agent`
  has no importer; two `cleat init` templates carry separate untested copies; cleat#1983 replaces
  them. Builders were being told to build a product on a loop the engine does not yet own.
- **`engine.Redact` on plugin payloads was checked**, so that "not verified" is discharged.

Line citations were replaced with symbol names throughout — `FuncOptions` was cited at
`plugin/plugin.go:208-243` and is at `:623`.
