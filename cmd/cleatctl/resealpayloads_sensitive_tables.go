package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/cleat-team/cleat/engine"
)

// ---------------------------------------------------------------------------
// reseal-payloads: the six tables cleat#2312 added, cleat#3241
// ---------------------------------------------------------------------------
//
// cleat#2312 extended --encrypt-sensitive-payloads past event_history to
// workflow_instances, workflow_signals, workflow_promises,
// workflow_update_requests, workflow_schedules and idempotency_keys. The
// sweep above (resealpayloads.go) never learned about them: it is written
// against event_history's own columns and its own two-column primary key,
// so a row already written to one of the six tables under a retired key is
// not touched by it. The row stays READABLE -- the previous-key path in
// engine.PayloadEncryption still opens it -- but a rotation meant to retire
// that key fleet-wide cannot complete for these tables through what exists
// today. That is the gap this file closes.
//
// WHY A SEPARATE FILE RATHER THAN FOLDING INTO resealPayloads. The six
// tables have HETEROGENEOUS PRIMARY KEYS -- a single bigint column
// (workflow_signals.id), a single text column (workflow_instances.id), two
// text columns (workflow_promises, workflow_update_requests), a uuid+text
// pair that is also the tenant column (workflow_schedules), and a
// bytea+uuid pair where the bytea is also not human-readable
// (idempotency_keys.key_hash). event_history's own two hardcoded
// placeholders ($1, $2) cannot express that, so generalising means building
// the WHERE clause from a variable-length key rather than reusing the
// existing function's shape.
//
// THE GENERALISATION THAT MAKES IT POSSIBLE: cast every primary-key column
// to text, both in the SELECT and in the WHERE equality, and let the
// database do the type-specific comparison. A bigint's ::text is its
// decimal string; a uuid's is its canonical form; a bytea's is Postgres's
// standard \x-prefixed hex -- all three are total, deterministic functions
// of the stored value, so round-tripping a value out through ::text and
// back into an equality predicate against that same cast selects exactly
// the row it came from. This is what lets one function walk all six tables
// without a type switch anywhere in it.
//
// resealValue (resealpayloads.go) needed NONE of this: it classifies a
// stored STRING by its own shape -- quoted (a jsonb column's JSON string
// literal) or bare (a text column's base64) -- not by the column's declared
// type. That is already documented there as deliberate ("the form is
// self-describing"), and it is the reason this file can share it unchanged:
// a function that does not ask what a column's SQL type is cannot be wrong
// about it.
//
// WHY tenant_id IS SOMETIMES A PRIMARY-KEY COLUMN AND SOMETIMES NOT.
// workflow_schedules' key is (tenant_id, name) and idempotency_keys' is
// (key_hash, tenant_id); both already select tenant_id for key matching, so
// a second, separate tenant_id read would be redundant, not wrong, but it
// would make the pending-row bookkeeping below carry two copies of the same
// value under two different names. tableSpec.pkCols is searched for
// "tenant_id" once per table precisely so the one value already fetched for
// the key is reused for the GCM additional authenticated data too -- the
// same tenant value drives both regardless of which path provided it.
//
// NOT A MIGRATION, for the same reason resealpayloads.go's own doc comment
// gives: the payload key is worker configuration read from a file, and
// nothing under migrations/ can hold it.
//
// RLS: all six tables carry `FORCE ROW LEVEL SECURITY`
// (migrations/postgres/001_schema.sql), the same as event_history, so the
// same requirement applies -- --db must name a role RLS does not apply to.
// resealTable reads tenant_id (or a pk column that doubles as it) off every
// row rather than iterating a tenant list, for the exact reason
// resealPayloads's own doc comment gives: a tenant missing from a list
// contributes no rows, no errors and no findings, and the sweep reports
// clean while leaving unbound or unrotated ciphertext behind.

// tableSpec names one of the six tables, its primary key (in column order,
// as created by migrations/postgres/001_schema.sql's own ADD CONSTRAINT
// lines), and the columns #2312 may have sealed.
type tableSpec struct {
	name   string
	pkCols []string
	cols   []string
}

// sensitiveTableSpecs is cleat#2312's own column list, restated here rather
// than imported: engine.EncryptedEventColumns names event_history's columns
// specifically, and there is no equivalent exported list for these six
// tables because sensitive_column_encryption.go calls each write site's
// encrypt helper directly rather than looping one. Re-derive by reading
// every encryptJSONColumnForStorage/encryptTextColumnForStorage call site:
//
//	git grep -n 'encrypt\(JSON\|Text\)ColumnForStorage' engine/*.go | grep -v _test
//
// and confirm against engine/sensitive_column_encryption_live_test.go's own
// three test names, which exercise exactly these columns end to end.
var sensitiveTableSpecs = []tableSpec{
	{
		name:   "workflow_instances",
		pkCols: []string{"id"},
		cols: []string{
			"input", "result", "error_msg", "error_code", "error_op",
			"cancellation_reason", "query_state",
		},
	},
	{
		name:   "workflow_signals",
		pkCols: []string{"id"},
		cols:   []string{"payload"},
	},
	{
		name:   "workflow_promises",
		pkCols: []string{"workflow_id", "promise_id"},
		cols:   []string{"result", "error_msg"},
	},
	{
		name:   "workflow_update_requests",
		pkCols: []string{"workflow_id", "request_id"},
		cols:   []string{"payload", "result", "error_msg"},
	},
	{
		name:   "workflow_schedules",
		pkCols: []string{"tenant_id", "name"},
		cols:   []string{"input"},
	},
	{
		name:   "idempotency_keys",
		pkCols: []string{"key_hash", "tenant_id"},
		cols:   []string{"error_msg"},
	},
}

// resealTable runs resealPayloads's own algorithm against one table named by
// spec, generalised over an arbitrary primary key instead of event_history's
// hardcoded (workflow_id, step).
func resealTable(ctx context.Context, db *sql.DB, enc *engine.PayloadEncryption,
	dryRun bool, out io.Writer, spec tableSpec) (resealStats, error) {

	var st resealStats

	tenantFromPK := -1
	for i, c := range spec.pkCols {
		if c == "tenant_id" {
			tenantFromPK = i
		}
	}

	selectCols := make([]string, 0, len(spec.pkCols)+1+len(spec.cols))
	for _, c := range spec.pkCols {
		selectCols = append(selectCols, quoteIdent(c)+"::text")
	}
	if tenantFromPK < 0 {
		selectCols = append(selectCols, "tenant_id::text")
	}
	selectCols = append(selectCols, quoteIdents(spec.cols)...)

	//nolint:gosec // G202: every identifier here (table name, pk columns, data
	// columns) comes from the sensitiveTableSpecs literal above, and each one is
	// wrapped by quoteIdent. Nothing read from the database or the command line
	// is concatenated into the query text -- same argument as resealpayloads.go's
	// own sel/q construction.
	sel := "SELECT " + strings.Join(selectCols, ", ") + " FROM " + quoteIdent(spec.name) +
		" ORDER BY " + strings.Join(quoteIdents(spec.pkCols), ", ")
	rows, err := db.QueryContext(ctx, sel)
	if err != nil {
		return st, fmt.Errorf("reading %s: %w", spec.name, err)
	}

	type pending struct {
		pk     []string
		values map[string]string
	}
	var work []pending

	for rows.Next() {
		pkVals := make([]sql.NullString, len(spec.pkCols))
		var tenant sql.NullString
		holders := make([]sql.NullString, len(spec.cols))

		scanTo := make([]any, 0, len(pkVals)+1+len(holders))
		for i := range pkVals {
			scanTo = append(scanTo, &pkVals[i])
		}
		if tenantFromPK < 0 {
			scanTo = append(scanTo, &tenant)
		}
		for i := range holders {
			scanTo = append(scanTo, &holders[i])
		}
		if err := rows.Scan(scanTo...); err != nil {
			rows.Close()
			return st, fmt.Errorf("scan %s: %w", spec.name, err)
		}
		st.Rows++

		pk := make([]string, len(pkVals))
		for i, v := range pkVals {
			pk[i] = v.String // every pk column across these six tables is NOT NULL
		}
		tenantID := tenant.String
		if tenantFromPK >= 0 {
			tenantID = pk[tenantFromPK]
		}

		changed := map[string]string{}
		for i, col := range spec.cols {
			if !holders[i].Valid || holders[i].String == "" {
				continue
			}
			next, kind, verr := resealValue(enc, tenantID, holders[i].String)
			switch kind {
			case valueConverted:
				st.Older++
				changed[col] = next
			case valueCurrent:
				st.Current++
			case valueNotCiphertext:
				st.NotCipher++
			case valueUnreadable:
				st.Unreadable++
				fmt.Fprintf(out, "  UNREADABLE %s %s %s: %v\n", spec.name, strings.Join(pk, "/"), col, verr)
			}
		}
		if len(changed) > 0 {
			work = append(work, pending{pk, changed})
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return st, fmt.Errorf("iterate %s: %w", spec.name, err)
	}
	rows.Close()

	if dryRun {
		return st, nil
	}

	for _, w := range work {
		names := make([]string, 0, len(w.values))
		for c := range w.values {
			names = append(names, c)
		}
		sort.Strings(names) // deterministic SQL, so a failure is reproducible

		args := make([]any, 0, len(w.pk)+len(names))
		for _, v := range w.pk {
			args = append(args, v)
		}
		sets := make([]string, 0, len(names))
		for i, c := range names {
			sets = append(sets, fmt.Sprintf("%s = $%d", quoteIdent(c), len(w.pk)+i+1))
			args = append(args, w.values[c])
		}
		wheres := make([]string, len(spec.pkCols))
		for i, c := range spec.pkCols {
			wheres[i] = quoteIdent(c) + "::text = $" + strconv.Itoa(i+1)
		}

		//nolint:gosec // G202: same argument as the SELECT above -- identifiers
		// from the literal spec via quoteIdent, every value bound as a parameter.
		q := "UPDATE " + quoteIdent(spec.name) + " SET " + strings.Join(sets, ", ") +
			" WHERE " + strings.Join(wheres, " AND ")
		res, err := db.ExecContext(ctx, q, args...)
		if err != nil {
			return st, fmt.Errorf("%s row %s: write failed: %w", spec.name, strings.Join(w.pk, "/"), err)
		}
		// Zero rows on an UPDATE of a row just read means readable but not
		// writable -- same refusal resealPayloads makes, for the same reason:
		// continuing would count a rewrite that did not happen.
		if n, err := res.RowsAffected(); err == nil && n == 0 {
			return st, fmt.Errorf("%s row %s matched no row on write: it was "+
				"read but not written, so this connection cannot complete the sweep",
				spec.name, strings.Join(w.pk, "/"))
		}
		st.Rewrote += len(names)
	}

	return st, nil
}

// resealSensitiveTables runs resealTable across every table in
// sensitiveTableSpecs and returns both the per-table breakdown (for the
// report) and the sum (for the exit-status decision, which does not care
// which table a shortfall is in).
func resealSensitiveTables(ctx context.Context, db *sql.DB, enc *engine.PayloadEncryption,
	dryRun bool, out io.Writer) ([]tableResealResult, resealStats, error) {

	results := make([]tableResealResult, 0, len(sensitiveTableSpecs))
	var total resealStats
	for _, spec := range sensitiveTableSpecs {
		st, err := resealTable(ctx, db, enc, dryRun, out, spec)
		results = append(results, tableResealResult{Table: spec.name, Stats: st})
		total.Rows += st.Rows
		total.Older += st.Older
		total.Rewrote += st.Rewrote
		total.Current += st.Current
		total.NotCipher += st.NotCipher
		total.Unreadable += st.Unreadable
		if err != nil {
			return results, total, fmt.Errorf("%s: %w", spec.name, err)
		}
	}
	return results, total, nil
}

type tableResealResult struct {
	Table string
	Stats resealStats
}
