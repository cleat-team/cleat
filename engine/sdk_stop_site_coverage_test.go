package engine

import (
	"sort"
	"testing"
)

// sdkRefusableCall binds one SDK's call to the engine method whose
// `stopBeforeNewWork()` can refuse it.
//
// The host site used to live in a `// DurableCall  engine/durablecalls.go`
// comment beside each list entry. That is data in a place nothing can check:
// a comment can name a method that was renamed, or the wrong method entirely,
// and every test over the list still passes. Making it a field is what lets
// TestEverySDKCoversEveryHostStopSite ask the question the comments were
// standing in for.
type sdkRefusableCall struct {
	sdk      string // the SDK's function or method name
	hostSite string // the execSession method that consults stopBeforeNewWork
}

// sdkCoverageExemption is a host stop site one SDK legitimately does not cover,
// with the reason as a value rather than a comment -- same rule as
// stopSurfaces' exemption constants, and for the same reason.
type sdkCoverageExemption struct {
	hostSite string
	why      string
}

var sdkStopSiteExemptions = map[string][]sdkCoverageExemption{
	"rust": {
		{
			hostSite: "DurableCallWithRetry",
			why: "the Rust SDK has no call_with_retry: a Rust guest reaches the host's " +
				"retry path through cleat_call, which is covered above. There is no " +
				"import to guard, so this is an absence rather than a gap.",
		},
		{
			hostSite: "ScheduleCron",
			why: "the Rust SDK declares no cleat_schedule_cron import and has no " +
				"schedule_cron method: `grep -c schedule_cron crates/cleat-sdk/src/host_calls.rs` " +
				"is 0 on 2026-09-04. A Rust guest cannot register a cron trigger at all, " +
				"so there is no decoder to guard. An absence, not a gap.",
		},
	},
	"java": {
		{
			hostSite: "ScheduleCron",
			why: "the Java SDK declares no scheduleCron method and no raw import for it, " +
				"so a Java guest cannot register a cron trigger and there is no decoder " +
				"to guard. Same absence as the Rust entry above; AssemblyScript DOES " +
				"have one and is covered by its list rather than exempted here.",
		},
	},
}

// TestEverySDKCoversEveryHostStopSite closes the hole that
// TestTheRequiredJavaGuardsCoverEveryHostStopSite left open for two of the
// three core-module SDKs.
//
// That test pins the host-site COUNT and fires when it moves -- it is what
// caught §3.111 on the PR that introduced the eighth site, with a message that
// named the fix. But it checks one language. Rust and AssemblyScript have
// per-method coverage of their OWN lists and no host-side pin at all, so a
// ninth stop site passed both of them silently: their loops verify that every
// entry they already list carries a guard, which is true and says nothing about
// an entry that was never added.
//
// This is the stronger property, and it subsumes a count: not "the number is
// still 8" but "every site the host can refuse is reachable in this SDK's list,
// by name". A count pin cannot tell "grew by one" from "grew by one and the
// wrong method was added to compensate".
func TestEverySDKCoversEveryHostStopSite(t *testing.T) {
	sites := hostStopSites(t)

	// Vacuity, per thing rather than per total -- a floor alone is satisfied by
	// the wrong things being present.
	if len(sites) < 6 || !sites["DurableCall"] {
		t.Fatalf("host stop-site scan found %d sites and DurableCall present=%v; it is "+
			"reading the wrong thing and would pass whatever the tree said",
			len(sites), sites["DurableCall"])
	}

	for _, sdk := range []struct {
		name  string
		calls []sdkRefusableCall
		fix   string
	}{
		{"java", javaCallsTheHostCanRefuse,
			"add the method to javaCallsTheHostCanRefuse and the Memory.throwIfStopped " +
				"guard to HostCalls.java"},
		{"rust", rustCallsTheHostCanRefuse,
			"add the function to rustCallsTheHostCanRefuse and the stop_requested guard " +
				"to crates/cleat-sdk/src/host_calls.rs"},
		{"assemblyscript", asCallsTheHostCanRefuse,
			"add the method to asCallsTheHostCanRefuse and the stopRequested guard to " +
				"packages/cleat-as/assembly/host-calls.ts"},
	} {
		if len(sdk.calls) == 0 {
			t.Errorf("%s: the refusable-call list is empty, so this SDK's arm checks nothing", sdk.name)
			continue
		}

		covered := map[string]bool{}
		for _, c := range sdk.calls {
			if c.hostSite == "" {
				t.Errorf("%s: entry %q names no host site. The binding is the point of the "+
					"field -- an entry with an empty one is checked by nothing.", sdk.name, c.sdk)
				continue
			}
			if !sites[c.hostSite] {
				t.Errorf("%s: entry %q names host site %q, which does not consult "+
					"stopBeforeNewWork.\n\nEither the host guard was removed -- which is the "+
					"regression this exists for -- or the method was renamed and this entry "+
					"was not. When the host site was a comment, neither failed anything.",
					sdk.name, c.sdk, c.hostSite)
				continue
			}
			covered[c.hostSite] = true
		}

		exempt := map[string]string{}
		for _, e := range sdkStopSiteExemptions[sdk.name] {
			// Checked here rather than in a test of its own. A separate test
			// would have nothing to assert when no exemptions are declared, and
			// the natural way to write that is a t.Skip -- which is a pass
			// wearing a skip's clothing, and which scripts/check-skips.sh
			// rejects for exactly that reason. Reading them where they are
			// consumed has no vacuous case: no exemptions, no iterations, and
			// the coverage assertion below is what carries the meaning.
			if len(e.why) < 40 {
				t.Errorf("%s: the exemption for %q gives %d characters of reason.\n\n"+
					"Every finding this family of guards has produced came from writing down "+
					"why an exemption was safe; a reason too short to state the mechanism is "+
					"the exemption going unexamined.", sdk.name, e.hostSite, len(e.why))
			}
			if !sites[e.hostSite] {
				t.Errorf("%s: exemption names host site %q, which is no longer a stop site. "+
					"A stale exemption excuses something that is not there.", sdk.name, e.hostSite)
				continue
			}
			if covered[e.hostSite] {
				t.Errorf("%s: host site %q is BOTH covered and exempted.\n\nThe exemption is "+
					"stale: %s", sdk.name, e.hostSite, e.why)
				continue
			}
			exempt[e.hostSite] = e.why
		}

		var missing []string
		for site := range sites {
			if !covered[site] && exempt[site] == "" {
				missing = append(missing, site)
			}
		}
		sort.Strings(missing)
		for _, site := range missing {
			t.Errorf("%s: the host can refuse %s and this SDK's list does not cover it.\n\n"+
				"A guest that does not test bit 31 on that call decodes the refusal as an "+
				"ordinary result and runs on, doing the work the defer segment exists to "+
				"prevent. To fix: %s. If the SDK genuinely cannot reach this host function, "+
				"add an sdkCoverageExemption with the reason instead.", sdk.name, site, sdk.fix)
		}

		t.Logf("%s: %d entries covering %d of %d host stop sites (%d exempt)",
			sdk.name, len(sdk.calls), len(covered), len(sites), len(exempt))
	}
}
