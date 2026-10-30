package engine

// cleat#3600: the record of each use of the plugin-dependency override.
//
// The override (--allow-missing-plugins, or allow_missing_plugins on the REST body) lets a deploy
// go past the check, so each use is a decision someone should be able to find afterwards, and a
// log line is not that: logs rotate and are not queryable by tenant or definition. A use writes a
// row to admin.plugin_dep_overrides BEFORE the definition is written, and a deploy whose row
// cannot be written is refused. There is no override without a record of it.
//
// The row says the check was bypassed for that attempt and for which plugin names. It does not say
// the deploy then succeeded: a later refusal (open runs on the version, say) leaves the row.
//
// Not the audit-log plugin. That is request-level middleware with a bounded queue that can lose an
// event, and it carries no missing names; this is a durable row with them, append-only by
// privilege on PostgreSQL and SQL Server.

import (
	"context"
	"encoding/json"
	"fmt"
	"os/user"
	"strings"

	"github.com/google/uuid"
)

// PluginDepOverride is one use of the override.
type PluginDepOverride struct {
	// TenantID is the tenant the definition is deployed for; the default tenant's id for a
	// command-line deploy.
	TenantID   string
	Definition string
	Version    int
	// Via names the path that took the override: "rest", "cleatctl", "deploy-workflow" or
	// "cleat-deploy".
	Via string
	// Actor is who asked, as the path knows them: the authenticated subject over REST, the
	// operating-system user for a command.
	Actor string
	// Missing is what the override went past. Set by EnforcePluginDeps, not by the caller.
	Missing []MissingPlugin
}

// OverrideRecorder writes the record of one use of the override.
type OverrideRecorder interface {
	RecordPluginDepOverride(ctx context.Context, o PluginDepOverride) error
}

// PluginRegistryAccess is what a deploy path needs of the plugin-name registry: to ask which names
// are known and to record an override. *PluginNameRegistry is the database-backed one.
type PluginRegistryAccess interface {
	PluginSource
	OverrideRecorder
}

var _ PluginRegistryAccess = (*PluginNameRegistry)(nil)

type overrideMissingJSON struct {
	Name       string `json:"name"`
	Constraint string `json:"constraint"`
	DeclaredBy string `json:"declared_by"`
}

// RecordPluginDepOverride implements OverrideRecorder.
func (r *PluginNameRegistry) RecordPluginDepOverride(ctx context.Context, o PluginDepOverride) error {
	return recordOverrideOn(ctx, r.DB, r.Dialect, o)
}

func recordOverrideOn(ctx context.Context, q querier, d Dialect, o PluginDepOverride) error {
	if len(o.Missing) == 0 {
		return fmt.Errorf("plugin dependency override: nothing was missing, so there is nothing to record")
	}
	missing := make([]overrideMissingJSON, len(o.Missing))
	for i, m := range o.Missing {
		missing[i] = overrideMissingJSON(m)
	}
	raw, err := json.Marshal(missing)
	if err != nil {
		return fmt.Errorf("plugin dependency override: %w", err)
	}
	ph := make([]string, 7)
	for i := range ph {
		ph[i] = d.placeholder(i + 1)
	}
	stmt := "INSERT INTO " + d.pluginDepOverridesTable() +
		" (override_id, tenant_id, definition_name, definition_version, via, actor, missing_plugins) VALUES (" +
		strings.Join(ph, ", ") + ")"
	if _, err := q.ExecContext(ctx, stmt, uuid.NewString(), o.TenantID, o.Definition, o.Version, o.Via, o.Actor, string(raw)); err != nil {
		return fmt.Errorf("plugin dependency override: record: %w", err)
	}
	return nil
}

func (d Dialect) pluginDepOverridesTable() string {
	if d == DialectMySQL {
		return "plugin_dep_overrides"
	}
	return "admin.plugin_dep_overrides"
}

// CommandLineActor is who a command-line deploy records as having asked for an override: the
// operating-system user, or "unknown" when that cannot be read. Best effort and not an
// authentication; a REST deploy records its authenticated subject instead.
func CommandLineActor() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return "unknown"
}

// RegistrySplit separates a possibly-nil registry into the two interfaces EnforceOptions takes, so
// that a nil registry is a nil source and a nil recorder and never a non-nil interface wrapping a
// nil pointer, which would defeat the "cannot check" and "cannot record" refusals.
func RegistrySplit(reg PluginRegistryAccess) (PluginSource, OverrideRecorder) {
	if reg == nil {
		return nil, nil
	}
	return reg, reg
}
