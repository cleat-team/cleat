package auth

import (
	"strings"
	"testing"
)

// API key creation was PostgreSQL-only. CreateAPIKey wrote
// `INSERT INTO admin.tenant_api_keys ... VALUES ($1, $2, $3)` whatever it was
// given, so on MySQL -- which has no `admin` schema, schema and database being
// one namespace there -- it failed with:
//
//	Error 1049 (42000): Unknown database 'admin'
//
// and cmd/cleat-worker reported it as "check that the tenant UUID exists",
// which is the wrong thing: the tenant existed, the dialect was wrong.
//
// The engine's READ path already knew all of this. MySQLStore.ResolveTenantFromAPIKey
// queries an unqualified `tenant_api_keys` with `?`, and the MSSQL one carries
// a comment about the schema qualifier specifically. Only the write side was
// PostgreSQL-only, which is #769's shape: an unwired write path behind a
// complete read path.
//
// NOT FIXED BY THIS, and asserted nowhere here because it is not a statement
// problem: on MySQL the key is written to the base database named in the DSN
// and read back through a PER-TENANT store, which is a different database
// entirely. Measured -- base 2 rows, tenant database 0. Filed separately.
func TestCreateAPIKeyStatementPerDialect(t *testing.T) {
	for _, tc := range []struct {
		dialect     string
		wantTable   string
		wantPlace   string
		wantKeyID   bool
		notContains string
		why         string
	}{
		{
			dialect:   DialectPostgres,
			wantTable: "INSERT INTO admin.tenant_api_keys", wantPlace: "$1",
			wantKeyID: false, notContains: "?",
			why: "PostgreSQL keeps tenants in an admin schema and defaults key_id from gen_random_uuid()",
		},
		{
			dialect:   DialectMySQL,
			wantTable: "INSERT INTO tenant_api_keys", wantPlace: "?",
			wantKeyID: true, notContains: "admin.",
			why: "MySQL has no admin schema -- `admin.` is read as a DATABASE and fails 1049 -- and key_id has no default",
		},
		{
			dialect:   DialectMSSQL,
			wantTable: "INSERT INTO admin.tenant_api_keys", wantPlace: "@p1",
			wantKeyID: false, notContains: "$1",
			why: "SQL Server has the admin schema and defaults key_id from NEWID(), but names parameters @pN",
		},
	} {
		t.Run(tc.dialect, func(t *testing.T) {
			stmt, needsKeyID := createAPIKeyStmt(tc.dialect)

			if !strings.Contains(stmt, tc.wantTable) {
				t.Errorf("statement does not target %q: %s\n%s", tc.wantTable, stmt, tc.why)
			}
			if !strings.Contains(stmt, tc.wantPlace) {
				t.Errorf("statement does not use %q placeholders: %s\n%s", tc.wantPlace, stmt, tc.why)
			}
			if tc.notContains != "" && strings.Contains(stmt, tc.notContains) {
				t.Errorf("statement contains %q, which is another dialect's spelling: %s\n%s",
					tc.notContains, stmt, tc.why)
			}
			if needsKeyID != tc.wantKeyID {
				t.Errorf("needsKeyID = %v, want %v -- %s", needsKeyID, tc.wantKeyID, tc.why)
			}
			if needsKeyID && !strings.Contains(stmt, "key_id") {
				t.Errorf("dialect needs key_id supplied but the statement does not name it: %s", stmt)
			}
		})
	}
}

// TestUnknownDialectIsRefused: falling back to PostgreSQL is what produced the
// original symptom -- a message about a missing database rather than about the
// driver -- so an unrecognised dialect must fail where it can still say so.
func TestUnknownDialectIsRefused(t *testing.T) {
	if _, err := NewTenantStoreForDialect(nil, "oracle"); err == nil {
		t.Error("NewTenantStoreForDialect accepted an unknown dialect; it must refuse rather " +
			"than silently emit PostgreSQL SQL to something else")
	}
	for _, d := range []string{DialectPostgres, DialectMySQL, DialectMSSQL} {
		if _, err := NewTenantStoreForDialect(nil, d); err != nil {
			t.Errorf("NewTenantStoreForDialect(%q): %v", d, err)
		}
	}
}

// TestTheTwoDeadMethodsRefuseNonPostgres. CreateTenant and RevokeAPIKey have no
// production caller (scripts/check-test-only-code.sh lists both), and their
// statements are still PostgreSQL-shaped -- RETURNING has no MySQL equivalent
// and SQL Server spells it OUTPUT. Rather than leave them emitting PostgreSQL
// SQL to whatever they are given, they refuse. Writing the per-dialect shapes
// for methods nothing calls would be inventing an untested API.
func TestTheTwoDeadMethodsRefuseNonPostgres(t *testing.T) {
	for _, d := range []string{DialectMySQL, DialectMSSQL} {
		s, err := NewTenantStoreForDialect(nil, d)
		if err != nil {
			t.Fatalf("NewTenantStoreForDialect(%q): %v", d, err)
		}
		if _, err := s.CreateTenant(t.Context(), "n", "d"); err == nil {
			t.Errorf("CreateTenant on %s did not refuse", d)
		}
		if err := s.RevokeAPIKey(t.Context(), [16]byte{}); err == nil {
			t.Errorf("RevokeAPIKey on %s did not refuse", d)
		}
	}
}
