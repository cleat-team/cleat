package engine

import "testing"

// A history that has been COMPACTED and expanded again must replay plugin_call_streaming as the uncompacted one does (cleat#4152, G3, the lesson of cleat#4325).
//
// pluginCallStreamingStep's Identity reads each chunk record's PluginName and PluginFunc, its Role reads the record's StreamFinish, and its answer reads the
// chunk's index, content, the StreamData flag and the StreamErrCode. A field that compaction does not carry would turn a replay that answers into a divergence
// (the identity) or into a different answer (the flags), and an identity that read a field compaction drops is exactly the failure cleat#4325 found for another call.
// Each row is replayed from the uncompacted history and from the same history after compaction and expansion, and the two must read the same.
func TestACompactedHistoryReplaysTheStreamingPluginCall(t *testing.T) {
	cases := g3sCases()
	for _, name := range []string{
		"replay_three_chunks",
		"replay_one_final_data_chunk",
		"replay_single_failure_unknown",
		"replay_single_failure_failure_code",
		"replay_single_non_final_chunk_without_the_data_flag",
		"replay_no_finish",
		"replay_output_too_large",
		"replay_two_adjacent_streams_are_two_answers",
		"replay_two_adjacent_streams_the_second_one_final_chunk",
		"replay_chunks_then_another_call",
		"replay_an_empty_stream_with_its_marker",
		"replay_an_empty_stream_with_its_marker_then_a_stream_of_the_same_function",
		"replay_a_marker_of_another_function_is_a_divergence",
	} {
		name := name
		t.Run(name, func(t *testing.T) {
			c, ok := cases[name]
			if !ok {
				t.Fatalf("PRECONDITION FAILED: the streaming table has no row %q, so this test measures nothing", name)
			}
			cs, err := extractCompactionStateFrom(nil, c.history)
			if err != nil {
				t.Fatalf("compacting the records the call writes: %v", err)
			}
			expanded := buildFullHistoryFromCompaction(nil, cs)
			if len(expanded) != len(c.history) {
				t.Fatalf("PRECONDITION FAILED: %d events went into compaction and %d came out", len(c.history), len(expanded))
			}
			// The control: the uncompacted row is exactly what the table pins, so a difference below is about compaction and not about the row.
			want := c.observe()
			if want != g3sGolden[name] {
				t.Fatalf("PRECONDITION FAILED: the uncompacted row does not read as the table pins it:\n  got:  %s\n  want: %s", want, g3sGolden[name])
			}
			c.history = expanded
			if got := c.observe(); got != want {
				t.Errorf("a COMPACTED history did not replay the way the uncompacted one does:\n  uncompacted: %s\n  compacted:   %s", want, got)
			}
		})
	}
}
