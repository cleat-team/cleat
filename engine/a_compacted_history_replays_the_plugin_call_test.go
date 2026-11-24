package engine

import "testing"

// A history that has been COMPACTED and expanded again must replay plugin_call as the uncompacted one does (cleat#4152, G3, the lesson of cleat#4325).
//
// pluginCallStep's Identity reads the record's PluginName and PluginFunc, and its re-invocation decision reads the record's replay FLAGS. compaction.go carries
// the first two (and the input, output and error) for a plugin_call and does NOT carry Idempotent and SameValueOnReplay, which event_history has no column
// for either. So a recorded answer, a recorded error and a re-invocation decided by the REGISTRY replay the same after compaction, and a re-invocation
// decided by the record's own FLAGS does not survive it: it serves the recorded answer. That last is not new (the old code read the same flags from the
// same expanded record); the test pins it so that a change to compaction or to the decision cannot move it unseen.
func TestACompactedHistoryReplaysThePluginCall(t *testing.T) {
	cases := g3iCases()
	for _, tc := range []struct {
		name string
		// survives is whether the compacted history replays exactly as the uncompacted one does.
		survives bool
	}{
		{"replay_recorded_output", true},
		{"replay_recorded_error", true},
		{"replay_recorded_output_too_large", true},
		{"reinvoke_by_the_registry", true},
		{"reinvoke_of_a_recorded_error", true},
		{"a_secret_function_never_reinvokes", true},
		{"reinvoke_refused_while_stopping_by_the_registry", true},
		// The record's flags are not carried by compaction: the recorded answer is served instead of re-invoking.
		{"reinvoke_by_the_record_flags", false},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			c, ok := cases[tc.name]
			if !ok {
				t.Fatalf("PRECONDITION FAILED: the plugin table has no row %q, so this test measures nothing", tc.name)
			}
			cs, err := extractCompactionStateFrom(nil, c.history)
			if err != nil {
				t.Fatalf("compacting the record the call writes: %v", err)
			}
			expanded := buildFullHistoryFromCompaction(nil, cs)
			if len(expanded) != len(c.history) {
				t.Fatalf("PRECONDITION FAILED: %d events went into compaction and %d came out", len(c.history), len(expanded))
			}
			// The control: the uncompacted row is exactly what the table pins, so a difference below is about compaction and not about the row.
			want := c.observe()
			if want != g3iGolden[tc.name] {
				t.Fatalf("PRECONDITION FAILED: the uncompacted row does not read as the table pins it:\n  got:  %s\n  want: %s", want, g3iGolden[tc.name])
			}
			c.history = expanded
			got := c.observe()
			if tc.survives && got != want {
				t.Errorf("a COMPACTED history did not replay the way the uncompacted one does:\n  uncompacted: %s\n  compacted:   %s", want, got)
			}
			if !tc.survives && got == want {
				t.Errorf("the compacted history replayed exactly as the uncompacted one, but compaction is not supposed to carry the replay flags this row depends on: %s", got)
			}
		})
	}
}
