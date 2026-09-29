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

var queryUnprocessedEvents = plugin.Query{
	Default: `SELECT id, tenant_id, event_type, event_data, retry_count
FROM ingested_events
WHERE NOT processed
  AND (status = 'pending' OR status IS NULL)
  AND received_at < NOW() - INTERVAL '10 seconds'
ORDER BY received_at
LIMIT 100`,
	MySQL: `SELECT id, tenant_id, event_type, event_data, retry_count
FROM ingested_events
WHERE NOT processed
  AND (status = 'pending' OR status IS NULL)
  AND received_at < DATE_SUB(NOW(), INTERVAL 10 SECOND)
ORDER BY received_at
LIMIT 100`,
	// `processed = 0`, not `NOT processed`: T-SQL has no boolean type, so a BIT
	// column is a value and not a condition. `WHERE NOT processed` is rejected
	// with "An expression of non-boolean type specified in a context where a
	// condition is expected" (Msg 4145) -- a BINDING error, which is why
	// SET PARSEONLY ON accepts the statement and only SET NOEXEC ON rejects it.
	//
	// This arm had LIMIT 100 translated to OFFSET/FETCH and NOW() - INTERVAL
	// translated to DATEADD, and kept the primary dialect's boolean test. That
	// is the failure mode cleat#1133 records for dialect arms generally: naming
	// what a variant is FOR narrows the reviewer to that purpose, and the rest
	// of the literal inherits the primary dialect unexamined.
	MSSQL: `SELECT id, tenant_id, event_type, event_data, retry_count
FROM ingested_events
WHERE processed = 0
  AND (status = 'pending' OR status IS NULL)
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
	// FORCE INDEX pins this to the index TestAwaitEventConcurrentClaimsSkipTheLockedRow
	// (added by cleat#2641/#2645) and TestAwaitEventConcurrentClaimsOfDifferentEventTypesDoNotInterfereOnMySQL
	// already characterise: idx_ingested_events_unprocessed's leading column is
	// (processed, received_at), narrow enough that InnoDB's next-key locking
	// under REPEATABLE READ has a known, tested shape (documented on the
	// latter test above). cleat#2625's Version 6 migration added a SECOND
	// index, idx_ingested_events_correlate (tenant_id, event_type, key1, key2,
	// key3, received_at), for signalAwaiters'/unregisterAwaiter's key-scoped
	// lookups -- and its leading columns happen to match this query's WHERE
	// clause too. Left to the optimizer, MySQL sometimes prefers it here
	// instead, and its locking shape has NOT been characterised the way the
	// other index's has: measured directly (10 iterations of
	// TestAwaitEventConcurrentClaimsSkipTheLockedRow against a real MySQL
	// 8.4, tenant_id fresh per iteration so cross-iteration data was the only
	// variable), the unforced query failed 9 of 10 with "claim query: sql: no
	// rows in result set" -- txB's SKIP LOCKED claim found nothing where
	// exactly one unprocessed row of its own tenant existed, unlocked and
	// waiting. Dropping idx_ingested_events_correlate made the same 10
	// iterations pass 10 of 10, isolating the index choice as the cause
	// rather than the query text. FORCE INDEX (not USE INDEX -- this must
	// exclude the correlate index, not merely admit the other one as an
	// option) restores the characterised locking shape; the correlate index
	// remains exactly as useful to the key-scoped queries it was built for.
	MySQL: `SELECT id, event_type, event_data, received_at
FROM ingested_events FORCE INDEX (idx_ingested_events_unprocessed)
WHERE tenant_id = $1
  AND event_type = $2
  AND key1 = $3
  AND key2 = $4
  AND key3 = $5
  AND NOT processed
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
