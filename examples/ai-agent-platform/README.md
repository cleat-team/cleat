# AI agent platform

A durable agent loop with per-tenant spend bounded: a task kicks off an agent
that calls a model, calls tools, waits for a human, and stops at a ceiling —
and a run that crashes mid-loop resumes **without re-asking the model**.

This is the reference implementation behind
[`docs/playbooks/ai-agent-platform.md`](../../docs/playbooks/ai-agent-platform.md).

## What this exists to show: a replayed run does not re-ask the model

Everything else here is arrangement. The claim is one line of the playbook, and
it is the reason an agent platform is a durability problem rather than a
caching problem:

> **A replayed agent run does not re-ask the model.** It reads back what the
> model said the first time.

`llm.chat` is registered with **neither** `Idempotent` nor `SameValueOnReplay`
(`plugins/llm/host_functions.go`), which means both are false — so the engine
returns the recorded answer instead of invoking again. That single fact is
simultaneously the cost control, the determinism guarantee and the audit trail.

**So the scenario SIGKILLs the worker mid-loop and counts the model's
requests.** The provider is a stub that counts what it receives; after the
kill the resumed run must leave that count exactly where it was. A run that
re-asked would double it. See `scripts/run-ai-agent-platform-scenario.sh`.

A green run that never crashes does not test any of this. The assertion is the
crash-resume count, not the completion.

## The four hitch points

`docs/playbooks/ai-agent-platform.md` lists the components in "The assembly".
Each is a different extension point, and a scenario that exercised three of
them would not show that they compose:

| hitch point | what it is here | where |
|---|---|---|
| **Host functions** | the model call (`llm.chat`), the artifact write (`blobstore.put`), the approval wait (`event-triggers.await_event`) | `agent.go` |
| **HTTP routes** | `POST /api/events/publish` — where a human's approval arrives | `event-triggers` |
| **Edge middleware** | the per-tenant limit on new runs, whose 429 carries `X-RateLimit-Limit` | `ratelimiter` |
| **Background loop** | `ratelimiter`'s reload, which reads every tenant's limits with `plugin.AcrossAllTenants` by name | `plugins/ratelimiter/background.go` |

**The background loop is not decoration for the middleware row, and the two are
asserted together for that reason.** `ratelimiter` builds its token buckets
from rows in its own table, refreshed by a loop running `AcrossAllTenants`. A
deployment that never writes a row has a registered middleware that **cannot
fire**. So "the limit fired" is evidence for both rows at once, and a scenario
that only seeded a limit and stopped would be asserting the seed.

**The plugin is configured `mode: "db"`, and the default is the wrong one for
this product.** In `memory` mode each worker keeps its own buckets, so with N
workers a tenant gets N times the configured rate. That is a per-process
limiter, and for anything counting spend it is the wrong shape. The compose sets
it; see `plugin-config.json`.

## The rope end is three things, and they are stubbed differently

**Your model provider, your retrieval store and your eval harness are not
here.** What the scenario owes you is the shape of each seam.

**The model provider is a local HTTP stub — a real durable call with a fake
answer.** The call is a genuine host call to the bundled `llm` plugin,
configured with `base_url` at the stub. Replacing it with OpenAI, Anthropic or a
self-hosted vLLM is a line in `plugin-config.json`, not a rewrite. This is the
same shape as `examples/integration-hub`'s `sink`, and for the same reason.

**The retrieval store is canned documents inside `agent.go`**, and it is worth
knowing why it is not a local HTTP stub like the model:
**`plugins/pgvector` is not linked into `cleat-worker`**. It exists, with
`search`/`upsert`/`delete`, and it is deliberately absent from the worker's
blank-import block (`cmd/cleat-worker/main.go`) — its `Migrations()` creates an
`embedding vector(1536)` column, and `plugin.RunMigrations` is **FATAL at
boot**, so linking it would stop the worker starting on any PostgreSQL without
the extension. The comment there says the repair is to make that migration
degrade, and that it is a change to the plugin rather than to the list.

> **The playbook's assembly table lists `pgvector` as the retrieval row with no
> such warning**, and discusses `pgvector.search`'s replay properties at
> length. A reader who follows that table will look for a plugin the shipped
> worker does not contain. Reported; the table is the thing that should change,
> not this example.

So the retrieval seam is a function. Replacing its body is the whole job, and
the playbook's "a paused run resumes against stale retrieval" applies the
moment it becomes a fetch.

**The eval harness does not exist in cleat and is not faked here.** The event
history tells you what happened; it does not tell you whether it was any good.

## The model key is per-tenant, and this example says so on purpose

The playbook lists "per-tenant model keys" among what you still have to build.
**That is out of date** — cleat#1988 fixed it, and cleat#1992 moved the key to a
deployment secret so it rotates without a worker restart:

- a request's `api_key` **wins over** the configured default, and an unresolved
  `${secret:…}` is refused rather than sent literally;
- `api_key` is declared in `SecretOnlyFields`, so a raw literal is **refused
  rather than written to `event_history`** (cleat#2043) — a tenant cannot put
  its key in the replay log even by trying.

**This example does not send `api_key` at all, and that is a decision rather
than an omission.** The stub needs no key, so sending one would demonstrate
nothing; what matters is *where* a key belongs when you have one. It belongs in
the plugin's provider config as a deployment secret, not in the request:

- a key in a request body is a key in the **event history**, which is the
  artifact this whole architecture exists to keep reproducible;
- a per-call key cannot be rotated by an operator without a deploy, which is
  the opposite of what a secrets rotation is for.

Set it with `cleatctl set-deployment-secret --name llm.providers.openai.api_key`
and every tenant's runs pick it up on their next call. That is the answer the
playbook's own premise — per-tenant budgets — requires, and it is why this
README states it rather than leaving a reader to infer it from an absent field.

## The spend ceiling, and what is honest about it

`AgentInput.BudgetUSD` is **per run**. The loop checks it before each model call
and stops with `budget_exceeded`, rather than finishing and reporting what it
spent. The scenario asserts the *number of model calls* a budgeted run makes,
not only its status — a loop that reported the ceiling without bounding the
loop would produce the same status having spent more.

**A cumulative per-tenant ceiling is not demonstrated here, and cannot be
today.** It needs an accumulator that survives the run, and cleat emits no token
or cost metric — it is item 1 of the playbook's "What you still have to build
or buy". What this file demonstrates is the **enforcement point**: the check
between the model's tool request and the next paid call. That half is the one
with a wrong answer you cannot correct after the fact.

> **`Cost` is not a measurement of what you were charged.** Every provider
> computes it from a hard-coded per-model table, and in both `openai` and
> `anthropic` the `default:` branch — anything not recognised **by exact name** —
> is the **mid-range model's** price: `gpt-4o` in one, `claude-sonnet-4-6` in the
> other (`plugins/llm/providers/{openai,anthropic}.go`).
>
> **A mid-range default is wrong in both directions, and the flattering one is
> the dangerous one:**
>
> | the model asked for | the default prices it | a ceiling built on `Cost` |
> |---|---|---|
> | something dearer than the default | **too cheap** | **permits several times the intended spend** |
> | something cheaper, or free | **too dear** | fires earlier than it should |
> | a self-hosted vLLM behind `base_url` | at gpt-4o's rates | as though every call went to a hosted model |
>
> **The under-pricing direction is fail-open and silent** — the caller cannot
> tell "priced" from "guessed", and the guess is a real model's real price, so it
> looks plausible on a dashboard. This is the number a spend ceiling is enforced
> against.
>
> **And the price follows the model that was ASKED FOR, not the one that
> answered:** the cost switch reads `input.Model` while the returned `Model`
> field is the response's own. A provider that quietly routes elsewhere is still
> billed at the requested rate.
>
> Found while writing this example, and the way it was found is the part worth
> carrying: **the first version of the scenario's budget test picked a dollar
> figure by hand and passed.** It was measuring the price table, not the ceiling —
> a constant chosen by hand had done the work the thing under test was supposed
> to do. It now reads the spend the run reports, derives the budget from it, and
> asserts the **count of model calls** as well as the status, which takes the
> price table out of the assertion entirely.

**And the three execution ceilings that DO exist are per-tenant, and are time
rather than money** — `--wasm-instance-timeout-ms`, `--wasm-wall-clock-ceiling-ms`
and `--host-retry-budget-ms`, settable with `cleatctl set-tenant-setting`, with a
tenant allowed to lower the operator's value and never raise it. A long agent
run under a tenant-set wall clock fails per-tenant, which is hard to reproduce
centrally: report the effective ceiling in the run's error.

## The agent loop is a copy, and you should not write one

**Do not copy `RunAgent`'s loop out of this file.** It is the last hand-written
copy of it left in this repository, and it is here because this example was
written before there was an alternative — not because the loop belongs in a
product.

As of 2026-09-28 there was **no reusable loop**: `cleat/ai/agent` existed and
had no importer at all, and the working versions were two hand-copies inside
`cleat init` templates (`cmd/cleat/templates/agent/workflow.go`,
`templates/agent-python/agent.py`) — separate from each other, and untested as
loops.

**cleat#1983 replaced all three with one reusable agent workflow that any SDK
starts as a child**, and both templates are clients of it now. What that buys,
and what a hand-written loop cannot: every LLM turn and every tool call is a
durable step, so an agent survives a crash mid-conversation and resumes without
asking the model again for turns it already completed. See "Agent Workflows" in
[`docs/reference/sdk-api.md`](../../docs/reference/sdk-api.md).

For a new product, start the shipped workflow — `agentworkflow.RunAsChild` in
Go, `cleat_sdk.agent.run_agent` in Python.

**This example has not been migrated onto it, and that is tracked rather than
intended as an example to follow**: cleat#2980. Until it lands, read this file
for the deployment shape (`cleat.yaml`, the backend, the web front end) and take
the agent itself from the workflow.

## Build

```bash
cleat build -o /tmp/out ./examples/ai-agent-platform/
```

The artifact is named for `cleat.yaml`'s own `name:` field: `ai-agent-platform.wasm`
(cleat#2692). `cleat.yaml` also lists entry points in snake_case while the Go
function is `RunAgent` — that pairing, a separate thing from the filename, is
part of the ABI.

## Run it

```bash
docker compose up -d
docker compose logs cleat-worker | grep -i 'Key:'
cleat deploy --db "postgres://cleat:cleat@localhost:5432/cleat?sslmode=disable" \
  --name ai-agent-platform /tmp/out/ai-agent-platform.wasm
```

The stack includes `model-stub`, which stands in for your provider. It answers
`GET /` with the number of model requests it has served — a count the scenario
reads, and the one to watch if you want to see the durability property for
yourself: **start two runs and watch it climb, then kill the worker mid-run and
watch it not.**

### Granting the model host, which takes TWO permissions

**The compose is not enough on its own, and the error you get without the second
one names the wrong thing.** After deploying, the tenant must be allowed to reach
the model host:

```bash
cleatctl --db "postgres://cleat:cleat@localhost:5432/cleat?sslmode=disable" \
  egress-allow add "$CLEAT_TENANT_ID" model-stub
```

Without it every run fails at step one with:

```
provider error: openai: request failed: Post http://model-stub:9100/v1/chat/completions:
egress to model-stub is refused by cleat's network policy:
host is not on this tenant's egress allowlist
```

**Egress needs BOTH permissions** — the operator permits a destination and the
requesting tenant permits it, and either saying no is a refusal. The compose
supplies the operator's half in two flags with different scopes:

| flag | what it is | who it covers |
|---|---|---|
| `--egress-allowlist` | the DEPLOYMENT's list | guest fetches, plugin calls, and sweeps |
| `--plugin-egress-allow-private` | this deployment's PRIVATE-address exceptions, plugin-only | plugin egress only |

This example sets only the second, because the call it needs to permit is made
by a **plugin** (`plugins/llm`) to a **private** host — which is exactly that
flag's case (`cmd/cleat-worker/config.go` names a self-hosted model server as its
motivating example). The tenant half has no compose flag: it is a row, written
with `cleatctl egress-allow`, because it is a grant to a customer rather than a
property of the deployment.

> **Why this scenario needs the tenant grant and `examples/integration-hub` does
> not, given both permit one private host by name.** That scenario's delivery is
> made by a plugin's **background loop**, which runs with no tenant in context —
> and a tenant-less call answers to the operator list alone, deliberately, since
> a sweep has no customer to scope by (`cmd/cleat-worker/plugin_egress.go`). The
> model call here is a **host function inside a run**, so it has a tenant, so the
> tenant's allowlist applies. Same flag, same host class, different answer —
> because the two calls are made by different shapes of code.

### Starting a run

```bash
curl -fsS -X POST http://localhost:8080/api/workflows/ai-agent-platform/start \
  -H "Authorization: Bearer $CLEAT_API_KEY" -H "Content-Type: application/json" \
  -d '{"tenant_id":"00000000-0000-0000-0000-000000000001",
       "task":"Summarise yesterday'\''s incidents.",
       "max_steps":6,"budget_usd":1.00,
       "artifact_key":"reports/nightly.md"}'
# -> {"workflow_id":"<RUN_ID>", ...}
```

The run's **live state is query state**, which is how you watch a run that may
wait for hours without reading event history:

```bash
curl -fsS "http://localhost:8080/api/workflows/$RUN_ID" \
  -H "Authorization: Bearer $CLEAT_API_KEY" | python3 -m json.tool
```

The keys to read are `status` (`running`, `thinking`, `tool_call`,
`awaiting_approval`, `approval_received`, `approval_timeout`, `done`,
`failed`, `budget_exceeded`), `steps`, `spent_usd`, `budget_usd` and
`total_tokens`.

> **There is no `completed` state.** A run that finished reads `done`. This is
> worth stating because it was got wrong while writing this example — a wait
> loop looking for `completed` reported a perfectly successful run as never
> finishing, and the message it printed contained both words.

### Approving a step

A human's decision arrives as an event published to a plugin-registered route,
which is the HTTP-routes hitch point:

```bash
curl -fsS -X POST http://localhost:8080/api/events/publish \
  -H "Authorization: Bearer $CLEAT_API_KEY" -H "Content-Type: application/json" \
  -d "{\"id\":\"$(python3 -c 'import uuid;print(uuid.uuid4())')\",
       \"event_type\":\"agent.approval\",
       \"data\":{\"approved\":true,\"note\":\"ship it\"}}"
```

The `id` is **required and must be a UUID** — the route rejects the request
without one. The run polls for this event on an interval, so the approval does
not have to arrive before the run starts waiting.

> **An approval event is not addressed to a run, and that is a real limit of
> this design.** `await_event` matches on `(tenant_id, event_type)` — the
> `run_id` in the payload is an audit trail, not an address — so **if one tenant
> has two runs parked at once, a single decision can be taken by either.** The
> backend narrows the window by refusing to publish unless the run it was asked
> about reads `awaiting_approval`, but that is a check at the publish end and it
> cannot make the event addressed. Anything where a wrong approval matters needs
> a per-run event type (`agent.approval.<run_id>`) or a signal, which *is*
> addressed to a workflow. Stated here rather than left for you to discover by
> approving the wrong run.

### The rate limit

```bash
curl -fsS -X PUT http://localhost:8080/rate-limits/edge \
  -H "Authorization: Bearer $CLEAT_API_KEY" -H "Content-Type: application/json" \
  -d '{"max_requests":5,"window_seconds":60}'
```

**Nothing fires until a limit exists.** The plugin's middleware builds its
buckets from rows in its own table, so a deployment that never writes one has a
registered middleware that cannot fire — and the background loop is what carries
them across.

**Two limiters run, and their 429s are told apart by a header.** The core
limiter (`--rate-limit`, per IP) is applied outside auth; the plugin's is inside
the middleware chain. **Both answer 429 with the body
`{"error":"rate limit exceeded"}`** — so the body discriminates nothing. Only
the plugin's carries `X-RateLimit-Limit`.

## The app

The commands above are the whole scenario, and they are what CI runs. There is
also a small web app for a reader who would rather click than type:

```bash
cd examples/ai-agent-platform
CLEAT_URL=http://localhost:8080 \
CLEAT_API_KEY="$CLEAT_API_KEY" \
  go run ./backend
# -> http://localhost:9090
```

Run it from this directory: `-web` defaults to `web`, which is this example's
own, and `go run ./backend` finds the `examples` module by walking up.

It is a `backendkit` proxy and holds no business logic — the agent IS the
workflow, and a backend that duplicated the loop would be demonstrating the
thing cleat exists to remove. It offers the run list with **spend against
budget**, the live query state, and the approve/deny buttons, which is what a
human-approval step needs to be usable at all.

## Tests

```bash
cd examples && go test ./ai-agent-platform/... -count=1
```

**One branch of this workflow has no unit test, and it is filed rather than
hidden — cleat#2522.** The approval wait polls, and the branch where the first
poll misses and a later one hits is the branch a real deployment takes most
often. It cannot be reached from `cleattest`: `OnPluginCall(...).Return(...)`
registers a single answer, and `pluginCallImpl` scans the stubs **in order** with
the first match winning — so registering a second `Return` for the same
plugin+function is *unreachable rather than sequenced*, and it is not an error.
A test written that way passes against the first-poll path while reading as
though it exercised the second.

## Files

- `agent.go` — the workflow: the loop, the ceiling, the tools and the human wait
- `agent_test.go` — unit tests, driven through `cleattest`
- `backend/main.go` — the app: a `backendkit` proxy over the run list and the
  approval route
- `web/` — the page `backend/` serves: runs, spend against budget, approvals
- `docker-compose.yml` — PostgreSQL, a worker, `model-stub` standing in for your
  provider, and the one-shot service that writes the plugin config. The config
  holds the `llm` provider's `base_url` at the stub and `ratelimiter`'s
  `mode: "db"` **at the same top level**, because every plugin's `Init` receives
  the same raw `--plugin-config` bytes and there is no per-plugin section — the
  top level is a namespace shared by accident, which is why the two plugins' key
  sets must not collide
- `cleat.yaml` — the workflow's name and entry points

> **The plugin config is written into a named volume rather than bind-mounted
> from this directory, and what that avoids is worth knowing if you copy the
> file.**
>
> **Measured, on one runtime:** under colima a host bind mount did not deliver
> this machine's files at all. A single-**file** target was created as a
> *directory*, and the worker exited with
> `read /etc/cleat/plugin-config.json: is a directory` — **naming the config
> rather than the mount**, which sends you to check JSON that is valid. The same
> happened binding from `/tmp`, so it was not this directory or its path.
>
> **Not measured:** whether some runtime fails this way for single-file binds
> specifically while binding directories correctly. Only colima was running here;
> Docker Desktop and OrbStack were both stopped, so the comparison was not
> available.
>
> **The claim is therefore only this:** on the runtime it was measured against,
> a host bind mount was not usable, and it failed in a way that points at the
> wrong thing. A named volume worked there and needs no host path, which is why
> it is what this file ships — **that is a preference for the shape that cannot
> fail this way, not a claim that every other runtime is broken.**
