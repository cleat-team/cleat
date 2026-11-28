//go:build chaos

package chaos

import (
	"reflect"
	"testing"
	"time"
)

// The residency knob (cleat#4162, PR 3): CLEAT_CHAOS_RESIDENT_MAX_RUNS=N starts every worker with --resident-max-runs N, unset it passes nothing (the product's own default),
// and a caller that names the flag itself, which is how a row says "residency off is what I measure", is never overridden.

func TestChaosTheResidencyKnobPassesOnlyWhatItWasGivenAndNeverOverridesACaller(t *testing.T) {
	for _, c := range []struct {
		name  string
		env   string
		extra []string
		want  []string
	}{
		{"unset: nothing, so the product's default decides", "", nil, nil},
		{"set: the flag with the value", "8", nil, []string{"--resident-max-runs", "8"}},
		{"set, caller states residency off: the caller wins", "8", residencyOff, nil},
		{"set, caller states another count: the caller wins", "8", []string{"--resident-max-runs", "2"}, nil},
		{"set, caller passes unrelated flags: the knob still applies", "8", []string{"--poll", "500ms"}, []string{"--resident-max-runs", "8"}},
		{"unset, caller states residency off: nothing added", "", residencyOff, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("CLEAT_CHAOS_RESIDENT_MAX_RUNS", c.env)
			if got := residencyFlags(c.extra); !reflect.DeepEqual(got, c.want) {
				t.Errorf("residencyFlags(%v) with the knob %q = %v, want %v", c.extra, c.env, got, c.want)
			}
		})
	}
}

// The unit above reads the helper; this reads the process: the worker that startWorker actually launches carries the flag exactly once, with the knob's value, and a worker
// started with residency off carries 0 and not the knob's value.
func TestChaosAWorkerIsStartedWithTheResidencyTheKnobAndTheCallerSay(t *testing.T) {
	t.Setenv("CLEAT_CHAOS_RESIDENT_MAX_RUNS", "3")
	e := newEnv(t)
	q := "chaos-q-knob-" + time.Now().Format("150405.000000")
	valueOf := func(w *worker) (values []string) {
		for i, a := range w.cmd.Args {
			if a == "--resident-max-runs" && i+1 < len(w.cmd.Args) {
				values = append(values, w.cmd.Args[i+1])
			}
		}
		return values
	}
	if got := valueOf(startWorker(t, e.bin, q, e.svc.srv.URL)); !reflect.DeepEqual(got, []string{"3"}) {
		t.Errorf("a worker started with the knob at 3 carries --resident-max-runs %v, want exactly [3]", got)
	}
	if got := valueOf(startWorker(t, e.bin, q, e.svc.srv.URL, residencyOff...)); !reflect.DeepEqual(got, []string{"0"}) {
		t.Errorf("a worker started with residency off carries --resident-max-runs %v, want exactly [0], not the knob's value", got)
	}
}
