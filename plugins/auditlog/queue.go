package auditlog

// The path an audit event takes from the request to the chain, and what happens when the
// database cannot take it (cleat#2168).
//
// The middleware fixes the event's id and timestamp and hands it to a bounded queue. A pool of
// workers appends events to their tenants' chains, retrying a failed append with backoff. An
// event that cannot be recorded is NOT silently dropped: it is counted, logged, and reported to
// the host, and the plugin reports itself unhealthy for a while.
//
// The bounds are what keep the audit log from becoming the API's outage. Nothing waits on the
// database inside a request longer than enqueueWait, however long the database is down; past
// that the event is given up on, loudly. What this does NOT do is survive the process: the queue
// is memory, so a killed process loses what it held. That gap is a durable spool, not a bound,
// and is not built here.
//
// CHAIN ORDER IS NOT TIMESTAMP ORDER. An event's timestamp is when its request finished; its seq
// is when it was appended. With several workers and retries the two can differ by seconds, so seq
// order and timestamp order can disagree. The chain is ordered by seq and nothing that verifies
// or exports it assumes otherwise (TestChainOrderIsNotTimestampOrder pins that).

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cleat-team/cleat/plugin"
	"github.com/google/uuid"
)

// The reasons an event can be lost. They are the label values of the host's counter.
const (
	lossBufferFull   = "buffer_full"   // no room in the queue within enqueue_wait
	lossInsertFailed = "insert_failed" // the database refused it until retry_deadline
	lossShutdown     = "shutdown"      // the process stopped before it could be written
)

// Defaults, used when the config does not set a positive value.
const (
	defaultBufferSize    = 1000
	defaultWorkers       = 4
	defaultEnqueueWait   = time.Second
	defaultRetryDeadline = time.Minute
	defaultShutdownDrain = 10 * time.Second

	// healthWindow is how long the plugin reports unhealthy after losing an event.
	healthWindow = 5 * time.Minute
	// logLossEvery bounds the Error logs per reason: a database outage loses events by the
	// thousand, and a log line for each would be its own outage. The count is exact either way.
	logLossEvery = time.Second
)

// Backoff between attempts. Variables so a test does not wait seconds.
var (
	retryBackoffMin = 100 * time.Millisecond
	retryBackoffMax = 5 * time.Second
)

func (c Config) bufferSize() int {
	if c.BufferSize > 0 {
		return c.BufferSize
	}
	return defaultBufferSize
}

func (c Config) workers() int {
	if c.Workers > 0 {
		return c.Workers
	}
	return defaultWorkers
}

func (c Config) enqueueWait() time.Duration {
	if c.EnqueueWaitMs > 0 {
		return time.Duration(c.EnqueueWaitMs) * time.Millisecond
	}
	return defaultEnqueueWait
}

func (c Config) retryDeadline() time.Duration {
	if c.RetryDeadlineMs > 0 {
		return time.Duration(c.RetryDeadlineMs) * time.Millisecond
	}
	return defaultRetryDeadline
}

func (c Config) shutdownDrain() time.Duration {
	if c.ShutdownDrainMs > 0 {
		return time.Duration(c.ShutdownDrainMs) * time.Millisecond
	}
	return defaultShutdownDrain
}

// queueState is the accounting and shutdown state of the queue. The zero value is ready.
type queueState struct {
	// mu makes "check stopped, then send" atomic against shutdown marking the queue stopped:
	// after stopped is set no send can land, so the final sweep of the buffer is exact.
	mu      sync.RWMutex
	stopped bool

	workers sync.WaitGroup
	// drainDeadline is the unix-nano instant by which shutdown must have finished (0 = not
	// shutting down). A retry never runs past it.
	drainDeadline atomic.Int64

	sent      atomic.Int64 // events handed to enqueueAudit
	recorded  atomic.Int64 // events on their chain, including ones a retry found already there
	lost      [3]atomic.Int64
	lastLoss  atomic.Int64 // unix nano of the latest loss, 0 = none
	lastLog   [3]atomic.Int64
	suppress  [3]atomic.Int64
	lostTotal atomic.Int64
}

var lossReasons = [3]string{lossBufferFull, lossInsertFailed, lossShutdown}

func lossIndex(reason string) int {
	for i, r := range lossReasons {
		if r == reason {
			return i
		}
	}
	return 0
}

// lose counts, logs and reports one event that will never be recorded.
func (p *Plugin) lose(reason string, ev queuedAuditEvent, err error) {
	i := lossIndex(reason)
	p.q.lost[i].Add(1)
	p.q.lostTotal.Add(1)
	now := time.Now()
	p.q.lastLoss.Store(now.UnixNano())
	if p.eventsLost != nil {
		p.eventsLost("audit-log", reason, 1)
	}
	// One Error per reason per logLossEvery, carrying how many were suppressed since. The first
	// loss of an outage is always logged.
	last := p.q.lastLog[i].Load()
	if now.UnixNano()-last < int64(logLossEvery) || !p.q.lastLog[i].CompareAndSwap(last, now.UnixNano()) {
		p.q.suppress[i].Add(1)
		return
	}
	p.logger.Error("audit-log: AUDIT EVENT LOST",
		"reason", reason,
		"also_lost_since_last_log", p.q.suppress[i].Swap(0),
		"tenant", ev.tenantID, "method", ev.method, "path", ev.path, "status", ev.statusCode,
		"error", err)
}

// Health reports the audit log as unhealthy for healthWindow after it last lost an event, and
// healthy otherwise (plugin.HasHealth). A lost audit event is a fact an operator has to see; it
// is not a reason for the host to stop serving.
func (p *Plugin) Health() error {
	last := p.q.lastLoss.Load()
	if last == 0 || time.Since(time.Unix(0, last)) > healthWindow {
		return nil
	}
	return fmt.Errorf("audit-log lost %d event(s) (buffer_full %d, insert_failed %d, shutdown %d); the latest %s ago",
		p.q.lostTotal.Load(), p.q.lost[0].Load(), p.q.lost[1].Load(), p.q.lost[2].Load(),
		time.Since(time.Unix(0, last)).Round(time.Second))
}

var _ plugin.HasHealth = (*Plugin)(nil)

// newQueuedEvent fixes the event's identity and time now, when the request has finished.
func newQueuedEvent(tenantID uuid.UUID, userID, method, path string, statusCode int, ipAddress, userAgent string, duration time.Duration) queuedAuditEvent {
	return queuedAuditEvent{
		id: uuid.New(), ts: time.Now().UTC().Truncate(time.Microsecond),
		tenantID: tenantID, userID: userID, method: method, path: path, statusCode: statusCode,
		ipAddress: ipAddress, userAgent: userAgent, duration: duration,
	}
}

// enqueueAudit hands an event to the queue. If there is no queue (a Plugin built directly, not
// through Init) it writes it now, once. If the queue is full it waits up to enqueue_wait for
// room, and then gives the event up as lost: a request never waits on the audit log longer than
// that, however long the database is down.
func (p *Plugin) enqueueAudit(tenantID uuid.UUID, userID, method, path string, statusCode int, ipAddress, userAgent string, duration time.Duration) {
	ev := newQueuedEvent(tenantID, userID, method, path, statusCode, ipAddress, userAgent, duration)
	p.q.sent.Add(1)
	if p.buffer == nil {
		p.recordOnce(ev)
		return
	}
	p.q.mu.RLock()
	defer p.q.mu.RUnlock()
	if p.q.stopped {
		p.lose(lossShutdown, ev, errors.New("the audit log has stopped"))
		return
	}
	select {
	case p.buffer <- ev:
		return
	default:
	}
	timer := time.NewTimer(p.config.enqueueWait())
	defer timer.Stop()
	select {
	case p.buffer <- ev:
	case <-timer.C:
		p.lose(lossBufferFull, ev, fmt.Errorf("the queue (%d) stayed full for %s", cap(p.buffer), p.config.enqueueWait()))
	}
}

// appendEvent makes one attempt to put ev on its tenant's chain.
func (p *Plugin) appendEvent(ev queuedAuditEvent, retry bool) error {
	if p.db == nil {
		return errors.New("audit-log: no database")
	}
	// A background context with a timeout, so the attempt does not hang and an event is written
	// even if the request it describes was cancelled. THE TENANT HAS TO BE PUT BACK: deriving from
	// context.Background() discards it, and audit_events has a row-level policy that refuses an
	// insert with no tenant (cleat#1278). plugin.ForTenant is the API for that; NOT
	// plugin.AcrossAllTenants, which would pass every test here and disable isolation for every
	// audit write.
	ctx, cancel := context.WithTimeout(plugin.ForTenant(context.Background(), ev.tenantID), 5*time.Second)
	defer cancel()
	return p.appendChained(ctx, chainEvent{
		tenantID: ev.tenantID, userID: ev.userID, method: ev.method, path: ev.path,
		statusCode: ev.statusCode, ipAddress: ev.ipAddress, userAgent: ev.userAgent,
		durationMs: int(ev.duration.Milliseconds()), id: ev.id, ts: ev.ts, retry: retry,
	})
}

// recordOnce is the queue-less path: one attempt, and a failure is counted and logged, not swallowed.
func (p *Plugin) recordOnce(ev queuedAuditEvent) {
	if p.db == nil {
		return
	}
	if err := p.appendEvent(ev, false); err != nil {
		p.lose(lossInsertFailed, ev, err)
		return
	}
	p.q.recorded.Add(1)
}

// limit is the latest instant an event may still be retried: its own deadline, or the end of the
// shutdown drain if that is sooner.
func (p *Plugin) limit(giveUp time.Time) (time.Time, bool) {
	if d := p.q.drainDeadline.Load(); d != 0 {
		if dl := time.Unix(0, d); dl.Before(giveUp) {
			return dl, true
		}
	}
	return giveUp, false
}

// persist records ev, retrying a failed append with jittered backoff until retry_deadline (or the
// end of the shutdown drain), and counts the event lost if it never lands. Nothing else drops it.
func (p *Plugin) persist(ev queuedAuditEvent) {
	giveUp := time.Now().Add(p.config.retryDeadline())
	backoff := retryBackoffMin
	var lastErr error
	for attempt := 1; ; attempt++ {
		err := p.appendEvent(ev, attempt > 1)
		if err == nil || errors.Is(err, errAlreadyRecorded) {
			p.q.recorded.Add(1)
			return
		}
		lastErr = err
		if attempt == 1 {
			p.logger.Warn("audit-log: could not record an event, retrying",
				"tenant", ev.tenantID, "path", ev.path, "error", err)
		}
		wait := backoff/2 + time.Duration(rand.Int63n(int64(backoff/2)+1))
		if lim, shutdown := p.limit(giveUp); time.Now().Add(wait).After(lim) {
			reason := lossInsertFailed
			if shutdown {
				reason = lossShutdown
			}
			p.lose(reason, ev, lastErr)
			return
		}
		time.Sleep(wait)
		if backoff *= 2; backoff > retryBackoffMax {
			backoff = retryBackoffMax
		}
	}
}

// persistGuarded is persist with a panic turned into a counted loss. A panic in the database driver
// or a hook would otherwise end the worker goroutine, shrinking the pool by one and dropping the
// event in hand without a count, which is the silent loss this file exists to prevent.
func (p *Plugin) persistGuarded(ev queuedAuditEvent) {
	defer func() {
		if r := recover(); r != nil {
			p.lose(lossInsertFailed, ev, fmt.Errorf("panic while recording: %v", r))
		}
	}()
	p.persist(ev)
}

// startWorkers starts the pool. Each worker appends one event at a time; different tenants append
// in parallel, and one tenant's appends queue on its head row, which is what keeps the chain
// unforked.
func (p *Plugin) startWorkers(ctx context.Context) {
	for i := 0; i < p.config.workers(); i++ {
		p.q.workers.Add(1)
		go func() {
			defer p.q.workers.Done()
			// The backstop for a panic outside persistGuarded: it must not take the worker process
			// down with it. persistGuarded is what keeps the pool at full size and the event counted.
			plugin.RecoverGoroutine("audit-log", nil, func() {
				for {
					select {
					case ev := <-p.buffer:
						p.persistGuarded(ev)
					case <-ctx.Done():
						return
					}
				}
			})
		}()
	}
}

// shutdown finishes the queue when the plugin is asked to stop: workers stop taking new events,
// what is buffered is drained by the pool within shutdown_drain, and whatever is left after that is
// counted lost. After it returns no event can be enqueued.
func (p *Plugin) shutdown() {
	deadline := time.Now().Add(p.config.shutdownDrain())
	p.q.drainDeadline.Store(deadline.UnixNano())
	p.q.workers.Wait()

	var wg sync.WaitGroup
	for i := 0; i < p.config.workers(); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			plugin.RecoverGoroutine("audit-log", nil, func() {
				for time.Now().Before(deadline) {
					select {
					case ev := <-p.buffer:
						p.persistGuarded(ev)
					default:
						return
					}
				}
			})
		}()
	}
	wg.Wait()

	// No send can land after this, so what is in the buffer now is exactly what is left.
	p.q.mu.Lock()
	p.q.stopped = true
	p.q.mu.Unlock()
	for {
		select {
		case ev := <-p.buffer:
			p.lose(lossShutdown, ev, errors.New("the process stopped before the event could be written"))
		default:
			return
		}
	}
}
