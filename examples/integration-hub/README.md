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
| **Background loop** | the retry sweep over undelivered events, running `AcrossAllTenants` by name | `webhook-ingest` |

The middleware row is the one people misread: `ratelimiter`'s middleware does
not wrap only plugin routes. The plugin mux becomes the core mux, so a plugin
implementing `HasMiddleware` sees `POST /api/workflows/:name/start` exactly as
it sees its own routes (`cmd/cleat-worker/main.go`, the comment above the
`RegisterRoutes` loop).

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

The artifact is named for the entry point: `sync_customer.wasm`. `cleat.yaml`
lists entry points in snake_case while the Go function is `SyncCustomer` — the
entry point is part of the ABI.

> `cleat build` emits **W003** here, warning that a single `string` parameter
> receives the whole input JSON rather than the field of that name. That is
> correct for this workflow — the input is an opaque payload it parses itself —
> and the warning is left visible rather than silenced, because it is the
> warning a reader needs if they meant the other thing.

## Run it

```bash
docker compose up -d
docker compose logs cleat-worker | grep -i 'Key:'
cleat deploy --db "postgres://cleat:cleat@localhost:5432/cleat?sslmode=disable" \
  --name integration-hub /tmp/out/sync_customer.wasm
```

Then stand up the rope end — a sink, standing in for the customer's system —
and register it as the connector:

```bash
# The rope end. In a real deployment this URL is the customer's CRM.
python3 -m http.server 9099 &

# Register it. The secret is required: notifications signs every delivery.
curl -fsS -X POST http://localhost:8080/webhooks \
  -H "Authorization: Bearer $CLEAT_API_KEY" -H "Content-Type: application/json" \
  -d '{"url":"http://host.docker.internal:9099/hook","secret":"whsec_local_dev","events":["contact.updated"]}'
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
BODY='{"event_type":"contact.updated","payload":{"id":"c-1"}}'
SIG="sha256=$(printf '%s' "$BODY" | openssl dgst -sha256 -hmac 'whsec_local_dev' -r | cut -d' ' -f1)"
curl -fsS -X POST "http://localhost:8080/ingest/$SOURCE_ID" \
  -H "Content-Type: application/json" -H "X-Hub-Signature-256: $SIG" -d "$BODY"
```

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
- `docker-compose.yml` — PostgreSQL and a worker, with a low per-tenant rate so
  the middleware is observable rather than described
- `cleat.yaml` — the workflow's name and entry points
