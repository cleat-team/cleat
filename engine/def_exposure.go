package engine

// cleat#3785. The HTTP layer decides whether a request may reach a definition by its exposure class, and it
// does so on nearly every route that addresses a definition or a run of one. It used to ask GetWorkflowDef,
// which returns the whole row: `wasm_bytes` (19 MB for a Python module, read to learn one short string) and,
// for a library member, a second read of `library.members` for the bytes. Under a loaded database that is
// the read most likely to time out, and it is made for every start.
//
// This is the same question with only the answer selected.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// DefExposureReader reads one definition version's exposure class and nothing else.
//
// found is false for a version that does not exist, which is not an error: callers treat it as "not
// internal" and let the route produce its own miss. An error means the class could not be read, and the
// caller must not treat that as either answer -- it fails closed (cleat#3785).
//
// The class is the stored value, not its default: pass it through ExposureClass.OrDefault, as every reader
// of the row does.
type DefExposureReader interface {
	GetWorkflowDefExposure(ctx context.Context, name string, version int) (class ExposureClass, found bool, err error)
}

var (
	_ DefExposureReader = (*PostgresStore)(nil)
	_ DefExposureReader = (*MySQLStore)(nil)
	_ DefExposureReader = (*MSSQLStore)(nil)
	_ DefExposureReader = (*ShardedStore)(nil)
)

// The statements are constants so a test can say what they select without running them. They name the
// tenant, as GetWorkflowDef does; that is the scope on MySQL, which has no row-level security.
const (
	pgDefExposureSQL    = `SELECT exposure FROM workflow_defs WHERE name = $1 AND version = $2 AND tenant_id = $3`
	mysqlDefExposureSQL = `SELECT exposure FROM workflow_defs WHERE name = ? AND version = ? AND tenant_id = ?`
	mssqlDefExposureSQL = `SELECT exposure FROM workflow_defs WHERE name = @p1 AND version = @p2 AND tenant_id = @p3`
)

// GetWorkflowDefExposure implements DefExposureReader.
func (s *PostgresStore) GetWorkflowDefExposure(ctx context.Context, name string, version int) (ExposureClass, bool, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return "", false, fmt.Errorf("get workflow def exposure: begin: %w", err)
	}
	defer tx.Rollback()

	var class ExposureClass
	err = tx.QueryRowContext(ctx, pgDefExposureSQL, name, version, s.tenantID).Scan(&class)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, tx.Commit()
	}
	if err != nil {
		return "", false, fmt.Errorf("get workflow def exposure: %w", err)
	}
	return class, true, tx.Commit()
}

// GetWorkflowDefExposure implements DefExposureReader.
func (s *MySQLStore) GetWorkflowDefExposure(ctx context.Context, name string, version int) (ExposureClass, bool, error) {
	var class ExposureClass
	err := s.db.QueryRowContext(ctx, mysqlDefExposureSQL, name, version, s.tenantID).Scan(&class)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("get workflow def exposure: %w", err)
	}
	return class, true, nil
}

// GetWorkflowDefExposure implements DefExposureReader.
func (s *MSSQLStore) GetWorkflowDefExposure(ctx context.Context, name string, version int) (ExposureClass, bool, error) {
	// beginTxWithContext, not bare s.db -- cleat#2210, as in GetWorkflowDef.
	tx, err := s.beginTxWithContext(ctx)
	if err != nil {
		return "", false, fmt.Errorf("get workflow def exposure: begin: %w", err)
	}
	defer tx.Rollback()

	var class ExposureClass
	err = tx.QueryRowContext(ctx, mssqlDefExposureSQL, name, version, s.tenantID).Scan(&class)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, tx.Commit()
	}
	if err != nil {
		return "", false, fmt.Errorf("get workflow def exposure: %w", err)
	}
	return class, true, tx.Commit()
}

// GetWorkflowDefExposure implements DefExposureReader, on the shard that holds the definition's name, as
// GetWorkflowDef does. A shard whose store cannot answer the narrow question answers it the wide way: this
// is a read, and the narrow form is an optimisation of it, not a different question.
func (s *ShardedStore) GetWorkflowDefExposure(ctx context.Context, name string, version int) (ExposureClass, bool, error) {
	shard := s.getShard(name)
	if shard == nil {
		return "", false, fmt.Errorf("get_workflow_def_exposure: no shard available -- check shard configuration in CLEAT_SHARD_CONFIG")
	}
	if r, ok := shard.Store.(DefExposureReader); ok {
		return r.GetWorkflowDefExposure(ctx, name, version)
	}
	def, err := shard.Store.GetWorkflowDef(ctx, name, version)
	if err != nil || def == nil {
		return "", false, err
	}
	return def.Exposure, true, nil
}
