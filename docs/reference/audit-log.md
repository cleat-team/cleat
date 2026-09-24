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

## Delivery: when the database is slow, or down

A request does not write its audit row itself. When the request finishes, the middleware fixes the
event's `id` and `timestamp` and hands it to a bounded in-memory queue; a small pool of workers appends
queued events to their tenants' chains. That keeps a slow audit table from slowing the API, and it has a
cost this page states rather than hides.

- **A full queue makes the request wait, briefly, and then gives the event up.** The wait is
  `enqueue_wait_ms` (default 1,000). It is paid only while the queue is full, and it is the most the audit log
  can ever add to a request, however long the database is down.
- **A failed append is retried** with jittered backoff (100 ms doubling to 5 s) for up to
  `retry_deadline_ms` (default 60,000). A retry first looks for its own event by `id` under the tenant's head
  lock, so an append that committed but whose acknowledgement was lost is recorded once and never twice.
- **An event that is given up on is counted and logged, not dropped.** The reasons are `buffer_full` (no room
  within the wait), `insert_failed` (the database refused it for the whole retry deadline) and `shutdown` (the
  process stopped first; shutdown drains the queue for up to `shutdown_drain_ms`, default 10,000, and counts what
  is left). Each is an `AUDIT EVENT LOST` Error log naming the tenant, method, path, status and the last
  error (at most one line per reason per second, carrying how many more were lost in between), and the host
  is told through `Environment.EventsLost`. While any loss is recent the plugin reports unhealthy
  (`plugin.HasHealth`).
- **The event's `timestamp` is when the request finished**, not when it was appended.

**Chain order is not timestamp order.** With several workers and retries, an event can be appended seconds
after its request finished, so `seq` order and `timestamp` order can disagree. The chain is ordered by `seq`:
verification, export and retention all read it that way (retention removes a prefix by `seq`, so a row whose
timestamp is older than a later row's is not removed before it), and none assumes timestamps increase.

**What this does not do: survive the process.** The queue is memory. A process that is killed loses the
events it held, and nothing counts them, because nothing was running to count. A durable spool would close
that; it is not built. `workers`, `buffer_size`, `enqueue_wait_ms`, `retry_deadline_ms` and `shutdown_drain_ms`
are the plugin config keys (defaults 4, 1,000, 1,000, 60,000, 10,000).

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

## Exporting

    GET /audit/export?from=<rfc3339>&to=<rfc3339>&cursor=<cursor>&format=jsonl
    cleatctl --db "$DSN" audit export (--tenant <tenant-id> | --all-tenants) [--from T] [--to T] [--out FILE]

The HTTP endpoint streams the caller's tenant as JSON Lines. Any authenticated caller of the tenant
may call it, exactly as `GET /audit/events`. There is no cross-tenant HTTP endpoint: an operator uses
`cleatctl audit export --all-tenants`, which connects to the database directly, and writes one
complete stream per tenant, one after another.

`from` and `to` are RFC 3339, inclusive, at microsecond resolution. A malformed value is a `400`,
not an ignored filter (`GET /audit/events` ignores one). An export of a range has gaps in `seq`
where the range excluded rows; consecutive records still link.

**A short export must not read as a complete one.** The last line is always a `checkpoint`
record. A stream without one is truncated. If the server fails after it has started sending, it
aborts the connection instead of ending the stream cleanly, and `cleatctl` prints `INCOMPLETE`
with the tenant and the number of records written and exits `2`.

Rows written before the chain existed come first (ordered by timestamp, then id), then the chained
rows by `seq`. Chained rows appended after the export began are not in it: they are the next
export's, and the checkpoint's `head_seq` says where this one ends.

**Rows removed while an export runs are not the next export's, they are missing from this one.**
Without a `from`/`to` range the chained rows must be an unbroken run, and if a retention sweep (or a
missing row) breaks it, the export fails instead of ending in a checkpoint over a hole: the HTTP
handler answers `409` if nothing was sent yet and otherwise aborts the connection, and `cleatctl`
prints `INCOMPLETE`. Repeat the export. A resume whose next rows were swept away gets the same
answer; start again from the beginning.

Records:

    {"type":"event","cursor":"...","id":"...","tenant_id":"...","seq":12,
     "timestamp":"2026-09-24T01:02:03.456789Z","method":"GET","path":"/workflows",
     "status_code":200,"user_id":"...","ip_address":"...","user_agent":"...","duration_ms":12,
     "metadata":{},"prev_hash":"<64 hex>","hash":"<64 hex>"}
    {"type":"checkpoint","tenant_id":"...","head_seq":15,"head_hash":"<64 hex>",
     "floor_seq":0,"floor_hash":"<64 hex>","from":null,"to":null,"after_seq":null,
     "events":15,"unchained":0}

- `seq`, `prev_hash` and `hash` are `null` for a row written before the chain existed, which the
  chain does not cover.
- `timestamp` and `metadata` are exactly the strings that were hashed (a UTC microsecond timestamp,
  canonical JSON), so a consumer can verify a record from the record alone.
- `cursor` is a position, not data, and is not covered by the hash. Pass the cursor of the last
  record you received as `cursor=` (`--cursor` on `cleatctl`, single tenant) to resume after it.
  It is opaque: it is not a format to build.
- `checkpoint.events` counts the records of this call, and `unchained` how many of them carry no `seq`.
  `head_seq` and `head_hash` are the tenant's chain head when the export began, and `floor_seq` and
  `floor_hash` where retention has moved the start; these are what an external anchor would record.
  `unchained` is the count **in this export**, not the tenant's current one: retention removes old
  unchained rows, so record it beside the head and floor and compare a later export against an
  earlier one's.

The checkpoint also says what kind of export this was, which decides what a verifier may require:
`from` and `to` (set for a range), and `after_seq` (set when the export resumed from a cursor).

Offline verification:

    python3 plugins/auditlog/testdata/audit_chain_reference.py verify-export [options] < export.jsonl

It needs no database and shares no code with cleat. `verify-export --help` prints the full rules. It
recomputes every chained record's hash from its own fields; refuses duplicate ids, a `seq` that does not
strictly increase, consecutive records that do not link, and an unchained record after a chained one (an
export sends the unchained ones first); and requires exactly one checkpoint whose `events` matches. What
else it requires depends on the kind of export the checkpoint says it is:

| export | also required |
|---|---|
| full (no range, no `after_seq`) | no gaps; the first chained record is `floor_seq + 1` and links to `floor_hash`; the last is `head_seq` with `head_hash` |
| resumed (`after_seq`) | no gaps; the first is `after_seq + 1`; the last is the head; **no unchained records** (a resume starts in the chained part). The join to the part before the cursor cannot be checked without an anchor |
| range (`from` or `to`) | nothing about coverage: a range has gaps by design, and a record deleted from inside one is not detectable |

Exit `0` verified, `1` a break, `2` could not establish it: an incomplete or unreadable stream, a contradictory
command line, or `INCONCLUSIVE` (below).

**The checkpoint is not signed**, and it says which kind of export the file is, so an edit can delete
records and relabel the file as a range, or move an end to match. From the file alone that is not
detectable, and the verifier prints a `NOTE` saying what it did not establish. The options say what *you*
know from somewhere the editor cannot reach. Each pins the kind you expect (a checkpoint claiming another
is `DOWNGRADED`) and is checked against the **records**, not against the checkpoint:

| option | kind it requires | what it checks |
|---|---|---|
| `--require-full` | full | nothing more: you asked for a whole export |
| `--expect-floor SEQ:HASH` | full | normally a **check, never a verification**: the floor you recorded is at the export's start (compared with the checkpoint's own `floor_hash`, and the first record must link to it) or has been retired. Given a later `SEQ` it is matched to a record like any anchor. Give `--expect-head` too |
| `--expect-head SEQ:HASH` | full, or resumed with `--expect-after` | the chain passes through `SEQ` with `HASH`, `SEQ` at most the export's head: **the anchor that verifies** |
| `--expect-after SEQ:HASH` | resumed, with `after_seq == SEQ` | a **check, never a verification**: the first chained record is `SEQ + 1` linking to `HASH` (the join to the part you already hold), which binds no record's content. Always give `--expect-head` too |
| `--expect-unchained N` | full, or resumed with `--expect-after` (then `N` is 0) | **at most** `N` unchained records: record `N` from the checkpoint's `unchained` when you record the head and floor |

**Only `--expect-head` verifies.** `--expect-floor` (at the floor) and `--expect-after` are checks: on their own an honest file exits `2`. **An anchor is a point the chain passes through**, not the end of the export. The record at `SEQ` must be
in the export with `HASH`; a hash depends on every record before it, so the anchor binds every record at or
below `SEQ`. That is what lets an anchor recorded last week verify an honest export made today, after the
tenant has grown. What it does not bind is everything **above** `SEQ`: those records are held only by the
unsigned checkpoint, and the run prints how many. Record the newest head you have.

- An anchor **above** the export's head means the chain was cut back, or the export is older than the
  anchor: `ANCHOR MISMATCH`.
- An anchor **below** the export's start has been retired by retention (or, for a resumed export, is in the
  part you already hold): the verifier has nothing to compare it with and prints `NOTE ... verified NOTHING`.
- An anchor **at** the export's start (the floor) is compared with the checkpoint's own `floor_hash`. That
  binds the first record's link and no record's content, and whoever edits the file also writes the
  checkpoint, so it does **not** count as verification.

**An anchor is verified only by a record present in the file with the anchored hash, and at least one anchor
must be, or the run is `INCONCLUSIVE` (exit 2).** A resumed export checked by `--expect-after` alone is the same case: a file forged wholesale from the join passes it. Retirement and
"the export starts here" are both decided by the checkpoint, which the editor of a file also chooses: cut
records 1 to 10, move the floor to 10, and a head anchor at 10 matches the forged floor, while an older floor
anchor is retired. Without this rule every anchor becomes a note and the file exits 0. A retired floor anchor
**beside a verified head anchor** is exit 0 with a note: that is every honest sweep. So **refresh the head
anchor more often than `retention_days`**, or an honest export will be inconclusive too.

- **The chain is unkeyed.** Anyone who can edit the file can recompute every hash, so deleting or editing a
  record and re-hashing what follows makes a file that is consistent with itself. Only an anchor at or above
  the edit disagrees with it. `--expect-floor` at the floor binds where the export starts and what it links
  to and **nothing else**: every record after it can have been rewritten.
- **A cut of the first records below your highest verified anchor looks exactly like a retention sweep**
  to an offline check, so no anchor can catch it. What tells them apart is the database: the floor only moves
  forward, so a file whose checkpoint `floor_seq` is **ahead of** the database's was cut. Compare the
  checkpoint's `floor_seq` and `floor_hash` (and `head_seq`, `head_hash`) with `cleatctl audit verify --json`,
  at or after the time of the export. `cleatctl audit verify --retention-days` checks the database's own floor
  against the retention period; it cannot see a cut made to a file. A consumer with no database access has
  only their anchors.

`--expect-after` cannot be combined with `--require-full` or `--expect-floor`. For a whole export give
`--expect-head` with the newest head you have recorded: that is what binds the records. `--expect-floor` adds
a check at the start for as long as the floor has not moved. For a resumed one give `--expect-after` and
`--expect-head`.

**`--expect-unchained` exists because an unchained record has no hash.** The chain cannot say that one was
added, and one added at the front of the file looks like the rows written before the chain existed. One
added after a chained record is refused without any option. The count is a **ceiling**: none are ever added
once the chain exists, but retention removes old ones, so a later honest export has fewer, and the run notes
that a removal cannot be told apart from a sweep. Record the count from the checkpoint's `unchained` (an
editor can change the checkpoint to match, so the recorded value is what counts). The ceiling bounds **only the count**, and it admits one forged row for every unchained row retention has removed since
`N` was recorded (2 recorded, 1 swept, 1 forged is exit `0`); that is inherent offline. Their **contents** are
covered by nothing, whatever is passed.

With no option a downgraded file verifies (exit `0`, with the note). That is the limit of the file alone, and
`chain_export_matrix_test.go` pins it: the whole grid of export kinds, option sets and edits, with the exit
code and finding each must give.

## Verifying

    cleatctl --db "$DSN" audit verify --tenant <tenant-id>
    cleatctl --db "$DSN" audit verify --all-tenants [--json]
    GET /audit/verify

`GET /audit/verify` does the same for the caller's tenant, using the plugin's own `retention_days`,
and answers `200` with `"ok": false` and the break when the chain does not verify: a finding is not
an HTTP error, and a `500` means the check could not be made.

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
rows old enough to have expired. Without it, a tenant whose floor exists gets one `NOTE` line on
stderr saying its age was not checked, and `--json` reports `"floor_age_checked": false` (only
meaningful when `floor_seq` is above zero): a check that was not made must not read as one that
passed. See "What a clean result means" for what that does and does not catch.

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
| `rows_below_floor` | chained rows survive at or below the recorded floor. Retention deletes them in the same transaction that moves the floor, so the floor was moved by something else, and every row below it went unverified |
| `floor_unexpired` | the floor covers a row younger than the retention window, or has no recorded timestamp. Only checked when the retention period is supplied; raising `retention_days` later reports floors set under the shorter value |

## What a clean result means, and what it does not

**It means** every row from the floor to the head is present, in order, unedited, and linked to
its predecessor, and the head agrees with the newest row. Anyone who edited or removed a row
without also rewriting the head, the floor, and every later row would be caught.

**It does not mean the log is complete.** An event that was never appended leaves no gap in the chain: one
given up on because the queue stayed full or the database kept refusing it is counted and logged (see
*Delivery*), and one held by a process that was killed is not counted at all. The chain proves the integrity of
what was recorded, not that everything was recorded.

**It does not protect against anyone who can write these tables.** The hash is not keyed, and
nothing outside the database anchors it. That is not only a database administrator: the
credential the workers run with can write `audit_events` and `audit_chain_heads`, so a worker, or
any plugin running in it, can rewrite a whole chain and its head consistently, and verification
will pass.

The floor is a second place to hide a deletion, and the head hash alone does not cover it. Verify
checks it in two ways. Retention deletes the rows at or below the floor in the same transaction that
moves it, so a chained row that survives there means the floor was not set by retention:
`rows_below_floor`, always checked. And retention records the timestamp of the last row it removed
(`floor_ts`), so `verify --retention-days N` reports a floor whose timestamp is inside the retention
window: `floor_unexpired`.

What that leaves. A floor move on its own is not trusted: one `UPDATE` of the head over rows that
are still there is `rows_below_floor`. To hide an edit at seq 50 an attacker needs an `UPDATE` of the
head **and** a `DELETE` of every row up to the floor, and, to pass `--retention-days`, a false
`floor_ts`, which nothing outside the database can contradict. The chain proves the integrity of
what was recorded; it does not stop a writer who does all of that.

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

Retention does not re-verify what it deletes. An expired row that had been tampered with can be
removed by a sweep, after which the chain verifies clean from the new floor: the deleted prefix is
no longer verifiable, and that is inherent.

Retention refuses to move the floor over a gap. If rows between the old floor and the new one are
already gone, it rolls back and logs a warning telling an operator to run `audit verify`, because
recording the gap as a floor would turn a finding into a fact.

The sweep visits every tenant that has an expired row, whether or not the tenant is still
registered. It no longer uses one cross-tenant `DELETE`.

A dropped tenant's audit rows and head row survive `drop-tenant`, as `audit_events` always has
(`docs/plugin-table-handling.md`), and are removed by retention when they expire.
