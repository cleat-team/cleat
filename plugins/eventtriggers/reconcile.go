package eventtriggers

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/cleat-team/cleat/plugin"
	"github.com/google/uuid"
)

// The awaiter reconciler. cleat#3835.
//
// PublishEvent commits the event row and THEN calls signalAwaiters, as a
// separate step. An event whose publisher died between the two, or whose
// SignalWorkflow failed on every attempt (signalRetryDelays), is stored,
// unprocessed, with an awaiter registered for it and nobody told. The waiter
// finds it only by looking again, and cleat#3806 slows that look to every
// five minutes. This sweep is the other half: it finds such pairs and delivers
// the signal the publisher did not.
//
// Every worker runs it, on its own tick, against the same tables. That is
// deliberate, not an oversight: a singleton would need leader election this
// plugin has nowhere to put, and two workers reaching the same awaiter is made
// harmless rather than prevented -- see reconcileOne.
const (
	// reconcileGrace is how old an unprocessed event must be before the sweep
	// treats it as stranded. A healthy publish signals within milliseconds
	// (the retries add under a second), so this is long enough that the sweep
	// does not race a publish still in flight and short enough to be well under
	// the cap a waiter's own re-check backs off to.
	reconcileGrace = 15 * time.Second

	reconcileInterval = 30 * time.Second

	// reconcileBackoffInitial doubles per consecutive failed delivery to one
	// awaiter, up to reconcileBackoffMax. Held in memory, per worker: a
	// restart forgets it, which costs one early retry per stranded awaiter.
	reconcileBackoffInitial = 30 * time.Second
	reconcileBackoffMax     = 10 * time.Minute

	// reconcileMaxPages bounds the pages one sweep reads.
	reconcileMaxPages = 50
)

// reconcilePageSize bounds one scan page. The scan pages by awaiter id rather
// than taking the first N, so a run of awaiters that keep failing and sit in
// backoff cannot fill the page and keep a deliverable one from ever being
// reached. A variable only so a test can make a page smaller than the number
// of failing awaiters it seeds.
var reconcilePageSize = 200

// zeroUUID is the keyset cursor's start: no real id sorts below it on any of
// the three dialects.
const zeroUUID = "00000000-0000-0000-0000-000000000000"

// stuckAwaiters pages through awaiters that have an unprocessed event, older
// than reconcileGrace, with the same tenant, type and key slots.
//
// "Unprocessed" is ingested_events.processed, the flag an awaiter's CLAIM sets,
// not dispatch_processed, which belongs to the subscription path. An event an
// awaiter has already claimed is not stranded.
//
// The grace comparison is in SQL, as queryUnprocessedEvents does it, so it
// reads the database's clock on both sides of the comparison instead of
// mixing a Go time with a column that is stored in the session's.
func stuckAwaiters(pageSize int) plugin.Query {
	return plugin.Query{
		Default: fmt.Sprintf(`SELECT a.id, a.workflow_id, a.tenant_id, a.event_type, a.key1, a.key2, a.key3
FROM event_awaiters a
WHERE a.id > $1
  AND EXISTS (
    SELECT 1 FROM ingested_events e
    WHERE e.tenant_id = a.tenant_id AND e.event_type = a.event_type
      AND e.key1 = a.key1 AND e.key2 = a.key2 AND e.key3 = a.key3
      AND NOT e.processed
      AND e.received_at < NOW() - INTERVAL '%d seconds')
ORDER BY a.id
LIMIT %d`, int(reconcileGrace.Seconds()), pageSize),
		MySQL: fmt.Sprintf(`SELECT a.id, a.workflow_id, a.tenant_id, a.event_type, a.key1, a.key2, a.key3
FROM event_awaiters a
WHERE a.id > $1
  AND EXISTS (
    SELECT 1 FROM ingested_events e
    WHERE e.tenant_id = a.tenant_id AND e.event_type = a.event_type
      AND e.key1 = a.key1 AND e.key2 = a.key2 AND e.key3 = a.key3
      AND e.processed = 0
      AND e.received_at < DATE_SUB(NOW(), INTERVAL %d SECOND))
ORDER BY a.id
LIMIT %d`, int(reconcileGrace.Seconds()), pageSize),
		// processed = 0, not NOT processed: a BIT is a value in T-SQL, not a
		// condition (see queryUnprocessedEvents).
		MSSQL: fmt.Sprintf(`SELECT CAST(a.id AS NVARCHAR(36)), a.workflow_id, CAST(a.tenant_id AS NVARCHAR(36)), a.event_type, a.key1, a.key2, a.key3
FROM event_awaiters a
WHERE a.id > $1
  AND EXISTS (
    SELECT 1 FROM ingested_events e
    WHERE e.tenant_id = a.tenant_id AND e.event_type = a.event_type
      AND e.key1 = a.key1 AND e.key2 = a.key2 AND e.key3 = a.key3
      AND e.processed = 0
      AND e.received_at < DATEADD(second, -%d, SYSUTCDATETIME()))
ORDER BY a.id
OFFSET 0 ROWS FETCH NEXT %d ROWS ONLY`, int(reconcileGrace.Seconds()), pageSize),
	}
}

// oldestStrandedEvent reads, without locking, the payload of the oldest
// unprocessed event an awaiter is waiting on. Oldest first, as the claim is
// (cleat#2641). The reconciler only wakes the waiter; the waiter's own claim
// is what consumes the event.
var oldestStrandedEvent = plugin.Query{
	Default: `SELECT event_data, received_at FROM ingested_events
WHERE tenant_id = $1 AND event_type = $2 AND key1 = $3 AND key2 = $4 AND key3 = $5
  AND NOT processed
ORDER BY seq
LIMIT 1`,
	MySQL: `SELECT event_data, received_at FROM ingested_events
WHERE tenant_id = $1 AND event_type = $2 AND key1 = $3 AND key2 = $4 AND key3 = $5
  AND processed = 0
ORDER BY seq
LIMIT 1`,
	MSSQL: `SELECT event_data, received_at FROM ingested_events
WHERE tenant_id = $1 AND event_type = $2 AND key1 = $3 AND key2 = $4 AND key3 = $5
  AND processed = 0
ORDER BY seq
OFFSET 0 ROWS FETCH NEXT 1 ROWS ONLY`,
}

// awaiterKey identifies one registration for the backoff table.
type awaiterKey struct {
	workflowID, eventType, key1, key2, key3 string
	tenantID                                uuid.UUID
}

type backoffEntry struct {
	failures int
	retryAt  time.Time
}

// reconcileBackoff is the in-memory, per-worker delay between attempts to
// deliver to an awaiter whose delivery keeps failing. Without it a workflow
// that cannot be signalled would be retried, with signalWithRetry's own
// sleeps, on every tick for as long as it stays registered.
type reconcileBackoff struct {
	mu      sync.Mutex
	entries map[awaiterKey]backoffEntry
}

func (b *reconcileBackoff) due(k awaiterKey, now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	e, ok := b.entries[k]
	return !ok || !now.Before(e.retryAt)
}

func (b *reconcileBackoff) failed(k awaiterKey, now time.Time) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.entries == nil {
		b.entries = make(map[awaiterKey]backoffEntry)
	}
	e := b.entries[k]
	e.failures++
	d := reconcileBackoffInitial
	for i := 1; i < e.failures && d < reconcileBackoffMax; i++ {
		d *= 2
	}
	if d > reconcileBackoffMax {
		d = reconcileBackoffMax
	}
	e.retryAt = now.Add(d)
	b.entries[k] = e
	return d
}

func (b *reconcileBackoff) clear(k awaiterKey) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.entries, k)
}

// retain forgets every key not in seen, so the table cannot outgrow the set of
// awaiters still stranded.
func (b *reconcileBackoff) retain(seen map[awaiterKey]struct{}) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for k := range b.entries {
		if _, ok := seen[k]; !ok {
			delete(b.entries, k)
		}
	}
}

// reconcileAwaiters runs one sweep: it pages through the stranded awaiters and
// delivers to each that is not in backoff. It returns how many it delivered.
func (p *Plugin) reconcileAwaiters(parent context.Context, now time.Time) int {
	if p.env == nil || p.env.SignalWorkflow == nil {
		return 0
	}
	// The scan spans tenants by definition; the work on each result does not,
	// and is rebuilt from context.Background() for the same reason
	// processBatch's is: a ForTenant applied over AcrossAllTenants is silently
	// ignored (cleat#1515).
	scanCtx := plugin.AcrossAllTenants(parent,
		"event-triggers awaiter reconciler: the scan for stranded awaiters spans every tenant by definition")

	type stuck struct {
		id  string
		key awaiterKey
	}
	delivered := 0
	seen := make(map[awaiterKey]struct{})
	cursor := zeroUUID
	complete := false

	for page := 0; page < reconcileMaxPages; page++ {
		if parent.Err() != nil {
			return delivered
		}
		rows, err := p.db.Query(scanCtx, stuckAwaiters(reconcilePageSize).For(p.dialect), cursor)
		if err != nil {
			p.logger.Error("event-triggers: reconcile: scan stranded awaiters", "error", err)
			return delivered
		}
		var batch []stuck
		for rows.Next() {
			var (
				id     string
				k      awaiterKey
				tenant string
			)
			if err := plugin.ScanRow(rows, &id, &k.workflowID, &tenant, &k.eventType, &k.key1, &k.key2, &k.key3); err != nil {
				p.logger.Error("event-triggers: reconcile: scan awaiter", "error", err)
				continue
			}
			tid, err := uuid.Parse(tenant)
			if err != nil {
				p.logger.Error("event-triggers: reconcile: awaiter has a malformed tenant id", "awaiter_id", id, "error", err)
				continue
			}
			k.tenantID = tid
			batch = append(batch, stuck{id: id, key: k})
		}
		rowsErr := rows.Err()
		rows.Close()
		if rowsErr != nil {
			p.logger.Error("event-triggers: reconcile: awaiter rows error", "error", rowsErr)
			return delivered
		}
		if len(batch) == 0 {
			complete = true
			break
		}
		for _, s := range batch {
			seen[s.key] = struct{}{}
			cursor = s.id
			if !p.backoff.due(s.key, now) {
				continue
			}
			if p.reconcileOne(s.id, s.key, now) {
				delivered++
			}
		}
		if len(batch) < reconcilePageSize {
			complete = true
			break
		}
	}
	// Only a sweep that reached the end of the set knows which awaiters are
	// no longer stranded; a truncated one would forget live backoff.
	if complete {
		p.backoff.retain(seen)
	}
	return delivered
}

// reconcileOne delivers the stored event to one stranded awaiter. It reports
// whether the signal was delivered.
//
// ONE DELIVERY OUT OF N WORKERS. Every worker sweeps, so two can reach the
// same awaiter in the same moment. The awaiter row is the token: each deletes
// it by id and only the one whose DELETE affected a row signals. The loser
// affects zero rows and stops. A failed delivery puts the registration back,
// so the next sweep -- after the backoff -- finds it again, which is also what
// keeps the waiter findable by the publisher's own signalAwaiters meanwhile.
func (p *Plugin) reconcileOne(awaiterID string, k awaiterKey, now time.Time) bool {
	ctx := plugin.ForTenant(context.Background(), k.tenantID)

	var (
		payload    []byte
		receivedAt time.Time
	)
	if err := p.db.QueryRow(ctx, oldestStrandedEvent.For(p.dialect),
		k.tenantID, k.eventType, k.key1, k.key2, k.key3).Scan(&payload, &receivedAt); err != nil {
		// Claimed by the waiter's own recheck since the scan: not stranded now.
		return false
	}

	n, err := p.db.Exec(ctx, `DELETE FROM event_awaiters WHERE id = $1`, awaiterID)
	if err != nil {
		p.logger.Warn("event-triggers: reconcile: take awaiter", "workflow_id", k.workflowID, "error", err)
		return false
	}
	if n != 1 {
		return false
	}

	switch deliverToAwaiter(ctx, p.db, p.logger, p.env, k.workflowID, k.eventType, string(payload), k.key1, k.key2, k.key3, true) {
	case awaiterDelivered:
		p.backoff.clear(k)
		p.awaitersReconciled.Add(1)
		p.logger.Info("event-triggers: reconciled a stranded event to its awaiter",
			"tenant_id", k.tenantID,
			"event_type", k.eventType,
			"workflow_id", k.workflowID,
			"event_age_ms", now.Sub(receivedAt).Milliseconds(),
		)
		return true
	case awaiterGone:
		p.backoff.clear(k)
		return false
	default:
		if err := registerAwaiterCore(ctx, p.db, p.dialect, p.logger,
			k.tenantID.String(), k.workflowID, k.eventType, k.key1, k.key2, k.key3); err != nil {
			p.logger.Error("event-triggers: reconcile: could not put the awaiter back after a failed delivery; the waiter's own re-check is now the only way it finds the event",
				"tenant_id", k.tenantID, "workflow_id", k.workflowID, "event_type", k.eventType, "error", err)
		}
		d := p.backoff.failed(k, now)
		p.logger.Warn("event-triggers: reconcile: delivery failed, backing off",
			"tenant_id", k.tenantID, "workflow_id", k.workflowID, "event_type", k.eventType, "retry_in", d)
		return false
	}
}
