package auditlog

import (
	"context"
	"fmt"
	"time"

	"github.com/cleat-team/cleat/plugin"
	"github.com/google/uuid"
)

// Run starts the background goroutine. It periodically drains the audit
// event buffer and runs retention cleanup. Returns when ctx is cancelled.
func (p *Plugin) Run(ctx context.Context) error {
	if p.db == nil {
		p.logger.Warn("audit-log: no database, retention cleanup disabled")
		<-ctx.Done()
		return nil
	}

	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	drainTicker := time.NewTicker(1 * time.Second)
	defer drainTicker.Stop()

	defer func() {
		p.drainBuffer()
	}()

	p.logger.Info("audit-log: background started, retention=1h, drain=1s")

	for {
		select {
		case <-ctx.Done():
			p.logger.Info("audit-log: retention cleanup stopped")
			return nil

		case <-ticker.C:
			start := time.Now()
			affected, err := p.cleanupRetention(ctx)
			if err != nil {
				p.logger.Error("audit-log: retention cleanup failed",
					"plugin", p.Info().Name,
					"error", err,
				)
				continue
			}
			p.logger.Info("audit-log: work cycle completed",
				"plugin", p.Info().Name,
				"duration_ms", time.Since(start).Milliseconds(),
				"deleted_events", affected,
			)

		case <-drainTicker.C:
			p.drainBuffer()
		}
	}
}

// drainBuffer drains queued audit events from the buffer and records them.
// It processes up to 100 events per call and returns early when the
// buffer is empty.
func (p *Plugin) drainBuffer() {
	if p.buffer == nil {
		return
	}
	for i := 0; i < 100; i++ {
		select {
		case evt := <-p.buffer:
			p.recordAudit(context.Background(),
				evt.tenantID, evt.userID, evt.method, evt.path,
				evt.statusCode, evt.ipAddress, evt.userAgent,
				evt.duration,
			)
		default:
			return
		}
	}
}

// cleanupRetention deletes audit events older than the configured retention period,
// one tenant at a time, and records what it removed from each chain (chain_retention.go).
// Returns the number of deleted events.
//
// The DELETEs are per tenant because retention is a fact about a tenant's chain: the head
// row's lock and the floor it records are that tenant's, and a statement across tenants
// could hold neither. Only the enumeration crosses tenants (expiredTenants), and it reads
// a list of ids, never a row.
func (p *Plugin) cleanupRetention(ctx context.Context) (int64, error) {
	retention := time.Duration(p.config.RetentionDays) * 24 * time.Hour
	now := time.Now
	if p.now != nil {
		now = p.now
	}
	cutoff := now().Add(-retention)

	tenants, err := p.expiredTenants(ctx, cutoff)
	if err != nil {
		return 0, fmt.Errorf("audit retention: %w", err)
	}
	var total int64
	var firstErr error
	for _, raw := range tenants {
		tid, err := uuid.Parse(raw)
		if err != nil {
			continue
		}
		n, err := p.retainTenant(ctx, tid, cutoff)
		total += n
		if err != nil {
			// One tenant's failure must not stop the others being swept.
			p.logger.Error("audit-log: retention for a tenant failed", "tenant", tid, "error", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	if total > 0 {
		p.logger.Info("audit-log: deleted expired events", "count", total, "cutoff", cutoff)
	}
	return total, firstErr
}

// expiredTenants lists the tenants that have at least one row older than cutoff. It asks
// the audit table, not the tenant registry: retention used to purge by age whatever the
// tenant's state, and a tenant removed from the registry leaves its audit rows behind
// (there is no foreign key), so enumerating registered tenants would keep those forever.
// It also visits only tenants with something to remove.
func (p *Plugin) expiredTenants(ctx context.Context, cutoff time.Time) ([]string, error) {
	col := "tenant_id"
	if p.dialect == plugin.DialectMSSQL {
		col = "CONVERT(varchar(36), tenant_id)"
	}
	ctx = plugin.AcrossAllTenants(ctx, "audit retention: list the tenants that have expired rows")
	rows, err := p.db.Query(ctx, plugin.Rebind(fmt.Sprintf(
		`SELECT DISTINCT %s FROM audit_events WHERE %s < $1`,
		col, epochMicrosExpr(p.dialect, "timestamp")), p.dialect), cutoff.UTC().UnixMicro())
	if err != nil {
		return nil, fmt.Errorf("list tenants with expired rows: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("list tenants with expired rows: scan: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
