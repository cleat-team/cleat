# Order lifecycle

A saga over a payment provider, with compensation you can watch happen.

Place an order and five steps run. Make a later step fail and the completed steps
unwind in reverse — and the order's published state says **which ones were
undone**, and separately **which undo failed**, so a page or an operator can tell
a clean rollback from one that left the customer charged.

This is the reference implementation behind
[`docs/playbooks/order-lifecycle.md`](../../docs/playbooks/order-lifecycle.md).

## It runs, and that is the point

**This is the only example in the tree that is executed end to end.**

Four other Go examples already show a saga as readable code —
`examples/fooddash/order.go`, `examples/subscription/billing.go`,
`examples/travel/booking.go` and `examples/saga-temporal-port/workflow.go`. All
four are **built** on every pull request by
`scripts/build-documented-examples.sh`, so a change that breaks their
compilation is caught. **None of the four is run.** Nothing deploys them, starts
a run, or looks at what a run published, so a change that compiles and then
misbehaves — a plugin call that no longer resolves, a compensation that stops
running, query state that stops being published — is found by a person noticing
or not at all.

`scripts/run-order-lifecycle-scenario.sh` deploys this workflow to a real
`cleat-worker` on a real database — **PostgreSQL, MySQL or SQL Server**, see
[Which dialects this runs on](#which-dialects-this-runs-on) — starts runs, and
asserts what each one published. It runs on every pull request, once per
dialect. If you change the saga, the engine's `Saga`, the worker's routes or the
query-state surface in a way that breaks this scenario, that job goes red.

That is the whole reason this directory exists next to `fooddash`. If you are
looking for the *clearest* reading of a saga, `fooddash` is probably it; this one
is here to be executed.

## The rope side is a placeholder

**Your PSP, your inventory system and your 3PL are not called.** Each rope-side
step is a recorded `DurableSleep` standing where the network round trip goes,
marked in `order.go` as the seam where your call belongs.

That is not laziness, and it is not the shape a real deployment should copy:

- A workflow reaches an external system through a durable call that resolves
  **by name to a plugin** (`engine/app.go`). This is one example directory, not a
  plugin set.
- The cleat SDK's generic outbound-HTTP call, `DurableCall("http", "fetch", …)`,
  is **embedder-only**. A stock `cleat-worker` cannot serve it and never could —
  `ABI.md` §2.48. `cmd/cleat/templates/fullstack` uses a placeholder for exactly
  this reason.

What *is* real, and is the part the playbook is about:

| Real | Where |
|---|---|
| Saga compensation, reverse order, declared per step | `cleat.NewSaga` / `AddStep` |
| An undone-step list and a failed-undo list, as query state | `SetQueryState` in `order.go` |
| A human approval signal above a threshold | `AwaitSignals` |
| A webhook wait — a genuine `await_webhook` against the bundled plugin | `awaitPaymentConfirmation` |
| Email delivery through the bundled `email-notify` plugin — **attempted**; see below | `notifyCustomer` |
| Idempotency on the start path | the worker's, not this file's |

### `email-notify` is bundled but disabled, and that shapes the workflow

The worker logs `plugin not configured, disabled` for `email-notify` on a stock
deployment. Enabling it needs `email_enabled` in `--plugin-config` **and** a
provider key — it builds SendGrid mail — so this example does not attempt to
make it work, and the notification step is **best-effort** as a result.

That is a real decision rather than a shortcut. A notification is the last thing
that happens to an order, not part of whether the order happened: a workflow that
makes it a saga step unwinds a charged, dispatched, settled order because an
email did not go out. Measured — this step was written to propagate, and the
scenario's second run failed at it with four completed steps and nothing else
wrong. When it fails the order stands and `notify_failed` is published.

The workflow calls two plugins. `await_webhook`, against `webhook-ingest`, is
genuinely exercised — it parks a run until a signed webhook arrives, and the
scenario delivers one (see the setup commands below). It is the one of the two
that a stock worker serves out of the box, which is why the other is the
best-effort one.

## Build

```bash
cleat build -o /tmp/out ./examples/order-lifecycle/
```

The artifact is named for the entry point, not the directory:
`place_order.wasm`. `cleat.yaml` lists entry points in snake_case
(`place_order`) while the Go function is `PlaceOrder` — the entry point is part
of the ABI. Getting the two out of step deploys cleanly and fails at *start*,
after the run id has been handed out.

## Run it against a real worker

The compose file brings up a database and a `cleat-worker`, and **the profile
picks which database**. PostgreSQL is the default and is what the rest of this
README assumes; the other two are supported and covered — see below.

The backend is not a service in the compose file — that is your application, and
you run it yourself.

```bash
# 1. The database and the worker. The worker prints an API key on first start.
export CLEAT_DB_URL="postgres://cleat:cleat@localhost:5432/cleat?sslmode=disable"
docker compose --profile postgres up -d
docker compose logs cleat-worker | grep -i 'Key:'

# 2. Deploy the compiled workflow.
cleat deploy --db "$CLEAT_DB_URL" --name order-lifecycle /tmp/out/place_order.wasm
```

### The deploy step is the one command that differs by dialect

**The `cleat` CLI talks to PostgreSQL only.** That is a deliberate limit and not a
gap in the engine: `cleat deploy` refuses a MySQL or SQL Server DSN with an error
that says so, and names the alternative. The engine and the worker both support
all three — it is the CLI that does not.

So on MySQL and SQL Server the deploy is a **different command**, not the same
command with a different DSN:

```bash
# MySQL or SQL Server: the CLI is PostgreSQL-only, so deploy with this instead.
export CLEAT_DIALECT=mysql          # or: mssql
go build -o /tmp/out/deploy-workflow ./cmd/deploy-workflow
/tmp/out/deploy-workflow --driver "$CLEAT_DIALECT" --db "$CLEAT_DB_URL" \
  order-lifecycle /tmp/out/place_order.wasm
```

`--driver` takes the dialect rather than a literal, so this one command covers
both non-PostgreSQL dialects; `CLEAT_DIALECT` is the same variable the compose
profile uses.

`deploy-workflow` is the only multi-dialect deploy path cleat has. It deploys and
nothing else — there is no multi-dialect `versions`, `rollback` or `plugin`
equivalent. **Everything after the deploy is dialect-independent**: starting runs,
the webhook wait, the published query state, the page. Only this one step knows
which database it is talking to beyond the DSN.

The scenario script asserts this both ways rather than routing around it: it runs
`cleat deploy` on PostgreSQL and `deploy-workflow` on the other two, **and it
asserts that `cleat deploy` is refused, with the dialect named, on the two where
it does not work.** That refusal is the reason the commands differ, so it is
checked rather than described.

### Which dialects this runs on

**All three: PostgreSQL, MySQL and SQL Server.** The scenario script takes the
dialect as its first argument, and CI runs one arm per dialect, so the
assertions below are asserted on each rather than only on the default:

```bash
scripts/run-order-lifecycle-scenario.sh postgres   # the default
scripts/run-order-lifecycle-scenario.sh mysql
scripts/run-order-lifecycle-scenario.sh mssql
```

The profile selects the database; the environment selects the DSN. These are the
**in-network** hosts, because the worker reaches the database over the compose
network — the `CLEAT_DB_URL` above is the same database seen from your host.

```bash
# MySQL
export CLEAT_MIGRATE_DB_URL='root:cleat@tcp(mysql:3306)/cleat?tls=false&parseTime=true'
export CLEAT_WORKER_DB_URL="$CLEAT_MIGRATE_DB_URL"
docker compose --profile mysql up -d
export CLEAT_DB_URL='root:cleat@tcp(localhost:3306)/cleat?tls=false&parseTime=true'

# SQL Server (single-quoted: the password ends in `!`, which zsh expands)
export CLEAT_MIGRATE_DB_URL='sqlserver://sa:CleatTest123!@mssql:1433?database=cleat'
export CLEAT_WORKER_DB_URL="$CLEAT_MIGRATE_DB_URL"
docker compose --profile mssql up -d
export CLEAT_DB_URL='sqlserver://sa:CleatTest123!@localhost:1433?database=cleat'
```

#### What actually differs, and what does not

Less than it looks, and the differences are the interesting part:

| | PostgreSQL | MySQL | SQL Server |
|---|---|---|---|
| DSN shape | `postgres://user:pw@host:5432/db` | **no scheme**: `user:pw@tcp(host:3306)/db` | `sqlserver://user:pw@host:1433?database=db` |
| worker `--driver` | `postgres` (the default) | `mysql` | `mssql` |
| who connects at runtime | `cleat_app`, a lesser role | the same user as the migration | the same user as the migration |
| the database itself is created by | `POSTGRES_DB` at first boot | the store factory, which issues `CREATE DATABASE IF NOT EXISTS` | a `mssql-db` step, because cleat does not create it |
| row-level security on the runtime connection | enforced, and the worker **verifies** it | **none — MySQL is single-tenant by construction** | **none asserted; no check runs** |

**The worker does not infer the dialect from the DSN.** `--driver` defaults to
`postgres`, so a MySQL DSN handed to a worker without `--driver=mysql` is a
connection error rather than a dialect switch. The compose file passes it for
you; a hand-written `cleat-worker` invocation has to.

**The saga assertions are identical on all three, and that is the claim.** Every
check the scenario makes reads either a run's `error` or its published query
state, and none of them is relaxed for a dialect. An arm that passed because it
asserted less would be worse than no arm.

**There is exactly one place the arms deliberately differ, and it is RLS.** On
PostgreSQL the scenario asserts the worker reports that its runtime connection
is subject to row-level security — a real guarantee, and the reason the
`app-role` step exists. On MySQL and SQL Server there is no such guarantee to
assert: `cmd/cleat-worker/main.go` gates the whole enforcement check on
`--driver == "postgres"`, before calling `engine.CheckRLSEnforced`, which
queries `pg_roles` and could not run elsewhere. So the arm **states the absence**
rather than asserting the weaker thing and calling it the same check. On MySQL
that absence is by design — cleat's MySQL is single-tenant by construction — and
it is why `docker-compose.yml` puts the `app-role` step under the `postgres`
profile instead of documenting it as not-for-MySQL.

Then start a run. `simulate_payment_failure` is the interesting one: the charge
is declined, and the reservation that already completed is released.

```bash
curl -fsS -X POST http://localhost:8080/api/workflows/order-lifecycle/start \
  -H "Authorization: Bearer $CLEAT_API_KEY" -H "Content-Type: application/json" \
  -d '{"input":{"order_id":"ord-1","email":"buyer@example.com",
       "items":[{"sku":"widget","quantity":1,"price_cents":2500}],
       "simulate_payment_failure":true}}'
```

Read what it did — this is the payoff, and it needs no event history:

```bash
curl -fsS "http://localhost:8080/api/workflows/$RUN_ID/query?key=compensated" \
  -H "Authorization: Bearer $CLEAT_API_KEY"
# reserve_inventory
curl -fsS "http://localhost:8080/api/workflows/$RUN_ID/query?key=unwind_failed" \
  -H "Authorization: Bearer $CLEAT_API_KEY"
# (empty — the release succeeded)
```

### The webhook step needs a source, and this is the awkward part

`await_payment_confirmation` calls the bundled `webhook-ingest` plugin, and that
plugin will not wait on a source that does not exist. The run fails at that step
with `no webhook source configured` if you start it with an empty `source_id`.

Creating one is a two-command setup, done **once per deployment** rather than per
order, and it is the least polished path in this example:

```bash
# 2a. Create the source. A signing secret is required -- an unsigned source
#     cannot be created at all (cleat#1992/#2172, owner decision).
#
#     correlation_key_field names the top-level JSON field this PSP's webhook
#     body carries the order id in -- "order_id" below, matching 2b's BODY.
#     One source is shared by every order; this is what lets the workflow
#     wait for THIS order's confirmation specifically rather than whichever
#     one arrives next (cleat#2649). Omit it and await_payment_confirmation
#     can still be told to wait on this source alone, but two orders in
#     flight at once could not be told apart.
curl -fsS -X POST http://localhost:8080/ingest/sources \
  -H "Authorization: Bearer $CLEAT_API_KEY" -H "Content-Type: application/json" \
  -d '{"name":"psp","source_type":"payment","secret":"whsec_local_dev","correlation_key_field":"order_id"}'
# -> {"id":"<SOURCE_ID>", ...}

# 2b. Deliver the PSP's confirmation. The signature is GitHub-style: HMAC-SHA256
#     of the raw body, hex, in X-Hub-Signature-256. order_id must match the
#     order_id this run was started with (1a) -- that is the correlation.
BODY='{"event_type":"payment.succeeded","order_id":"ord-1"}'
SIG="sha256=$(printf '%s' "$BODY" | openssl dgst -sha256 -hmac 'whsec_local_dev' -r | cut -d' ' -f1)"
curl -fsS -X POST "http://localhost:8080/ingest/$SOURCE_ID" \
  -H "Content-Type: application/json" -H "X-Hub-Signature-256: $SIG" -d "$BODY"
```

`POST /ingest/{source_id}` is deliberately **auth-exempt** — the caller is the
payment provider and holds no cleat credential. The signature is what replaces
the key, so the secret is the security boundary.

Pass the id as `source_id` on the run's input and the workflow parks on the
webhook instead of failing -- a real suspend now, not a poll: `source_id` is
now **required** (cleat#2649, a breaking change from earlier versions of this
example -- see CHANGELOG.md), and the wait is woken directly by 2b's POST
rather than checking back on a timer.

## The web page

```bash
CLEAT_API_KEY=$CLEAT_API_KEY go run ./backend
# http://localhost:9090
```

The page exists to show the compensation outcome, not just a status: when a run
compensates it names the steps that were undone, and it gives a failed undo its
own treatment — *"Compensated, but not completely"* — because a refund that did
not go through leaves the customer charged on an order that did not complete,
which needs a person rather than a retry.

`backend/main.go` holds **no business logic**, deliberately. It starts runs,
reads their published state, and forwards one signal. The API key is attached by
an HTTP transport rather than at each call site, so no handler can forget it and
anything the browser sent is stripped.

To see each outcome, tick the corresponding box on the form:

| Tick | What happens |
|---|---|
| *(nothing)* | Completes. Nothing is undone. |
| **Decline the charge** | The first spending step fails; the reservation unwinds. |
| **Fail the dispatch** | Fails *after* the charge succeeded, so the refund is a real unwind. |
| **Fail the compensation too** | The unwind runs and fails — the state the saga exists to avoid. |
| Above the approval threshold | Parks on a signal; nothing is spent until you approve. |

## Files

- `order.go` — the workflow: the saga, its compensations, and the query state
- `order_test.go` — unit tests, driven through `cleattest`
- `backend/` — the API and page server. No pipeline logic
- `web/` — the page. Served under `default-src 'none'`, so no inline script or style
- `docker-compose.yml` — PostgreSQL and a worker
- `cleat.yaml` — the workflow's name and entry points

## Tests

```bash
cd examples && go test ./order-lifecycle/... -count=1
```
