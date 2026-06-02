package plugin

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Audit event types
// ---------------------------------------------------------------------------

// AuditEventType classifies plugin audit log entries.
type AuditEventType string

const (
	AuditPluginDeploy          AuditEventType = "plugin_deploy"
	AuditPluginDeprecate       AuditEventType = "plugin_deprecate"
	AuditPluginCapabilityChange AuditEventType = "plugin_capability_change"
	AuditPluginInvocation      AuditEventType = "plugin_invocation"
)

// AuditEntry represents a single row in the plugin_audit_log table.
type AuditEntry struct {
	ID            int64          `json:"id"`
	PluginName    string         `json:"plugin_name"`
	PluginVersion string         `json:"plugin_version,omitempty"`
	EventType     AuditEventType `json:"event_type"`
	Details       string         `json:"details,omitempty"`
	CreatedAt     time.Time      `json:"created_at"`
}

// ---------------------------------------------------------------------------
// SQL for the plugin_audit_log table
// ---------------------------------------------------------------------------

// CreatePluginAuditLogTableSQL returns the PostgreSQL DDL for the
// plugin_audit_log table and its indexes.
func CreatePluginAuditLogTableSQL() string {
	return `
	CREATE TABLE IF NOT EXISTS plugin_audit_log (
		id              BIGSERIAL PRIMARY KEY,
		plugin_name     TEXT NOT NULL,
		plugin_version  TEXT NOT NULL DEFAULT '',
		event_type      TEXT NOT NULL,
		details         TEXT NOT NULL DEFAULT '',
		created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
	);

	CREATE INDEX IF NOT EXISTS idx_plugin_audit_log_plugin_name
		ON plugin_audit_log (plugin_name);

	CREATE INDEX IF NOT EXISTS idx_plugin_audit_log_event_type
		ON plugin_audit_log (event_type);

	CREATE INDEX IF NOT EXISTS idx_plugin_audit_log_created_at
		ON plugin_audit_log (created_at);
	`
}

// DropPluginAuditLogTableSQL returns the PostgreSQL DDL for dropping the
// plugin_audit_log table (used in rollback).
func DropPluginAuditLogTableSQL() string {
	return `DROP TABLE IF EXISTS plugin_audit_log CASCADE;`
}

// ---------------------------------------------------------------------------
// AuditLog — concrete AuditLogger implementation
// ---------------------------------------------------------------------------

// AuditLog provides methods for writing audit events to the plugin_audit_log
// table. It implements the AuditLogger interface.
type AuditLog struct {
	db *sql.DB
	rng *rand.Rand
	mu  sync.Mutex
}

// NewAuditLog creates a new AuditLog backed by the given database connection.
func NewAuditLog(db *sql.DB) *AuditLog {
	return &AuditLog{
		db:  db,
		rng: rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

// Deploy records a plugin deployment audit event.
func (a *AuditLog) Deploy(ctx context.Context, pluginName, pluginVersion string) error {
	return a.write(ctx, pluginName, pluginVersion, AuditPluginDeploy, "plugin deployed")
}

// Deprecate records a plugin deprecation audit event.
func (a *AuditLog) Deprecate(ctx context.Context, pluginName, pluginVersion string) error {
	return a.write(ctx, pluginName, pluginVersion, AuditPluginDeprecate, "plugin deprecated")
}

// CapabilityChange records a change in plugin capabilities.
func (a *AuditLog) CapabilityChange(ctx context.Context, pluginName, pluginVersion, details string) error {
	return a.write(ctx, pluginName, pluginVersion, AuditPluginCapabilityChange, details)
}

// Invocation records a plugin function invocation audit event.
// To avoid unbounded growth, invocations are sampled: dropRate is the
// probability (0.0-1.0) of dropping the event. A dropRate of 0.995 means
// roughly 1 in 200 invocations are logged.
func (a *AuditLog) Invocation(ctx context.Context, pluginName, functionName string, dropRate float64) error {
	if dropRate > 0.0 {
		a.mu.Lock()
		sample := a.rng.Float64()
		a.mu.Unlock()
		if sample < dropRate {
			return nil
		}
	}
	return a.write(ctx, pluginName, "", AuditPluginInvocation,
		fmt.Sprintf("function=%s", functionName))
}

// ---------------------------------------------------------------------------
// AuditLogger implementation — RetentionPolicy as interface{}
// ---------------------------------------------------------------------------

// EnforceRetention deletes audit log entries that exceed the given policy.
// The policy parameter must be a RetentionPolicy value. Returns the number
// of rows deleted.
func (a *AuditLog) EnforceRetention(ctx context.Context, policy interface{}) (int64, error) {
	p, ok := policy.(RetentionPolicy)
	if !ok {
		return 0, fmt.Errorf("plugin audit: expected RetentionPolicy, got %T", policy)
	}
	return a.enforceRetention(ctx, p)
}

// enforceRetention is the typed implementation.
func (a *AuditLog) enforceRetention(ctx context.Context, policy RetentionPolicy) (int64, error) {
	var total int64

	if policy.MaxAge > 0 {
		result, err := a.db.ExecContext(ctx, `
			DELETE FROM plugin_audit_log
			WHERE created_at < $1
		`, time.Now().Add(-policy.MaxAge))
		if err != nil {
			return total, fmt.Errorf("plugin audit retention by age: %w", err)
		}
		n, _ := result.RowsAffected()
		total += n
	}

	if policy.MaxEntriesPerPlugin > 0 {
		result, err := a.db.ExecContext(ctx, `
			DELETE FROM plugin_audit_log
			WHERE (plugin_name, created_at) IN (
				SELECT plugin_name, created_at FROM (
					SELECT plugin_name, created_at,
						ROW_NUMBER() OVER (PARTITION BY plugin_name ORDER BY created_at DESC) AS rn
					FROM plugin_audit_log
				) ranked
				WHERE ranked.rn > $1
			)
		`, policy.MaxEntriesPerPlugin)
		if err != nil {
			return total, fmt.Errorf("plugin audit retention by count: %w", err)
		}
		n, _ := result.RowsAffected()
		total += n
	}

	return total, nil
}

// ---------------------------------------------------------------------------
// Retention policy
// ---------------------------------------------------------------------------

// RetentionPolicy configures automatic cleanup of old audit log entries.
type RetentionPolicy struct {
	// MaxAge is the maximum age of audit log entries to retain.
	// Entries older than this are deleted during cleanup.
	MaxAge time.Duration

	// MaxEntriesPerPlugin is the maximum number of audit entries to retain
	// per plugin. When exceeded, the oldest entries are deleted.
	// 0 means no per-plugin limit.
	MaxEntriesPerPlugin int
}

// DefaultRetentionPolicy returns a sensible default retention policy.
func DefaultRetentionPolicy() RetentionPolicy {
	return RetentionPolicy{
		MaxAge:             90 * 24 * time.Hour, // 90 days
		MaxEntriesPerPlugin: 10000,
	}
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

func (a *AuditLog) write(ctx context.Context, pluginName, pluginVersion string, eventType AuditEventType, details string) error {
	_, err := a.db.ExecContext(ctx, `
		INSERT INTO plugin_audit_log (plugin_name, plugin_version, event_type, details, created_at)
		VALUES ($1, $2, $3, $4, NOW())
	`, pluginName, pluginVersion, string(eventType), details)
	if err != nil {
		return fmt.Errorf("plugin audit: write %s for %s: %w", eventType, pluginName, err)
	}
	return nil
}
