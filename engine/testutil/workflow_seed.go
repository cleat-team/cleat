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
// PostgreSQL only, like SeedWorkflowLease. The three inserts commit together:
// given a *sql.DB or *sql.Conn, SeedRun opens a transaction around them, and
// given a *sql.Tx it writes into that one and leaves the commit to the caller.
//
// That is not tidiness. A run is claimable the moment its workflow_leases row
// is visible, and the claim reads def_name from workflow_payloads through a
// LEFT JOIN, so three autocommitted statements let a concurrent claimer take a
// lease whose payload row does not exist yet and fail with "converting NULL to
// string is unsupported" (cleat#3733). StartNewRun never has that window
// because its three rows share a transaction; a fixture should not either.
func SeedRun(t testing.TB, db Execer, seed RunSeed) {
	t.Helper()
	if err := seedRunOn(context.Background(), db, seed); err != nil {
		t.Fatal(err)
	}
}

// seedRunOn is SeedRun's body, returning its error so a test can drive it from
// a goroutine, where t.Fatal is not allowed.
func seedRunOn(ctx context.Context, db Execer, seed RunSeed) error {
	if b, ok := db.(txBeginner); ok {
		tx, err := b.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("SeedRun: begin: %w", err)
		}
		if err := SeedRunErr(ctx, tx, seed); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("SeedRun: committing %s: %w", seed.ID, err)
		}
		return nil
	}
	if tx, ok := db.(*sql.Tx); ok {
		return SeedRunErr(ctx, tx, seed)
	}
	return fmt.Errorf("SeedRun: %T can neither begin a transaction nor is one; the three inserts would not be atomic", db)
}

// txBeginner is what *sql.DB and *sql.Conn both offer and *sql.Tx does not.
type txBeginner interface {
	BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error)
}

// SeedRunErr is SeedRun for code with no *testing.T to fail, such as a
// goroutine in a load test. It returns the first error instead.
//
// It takes a *sql.Tx, not an Execer, and the caller commits. An Execer would
// accept a *sql.DB, which autocommits each of the three inserts and makes the
// run visible half-written -- the defect cleat#3733 found in the soak test
// after it passed the pool here and ran under concurrent claimers.
func SeedRunErr(ctx context.Context, tx *sql.Tx, seed RunSeed) error {
	return insertRunRows(ctx, tx, seed)
}

// insertRunRows is the three inserts, on whatever it is handed. Unexported
// because handing it a *sql.DB is the cleat#3733 defect; it exists so a test
// can reproduce that defect on purpose.
func insertRunRows(ctx context.Context, tx Execer, seed RunSeed) error {
	if seed.ID == "" || seed.DefName == "" {
		return fmt.Errorf("SeedRun: ID and DefName are required, got %+v", seed)
	}
	defVersion := seed.DefVersion
	if defVersion == 0 {
		defVersion = 1
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO workflow_instances (id, tenant_id, parent_close_policy)
		VALUES ($1, COALESCE(NULLIF($2, ''), $3)::uuid, COALESCE(NULLIF($4, ''), 'ABANDON'))`,
		seed.ID, seed.TenantID, defaultLeaseTenantID, seed.ParentClosePolicy,
	); err != nil {
		return fmt.Errorf("seeding %s's workflow_instances row: %w", seed.ID, err)
	}
	if _, err := tx.ExecContext(ctx, `
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
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO workflow_payloads (id, tenant_id, def_name, def_version, input)
		VALUES ($1, COALESCE(NULLIF($2, ''), $6)::uuid, $3, $4, COALESCE(NULLIF($5, ''), '{}')::jsonb)`,
		seed.ID, seed.TenantID, seed.DefName, defVersion, seed.Input, defaultLeaseTenantID,
	); err != nil {
		return fmt.Errorf("seeding %s's workflow_payloads row: %w", seed.ID, err)
	}
	return nil
}
