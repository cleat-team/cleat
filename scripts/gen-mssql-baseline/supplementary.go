package main

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

// supplementary compares the classes catalogdiff cannot see.
//
// It is not a nicety. migration/catalogdiff/mssql.go compares a column's type
// NAME but not its length, precision, scale or collation, and it compares an
// index's key columns but not its INCLUDE list, its filter or its sort order.
// So a diff of A against B can be empty while the baseline has lost every
// column width and every filtered index. That is the class of regression a
// baseline generator is most likely to introduce and least likely to notice.
//
// Security policies/predicates, schemas, database role existence and trigger
// identity used to be gaps of exactly this shape -- they are compared in
// catalogdiff itself now (cleat#2432) and are deliberately not repeated here;
// see the comment on supplementaryChecks below.
//
// Each check returns a SET of rendered rows, sorted. Comparing sets rather than
// counts is deliberate: a count answers "did this go up" and never "is anything
// still missing", and two sides can agree on a total while disagreeing on a
// tenth of the membership.
//
// A check whose query fails is UNMEASURED, not PASS. A check that measured
// nothing agrees with every database, correct or not.
type check struct {
	name  string
	query string
}

var supplementaryChecks = []check{
	{
		"column shape (width/precision/scale/collation/identity/nullability)",
		`SELECT s.name, t.name, c.name, ty.name, c.max_length, c.precision, c.scale,
		        COALESCE(c.collation_name, ''), c.is_nullable, c.is_identity, c.is_computed
		 FROM sys.columns c
		 JOIN sys.tables t ON t.object_id = c.object_id
		 JOIN sys.schemas s ON s.schema_id = t.schema_id
		 JOIN sys.types ty ON ty.user_type_id = c.user_type_id
		 WHERE t.is_ms_shipped = 0 AND t.name NOT IN ('schema_migrations','plugin_migrations')`,
	},
	{
		"identity seed/increment",
		`SELECT s.name, t.name, c.name, CONVERT(varchar(40), ic.seed_value), CONVERT(varchar(40), ic.increment_value)
		 FROM sys.identity_columns ic
		 JOIN sys.columns c ON c.object_id = ic.object_id AND c.column_id = ic.column_id
		 JOIN sys.tables t ON t.object_id = ic.object_id
		 JOIN sys.schemas s ON s.schema_id = t.schema_id`,
	},
	{
		"index attributes (filter, INCLUDE, sort order, unique, type)",
		`SELECT s.name, t.name, i.name, i.type_desc, i.is_unique, i.has_filter,
		        COALESCE(i.filter_definition, ''), ic.is_included_column, ic.is_descending_key,
		        ic.key_ordinal, c.name
		 FROM sys.indexes i
		 JOIN sys.tables t ON t.object_id = i.object_id
		 JOIN sys.schemas s ON s.schema_id = t.schema_id
		 JOIN sys.index_columns ic ON ic.object_id = i.object_id AND ic.index_id = i.index_id
		 JOIN sys.columns c ON c.object_id = ic.object_id AND c.column_id = ic.column_id
		 WHERE i.name IS NOT NULL
		   AND t.name NOT IN ('schema_migrations','plugin_migrations')`,
	},
	// Security policies/predicates, schemas, database roles (existence) and
	// trigger NAME/BODY/schema moved to migration/catalogdiff/mssql.go itself
	// -- cleat#2432. That file's own header comment used to name this script
	// as the place those classes were compared and call moving them into
	// catalogdiff "the better repair"; they are compared there now, with
	// known-positive tests proving each one (see
	// the_mssql_snapshot_sees_rls_schemas_roles_triggers_test.go), so keeping
	// the identical queries here too would be the same fact checked in two
	// places with no guard keeping them in sync -- exactly the duplication
	// this repo's CLAUDE.md warns a census-style check rots into. Trigger
	// SETTINGS (is_disabled, QUOTED_IDENTIFIER/ANSI_NULLS) remain below, in
	// "module settings", because catalogdiff does not compare those.
	{
		"module settings (QUOTED_IDENTIFIER / ANSI_NULLS) and body hash",
		`SELECT s.name, o.name, o.type_desc, m.uses_quoted_identifier, m.uses_ansi_nulls,
		        CONVERT(varchar(64), HASHBYTES('SHA2_256', CONVERT(varbinary(max), m.definition)), 2)
		 FROM sys.objects o
		 JOIN sys.schemas s ON s.schema_id = o.schema_id
		 JOIN sys.sql_modules m ON m.object_id = o.object_id
		 WHERE o.is_ms_shipped = 0 AND o.type IN ('P','FN','IF','TF','TR','V')`,
	},
	{
		"column-level extended properties",
		`SELECT s.name, t.name, c.name, ep.name, CONVERT(varchar(max), ep.value)
		 FROM sys.extended_properties ep
		 JOIN sys.columns c ON c.object_id = ep.major_id AND c.column_id = ep.minor_id
		 JOIN sys.tables t ON t.object_id = c.object_id
		 JOIN sys.schemas s ON s.schema_id = t.schema_id
		 WHERE ep.class = 1`,
	},
	{
		"seed data row counts",
		`SELECT s.name, t.name, CONVERT(varchar(20), SUM(p.rows))
		 FROM sys.tables t
		 JOIN sys.schemas s ON s.schema_id = t.schema_id
		 JOIN sys.partitions p ON p.object_id = t.object_id AND p.index_id IN (0,1)
		 WHERE t.is_ms_shipped = 0 AND t.name NOT IN ('schema_migrations','plugin_migrations')
		 GROUP BY s.name, t.name`,
	},
}

func supplementary(ctx context.Context, a *sql.DB, bdsn string) error {
	if bdsn == "" {
		return fmt.Errorf("-bdsn is required for -mode=supplementary")
	}
	b, err := sql.Open("sqlserver", bdsn)
	if err != nil {
		return err
	}
	defer b.Close()

	failed, unmeasured := 0, 0
	for _, c := range supplementaryChecks {
		setA, errA := runCheck(ctx, a, c.query)
		setB, errB := runCheck(ctx, b, c.query)
		switch {
		case errA != nil || errB != nil:
			// UNMEASURED is its own outcome, never a pass: a check that could
			// not establish what it was measuring agrees with every database.
			fmt.Printf("UNMEASURED  %s\n", c.name)
			if errA != nil {
				fmt.Printf("              A: %v\n", errA)
			}
			if errB != nil {
				fmt.Printf("              B: %v\n", errB)
			}
			unmeasured++
			continue
		}
		onlyA, onlyB := setDifference(setA, setB)
		if len(onlyA) == 0 && len(onlyB) == 0 {
			fmt.Printf("PASS        %-62s %d row(s)\n", c.name, len(setA))
			continue
		}
		failed++
		fmt.Printf("FAIL        %-62s A=%d B=%d  onlyA=%d onlyB=%d\n",
			c.name, len(setA), len(setB), len(onlyA), len(onlyB))
		for i, line := range onlyA {
			if i >= 3 {
				break
			}
			fmt.Printf("              only in A: %s\n", line)
		}
		for i, line := range onlyB {
			if i >= 3 {
				break
			}
			fmt.Printf("              only in B: %s\n", line)
		}
	}

	fmt.Printf("\nsupplementary: %d check(s) failed, %d unmeasured, %d passed\n",
		failed, unmeasured, len(supplementaryChecks)-failed-unmeasured)
	if failed != 0 || unmeasured != 0 {
		return fmt.Errorf("supplementary checks did not all pass")
	}
	return nil
}

// runCheck renders each row as a joined string and returns the SET, sorted.
// Values are joined in Go rather than in SQL: concatenating catalogue columns
// with a literal mixes collations and fails outright on this schema.
func runCheck(ctx context.Context, db *sql.DB, query string) ([]string, error) {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out []string
	for rows.Next() {
		vals := make([]sql.NullString, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		parts := make([]string, len(cols))
		for i, v := range vals {
			if v.Valid {
				parts[i] = v.String
			} else {
				parts[i] = "<NULL>"
			}
		}
		out = append(out, strings.Join(parts, " | "))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

func setDifference(a, b []string) (onlyA, onlyB []string) {
	inB := make(map[string]bool, len(b))
	for _, s := range b {
		inB[s] = true
	}
	inA := make(map[string]bool, len(a))
	for _, s := range a {
		inA[s] = true
	}
	for _, s := range a {
		if !inB[s] {
			onlyA = append(onlyA, s)
		}
	}
	for _, s := range b {
		if !inA[s] {
			onlyB = append(onlyB, s)
		}
	}
	return onlyA, onlyB
}
