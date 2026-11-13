# Find a run by a value it published

"Which run is for order 123?" has a one-line answer once the workflow has published
`order_id`:

```bash
curl "http://localhost:8080/api/workflows?state.order_id=123"
```

The response is the usual workflow listing (a bare array, with the total in the
`X-Total-Count` header), narrowed to the runs whose published state contains that pair.

## Publish the value

A workflow publishes with `SetQueryState`, as described under *Query state* in
[Common patterns](common-patterns.md#query-state):

```go
h.SetQueryState("order_id", orderID)
```

**The search sees what the run had published when its last segment ended.** State is
written to the database when a run suspends (a sleep, a signal wait, a retry) or
finishes or fails, not at the instant `SetQueryState` runs. A run in the middle of a
long segment is found by what it published at the segment before.

## What the filter does

| | |
|---|---|
| **Match** | Exact. Key and value are both compared as written, so `ORDER_ID` is not `order_id` and `Open` is not `open`, on every database. |
| **Several pairs** | `?state.order_id=123&state.status=open` selects runs that published **both**. At most **4** pairs; a fifth is a `400`. |
| **A repeated key** | `?state.a=1&state.a=2` is a `400`, because no run can satisfy both. |
| **An empty value** | `?state.status=` selects runs that published an empty status. A run that published no `status` at all is not selected. |
| **A bare `state=`** | A `400`: the parameter needs a key, `state.<key>=<value>`. |
| **Other filters** | They compose: `?state.order_id=123&status=ready&def_name=process-order`. |
| **Scope** | Your tenant only. On PostgreSQL the statement names the tenant itself, so this holds even on a connection that bypasses row-level security; SQL Server applies its policy to every connection, and MySQL keeps each tenant in its own database. |
| **Paging** | `limit` and `offset` work as on the plain listing, and `X-Total-Count` counts the filtered set. |

The dashboard's **Workflows** page has a box for the same thing: type `order_id=123`,
press Enter or **Search**, and **Clear** to go back. A value may contain `=`; spaces
around the key and value are trimmed (use the API for a value with leading or trailing
spaces).

PostgreSQL, MySQL and SQL Server answer the same questions the same way. Only
PostgreSQL has a documented index for it, below.

### A deployment that seals payloads cannot be searched this way

With `--encrypt-sensitive-payloads` the database holds `query_state` as ciphertext, and
a database cannot compare ciphertext to a value. The request fails with **`422`**
and a message naming the flag, rather than returning an empty list that reads as "no
such run". That flag is PostgreSQL only.

### If you want exactly one run per key

When the question is "at most one run for this order, and give me that one",
a search is the wrong tool. Start the run with a **`concurrency_key`** and look it up
directly:

```bash
curl "http://localhost:8080/api/workflows?concurrency_key=order-123"
```

That needs no published state. **It also changes behaviour:** runs that share a
concurrency key are serialised, so it is the right choice only when you want one run at
a time per key, and the wrong one if you only wanted to be able to find several.

## What it costs, and the index you can add

By default the search is **unindexed**: the database reads your tenant's rows and tests
each one. That is deliberately the default, because the alternative taxes every
deployment, including those that never search.

Measured 2026-11-13, PostgreSQL 16.14 in a container on a laptop that other work was
also loading. These are the shape of the cost, not a promise about your hardware:

| | rows | time |
|---|---|---|
| unindexed, tenant-scoped `query_state @>` | 1,000,000 (250,000 for the tenant) | 75–95 ms warm; 415 ms the first time |
| with the index below | 200,000 | 0.02 ms for one key |

So at 250,000 runs for one tenant an unindexed search took under a tenth of a second
warm. Add the index only when you have **a very large table and search often**.

### Add it

```sql
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_payloads_query_state
    ON workflow_payloads USING gin (query_state jsonb_path_ops);
```

- Run it as the **owner of the table** (the migration role), not the application
  role. `CONCURRENTLY` cannot run inside a transaction block.
- If cleat's tables live in a schema other than the default one, qualify the table:
  `ON <schema>.workflow_payloads`.
- If a `CONCURRENTLY` build is interrupted it leaves an `INVALID` index behind. Drop it
  and run the statement again.
- **Use that name.** A later release that ships an index of its own would pick a different one.
- The filter needs no change to use it: it is written as `query_state @> '{...}'::jsonb`,
  the containment form a GIN index serves, and not `query_state ->> 'key' = 'value'`,
  which none can.
  `TestAStateFilterUsesAnIndexAnOperatorAddsAndNeedsNoneToWork`
  (`engine/a_run_is_found_by_what_it_published_on_postgres_test.go`) runs `EXPLAIN` on the
  statement the store builds, with the index absent and then present, and fails if the
  index is not in the plan.

### What the index costs on writes

The same laptop, `pgbench`, 16 clients, 60 s per variant, two repetitions in opposite
orders, on a table shaped like `workflow_payloads` with its real indexes. One "lifecycle" is
one insert and five updates, three of which rewrite `query_state`. **That mix is a reading
of the write paths, not a production trace.**

| | no index | with the index |
|---|---|---|
| lifecycles per second | 1087 / 1158 | 963 / 852 (about 19% lower on average) |
| p99 latency per lifecycle | 59 / 66 ms | 74 / 113 ms |
| share of updates that are heap-only (HOT) | 98.4 / 98.6 % | **19.8 / 19.8 %** |
| dead tuples at the end | 4,266 / 88 | 194,654 / 205,236 |

The structural cost is the HOT share: with the index, four updates in five stop being
heap-only, which are exactly the ones that rewrite `query_state`. Expect more
autovacuum work. The p99.9 and maximum latencies were dominated by noise and are not quoted. Anything
beyond 16 clients, a long soak, tuned autovacuum, and encrypted runs were not measured.
**Measure on your own workload before relying on any of these numbers.**

### It does not interfere with cleat's own checks

Checked 2026-11-13, by reading what each check inspects:

- **Startup and `--migrate-only`** verify each migration's name and checksum against
  `schema_migrations`, and the checksum is of the migration *file*. They do not read the
  live indexes.
- **The schema comparison** (`migration/catalogdiff`) is run by tests, against databases
  they build themselves. It is not run against yours.
- **No later migration alters `workflow_payloads`.** Only `001_schema.sql` declares or
  alters it:

  ```bash
  grep -lE 'ALTER TABLE( ONLY)?[[:space:]]+workflow_payloads' migrations/postgres/*.sql
  ```

  A migration that changed the column's type would have to rebuild this index, and to a
  type with no `jsonb_path_ops` operator class it would fail. None does today; dropping
  the column drops the index with it.
