package host

import (
	"testing"
)

// TestPluginCallCompactionRoundTrip verifies that plugin_call and
// plugin_call_stream_chunk events survive a compaction round-trip.
func TestPluginCallCompactionRoundTrip(t *testing.T) {
	events := []EventRecord{
		{
			Step:      0,
			EventType: EventTypePluginCall,
			PluginName:   "test-plugin",
			PluginFunc:   "DoSomething",
			PluginInput:  `{"key":"value"}`,
			PluginOutput: `{"result":"ok"}`,
			PluginError:  "",
		},
		{
			Step:      1,
			EventType: EventTypePluginCallStreamChunk,
			PluginName:   "test-plugin",
			PluginFunc:   "DoSomething",
			PluginInput:  `{"key":"value"}`,
			PluginOutput: `{"chunk":1}`,
			PluginError:  "",
		},
		{
			Step:      2,
			EventType: EventTypePluginCallStreamChunk,
			PluginName:   "test-plugin",
			PluginFunc:   "DoSomething",
			PluginInput:  `{"key":"value"}`,
			PluginOutput: `{"chunk":2,"finish":true}`,
			PluginError:  "",
		},
		{
			Step:      3,
			EventType: EventTypePluginCall,
			PluginName:   "test-plugin",
			PluginFunc:   "GetObject",
			PluginInput:  `{"bucket":"x","key":"y"}`,
			PluginOutput: "",
			PluginError:  "not found",
		},
	}

	// Compact at every possible split point and verify round-trip.
	for split := 1; split <= len(events); split++ {
		compacted := events[:split]
		tail := events[split:]

		cs := extractCompactionState(compacted)
		reconstructed := buildFullHistoryFromCompaction(tail, cs)

		if len(reconstructed) != len(events) {
			t.Errorf("split=%d: length mismatch: got %d events, want %d",
				split, len(reconstructed), len(events))
			continue
		}

		for i := range events {
			if !eventFieldsMatch(events[i], reconstructed[i]) {
				t.Errorf("split=%d: event %d (%s) mismatch", split, i, events[i].EventType)
				dumpEventDiff(t, events[i], reconstructed[i])
			}
		}
	}
}
