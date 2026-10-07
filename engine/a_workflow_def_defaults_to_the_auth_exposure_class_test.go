package engine

// cleat#1986. workflow_defs.exposure is a closed, three-valued class --
// 'public', 'auth' or 'internal' -- added by migrations/<dialect>/00[4-5]_
// workflow_defs_exposure_class.sql. Nothing writes it yet (that's the
// analyzer/deploy wiring, a later PR in the same issue), so what this test
// owns is the two things the schema alone has to guarantee on all three
// dialects before any Go code reads or writes the column: a definition
// deployed through today's path gets the secure default, and the database
// itself refuses a value outside the closed set -- not just the application
// code that hasn't been written yet.

import (
	"context"
	"testing"
)

func TestWorkflowDefDefaultsToTheAuthExposureClass(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()

			const defName = "exposure-class-probe"
			if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
				Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
				ABIVersion: 1, MinVersion: 1,
			}); err != nil {
				t.Fatalf("deploy: %v", err)
			}

			db := adminDBFor(t, backend)

			var selectSQL string
			switch backend.Name() {
			case "mysql":
				selectSQL = `SELECT exposure FROM workflow_defs WHERE name = ? AND version = ?`
			case "mssql":
				selectSQL = `SELECT exposure FROM workflow_defs WHERE name = @p1 AND version = @p2`
			default:
				selectSQL = `SELECT exposure FROM workflow_defs WHERE name = $1 AND version = $2`
			}
			var got string
			if err := db.QueryRowContext(ctx, selectSQL, defName, 1).Scan(&got); err != nil {
				t.Fatalf("read exposure: %v", err)
			}
			if got != "auth" {
				t.Errorf("exposure for a definition deployed through today's path: got %q, want %q -- "+
					"a definition deployed before anything declares a class must keep today's behaviour",
					got, "auth")
			}

			// The negative control: the CHECK constraint, not application code,
			// is what has to refuse this, because the Go struct and every writer
			// of it don't exist yet. A table that silently accepted anything here
			// would make the closed set a comment rather than a guarantee.
			var updateSQL string
			switch backend.Name() {
			case "mysql":
				updateSQL = `UPDATE workflow_defs SET exposure = ? WHERE name = ? AND version = ?`
			case "mssql":
				updateSQL = `UPDATE workflow_defs SET exposure = @p1 WHERE name = @p2 AND version = @p3`
			default:
				updateSQL = `UPDATE workflow_defs SET exposure = $1 WHERE name = $2 AND version = $3`
			}
			if _, err := db.ExecContext(ctx, updateSQL, "bogus-class", defName, 1); err == nil {
				t.Errorf("UPDATE ... SET exposure = 'bogus-class' on %s succeeded -- "+
					"the CHECK constraint did not fire, so the closed set is not enforced", backend.Name())
			}
		})
	}
}
