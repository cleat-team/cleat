package engine

// cleat#3775, the parts that are PostgreSQL's alone: the filter names its tenant in the SQL, it is refused
// where query_state is ciphertext, and it is written so an index an operator adds is used.

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
)

const stateFilterTenantB = "f3775000-0000-4000-8000-0000000000bb"

// A superuser connection, so the listing's row-level security does NOT apply and the SQL predicate is the
// only scope there is (the lesson of cleat#3740). Two tenants publish the SAME state; each must see its own.
func TestARunIsFoundOnlyInItsOwnTenantEvenThroughARoleThatBypassesRLS(t *testing.T) {
	db := testDB(t)
	t.Cleanup(func() { db.Close() })
	var bypass bool
	if err := db.QueryRow(`SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&bypass); err != nil || !bypass {
		t.Fatalf("this test needs a connection that BYPASSES row-level security (err=%v): the statement is its only scope there", err)
	}
	if _, err := db.Exec(`INSERT INTO admin.tenants (tenant_id, name, suspended) VALUES ($1, 'cleat-3775-b', false) ON CONFLICT (tenant_id) DO NOTHING`, stateFilterTenantB); err != nil {
		t.Fatalf("registering tenant B: %v", err)
	}
	t.Cleanup(func() {
		testutil.CleanupPostgresTestData(t, db)
		_, _ = db.Exec(`DELETE FROM admin.tenants WHERE tenant_id = $1`, stateFilterTenantB)
	})

	a := NewPostgresStore(db).WithTenant(DefaultTenantUUID)
	b := NewPostgresStore(db).WithTenant(stateFilterTenantB)
	deployFaultTestDef(t, a)
	deployFaultTestDef(t, b)
	ctx := context.Background()

	// Tenant B first, so a statement with no tenant predicate that returned "the first rows" would answer B's.
	idB := publishRunAs(t, b, stateFilterTenantB, "test", "qsf-pg-b", map[string]string{"order_id": "123"})
	idA := publishRunAs(t, a, DefaultTenantUUID, "test", "qsf-pg-a", map[string]string{"order_id": "123"})

	for _, c := range []struct {
		name  string
		store *PostgresStore
		want  []string
	}{{"tenant A", a, []string{idA}}, {"tenant B", b, []string{idB}}} {
		filter := WorkflowFilter{QueryState: map[string]string{"order_id": "123"}, Limit: 100}
		got, err := c.store.ListWorkflows(ctx, filter)
		if err != nil {
			t.Fatalf("%s: ListWorkflows: %v", c.name, err)
		}
		if g := publishedIDs(got); !reflect.DeepEqual(g, c.want) {
			t.Errorf("%s's listing = %v, want only its own %v: the filter read the other tenant's run (cleat#3775)", c.name, g, c.want)
		}
		if n, err := c.store.CountWorkflows(ctx, filter); err != nil || n != 1 {
			t.Errorf("%s's count = %d (err=%v), want 1", c.name, n, err)
		}
	}
}

// With --encrypt-sensitive-payloads query_state is a JSON string literal wrapping ciphertext, so the filter
// would answer "no" for every row. It is refused, and the refusal is only right if the column really is
// ciphertext, so that is read off the table first.
func TestAStateFilterIsRefusedWhereQueryStateIsCiphertext(t *testing.T) {
	db := testDB(t)
	t.Cleanup(func() { db.Close() })
	t.Cleanup(func() { testutil.CleanupPostgresTestData(t, db) })
	enc, err := NewPayloadEncryption(validKey(t))
	if err != nil {
		t.Fatal(err)
	}
	store := NewPostgresStore(db).WithEncryption(enc, true)
	deployFaultTestDef(t, store)
	ctx := context.Background()
	id := publishRunAs(t, store, DefaultTenantUUID, "test", "qsf-sealed", map[string]string{"order_id": "123"})

	var raw string
	if err := db.QueryRow(`SELECT query_state::text FROM workflow_payloads WHERE id = $1`, id).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(raw, `"`) || strings.Contains(raw, "order_id") {
		t.Fatalf("UNMEASURED: query_state on disk is %q, not a sealed string: the premise of the refusal does not hold", raw)
	}

	filter := WorkflowFilter{QueryState: map[string]string{"order_id": "123"}}
	if _, err := store.ListWorkflows(ctx, filter); !errors.Is(err, ErrQueryStateFilterUnavailableUnderEncryption) {
		t.Errorf("ListWorkflows with a state filter under encryption: err = %v, want ErrQueryStateFilterUnavailableUnderEncryption", err)
	}
	if _, err := store.CountWorkflows(ctx, filter); !errors.Is(err, ErrQueryStateFilterUnavailableUnderEncryption) {
		t.Errorf("CountWorkflows with a state filter under encryption: err = %v, want ErrQueryStateFilterUnavailableUnderEncryption", err)
	}
	// Control: the refusal is for the filter, not for the listing.
	if got, err := store.ListWorkflows(ctx, WorkflowFilter{Limit: 100}); err != nil || len(got) != 1 {
		t.Errorf("an unfiltered listing under encryption = %v (err=%v), want the one run", publishedIDs(got), err)
	}
}

// The filter is written as containment so an index an operator adds is used. This builds the statement the
// store builds (same builder, same filter function) and asks the planner, with sequential and plain index
// scans disabled so "a bitmap scan on an index over query_state is possible" is what is being asked: without
// the index there is nothing to choose, and with it the planner must choose it. The index is the one docs/how-to/find-a-run.md tells operators to create,
// by the name it gives.
func TestAStateFilterUsesAnIndexAnOperatorAddsAndNeedsNoneToWork(t *testing.T) {
	db := testDB(t)
	t.Cleanup(func() { db.Close() })
	t.Cleanup(func() {
		testutil.CleanupPostgresTestData(t, db)
		_, _ = db.Exec(`DROP INDEX IF EXISTS idx_payloads_query_state`)
	})
	store := NewPostgresStore(db)
	deployFaultTestDef(t, store)
	ctx := context.Background()
	id := publishRunAs(t, store, DefaultTenantUUID, "test", "qsf-idx", map[string]string{"order_id": "123"})

	filter := WorkflowFilter{QueryState: map[string]string{"order_id": "123"}, Limit: 100}
	list := func(label string) {
		t.Helper()
		got, err := store.ListWorkflows(ctx, filter)
		if err != nil || !reflect.DeepEqual(publishedIDs(got), []string{id}) {
			t.Fatalf("%s: listing = %v (err=%v), want [%s]", label, publishedIDs(got), err, id)
		}
	}
	plan := func() string {
		t.Helper()
		qb := NewQueryBuilder(DialectPostgres, "SELECT COUNT(*) FROM workflow_leases l JOIN workflow_payloads p ON l.id = p.id WHERE 1=1")
		applyWorkflowFilters(qb, DialectPostgres, filter, "l.id")
		qb.AddCondition("p.tenant_id = %s", DefaultTenantUUID)
		query, args := qb.SQL()
		conn, err := db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		// Sequential scans AND plain index scans are off, so the only way to use an index on query_state is a
		// bitmap scan on it. With one row in the table the planner would otherwise pick the primary key and
		// filter, which says nothing about whether the predicate can use a GIN index.
		for _, setting := range []string{"enable_seqscan", "enable_indexscan", "enable_indexonlyscan"} {
			if _, err := conn.ExecContext(ctx, `SET `+setting+` = off`); err != nil {
				t.Fatal(err)
			}
		}
		return explainText(t, ctx, conn, query, args)
	}

	// 1. No index: it works, and the planner has nothing to use.
	_, _ = db.Exec(`DROP INDEX IF EXISTS idx_payloads_query_state`)
	list("without the index")
	if p := plan(); strings.Contains(p, "idx_payloads_query_state") {
		t.Fatalf("UNMEASURED: the index was dropped and the plan still names it:\n%s", p)
	}

	// 2. With the operator's index: same answer, and the plan uses it.
	if _, err := db.Exec(`CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_payloads_query_state ON workflow_payloads USING gin (query_state jsonb_path_ops)`); err != nil {
		t.Fatalf("creating the documented index: %v", err)
	}
	list("with the index")
	if p := plan(); !strings.Contains(p, "idx_payloads_query_state") {
		t.Errorf("an index on query_state with jsonb_path_ops was added and the statement does not use it, so the filter is not in the containment form the index serves:\n%s", p)
	}
}

func explainText(t *testing.T, ctx context.Context, conn *sql.Conn, query string, args []any) string {
	t.Helper()
	rows, err := conn.QueryContext(ctx, "EXPLAIN "+query, args...)
	if err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}
