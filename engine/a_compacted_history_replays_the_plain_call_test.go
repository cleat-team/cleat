package engine

import "testing"

// A history that has been COMPACTED and expanded again must replay the plain call as the uncompacted one does (cleat#4152, G3).
//
// callStep's Identity reads the record's Service and Op. The lesson of cleat#4325 is that an identity can read a field compaction does not carry, and then
// every compacted history replays as a divergence while the unit table, which feeds the call a record in memory, stays green. Reading compaction.go says Service,
// Op, Request, Response and the error fields ARE carried for a call event; this measures it, per recorded outcome, so a change to compaction or to the identity
// cannot lose it unseen. Each case runs a row of the call table, then the same row with its history sent through compaction and expansion.
func TestACompactedHistoryReplaysThePlainCall(t *testing.T) {
	cases := g3dCases()
	for _, name := range []string{"replay_a_recorded_call", "replay_a_recorded_error", "replay_a_recorded_non_retryable_error"} {
		name := name
		t.Run(name, func(t *testing.T) {
			c, ok := cases[name]
			if !ok {
				t.Fatalf("PRECONDITION FAILED: the call table has no row %q, so this test measures nothing", name)
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
			if want != g3dGolden[name] {
				t.Fatalf("PRECONDITION FAILED: the uncompacted row does not read as the table pins it:\n  got:  %s\n  want: %s", want, g3dGolden[name])
			}
			c.history = expanded
			if got := c.observe(); got != want {
				t.Errorf("a COMPACTED history did not replay the way the uncompacted one does:\n  uncompacted: %s\n  compacted:   %s", want, got)
			}
		})
	}
}
