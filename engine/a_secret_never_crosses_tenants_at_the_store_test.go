package engine

// cleat#1992. Before exposing PutSecret/GetSecret/RetireSecret to plugin Go
// code through a new plugin.Environment.Secrets interface, the design needs a
// real answer to "can tenant B's context read tenant A's row through these
// three methods, on the connection a plugin actually gets" -- not the crypto-
// level answer TestASecretDoesNotOpenUnderAnotherTenant already gives (that
// test never touches a database; it calls seal/open directly).
//
// TWO separate protections exist and this file is what tells them apart:
//   - PostgreSQL and SQL Server enforce tenant scoping AT THE DATABASE, via
//     RLS (Postgres) and a SECURITY POLICY (SQL Server) -- see
//     081_a_secret_never_reaches_the_guest.sql and
//     073_a_secret_never_reaches_the_guest.sql. A predicate-dropping bug in
//     execTenantScoped's SQL would still be refused.
//   - MySQL has no equivalent, and that is not a gap: #2052 records the
//     owner's decision that MySQL is single-tenant, with isolation enforced
//     as a static predicate guard over source
//     (engine/mysql_tenant_predicate_test.go), not a database-level
//     mechanism. tenant_secrets_test.go's TestASecretDoesNotOpenUnderAnother
//     Tenant shows the ciphertext wouldn't OPEN correctly even if read, which
//     is real protection on every dialect; it is not the same claim as "the
//     row cannot be READ", and engine/readonlydb.go's own doc comment is the
//     reminder that those two claims get confused. This file measures the
//     read, not the decrypt.
//
// MYSQL IS NOT IN THIS TEST'S DIALECT SET, and that is not the same
// exemption as the one above. This test needs a SECOND tenant to exist so it
// can seed a row under one and read it as the other, and MySQL migration 038
// (`uq_tenants_mysql_is_single_tenant_only_see_tiers_yaml_d1`) makes that
// impossible: inserting a second row into `tenants` fails with a unique-key
// violation, at the database, for every client -- this test included. A
// cross-tenant probe cannot run on a dialect where a second tenant cannot
// exist. Confirmed 2026-09-23 on cleat#2159's own CI run: the mysql subtest
// failed with exactly that duplicate-key error out of
// seedSecondTenantForTest, not out of anything this test is trying to
// measure. Excluded explicitly in the dialect list below rather than
// skipped at runtime, so the exclusion is visible in the test's own source
// instead of in a log line.
//
// WHAT THIS DOES NOT PROVE. Every call here reaches the store through the
// request's own tenant CONTEXT (e.ctx(tenant)), matching how a request-path
// caller would use it. It says nothing about a caller that passes the wrong
// tenantID as a plain function argument -- RLS and the security policy both
// protect the SESSION's tenant, and a parameter IS what sets that session's
// tenant, so a wrong parameter and a correct one are indistinguishable to the
// database. That is the argument for plugin.Environment.Secrets taking no
// tenantID parameter at all on the request path (posted on #1992) rather than
// mirroring SecretStore's signature: no parameter, nothing to get wrong.
//
// EVERY CALL HERE RUNS AS cleat_app -- newRotationEnv's worker connection,
// the same role plugin.Environment.DB and any future plugin.Environment.
// Secrets would use. A test run as the schema owner would prove nothing:
// see a_secret_rotation_test.go's own comment on the same point.

import (
	"errors"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/google/uuid"
)

func seedSecondTenantForTest(t *testing.T, e *rotationEnv) uuid.UUID {
	t.Helper()
	id := uuid.New()
	ins := map[testutil.Dialect]string{
		testutil.DialectPostgres: `INSERT INTO admin.tenants (tenant_id, name) VALUES ($1, $2)`,
		testutil.DialectMSSQL:    `INSERT INTO admin.tenants (tenant_id, name) VALUES (@p1, @p2)`,
	}[e.dialect]
	if _, err := e.owner.Exec(ins, id.String(), "cleat-1992-secretcross-"+id.String()[:8]); err != nil {
		t.Fatalf("seed a second tenant: %v", err)
	}
	del := map[testutil.Dialect]string{
		testutil.DialectPostgres: `DELETE FROM admin.tenants WHERE tenant_id = $1`,
		testutil.DialectMSSQL:    `DELETE FROM admin.tenants WHERE tenant_id = @p1`,
	}[e.dialect]
	t.Cleanup(func() { e.owner.Exec(del, id.String()) }) //nolint:errcheck // best-effort cleanup
	return id
}

// TestGetSecretRefusesAnotherTenantsRowOnEveryApplicableDialect is the
// known-positive half deliberately kept in the SAME test as the refusal, not
// split out: a probe that only ever asserts "not found" cannot tell
// "correctly refused" from "the whole store is broken and finds nothing for
// anyone" (the empty-table trap CLAUDE.md records against ReadOnlyDB's own
// #1285/#1286 probe). Tenant A's own read succeeding is what rules that out.
//
// "EveryApplicableDialect", not "EveryDialect" -- see the file comment above
// for why MySQL is not in the loop below.
func TestGetSecretRefusesAnotherTenantsRowOnEveryApplicableDialect(t *testing.T) {
	for _, dialect := range []testutil.Dialect{testutil.DialectPostgres, testutil.DialectMSSQL} {
		t.Run(string(dialect), func(t *testing.T) {
			e := newRotationEnv(t, dialect)
			tenantA := e.tenants[0]
			tenantB := seedSecondTenantForTest(t, e)
			e.claim(t, tenantA, "cross_tenant_probe")

			ring, err := NewKeyRing(rotV1)
			if err != nil {
				t.Fatalf("NewKeyRing: %v", err)
			}
			store := e.store(ring)
			if err := store.PutSecret(e.ctx(tenantA), tenantA.String(), "cross_tenant_probe", "tenant-a-only-value"); err != nil {
				t.Fatalf("PutSecret under tenant A: %v", err)
			}

			// Known-positive: tenant A reading its own row must succeed, and
			// with the right value, or a "not found" below proves nothing.
			got, err := store.GetSecret(e.ctx(tenantA), tenantA.String(), "cross_tenant_probe")
			if err != nil {
				t.Fatalf("UNMEASURED: tenant A could not read its own just-written secret: %v", err)
			}
			if got != "tenant-a-only-value" {
				t.Fatalf("tenant A read %q, want %q", got, "tenant-a-only-value")
			}

			// The refusal under test: tenant B's context, same store, same
			// name. Both remaining dialects enforce this at the database
			// (RLS on Postgres, a SECURITY POLICY on SQL Server), and it
			// typically surfaces as a distinct error rather than a plain
			// not-found -- either is an acceptable refusal, a nil error is
			// not.
			if _, err := store.GetSecret(e.ctx(tenantB), tenantA.String(), "cross_tenant_probe"); err == nil {
				t.Fatal("tenant B read tenant A's secret through GetSecret")
			} else if !errors.Is(err, ErrSecretNotFound) {
				t.Logf("tenant B's read was refused with: %v", err)
			}
		})
	}
}
