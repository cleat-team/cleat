package main

import (
	"fmt"
	"strings"

	"github.com/cleat-team/cleat/engine"
)

// shardPoolMaxConns is the fixed per-shard ceiling, set at the shard pool's
// construction site in main.go. Named here so the census and the pool cannot
// disagree about it silently; if that literal moves, this must move with it.
const shardPoolMaxConns = 15

// migratePoolMaxConns is the migration pool's ceiling, likewise.
const migratePoolMaxConns = 2

// connectionBudget is what a worker's pools may open, broken out by pool.
//
// IT EXISTS BECAUSE NOTHING SUMMED THEM. cleat#1486: a worker opens six
// independent pools and no code anywhere added them up, so "how many
// connections does a worker use" had no answer except a hand calculation from
// flag defaults -- which is how the documented figure came to be `concurrency +
// 5`, the CORE pool alone, understating a default worker by a factor of five.
//
// Every field is a CEILING, not a count. The distinction matters more than it
// looks: a pool holds connections it has actually needed, not its maximum, so
// the sum below is what a worker may reach under load rather than what it costs
// at rest. Anything that reports live cost must read Stats(), not this.
type connectionBudget struct {
	Core    int // --concurrency + 5
	Plugin  int // --max-plugin-connections, 0 when no separate pool
	Flusher int // --batch-flush-max-connections, 0 when the flusher is off
	Shards  int // shardPoolMaxConns per configured shard
	Migrate int // migratePoolMaxConns, only with --migrate-db

	// TenantPerPool is --tenant-pool-max-conns: the ceiling for ONE tenant's
	// pool. It is deliberately not part of Fixed(), because the number of
	// tenant pools is a property of traffic rather than of configuration --
	// which is exactly why a worker's total cannot be stated as one number.
	TenantPerPool int
}

// Fixed is the ceiling of every pool whose count does not depend on traffic.
func (b connectionBudget) Fixed() int {
	return b.Core + b.Plugin + b.Flusher + b.Shards + b.Migrate
}

// TenantHeadroom is how many tenant pools fit under a budget, at their ceiling.
//
// A CEILING-BASED FLOOR: it assumes every tenant pool reaches
// TenantPerPool, which almost none do -- a pool serving two concurrent queries
// holds two connections, and one untouched for a ConnMaxLifetime holds none. So
// this is the number of tenants a worker can serve in the WORST case, and the
// real capacity is higher. Reporting the pessimistic figure is deliberate: a
// budget that is only satisfied when tenants behave is not a budget.
//
// Returns 0 when the fixed pools already exhaust the budget, and -1 when
// TenantPerPool is 0 (no tenant pools configured), so a caller can tell "no
// room" from "not applicable".
func (b connectionBudget) TenantHeadroom(budget int) int {
	if b.TenantPerPool <= 0 {
		return -1
	}
	spare := budget - b.Fixed()
	if spare <= 0 {
		return 0
	}
	return spare / b.TenantPerPool
}

// Describe renders the census for a log line, one pool per term.
//
// Written out rather than totalled because the total is what misled everyone:
// `concurrency + 5` was quoted as a worker's cost for as long as nobody saw the
// other five terms beside it.
func (b connectionBudget) Describe() string {
	var parts []string
	add := func(name string, n int) {
		if n > 0 {
			parts = append(parts, fmt.Sprintf("%s=%d", name, n))
		}
	}
	add("core", b.Core)
	add("plugin", b.Plugin)
	add("flusher", b.Flusher)
	add("shards", b.Shards)
	add("migrate", b.Migrate)
	s := strings.Join(parts, " + ")
	if s == "" {
		s = "none"
	}
	s += fmt.Sprintf(" = %d fixed", b.Fixed())
	if b.TenantPerPool > 0 {
		s += fmt.Sprintf("; plus %d per tenant pool", b.TenantPerPool)
	}
	return s
}

// checkConnectionBudget refuses a budget the fixed pools cannot fit inside.
//
// REFUSES RATHER THAN WARNS, and only when a budget was explicitly configured.
// A worker whose fixed pools already exceed its stated budget cannot honour it
// under any traffic -- the first tenant pool puts it further over, and there is
// nothing eviction can reclaim, because the fixed pools are not evictable. That
// is a configuration error the operator can fix in one line, and the failure it
// otherwise produces is connection exhaustion in production under load.
//
// A zero budget means "not configured" and checks nothing. The flag is new, so
// every existing deployment gets the census in its log and no new failure mode.
func checkConnectionBudget(budget int, b connectionBudget) error {
	if budget <= 0 {
		return nil
	}
	if fixed := b.Fixed(); fixed > budget {
		return fmt.Errorf(
			"--connection-budget=%d is smaller than this worker's fixed pools (%s). "+
				"Those pools are opened at startup and are not evictable, so the budget "+
				"cannot be honoured under any traffic. Raise the budget, or lower "+
				"--concurrency, --max-plugin-connections or --batch-flush-max-connections",
			budget, b.Describe())
	}
	return nil
}

// perTenantPoolCeiling is the census's per-tenant term: the ceiling for ONE
// tenant's pool, or 0 when this worker opens no pool per tenant.
//
// EXTRACTED SO IT CAN BE TESTED. It lived inline in main(), which nothing can
// call, and the bug it had was not in the arithmetic -- connectionBudget was
// right -- but in the value handed to it. A test over connectionBudget alone
// passes with this decision made wrongly, which is measured rather than
// assumed: reverting this to its old form leaves such a test green.
//
// Two independent sources, and either is enough:
//
//   - rolePools: plugin.TenantPools, built only under --tenant-isolation=role
//     and therefore only on PostgreSQL.
//   - factory: MySQL and SQL Server pool per tenant BY CONSTRUCTION, because
//     each scopes a tenant to something a shared pool cannot carry -- a
//     database, and a per-connection SESSION_CONTEXT respectively.
//
// Gating on the first alone reported zero on exactly the two dialects that
// always have the term. Both read the same flag, so the max is not a
// reconciliation, just the answer when both apply.
func perTenantPoolCeiling(rolePools any, factory any, flagValue int) int {
	ceiling := 0
	if rolePools != nil {
		ceiling = flagValue
	}
	if pooler, ok := factory.(engine.PerTenantPooler); ok {
		if n := pooler.TenantPoolMaxConns(); n > ceiling {
			ceiling = n
		}
	}
	return ceiling
}
