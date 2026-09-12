package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/cleat-team/cleat/engine"
)

func runVersions(ctx context.Context, store engine.WorkflowStore, args []string) {
	if len(args) < 1 {
		printVersionsUsage()
		osExit(1)
	}

	sub := args[0]
	switch sub {
	case "list":
		listVersions(ctx, store, args[1:])
	case "deprecate":
		deprecateVersion(ctx, store, args[1:], true)
	case "restore":
		deprecateVersion(ctx, store, args[1:], false)
	case "purge":
		purgeVersion(ctx, store, args[1:])
	case "active":
		activeInstances(ctx, store, args[1:])
	case "gc":
		gcVersions(ctx, store, args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown versions subcommand: %s\n\n", sub)
		printVersionsUsage()
		osExit(1)
	}
}

func printVersionsUsage() {
	fmt.Fprintf(os.Stderr, `Usage: cleatctl versions <subcommand> [<args>]

Subcommands:
  list [<name>]                    list workflow versions
  deprecate <name> <version>       mark a version deprecated
  restore <name> <version>         mark a version active
  purge <name> <version>           permanently delete a version
  active [<name>]                  show active instance counts by version
  gc [--dry-run]                   run garbage collection on deprecated versions

`)
}

func listVersions(ctx context.Context, store engine.WorkflowStore, args []string) {
	var name string
	if len(args) > 0 {
		name = args[0]
	}

	defs, err := store.ListWorkflowDefs(ctx, name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error listing workflow versions %q: %v\n", name, err)
		osExit(1)
	}

	if name == "" {
		// Show summary across all workflows.
		summary, err := engine.CollectVersionMetrics(ctx, store)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error collecting version metrics: %v\n", err)
			osExit(1)
		}

		fmt.Printf("Total versions: %d | Active: %d | Deprecated: %d | Active instances: %d\n\n",
			summary.TotalVersions, summary.ActiveVersions, summary.Deprecated, summary.TotalActiveInstances)

		w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
		fmt.Fprintln(w, "Workflow\tVersion\tABI\tMinVer\tDeprecated\tAge\tActiveInst")
		fmt.Fprintln(w, "--------\t-------\t---\t------\t----------\t---\t----------")
		for _, vm := range summary.Workflows {
			dep := "no"
			if vm.Deprecated {
				dep = "yes"
			}
			fmt.Fprintf(w, "%s\t%d\t%d\t%d\t%s\t%s\t%d\n",
				vm.Name, vm.Version, vm.ABIVersion, vm.MinVersion, dep, vm.Age, vm.ActiveInstances)
		}
		w.Flush()
		return
	}

	// Single workflow listing.
	if defs == nil {
		fmt.Printf("No versions found for %q\n", name)
		return
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "Version\tABI\tMinVer\tDeprecated\tCreated\tActiveInst")
	fmt.Fprintln(w, "-------\t---\t------\t----------\t-------\t----------")
	for _, def := range defs {
		dep := "no"
		if def.Deprecated {
			dep = "yes"
		}
		count, err := store.CountActiveInstances(ctx, def.Name, def.Version)
		if err != nil {
			count = -1
		}
		fmt.Fprintf(w, "%d\t%d\t%d\t%s\t%s\t%d\n",
			def.Version, def.ABIVersion, def.MinVersion, dep,
			def.CreatedAt.Format(time.RFC3339), count)
	}
	w.Flush()
}

func deprecateVersion(ctx context.Context, store engine.WorkflowStore, args []string, deprecated bool) {
	if len(args) < 2 {
		fmt.Fprintf(os.Stderr, "usage: cleatctl versions %s <name> <version>\n", map[bool]string{true: "deprecate", false: "restore"}[deprecated])
		osExit(1)
	}
	name := args[0]
	version, err := strconv.Atoi(args[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: invalid version %q\n", args[1])
		osExit(1)
	}

	if err := store.MarkVersionDeprecated(ctx, name, version, deprecated); err != nil {
		op := "deprecating"
		if !deprecated {
			op = "restoring"
		}
		fmt.Fprintf(os.Stderr, "error %s %s v%d: %v\n", op, name, version, err)
		osExit(1)
	}

	action := "deprecated"
	if !deprecated {
		action = "restored"
	}
	fmt.Printf("%s v%d %s\n", name, version, action)
}

func purgeVersion(ctx context.Context, store engine.WorkflowStore, args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: cleatctl versions purge <name> <version>")
		osExit(1)
	}
	name := args[0]
	version, err := strconv.Atoi(args[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: invalid version %q\n", args[1])
		osExit(1)
	}

	// Confirm.
	fmt.Printf("Permanently delete %s v%d? [y/N]: ", name, version)
	var confirm string
	fmt.Scanln(&confirm)
	if confirm != "y" && confirm != "Y" {
		fmt.Println("cancelled")
		return
	}

	if err := store.PurgeWorkflowDef(ctx, name, version); err != nil {
		fmt.Fprintf(os.Stderr, "error purging %s v%d: %v\n", name, version, err)
		osExit(1)
	}
	fmt.Printf("%s v%d purged\n", name, version)
}

func activeInstances(ctx context.Context, store engine.WorkflowStore, args []string) {
	var name string
	if len(args) > 0 {
		name = args[0]
	}

	if name != "" {
		// Show for a specific workflow.
		defs, err := store.ListWorkflowDefs(ctx, name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error listing workflow definitions for %q: %v\n", name, err)
			osExit(1)
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
		fmt.Fprintln(w, "Version\tActive Instances\tDeprecated")
		fmt.Fprintln(w, "-------\t----------------\t----------")
		total := 0
		for _, def := range defs {
			count, err := store.CountActiveInstances(ctx, def.Name, def.Version)
			if err != nil {
				continue
			}
			dep := "no"
			if def.Deprecated {
				dep = "yes"
			}
			fmt.Fprintf(w, "%d\t%d\t%s\n", def.Version, count, dep)
			total += count
		}
		w.Flush()
		fmt.Printf("\nTotal active instances for %s: %d\n", name, total)
		return
	}

	// Show across all workflows.
	counts, err := store.GetActiveInstanceCountsByVersion(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error getting active instance counts: %v\n", err)
		osExit(1)
	}
	if len(counts) == 0 {
		fmt.Println("No active instances")
		return
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "Workflow\tVersion\tActive Instances")
	fmt.Fprintln(w, "--------\t-------\t----------------")
	grandTotal := 0
	for key, count := range counts {
		fmt.Fprintf(w, "%s\t%d\n", key, count)
		grandTotal += count
	}
	w.Flush()
	fmt.Printf("\nGrand total active instances: %d\n", grandTotal)
}

// gcVersions runs one garbage-collection pass.
//
// RETENTION IS OVERRIDABLE HERE. Until cleat#1315 this read only --dry-run and
// then took engine.DefaultGCOptions() wholesale, so MinVersionsToKeep=3 and
// MaxVersionAge=30d were compiled in and unreachable -- an operator could not
// tune the policy from any interface, while docs/troubleshooting.md told them
// to adjust it with two flags that did not exist.
//
// Parsed by hand rather than with a FlagSet to match the rest of this file,
// where every subcommand reads its own args. An unrecognised argument is
// REFUSED rather than ignored: silently dropping --min-versions=5 would run a
// destructive sweep under a policy the caller did not ask for, which is the
// failure this change exists to remove.
func gcVersions(ctx context.Context, store engine.WorkflowStore, args []string) {
	opts := engine.DefaultGCOptions()
	for _, a := range args {
		switch {
		case a == "--dry-run":
			opts.DryRun = true
		case strings.HasPrefix(a, "--min-versions="):
			n, err := strconv.Atoi(strings.TrimPrefix(a, "--min-versions="))
			// >= 1, not >= 0: GarbageCollectVersions normalises <= 0 back to
			// the default, so accepting 0 would run under a policy of 3 while
			// reporting success -- the silent substitution being removed here.
			if err != nil || n < 1 {
				fmt.Fprintf(os.Stderr, "invalid --min-versions: %q (want an integer >= 1; "+
					"0 is treated as unset and substitutes %d)\n",
					strings.TrimPrefix(a, "--min-versions="), engine.DefaultMinVersionsToKeep)
				osExit(1)
				return
			}
			opts.MinVersionsToKeep = n
		case strings.HasPrefix(a, "--max-age="):
			d, err := time.ParseDuration(strings.TrimPrefix(a, "--max-age="))
			if err != nil || d <= 0 {
				fmt.Fprintf(os.Stderr, "invalid --max-age: %q (want a positive Go duration "+
					"such as 720h; 0 is treated as unset and substitutes %s)\n",
					strings.TrimPrefix(a, "--max-age="), engine.DefaultMaxVersionAge)
				osExit(1)
				return
			}
			opts.MaxVersionAge = d
		default:
			fmt.Fprintf(os.Stderr, "unknown argument for `versions gc`: %q\n"+
				"usage: cleatctl versions gc [--dry-run] [--min-versions=N] [--max-age=DURATION]\n", a)
			osExit(1)
			return
		}
	}

	result, err := engine.GarbageCollectVersions(ctx, store, opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error running garbage collection: %v\n", err)
		osExit(1)
	}

	mode := ""
	if opts.DryRun {
		mode = " (dry run)"
	}

	fmt.Printf("GC complete%s:\n", mode)
	// The policy is echoed because it is now overridable. A run that removed
	// nothing under --min-versions=50 and one that removed nothing under the
	// default 3 are different answers, and the counts alone cannot say which.
	fmt.Printf("  Policy:            keep >= %d versions, deprecated older than %s\n",
		opts.MinVersionsToKeep, opts.MaxVersionAge)
	fmt.Printf("  Versions removed:  %d\n", result.VersionsRemoved)
	fmt.Printf("  Versions skipped:  %d\n", result.VersionsSkipped)
	if len(result.Errors) > 0 {
		fmt.Printf("  Errors (%d):\n", len(result.Errors))
		for _, err := range result.Errors {
			fmt.Printf("    - %v\n", err)
		}
	}
}
