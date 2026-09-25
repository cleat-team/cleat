package catalogdiff

import (
	"context"
	"database/sql"
	"fmt"

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
			SELECT column_name, column_type, is_nullable, COALESCE(column_default, '')
			FROM information_schema.columns
			WHERE table_schema = DATABASE() AND table_name = ?
			ORDER BY ordinal_position
		`, name)
		if err != nil {
			return nil, fmt.Errorf("catalogdiff: columns for %s: %w", name, err)
		}
		for colRows.Next() {
			var cname, ctype, nullable, def string
			if err := colRows.Scan(&cname, &ctype, &nullable, &def); err != nil {
				colRows.Close()
				return nil, fmt.Errorf("catalogdiff: scanning column for %s: %w", name, err)
			}
			t.Columns = append(t.Columns, Column{Name: cname, DataType: ctype, Nullable: nullable == "YES", Default: def})
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
			SELECT index_name, non_unique, seq_in_index, column_name
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
			if err := idxRows.Scan(&iname, &nonUnique, &seq, &col); err != nil {
				idxRows.Close()
				return nil, fmt.Errorf("catalogdiff: scanning index for %s: %w", name, err)
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
			t.Indexes = append(t.Indexes, Index{
				Name:       iname,
				Definition: fmt.Sprintf("unique=%t columns=%v", acc.unique, acc.cols),
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
			       COALESCE(cc.check_clause, '')
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
			var cname, ctype, check string
			if err := conRows.Scan(&cname, &ctype, &check); err != nil {
				conRows.Close()
				return nil, fmt.Errorf("catalogdiff: scanning constraint for %s: %w", name, err)
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

	grantRows, err := db.QueryContext(ctx, `
		SELECT grantee, table_schema, table_name, privilege_type
		FROM information_schema.table_privileges
		WHERE table_schema = DATABASE()
		ORDER BY 1, 2, 3, 4
	`)
	if err != nil {
		return nil, fmt.Errorf("catalogdiff: listing grants: %w", err)
	}
	for grantRows.Next() {
		var grantee, schema, table, priv string
		if err := grantRows.Scan(&grantee, &schema, &table, &priv); err != nil {
			grantRows.Close()
			return nil, fmt.Errorf("catalogdiff: scanning grant: %w", err)
		}
		cat.Grants = append(cat.Grants, fmt.Sprintf("%s ON %s.%s TO %s", priv, schema, table, grantee))
	}
	if err := grantRows.Err(); err != nil {
		return nil, fmt.Errorf("catalogdiff: listing grants: %w", err)
	}
	grantRows.Close()

	return cat, nil
}
