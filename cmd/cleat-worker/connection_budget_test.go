package main

import (
	"os"
	"strings"
	"testing"
)

// The census is the thing cleat#1486 says does not exist: six pools, nothing
// summing them.
//
// THE DEFAULT WORKER IS THE CASE THAT MATTERS. Its documented cost was
// `concurrency + 5` for as long as nobody looked at the other five terms, which
// understated it by a factor of five. This pins the arithmetic so that reading
// one term as the whole is a failing test rather than a doc correction.
func TestTheDefaultWorkerIsNotJustItsCorePool(t *testing.T) {
	// The defaults as ci.yml and the flag definitions have them: --concurrency
	// 10, --max-plugin-connections 10, --batch-flush-max-connections 50, no
	// shards, no --migrate-db, --tenant-pool-max-conns 25.
	b := connectionBudget{
		Core:          10 + 5,
		Plugin:        10,
		Flusher:       50,
		TenantPerPool: 25,
	}
	if got := b.Fixed(); got != 75 {
		t.Errorf("a default worker's fixed pools total %d, want 75 (15 core + 10 plugin + "+
			"50 flusher).\n\nIf this is 15, something is counting the core pool alone -- "+
			"which is the figure that was documented and wrong.", got)
	}
	// THE FLUSHER IS TWO THIRDS OF IT AND IS DEFAULT-ON. Both of its gates
	// default to false, so it reads like an opt-in feature and is not one.
	if b.Flusher*3 < b.Fixed()*2 {
		t.Errorf("the flusher is %d of %d fixed connections; it was two thirds when this "+
			"was written, and a reader sizing a worker needs to know it is the largest term",
			b.Flusher, b.Fixed())
	}
}

// Turning the flusher off is the single biggest lever, and the census must show
// it rather than keeping a constant.
func TestTheCensusFollowsTheFlags(t *testing.T) {
	for _, tc := range []struct {
		name  string
		b     connectionBudget
		fixed int
	}{
		{"default", connectionBudget{Core: 15, Plugin: 10, Flusher: 50}, 75},
		{"flusher off", connectionBudget{Core: 15, Plugin: 10}, 25},
		{"no separate plugin pool", connectionBudget{Core: 15, Flusher: 50}, 65},
		{"two shards", connectionBudget{Core: 15, Plugin: 10, Flusher: 50, Shards: 2 * shardPoolMaxConns}, 105},
		{"migrating", connectionBudget{Core: 15, Plugin: 10, Flusher: 50, Migrate: migratePoolMaxConns}, 77},
		{"heartbeat pool reserved", connectionBudget{Core: 15, Plugin: 10, Flusher: 50, Heartbeat: 3}, 78},
		{"nothing configured", connectionBudget{}, 0},
	} {
		if got := tc.b.Fixed(); got != tc.fixed {
			t.Errorf("%s: Fixed() = %d, want %d", tc.name, got, tc.fixed)
		}
	}
}

// Headroom is how many tenant pools fit, and it is deliberately pessimistic.
func TestTenantHeadroomIsCountedAtTheCeiling(t *testing.T) {
	b := connectionBudget{Core: 15, Plugin: 10, Flusher: 50, TenantPerPool: 25}

	if got := b.TenantHeadroom(200); got != 5 {
		t.Errorf("TenantHeadroom(200) = %d, want 5 ((200-75)/25)", got)
	}
	// Exhausted, not negative: a caller rendering this should say "no room",
	// not "-2 tenants".
	if got := b.TenantHeadroom(50); got != 0 {
		t.Errorf("TenantHeadroom(50) = %d, want 0 -- the fixed pools already exceed it", got)
	}
	// NOT APPLICABLE is distinct from NO ROOM. Without the distinction a
	// worker running no tenant pools at all reports "0 tenants fit", which
	// reads as a capacity problem rather than as a configuration that has no
	// tenant pools.
	none := connectionBudget{Core: 15}
	if got := none.TenantHeadroom(200); got != -1 {
		t.Errorf("TenantHeadroom with no tenant pools = %d, want -1 (not applicable)", got)
	}
}

// An impossible budget is refused at startup rather than discovered under load.
func TestAnImpossibleBudgetIsRefused(t *testing.T) {
	b := connectionBudget{Core: 15, Plugin: 10, Flusher: 50, TenantPerPool: 25}

	// UNCONFIGURED CHECKS NOTHING. The flag is new; every existing deployment
	// must keep starting.
	if err := checkConnectionBudget(0, b); err != nil {
		t.Errorf("an unset budget refused a worker: %v", err)
	}
	if err := checkConnectionBudget(-1, b); err != nil {
		t.Errorf("a negative budget refused a worker: %v", err)
	}

	// Exactly the fixed total is allowed: it leaves no tenant headroom, which
	// is a legitimate single-tenant configuration rather than an error.
	if err := checkConnectionBudget(75, b); err != nil {
		t.Errorf("a budget equal to the fixed pools was refused: %v", err)
	}

	err := checkConnectionBudget(74, b)
	if err == nil {
		t.Fatal("a budget one below the fixed pools was accepted.\n\n" +
			"Those pools open at startup and are not evictable, so nothing can bring the " +
			"worker under the budget; the failure otherwise appears as connection " +
			"exhaustion in production under load.")
	}
	// The message must carry the arithmetic. "Budget too small" sends the
	// operator to guess which flag; the breakdown names the three that matter.
	for _, want := range []string{"core=15", "plugin=10", "flusher=50", "75 fixed", "--concurrency"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q:\n  %v", want, err)
		}
	}
}

// Describe names every term, because the total is what misled everyone.
func TestDescribeNamesEveryPool(t *testing.T) {
	b := connectionBudget{Core: 15, Plugin: 10, Flusher: 50, TenantPerPool: 25}
	got := b.Describe()
	for _, want := range []string{"core=15", "plugin=10", "flusher=50", "= 75 fixed", "25 per tenant pool"} {
		if !strings.Contains(got, want) {
			t.Errorf("Describe() omits %q:\n  %s", want, got)
		}
	}
	// Pools that are not open are not listed, so a log line describes THIS
	// worker rather than a template with zeros in it.
	if strings.Contains(got, "shards") || strings.Contains(got, "migrate") {
		t.Errorf("Describe() lists pools this worker does not open:\n  %s", got)
	}
}

// The census is computed and checked at startup, not merely defined.
//
// THIS ISSUE'S OWN HISTORY IS THE ARGUMENT. cleat#1470's EvictIdle was a
// complete, correct-looking function that `return 0`-ed and had no caller for
// months; nothing failed, because nothing called it. A budget check that is
// written and never invoked is the same artefact, and the unit tests above
// would stay green through it -- they test the function, and the function would
// be fine.
//
// Source-level because the end-to-end path cannot be driven here: the worker
// refuses a superuser DSN before it reaches the budget check (it will not run
// without row-level security), and a local run has no non-superuser role. So
// this asserts the wiring rather than the behaviour, and says so.
func TestTheConnectionBudgetIsActuallyCheckedAtStartup(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	// Strip line comments: this file and main.go both name these symbols in
	// prose, and a search cannot tell a call from a sentence about one.
	var lines []string
	for _, l := range strings.Split(string(src), "\n") {
		if i := strings.Index(l, "//"); i >= 0 {
			l = l[:i]
		}
		lines = append(lines, l)
	}
	body := strings.Join(lines, "\n")

	for _, want := range []string{
		"checkConnectionBudget(",         // the refusal is invoked
		"budget.Describe()",              // the census is logged
		"*connectionBudgetFlag",          // the flag is read, not a constant
		"budget.Shards = shardPoolCount", // sharded workers are counted
		"SetConnectionBudget(",           // the tenant pools are actually bounded
		"queryServerConnectionLimit(",    // the server's ceiling is asked for
		"assessConnectionFit(",           // and compared against this worker
	} {
		if !strings.Contains(body, want) {
			t.Errorf("main.go does not contain %q.\n\n"+
				"The census must be computed from this worker's flags and checked at "+
				"startup. A budget helper with no caller is the defect cleat#1470 already "+
				"had once, in EvictIdle.", want)
		}
	}
}
