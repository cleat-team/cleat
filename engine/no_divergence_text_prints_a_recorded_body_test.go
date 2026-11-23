package engine

import (
	"fmt"
	"sort"
	"strings"
	"testing"
)

// cleat#4321, the structural half. The per-call rows in a_divergence_names_its_bodies_and_never_prints_them_test.go pin the calls that print detail TODAY; this
// runs over every spec on the registry, so a spec that gains a DivergenceDetail, or a detail that gains a field, is read against the same rule without anyone
// remembering to add a row: a divergence message must not print any data field of the record it diverged from.
//
// A record is built with EVERY field in eventDataFields (the table of fields that can carry what a workflow or a service produced, itself held complete by
// TestEventRecordFieldsAreAllClassified) set to its own sentinel, and each spec's detail is rendered against it. A sentinel in the text is a body printed.
//
// What this does NOT see, and the per-call rows do: the ARGUMENTS of the call (a.s1 to a.s4). Which of them is a body is not recorded on the spec, so a body
// printed from an argument alone is invisible here. fetch's body, schedule_cron's input and the plugin call's input are such arguments, and each has a row.

// divergenceIdentifiersRecordedInDataFields lists the specs that print a recorded data field because it holds an IDENTIFIER and not a body, with the reason.
// await_all_children keeps the run IDs it was handed in Request (children.go), and a run ID is what the message is for.
var divergenceIdentifiersRecordedInDataFields = map[string]map[string]string{
	"await_all_children": {"Request": "the recorded run IDs of the children, which the detail names as `expected run IDs:`"},
}

func TestNoDivergenceTextPrintsARecordedBody(t *testing.T) {
	fieldNames := make([]string, 0, len(eventDataFields))
	for name := range eventDataFields {
		fieldNames = append(fieldNames, name)
	}
	sort.Strings(fieldNames)
	sentinel := func(name string) string { return "BODY-" + name + "-4321" }

	seen := 0
	for _, sp := range stepSpecs {
		if sp.DivergenceDetail == nil {
			continue
		}
		seen++
		t.Run(sp.Name, func(t *testing.T) {
			var rec EventRecord
			for _, name := range fieldNames {
				*eventDataFields[name](&rec) = sentinel(name)
			}
			// the arguments are identifiers or bodies and are the per-call rows' business; give them their own marker so a sentinel from a RECORD is unambiguous
			a := stepArgs{s1: "ARG-1", s2: "ARG-2", s3: "ARG-3", s4: "ARG-4"}
			got := sp.DivergenceDetail(a, &rec)
			// PRECONDITION: the detail ran and, where it reads the record, did so. A detail that returned "" for this record would pass the rule below vacuously.
			if got == "" {
				t.Fatalf("PRECONDITION FAILED: %s's detail is empty against a full record, so this measures nothing", sp.Name)
			}
			for _, name := range fieldNames {
				if !strings.Contains(got, sentinel(name)) {
					continue
				}
				if reason, ok := divergenceIdentifiersRecordedInDataFields[sp.Name][name]; ok {
					t.Logf("%s prints %s: %s", sp.Name, name, reason)
					continue
				}
				t.Errorf("%s's divergence detail prints the recorded %s field, which can hold a decrypted body (cleat#4321). Name it with redactedBody.\n%s", sp.Name, name, got)
			}
		})
	}
	if seen == 0 {
		t.Fatal("PRECONDITION FAILED: no spec on the registry has a DivergenceDetail, so this measures nothing")
	}
	// the allowlist is itself held to account: an entry for a spec that no longer prints that field is a stale exemption
	for specName, fields := range divergenceIdentifiersRecordedInDataFields {
		var sp *stepSpec
		for _, have := range stepSpecs {
			if have.Name == specName {
				sp = have
			}
		}
		if sp == nil || sp.DivergenceDetail == nil {
			t.Errorf("the exemption list names %q, which has no divergence detail", specName)
			continue
		}
		var rec EventRecord
		for _, name := range fieldNames {
			*eventDataFields[name](&rec) = sentinel(name)
		}
		got := sp.DivergenceDetail(stepArgs{s1: "ARG-1"}, &rec)
		for f := range fields {
			if !strings.Contains(got, sentinel(f)) {
				t.Errorf("the exemption for %s/%s is stale: its detail no longer prints that field. Remove it. (%s)", specName, f, fmt.Sprint(got))
			}
		}
	}
}
