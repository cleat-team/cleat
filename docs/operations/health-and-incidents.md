# Health, readiness, and telling a database incident from a worker incident

A worker answers three unauthenticated probes and one authenticated one. They exist so that a database
outage and a worker fault look different to a load balancer, to Kubernetes, and to whoever is on call.

| Path | Answers | 200 | 503 |
|---|---|---|---|
| `/livez` | is the process alive and are its background loops ticking? | yes | a background loop is stuck (`background_loop_stuck`) |
| `/readyz` | can it serve traffic right now? | `/livez`, and started, and not draining, and its database answered | `starting`, `draining`, `database_unreachable`, or `background_loop_stuck` |
| `/healthz` | an alias of `/livez`, kept because callers exist | as `/livez` | as `/livez` |
| `GET /api/admin/health` | the same facts with the detail (needs `--enable-admin-api` and an API key) | always 200 | (404 unless `--enable-admin-api`; 401 without a key) |

**`/livez` does not look at the database.** A database outage must not restart workers: restarting cannot
fix it, and every restart strands the worker's in-flight runs. That includes the loops it causes to stall: a background loop whose database call is hanging goes stale after a few of its intervals, and a loop the database is holding does not fail `/livez` (it is listed under `stale_loops` on `/api/admin/health`, with `loops_blocked_on_database`; how that is decided is under [How the database is measured](#how-the-database-is-measured)). A loop that is stuck while the database answers still does. Measured against a real `docker pause`: without this, `/livez` answered 503 six seconds in and a kubelet would have restarted every worker. `/readyz` does look at it, so a worker that
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
DSN fragment can be in an error). They are on `GET /api/admin/health`, which is off unless the worker runs with
`--enable-admin-api` (it answers 404 otherwise) and, when on, is callable by ANY authenticated API key of any
tenant, plugin messages included, until cleat has an operator credential (cleat#2169). See
[The admin API](admin-api.md). With `--require-auth=false` it is open to everyone who can reach the port.

## How the database is measured

Every deadline-bounded database call a worker already makes (the heartbeat when it has work, an idle ping
when it does not, and the reaper) reports its outcome. A call counts as **failed** if it returns an error or
if it returns success after its deadline (half the heartbeat interval, at least two seconds). The second
half is what catches a database that hangs connections instead of refusing them, which is what a pause, a
failover, or a saturated proxy looks like: no connection error is ever returned. The worker also makes one
such call at startup, so `/readyz` has an answer within seconds of boot (until then it says `starting`).

Whether a stale background loop is put down to the database (and so does not fail `/livez`) is decided
per loop, from what the worker has actually observed. Each rule was added after a real `docker pause` or a
review measurement showed the simpler version wrong:

- **The database has not answered since the loop went quiet.** If a bounded call that began more than one
  loop interval after the loop's last tick succeeded, the database was fine while the loop stayed silent, and
  the loop is wedged on something else: `/livez` is 503. (The first version asked "is the database known to
  be unreachable?", which is not known until a bounded call misses its deadline, up to a heartbeat interval
  plus that deadline after the pause. `/livez` answered 503 in that window.)
- **Newer evidence wins.** A failure from a call that started *before* the latest successful call began is
  dropped. Otherwise a call that hung, followed by one that succeeded, would let the first call's deadline
  mark a database that just answered as unreachable.
- **Evidence expires.** The excuse holds only while a database call is in flight (a paused database holds
  the call, which is the only observation there is) or the last observation is recent (twice the heartbeat
  interval plus the call deadline). If nothing has looked at the database for longer than that,
  "it has not answered" is only the absence of asking, and `/livez` stops excusing stale loops. When a hung
  call finally returns, that counts as an observation, or the verdict would look abandoned half a second
  before the next probe.
- **A grace after recovery, for the loops that outage held.** For 30 seconds after the database answers
  again, a stale loop that went quiet during the outage that just ended is still put down to it: the loops
  that were stuck in a call resume only when the driver returns. A loop that went quiet BEFORE the outage
  began gets no grace. Without that, one missed deadline (which is an "outage" of a few seconds) excused
  every stale loop for 30 seconds, and a loop wedged while the database was healthy stayed live for
  17 minutes under a miss every 26 seconds.

Measured against a real 50-second `docker pause`, sampling every second: `/livez` 200 throughout the pause
and for 25 seconds after unpausing, `/readyz` 503 with `database_unreachable` during and 200 one second after.

One limit remains, and it is inherent: a loop that went quiet *during* an outage cannot be told from one the
outage is holding, so an unrelated wedge that begins mid-outage is reported live until the outage ends and
the grace runs out. `/readyz` is already 503 for the outage, and `stale_loops` on the admin route lists the
loop either way.

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
  connectivity. Read that worker's log for the line `database unreachable (deadline exceeded)`,
  `(connection error)` or `(error or ran past its deadline)`. `GET /api/admin/health` has the error text
  too, when the admin API is enabled, but it authenticates against the same database, so during an outage
  it can hang or answer 401; do not rely on it for this.
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
