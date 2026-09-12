package engine_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	_ "github.com/lib/pq"

	"github.com/cleat-team/cleat/engine/testutil"
)

// A tenant's own login role must not be able to destroy another tenant.
// cleat#1365.
//
// WHAT THE BUG WAS. admin.drop_tenant is SECURITY DEFINER, so it executes as
// its owner and does not consult row-level security, and PostgreSQL's default
// for a new function is EXECUTE TO PUBLIC. admin.create_tenant_role grants
// every tenant role USAGE ON SCHEMA admin. Those two together meant any
// authenticated tenant could call it on any other tenant's id.
//
// WHY THE CONTROL IS THE TEST. A permission test passes trivially if the
// fixture never had reach -- connect as the wrong role, or with RLS already
// hiding everything, and "denied" proves nothing. So this asserts three things
// in one run, and the first two are what give the third meaning:
//
//  1. the attacker is a REAL tenant role: it authenticates, and RLS shows it
//     zero of the victim's rows -- so isolation is working for direct access
//  2. a caller that SHOULD be able to drop still can (the owner), so the fix
//     narrowed the grant rather than removing the capability
//  3. the attacker is refused, and the victim's rows are still there
//
// Before 065 this test fails on (3) with err=<nil> and the victim's rows gone.
func TestATenantRoleCannotDropAnotherTenant(t *testing.T) {
	db := testutil.TestDB(t, testutil.DialectPostgres)
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)
	ctx := context.Background()

	// Roles are CLUSTER-global while databases are not, so a fixed name would
	// collide with a concurrent run of this test against the same server --
	// and with other sessions' databases on a shared container. Random ids
	// keep the role names unique; the cleanup below removes them either way.
	attacker, victim := uuid.New(), uuid.New()
	const pw = "0123456789abcdef0123456789abcdef" // >= 32, what 064 requires

	var attackerRole, victimRole string
	for _, tc := range []struct {
		id   uuid.UUID
		role *string
	}{{attacker, &attackerRole}, {victim, &victimRole}} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO admin.tenants (tenant_id, name, display_name)
			 VALUES ($1, $2, $2) ON CONFLICT (tenant_id) DO NOTHING`,
			tc.id, "t-"+tc.id.String()[:8]); err != nil {
			t.Fatalf("seed tenant %s: %v", tc.id, err)
		}
		if err := db.QueryRowContext(ctx,
			`SELECT admin.create_tenant_role($1, $2)`, tc.id, pw).Scan(tc.role); err != nil {
			t.Fatalf("create_tenant_role(%s): %v", tc.id, err)
		}
	}
	t.Cleanup(func() {
		free := context.WithoutCancel(ctx)
		for _, r := range []string{attackerRole, victimRole} {
			if r == "" {
				continue
			}
			_, _ = db.ExecContext(free, fmt.Sprintf("DROP OWNED BY %q CASCADE", r))
			_, _ = db.ExecContext(free, fmt.Sprintf("DROP ROLE IF EXISTS %q", r))
		}
	})

	// The victim has something to lose. tenant_settings cascades off
	// admin.tenants, so it disappears exactly when the tenant row does --
	// which is what makes it the thing to count.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO tenant_settings (tenant_id) VALUES ($1)
		 ON CONFLICT (tenant_id) DO NOTHING`, victim); err != nil {
		t.Fatalf("seed victim settings: %v", err)
	}

	attackerDB := connectAs(t, db, attackerRole, pw)
	defer attackerDB.Close()

	// (1) The attacker is a real, authenticated tenant role, and RLS is
	//     working: it sees none of the victim's rows.
	var who string
	var isSuper bool
	if err := attackerDB.QueryRowContext(ctx,
		`SELECT current_user, (SELECT rolsuper FROM pg_roles WHERE rolname = current_user)`).
		Scan(&who, &isSuper); err != nil {
		t.Fatalf("attacker identity: %v", err)
	}
	if who != attackerRole || isSuper {
		t.Fatalf("attacker connected as %q (super=%v), want %q non-superuser",
			who, isSuper, attackerRole)
	}
	var visible int
	if err := attackerDB.QueryRowContext(ctx,
		`SELECT count(*) FROM tenant_settings WHERE tenant_id = $1`, victim).Scan(&visible); err != nil {
		t.Fatalf("attacker reading the victim's settings: %v", err)
	}
	if visible != 0 {
		t.Fatalf("the attacker can already SEE %d of the victim's tenant_settings "+
			"rows, so this test is not measuring what it claims: RLS is not "+
			"isolating these two tenants and the drop below would be the "+
			"smaller of two problems", visible)
	}

	// (3) The attack itself.
	_, err := attackerDB.ExecContext(ctx, `SELECT admin.drop_tenant($1)`, victim)
	if err == nil {
		t.Error("a tenant login role successfully called admin.drop_tenant() on " +
			"ANOTHER tenant.\n\n" +
			"It reads none of that tenant's rows -- asserted above -- and can " +
			"destroy all of them, because a SECURITY DEFINER function executes " +
			"as its owner and never consults row-level security. PostgreSQL " +
			"grants EXECUTE to PUBLIC by default; 065 revokes it.")
	} else if !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("the attacker was refused, but not by the grant: %v\n\n"+
			"Want 'permission denied for function drop_tenant'. Another error "+
			"means the call failed for a reason that might not survive a "+
			"change elsewhere.", err)
	}

	var survives int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM admin.tenants WHERE tenant_id = $1`, victim).Scan(&survives); err != nil {
		t.Fatalf("counting the victim: %v", err)
	}
	if survives != 1 {
		t.Errorf("the victim's admin.tenants row is gone (count=%d)", survives)
	}

	// (2) The known-good call: the capability still exists for a caller that
	//     holds the grant. Without this, a database in which drop_tenant is
	//     broken for everyone passes everything above.
	if _, err := db.ExecContext(ctx, `SELECT admin.drop_tenant($1)`, victim); err != nil {
		t.Fatalf("the owner can no longer drop a tenant either: %v\n\n"+
			"065 narrows the grant; it must not remove the capability.", err)
	}
	var afterOwner int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM admin.tenants WHERE tenant_id = $1`, victim).Scan(&afterOwner); err != nil {
		t.Fatalf("counting the victim after the owner's drop: %v", err)
	}
	if afterOwner != 0 {
		t.Errorf("the owner's drop_tenant left the tenant row in place (count=%d), "+
			"so the 'refused' result above cannot be distinguished from "+
			"drop_tenant not working at all", afterOwner)
	}
}

// connectAs opens a connection authenticated as role, derived from the DSN the
// suite is already using so it reaches the same server and database.
func connectAs(t *testing.T, db *sql.DB, role, password string) *sql.DB {
	t.Helper()
	var dbName string
	if err := db.QueryRow(`SELECT current_database()`).Scan(&dbName); err != nil {
		t.Fatalf("current_database: %v", err)
	}
	base := testutil.PostgresTestDSN()
	at := strings.LastIndex(base, "@")
	scheme := strings.Index(base, "://")
	if at < 0 || scheme < 0 {
		t.Fatalf("cannot rewrite credentials into DSN %q", base)
	}
	hostAndPath := base[at+1:]
	if slash := strings.Index(hostAndPath, "/"); slash >= 0 {
		q := ""
		if qi := strings.Index(hostAndPath, "?"); qi >= 0 {
			q = hostAndPath[qi:]
		}
		hostAndPath = hostAndPath[:slash] + "/" + dbName + q
	}
	dsn := base[:scheme+3] + role + ":" + password + "@" + hostAndPath
	conn, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open as %s: %v", role, err)
	}
	if err := conn.Ping(); err != nil {
		conn.Close()
		t.Fatalf("authenticate as %s: %v\n\n"+
			"The attacker in this test must be a real, authenticated tenant "+
			"role. If it cannot connect, nothing below measures anything.", role, err)
	}
	return conn
}
