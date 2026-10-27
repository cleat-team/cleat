package testutil

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

// These three match workflow_leases' own column defaults
// (migrations/postgres/001_schema.sql) exactly -- COALESCE below falls back to
// these literals rather than the table's DEFAULT clause because a column given
// an explicit (even empty-string) argument never reaches its DEFAULT.
// Re-derive if the table's defaults ever change.
const (
	defaultLeaseTenantID  = "00000000-0000-0000-0000-000000000000"
	defaultLeaseTaskQueue = "default"
	defaultLeaseStatus    = "ready"
)

// Execer is what SeedRun writes through: a *sql.DB, *sql.Tx or *sql.Conn.
type Execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// RunSeed describes one run for SeedRun. ID and DefName are required. A
// zero-value field takes the column default: tenant the zero UUID, def
// version 1, input '{}', task queue 'default', status 'ready', generation 0,
// parent close policy 'ABANDON', and NULL for the rest.
//
// Anything else a fixture needs -- heartbeat_at, next_wake_at, completed_at,
// result, error_* and so on -- it sets afterwards with a plain UPDATE on
// workflow_leases or workflow_payloads, whichever owns the column.
type RunSeed struct {
	ID                string
	TenantID          string
	DefName           string
	DefVersion        int
	Input             string // JSON
	TaskQueue         string
	Status            string
	AssignedTo        string
	Generation        int64
	ParentWorkflowID  string
	ParentClosePolicy string
}

// SeedRun inserts one PostgreSQL run the way StartNewRun lays it out: a
// workflow_instances row carrying only what stays there after cleat#3245's
// baseline regeneration, plus the run's workflow_leases and
// workflow_payloads rows. It is meant to be the only place a PostgreSQL
// fixture writes workflow_instances, so that B5 changes one statement when it
// drops the redundant columns.
//
// def_name and def_version are written to workflow_instances as well as to
// workflow_payloads because they are NOT NULL there until B5 drops them.
// That is the one non-narrow write left here, and B5 removes it.
//
// PostgreSQL only, like SeedWorkflowLease. The three inserts are not wrapped
// in a transaction; pass a *sql.Tx to make them atomic.
func SeedRun(t testing.TB, db Execer, seed RunSeed) {
	t.Helper()
	if err := SeedRunErr(context.Background(), db, seed); err != nil {
		t.Fatal(err)
	}
}

// SeedRunErr is SeedRun for code with no *testing.T to fail, such as a
// goroutine in a load test. It returns the first error instead.
func SeedRunErr(ctx context.Context, db Execer, seed RunSeed) error {
	if seed.ID == "" || seed.DefName == "" {
		return fmt.Errorf("SeedRun: ID and DefName are required, got %+v", seed)
	}
	defVersion := seed.DefVersion
	if defVersion == 0 {
		defVersion = 1
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO workflow_instances (id, tenant_id, parent_close_policy)
		VALUES ($1, COALESCE(NULLIF($2, ''), $3)::uuid, COALESCE(NULLIF($4, ''), 'ABANDON'))`,
		seed.ID, seed.TenantID, defaultLeaseTenantID, seed.ParentClosePolicy,
	); err != nil {
		return fmt.Errorf("seeding %s's workflow_instances row: %w", seed.ID, err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO workflow_leases (id, tenant_id, task_queue, status, assigned_to, generation, parent_workflow_id)
		VALUES (
			$1,
			COALESCE(NULLIF($2, ''), $8)::uuid,
			COALESCE(NULLIF($3, ''), $9),
			COALESCE(NULLIF($4, ''), $10),
			NULLIF($5, ''),
			$6,
			NULLIF($7, '')
		)`,
		seed.ID, seed.TenantID, seed.TaskQueue, seed.Status, seed.AssignedTo, seed.Generation,
		seed.ParentWorkflowID, defaultLeaseTenantID, defaultLeaseTaskQueue, defaultLeaseStatus,
	); err != nil {
		return fmt.Errorf("seeding %s's workflow_leases row: %w", seed.ID, err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO workflow_payloads (id, tenant_id, def_name, def_version, input)
		VALUES ($1, COALESCE(NULLIF($2, ''), $6)::uuid, $3, $4, COALESCE(NULLIF($5, ''), '{}')::jsonb)`,
		seed.ID, seed.TenantID, seed.DefName, defVersion, seed.Input, defaultLeaseTenantID,
	); err != nil {
		return fmt.Errorf("seeding %s's workflow_payloads row: %w", seed.ID, err)
	}
	return nil
}
