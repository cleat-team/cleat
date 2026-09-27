package auth

import (
	"strings"
	"testing"

	"github.com/google/uuid"
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

// TestCreateTenantAndRevokeAPIKeyRefuseNonPostgres.
//
// This used to be named for a claim that was true of both methods: neither
// had a production caller (scripts/check-test-only-code.sh listed both), and
// their statements are still PostgreSQL-shaped -- RETURNING has no MySQL
// equivalent and SQL Server spells it OUTPUT. That stopped being true of
// CreateTenant when cmd/cleat-worker's --create-tenant flag started calling
// it (cleat#1114) -- RevokeAPIKey remains test-only. Renamed rather than left
// claiming "dead" about a method a real deployment now calls: whichever is
// or isn't dead changes over time, and the assertion below (refuses cleanly
// on a dialect it cannot serve) is the property worth keeping regardless.
func TestCreateTenantAndRevokeAPIKeyRefuseNonPostgres(t *testing.T) {
	for _, d := range []string{DialectMySQL, DialectMSSQL} {
		s, err := NewTenantStoreForDialect(nil, d)
		if err != nil {
			t.Fatalf("NewTenantStoreForDialect(%q): %v", d, err)
		}
		if _, err := s.CreateTenant(t.Context(), "n", "d", uuid.MustParse(DefaultOrgUUID)); err == nil {
			t.Errorf("CreateTenant on %s did not refuse", d)
		}
		if err := s.RevokeAPIKey(t.Context(), [16]byte{}); err == nil {
			t.Errorf("RevokeAPIKey on %s did not refuse", d)
		}
		// cleat#2352: same Postgres-only scope, same reason -- see
		// RevokeAPIKeyByHash's doc comment.
		if err := s.RevokeAPIKeyByHash(t.Context(), []byte{0x01}); err == nil {
			t.Errorf("RevokeAPIKeyByHash on %s did not refuse", d)
		}
	}
}

// TestResolveAPIKeyStmt_ExcludesExpired mirrors
// TestCreateAPIKeyStatementPerDialect's style: a cheap, DB-less check that
// cleat#2352's expiry clause is present, uses the right "now" spelling for
// each dialect, and did not accidentally introduce another dialect's
// placeholder style. TestExpiredKeyCannotAuthenticate (a_key_expiry_test.go)
// is what proves the clause actually behaves correctly against a real
// database; this test exists so a dialect mistake here fails in
// milliseconds rather than only when a database happens to be configured.
func TestResolveAPIKeyStmt_ExcludesExpired(t *testing.T) {
	for _, tc := range []struct {
		dialect string
		wantOr  string
	}{
		{DialectPostgres, "expires_at IS NULL OR expires_at > now()"},
		{DialectMySQL, "expires_at IS NULL OR expires_at > NOW(6)"},
		{DialectMSSQL, "expires_at IS NULL OR expires_at > SYSUTCDATETIME()"},
	} {
		t.Run(tc.dialect, func(t *testing.T) {
			stmt := resolveAPIKeyStmt(tc.dialect)
			if !strings.Contains(stmt, tc.wantOr) {
				t.Errorf("resolveAPIKeyStmt(%q) = %q, missing the expiry clause %q",
					tc.dialect, stmt, tc.wantOr)
			}
			if !strings.Contains(stmt, "disabled_at IS NULL") {
				t.Errorf("resolveAPIKeyStmt(%q) = %q, lost the existing disabled_at check "+
					"while gaining the expiry one", tc.dialect, stmt)
			}
		})
	}
}

// TestCreateAPIKeyStatementWritesTheOAuthColumns. cleat#2340.
//
// THE MySQL AND MSSQL SPELLINGS ARE EXECUTED BY NOTHING, and this is what
// covers them. The only caller of CreateOAuthAPIKey is oauthprovider's callback,
// which handleCallback reaches only after p.pgOnly has refused every other
// dialect with 501 -- so on MySQL and MSSQL this INSERT is unreachable code.
// The PostgreSQL spelling IS executed, by the real-dialect login test in that
// package, which resolves the minted key back through ResolveTenantFromAPIKey.
//
// Unreachable is not the same as untested-by-omission: the statement still has
// to be right the day OAuth login reaches those dialects, and a column named
// without a matching placeholder shifts every argument after it -- which is a
// key minted with its identity tag in the description column and silently
// nothing to revoke it by.
func TestCreateAPIKeyStatementWritesTheOAuthColumns(t *testing.T) {
	for _, dialect := range []string{DialectPostgres, DialectMySQL, DialectMSSQL} {
		t.Run(dialect, func(t *testing.T) {
			stmt, _ := createAPIKeyStmt(dialect)

			for _, col := range []string{"expires_at", "oauth_identity"} {
				if !strings.Contains(stmt, col) {
					t.Errorf("createAPIKeyStmt(%q) does not name %s, so an OAuth-minted key's %s "+
						"would be NULL there and the row would carry no expiry to sweep or no "+
						"identity to revoke: %s", dialect, col, col, stmt)
				}
			}

			// The column names and the values are two lists that must stay the
			// same length. Asserting only that the columns are named would miss
			// a placeholder left out, which is the shape that shifts arguments
			// rather than omitting them.
			open, close := strings.Index(stmt, "("), strings.Index(stmt, ")")
			lastOpen, lastClose := strings.LastIndex(stmt, "("), strings.LastIndex(stmt, ")")
			if open < 0 || close < open || lastOpen <= close || lastClose < lastOpen {
				t.Fatalf("cannot read the column and value lists out of %q", stmt)
			}
			cols := strings.Count(stmt[open+1:close], ",") + 1
			vals := strings.Count(stmt[lastOpen+1:lastClose], ",") + 1
			if cols != vals {
				t.Errorf("createAPIKeyStmt(%q) names %d columns but supplies %d values, so every "+
					"argument after the gap lands in the wrong column: %s", dialect, cols, vals, stmt)
			}
		})
	}
}
