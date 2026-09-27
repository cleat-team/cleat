package main

import (
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// The factories that pool per tenant say so, and the one that does not stays
// silent.
//
// THE CENSUS ASKS THIS QUESTION AND USED TO GUESS THE ANSWER. It inferred the
// per-tenant term from plugin.TenantPools, which exists only under
// --tenant-isolation=role and therefore only on PostgreSQL -- so it reported
// zero on MySQL and SQL Server, where a pool per tenant is unavoidable and the
// term is largest.
//
// A test on connectionBudget alone would not catch that: the struct was right
// and the value handed to it was wrong. This pins the source of the value.
func TestTheFactoriesThatPoolPerTenantSaySo(t *testing.T) {
	t.Run("SQL Server pools per tenant", func(t *testing.T) {
		f := engine.NewMSSQLStoreFactory("sqlserver://unused").WithTenantPoolMaxConns(17)
		pooler, ok := any(f).(engine.PerTenantPooler)
		if !ok {
			t.Fatal("MSSQLStoreFactory does not implement PerTenantPooler; its RLS reads " +
				"SESSION_CONTEXT, set per connection, so it cannot share a pool")
		}
		if got := pooler.TenantPoolMaxConns(); got != 17 {
			t.Errorf("TenantPoolMaxConns = %d, want 17 -- the census would size the term wrongly", got)
		}
	})

	t.Run("PostgreSQL does not", func(t *testing.T) {
		// The CONTROL, and the half that makes the assertions above mean
		// something. PostgresStoreFactory shares one *sql.DB and supplies the
		// tenant per transaction, so it must NOT claim a per-tenant ceiling --
		// a census that counted one there would overstate every PostgreSQL
		// worker by 25 connections per tenant.
		var f any = &engine.PostgresStoreFactory{}
		if _, ok := f.(engine.PerTenantPooler); ok {
			t.Error("PostgresStoreFactory implements PerTenantPooler, but it shares a single " +
				"pool; the census would count per-tenant connections that do not exist")
		}
	})
}

// The census's per-tenant term, decided by the function main() calls.
//
// This asserts the DECISION, not the arithmetic. The first version of this
// test built a connectionBudget and set TenantPerPool itself -- which is what
// main() does, so it looked equivalent -- and stayed green when the decision
// was reverted to its buggy form. Measured, by reverting it. The bug was never
// in connectionBudget; it was in the value handed to it, and only a test that
// calls this function can see that.
func TestThePerTenantCeilingCountsAFactorysPools(t *testing.T) {
	mssql := engine.NewMSSQLStoreFactory("sqlserver://unused").WithTenantPoolMaxConns(25)
	pg := &engine.PostgresStoreFactory{}
	rolePools := &struct{ notNil bool }{}

	for _, tc := range []struct {
		name      string
		rolePools any
		factory   any
		want      int
	}{
		{"SQL Server, no role isolation", nil, mssql, 25},
		{"PostgreSQL, no role isolation", nil, pg, 0},
		{"PostgreSQL with role isolation", rolePools, pg, 25},
		{"SQL Server with role isolation", rolePools, mssql, 25},
		{"no factory at all", nil, nil, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := perTenantPoolCeiling(tc.rolePools, tc.factory, 25); got != tc.want {
				t.Errorf("perTenantPoolCeiling = %d, want %d", got, tc.want)
			}
		})
	}
}

// And the term, once decided, reaches the headroom figure an operator reads.
func TestThePerTenantTermReachesTheHeadroom(t *testing.T) {
	budget := connectionBudget{Core: 10}
	if got := budget.TenantHeadroom(100); got != -1 {
		t.Errorf("with no per-tenant term, TenantHeadroom = %d; want -1 (not applicable)", got)
	}
	budget.TenantPerPool = perTenantPoolCeiling(nil,
		engine.NewMSSQLStoreFactory("sqlserver://unused").WithTenantPoolMaxConns(25), 25)
	// 100 - 10 fixed = 90, at 25 each = 3 tenants.
	if got := budget.TenantHeadroom(100); got != 3 {
		t.Errorf("TenantHeadroom(100) = %d, want 3; the census is not costing per-tenant pools", got)
	}
}
