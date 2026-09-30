package eventtriggers

import "github.com/cleat-team/cleat/plugin"

// Dialect-specific query variants for structurally different SQL.

// upsertAwaiter registers or refreshes an awaiter's registration. The
// conflict target is registration_key -- a 64-character SHA-256 hex digest
// of (workflow_id, event_type, key1, key2, key3), computed in Go by
// keys.go's registrationKey and passed as the final argument -- not the
// surrogate id and not the five raw columns. A five-column unique index
// does not fit MySQL's or SQL Server's index-key byte limits once
// workflow_id and event_type are both present at full width; see
// registrationKey's own doc comment and migrations.go's Version 6 comment
// for the byte accounting. Matching on the hash is what makes a REPLAYED
// registerAwaiter call (awaitEvent is Idempotent: false,
// SameValueOnReplay: false, so a replay re-executes for real) refresh the
// same row rather than accumulate a duplicate, while still letting two
// awaits for the same (workflow, type) coexist when their key slots differ.
// cleat#2625.
var upsertAwaiter = plugin.Query{
	Default: `INSERT INTO event_awaiters (id, workflow_id, tenant_id, event_type, key1, key2, key3, registration_key, created_at)
VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6, $7, NOW())
ON CONFLICT (registration_key) DO UPDATE
	SET created_at = NOW()`,
	MySQL: `INSERT INTO event_awaiters (id, workflow_id, tenant_id, event_type, key1, key2, key3, registration_key, created_at)
VALUES (UUID(), $1, $2, $3, $4, $5, $6, $7, NOW())
ON DUPLICATE KEY UPDATE
	created_at = NOW()`,
	MSSQL: `MERGE event_awaiters AS target
USING (VALUES ($1, $2, $3, $4, $5, $6, $7, SYSUTCDATETIME())) AS source (workflow_id, tenant_id, event_type, key1, key2, key3, registration_key, created_at)
ON target.registration_key = source.registration_key
WHEN MATCHED THEN UPDATE SET created_at = SYSUTCDATETIME()
WHEN NOT MATCHED THEN INSERT (id, workflow_id, tenant_id, event_type, key1, key2, key3, registration_key, created_at)
VALUES (NEWID(), source.workflow_id, source.tenant_id, source.event_type, source.key1, source.key2, source.key3, source.registration_key, source.created_at);`,
}

var insertEventIdempotent = plugin.Query{
	Default: `INSERT INTO ingested_events (id, tenant_id, event_type, event_data, key1, key2, key3, received_at, processed)
VALUES ($1, $2, $3, $4, $5, $6, $7, NOW(), false)
ON CONFLICT (id) DO NOTHING`,
	MySQL: `INSERT IGNORE INTO ingested_events (id, tenant_id, event_type, event_data, key1, key2, key3, received_at, processed)
VALUES ($1, $2, $3, $4, $5, $6, $7, NOW(), false)`,
	MSSQL: `MERGE ingested_events AS target
USING (VALUES ($1, $2, $3, $4, $5, $6, $7, SYSUTCDATETIME(), 0)) AS source (id, tenant_id, event_type, event_data, key1, key2, key3, received_at, processed)
ON target.id = source.id
WHEN NOT MATCHED THEN INSERT (id, tenant_id, event_type, event_data, key1, key2, key3, received_at, processed)
VALUES (source.id, source.tenant_id, source.event_type, source.event_data, source.key1, source.key2, source.key3, source.received_at, source.processed);`,
}

var insertSubscriptionReturning = plugin.Query{
	Default: `INSERT INTO event_subscriptions (tenant_id, event_type, def_name, entry_point, input_template, filter_expr, max_retries, enabled, created_at)
VALUES ($1, $2, $3, $4, $5, $6, COALESCE($7, 3), true, $8)
RETURNING id`,
	MySQL: `INSERT INTO event_subscriptions (id, tenant_id, event_type, def_name, entry_point, input_template, filter_expr, max_retries, enabled, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, true, $9)`,
	MSSQL: `INSERT INTO event_subscriptions (tenant_id, event_type, def_name, entry_point, input_template, filter_expr, max_retries, enabled, created_at)
OUTPUT INSERTED.id
VALUES ($1, $2, $3, $4, $5, $6, COALESCE($7, 3), 1, $8)`,
}

// queryUnprocessedEvents feeds processBatch's subscription-dispatch sweep.
// It filters on dispatch_processed, NOT processed -- cleat#2663. `processed`
// is the awaiter-claim path's own flag (queryOldestUnprocessedEventForClaim,
// tryClaim in claim.go); this query used to share it, which meant an
// awaiter's claim could permanently starve a failed dispatch of its retry,
// the mirror image of that issue's main hazard. dispatch_processed
// (migrations.go Version 8) is this query's own column, written only by
// retryEvent/markRetryFailed in background.go.
//
// NO `status` PREDICATE ANY MORE, and that is deliberate, not an oversight --
// `status = 'pending' OR status IS NULL` used to do the SAME job
// `dispatch_processed` now does more precisely, plus one thing it should
// NOT have been doing: tryClaim (claim.go) sets status = 'consumed' on an
// awaiter's claim, which is a THIRD value this predicate excluded, so an
// awaiter's claim alone was enough to starve a dispatch retry even before
// `processed` entered into it. Every dispatch-domain terminal state
// (`completed`, `dead_letter`) is now set in the SAME statement as
// `dispatch_processed = true` (background.go), so `NOT dispatch_processed`
// alone already excludes them -- the status check was redundant for those
// two values and actively wrong for the third (`consumed`, a value this
// query's writer, retryEvent/markRetryFailed, never sets and has no reason
// to filter on).
//
// A THIRD writer of `processed` exists outside this package --
// webhookingest's handleDeleteSource cancels a deleted source's pending
// ingested_events rows, to stop await_webhook/awaitEvent's backstop scan
// from ever delivering one. Per cleat-review round 2 on cleat#2822, it also
// sets dispatch_processed = true in the same statement, so a cancelled
// event stays fully inert on both paths -- preserving what the shared
// column did as a side effect before this split. Decided in #2822 (closing
// #2820, which asked the question): a deleted source's events are inert on
// both the awaiter and dispatch paths, matching what webhook_events already
// does for its own cancellation.
var queryUnprocessedEvents = plugin.Query{
	Default: `SELECT id, tenant_id, event_type, event_data, retry_count
FROM ingested_events
WHERE NOT dispatch_processed
  AND received_at < NOW() - INTERVAL '10 seconds'
ORDER BY received_at
LIMIT 100`,
	MySQL: `SELECT id, tenant_id, event_type, event_data, retry_count
FROM ingested_events
WHERE NOT dispatch_processed
  AND received_at < DATE_SUB(NOW(), INTERVAL 10 SECOND)
ORDER BY received_at
LIMIT 100`,
	// `dispatch_processed = 0`, not `NOT dispatch_processed`: T-SQL has no
	// boolean type, so a BIT column is a value and not a condition.
	// `WHERE NOT dispatch_processed` is rejected with "An expression of
	// non-boolean type specified in a context where a condition is expected"
	// (Msg 4145) -- a BINDING error, which is why SET PARSEONLY ON accepts
	// the statement and only SET NOEXEC ON rejects it.
	//
	// This arm had LIMIT 100 translated to OFFSET/FETCH and NOW() - INTERVAL
	// translated to DATEADD, and kept the primary dialect's boolean test. That
	// is the failure mode cleat#1133 records for dialect arms generally: naming
	// what a variant is FOR narrows the reviewer to that purpose, and the rest
	// of the literal inherits the primary dialect unexamined.
	MSSQL: `SELECT id, tenant_id, event_type, event_data, retry_count
FROM ingested_events
WHERE dispatch_processed = 0
  AND received_at < DATEADD(second, -10, SYSUTCDATETIME())
ORDER BY received_at
OFFSET 0 ROWS FETCH NEXT 100 ROWS ONLY`,
}

// The oldest unprocessed event of a type for a tenant, locked for exclusive
// claim by the caller's transaction.
//
// OLDEST, NOT LATEST -- cleat#2641. This used to be queryLatestUnprocessedEvent,
// ORDER BY received_at DESC: a newest-first read that starves any older
// unconsumed event of the same type, the same property queryUnprocessedEvents
// (the plural one, below) already avoids for the background dispatcher. The
// two names differing by one word is what let the bug hide -- a grep for
// "ORDER BY received_at" finds the plural query's ascending order and reads as
// "the fix landed" without checking which query, or which caller.
//
// FOR UPDATE SKIP LOCKED / WITH (UPDLOCK, READPAST, ROWLOCK) -- the same
// per-dialect claim idiom plugins/scheduler/background.go already uses for
// exactly this problem (see dueSchedulesQuery there). The lock is what makes
// folding the SELECT and the consuming UPDATE into one transaction actually
// atomic: two concurrent await_event calls for the same tenant+type must not
// both select the same row, and SKIP LOCKED means the second one finds the
// next-oldest row instead of blocking or double-claiming. The caller commits
// (or rolls back) the transaction this query runs in; the lock is held no
// longer than that.
//
// Three constructs here have no portable spelling, which is why this is a
// plugin.Query rather than one literal (cleat#1133):
//
//   - `NOT processed`. T-SQL has no boolean type, so a BIT column is a value
//     and not a condition; SQL Server answers Msg 4145, "An expression of
//     non-boolean type specified in a context where a condition is expected".
//     That is a BINDING error rather than a syntax error, so SET PARSEONLY ON
//     accepts it and only SET NOEXEC ON rejects it.
//   - `LIMIT 1`. T-SQL spells row limits as TOP or OFFSET/FETCH.
//   - `FOR UPDATE SKIP LOCKED`. T-SQL spells the same intent as a table hint
//     on the FROM clause, not a trailing clause -- WITH (UPDLOCK, READPAST,
//     ROWLOCK). UPDLOCK takes the lock this statement needs to hold; READPAST
//     is SQL Server's SKIP LOCKED; ROWLOCK asks for row- rather than
//     page-granularity so an unrelated row in the same page is not blocked.
//
// None of the three is something the adapter's Rebind should attempt. A
// boolean COLUMN is not a boolean LITERAL -- rewriting `NOT x` would have to
// leave NOT EXISTS, NOT IN, NOT LIKE, NOT NULL and NOT (a AND b) alone --
// moving LIMIT to TOP relocates a token to a different clause, and FOR UPDATE
// SKIP LOCKED relocates to a different clause entirely. All three belong in an
// explicit arm.
// key1/key2/key3 are equality predicates, not a filter (§2 of the event
// routing design: "correlation is equality and filtering is not" -- only an
// indexed equality is affordable per (event x awaiter) pair). An uncorrelated
// caller passes keySlots(nil)'s "","","" -- §4.4's sentinel -- which matches
// only an event published with no keys either, exactly as it did before this
// predicate existed: an empty-keyed ingested_events row's key1/key2/key3
// columns are ” by the same DEFAULT ” every pre-existing row already
// carries, so this is additive to the WHERE clause, not a behaviour change
// for a caller that passes no keys.
var queryOldestUnprocessedEventForClaim = plugin.Query{
	Default: `SELECT id, event_type, event_data, received_at
FROM ingested_events
WHERE tenant_id = $1
  AND event_type = $2
  AND key1 = $3
  AND key2 = $4
  AND key3 = $5
  AND NOT processed
ORDER BY received_at
LIMIT 1
FOR UPDATE SKIP LOCKED`,
	// FORCE INDEX (idx_ingested_events_correlate), NOT idx_ingested_events_unprocessed
	// -- this changed once, and changed BACK, and both changes have a
	// measured reason rather than a guess.
	//
	// cleat#2646 round 4 forced idx_ingested_events_unprocessed
	// (processed, received_at), because at that time the WHERE clause had
	// no key1/key2/key3 predicate at all -- every awaiter used the "","",""
	// sentinel -- and MySQL's optimizer sometimes preferred
	// idx_ingested_events_correlate anyway, purely on its (tenant_id,
	// event_type) prefix. Under THAT shape, the correlate index's remaining
	// columns (key1, key2, key3, received_at) supplied no useful ordering
	// or narrowing -- every candidate row shared the same empty keys -- so
	// InnoDB's next-key locking over an effectively unconstrained scan
	// left the wrong rows locked: measured 9 of 10 failures on
	// TestAwaitEventConcurrentClaimsSkipTheLockedRow, 10 of 10 clean with
	// the correlate index dropped entirely.
	//
	// cleat#2647 gave every claim a REAL key1/key2/key3 equality predicate.
	// That changes which index is actually cheap: idx_ingested_events_unprocessed
	// (processed, received_at) has no key columns at all, so a claim
	// correlated on one key must SCAN PAST every other key's rows of the
	// same type to reach the one it wants, evaluating key1/key2/key3 as a
	// post-scan filter -- and under MySQL's default REPEATABLE READ,
	// InnoDB's locking read takes next-key locks on every index record it
	// examines during that scan, not only the one row the filter keeps.
	// cleat-review predicted the consequence by reasoning alone (#2668
	// round 1) and it reproduced exactly as stated:
	// TestAwaitEventConcurrentClaimsOfDifferentKeysDoNotBlockOnMySQL claims
	// a NEWER key while an OLDER, differently-keyed row of the same type
	// sits in front of it in (processed, received_at) order -- the scan
	// locks the older row on its way past, and a concurrent claim for that
	// older key hits "sql: no rows in result set" against its own,
	// unrelated, unlocked-in-principle event.
	//
	// idx_ingested_events_correlate (tenant_id, event_type, key1, key2,
	// key3, received_at) does not have this problem ONCE every column
	// ahead of received_at is bound by equality, which #2647 made
	// permanent: MySQL can seek directly to the exact (tenant_id,
	// event_type, key1, key2, key3) range and never touch a row of a
	// different key at all, let alone lock one. Measured: switching this
	// FORCE INDEX target makes
	// TestAwaitEventConcurrentClaimsOfDifferentKeysDoNotBlockOnMySQL pass,
	// while TestAwaitEventConcurrentClaimsSkipTheLockedRow (same key,
	// round 4's original scenario) and
	// TestAwaitEventConcurrentClaimsOfDifferentEventTypesDoNotInterfereOnMySQL
	// (#2645's own coverage) both stay green -- neither of those two ever
	// needed the unprocessed index for correctness; they simply never
	// exercised a case where the two indexes' locking shapes diverged.
	//
	// cleat#2669: idx_ingested_events_correlate itself is gone as of
	// Version 7, replaced by idx_ingested_events_claim -- same leading
	// columns, but with `processed` added so the index can exclude a
	// processed row rather than merely filter it out after finding it.
	// idx_ingested_events_correlate had no processed column, so the claim
	// walked -- and under REPEATABLE READ, locked -- every PROCESSED row
	// for its key tuple before reaching the first unprocessed one; nothing
	// deletes from ingested_events, so that walk was the tenant's entire
	// history of the event type for an unkeyed claim (today's only usage).
	// See migrations.go's Version 7 comment for the measurement (Postgres
	// 2.5ms to 0.46ms, SQL Server 193ms to 5.5ms, MySQL 28.4ms to 0.33ms
	// (85x), all at 20,000 rows of history) and for why this was a real,
	// and equally unbounded, cost on Postgres and SQL Server too, not a
	// MySQL-only concern -- MySQL is only the dialect where the old shape
	// was a correctness bug on top of the performance one.
	//
	// "processed = FALSE", not "NOT processed": measured with EXPLAIN
	// ANALYZE at n=2000 that MySQL's optimizer already produces the exact
	// same plan for both -- "Index lookup ... (..., processed=0)",
	// actual rows=1 -- because `processed` is a two-valued (0/1) column
	// and MySQL's range optimizer folds a NOT of one into an equality
	// against the other. So this is not a correctness fix; it is the
	// explicit form, matching MSSQL's `processed = 0` arm below, so a
	// future reader does not have to re-derive the same EXPLAIN to be
	// sure the predicate is sargable -- raised in cleat-review's #2675
	// round 1 as a question ("is this sargable on MySQL?"), settled by
	// measurement rather than by inspection.
	MySQL: `SELECT id, event_type, event_data, received_at
FROM ingested_events FORCE INDEX (idx_ingested_events_claim)
WHERE tenant_id = $1
  AND event_type = $2
  AND key1 = $3
  AND key2 = $4
  AND key3 = $5
  AND processed = FALSE
ORDER BY received_at
LIMIT 1
FOR UPDATE SKIP LOCKED`,
	MSSQL: `SELECT TOP 1 id, event_type, event_data, received_at
FROM ingested_events WITH (UPDLOCK, READPAST, ROWLOCK)
WHERE tenant_id = $1
  AND event_type = $2
  AND key1 = $3
  AND key2 = $4
  AND key3 = $5
  AND processed = 0
ORDER BY received_at`,
}
