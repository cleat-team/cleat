package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
)

// The queue subcommand on every dialect.
//
// This is the test `queue`'s ABSENCE from portedOn is supposed to have behind
// it. That map's rule is that a subcommand going entirely through engine's
// store interface is unrestricted, which is a claim about this command, not a
// property the map can check -- so it is asserted here, the same way
// TestEgressAllowWorksOnEveryDialect backs egress-allow's explicit entry.
//
// It drives runQueue rather than engine.QueueStore directly, on purpose: the
// store already has its own three-dialect coverage in engine, and what is
// untested without this is the layer between an operator's argv and it --
// argument shape, the UUID parse, which errors exit 1 rather than 2, and the
// messages, which carry behaviour an operator cannot get anywhere else.
func TestQueueCommandWorksOnEveryDialect(t *testing.T) {
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

			// The DEFAULT tenant. queues has a foreign key to admin.tenants --
			// deliberate, it is what makes drop-tenant remove a tenant's
			// queues with it -- so a test cannot invent a tenant id.
			tenant := "00000000-0000-0000-0000-000000000000"

			// dsn is the literal --db string an operator passes; only
			// tenantScopedDB reads it, and only for mysql (see its doc
			// comment), so it is left empty for the other two dialects.
			//
			// verifyDB is where THIS TEST checks what runQueue actually did.
			// For postgres and mssql that is db itself. For mysql it must be
			// the same tenant-scoped database runQueue now writes to
			// (cleat#1956) -- asserting against the raw db here would pass
			// by construction whether or not the fix works, the same way the
			// bug's own `queue list` readback did.
			var dsn string
			verifyDB := db
			if tc.d.name == "mysql" {
				dsn = os.Getenv("CLEAT_TEST_MYSQL")
				var err error
				verifyDB, err = tc.d.tenantScopedDB(ctx, db, dsn, tenant)
				if err != nil {
					t.Fatalf("tenantScopedDB: %v", err)
				}
				// A freshly opened per-tenant database has no schema of its
				// own -- production applies migrations to it separately, at
				// cmd/cleat-worker startup -- so this test has to do the same
				// before QueueStore can see `queues` at all.
				testutil.SetupMySQLFullSchema(t, verifyDB)
			}
			store := engine.NewQueueStore(verifyDB, tc.d.name)

			// A NAME UNIQUE TO THIS RUN, AND NO CLEANUP DELETE.
			//
			// The same idiom as engine's queueTestName, for the reason stated
			// there -- testutil.TestDB hands out a shared, possibly-reused
			// database, so a fixed name collides with whatever a previous run
			// left behind. The first version of this test did delete its row
			// instead, and SQL SERVER FAILED ON IT while the other two passed:
			// `queues` carries an RLS filter predicate, a raw DELETE on a
			// connection with no tenant context matches nothing, and the
			// delete silently removed zero rows. That is the identical trap
			// engine/queue_store.go's withQueueTenant comment was written
			// about, met from the other side -- which is the argument for this
			// command going through QueueStore rather than inlining SQL, so it
			// is worth leaving the scar here rather than only in that comment.
			name := fmt.Sprintf("ctl-queue-%s-%d", tc.name, time.Now().UnixNano())

			// The empty-list message, when the fixture allows it to be
			// checked. Asserted conditionally rather than by emptying the
			// table: these suites share a database, and a test that deleted
			// every queue to make its own assertion land would break whatever
			// else was mid-run. On CI, which starts from a fresh database,
			// this fires; locally against a reused one it may not, and
			// silently not-checking beats a flake or a destructive fixture.
			if existing, err := store.ListQueues(ctx, tenant); err == nil && len(existing) == 0 {
				out, _ := withExitPanicOutput(t, func() {
					runQueue(ctx, db, tc.d, dsn, []string{"list", tenant})
				})
				if !strings.Contains(out, "no registered queues") || !strings.Contains(out, "MUTEX") {
					t.Errorf("the empty listing does not say that an unregistered key is still a "+
						"mutex, which is the surprise cleat#1116 was filed about:\n%s", out)
				}
			}

			// Create.
			out, errOut := withExitPanicOutput(t, func() {
				runQueue(ctx, db, tc.d, dsn, []string{"create", tenant, name, "--concurrency", "4"})
			})
			if !strings.Contains(out, "concurrency 4") {
				t.Fatalf("create did not confirm the limit: %s%s", out, errOut)
			}
			if !strings.Contains(out, "concurrency_key") {
				t.Errorf("create does not say how to reference the queue it just made:\n%s", out)
			}
			if !strings.Contains(out, "deferred, not rejected") {
				// The single most load-bearing sentence in this command's
				// output: cleat's pre-existing behaviour for a contended key
				// is a 409, and a reader who assumes that still applies will
				// build retry logic they do not need.
				t.Errorf("create does not say that work past the limit waits rather than being "+
					"refused:\n%s", out)
			}

			// A registered name is not silently overwritten.
			_, errOut = withExitPanicOutput(t, func() {
				runQueue(ctx, db, tc.d, dsn, []string{"create", tenant, name, "--concurrency", "9"})
			})
			if !strings.Contains(errOut, "already has a queue") {
				t.Errorf("re-registering an existing name did not refuse: %q", errOut)
			}
			if q, err := store.GetQueue(ctx, tenant, name); err != nil {
				t.Fatalf("GetQueue: %v", err)
			} else if q.ConcurrencyLimit != 4 {
				t.Errorf("the refused re-registration still changed the limit to %d; "+
					"create must not be an upsert", q.ConcurrencyLimit)
			}

			// List shows it.
			out, _ = withExitPanicOutput(t, func() {
				runQueue(ctx, db, tc.d, dsn, []string{"list", tenant})
			})
			if !strings.Contains(out, name) || !strings.Contains(out, "concurrency=4") {
				t.Errorf("list does not show the queue just registered:\n%s", out)
			}

			// Disable, and the message that matters most.
			out, _ = withExitPanicOutput(t, func() {
				runQueue(ctx, db, tc.d, dsn, []string{"disable", tenant, name})
			})
			if !strings.Contains(out, "DOES NOT STOP THE WORK") || !strings.Contains(out, "MUTEX") {
				t.Errorf("disable does not tell the operator that the key reverts to a mutex "+
					"rather than stopping. TestADisabledQueueFallsBackToTheBareKeyMutex is the "+
					"behaviour this text describes:\n%s", out)
			}
			out, _ = withExitPanicOutput(t, func() {
				runQueue(ctx, db, tc.d, dsn, []string{"list", tenant})
			})
			if !strings.Contains(out, "DISABLED") {
				t.Errorf("list does not mark the disabled queue:\n%s", out)
			}

			// Enable puts it back. Retirement is reversible by contract
			// (docs/reference/entity-lifecycle.md), and shipping disable
			// without this would have made it a one-way door.
			out, _ = withExitPanicOutput(t, func() {
				runQueue(ctx, db, tc.d, dsn, []string{"enable", tenant, name})
			})
			if !strings.Contains(out, "in force again") {
				t.Errorf("enable does not confirm the limit applies again:\n%s", out)
			}
			if q, err := store.GetQueue(ctx, tenant, name); err != nil {
				t.Fatalf("GetQueue after enable: %v", err)
			} else if q.DisabledAt != nil {
				t.Errorf("enable left disabled_at set to %v", q.DisabledAt)
			}

			// queue update sets a rate limit on the queue create above left
			// unlimited -- cleat#1918.
			out, _ = withExitPanicOutput(t, func() {
				runQueue(ctx, db, tc.d, dsn, []string{"update", tenant, name, "--rate-limit", "7", "--rate-period", "60"})
			})
			if !strings.Contains(out, "rate-limited to 7") {
				t.Errorf("update does not confirm the rate limit it just set:\n%s", out)
			}
			if q, err := store.GetQueue(ctx, tenant, name); err != nil {
				t.Fatalf("GetQueue after rate limit update: %v", err)
			} else if q.RateLimit == nil || *q.RateLimit != 7 || q.RatePeriodSeconds == nil || *q.RatePeriodSeconds != 60 {
				t.Errorf("GetQueue after update = RateLimit=%v RatePeriodSeconds=%v, want 7, 60", q.RateLimit, q.RatePeriodSeconds)
			}

			// list reflects it.
			out, _ = withExitPanicOutput(t, func() {
				runQueue(ctx, db, tc.d, dsn, []string{"list", tenant})
			})
			if !strings.Contains(out, "rate<=7/60s") {
				t.Errorf("list does not show the rate limit just set:\n%s", out)
			}

			// --clear-rate-limit removes it again.
			out, _ = withExitPanicOutput(t, func() {
				runQueue(ctx, db, tc.d, dsn, []string{"update", tenant, name, "--clear-rate-limit"})
			})
			if !strings.Contains(out, "has no rate limit") {
				t.Errorf("update --clear-rate-limit does not confirm the clear:\n%s", out)
			}
			if q, err := store.GetQueue(ctx, tenant, name); err != nil {
				t.Fatalf("GetQueue after clearing rate limit: %v", err)
			} else if q.RateLimit != nil || q.RatePeriodSeconds != nil {
				t.Errorf("GetQueue after --clear-rate-limit = RateLimit=%v RatePeriodSeconds=%v, want nil, nil", q.RateLimit, q.RatePeriodSeconds)
			}
		})
	}
}

// The refusals, which are where an operator meets this command when they get
// it wrong. Postgres only: these exit before any dialect-specific statement,
// so running them three times would assert the same code path three times.
func TestQueueCommandRefusesBadInput(t *testing.T) {
	db := testutil.TestDB(t, testutil.DialectPostgres)
	ctx := context.Background()
	tenant := "00000000-0000-0000-0000-000000000000"

	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"no subcommand", []string{}, "Usage"},
		{"not a uuid", []string{"list", "tenant-one"}, "not a tenant UUID"},
		{"unknown subcommand", []string{"destroy", tenant}, "unknown subcommand"},
		{
			// The default of an unset --concurrency is 0, and the store's own
			// "must be >= 1, got 0" would not tell an operator that they
			// simply left the flag off.
			"create without a limit",
			[]string{"create", tenant, "q-no-limit"},
			"--concurrency is required",
		},
		{"create with a zero limit", []string{"create", tenant, "q-zero", "--concurrency", "0"}, "must be >= 1"},
		{"disable an unregistered name", []string{"disable", tenant, "no-such-queue-here"}, "has no queue named"},
		{"enable an unregistered name", []string{"enable", tenant, "no-such-queue-here"}, "has no queue named"},
		{"create with two names", []string{"create", tenant, "a", "b", "--concurrency", "2"}, "exactly one queue name"},
		{"create with no name at all", []string{"create", tenant, "--concurrency", "2"}, "exactly one queue name"},
		{
			// cleat#1918: a rate limit needs both a count and a window, and
			// the refusal has to name which flag is missing rather than
			// surface ck_queues_rate_limit_paired's opaque constraint error.
			"create with rate-limit but no rate-period",
			[]string{"create", tenant, "q-unpaired-rate", "--concurrency", "2", "--rate-limit", "5"},
			"--rate-limit and --rate-period must both be given",
		},
		{
			"create with rate-period but no rate-limit",
			[]string{"create", tenant, "q-unpaired-period", "--concurrency", "2", "--rate-period", "60"},
			"--rate-limit and --rate-period must both be given",
		},
		{"update an unregistered name", []string{"update", tenant, "no-such-queue-here", "--rate-limit", "5", "--rate-period", "60"}, "has no queue named"},
		{"update with neither a rate limit nor --clear-rate-limit", []string{"update", tenant, "q-update-nothing"}, "needs --rate-limit and --rate-period, or --clear-rate-limit"},
		{
			"update with --clear-rate-limit and --rate-limit together",
			[]string{"update", tenant, "q-update-both", "--clear-rate-limit", "--rate-limit", "5", "--rate-period", "60"},
			"mutually exclusive",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, errOut := withExitPanicOutput(t, func() {
				runQueue(ctx, db, dialectPostgres, "", tc.args)
			})
			if !strings.Contains(errOut, tc.want) {
				t.Errorf("stderr does not contain %q:\n%s", tc.want, errOut)
			}
		})
	}

	// The control, and the flag-ordering guard in one.
	//
	// Without a control, every assertion above passes against a command that
	// refuses everything, including what it should accept. And BOTH ORDERS are
	// asserted because the natural one -- name first, flag after -- is the one
	// Go's flag package silently drops: it stops parsing at the first non-flag
	// argument, so --concurrency would never be seen and the operator would be
	// told to pass a flag they just passed. `cleatctl set-secret <uuid> --name
	// foo` and `cleatctl suspend-tenant <uuid> --yes` were both unusable for
	// exactly that reason; that was cleat#1933, fixed in #1936, and the two
	// commands now share this one's parser. parseFlagsAnywhere is what keeps
	// this command out of that set, and these two cases are what hold it there.
	for _, order := range []struct {
		label     string
		flagFirst bool
	}{
		{"name before the flag", false},
		{"flag before the name", true},
	} {
		t.Run("a well-formed create is not refused: "+order.label, func(t *testing.T) {
			// Unique per run, and no cleanup delete -- see the note in
			// TestQueueCommandWorksOnEveryDialect.
			name := fmt.Sprintf("ctl-queue-control-%d", time.Now().UnixNano())
			args := []string{"create", tenant, name, "--concurrency", "2"}
			if order.flagFirst {
				args = []string{"create", tenant, "--concurrency", "2", name}
			}
			out, errOut := withExitPanicOutput(t, func() {
				runQueue(ctx, db, dialectPostgres, "", args)
			})
			if errOut != "" {
				t.Errorf("a valid create wrote to stderr: %s", errOut)
			}
			if !strings.Contains(out, "registered queue") {
				t.Errorf("a valid create did not succeed:\n%s", out)
			}
			// The limit actually landed -- "registered queue" printing is not
			// the same as --concurrency having been read. This is the exact
			// assertion that fails if parseFlagsAnywhere is replaced by a plain
			// fs.Parse: the name-first case would refuse, and a flag-parsing
			// bug that defaulted the limit to something would land the wrong
			// number here rather than an error anywhere.
			if q, err := engine.NewQueueStore(db, dialectPostgres.name).GetQueue(ctx, tenant, name); err != nil {
				t.Fatalf("GetQueue: %v", err)
			} else if q.ConcurrencyLimit != 2 {
				t.Errorf("--concurrency 2 stored a limit of %d", q.ConcurrencyLimit)
			}
		})
	}
}
