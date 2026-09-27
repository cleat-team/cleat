package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/cleat-team/cleat/engine"
	"github.com/google/uuid"
)

// runQueue registers and retires a tenant's declared queues.
//
// # Why this command exists
//
// cleat#1116 shipped in two halves: migration 093 added `queues`, and the
// claim path learned to read `concurrency_limit` from it. Both landed, and
// between them they left a table an operator could populate only by
// hand-written SQL -- every exported QueueStore method sat in
// scripts/exported-test-only-baseline.txt with tests as its only callers.
// That is the state tenant_settings was in for months (cleat#1187) and the
// state `cleatctl egress-allow` was written to end for the egress allowlist.
// The four baseline entries come out with this file, so check-dead-exports.sh
// now fails if anything orphans the registration API again.
//
//	cleatctl queue list    <tenant-uuid>
//	cleatctl queue create  <tenant-uuid> <name> --concurrency N
//	cleatctl queue disable <tenant-uuid> <name>
//	cleatctl queue enable  <tenant-uuid> <name>
//
// # Why cleatctl rather than a cleat-worker one-shot flag
//
// The plan on cleat#1116 said registration would mirror `--create-org` /
// `--create-tenant`. Those two live on the worker because they are BOOTSTRAP:
// you cannot point cleatctl at a tenant that does not exist yet. A queue is
// per-tenant operational config, which is the company `set-secret`,
// `set-tenant-setting` and `egress-allow` keep -- all cleatctl subcommands.
//
// # Why it goes through engine.QueueStore rather than inlining SQL
//
// Unlike egress-allow, which writes its own statements, this calls the store.
// QueueStore wraps the tenant into ctx itself (withQueueTenant), and skipping
// that step is invisible on PostgreSQL -- whose RLS exempts the role cleatctl
// connects as -- while SQL Server's filter predicate reads back "not found"
// immediately after a successful write. cleat#1116's first PR found that the
// hard way. Inlining the SQL here would reproduce the trap in a second place.
//
// Absent from portedOn, on that map's own stated rule: a subcommand that
// "goes entirely through engine's store interface, which is already
// dialect-aware" is unrestricted. TestQueueCommandWorksOnEveryDialect is the
// test that entry-by-absence is supposed to have behind it, the same way
// TestEgressAllowWorksOnEveryDialect backs egress-allow's explicit one.
//
// "ALREADY DIALECT-AWARE" WAS TRUE OF RLS/FILTER-PREDICATE SCOPING AND FALSE
// OF MYSQL'S. cleat#1956: QueueStore given the raw db cleatctl opened at
// startup wrote a perfectly well-formed row -- right tenant, right limit --
// into whichever database --db literally named, while ClaimWorkflows reads
// queues from cleat_<tenant-id>, a different physical database MySQL alone
// uses for tenant isolation. `queue create` reported success and `queue
// list` read the row straight back, because both went through the same
// wrong database; nothing in this command's own output or tests could have
// told the two apart. Fixed by routing through tenantScopedDB, which is the
// one place that now knows the difference.
func runQueue(ctx context.Context, db *sql.DB, d dialect, dsn string, args []string) {
	if len(args) < 2 {
		printQueueUsage()
		osExit(2)
		return
	}
	sub, tenantArg := args[0], args[1]
	tenant, err := uuid.Parse(tenantArg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %q is not a tenant UUID\n", tenantArg)
		osExit(2)
		return
	}
	// queues lives in the tenant's own database on MySQL, not whatever
	// database --db names -- see tenantScopedDB's doc comment. A no-op on
	// PostgreSQL and SQL Server.
	tdb, err := d.tenantScopedDB(ctx, db, dsn, tenant.String())
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		osExit(1)
		return
	}
	store := engine.NewQueueStore(tdb, d.name)

	switch sub {
	case "list":
		queueList(ctx, store, tenant.String())
	case "create":
		queueCreate(ctx, store, tenant.String(), args[2:])
	case "disable":
		queueSetRetired(ctx, store, tenant.String(), args[2:], true)
	case "enable":
		queueSetRetired(ctx, store, tenant.String(), args[2:], false)
	case "update":
		queueUpdate(ctx, store, tenant.String(), args[2:])
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q; want list, create, disable, enable or update\n\n", sub)
		printQueueUsage()
		osExit(2)
	}
}

func queueList(ctx context.Context, store *engine.QueueStore, tenant string) {
	queues, err := store.ListQueues(ctx, tenant)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error listing queues: %v\n", err)
		osExit(1)
		return
	}
	if len(queues) == 0 {
		// Said outright, for the reason egress-allow says its empty list
		// denies everything: "no queues" is not "no admission control", it is
		// a DIFFERENT admission control, and the difference is the surprise
		// cleat#1116 was filed about. A reader arriving from Temporal or DBOS
		// reads `concurrency_key` as a semaphore; without a registered queue
		// it is a mutex.
		fmt.Printf("tenant %s has no registered queues.\n", tenant)
		fmt.Printf("Its workflows still honour concurrency_key, but as a MUTEX: at most one run\n")
		fmt.Printf("per key at a time. Register a queue to raise that to N.\n")
		return
	}
	fmt.Printf("tenant %s has %d queue(s):\n", tenant, len(queues))
	for _, q := range queues {
		state := "live"
		if q.DisabledAt != nil {
			// The limit is printed for a disabled queue too, and labelled as
			// not in force, rather than hidden: it is what the queue returns
			// to on `queue enable`, and an operator deciding whether to
			// re-enable wants to see it.
			state = fmt.Sprintf("DISABLED %s (limit not in force; this key is a mutex, N=1)",
				q.DisabledAt.UTC().Format("2006-01-02T15:04:05Z"))
		}
		rate := "no rate limit"
		if q.RateLimit != nil {
			rate = fmt.Sprintf("rate<=%d/%ds", *q.RateLimit, *q.RatePeriodSeconds)
		}
		perWorker := "no per-worker cap"
		if q.WorkerConcurrency != nil {
			perWorker = fmt.Sprintf("worker<=%d", *q.WorkerConcurrency)
		}
		fmt.Printf("  %-32s concurrency=%-4d %-16s %-20s %s\n", q.Name, q.ConcurrencyLimit, rate, perWorker, state)
	}
}

func queueCreate(ctx context.Context, store *engine.QueueStore, tenant string, args []string) {
	fs := flag.NewFlagSet("queue create", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = printQueueUsage
	limit := fs.Int("concurrency", 0, "how many of this queue's runs may be claimed at once (>= 1)")
	rateLimit := fs.Int("rate-limit", 0, "cap on admissions per --rate-period (>= 1; requires --rate-period)")
	ratePeriod := fs.Int("rate-period", 0, "the rolling window --rate-limit applies over, in seconds (>= 1; requires --rate-limit)")
	workerConcurrency := fs.Int("worker-concurrency", 0, "cap on how many of this queue's holders one worker may own at once (>= 1, <= --concurrency)")

	operands, err := parseFlagsAnywhere(fs, args)
	if err != nil {
		osExit(2)
		return
	}
	if len(operands) != 1 {
		fmt.Fprintf(os.Stderr, "error: create takes exactly one queue name (got %d)\n\n", len(operands))
		printQueueUsage()
		osExit(2)
		return
	}
	name := operands[0]
	// Refused here as well as in the store. QueueStore rejects < 1 too, but
	// the default of an unset --concurrency is 0, and "must be >= 1, got 0"
	// does not tell an operator that they simply left the flag off.
	if *limit < 1 {
		fmt.Fprintf(os.Stderr, "error: --concurrency is required and must be >= 1 (got %d).\n"+
			"A queue with no limit is what an unregistered concurrency_key already is.\n", *limit)
		osExit(2)
		return
	}
	rl, rp, err := parseRateLimitFlags(*rateLimit, *ratePeriod)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		osExit(2)
		return
	}
	wc := parseWorkerConcurrencyFlag(*workerConcurrency)

	err = store.CreateQueue(ctx, tenant, name, *limit, rl, rp, wc)
	if errors.Is(err, engine.ErrQueueAlreadyExists) {
		// Not an upsert, and this says why rather than reporting a bare
		// conflict: silently raising a limit an operator did not intend to
		// touch is the kind of change nobody reviews.
		fmt.Fprintf(os.Stderr, "error: tenant %s already has a queue named %q.\n"+
			"Registration does not overwrite an existing limit. Inspect it with\n"+
			"  cleatctl queue list %s\n", tenant, name, tenant)
		osExit(1)
		return
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error creating queue %q: %v\n", name, err)
		osExit(1)
		return
	}

	fmt.Printf("registered queue %q for tenant %s with concurrency %d\n", name, tenant, *limit)
	if rl != nil {
		fmt.Printf("Rate-limited to %d admission(s) per %d second(s).\n", *rl, *rp)
	}
	if wc != nil {
		fmt.Printf("Capped at %d holder(s) per worker.\n", *wc)
	}
	fmt.Printf("Start a workflow against it by setting its concurrency_key to %q.\n", name)
	fmt.Printf("At most %d of them are claimed at once; the rest WAIT and are claimed as slots\n", *limit)
	fmt.Printf("free. They are deferred, not rejected -- no start returns 409 because of this.\n")
}

// parseWorkerConcurrencyFlag turns queueCreate/queueUpdate's --worker-concurrency
// int into the *int QueueStore takes. 0 (the flag's default, left unset) means
// "no per-worker cap" -- the same "0 means not passed" convention --concurrency
// and --rate-limit/--rate-period already use, valid here for the same reason: a
// real cap of 0 is refused anyway (QueueStore requires >= 1), so 0 is never a
// value a caller means to set.
func parseWorkerConcurrencyFlag(workerConcurrency int) *int {
	if workerConcurrency == 0 {
		return nil
	}
	return &workerConcurrency
}

// queueSetRetired is disable and enable, which differ only in direction.
//
// One function because the two halves are mirror images and the messages they
// print are where the real content is: what each does to the claim. Splitting
// them would duplicate that explanation, which is precisely the text that has
// to stay consistent between them.
func queueSetRetired(ctx context.Context, store *engine.QueueStore, tenant string, args []string, retire bool) {
	verb := "enable"
	if retire {
		verb = "disable"
	}
	if len(args) != 1 {
		fmt.Fprintf(os.Stderr, "error: %s takes exactly one queue name\n\n", verb)
		printQueueUsage()
		osExit(2)
		return
	}
	name := args[0]

	var err error
	if retire {
		err = store.DisableQueue(ctx, tenant, name)
	} else {
		err = store.EnableQueue(ctx, tenant, name)
	}
	if errors.Is(err, engine.ErrQueueNotFound) {
		fmt.Fprintf(os.Stderr, "error: tenant %s has no queue named %q\n", tenant, name)
		osExit(1)
		return
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %s %q: %v\n", verb, name, err)
		osExit(1)
		return
	}

	if retire {
		// THE MESSAGE AN OPERATOR MOST NEEDS AND WOULD LEAST EXPECT.
		// Every claim statement joins queues with `AND q.disabled_at IS NULL`,
		// so a disabled queue is indistinguishable from an unregistered name
		// and takes the bare-key arm -- the mutex. Disabling therefore drops
		// admission to one at a time; it does not stop the work, and a reader
		// who assumes it does has just made their queue serial without
		// noticing. TestADisabledQueueFallsBackToTheBareKeyMutex holds it.
		fmt.Printf("queue %q disabled for tenant %s.\n", name, tenant)
		fmt.Printf("\nTHIS DOES NOT STOP THE WORK. The key falls back to an unregistered\n")
		fmt.Printf("concurrency_key, which is a MUTEX: its runs keep being claimed, one at a\n")
		fmt.Printf("time, instead of up to the declared limit. Nothing in flight is cancelled.\n")
		fmt.Printf("\nTo stop a tenant's work entirely, use `cleatctl suspend-tenant`.\n")
		fmt.Printf("To put this queue back, `cleatctl queue enable %s %s`.\n", tenant, name)
		return
	}
	fmt.Printf("queue %q enabled for tenant %s; its declared limit is in force again.\n", name, tenant)
	fmt.Printf("Workers apply it on their next claim -- there is no cache in front of it.\n")
}

// parseRateLimitFlags turns queueCreate/queueUpdate's --rate-limit and
// --rate-period ints into the *int pair QueueStore takes. 0/0 (both flags
// left at their default) means "no rate limit" -- the same "0 means not
// passed" convention --concurrency already uses above, valid here for the
// same reason: a real rate limit or period of 0 is refused anyway, so 0 is
// never a value a caller means to set.
func parseRateLimitFlags(rateLimit, ratePeriod int) (rl, rp *int, err error) {
	if rateLimit == 0 && ratePeriod == 0 {
		return nil, nil, nil
	}
	if rateLimit == 0 || ratePeriod == 0 {
		return nil, nil, fmt.Errorf(
			"--rate-limit and --rate-period must both be given, or neither (got --rate-limit=%d --rate-period=%d)",
			rateLimit, ratePeriod)
	}
	if rateLimit < 1 || ratePeriod < 1 {
		return nil, nil, fmt.Errorf("--rate-limit and --rate-period must both be >= 1 (got %d, %d)", rateLimit, ratePeriod)
	}
	return &rateLimit, &ratePeriod, nil
}

// queueUpdate sets or clears a registered queue's rate limit and/or its
// worker concurrency cap (cleat#1917). It does not touch --concurrency --
// cleat#1918's own scope, unchanged by #1917: there is no operational need to
// change the semaphore's own ceiling here that queue disable/enable's mutex
// fallback does not already cover for the "I want fewer running at once,
// right now" case, and worker_concurrency is validated AGAINST that ceiling
// (SetQueueWorkerConcurrency), so leaving it fixed here keeps that
// validation meaningful.
//
// The two settings are independent (queue_store.go), and this command lets
// an operator touch either, both, or neither's flags in one invocation --
// but at least one operation must be given; a bare `queue update <tenant>
// <name>` with no flags does nothing and is refused rather than silently
// succeeding.
func queueUpdate(ctx context.Context, store *engine.QueueStore, tenant string, args []string) {
	fs := flag.NewFlagSet("queue update", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = printQueueUsage
	rateLimit := fs.Int("rate-limit", 0, "cap on admissions per --rate-period (>= 1; requires --rate-period)")
	ratePeriod := fs.Int("rate-period", 0, "the rolling window --rate-limit applies over, in seconds (>= 1; requires --rate-limit)")
	clearRate := fs.Bool("clear-rate-limit", false, "remove the queue's rate limit (mutually exclusive with --rate-limit/--rate-period)")
	workerConcurrency := fs.Int("worker-concurrency", 0, "cap on how many of this queue's holders one worker may own at once (>= 1, <= the queue's --concurrency)")
	clearWorker := fs.Bool("clear-worker-concurrency", false, "remove the queue's per-worker cap (mutually exclusive with --worker-concurrency)")

	operands, err := parseFlagsAnywhere(fs, args)
	if err != nil {
		osExit(2)
		return
	}
	if len(operands) != 1 {
		fmt.Fprintf(os.Stderr, "error: update takes exactly one queue name (got %d)\n\n", len(operands))
		printQueueUsage()
		osExit(2)
		return
	}
	name := operands[0]

	if *clearRate && (*rateLimit != 0 || *ratePeriod != 0) {
		fmt.Fprintf(os.Stderr, "error: --clear-rate-limit and --rate-limit/--rate-period are mutually exclusive\n")
		osExit(2)
		return
	}
	if *clearWorker && *workerConcurrency != 0 {
		fmt.Fprintf(os.Stderr, "error: --clear-worker-concurrency and --worker-concurrency are mutually exclusive\n")
		osExit(2)
		return
	}
	rateGiven := *clearRate || *rateLimit != 0 || *ratePeriod != 0
	workerGiven := *clearWorker || *workerConcurrency != 0
	if !rateGiven && !workerGiven {
		fmt.Fprintf(os.Stderr, "error: update needs at least one of --rate-limit/--rate-period, "+
			"--clear-rate-limit, --worker-concurrency or --clear-worker-concurrency\n\n")
		printQueueUsage()
		osExit(2)
		return
	}

	var rl, rp *int
	if rateGiven && !*clearRate {
		rl, rp, err = parseRateLimitFlags(*rateLimit, *ratePeriod)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			osExit(2)
			return
		}
		if rl == nil {
			fmt.Fprintf(os.Stderr, "error: update needs --rate-limit and --rate-period, or --clear-rate-limit\n\n")
			printQueueUsage()
			osExit(2)
			return
		}
	}
	var wc *int
	if workerGiven && !*clearWorker {
		wc = parseWorkerConcurrencyFlag(*workerConcurrency)
	}

	if rateGiven {
		err = store.SetQueueRateLimit(ctx, tenant, name, rl, rp)
		if errors.Is(err, engine.ErrQueueNotFound) {
			fmt.Fprintf(os.Stderr, "error: tenant %s has no queue named %q\n", tenant, name)
			osExit(1)
			return
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "error updating queue %q: %v\n", name, err)
			osExit(1)
			return
		}
	}
	if workerGiven {
		err = store.SetQueueWorkerConcurrency(ctx, tenant, name, wc)
		if errors.Is(err, engine.ErrQueueNotFound) {
			fmt.Fprintf(os.Stderr, "error: tenant %s has no queue named %q\n", tenant, name)
			osExit(1)
			return
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "error updating queue %q: %v\n", name, err)
			osExit(1)
			return
		}
	}

	if rateGiven {
		if rl != nil {
			fmt.Printf("queue %q for tenant %s is now rate-limited to %d admission(s) per %d second(s).\n",
				name, tenant, *rl, *rp)
		} else {
			fmt.Printf("queue %q for tenant %s has no rate limit.\n", name, tenant)
		}
	}
	if workerGiven {
		if wc != nil {
			fmt.Printf("queue %q for tenant %s is now capped at %d holder(s) per worker.\n", name, tenant, *wc)
		} else {
			fmt.Printf("queue %q for tenant %s has no per-worker cap.\n", name, tenant)
		}
	}
	fmt.Printf("Workers apply it on their next claim -- there is no cache in front of it.\n")
}

func printQueueUsage() {
	fmt.Fprintf(os.Stderr, `Usage: cleatctl --db <dsn> queue <list|create|disable|enable|update> <tenant-uuid> [args]

NOTE: --db is a GLOBAL flag and goes BEFORE the command name.

  queue list    <tenant>                        show a tenant's declared queues
  queue create  <tenant> <name> --concurrency N register one, admitting N at a time
                [--rate-limit N --rate-period S]  and optionally cap admissions to
                                                   N per S seconds (both or neither)
                [--worker-concurrency N]           and optionally cap one worker to
                                                   N of this queue's holders at once
  queue disable <tenant> <name>                 retire it (see below -- NOT a stop)
  queue enable  <tenant> <name>                 put a retired queue back
  queue update  <tenant> <name> --rate-limit N --rate-period S     set the rate limit
                <tenant> <name> --clear-rate-limit                 or remove it
                <tenant> <name> --worker-concurrency N             set the per-worker cap
                <tenant> <name> --clear-worker-concurrency         or remove it
                (rate-limit and worker-concurrency flags may be combined in one call;
                 at least one of the four must be given)

A queue is a DECLARED concurrency limit. A workflow joins it by setting its
concurrency_key to the queue's name; at most N of them run at once and the rest
wait, then are claimed as slots free. Work is DEFERRED, never rejected.

A concurrency_key with no registered queue behind it is cleat's original
behaviour: a mutex, one run at a time. Disabling a queue returns its key to
exactly that -- one at a time, not zero. It does not stop the work.

A queue's RATE LIMIT is separate from its concurrency limit: it caps how many
runs are ADMITTED per rolling window, independent of how many run at once. A
queue with no rate limit admits as fast as its concurrency limit allows, same
as before this existed.

A queue's WORKER CONCURRENCY is a PER-WORKER cap, separate from both: how many
of this queue's holders one worker process may own at once, including runs
that are currently sleeping/parked (DBOS parity -- see the queue store's own
doc comment). It must be between 1 and the queue's own --concurrency. A worker
serving two tenants, each capped at 1 on the same queue name, may still run
one of each at the same time -- this is a per-tenant cap, not hardware
protection.

Queue names match [A-Za-z0-9_.-]{1,128}.
`)
}
