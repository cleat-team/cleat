package auth

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// cleat#3552, B2b-1, executed against real databases. The DB-free tests in
// a_scoped_operator_key_reaches_only_its_routes_test.go prove the gate; these prove the gate is
// fed the right thing: what is written is what is read back, on every dialect, through the
// columns migration 004 adds.

// legacyOperatorInsert is the INSERT as it was BEFORE migration 004: no scopes, no principal. A
// row written this way is what every operator key in an existing deployment is, so it is the
// only way to show the column default keeps such a key exactly as powerful as it was.
func legacyOperatorInsert(dialect string) string {
	switch dialect {
	case DialectMySQL:
		return `INSERT INTO operator_api_keys (key_id, key_hash, description) VALUES (?, ?, ?)`
	case DialectMSSQL:
		return `INSERT INTO admin.operator_api_keys (key_id, key_hash, description) VALUES (@p1, @p2, @p3)`
	default:
		return `INSERT INTO admin.operator_api_keys (key_id, key_hash, description) VALUES ($1, $2, $3)`
	}
}

func TestAScopedOperatorKeyRoundTripsOnEveryDialect(t *testing.T) {
	for _, dialect := range []string{DialectPostgres, DialectMySQL, DialectMSSQL} {
		t.Run(dialect, func(t *testing.T) {
			store := operatorStoreForTest(t, dialect)
			ctx := context.Background()
			tag := uuid.New().String() // the database is persistent; this run's rows are told apart by it

			// A provision key, with the principal it was issued to.
			raw := GenerateOperatorKey()
			made, err := store.CreateOperatorKey(ctx, raw, OperatorKeySpec{
				Description: "provisioner " + tag, Scopes: []string{ScopeProvision}, Principal: "clew-service",
			})
			if err != nil {
				t.Fatalf("CreateOperatorKey(provision) on %s: %v", dialect, err)
			}
			if made.Scopes != ScopeProvision || made.Principal != "clew-service" {
				t.Errorf("Create returned scopes %q principal %q", made.Scopes, made.Principal)
			}
			got, err := store.ResolveOperatorFromAPIKey(ctx, operatorHash(raw))
			if err != nil {
				t.Fatalf("a provision key did not resolve on %s: %v", dialect, err)
			}
			if got.Scopes != ScopeProvision || got.Principal != "clew-service" {
				t.Errorf("resolved scopes %q principal %q, want %q and clew-service", got.Scopes, got.Principal, ScopeProvision)
			}
			// What was stored drives the gate, end to end: the create route and nothing else.
			if !got.MayReach(http.MethodPost, "/api/admin/tenants") {
				t.Error("the resolved provision key cannot reach the create route")
			}
			if got.MayReach(http.MethodPost, "/api/admin/drain") {
				t.Error("the resolved provision key can reach /api/admin/drain")
			}

			// Several scopes store canonically, whatever order they were given in.
			multiRaw := GenerateOperatorKey()
			if _, err := store.CreateOperatorKey(ctx, multiRaw, OperatorKeySpec{
				Description: "both " + tag, Scopes: []string{ScopeProvision, ScopeAdmin}, Principal: "ops",
			}); err != nil {
				t.Fatalf("CreateOperatorKey(both): %v", err)
			}
			multi, err := store.ResolveOperatorFromAPIKey(ctx, operatorHash(multiRaw))
			if err != nil || multi.Scopes != "admin provision" {
				t.Errorf("a two-scope key resolved to %q, %v; want %q", multi.Scopes, err, "admin provision")
			}

			// An admin key needs no principal and reads back with none.
			adminRaw := GenerateOperatorKey()
			if _, err := store.CreateOperatorKey(ctx, adminRaw, adminSpec("admin "+tag, nil)); err != nil {
				t.Fatalf("CreateOperatorKey(admin): %v", err)
			}
			adm, err := store.ResolveOperatorFromAPIKey(ctx, operatorHash(adminRaw))
			if err != nil || adm.Scopes != ScopeAdmin || adm.Principal != "" {
				t.Errorf("an admin key resolved to scopes %q principal %q, %v", adm.Scopes, adm.Principal, err)
			}

			// A key written BEFORE the columns existed keeps full reach: the default is what
			// keeps every existing deployment's keys exactly as they were.
			legacyRaw := GenerateOperatorKey()
			legacyHash := operatorHash(legacyRaw)
			if _, err := store.db.ExecContext(ctx, legacyOperatorInsert(dialect), uuid.New().String(), legacyHash, "legacy "+tag); err != nil {
				t.Fatalf("UNMEASURED: write a pre-004 style row on %s: %v", dialect, err)
			}
			legacy, err := store.ResolveOperatorFromAPIKey(ctx, legacyHash)
			if err != nil {
				t.Fatalf("a key written without the new columns did not resolve: %v", err)
			}
			if legacy.Scopes != ScopeAdmin || legacy.Principal != "" {
				t.Errorf("a pre-004 key resolved to scopes %q principal %q, want admin and none", legacy.Scopes, legacy.Principal)
			}
			if !legacy.MayReach(http.MethodPost, "/api/admin/drain") {
				t.Error("a pre-004 key lost its reach to /api/admin/drain")
			}

			// The list carries both columns.
			keys, err := store.ListOperatorKeys(ctx)
			if err != nil {
				t.Fatalf("ListOperatorKeys: %v", err)
			}
			var listed Operator
			for _, k := range keys {
				if k.KeyID == made.KeyID {
					listed = k
				}
			}
			if listed.KeyID == "" || listed.Scopes != ScopeProvision || listed.Principal != "clew-service" {
				t.Errorf("the list shows %+v for the provision key", listed)
			}

			// Refusals write NOTHING. Counted by this run's tag, so another test's rows cannot hide a write.
			for name, spec := range map[string]OperatorKeySpec{
				"provision without a principal": {Description: "refused-a " + tag, Scopes: []string{ScopeProvision}},
				"a malformed principal":         {Description: "refused-b " + tag, Scopes: []string{ScopeAdmin}, Principal: "Not Valid"},
				"an unknown scope":              {Description: "refused-c " + tag, Scopes: []string{"bogus"}},
				"no scope at all":               {Description: "refused-d " + tag},
			} {
				if _, err := store.CreateOperatorKey(ctx, GenerateOperatorKey(), spec); err == nil {
					t.Errorf("%s was accepted", name)
				}
			}
			after, err := store.ListOperatorKeys(ctx)
			if err != nil {
				t.Fatalf("ListOperatorKeys after the refusals: %v", err)
			}
			for _, k := range after {
				if strings.HasPrefix(k.Description, "refused-") && strings.HasSuffix(k.Description, tag) {
					t.Errorf("a refused mint still wrote a row: %q", k.Description)
				}
			}
		})
	}
}

// Migration 004 must be safe to run twice. PostgreSQL has ADD COLUMN IF NOT EXISTS and SQL Server
// a COL_LENGTH guard; MySQL has neither, which is why its file is one atomic ALTER and not
// something this test applies again.
func TestTheOperatorKeyScopeMigrationIsSafeToRunTwice(t *testing.T) {
	for _, dialect := range []string{DialectPostgres, DialectMSSQL} {
		t.Run(dialect, func(t *testing.T) {
			store := operatorStoreForTest(t, dialect)
			raw, err := os.ReadFile(filepath.Join("..", "migrations", dialect, "004_operator_key_scopes.sql"))
			if err != nil {
				t.Fatalf("UNMEASURED: read the migration: %v", err)
			}
			// The runner splits SQL Server files on GO; here the batches are run one at a time.
			batches := []string{string(raw)}
			if dialect == DialectMSSQL {
				batches = nil
				for _, b := range strings.Split(string(raw), "\nGO") {
					if strings.TrimSpace(b) != "" {
						batches = append(batches, b)
					}
				}
			}
			// TestDB has already applied it once; this is the second and third time.
			for run := 2; run <= 3; run++ {
				for _, b := range batches {
					if _, err := store.db.Exec(b); err != nil {
						t.Fatalf("running migration 004 again (run %d) on %s: %v", run, dialect, err)
					}
				}
			}
		})
	}
}
