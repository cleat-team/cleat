# Integration hub

Per-tenant connector dispatch: an inbound event from a customer's system is
delivered durably to that customer's connector, with the retry and dead-letter
path underneath it.

This is the reference implementation behind
[`docs/playbooks/integration-hub.md`](../../docs/playbooks/integration-hub.md).

## The rope end is one URL, and that is a smaller stub than it sounds

**Your customer's CRM, warehouse or ERP is not called.** What the scenario does
is register a local HTTP sink as a webhook and point the connector at it — so
the dispatch is a **genuine durable call to a bundled plugin**, and the only
stub is the thing at the far end of it.

**This is the difference worth comparing against
[`examples/order-lifecycle`](../order-lifecycle/), and it is in this scenario's
favour.** That one needed three `DurableSleep` placeholders, because a PSP, an
inventory system and a 3PL have no bundled plugin and a durable call that
resolves nowhere cannot serve a stock worker (`ABI.md` §2.48). Here the
dispatch resolves to `notifications`, which ships in the worker, so **there is
no placeholder for the call itself** — only for the endpoint, which is the
reader's to replace with their customer's system.

The difference is the shape of the integration, not the quality of the two
examples: an order pipeline's rope side is three third-party services, and an
integration hub's rope side is one URL per connector.

## The four hitch points, and this scenario is the one that has all four

`docs/playbooks/integration-hub.md` lists them in "The assembly". Each is a
different extension point, and a scenario that exercised three of them would
not show that they compose:

| hitch point | what it is here | where |
|---|---|---|
| **HTTP routes** | the ingest endpoint, `POST /ingest/{source_id}` — HMAC-verified and auth-exempt | `webhook-ingest` |
| **Host functions** | the connector dispatch, `notifications.send_webhook` | `hub.go`, `dispatchToConnector` |
| **Edge middleware** | the rate limit, wrapping **every** request including core routes | `ratelimiter` |
| **Background loop** | the sweep over pending `webhook_delivery` rows, running `AcrossAllTenants` by name | `notifications` |

The middleware row is the one people misread. `ratelimiter`'s middleware does
not wrap only plugin routes: the plugin mux becomes the core mux, so a plugin
implementing `HasMiddleware` sees `POST /api/workflows/:name/start` exactly as
it sees its own routes (`cmd/cleat-worker/main.go`, the comment above the
`RegisterRoutes` loop).

**Two limiters run, they are different mechanisms, and one of them needs
seeding before it does anything.**

| | **core** | **plugin (`ratelimiter`)** |
|---|---|---|
| flags | `--rate-limit` (per IP, **100/s burst 200**), `--rate-limit-per-tenant` (**0 — off**) | none; limits live in the plugin's own table |
| seeded by | those flags | `PUT /rate-limits/{key}` — **nothing fires until a limit exists** |
| where | `rateLimitMiddleware`, wrapping the auth-wrapped handler | inside the plugin middleware chain, after auth resolves the tenant |
| its 429 carries | no `X-RateLimit-*` headers | `X-RateLimit-Limit`, `X-RateLimit-Remaining`, `X-RateLimit-Reset` |

**Both answer 429 with the body `{"error":"rate limit exceeded"}`**, so the body
cannot tell them apart. **The header is the discriminator**, and the scenario
asserts on it: a 429 that carries `X-RateLimit-Limit` is the plugin's edge
middleware, and one that does not is the core's.

The plugin one is the hitch point in the assembly table, and the seeding is the
part that catches people: `ratelimiter`'s middleware builds its token buckets
from rows in its own table, refreshed by its background loop, so a deployment
that never writes one has a registered middleware that **cannot fire**. Setting
`--rate-limit-per-tenant` configures the *core's* tier instead — a different
limiter with a different 429.

**The core limiter is what reaches the auth-exempt ingest route.** It is applied
outside auth, so it sees every request before anything decides who sent it, and
`--rate-limit` (100/s per IP by default) bounds the ingest endpoint. The
plugin's limiter cannot: it returns early when `auth.TenantIDFromContext` finds
no tenant, which is exactly what an auth-exempt route has.

**`--rate-limit 0` disables the IP limiter** — that is the condition under which
the ingest endpoint is bounded only by its body limit (1 MiB) and its signature
check. Stated as a condition rather than a default, because the default is
bounded in both size and rate.

## What this exists to show: a host call is recorded, not repeated

`notifications.send_webhook` is registered **`Idempotent: false`**. That makes
the durability property observable rather than merely asserted: if the engine
did not record the call, a worker that died after making it would make it again
on resume, and the customer's CRM would receive the same event twice.

So the scenario **kills the worker mid-run and asserts the count afterwards**:

- `send_webhook` does **not** deliver inline. It writes a `pending` delivery row
  and the plugin's own background loop performs the HTTP request.
- That gives a **countable** artefact: a re-executed call is a **second row**,
  visible through `GET /webhooks/{id}/deliveries`, and the count does not depend
  on HTTP timing.
- The workflow settles on a `DurableSleep` between the dispatch and the end of
  the run, which is where the kill lands — a durable step, so the resume point
  is unambiguous.

**A green run that never crashes does not test any of this.** The assertion is
the crash-resume count, not the completion.

## Build

```bash
cleat build -o /tmp/out ./examples/integration-hub/
```

The artifact is named for `cleat.yaml`'s own `name:` field: `integration-hub.wasm`
(cleat#2692). `cleat.yaml` also lists entry points in snake_case while the Go
function is `SyncCustomer` — that pairing, a separate thing from the filename,
is part of the ABI.

> `cleat build` emits **W003** here, warning that a single `string` parameter
> receives the whole input JSON rather than the field of that name. That is
> correct for this workflow — the input is an opaque payload it parses itself —
> and the warning is left visible rather than silenced, because it is the
> warning a reader needs if they meant the other thing.

## Run it

The compose file brings up a database and a `cleat-worker`, and **the profile
picks which database**. PostgreSQL is the default and is what the rest of this
README assumes; the other two are supported and covered — see below.

```bash
# 1. The database and the worker. The worker prints an API key on first start.
export CLEAT_DB_URL="postgres://cleat:cleat@localhost:5432/cleat?sslmode=disable"
docker compose --profile postgres up -d
docker compose logs cleat-worker | grep -i 'Key:'

# 2. Deploy the compiled workflow.
cleat deploy --db "$CLEAT_DB_URL" --name integration-hub /tmp/out/integration-hub.wasm
```

The `sink` service came up with the stack: it is the rope end, standing in for
your customer's system, and it carries **no profile** — it is not a database, so
every dialect needs it.

### Which dialects this runs on

**All three: PostgreSQL, MySQL and SQL Server.** The scenario script takes the
dialect as its first argument and CI runs one arm per dialect:

```bash
scripts/run-integration-hub-scenario.sh postgres   # the default
scripts/run-integration-hub-scenario.sh mysql
scripts/run-integration-hub-scenario.sh mssql
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

### The deploy step is the one command that differs by dialect

**The `cleat` CLI talks to PostgreSQL only.** That is a deliberate limit and not
a gap in the engine: `cleat deploy` refuses a MySQL or SQL Server DSN with an
error that says so, and names the alternative.

```bash
# MySQL or SQL Server: the CLI is PostgreSQL-only, so deploy with this instead.
export CLEAT_DIALECT=mysql          # or: mssql
go build -o /tmp/out/deploy-workflow ./cmd/deploy-workflow
/tmp/out/deploy-workflow --driver "$CLEAT_DIALECT" --db "$CLEAT_DB_URL" \
  integration-hub /tmp/out/integration-hub.wasm
```

`deploy-workflow` is the only multi-dialect deploy path cleat has, and it deploys
and nothing else. The scenario asserts this both ways: it runs the right command
per arm **and asserts that `cleat deploy` is refused, with the dialect named, on
the two where it does not work.**

### This example is where the dialect difference actually bites

Unlike its sibling, this scenario's assertions cross SQL that is written per
dialect. `delivery_count()` polls the notifications plugin's deliveries route,
and that handler alone crosses four seams — every one of them a place the
statement differs rather than the data:

| seam | why |
|---|---|
| `webhookExistsSQL` | `SELECT EXISTS(...)` **is not valid SQL Server syntax**; it is a `CASE WHEN EXISTS(...) THEN 1 ELSE 0 END` there |
| `plugin.LimitClause` | SQL Server has no `LIMIT` |
| `plugin.Rebind` | `$N` and `?` bind differently — by number vs by appearance |
| `plugin.ScanRow` | column scanning |

That endpoint's own source comment records what it cost the first time it met SQL
Server: *"every call failed outright."* **So the deliveries assertions are the
ones worth watching on this dialect arm** — they are what would have caught it,
and they are the reason an arm here is worth more than an arm on the saga.

**And one thing this arm does not prove on MySQL.** Every query on this path is
tenant-scoped (`... AND tenant_id = $2`), but cleat's MySQL is single-tenant by
construction, so there is exactly one possible value for that parameter and the
predicate cannot discriminate between tenants. The assertion is the same on all
three dialects; **what it proves is not** — on PostgreSQL that predicate sits
under row-level security, and on MySQL it is the only thing there.

The RLS difference is reported rather than averaged, exactly as in the sibling
example: on PostgreSQL the arm asserts the worker reports
`row-level security is enforced on this connection`; on MySQL and SQL Server it
prints that no such guarantee exists and asserts nothing about it, counting as
neither a pass nor a failure. `cmd/cleat-worker/main.go` gates that check on
`--driver == "postgres"`, so `--require-auth` there is not a weaker guarantee —
it is no check at all.

## The app

The commands above are the whole scenario, and they are what CI runs. There is
also a small web app, which is what a reader wants when they would rather click
than type:

```bash
cd examples/integration-hub
CLEAT_URL=http://localhost:8080 \
CLEAT_API_KEY="$CLEAT_API_KEY" \
CLEAT_SOURCE_ID="$SOURCE_ID" \
  go run ./backend
# -> http://localhost:9090
```

Run it from this directory: `-web` defaults to `web`, which is this example's
own, and `go run ./backend` finds the `examples` module by walking up.

`CLEAT_SOURCE_ID` is the ingest source registered below, and it is required
rather than discoverable: the ingest route resolves the tenant from that row and
not from the request, so this process may not invent one.

It is a `backendkit` proxy and holds no business logic — the integration IS the
workflow, and a backend that duplicated any of the dispatch would be
demonstrating the thing cleat exists to remove. It offers three things the
command line does not: a **delivery log** read from the connector's own record,
a **one-click inbound event** (it signs server-side, because the secret is the
source's and a browser must not hold it), and per-run **query state**, so you can
watch a run move through `waiting_for_event` → `dispatching` → `dispatched` →
`done` without reading event history.

> **The delivery log is where the durability property becomes visible.** Start a
> sync, deliver the inbound event, and watch one row appear. That row is the
> connector's record of the dispatch; the crash-resume assertion in
> `scripts/run-integration-hub-scenario.sh` is the same count, taken across a
> real `SIGKILL`. A resumed run that re-executed the call would leave **two**.

**A delivery's states are `pending`, `retrying`, `delivered`, `failed` and
`cancelled` — there is no `dead_lettered`, and the page offers exactly those
five.** `retrying` and `failed` are the same path with the ten-attempt ceiling
either side of it (`retryOrFail`, `plugins/notifications/background.go`), and
`cancelled` is a webhook deleted while its delivery was in flight.

**There are three dead-letter-adjacent vocabularies in reach here and
`webhook_delivery` is in none of them**, which is the conflation cleat#2050
corrected in this scenario's playbook:

| | the state | where |
|---|---|---|
| workflow **runs** | the dead-letter **queue**, `GET /api/dead-letters` | only runs whose last durable call exhausted its retry policy |
| inbound **events** | `status = 'dead_letter'` | `webhook-ingest`'s sweep, after its own retries |
| outbound **deliveries** | `failed` — there is no dead-letter state | ten attempts, then terminal |

So a filter offering `dead_lettered` on this table would be a control that can
never match, which is the same defect as one that filters nothing, wearing a
friendlier face.

**One read does not go through `backendkit`, and it is a gap rather than a
choice.** The delivery log and the connector list are a plugin's own HTTP routes
(`plugins/notifications/routes.go`), mounted on the worker's mux beside `/api/*`.
`backendkit.Client` covers the `/api/*` resources and the plugin *host-function*
path (`CallPlugin` → `/api/plugins/…`), and has no method for a route a plugin
registers. The example spells those two paths in `pluginGET` and reuses the
client `backendkit` was configured with, so the API key, the timeouts and the
tenant are still its — but the URL is not. Filed as **cleat#2550**.

> **A private rope end is refused by default, and permitting it is one flag.**
>
> cleat's egress policy refuses loopback and RFC1918 addresses, so without a
> permission the delivery is refused:
>
> ```
> notifications: delivery retrying attempt=1
> reason="request failed: Post \"http://sink:9099/hook\": egress to sink
> (172.19.0.3) is refused by cleat's network policy: RFC1918 private"
> ```
>
> **That error names a range and a reason and reads like a closed door. It is a
> door with a handle.** The policy is a floor *with an exemption*, not an
> absolute — `engine/egress_policy.go` marks the RFC1918 prefixes
> `exemptible: true`, and the compose sets:
>
> ```
> --plugin-egress-allow-private=sink
> ```
>
> The value is the host **as it appears in the endpoint URL**, matched as a
> string — so `sink`, `localhost` and `127.0.0.1` are three different entries.
> `cmd/cleat-worker/config.go` has the flag's own text, and its motivating case
> is a self-hosted model server on `http://localhost:11434`.
>
> **This is the permission an integration hub exists to need.** An iPaaS reaches
> systems *inside* the customer's network; telling yourself to use a publicly
> reachable endpoint is the opposite of the architecture. The flag is how you
> grant exactly one host, by name, rather than opening the range.

Register it as the connector:

```bash
# The rope end. In a deployment this URL is your customer's CRM, and the sink
# service does not exist -- it is here so the whole thing runs.
curl -fsS -X POST http://localhost:8080/webhooks \
  -H "Authorization: Bearer $CLEAT_API_KEY" -H "Content-Type: application/json" \
  -d '{"url":"http://sink:9099/hook","secret":"whsec_local_dev","events":["contact.updated"]}'
# -> {"id":"<WEBHOOK_ID>", ...}
```

And the ingest source the **customer's** system delivers into — a separate
registration, because it is a separate direction:

```bash
curl -fsS -X POST http://localhost:8080/ingest/sources \
  -H "Authorization: Bearer $CLEAT_API_KEY" -H "Content-Type: application/json" \
  -d '{"name":"crm","source_type":"crm","secret":"whsec_local_dev"}'
# -> {"id":"<SOURCE_ID>", ...}
```

Start a sync with both ids, then deliver the inbound event:

```bash
BODY='{"id":"c-1"}'
SIG="sha256=$(printf '%s' "$BODY" | openssl dgst -sha256 -hmac 'whsec_local_dev' -r | cut -d' ' -f1)"
curl -fsS -X POST "http://localhost:8080/ingest/$SOURCE_ID" \
  -H "Content-Type: application/json" \
  -H "X-Event-Type: contact.updated" \
  -H "X-Hub-Signature-256: $SIG" -d "$BODY"
```

**The event type is a HEADER, not a body field.** `webhook-ingest` reads
`X-Github-Event`, then `X-Event-Type`, and falls back to the literal `"webhook"`
if neither is present (`plugins/webhookingest/routes.go`). A body field named
`event_type` is carried through as payload and does not affect routing.

That is worth knowing because **the failure is on the other side of the
system**: a mismatched type stores the event under `webhook`, `await_webhook`
filters on the name it was given, finds nothing, and the run fails a wait-window
later saying no event arrived — which points at the ingest rather than at the
name. The event is in the table the whole time.

The delivery the connector enqueues is visible without reading any event
history:

```bash
curl -fsS "http://localhost:8080/webhooks/$WEBHOOK_ID/deliveries" \
  -H "Authorization: Bearer $CLEAT_API_KEY"
```

## Tests

```bash
cd examples && go test ./integration-hub/... -count=1
```

## Files

- `hub.go` — the workflow: the inbound wait, the dispatch, and the settle step
- `hub_test.go` — unit tests, driven through `cleattest`
- `backend/main.go` — the app: a `backendkit` proxy, and the delivery log
- `web/` — the page `backend/` serves: syncs, their query state, and the
  connector's deliveries
- `docker-compose.yml` — a database, a worker, and the `sink` standing in for the
  customer's system. **The database is chosen by profile** — PostgreSQL, MySQL or
  SQL Server, see "Which dialects this runs on" — and the `sink` carries no
  profile, because it is not a database and every dialect needs it. Two settings
  in it are the scenario's: the one private host
  is permitted by name (`--plugin-egress-allow-private=sink`), and the core's
  per-tenant limiter is left **off**, so a 429 observed here can only be the
  plugin's — see the two-limiter table above
- `cleat.yaml` — the workflow's name and entry points
