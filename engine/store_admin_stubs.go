package engine

import (
	"context"
	"fmt"
)

// AdminForceComplete marks a workflow as done, bypassing worker ownership.
func (s *PostgresStore) AdminForceComplete(ctx context.Context, workflowID string, generation int64, result string, operator string) error {
	return fmt.Errorf("admin force-complete: not implemented yet")
}

// AdminForceFail marks a workflow as failed, bypassing worker ownership.
func (s *PostgresStore) AdminForceFail(ctx context.Context, workflowID string, generation int64, errorMsg, errorCode string, operator string) error {
	return fmt.Errorf("admin force-fail: not implemented yet")
}

// AdminReReplay replays a workflow's event history for debugging.
func (s *PostgresStore) AdminReReplay(ctx context.Context, workflowID string, generation int64, operator string) error {
	return fmt.Errorf("admin re-replay: not implemented yet")
}

// Stubs for MySQL store.
func (s *MySQLStore) AdminForceComplete(ctx context.Context, workflowID string, generation int64, result string, operator string) error {
	return fmt.Errorf("admin force-complete: not implemented yet")
}
func (s *MySQLStore) AdminForceFail(ctx context.Context, workflowID string, generation int64, errorMsg, errorCode string, operator string) error {
	return fmt.Errorf("admin force-fail: not implemented yet")
}
func (s *MySQLStore) AdminReReplay(ctx context.Context, workflowID string, generation int64, operator string) error {
	return fmt.Errorf("admin re-replay: not implemented yet")
}

// Stubs for MSSQL store.
func (s *MSSQLStore) AdminForceComplete(ctx context.Context, workflowID string, generation int64, result string, operator string) error {
	return fmt.Errorf("admin force-complete: not implemented yet")
}
func (s *MSSQLStore) AdminForceFail(ctx context.Context, workflowID string, generation int64, errorMsg, errorCode string, operator string) error {
	return fmt.Errorf("admin force-fail: not implemented yet")
}
func (s *MSSQLStore) AdminReReplay(ctx context.Context, workflowID string, generation int64, operator string) error {
	return fmt.Errorf("admin re-replay: not implemented yet")
}
