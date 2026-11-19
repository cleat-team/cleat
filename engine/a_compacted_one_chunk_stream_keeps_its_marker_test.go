package engine

import (
	"testing"

	"github.com/cleat-team/cleat/plugin"
)

// cleat#4117, the compaction carrier. A one-chunk stream replays as the success it was only if the chunk still says it is plugin output
// (EventRecord.StreamData), and compaction copies fields PER EVENT TYPE: a field it does not capture and expand is lost the first time a
// run compacts, and the chunk then replays as the stream-level failure it was once mistaken for (status 1, the chunk's text as the
// message). The payload carrier is covered by a round trip in a_lone_finished_stream_chunk_replays_as_the_success_it_was_test.go; this is
// the other one, found missing by review: deleting either line below left 529 engine tests green.
//
// The history goes through the real extractCompactionState and buildFullHistoryFromCompaction, and is then REPLAYED and read as the guest
// would read it, so a field that survives as a struct member but is not honoured fails too.
func TestACompactedOneChunkStreamKeepsItsMarker(t *testing.T) {
	for _, tc := range []struct {
		name   string
		chunks []plugin.StreamEvent
	}{
		{"one finished chunk", []plugin.StreamEvent{{Index: 0, Content: "only", Finish: true}}},
		{"two chunks (control)", []plugin.StreamEvent{{Index: 0, Content: "a"}, {Index: 1, Content: "b", Finish: true}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fresh := newTestExecSession()
			registerOneChunkStream(fresh, tc.chunks...)
			freshErr, freshOut := streamCallOutput(t, fresh)
			if freshErr != 0 || len(fresh.history) != len(tc.chunks) {
				t.Fatalf("PRECONDITION FAILED: the fresh run gave status %d, %d event(s), want success and %d", freshErr, len(fresh.history), len(tc.chunks))
			}
			for i, ev := range fresh.history {
				if !ev.StreamData {
					t.Fatalf("PRECONDITION FAILED: recorded chunk %d is not marked as plugin output, so there is nothing for compaction to carry", i)
				}
			}

			cs, err := extractCompactionState(fresh.history)
			if err != nil {
				t.Fatalf("extractCompactionState: %v", err)
			}
			full := buildFullHistoryFromCompaction(nil, cs)
			if len(full) != len(fresh.history) {
				t.Fatalf("expanded history has %d events, recorded %d", len(full), len(fresh.history))
			}
			for i := range full {
				if !full[i].StreamData {
					t.Errorf("chunk %d lost StreamData through compaction", i)
				}
			}

			replayed := newTestExecSession()
			registerOneChunkStream(replayed, tc.chunks...)
			replayed.isReplay = true
			replayed.history = full
			replayErr, replayOut := streamCallOutput(t, replayed)
			if replayErr != 0 || replayOut != freshOut {
				t.Errorf("a compacted stream replayed as status %d %q, fresh gave success %q", replayErr, replayOut, freshOut)
			}
		})
	}

	// The other side of the discriminator survives compaction too: a stream-level failure is still a failure, with the same message.
	t.Run("a stream failure is still a failure after compaction", func(t *testing.T) {
		fresh := newTestExecSession()
		fresh.engine.pluginStreamRegistry = NewPluginStreamRegistry() // not registered: a stream-level failure
		freshErr, freshOut := streamCallOutput(t, fresh)
		if freshErr == 0 || len(fresh.history) != 1 {
			t.Fatalf("PRECONDITION FAILED: the fresh failure gave status %d, %d event(s)", freshErr, len(fresh.history))
		}
		cs, err := extractCompactionState(fresh.history)
		if err != nil {
			t.Fatalf("extractCompactionState: %v", err)
		}
		replayed := newTestExecSession()
		replayed.engine.pluginStreamRegistry = NewPluginStreamRegistry()
		replayed.isReplay = true
		replayed.history = buildFullHistoryFromCompaction(nil, cs)
		if replayErr, replayOut := streamCallOutput(t, replayed); replayErr != freshErr || replayOut != freshOut {
			t.Errorf("a compacted stream failure replayed as status %d %q, fresh gave %d %q", replayErr, replayOut, freshErr, freshOut)
		}
	})
}
