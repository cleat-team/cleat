package main

import (
	"flag"
	"testing"
)

// A default install bounds how much history one run may write, and the three
// quotas that FAIL rather than roll over stay unbounded on purpose. cleat#1829.
//
// # Why assert a default at all
//
// --max-quota-events defaulted to 0 = unlimited, so a looping workflow had no
// bound on event_history. --retention-days (default 30) does not help: it
// sweeps TERMINAL runs, and a runaway is not terminal.
//
// The value is a starting point rather than a measurement. What this pins is
// that it is FINITE -- a refactor that restores 0 does so visibly, because
// "unlimited" is exactly the state that reads as working until a tenant fills
// the table.
//
// # Why the other three are asserted to be ZERO
//
// Asserting only the one that changed would leave the other three looking
// unconsidered, and cleat#1774 was filed -- by me -- on exactly that reading.
// They are unbounded because exceeding them writes an error back into the
// workflow (engine/children.go:204) rather than continuing it as new, so a
// default would break working deployments at whatever number was picked. This
// records that as a position. If someone later defaults them WITH usage data,
// this test failing is the prompt to update the reasoning rather than a
// blocker.
//
// # Read through flag.Lookup, not from the source text
//
// The parsed DefValue is what a deployment gets. A test that grepped config.go
// for "50000" would pass on a flag that was declared and never registered --
// the mechanism-wired-to-nothing shape this repository keeps finding, most
// recently in cleat#1810 where an override existed with no flag to reach it.
func TestARunawayWorkflowHasAnEventBound(t *testing.T) {
	// The flag package registers on package init, so the set is already
	// populated; if it is not, every lookup below returns nil and the
	// assertions would be vacuous.
	if flag.Lookup("max-quota-events") == nil {
		t.Fatal("UNMEASURED: max-quota-events is not registered on the default FlagSet, " +
			"so every assertion here would be about nothing. This is a failure of the check.")
	}

	bounded := flag.Lookup("max-quota-events")
	if bounded.DefValue == "0" {
		t.Errorf("--max-quota-events defaults to 0 (unlimited). A looping workflow then has no "+
			"bound on event_history, and --retention-days does not reach it because that sweep "+
			"selects TERMINAL runs. Exceeding this quota is a rollover, not a failure "+
			"(engine/callerrors.go eventCapCallError), so a finite default is safe. cleat#1829. "+
			"got %q", bounded.DefValue)
	}
	if got, want := bounded.DefValue, "50000"; got != want {
		t.Logf("--max-quota-events default is %s (was %s when cleat#1829 landed); "+
			"finite is what this test pins, the number is a starting point", got, want)
	}

	// The three that fail the caller rather than rolling over.
	for _, name := range []string{
		"max-quota-children",
		"max-quota-concurrency-keys",
		"max-quota-schedules",
	} {
		f := flag.Lookup(name)
		if f == nil {
			t.Errorf("UNMEASURED: --%s is not registered, so its default cannot be checked.", name)
			continue
		}
		if f.DefValue != "0" {
			t.Errorf("--%s now defaults to %q rather than 0. That may well be right, but it was "+
				"left unbounded deliberately: exceeding it FAILS the workflow rather than "+
				"continuing it as new, so a default breaks working deployments at whatever number "+
				"is chosen. If this was done with usage data, update this test and the reasoning "+
				"in config.go together. cleat#1829.", name, f.DefValue)
		}
	}
}
