package migration

import (
	"context"
	"fmt"
)

// minPostgresVersionNum is the oldest PostgreSQL this schema can be applied to,
// as server_version_num: 16.0.
//
// This is a REQUIREMENT, not a preference, and it has one cause. Migration 077
// (cleat#1490) grants the cross-tenant sweep role with
//
//	GRANT cleat_sweep TO <role> WITH INHERIT FALSE
//
// and `WITH INHERIT FALSE` is PostgreSQL 16 syntax. It is load-bearing rather
// than stylistic: membership that INHERITS would make every member match the
// sweep's `USING (true)` policy, which is the isolation boundary itself.
//
// tiers.yaml has claimed 16+ since 2026-08-08, but as a statement about what CI
// verified -- it said in as many words that "nothing in the codebase needs 16".
// That stopped being true when 077 landed on 2026-09-14, and this check is what
// makes the claim enforced rather than merely asserted.
const minPostgresVersionNum = 160000

// checkServerVersion refuses to apply migrations to a server too old to hold
// them, before any migration runs.
//
// WHY A PRE-FLIGHT AND NOT A MIGRATION. The natural place for a guard is a
// migration, and it cannot work here: migrations apply in order, so a new one
// would run *after* 077 and never be reached on the server it exists to warn.
// Amending 077 in place is worse -- it is already applied on deployed databases.
//
// MEASURED against a real PostgreSQL 15.19 rather than reasoned about. Running
// the full set produces:
//
//	ERROR: migration 077_a_plugin_policy_can_use_its_index.sql: execute:
//	       pq: syntax error at or near "INHERIT" (42601)
//
// and the failure is CLEAN -- the file is wrapped in a transaction, so it rolls
// back whole. Verified on that database afterwards: highest recorded migration
// 76, `cleat_sweep` absent, no partial policies. So this check does not prevent
// a corrupted schema; there was never one to prevent. It replaces a Postgres
// syntax error with a sentence naming the requirement, which is the difference
// between an operator reading a release note and an operator reading a stack
// trace.
//
// MySQL and SQL Server have no equivalent floor to enforce here and are skipped
// rather than given a check that always passes -- a check that cannot fail is
// indistinguishable from one that is not running.
func (r *Runner) checkServerVersion(ctx context.Context, session sqlSession) error {
	if r.dialect != DialectPostgres {
		return nil
	}

	var num int
	if err := scanOne(ctx, session,
		`SELECT current_setting('server_version_num')::int`, &num); err != nil {
		// Deliberately not fatal. A server that cannot answer this is not
		// thereby known to be too old, and refusing to migrate on a failed
		// *diagnostic* would turn a working deployment into a broken one. The
		// migration that needs 16 still fails on its own terms if the server
		// is older; this check only ever improves the message.
		return nil
	}
	var shown string
	if err := scanOne(ctx, session, `SHOW server_version`, &shown); err != nil {
		shown = fmt.Sprintf("server_version_num=%d", num)
	}
	return postgresVersionError(num, shown)
}

// postgresVersionError is the whole decision, separated from the queries that
// feed it so that the threshold and the message are testable on any server.
//
// CI runs PostgreSQL 16 and nothing else -- tiers.yaml records that there is no
// version matrix for any dialect -- so the refusal itself cannot be exercised
// there. Without this split the only test possible in CI would be "16 is
// allowed", which every broken threshold also passes.
func postgresVersionError(num int, shown string) error {
	if num >= minPostgresVersionNum {
		return nil
	}
	return fmt.Errorf(
		"cleat requires PostgreSQL %d or later and this server is %s.\n"+
			"  Migration 077 grants the cross-tenant sweep role WITH INHERIT FALSE, which is\n"+
			"  PostgreSQL 16 syntax and is what keeps a plugin table's tenant policy indexable\n"+
			"  without making every role a member of the sweep (cleat#1490).\n"+
			"  Nothing has been applied. Upgrade the server, or pin cleat to a release before\n"+
			"  migration 077.",
		minPostgresVersionNum/10000, shown)
}

// scanOne reads a single value, because sqlSession exposes QueryContext and not
// QueryRowContext -- widening the interface for one caller would make every
// implementation of it carry a method only this file uses.
func scanOne(ctx context.Context, session sqlSession, query string, dest any) error {
	rows, err := session.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return err
		}
		return fmt.Errorf("%s returned no rows", query)
	}
	if err := rows.Scan(dest); err != nil {
		return err
	}
	return rows.Err()
}
