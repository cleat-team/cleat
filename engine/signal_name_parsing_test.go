package engine

import (
	"context"
	"testing"
	"time"
)

// cleat#975: the two await paths parsed the guest's signal-name argument
// differently. The replay arm tried JSON with a comma-split fallback; the fresh
// path used the comma-split alone, so `["a","b"]` became `["a` and `"b]` and
// matched nothing in the store.
//
// It produced a correct result, which is why it survived: the fresh await
// missed the delivery and suspended, and the replay arm found the same delivery
// one segment later. The workflow got the right signal and nothing reported
// anything -- while the delivery-found branch of the fresh path could not
// execute with the input a guest actually sends.

type nameParseStore struct {
	deliveries map[string]SignalDelivery
	polled     []string
}

func (n *nameParseStore) DeliverSignal(ctx context.Context, wf, s, p string) error { return nil }
func (n *nameParseStore) PollSignal(ctx context.Context, wf, name string) (SignalDelivery, bool, error) {
	n.polled = append(n.polled, name)
	d, ok := n.deliveries[name]
	return d, ok, nil
}
func (n *nameParseStore) ConsumeSignal(ctx context.Context, wf string, id int64) error { return nil }
func (n *nameParseStore) PollCancellation(ctx context.Context, wf string) (bool, string, error) {
	return false, "", nil
}

// TestBothAwaitPathsParseTheSameNames is the property that was missing.
//
// Testing either path alone cannot reveal the defect -- each is self-consistent
// and the combined outcome was right. Only asking whether they AGREE on one
// input shows it.
func TestBothAwaitPathsParseTheSameNames(t *testing.T) {
	for _, in := range []string{
		`["a","b"]`, // what a guest sends
		`["a"]`,
		"a,b", // the bare form engine callers use
		"a",
		"",
	} {
		fresh := &nameParseStore{deliveries: map[string]SignalDelivery{}}
		freshEng := NewEngine(nil, &mockCaller{}, WithSignalStore(fresh), WithWorkflowID("wf-1"))
		fs := &execSession{engine: freshEng, workflowID: "wf-1", nowMs: time.Now().UnixMilli(),
			deferrals: map[string]string{}, queryState: map[string]string{}}
		buf := make([]byte, 512)
		fs.DurableAwaitSignals(contextWithRawMemBuf(context.Background(), buf), nil,
			in, 20000, 0, 200, 256, 200)

		replay := &nameParseStore{deliveries: map[string]SignalDelivery{}}
		replayEng := NewEngine(nil, &mockCaller{}, WithSignalStore(replay), WithWorkflowID("wf-1"))
		rs := &execSession{engine: replayEng, workflowID: "wf-1", nowMs: time.Now().UnixMilli(),
			deferrals: map[string]string{}, queryState: map[string]string{},
			isReplay: true,
			history: []EventRecord{{
				Step: 0, EventType: EventTypeAwaitSignals, SignalNames: in,
				TimeoutMs: 20000, TimestampMs: time.Now().UnixMilli(),
			}},
		}
		buf2 := make([]byte, 512)
		rs.DurableAwaitSignals(contextWithRawMemBuf(context.Background(), buf2), nil,
			in, 20000, 0, 200, 256, 200)

		if len(fresh.polled) != len(replay.polled) {
			t.Errorf("input %q: fresh path polled %v, replay arm polled %v",
				in, fresh.polled, replay.polled)
			continue
		}
		for i := range fresh.polled {
			if fresh.polled[i] != replay.polled[i] {
				t.Errorf("input %q: fresh path polled %v, replay arm polled %v",
					in, fresh.polled, replay.polled)
				break
			}
		}
	}
}

// TestAFreshAwaitFindsADeliveryUnderTheNameAGuestSends is the branch that could
// not execute. It is written with the JSON form on purpose -- with the
// comma-split parser this returns "" and suspends.
func TestAFreshAwaitFindsADeliveryUnderTheNameAGuestSends(t *testing.T) {
	store := &nameParseStore{deliveries: map[string]SignalDelivery{"a": {ID: 1, Payload: "p"}}}
	eng := NewEngine(nil, &mockCaller{}, WithSignalStore(store), WithWorkflowID("wf-1"))
	s := &execSession{engine: eng, workflowID: "wf-1", nowMs: time.Now().UnixMilli(),
		deferrals: map[string]string{}, queryState: map[string]string{}}

	buf := make([]byte, 512)
	packed := s.DurableAwaitSignals(contextWithRawMemBuf(context.Background(), buf), nil,
		`["a","b"]`, 20000, 0, 200, 256, 200)

	nameLen := uint32((packed >> 48) & 0xFFFF)
	if got := string(buf[:nameLen]); got != "a" {
		t.Errorf("a fresh await returned %q for a delivery that is in the store under "+
			"\"a\".\n\nThe guest sends [\"a\",\"b\"]; a comma-split parser turns that "+
			"into `[\"a` and `\"b]` and matches nothing, so this branch never runs and "+
			"the delivery is only found on the next replay. polled=%v", got, store.polled)
	}
	if s.suspendErr != nil {
		t.Error("suspended despite a delivery being available under the requested name")
	}
}

// TestParseSignalNamesHandlesBothForms pins the parser itself, including the
// fallback -- engine callers pass bare and comma-separated lists, and dropping
// that would trade this defect for its mirror image.
func TestParseSignalNamesHandlesBothForms(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []string
	}{
		{`["a","b"]`, []string{"a", "b"}},
		{`["only"]`, []string{"only"}},
		{"a,b", []string{"a", "b"}},
		{"solo", []string{"solo"}},
		{"", nil},
	} {
		got := parseSignalNames(tc.in)
		if len(got) != len(tc.want) {
			t.Errorf("parseSignalNames(%q) = %v, want %v", tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("parseSignalNames(%q) = %v, want %v", tc.in, got, tc.want)
				break
			}
		}
	}
}
