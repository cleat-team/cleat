# Audit log: the hash chain and how to verify it

The `audit-log` plugin records one row in `audit_events` for every API request: who, what,
when, from where, and the status returned. Since cleat#2047 each tenant's rows also form a
**hash chain**, so an edit to a row, or a row removed from the middle or the end, can be
detected afterwards.

This page says what that does and does not establish. Read the second half before you
describe the log to an auditor.

## What is recorded

`method`, `path`, `status_code`, `user_id` (the OAuth subject when the request carried one, else
empty), `ip_address`, `user_agent`, `duration_ms` and `timestamp`, plus the chain columns
`seq`, `prev_hash` and `row_hash`. `metadata` is always `{}` today; it is part of the hash so a
later use of the column is already covered.

Text that cannot be stored or reproduced is replaced, not dropped: a `User-Agent` is bytes an
attacker chooses, and PostgreSQL refuses invalid UTF-8 and NUL bytes. Both are replaced with
U+FFFD before the row is hashed and before it is stored, so the two agree. Before this change
such a request was not recorded at all.

A value too long for its column is cut, with the marker `...[truncated]`, before it is hashed
and stored, for the same reason: a 1,000-character path used to fail the insert on MySQL and
SQL Server, and the request was simply not in the log. The limits are the same on every
dialect, so one request makes the same row everywhere: `method` 255 characters, `path` 700
characters and 800 UTF-16 code units (SQL Server cannot index more), and `user_id`,
`ip_address` and `user_agent` 4,096 characters. The MySQL `timestamp` column is `DATETIME(6)`
holding UTC, not `TIMESTAMP(6)`, which stops at 2038.

## The chain

Each tenant has its own chain and its own head row in `audit_chain_heads`. An append is one
transaction: lock the head, hash the new row over the previous row's hash, insert it at
`seq + 1`, and move the head. Two workers appending for one tenant queue on the head's lock;
`UNIQUE (tenant_id, seq)` makes a fork impossible to store even if something bypassed the lock.
Tenants never contend with each other.

`row_hash = SHA-256(domain || prev_hash || fields)`, where the fields are, in order, `tenant_id`,
`seq`, `id`, `timestamp`, `method`, `path`, `status_code`, `user_id`, `ip_address`, `user_agent`,
`duration_ms`, `metadata`. The encoding is a contract so that a verifier that is not cleat can
reproduce it:

- the domain is the bytes `cleat-audit-chain-v1` followed by a NUL;
- `prev_hash` is 32 raw bytes; a tenant's first row uses 32 zero bytes;
- each field is a presence byte (`0` for SQL NULL, `1` for present) and, when present,
  `uvarint(len)` and the UTF-8 bytes;
- every field is text: integers in decimal, UUIDs lowercase and hyphenated;
- the timestamp is UTC to the microsecond, `YYYY-MM-DDTHH:MM:SS.ffffffZ`. It is read from the
  database as epoch microseconds by a dialect-specific expression, never through a driver, so a
  MySQL session's time zone cannot change a hash;
- `metadata` is canonical JSON: keys sorted, no whitespace, numbers as their original token.

`plugins/auditlog/testdata/audit_chain_reference.py` implements this in Python, independently of
the Go code, and generates the test vectors the Go code is checked against. Use it as the
starting point for an offline verifier.

## Verifying

    cleatctl --db "$DSN" audit verify --tenant <tenant-id>
    cleatctl --db "$DSN" audit verify --all-tenants [--json]

The command recomputes every row from the recorded floor to the head and reports the **first**
break for each tenant. It reads and never writes. `--all-tenants` also visits a tenant whose head
row is missing, which is itself a break. It needs the same kind of `--db` role as the other
`cleatctl` commands.

| exit | meaning |
|---|---|
| `0` | every chain verified |
| `1` | a chain did not verify; the output names the first break |
| `2` | the check could not be completed: bad usage, the audit tables are absent, a query failed. **This is a failure of the check, not a finding about the log.** |

A run that could not read some tenant exits `2` even when others verified, and prints how many
it did read (`verified 2 of 3 tenant chain(s)`), so a partial run cannot be read as a clean one.

Verification is safe on a live log. It bounds its scan by the head it read at the start, so rows
appended meanwhile are not reported as extra rows, and it repeats an attempt whose floor moved
under it (a retention sweep) or that was chosen as a deadlock victim. If the chain keeps
changing faster than it can be read, verify gives up with exit `2`, never with a finding.

Pass `--retention-days N` (the plugin's `retention_days`) to also check that the floor covers only
rows old enough to have expired. See "What a clean result means" for what that does and does not
catch.

| break | what it means |
|---|---|
| `edited` | a row's stored hash is not the hash of its own contents |
| `missing` | rows are absent from the middle of the chain, or from its start without the floor accounting for them |
| `relinked` | a row's `prev_hash` is not its predecessor's hash |
| `truncated_tail` | the head is ahead of the newest row: the newest rows are gone |
| `extra_rows` | rows exist beyond the head |
| `head_missing` | chained rows exist and the head row that anchors them does not |
| `head_mismatch` | every row verifies, but the head records a different hash for the newest row |
| `unreadable` | a row could not be hashed at all |
| `floor_unexpired` | the floor covers a row younger than the retention window, or has no recorded timestamp. Only checked when the retention period is supplied; raising `retention_days` later reports floors set under the shorter value |

## What a clean result means, and what it does not

**It means** every row from the floor to the head is present, in order, unedited, and linked to
its predecessor, and the head agrees with the newest row. Anyone who edited or removed a row
without also rewriting the head, the floor, and every later row would be caught.

**It does not mean the log is complete.** An event that was never appended, because the process
crashed, the buffer was full, or an insert failed, leaves no gap in the chain. The chain proves
the integrity of what was recorded, not that everything was recorded.

**It does not protect against anyone who can write these tables.** The hash is not keyed, and
nothing outside the database anchors it. That is not only a database administrator: the
credential the workers run with can write `audit_events` and `audit_chain_heads`, so a worker, or
any plugin running in it, can rewrite a whole chain and its head consistently, and verification
will pass.

The floor is a second place to hide a deletion, and the head hash alone does not cover it.
Deleting the first rows and moving the floor over them (`floor_seq`, `floor_hash`) leaves a chain
that verifies and a head hash that has not changed, looking like retention. Retention records the
timestamp of the row it removed (`floor_ts`), and `verify --retention-days N` reports a floor whose
timestamp is inside the retention window. That catches a floor moved carelessly or by code that
did not record one. It cannot catch a floor written with a false timestamp, because nothing
outside the database says what the timestamp should be.

If you need the guarantee against a writer, copy `head_seq`, `head_hash`, `floor_seq`, `floor_hash`
and `floor_ts` for each tenant to somewhere the workers' credential cannot write, on a schedule,
and compare: the head must only move forward and the floor must only move forward, at the pace
retention would move it.

**It does not cover rows written before the chain existed.** Migration `3` of the plugin adds the
chain columns and does not backfill them: older rows have no `seq`, are counted as `unchained` in
the report, and are outside the guarantee.

## Retention

Retention removes an expired **prefix** of a tenant's chain, at most 5,000 rows per tenant per
sweep, and records what it removed in the head row: `floor_seq` and `floor_hash` (the hash of
the last row deleted), in the same transaction, under the head's lock. The verifier starts at
`floor_seq + 1` and requires the first surviving row to link to `floor_hash`. Rows removed
without the floor moving are reported as `missing`.

Retention refuses to move the floor over a gap. If rows between the old floor and the new one are
already gone, it rolls back and logs a warning telling an operator to run `audit verify`, because
recording the gap as a floor would turn a finding into a fact.

The sweep visits every tenant that has an expired row, whether or not the tenant is still
registered. It no longer uses one cross-tenant `DELETE`.

A dropped tenant's audit rows and head row survive `drop-tenant`, as `audit_events` always has
(`docs/plugin-table-handling.md`), and are removed by retention when they expire.
