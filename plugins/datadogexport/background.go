package datadogexport

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/cleat-team/cleat/plugin"
	"github.com/google/uuid"
)

// Lease name for leader election. The plugin with an active lease is the sole
// worker that exports metrics to Datadog.
const leaseName = "datadog-export"

// leaseSQL holds dialect-specific SQL statements for leader election via the
// plugin_lease table. Default (PostgreSQL) uses $N placeholders; MySQL and
// MSSQL variants use dialect-appropriate placeholders and time functions.
var leaseSQL = struct {
	renewIfHolder plugin.Query // UPDATE expires_at WHERE name=$1 AND holder=$2
	grabExpired   plugin.Query // UPDATE holder,expires_at WHERE name=$1 AND expires_at<now()
	insertNew     plugin.Query // INSERT INTO plugin_lease (name, holder, expires_at)
	checkLeader   plugin.Query // SELECT 1 WHERE name=$1 AND holder=$2 AND expires_at>now()
}{
	renewIfHolder: plugin.Query{
		Default: `UPDATE plugin_lease SET expires_at = now() + interval '50 seconds' WHERE name = $1 AND holder = $2`,
		MySQL:   `UPDATE plugin_lease SET expires_at = DATE_ADD(now(), INTERVAL 50 SECOND) WHERE name = $1 AND holder = $2`,
		MSSQL:   `UPDATE plugin_lease SET expires_at = DATEADD(second, 50, now()) WHERE name = $1 AND holder = $2`,
	},
	grabExpired: plugin.Query{
		Default: `UPDATE plugin_lease SET holder = $1, expires_at = now() + interval '50 seconds' WHERE name = $2 AND expires_at < now()`,
		MySQL:   `UPDATE plugin_lease SET holder = $1, expires_at = DATE_ADD(now(), INTERVAL 50 SECOND) WHERE name = $2 AND expires_at < now()`,
		MSSQL:   `UPDATE plugin_lease SET holder = $1, expires_at = DATEADD(second, 50, now()) WHERE name = $2 AND expires_at < now()`,
	},
	insertNew: plugin.Query{
		Default: `INSERT INTO plugin_lease (name, holder, expires_at) VALUES ($1, $2, now() + interval '50 seconds')`,
		MySQL:   `INSERT INTO plugin_lease (name, holder, expires_at) VALUES ($1, $2, DATE_ADD(now(), INTERVAL 50 SECOND))`,
		MSSQL:   `INSERT INTO plugin_lease (name, holder, expires_at) VALUES ($1, $2, DATEADD(second, 50, now()))`,
	},
	checkLeader: plugin.Query{
		Default: `SELECT 1 FROM plugin_lease WHERE name = $1 AND holder = $2 AND expires_at > now()`,
		MySQL:   `SELECT 1 FROM plugin_lease WHERE name = $1 AND holder = $2 AND expires_at > now()`,
		MSSQL:   `SELECT 1 FROM plugin_lease WHERE name = $1 AND holder = $2 AND expires_at > now()`,
	},
}

// Run starts the background metric export loop. Leader election is performed
// via the plugin_lease table: every 30 seconds the worker tries to acquire or
// renew the lease. Only the leader (the worker holding the lease) exports
// metrics every 60 seconds. Returns when ctx is cancelled.
func (p *Plugin) Run(ctx context.Context) error {
	if p.db == nil {
		p.logger.Warn("datadog-export: no database, export disabled")
		<-ctx.Done()
		return nil
	}

	// Attempt to acquire the lease immediately on startup.
	p.tryAcquireOrRenewLease(ctx)

	leaseTicker := time.NewTicker(30 * time.Second)
	defer leaseTicker.Stop()

	exportTicker := time.NewTicker(60 * time.Second)
	defer exportTicker.Stop()

	p.logger.Info("datadog-export: metric export started, interval=60s")

	for {
		select {
		case <-ctx.Done():
			p.logger.Info("datadog-export: metric export stopped")
			return nil

		case <-leaseTicker.C:
			p.tryAcquireOrRenewLease(ctx)

		case <-exportTicker.C:
			if p.isLeader(ctx) {
				if err := p.exportMetrics(ctx); err != nil {
					p.logger.Error("datadog-export: export failed", "error", err)
				}
			}
		}
	}
}

// ---- types for metric export ----

// ddConfigRow represents an enabled Datadog configuration from the database.
type ddConfigRow struct {
	ID            uuid.UUID
	TenantID      uuid.UUID
	APIKey        plugin.Secret
	Site          string
	MetricsPrefix string
}

// statusCount represents a workflow status count from the database query.
type statusCount struct {
	Status string
	Count  int
}

// metricPoint represents a single data point in a Datadog metric series.
type metricPoint struct {
	Timestamp int64   `json:"timestamp"`
	Value     float64 `json:"value"`
}

// metricSeries represents a Datadog metric series.
type metricSeries struct {
	Metric string        `json:"metric"`
	Type   string        `json:"type"`
	Points []metricPoint `json:"points"`
	Tags   []string      `json:"tags"`
}

// ddSeriesPayload is the request body for the Datadog Metrics API.
type ddSeriesPayload struct {
	Series []metricSeries `json:"series"`
}

// ---- leader election via plugin_lease ----

// tryAcquireOrRenewLease attempts to become the leader or renew the current
// lease. The algorithm is:
//
//  1. Try to renew if we are already the holder
//     (UPDATE ... WHERE name=$1 AND holder=$2)
//  2. If that fails, try to grab an expired lease
//     (UPDATE ... WHERE name=$1 AND expires_at < now())
//  3. If that also fails, try inserting a brand new lease
//     (INSERT ...)
//
// Errors are logged but never returned — the worker must not crash from a
// lease failure.
func (p *Plugin) tryAcquireOrRenewLease(ctx context.Context) {
	// 1. Try to renew if we are already the holder.
	n, err := p.db.Exec(ctx, plugin.Rebind(leaseSQL.renewIfHolder.For(p.dialect), p.dialect), leaseName, p.workerID)
	if err == nil && n > 0 {
		p.logger.Debug("datadog-export: lease renewed", "worker", p.workerID)
		return
	}

	// 2. Try to grab an expired lease.
	n, err = p.db.Exec(ctx, plugin.Rebind(leaseSQL.grabExpired.For(p.dialect), p.dialect), p.workerID, leaseName)
	if err == nil && n > 0 {
		p.logger.Debug("datadog-export: acquired expired lease", "worker", p.workerID)
		return
	}

	// 3. Try to insert a brand new lease (first worker).
	_, err = p.db.Exec(ctx, plugin.Rebind(leaseSQL.insertNew.For(p.dialect), p.dialect), leaseName, p.workerID)
	if err == nil {
		p.logger.Debug("datadog-export: created new lease", "worker", p.workerID)
		return
	}

	// If we get here, another worker holds the active lease.
	p.logger.Debug("datadog-export: lease held by another worker", "worker", p.workerID)
}

// isLeader checks whether this worker currently holds a valid lease.
func (p *Plugin) isLeader(ctx context.Context) bool {
	row := p.db.QueryRow(ctx, plugin.Rebind(leaseSQL.checkLeader.For(p.dialect), p.dialect), leaseName, p.workerID)
	var one int
	if err := row.Scan(&one); err != nil {
		return false
	}
	return one == 1
}

// exportMetrics queries all enabled Datadog configs and exports workflow
// metrics for each one. Errors for individual configs are logged but do not
// prevent other configs from being processed.
func (p *Plugin) exportMetrics(ctx context.Context) error {
	// A NAMED cross-tenant read. cleat#1278.
	//
	// This query's whole job is to discover WHICH tenants have an export
	// configured, so it cannot be scoped to one -- there is no tenant to scope
	// it to until after it returns. Once dd_config carries a row-level policy
	// (migrations.go v3) an unnamed statement here is refused with
	// "cleat.tenant_id is not set", so adding the policy and leaving this bare
	// are the same change.
	//
	// The scope ends here deliberately: exportForConfig below re-narrows to one
	// tenant with plugin.ForTenant rather than inheriting this bypass.
	// A SEPARATE VARIABLE, NOT `ctx =`. Reassigning would carry the bypass into
	// exportForConfig below, where plugin.ForTenant would be silently ignored --
	// beginTenantTx tests CrossTenant first, so a bypass in scope wins. That is
	// the hazard ForTenant's own doc comment names, and the first draft of this
	// function had it: `ctx =`, then `exportForConfig(ctx, cfg)`, with a comment
	// in exportForConfig claiming it received the unmarked parent. It did not.
	//
	// The narrow scope is the point: the bypass covers the discovery query and
	// nothing else.
	discoverCtx := plugin.AcrossAllTenants(ctx, "datadog-export: discovering which tenants have an export configured")

	rows, err := p.db.Query(discoverCtx, `
			SELECT id, tenant_id, api_key, site, metrics_prefix
			FROM dd_config
			WHERE enabled = true
		`)
	if err != nil {
		return fmt.Errorf("query enabled configs: %w", err)
	}
	defer rows.Close()

	var configs []ddConfigRow
	for rows.Next() {
		var cfg ddConfigRow
		if err := plugin.ScanRow(rows, &cfg.ID, &cfg.TenantID, &cfg.APIKey, &cfg.Site, &cfg.MetricsPrefix); err != nil {
			p.logger.Error("datadog-export: scan config row", "error", err)
			continue
		}
		configs = append(configs, cfg)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate configs: %w", err)
	}

	for _, cfg := range configs {
		if err := p.exportForConfig(ctx, cfg); err != nil {
			p.logger.Error("datadog-export: config export failed",
				"config_id", cfg.ID, "tenant", cfg.TenantID, "error", err)
		}
	}

	return nil
}

// exportForConfig queries workflow statistics for a single tenant and sends
// them as gauge metrics to the Datadog Metrics API.
func (p *Plugin) exportForConfig(ctx context.Context, cfg ddConfigRow) error {
	// NARROWED TO ONE TENANT, not inheriting the caller's bypass. cleat#1278.
	//
	// cfg.TenantID is in hand, so this is the ForTenant case rather than the
	// AcrossAllTenants one: the statement below reads ONE tenant's rows and
	// should be scoped to say so.
	//
	// IT IS NOT COSMETIC. workflow_instances is a CORE table whose policy is
	// `tenant_id = cleat.assert_tenant_set()`, which RAISES when no tenant is
	// set -- so this read survives today only because the worker's plugin pool
	// connects as an owner or superuser, which PostgreSQL waves past a policy.
	// Point that pool at a non-superuser and the query fails; exportMetrics
	// logs the error and continues, so the symptom is no metrics, which is
	// indistinguishable from no enabled configs. Same shape as cleat#958, where
	// an empty audit table read as a quiet system.
	//
	// The WHERE tenant_id = $1 below stays. It is the same value by a second
	// route, and a scoped connection plus an explicit predicate disagree only
	// if something is wrong.
	//
	// A BYPASS ALREADY IN SCOPE WOULD WIN AND THIS WOULD BE INERT -- see
	// plugin.ForTenant. The caller marks cross-tenant for its discovery query;
	// it passes the UNMARKED parent ctx here for exactly that reason.
	ctx = plugin.ForTenant(ctx, cfg.TenantID)

	// Query workflow counts by status for this tenant.
	statusRows, err := p.db.Query(ctx, plugin.Rebind(`
			SELECT status, COUNT(*) AS count
			FROM workflow_instances
			WHERE tenant_id = $1
			GROUP BY status
		`, p.dialect), cfg.TenantID)
	if err != nil {
		return fmt.Errorf("query workflow stats: %w", err)
	}
	defer statusRows.Close()

	var counts []statusCount
	for statusRows.Next() {
		var sc statusCount
		if err := statusRows.Scan(&sc.Status, &sc.Count); err != nil {
			p.logger.Error("datadog-export: scan status count", "error", err)
			continue
		}
		counts = append(counts, sc)
	}
	if err := statusRows.Err(); err != nil {
		return fmt.Errorf("iterate status counts: %w", err)
	}

	now := time.Now().Unix()

	// Build series from status counts.
	var total int
	series := make([]metricSeries, 0, len(counts)+1)
	for _, sc := range counts {
		total += sc.Count
		series = append(series, metricSeries{
			Metric: cfg.MetricsPrefix + ".workflows." + sc.Status,
			Type:   "gauge",
			Points: []metricPoint{{Timestamp: now, Value: float64(sc.Count)}},
			Tags:   []string{"tenant:" + cfg.TenantID.String()},
		})
	}

	// Add total metric.
	series = append(series, metricSeries{
		Metric: cfg.MetricsPrefix + ".workflows.total",
		Type:   "gauge",
		Points: []metricPoint{{Timestamp: now, Value: float64(total)}},
		Tags:   []string{"tenant:" + cfg.TenantID.String()},
	})

	payload := ddSeriesPayload{Series: series}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}

	// POST to the Datadog Metrics API.
	site := cfg.Site
	if site == "" {
		site = "datadoghq.com"
	}
	url := fmt.Sprintf("https://api.%s/api/v1/series", site)

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("DD-API-KEY", cfg.APIKey.Reveal())
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("datadog API returned status %d", resp.StatusCode)
	}

	p.logger.Info("datadog-export: metrics exported",
		"config_id", cfg.ID,
		"tenant", cfg.TenantID,
		"series_count", len(series),
	)

	return nil
}
