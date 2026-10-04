package catalogdiff

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/cleat-team/cleat/migration"
)

// snapshotMySQL builds a Catalog for a MySQL database. MySQL has no
// row-level-security or policy concept in this schema, so every Table's
// RowSecurity is nil and Policies is empty -- Diff treats an absent
// RowSecurity the same on both sides, so this never produces a spurious
// difference between two MySQL snapshots.
func snapshotMySQL(ctx context.Context, db *sql.DB) (*Catalog, error) {
	cat := &Catalog{
		Dialect:  migration.DialectMySQL,
		Tables:   map[string]*Table{},
		Routines: map[string]string{},
	}

	rows, err := db.QueryContext(ctx, `
		SELECT table_name FROM information_schema.tables
		WHERE table_schema = DATABASE() AND table_type = 'BASE TABLE'
		ORDER BY table_name
	`)
	if err != nil {
		return nil, fmt.Errorf("catalogdiff: listing tables: %w", err)
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return nil, fmt.Errorf("catalogdiff: scanning table row: %w", err)
		}
		if n == schemaMigrationsTable {
			continue
		}
		names = append(names, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("catalogdiff: listing tables: %w", err)
	}
	rows.Close()

	for _, name := range names {
		t := &Table{Name: name}

		colRows, err := db.QueryContext(ctx, `
			SELECT column_name, column_type, is_nullable, COALESCE(column_default, ''), extra,
			       COALESCE(collation_name, '')
			FROM information_schema.columns
			WHERE table_schema = DATABASE() AND table_name = ?
			ORDER BY ordinal_position
		`, name)
		if err != nil {
			return nil, fmt.Errorf("catalogdiff: columns for %s: %w", name, err)
		}
		for colRows.Next() {
			var cname, ctype, nullable, def, extra, collation string
			if err := colRows.Scan(&cname, &ctype, &nullable, &def, &extra, &collation); err != nil {
				colRows.Close()
				return nil, fmt.Errorf("catalogdiff: scanning column for %s: %w", name, err)
			}
			// `auto_increment` exists NOWHERE ELSE this query reads: COLUMN_TYPE
			// reports `bigint` and COLUMN_DEFAULT reports NULL, so a column
			// quietly losing AUTO_INCREMENT presented as identical rows and the
			// differential could not disagree. Two columns carry it today.
			//
			// Folded into DataType rather than added as a Column field, and that
			// is not a style choice: canonicalize renders a column as
			// `type=%s nullable=%t default=%s` and nothing else, so a new struct
			// field would never reach the diff -- it would look like a fix and
			// change no comparison. The Index below folds its extra attribute
			// the same way, into its Definition string.
			//
			// `DEFAULT_GENERATED` is deliberately NOT folded in: it is fully
			// derivable from column_default (which reports CURRENT_TIMESTAMP(6)
			// either way), so including it would add 52 columns' worth of text
			// to every comparison and discriminate nothing.
			if autoInc := autoIncrementAttribute(extra); autoInc != "" {
				ctype += " " + autoInc
			}
			t.Columns = append(t.Columns, Column{Name: cname, DataType: ctype, Nullable: nullable == "YES", Default: def, Collation: collation})
		}
		if err := colRows.Err(); err != nil {
			return nil, fmt.Errorf("catalogdiff: columns for %s: %w", name, err)
		}
		colRows.Close()

		// One row per (index, column) in MySQL's own view; fold to one
		// Index per index name with a synthetic, sorted "col1,col2,..."
		// definition so it participates in the line-set diff like every
		// other dialect's single-row-per-index definition.
		idxRows, err := db.QueryContext(ctx, `
			SELECT index_name, non_unique, seq_in_index, column_name, collation
			FROM information_schema.statistics
			WHERE table_schema = DATABASE() AND table_name = ?
			ORDER BY index_name, seq_in_index
		`, name)
		if err != nil {
			return nil, fmt.Errorf("catalogdiff: indexes for %s: %w", name, err)
		}
		type idxAcc struct {
			unique bool
			cols   []string
		}
		idxByName := map[string]*idxAcc{}
		var idxOrder []string
		for idxRows.Next() {
			var iname string
			var nonUnique int
			var seq int
			var col string
			var collation sql.NullString
			if err := idxRows.Scan(&iname, &nonUnique, &seq, &col, &collation); err != nil {
				idxRows.Close()
				return nil, fmt.Errorf("catalogdiff: scanning index for %s: %w", name, err)
			}
			// STATISTICS.COLLATION is 'A' or 'D' -- ASCENDING or DESCENDING --
			// and it was not read, so an index that silently lost its DESC
			// presented as identical rows. That is not hypothetical on this
			// dialect: workflow_memory_samples.idx_memory_samples_tenant_def has
			// recorded_at DESC in BOTH the chain and the compacted baseline, so
			// the class is live and the instrument could not see it either way.
			//
			// Folded into the column token rather than the definition string, so
			// the direction reaches the line canonicalize already renders -- for
			// a descending key, on a three-column index:
			//
			//     unique=false columns=[tenant_id,def_name,recorded_at DESC]
			//
			// -- rather than being left to a separate field nothing renders.
			// NULL is a legitimate value here -- a functional index has no
			// column -- so it is NOT treated as ascending by default.
			if collation.Valid && collation.String == "D" {
				col += " DESC"
			}
			acc, ok := idxByName[iname]
			if !ok {
				acc = &idxAcc{unique: nonUnique == 0}
				idxByName[iname] = acc
				idxOrder = append(idxOrder, iname)
			}
			acc.cols = append(acc.cols, col)
		}
		if err := idxRows.Err(); err != nil {
			return nil, fmt.Errorf("catalogdiff: indexes for %s: %w", name, err)
		}
		idxRows.Close()
		for _, iname := range idxOrder {
			acc := idxByName[iname]
			// Comma-joined, matching how the MSSQL snapshot builds this same
			// field -- STRING_AGG(c.name, ',') in that file's statistics query
			// -- so the element boundaries are explicit. `%v` on a []string
			// space-separates instead, which is ambiguous for exactly the token
			// the fold above produces: `[a b recorded_at DESC]` does not say
			// whether the third element is `recorded_at DESC` or `DESC` is a
			// fourth. Two dialects disagreeing about the separator is a wart,
			// not a hazard -- but the fold is what makes the space dangerous.
			t.Indexes = append(t.Indexes, Index{
				Name:       iname,
				Definition: fmt.Sprintf("unique=%t columns=[%s]", acc.unique, strings.Join(acc.cols, ",")),
			})
		}

		// Constraints: MySQL's information_schema.table_constraints names
		// them but does not give a rebuildable definition the way PostgreSQL's
		// pg_get_constraintdef or SQL Server's OBJECT_DEFINITION do, so the
		// constraint TYPE plus its check clause (where present) is what gets
		// captured -- enough to catch a constraint being added, removed, or
		// changed from CHECK to FOREIGN KEY, which is the class of regression
		// a migration compaction could introduce.
		conRows, err := db.QueryContext(ctx, `
			SELECT tc.constraint_name, tc.constraint_type,
			       COALESCE(cc.check_clause, ''), tc.enforced
			FROM information_schema.table_constraints tc
			LEFT JOIN information_schema.check_constraints cc
			  ON cc.constraint_schema = tc.constraint_schema AND cc.constraint_name = tc.constraint_name
			WHERE tc.table_schema = DATABASE() AND tc.table_name = ?
			ORDER BY tc.constraint_name
		`, name)
		if err != nil {
			return nil, fmt.Errorf("catalogdiff: constraints for %s: %w", name, err)
		}
		for conRows.Next() {
			var cname, ctype, check, enforced string
			if err := conRows.Scan(&cname, &ctype, &check, &enforced); err != nil {
				conRows.Close()
				return nil, fmt.Errorf("catalogdiff: scanning constraint for %s: %w", name, err)
			}
			// ENFORCED lives in TABLE_CONSTRAINTS, not CHECK_CONSTRAINTS, and
			// it was read from neither -- so a CHECK added [NOT] ENFORCED and
			// one validated normally presented as the same row. This is the
			// MSSQL is_not_trusted class on this dialect (cleat#2445): an
			// attribute that does not change what the two forms ACCEPT for
			// well-formed data, so no behavioural test distinguishes them either.
			//
			// Zero constraints carry it today -- measured on a built database,
			// 42 CHECK constraints, 0 with ENFORCED='NO'. It is added for the
			// same reason the PostgreSQL side wants convalidated: the value is
			// that the instrument can see the state BEFORE someone writes it.
			//
			// 'YES' is the default and is left implicit, so the diff line is
			// unchanged for every constraint that is enforced.
			if enforced == "NO" {
				check += " NOT ENFORCED"
			}
			t.Constraints = append(t.Constraints, Constraint{Name: cname, Definition: ctype + " " + check})
		}
		if err := conRows.Err(); err != nil {
			return nil, fmt.Errorf("catalogdiff: constraints for %s: %w", name, err)
		}
		conRows.Close()

		cat.Tables[name] = t
	}

	rtRows, err := db.QueryContext(ctx, `
		SELECT routine_name, routine_type, COALESCE(routine_definition, '')
		FROM information_schema.routines
		WHERE routine_schema = DATABASE()
		ORDER BY routine_name, routine_type
	`)
	if err != nil {
		return nil, fmt.Errorf("catalogdiff: listing routines: %w", err)
	}
	for rtRows.Next() {
		var name, rtype, def string
		if err := rtRows.Scan(&name, &rtype, &def); err != nil {
			rtRows.Close()
			return nil, fmt.Errorf("catalogdiff: scanning routine: %w", err)
		}
		cat.Routines[rtype+" "+name] = def
	}
	if err := rtRows.Err(); err != nil {
		return nil, fmt.Errorf("catalogdiff: listing routines: %w", err)
	}
	rtRows.Close()

	// table_schema is deliberately left OUT of the comparable string. On every other
	// dialect a schema is a namespace that can genuinely differ from another schema in
	// the SAME database ("admin" vs "dbo"/"public"), so Postgres's and SQL Server's
	// grant strings keep it. On MySQL a schema IS the database -- there is no narrower
	// namespace below it -- and `scratchMySQLDB` gives every test database its own
	// generated name. Found working on cleat#2203: an early draft added a new MySQL
	// migration file issuing MySQL's first-ever migration GRANT, and
	// TestSnapshotIsIdenticalForTwoBuildsOfTheSameChainMySQL built two scratch databases
	// from that identical chain and reported every one of its GRANTs as a difference,
	// because the name `table_schema` resolves to was never the same string twice.
	// cleat#2203 ultimately moved cleat_app's creation and GRANTs to a deploy-time script
	// (deploy/mysql/900-app-role.sh) instead, over a privilege requirement a migration
	// cannot ask of a MySQL migrate login (CREATE USER, GRANT OPTION) -- see that script's
	// own header -- so no committed migration triggers this comparator bug today. The bug
	// is still real and still here: the next MySQL migration to issue ANY grant will hit
	// it, and `TestSnapshotIsIdenticalForTwoBuildsOfTheSameChainMySQL` passing without this
	// fix proves nothing one way or the other about whether it is present, only that
	// nothing currently exercises it. Filtering on `table_schema = DATABASE()` already
	// scopes the query to the connection's own database, so the name itself adds nothing a
	// reader could use to tell two grants apart.
	grantRows, err := db.QueryContext(ctx, `
		SELECT grantee, table_name, privilege_type
		FROM information_schema.table_privileges
		WHERE table_schema = DATABASE()
		ORDER BY 1, 2, 3
	`)
	if err != nil {
		return nil, fmt.Errorf("catalogdiff: listing grants: %w", err)
	}
	for grantRows.Next() {
		var grantee, table, priv string
		if err := grantRows.Scan(&grantee, &table, &priv); err != nil {
			grantRows.Close()
			return nil, fmt.Errorf("catalogdiff: scanning grant: %w", err)
		}
		cat.Grants = append(cat.Grants, fmt.Sprintf("%s ON %s TO %s", priv, table, grantee))
	}
	if err := grantRows.Err(); err != nil {
		return nil, fmt.Errorf("catalogdiff: listing grants: %w", err)
	}
	grantRows.Close()

	return cat, nil
}

// autoIncrementAttribute returns the one value in information_schema.columns'
// `extra` that this comparator folds into a column's definition, or "".
//
// WHY ONLY THIS ONE. `extra` can hold several tokens -- auto_increment,
// DEFAULT_GENERATED, `on update CURRENT_TIMESTAMP`, STORED GENERATED -- and the
// test for including one is not "is it in extra" but **can two databases
// differing in it present identical rows to the query above?**
//
//	auto_increment        NO other selected column carries it: COLUMN_TYPE says
//	                      `bigint` and COLUMN_DEFAULT says NULL either way. Folded in.
//	DEFAULT_GENERATED     YES -- column_default reports CURRENT_TIMESTAMP(6)
//	                      whichever way it is set. Excluded deliberately; it
//	                      would add 52 columns' worth of text to every
//	                      comparison and discriminate nothing.
//	on update ...         NO -- and it is not in COLUMN_DEFAULT either, so it is
//	                      a real gap. Nothing carries it today, so it is left
//	                      out rather than guessed at: a token that never
//	                      appears is a fold that can never be exercised, and the
//	                      next person to add it should find this comment and
//	                      the census that produced it (cleat#2446).
//
// The distinction is the whole point: a census that folds in every unread
// column is a sample with a bigger N.
func autoIncrementAttribute(extra string) string {
	for _, tok := range strings.Split(extra, " ") {
		if strings.EqualFold(strings.TrimSpace(tok), "auto_increment") {
			return "auto_increment"
		}
	}
	return ""
}
