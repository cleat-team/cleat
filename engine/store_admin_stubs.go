package engine

import (
	"context"
	"errors"
	"fmt"
)

// ErrAdminOpNotImplemented marks an admin operation the store genuinely does
// not implement, as opposed to one that failed.
//
// The distinction is the whole reason it exists: cmd/cleat-worker mapped every
// error from these methods to 500, so "this endpoint was never built" and "the
// database is broken" were the same answer to a caller. AdminForceComplete and
// AdminForceFail are implemented in store_admin.go; AdminReReplay is not, and
// says so with 501 rather than pretending to have tried. What it needs is in
// IMPROVEMENT-PLAN.md 3.20.
var ErrAdminOpNotImplemented = errors.New("not implemented")

func adminNotImplemented(op string) error {
	return fmt.Errorf("admin %s: %w", op, ErrAdminOpNotImplemented)
}

// AdminReReplay replays a workflow's event history for debugging.
func (s *PostgresStore) AdminReReplay(ctx context.Context, workflowID string, generation int64, operator string) error {
	return adminNotImplemented("re-replay")
}

func (s *MySQLStore) AdminReReplay(ctx context.Context, workflowID string, generation int64, operator string) error {
	return adminNotImplemented("re-replay")
}

func (s *MSSQLStore) AdminReReplay(ctx context.Context, workflowID string, generation int64, operator string) error {
	return adminNotImplemented("re-replay")
}
