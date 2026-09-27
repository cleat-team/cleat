package main

import (
	"context"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/google/uuid"
)

// cleat#1565. An allowlist nobody can write is a table, not a policy --
// tenant_settings sat in exactly that state for months (cleat#1187), which is
// why the management surface ships with the enforcement rather than after it.
func TestEgressAllowWorksOnEveryDialect(t *testing.T) {
	for _, tc := range []struct {
		name string
		td   testutil.Dialect
		d    dialect
	}{
		{"postgres", testutil.DialectPostgres, dialectPostgres},
		{"mysql", testutil.DialectMySQL, dialectMySQL},
		{"mssql", testutil.DialectMSSQL, dialectMSSQL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testutil.TestDB(t, tc.td)
			ctx := context.Background()

			// The DEFAULT tenant, not a fresh UUID: the table has a foreign
			// key to tenants, which is deliberate -- it is what makes
			// drop-tenant remove a tenant's allowlist with it. So a test
			// cannot invent a tenant id, and inventing one is what the first
			// version of this did.
			tenant := "00000000-0000-0000-0000-000000000000"
			t.Cleanup(func() {
				for _, h := range []string{"api.example.com", ".corp.example"} {
					_, _ = egressRemove(context.Background(), db, tc.d, tenant, h)
				}
			})

			// A tenant with no rows must read as an EMPTY list, not an error.
			// Empty is a meaningful answer here -- it denies everything.
			got, err := egressList(ctx, db, tc.d, tenant)
			if err != nil {
				t.Fatalf("list on an empty allowlist: %v", err)
			}
			if len(got) != 0 {
				t.Fatalf("a fresh tenant already has %v", got)
			}

			if err := egressAdd(ctx, db, tc.d, tenant, "api.example.com"); err != nil {
				t.Fatalf("add: %v", err)
			}
			// Twice: add is remove-then-insert, so a repeat must not collide
			// on the primary key.
			if err := egressAdd(ctx, db, tc.d, tenant, "api.example.com"); err != nil {
				t.Fatalf("adding the same host twice: %v", err)
			}
			if err := egressAdd(ctx, db, tc.d, tenant, ".corp.example"); err != nil {
				t.Fatalf("add suffix form: %v", err)
			}

			got, err = egressList(ctx, db, tc.d, tenant)
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			if len(got) != 2 || got[0] != ".corp.example" || got[1] != "api.example.com" {
				t.Fatalf("list returned %v, want the two entries sorted", got)
			}

			n, err := egressRemove(ctx, db, tc.d, tenant, "api.example.com")
			if err != nil {
				t.Fatalf("remove: %v", err)
			}
			if n != 1 {
				t.Errorf("remove reported %d rows, want 1", n)
			}
			n, err = egressRemove(ctx, db, tc.d, tenant, "not.listed.example")
			if err != nil {
				t.Fatalf("remove of an absent host errored: %v", err)
			}
			if n != 0 {
				t.Errorf("removing an absent host reported %d rows, want 0", n)
			}

			// A different tenant sees nothing: the predicate is per tenant.
			// A read needs no tenants row, so an invented id is fine here and
			// is the point -- it must not see the default tenant's entries.
			other := uuid.New().String()
			if got, err := egressList(ctx, db, tc.d, other); err != nil || len(got) != 0 {
				t.Errorf("a different tenant sees %v (err %v); the query is not scoped", got, err)
			}
		})
	}
}

// The command refuses entries the worker would never match, rather than storing
// them and leaving an operator staring at a listing that contains the host they
// think they permitted.
func TestAnUnmatchableEntryIsRefusedRatherThanStored(t *testing.T) {
	for _, tc := range []struct{ in, why string }{
		{"https://example.com", "a URL, not a host"},
		{"example.com/path", "carries a path"},
		{"example.com:8443", "carries a port -- the list is per host"},
		{"*.example.com", "a wildcard; the form is .example.com"},
		{"", "empty"},
		{"   ", "whitespace only"},
		{".", "just a dot"},
	} {
		if _, err := normaliseEgressHost(tc.in); err == nil {
			t.Errorf("%q was accepted (%s)", tc.in, tc.why)
		}
	}
	for _, tc := range []struct{ in, want string }{
		{"Example.COM", "example.com"},
		{"  api.example.com  ", "api.example.com"},
		{"example.com.", "example.com"},
		{".corp.example", ".corp.example"},
	} {
		got, err := normaliseEgressHost(tc.in)
		if err != nil {
			t.Errorf("%q was refused: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("normalise(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
