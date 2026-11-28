package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// A deploy must not replace the WASM of a version that has open runs (cleat#3497).
//
// WHY. A run replays its recorded history under whatever bytes the definition
// holds when a worker claims it. `cleat build` stamps version 1 unless told
// otherwise and `cleat deploy` honours the stamp, so the default build-and-deploy
// loop upserted new bytes onto v1 while earlier runs of v1 were still open. A
// change that alters the command sequence makes each of those runs diverge: a loud
// non-determinism error at best, a silent wrong path at worst. The comment on the
// deploy code called a same-version redeploy idempotent, and that holds only when
// the bytes are identical, which nothing checked.
//
// THE RULE, one piece of code per dialect and the same text in each:
//
//	a row exists for (tenant, name, version), AND the new bytes differ from its
//	bytes, AND at least one run of that version is not settled   =>  refuse.
//
// Identical bytes stay an idempotent upsert, which is what lets a retried deploy
// and a metadata-only redeploy (exposure, validation) keep working. Different bytes
// with every run settled, or with no run at all, are allowed: nothing is replaying.
// WorkflowDef.ReplaceUnderOpenRuns is the deliberate hot-fix.
//
// "NOT SETTLED" is the canonical settled list (engine/status_vocabulary.go), so a
// run in 'terminating' counts as open. That is not the question CountActiveInstances
// asks: it names only 'ready' and 'running'.
//
// THE COMPARISON HAPPENS IN THE DATABASE and the existing row is read FOR UPDATE.
// The bytes can be ~19MB for a Python component, so the row is not transferred to be
// compared, and the lock makes two concurrent deploys of one version serialise: the
// second compares against what the first wrote.
//
// A run that starts between this check and the write is not a case the rule can
// cover or needs to: it has no history yet, so nothing of it can diverge.

// ErrWorkflowDefInUse is matched by errors.Is for a refusal under this rule.
var ErrWorkflowDefInUse = errors.New("workflow definition has open runs")

// WorkflowDefInUseError names what was refused. It is deliberately free of CLI flag
// names: callers add the way to proceed that fits their interface (a new version,
// or the force option).
type WorkflowDefInUseError struct {
	Name     string
	Version  int
	OpenRuns int
}

func (e *WorkflowDefInUseError) Error() string {
	return fmt.Sprintf("refusing to replace the WASM of workflow %q version %d: the bytes differ from the deployed ones and %d run(s) are still open on that version, "+
		"so they would replay their history under different code", e.Name, e.Version, e.OpenRuns)
}

// WorkflowDefInUseCLIAdvice is what `cleat deploy` and `deploy-workflow` print after
// the refusal. They share the --force and --version flags, so it is written once.
const WorkflowDefInUseCLIAdvice = "To proceed: build with a higher version (cleat build --version N) and deploy that, " +
	"so the open runs keep the code they started on; or pass --force to replace these bytes on purpose, " +
	"which makes those runs replay their history under the new code."

// Is reports a match against ErrWorkflowDefInUse.
func (e *WorkflowDefInUseError) Is(target error) bool { return target == ErrWorkflowDefInUse }

// ErrWorkflowDefIsLibraryMember is matched by errors.Is for a refusal to write bytes
// over a definition that is installed from a library (cleat#3540).
var ErrWorkflowDefIsLibraryMember = errors.New("workflow definition is installed from a library")

// WorkflowDefIsLibraryMemberError names what was refused. A library member is
// immutable: the tenant's row holds no bytes, it points at a published member, and
// the table's CHECK (bytes XOR reference) would refuse the write with a constraint
// name. Refusing here says why, and does so whether or not the bytes are identical
// and whatever ReplaceUnderOpenRuns says, because nothing can be written to such a
// row, not because runs are open.
type WorkflowDefIsLibraryMemberError struct {
	Name    string
	Version int
	Library string
}

func (e *WorkflowDefIsLibraryMemberError) Error() string {
	return fmt.Sprintf("refusing to deploy over workflow %q version %d: it is installed from library %q, and a library member is immutable. "+
		"To change what this tenant runs, install a newer version of the library", e.Name, e.Version, e.Library)
}

// Is reports a match against ErrWorkflowDefIsLibraryMember.
func (e *WorkflowDefIsLibraryMemberError) Is(target error) bool {
	return target == ErrWorkflowDefIsLibraryMember
}

// libraryMemberReadError is what a definition's reference to a library member that
// cannot be read becomes. The foreign key means a referenced member cannot be deleted,
// so a missing one is a database that was changed underneath the engine, and it is
// reported as that rather than as a bare "no rows".
func libraryMemberReadError(op, library, name string, version int, err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%s: workflow %s v%d is installed from library %q but that library member is missing, "+
			"which its foreign key should make impossible", op, name, version, library)
	}
	return fmt.Errorf("%s: library member %s/%s v%d: %w", op, library, name, version, err)
}

// CheckWorkflowDefReplacementPostgres applies the rule inside tx, which must
// already carry the tenant (beginTxWithRLS, or `cleat deploy`'s own set_config).
// PostgresStore.DeployWorkflowDef and `cleat deploy` both call it, so the two share
// one rule instead of each carrying a copy.
func CheckWorkflowDefReplacementPostgres(ctx context.Context, tx *sql.Tx, tenantID, name string, version int, wasm []byte, force bool) error {
	// D59 (cleat#3629): before anything is read or locked on the row, so a refusal leaves no trace.
	if err := lockLibraryNames(ctx, tx, DialectPostgres, false); err != nil {
		return err
	}
	if err := refuseNameUnderLibraryPrefix(ctx, tx, DialectPostgres, name); err != nil {
		return err
	}
	var same bool
	var library sql.NullString
	err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(wasm_bytes = $4, false), library FROM workflow_defs
		WHERE tenant_id = $1 AND name = $2 AND version = $3
		FOR UPDATE
	`, tenantID, name, version, wasm).Scan(&same, &library)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // the first deploy of this version
	}
	if err != nil {
		return fmt.Errorf("check workflow def replacement: read existing: %w", err)
	}
	if library.Valid {
		return &WorkflowDefIsLibraryMemberError{Name: name, Version: version, Library: library.String}
	}
	if same || force {
		return nil
	}
	var open int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM workflow_leases l
		JOIN workflow_payloads p ON p.id = l.id
		WHERE p.tenant_id = $1 AND p.def_name = $2 AND p.def_version = $3
		  AND l.status NOT IN ('done', 'failed', 'dead_lettered', 'terminated', 'cancelled')
	`, tenantID, name, version).Scan(&open); err != nil {
		return fmt.Errorf("check workflow def replacement: count open runs: %w", err)
	}
	if open > 0 {
		return &WorkflowDefInUseError{Name: name, Version: version, OpenRuns: open}
	}
	return nil
}

// checkWorkflowDefReplacement is the rule on MySQL. A run keeps its definition and
// status on workflow_instances there.
func (s *MySQLStore) checkWorkflowDefReplacement(ctx context.Context, tx *sql.Tx, def *WorkflowDef) error {
	var same bool
	err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(wasm_bytes = ?, 0) FROM workflow_defs
		WHERE tenant_id = ? AND name = ? AND version = ?
		FOR UPDATE
	`, def.WASMBytes, s.tenantID, def.Name, def.Version).Scan(&same)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("DeployWorkflowDef: read existing: %w", err)
	}
	if same || def.ReplaceUnderOpenRuns {
		return nil
	}
	var open int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM workflow_instances
		WHERE tenant_id = ? AND def_name = ? AND def_version = ?
		  AND status NOT IN ('done', 'failed', 'dead_lettered', 'terminated', 'cancelled')
	`, s.tenantID, def.Name, def.Version).Scan(&open); err != nil {
		return fmt.Errorf("DeployWorkflowDef: count open runs: %w", err)
	}
	if open > 0 {
		return &WorkflowDefInUseError{Name: def.Name, Version: def.Version, OpenRuns: open}
	}
	return nil
}

// checkWorkflowDefReplacement is the rule on SQL Server. The existing row is read
// WITH (UPDLOCK, HOLDLOCK), the same lock the MERGE that follows takes.
func (s *MSSQLStore) checkWorkflowDefReplacement(ctx context.Context, tx *sql.Tx, def *WorkflowDef) error {
	// D59 (cleat#3629): before anything is read or locked on the row, so a refusal leaves no trace.
	if err := lockLibraryNames(ctx, tx, DialectMSSQL, false); err != nil {
		return err
	}
	if err := refuseNameUnderLibraryPrefix(ctx, tx, DialectMSSQL, def.Name); err != nil {
		return err
	}
	var same int
	var library sql.NullString
	err := tx.QueryRowContext(ctx, `
		SELECT CASE WHEN wasm_bytes = @p4 THEN 1 ELSE 0 END, library FROM workflow_defs WITH (UPDLOCK, HOLDLOCK)
		WHERE tenant_id = @p1 AND name = @p2 AND version = @p3
	`, s.tenantID, def.Name, def.Version, def.WASMBytes).Scan(&same, &library)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("deploy workflow def: read existing: %w", err)
	}
	if library.Valid {
		return &WorkflowDefIsLibraryMemberError{Name: def.Name, Version: def.Version, Library: library.String}
	}
	if same == 1 || def.ReplaceUnderOpenRuns {
		return nil
	}
	var open int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM workflow_instances
		WHERE tenant_id = @p1 AND def_name = @p2 AND def_version = @p3
		  AND status NOT IN ('done', 'failed', 'dead_lettered', 'terminated', 'cancelled')
	`, s.tenantID, def.Name, def.Version).Scan(&open); err != nil {
		return fmt.Errorf("deploy workflow def: count open runs: %w", err)
	}
	if open > 0 {
		return &WorkflowDefInUseError{Name: def.Name, Version: def.Version, OpenRuns: open}
	}
	return nil
}
