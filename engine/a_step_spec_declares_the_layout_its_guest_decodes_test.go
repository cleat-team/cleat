package engine

// A step spec declares the result layout its guest decodes, and a divergence is packed in it (cleat#4152).
//
// The dispatcher answers every divergence with the same words, and the words have to be packed into the int64 the guest decodes: three layouts in ABI.md
// (a simple one, the durable-call one, and await_signals' own) put the message length and the error code in different bits. Until this field existed
// the reporter wrote the simple layout for everything, which is correct for every spec on the dispatcher today and WRONG for the six calls ABI.md puts in
// the durable-call layout (cleat_call, cleat_call_retry, cleat_call_heartbeat, plugin_call, plugin_call_streaming, cleat_wait_for_event): a guest
// decoding that answer reads the message length inside its call-error field, a garbage code and a one-byte message. Found when the heartbeat call, the
// first of them, was moved; none of the specs registered at the time was affected (measured by decoding each one's recorded divergence).
//
// Three things are pinned here, each with its own control:
//
//	the field is REQUIRED          a spec without a valid one is refused at validate(), by text, and the same spec with one validates, so the
//	                               refusal is the field's and not another rule's (the neighbouring controls were repaired for exactly this: with
//	                               the field required, a "bad" spec that already lacked it was refused for the NEW reason and stopped testing its own);
//	every return site packs it     a divergence is driven through all SIX places the dispatcher returns one, in each of the three layouts, and
//	                               decoded with the GUEST's decoders (decodeSimpleResultGuest, decodeCallResultGuest, decodeAwaitSignalsResultGuest),
//	                               not with a decoder written to agree with the packer;
//	await_signals' bound           its payload length is 16 bits and a message's length is not: the packing clamps, so a long message cannot spill
//	                               into the name field. No message the engine writes is that long, so the clamp is pinned on the helper.

import (
	"context"
	"strings"
	"testing"
)

func TestAStepSpecWithoutAValidResultLayoutIsRefusedByText(t *testing.T) {
	ok := func() stepSpec {
		return stepSpec{Result: layoutSimple, NoIdentity: "x", Name: "x", Want: fakeEvent, Effect: effectEffect, Records: recordsOne,
			Decode: func(*stepCtx, *EventRecord) int64 { return 0 }, Fresh: func(*stepCtx) int64 { return 0 }}
	}
	// CONTROL: the spec the cases below start from is valid, so each refusal is attributable to the one thing changed.
	if err := func() error { s := ok(); return s.validate() }(); err != nil {
		t.Fatalf("PRECONDITION FAILED: the base spec does not validate: %v", err)
	}
	for name, c := range map[string]struct {
		mutate func(*stepSpec)
		want   string // the text that says WHICH mistake it was: an unset layout and a nonexistent one are different
	}{
		"no layout declared (the zero value)": {func(s *stepSpec) { s.Result = layoutUnset }, "declares no result layout"},
		"a layout below the range":            {func(s *stepSpec) { s.Result = -1 }, "result layout that does not exist"},
		"a layout above the range":            {func(s *stepSpec) { s.Result = layoutAwaitSignals + 1 }, "result layout that does not exist"},
	} {
		s := ok()
		c.mutate(&s)
		err := s.validate()
		if err == nil {
			t.Errorf("validate accepted a spec with %s", name)
		} else if !strings.Contains(err.Error(), c.want) {
			t.Errorf("a spec with %s was refused for the wrong reason: %q does not contain %q", name, err.Error(), c.want)
		}
	}
	for _, l := range []resultLayout{layoutSimple, layoutDurableCall, layoutAwaitSignals} {
		s := ok()
		s.Result = l
		if err := s.validate(); err != nil {
			t.Errorf("layout %d was refused: %v", l, err)
		}
	}
}

// layoutProbe is one place the dispatcher returns a divergence, with what drives it there.
type layoutProbe struct {
	site  string
	drive func(layout resultLayout, buf []byte) int64
}

func layoutProbes() []layoutProbe {
	run := func(layout resultLayout, hist []EventRecord, mut func(*stepSpec), s1 string, buf []byte) int64 {
		var ran int32
		var dec, pre []EventRecord
		sp := runSpec(&ran, &dec, &pre)
		sp.Result = layout
		if mut != nil {
			mut(sp)
		}
		s := stepSession(nil, hist)
		return s.step(context.WithValue(context.Background(), wasmMemBufKey{}, buf), nil, sp, stepArgs{s1: s1, outPtr: 0, outMaxLen: uint32(len(buf))})
	}
	single := func(layout resultLayout, hist []EventRecord, identity string, buf []byte) int64 {
		var ran int32
		sp := fakeSpec(effectEffect, recordsOne, &ran, recordOneFake)
		sp.Result = layout
		if identity != "" {
			sp.NoIdentity = ""
			sp.Identity = func(stepArgs, *EventRecord) string { return identity }
		}
		s := stepSession(nil, hist)
		return s.step(context.WithValue(context.Background(), wasmMemBufKey{}, buf), nil, sp, stepArgs{outPtr: 0, outMaxLen: uint32(len(buf))})
	}
	return []layoutProbe{
		{"single-record step: the record is another type", func(l resultLayout, b []byte) int64 {
			return single(l, []EventRecord{{Step: 0, EventType: runOtherEvent}}, "", b)
		}},
		{"single-record step: the record is another call's (identity)", func(l resultLayout, b []byte) int64 {
			return single(l, []EventRecord{{Step: 0, EventType: fakeEvent}}, "names another call", b)
		}},
		{"run: the first record is foreign", func(l resultLayout, b []byte) int64 {
			return run(l, []EventRecord{runRec(0, runOtherEvent, "a")}, nil, "a", b)
		}},
		{"run: a record of the run names another call (identity)", func(l resultLayout, b []byte) int64 {
			return run(l, []EventRecord{runRec(0, runFinalEvent, "b")}, nil, "a", b)
		}},
		{"run: a foreign record ends a run that has no final", func(l resultLayout, b []byte) int64 {
			return run(l, []EventRecord{runRec(0, runPrefixEvent, "a"), runRec(1, runOtherEvent, "a")}, nil, "a", b)
		}},
		{"run: a record the spec's Role does not classify", func(l resultLayout, b []byte) int64 {
			return run(l, []EventRecord{runRec(0, runPrefixEvent, "a"), runRec(1, runOtherEvent, "a")}, func(sp *stepSpec) {
				sp.Role = func(_ stepArgs, rec *EventRecord) recordRole {
					if rec.EventType == runPrefixEvent {
						return rolePrefix
					}
					return recordRole(99)
				}
			}, "a", b)
		}},
	}
}

func TestADivergenceIsPackedInTheLayoutItsGuestDecodesAtEverySite(t *testing.T) {
	probes := layoutProbes()
	if len(probes) != 6 {
		t.Fatalf("PRECONDITION FAILED: %d probes, want the six places the dispatcher returns a divergence (two in step, four in replayRun)", len(probes))
	}
	for _, p := range probes {
		for _, layout := range []resultLayout{layoutSimple, layoutDurableCall, layoutAwaitSignals} {
			p, layout := p, layout
			t.Run(p.site+"/layout="+layoutName(layout), func(t *testing.T) {
				buf := make([]byte, 600)
				res := p.drive(layout, buf)
				var n int
				switch layout {
				case layoutSimple:
					extra, errCode := decodeSimpleResultGuest(res)
					if errCode != 1 {
						t.Fatalf("errCode = %d, want 1 (result %#x)", errCode, res)
					}
					n = int(extra)
				case layoutDurableCall:
					responseLen, callErrorCode, errCode := decodeCallResultGuest(res)
					if errCode != 1 || callErrorCode != uint32(callErrorUnknown) {
						t.Fatalf("errCode = %d callErrorCode = %d, want 1 and callErrorUnknown (%d) (result %#x)", errCode, callErrorCode, callErrorUnknown, res)
					}
					n = int(responseLen)
				case layoutAwaitSignals:
					sigNameLen, payloadLen, timedOut, errCode := decodeAwaitSignalsResultGuest(res)
					if errCode != 1 || timedOut || sigNameLen != 0 {
						t.Fatalf("errCode = %d timedOut = %v sigNameLen = %d, want 1, false, 0 (result %#x)", errCode, timedOut, sigNameLen, res)
					}
					n = int(payloadLen)
				}
				// The length the guest decoded must be the length of the message that was written: a layout packed in the wrong bits reads some
				// other number here, and a length that is right by accident still has to end the message exactly where the buffer's text does.
				if n <= 0 || n >= len(buf) || buf[n] != 0 || buf[n-1] == 0 {
					t.Fatalf("the guest decodes a message length of %d, which is not where the written text ends (result %#x, text %q)", n, res, strings.TrimRight(string(buf), "\x00"))
				}
				if msg := string(buf[:n]); !strings.HasPrefix(msg, "replay divergence at step") {
					t.Errorf("the text the guest reads is %q, want a replay divergence message", msg)
				}
			})
		}
	}
}

func layoutName(l resultLayout) string {
	switch l {
	case layoutSimple:
		return "simple"
	case layoutDurableCall:
		return "durableCall"
	case layoutAwaitSignals:
		return "awaitSignals"
	}
	return "unset"
}

// The real await_signals, through the real call, decoded by its guest's decoder. The case above proves the layout on a fake spec; this proves the
// registered spec declares the layout its call's ABI row names, which a fake cannot (a spec registered as the simple layout decodes the same for a
// short message, so only the clamp below would tell them apart).
func TestAnAwaitSignalsDivergenceDecodesAsItsOwnLayout(t *testing.T) {
	w := &awaitWorld{}
	s := &execSession{
		engine: NewEngine(nil, &mockCaller{}, WithClock(func() int64 { return awaitT }), WithWorkflowID("wf-await"), WithSignalStore(w)), workflowID: "wf-await", nowMs: awaitT,
		deferrals: map[string]string{}, queryState: map[string]string{},
		isReplay: true, history: []EventRecord{{Step: 0, EventType: fakeEvent}},
	}
	buf := make([]byte, 600)
	got := s.DurableAwaitSignals(contextWithRawMemBuf(context.Background(), buf), nil, awaitNames, 60000, 0, 200, 256, 200)
	sigNameLen, payloadLen, timedOut, errCode := decodeAwaitSignalsResultGuest(got)
	if errCode != 1 || timedOut || sigNameLen != 0 {
		t.Fatalf("errCode = %d timedOut = %v sigNameLen = %d, want 1, false, 0 (result %#x)", errCode, timedOut, sigNameLen, got)
	}
	n := 0
	for n < 200 && buf[256+n] != 0 {
		n++
	}
	if payloadLen == 0 || int(payloadLen) != n {
		t.Errorf("payloadLen = %d but %d bytes were written at the payload buffer", payloadLen, n)
	}
	if name := strings.TrimRight(string(buf[:200]), "\x00"); name != "" {
		t.Errorf("the name buffer holds %q, want it empty: a divergence has no signal name", name)
	}
	if msg := string(buf[256 : 256+n]); !strings.HasPrefix(msg, "replay divergence at step") {
		t.Errorf("the payload buffer holds %q, want a replay divergence message", msg)
	}
	// CONTROL: the spec is registered under the layout this test decodes, so a pass is not the simple layout agreeing by accident.
	if awaitSignalsStep.Result != layoutAwaitSignals {
		t.Errorf("awaitSignalsStep declares layout %s, want awaitSignals", layoutName(awaitSignalsStep.Result))
	}
}

// The bound on await_signals' payload length. No message the engine writes comes near 64 KiB (they are a few hundred bytes), so this cannot be driven
// through the call: it is pinned on the helper, with the unclamped layouts beside it as controls that a long length is NOT clamped where it fits.
func TestAnAwaitSignalsDivergenceLengthIsClampedToItsSixteenBits(t *testing.T) {
	long := packSimpleResult(1, 70000) // what the reporter writes for a 70,000-byte message
	aw := stepSpec{Result: layoutAwaitSignals}
	sigNameLen, payloadLen, timedOut, errCode := decodeAwaitSignalsResultGuest(aw.divergenceAnswer(long))
	if payloadLen != 0xFFFF || sigNameLen != 0 || timedOut || errCode != 1 {
		t.Errorf("a 70000-byte message decodes as payloadLen=%d sigNameLen=%d timedOut=%v errCode=%d, want 65535, 0, false, 1: the length must be clamped, not spilled into the name field", payloadLen, sigNameLen, timedOut, errCode)
	}
	if _, payloadLen, _, _ = decodeAwaitSignalsResultGuest(aw.divergenceAnswer(packSimpleResult(1, 250))); payloadLen != 250 {
		t.Errorf("a 250-byte message decodes as payloadLen=%d, want 250 (the clamp must not touch a length that fits)", payloadLen)
	}
	// The EDGE, where an off-by-one in the bound hides (cleat#4293, found by cleat-review3: `> 0x10000` survived rows at 70000 and 250): 65535 fits
	// and is kept; 65536 does not and is clamped to 65535, and unclamped it would land in bit 48, which is sigNameLen.
	for _, c := range []struct{ written, want uint32 }{{0xFFFF, 0xFFFF}, {0x10000, 0xFFFF}} {
		sigNameLen, payloadLen, timedOut, errCode := decodeAwaitSignalsResultGuest(aw.divergenceAnswer(packSimpleResult(1, c.written)))
		if payloadLen != c.want || sigNameLen != 0 || timedOut || errCode != 1 {
			t.Errorf("a %d-byte message decodes as payloadLen=%d sigNameLen=%d timedOut=%v errCode=%d, want %d, 0, false, 1", c.written, payloadLen, sigNameLen, timedOut, errCode, c.want)
		}
	}
	// Controls: the durable-call layout carries 24 bits and the simple one 32, so the same long message is kept whole there.
	dc := stepSpec{Result: layoutDurableCall}
	if responseLen, _, _ := decodeCallResultGuest(dc.divergenceAnswer(long)); responseLen != 70000 {
		t.Errorf("the durable-call layout decodes a 70000-byte message as %d, want 70000", responseLen)
	}
	sm := stepSpec{Result: layoutSimple}
	if extra, _ := decodeSimpleResultGuest(sm.divergenceAnswer(long)); extra != 70000 {
		t.Errorf("the simple layout decodes a 70000-byte message as %d, want 70000", extra)
	}
}

// A nil engine is a session the reporter must still answer: the heartbeat call's old replay tolerated it, and the dispatcher's reporter read
// s.engine.Metrics with no check, so moving that call turned two existing tests (a session built with no engine) into a nil dereference.
func TestADivergenceOnASessionWithNoEngineIsAnsweredNotAPanic(t *testing.T) {
	buf := make([]byte, 600)
	s := &execSession{isReplay: true, history: []EventRecord{{Step: 0, EventType: runOtherEvent}}}
	var ran int32
	sp := fakeSpec(effectEffect, recordsOne, &ran, recordOneFake)
	got := s.step(context.WithValue(context.Background(), wasmMemBufKey{}, buf), nil, sp, stepArgs{outPtr: 0, outMaxLen: uint32(len(buf))})
	if _, errCode := decodeSimpleResultGuest(got); errCode != 1 {
		t.Errorf("errCode = %d, want 1 (result %#x)", errCode, got)
	}
}
