package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/cleat-team/cleat/engine"
	"github.com/google/uuid"
)

// runSetTenantSetting writes one tenant's row in tenant_settings.
//
// # Why this command exists
//
// tenant_settings has carried the clamp rule since migration 039 -- a tenant
// may set a LOWER value than the operator's flag and can never raise it -- and
// had no writer anywhere in the codebase. Every Go reference was a
// GetTenantSettings read, on all three dialects. The table could only be
// populated by writing SQL by hand, which meant the per-tenant half of the
// design was implemented in the engine and unreachable by anyone using the
// product. cleat#1187.
//
// # Read-modify-write, and why the precondition is not optional
//
// The obvious implementation is a bare UPDATE, and it loses changes in silence:
// two operators, one raising a limit and one lowering a different one, and
// whichever commits second erases the other's field with neither seeing an
// error. So the write carries the updated_at it read, and a row that has moved
// since is REFUSED. Cadence's configStorePersistenceTest.go has a case for
// exactly this collision; it was unportable to cleat only because the operation
// did not exist.
//
// A refusal is not a failure to be retried blindly. It means someone else
// changed the row, and the right response is to look at what they did.
func runSetTenantSetting(ctx context.Context, db *sql.DB, args []string) {
	// Hand-rolled rather than flag.FlagSet, following drop-tenant in this
	// package -- and for a reason I met the hard way. Go's flag package stops
	// parsing at the first non-flag argument, so with a FlagSet
	// `set-tenant-setting <uuid> --show` silently ignores --show: the tenant id
	// comes first, parsing stops, and every flag after it is dropped. The
	// command then reported "nothing to change" for a request that named
	// exactly what to change.
	//
	// Scanning all arguments makes position irrelevant, which is what a reader
	// expects from `<command> <subject> --flags`.
	const unset = int64(-1)
	vals := map[string]int64{
		"--wasm-instance-timeout-ms":   unset,
		"--wasm-wall-clock-ceiling-ms": unset,
		"--host-retry-budget-ms":       unset,
	}
	var tenantID string
	show := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--show" || a == "-show" {
			show = true
			continue
		}
		key := a
		var raw string
		if eq := strings.IndexByte(a, '='); eq >= 0 {
			key, raw = a[:eq], a[eq+1:]
		}
		if canonical, ok := vals[strings.TrimPrefix(key, "-")]; ok || isSettingFlag(key, vals) {
			_ = canonical
			name := "--" + strings.TrimLeft(key, "-")
			if raw == "" {
				if i+1 >= len(args) {
					fmt.Fprintf(os.Stderr, "%s needs a value in milliseconds\n", name)
					osExit(1)
					return
				}
				i++
				raw = args[i]
			}
			v, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || v < 0 {
				fmt.Fprintf(os.Stderr, "%s needs a non-negative integer in milliseconds, got %q\n", name, raw)
				osExit(1)
				return
			}
			vals[name] = v
			continue
		}
		if strings.HasPrefix(a, "-") {
			fmt.Fprintf(os.Stderr, "unknown flag: %s\n\n", a)
			printSetTenantSettingUsage()
			osExit(1)
			return
		}
		if tenantID != "" {
			fmt.Fprintf(os.Stderr, "unexpected extra argument: %s\n\n", a)
			printSetTenantSettingUsage()
			osExit(1)
			return
		}
		tenantID = a
	}
	if tenantID == "" {
		printSetTenantSettingUsage()
		osExit(1)
		return
	}
	if _, err := uuid.Parse(tenantID); err != nil {
		fmt.Fprintf(os.Stderr, "not a tenant UUID: %q: %v\n", tenantID, err)
		osExit(1)
		return
	}

	store := engine.NewPostgresStore(db).WithTenant(tenantID)

	current, rev, err := store.ReadTenantSettingsForUpdate(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "reading the current settings: %v\n", err)
		osExit(1)
		return
	}

	printSettings := func(label string, s engine.TenantSettings) {
		fmt.Printf("%s\n", label)
		fmt.Printf("  wasm_instance_timeout   %s\n", describeSetting(s.WasmInstanceTimeout))
		fmt.Printf("  wasm_wall_clock_ceiling %s\n", describeSetting(s.WasmWallClockCeiling))
		fmt.Printf("  host_retry_budget       %s\n", describeSetting(s.HostRetryBudget))
	}

	if show {
		printSettings(fmt.Sprintf("tenant %s:", tenantID), current)
		if !rev.Existed {
			fmt.Printf("\n  (no row: every value falls back to the operator's flags)\n")
		}
		return
	}

	// -1 is "not passed". 0 is "clear it", which is a different instruction and
	// has to stay distinguishable -- a flag defaulting to 0 would make "leave
	// it alone" and "clear it" the same request.
	next := current
	changed := false
	for _, f := range []struct {
		v   int64
		dst *time.Duration
	}{
		{vals["--wasm-instance-timeout-ms"], &next.WasmInstanceTimeout},
		{vals["--wasm-wall-clock-ceiling-ms"], &next.WasmWallClockCeiling},
		{vals["--host-retry-budget-ms"], &next.HostRetryBudget},
	} {
		if f.v < 0 {
			continue
		}
		*f.dst = time.Duration(f.v) * time.Millisecond
		changed = true
	}
	if !changed {
		printSetTenantSettingUsage()
		fmt.Fprintf(os.Stderr, "\nnothing to change: pass at least one value, or --show to read them\n")
		osExit(1)
		return
	}

	if err := store.WriteTenantSettings(ctx, next, rev); err != nil {
		if errors.Is(err, engine.ErrTenantSettingsConflict) {
			fmt.Fprintf(os.Stderr,
				"refused: tenant %s's settings changed since this command read them.\n\n"+
					"Nothing was written. Someone else changed the row -- re-read it with\n"+
					"  cleatctl set-tenant-setting %s --show --db \"$DSN\"\n"+
					"and decide, rather than re-running this and overwriting their change.\n",
				tenantID, tenantID)
			osExit(1)
			return
		}
		fmt.Fprintf(os.Stderr, "writing the settings: %v\n", err)
		osExit(1)
		return
	}

	printSettings(fmt.Sprintf("tenant %s updated:", tenantID), next)
	fmt.Printf("\nEach value is a ceiling this tenant may lower and cannot raise;\n" +
		"the operator's flags remain the maximum.\n")
}

// describeSetting says "unset" rather than "0s", because 0 here means the value
// falls back to the operator's flag and printing a duration would imply a limit
// of zero.
func describeSetting(d time.Duration) string {
	if d <= 0 {
		return "unset (uses the operator's flag)"
	}
	return d.String()
}

// isSettingFlag reports whether `key` names one of the millisecond settings, in
// either the -flag or --flag spelling.
func isSettingFlag(key string, vals map[string]int64) bool {
	_, ok := vals["--"+strings.TrimLeft(key, "-")]
	return ok
}

func printSetTenantSettingUsage() {
	fmt.Fprintf(os.Stderr, `Usage: cleatctl --db <dsn> set-tenant-setting <tenant-uuid> [flags]

NOTE: --db is a GLOBAL flag and goes BEFORE the command name.

Writes one tenant's overrides in tenant_settings. Each value is a CEILING the
tenant may lower and can never raise: the operator's flag is the maximum, and a
larger value here is clamped to it at execution time rather than rejected.

  --wasm-instance-timeout-ms N    guest EXECUTION time ceiling
  --wasm-wall-clock-ceiling-ms N  WALL CLOCK ceiling for one invocation
  --host-retry-budget-ms N        worst-case host retry backoff ceiling
  --show                          print the current settings and exit

Omitting a flag leaves that value unchanged. Passing 0 CLEARS it, which means
"use the operator's flag" -- so "leave alone" and "clear" stay distinguishable.

The write is read-modify-write with an updated_at precondition: if the row
changed since this command read it, the write is REFUSED rather than applied, so
a concurrent change by another operator is reported instead of erased.
`)
}
