# Health, readiness, and telling a database incident from a worker incident

A worker answers three unauthenticated probes and one authenticated one. They exist so that a database
outage and a worker fault look different to a load balancer, to Kubernetes, and to whoever is on call.

| Path | Answers | 200 | 503 |
|---|---|---|---|
| `/livez` | is the process alive and are its background loops ticking? | yes | a background loop is stuck (`background_loop_stuck`) |
| `/readyz` | can it serve traffic right now? | `/livez`, and started, and not draining, and its database answered | `starting`, `draining`, `database_unreachable`, or `background_loop_stuck` |
| `/healthz` | an alias of `/livez`, kept because callers exist | as `/livez` | as `/livez` |
| `GET /api/admin/health` | the same facts with the detail (needs an API key) | always 200 | (401 without a key) |

**`/livez` does not look at the database.** A database outage must not restart workers: restarting cannot
fix it, and every restart strands the worker's in-flight runs. That includes the loops it causes to stall: a background loop whose database call is hanging goes stale after a few of its intervals, and while the database is known to be unreachable such a loop does not fail `/livez` (it is listed under `stale_loops` on `/api/admin/health`, with `loops_blocked_on_database`). A loop that is stuck while the database answers still does. Measured against a real `docker pause`: without this, `/livez` answered 503 six seconds in and a kubelet would have restarted every worker. `/readyz` does look at it, so a worker that
cannot reach its database stops receiving traffic it cannot serve, and is left running.

## What the bodies say

The three unauthenticated bodies contain **only** `ok`, `degraded` and reason codes:

```json
{"ok": false, "reason": "database_unreachable", "reasons": ["database_unreachable"]}
{"ok": true, "degraded": true, "reason": "plugin_unhealthy", "reasons": ["plugin_unhealthy"]}
```

The codes are `background_loop_stuck`, `database_unreachable`, `starting`, `draining` (these make `/readyz`
503), and `memory_pressure`, `plugin_unhealthy` (these do **not**). A degraded worker is still serving, so
a degraded state is reported with a 200 and never fails a probe: a lost audit event
([audit log](../reference/audit-log.md)) must not take every worker out of rotation. Degraded reasons
combine: memory pressure no longer hides an unhealthy plugin.

Loop names, plugin names and messages, and database error text are **not** in a public body (a host name or a
DSN fragment can be in an error). They are on `GET /api/admin/health`, which sits behind the same
authentication as the other `/api/admin/*` routes and is exactly as open as they are: with
`--require-auth=false` everything is open.

## How the database is measured

Every deadline-bounded database call a worker already makes (the heartbeat when it has work, an idle ping
when it does not, and the reaper) reports its outcome. A call counts as **failed** if it returns an error or
if it returns success after its deadline (half the heartbeat interval, at least two seconds). The second
half is what catches a database that hangs connections instead of refusing them, which is what a pause, a
failover, or a saturated proxy looks like: no connection error is ever returned. The worker also makes one
such call at startup, so `/readyz` has an answer within seconds of boot (until then it says `starting`).

Two log lines mark the transitions, once each, not once per probe:

```
database unreachable (deadline exceeded)
database reachable again after 43s
```

A worker that finds its schema behind (`--migrate-only` not run, #2117) refuses to start and never listens,
so it is never ready. There is no runtime re-check of the schema.

## Metrics

| Metric | Meaning |
|---|---|
| `cleat_db_reachable{dialect}` | 1 if the latest bounded call succeeded within its deadline, 0 if not |
| `cleat_db_last_success_timestamp_seconds{dialect}` | when the last one did |
| `cleat_db_consecutive_failures{dialect}` | failed calls in a row |
| `cleat_db_probe_duration_seconds{dialect}` | histogram of the calls' durations; a slow database shows here before it is unreachable |

## Telling the two incidents apart

`monitoring/prometheus/alerts.yml`, loaded by the bundled `prometheus.yml`:

- **Every worker reports `cleat_db_reachable 0`** (`CleatDatabaseUnreachable`): a **database incident**.
  Restarting workers will not help; check the database, the network to it, credentials, connection limits.
- **One worker reports 0 while a peer reports 1** (`CleatWorkerCannotReachDatabase`): **that worker's**
  connectivity. `GET /api/admin/health` on it has the error text.
- **`up == 0`** (`CleatWorkerDown`): the process or the network to it, not the database (a database outage
  leaves `/metrics` up).
- **Runs reclaimed by the reaper** (`CleatRunsReclaimedByTheReaper`) read as workers failing. The rule is
  suppressed while the database is unreachable and for ten minutes after, when a reclaim is the outage's
  echo; outside that it means a worker stopped heartbeating while its peers could reach the database.
- **`CleatDatabaseSlow`** (p95 round trip over 1s) is the warning before the first.

The rules are unit-tested with synthetic series (`monitoring/prometheus/alerts_test.yml`, run by
`promtool test rules` in CI), including the case that the same evidence pages differently when one worker
reports 0 and when all do.

## Kubernetes

The Helm chart and `k8s/deployment.yaml` use `/livez` for the liveness probe and `/readyz` for the readiness
probe. The bundled cluster compose file uses `/livez` for its healthchecks, and the cluster tests wait on
`/readyz`. Anything of yours that polled `/healthz` for "can this serve traffic" should move to `/readyz`;
`/healthz` keeps meaning liveness.
