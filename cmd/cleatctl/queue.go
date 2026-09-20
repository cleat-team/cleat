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
func runQueue(ctx context.Context, db *sql.DB, d dialect, args []string) {
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
	store := engine.NewQueueStore(db, d.name)

	switch sub {
	case "list":
		queueList(ctx, store, tenant.String())
	case "create":
		queueCreate(ctx, store, tenant.String(), args[2:])
	case "disable":
		queueSetRetired(ctx, store, tenant.String(), args[2:], true)
	case "enable":
		queueSetRetired(ctx, store, tenant.String(), args[2:], false)
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q; want list, create, disable or enable\n\n", sub)
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
		fmt.Printf("  %-32s concurrency=%-4d %s\n", q.Name, q.ConcurrencyLimit, state)
	}
}

func queueCreate(ctx context.Context, store *engine.QueueStore, tenant string, args []string) {
	fs := flag.NewFlagSet("queue create", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = printQueueUsage
	limit := fs.Int("concurrency", 0, "how many of this queue's runs may be claimed at once (>= 1)")

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

	err = store.CreateQueue(ctx, tenant, name, *limit)
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
	fmt.Printf("Start a workflow against it by setting its concurrency_key to %q.\n", name)
	fmt.Printf("At most %d of them are claimed at once; the rest WAIT and are claimed as slots\n", *limit)
	fmt.Printf("free. They are deferred, not rejected -- no start returns 409 because of this.\n")
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

func printQueueUsage() {
	fmt.Fprintf(os.Stderr, `Usage: cleatctl --db <dsn> queue <list|create|disable|enable> <tenant-uuid> [args]

NOTE: --db is a GLOBAL flag and goes BEFORE the command name.

  queue list    <tenant>                        show a tenant's declared queues
  queue create  <tenant> <name> --concurrency N register one, admitting N at a time
  queue disable <tenant> <name>                 retire it (see below -- NOT a stop)
  queue enable  <tenant> <name>                 put a retired queue back

A queue is a DECLARED concurrency limit. A workflow joins it by setting its
concurrency_key to the queue's name; at most N of them run at once and the rest
wait, then are claimed as slots free. Work is DEFERRED, never rejected.

A concurrency_key with no registered queue behind it is cleat's original
behaviour: a mutex, one run at a time. Disabling a queue returns its key to
exactly that -- one at a time, not zero. It does not stop the work.

Queue names match [A-Za-z0-9_.-]{1,128}.
`)
}
