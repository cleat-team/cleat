//go:build cgo

package engine

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
)

// cleat#4130: every guest-supplied string a component dispatcher reads is bounded by the CONFIGURED MaxWasmStringLen, as the core-module path
// bounds it. Until then readStrArg copied an argument whole, so a component guest could hand the host a request as large as its own memory.
//
// One subtest per (dispatcher, payload parameter), each at limit-1, limit and limit+1. The refusal is the one the dispatcher already gives a bad
// NAME, because that is the one its WIT result type can carry: err(call-failure) classified invalid-request, errBadParam for a u64, and a trap
// (or "" while draining defers) for a bare string. A refused call runs no handler method, so it records no event.

type payloadSite struct {
	dispatcher string
	answers    string // "call" | "u64" | "string"
	base       []any  // a valid argument list; each index in payloadIdx is replaced with the sized string
	payloadIdx []int
}

var payloadSites = []payloadSite{
	{"dispatchDurableCallString", "call", []any{"svc", "op", "{}"}, []int{2}},
	{"dispatchDurableCallRetry", "call", []any{"svc", "op", "{}", uint64(3), uint64(1), uint64(2), uint64(10), "[]"}, []int{2, 7}},
	{"dispatchDurableCallHeartbeat", "call", []any{"svc", "op", "{}", uint64(1000)}, []int{2}},
	{"dispatchDurableLog", "u64", []any{"m"}, []int{0}},
	{"dispatchDurableDefer", "string", []any{"d"}, []int{0}},
	{"dispatchContinueAsNew", "u64", []any{"{}"}, []int{0}},
	{"dispatchAwaitSignals", "call", []any{"a", uint64(0)}, []int{0}},
	{"dispatchSignalWorkflow", "u64", []any{"wf", "sig", "{}"}, []int{2}},
	{"dispatchChildWorkflow", "call", []any{"child", "{}"}, []int{1}},
	{"dispatchAwaitAnyChild", "string", []any{"[]"}, []int{0}},
	{"dispatchAwaitAllChildren", "string", []any{"[]"}, []int{0}},
	{"dispatchChildWorkflowWithOptions", "call", []any{"child", "{}", uint64(0), uint64(0), ""}, []int{1}},
	{"dispatchResolvePromise", "u64", []any{"p", "{}"}, []int{1}},
	{"dispatchRejectPromise", "u64", []any{"p", "e"}, []int{1}},
	{"dispatchSetQueryState", "u64", []any{"k", "{}"}, []int{1}},
	{"dispatchCompleteUpdate", "u64", []any{"r", "{}", "e"}, []int{0, 1, 2}},
	{"dispatchDurableSend", "u64", []any{"svc", "op", "{}"}, []int{2}},
	{"dispatchScheduleInvoke", "u64", []any{"svc", "op", "{}", uint64(0)}, []int{2}},
	{"dispatchPluginCall", "call", []any{"plug", "op", "{}"}, []int{2}},
	{"dispatchPluginCallStreaming", "call", []any{"plug", "op", "{}"}, []int{2}},
	{"dispatchUUID", "string", []any{"seed"}, []int{0}},
	{"dispatchRunDetached", "u64", []any{"wf", "{}"}, []int{1}},
	{"dispatchContinueAsNewVersioned", "u64", []any{"{}", uint32(1)}, []int{0}},
	{"dispatchSideEffect", "call", []any{"v"}, []int{0}},
	{"dispatchFetch", "call", []any{"GET", "http://x", "{}", ""}, []int{1, 2, 3}},
	{"dispatchScheduleCron", "call", []any{"nightly", "* * * * *", "UTC", "{}"}, []int{1, 2, 3}},
}

func TestEveryComponentPayloadArgumentIsBoundedByTheConfiguredLimit(t *testing.T) {
	const limit = 256
	old := MaxWasmStringLen
	MaxWasmStringLen = limit
	t.Cleanup(func() { MaxWasmStringLen = old })

	for _, site := range payloadSites {
		for _, idx := range site.payloadIdx {
			for _, size := range []int{limit - 1, limit, limit + 1} {
				name := fmt.Sprintf("%s/arg%d/%d", site.dispatcher, idx, size)
				t.Run(name, func(t *testing.T) {
					h := &mockHostHandler{}
					b := &wasmtimeBackend{handler: h}
					args := append([]any(nil), site.base...)
					args[idx] = strings.Repeat("x", size)

					out, err := b.cgotestDispatchPayload(site.dispatcher, site.answers, args...)
					if err != nil {
						t.Fatal(err)
					}
					refused := size > limit
					if refused && len(h.recorded()) != 0 {
						t.Errorf("a %d-byte argument (limit %d) was refused but the handler still ran: %v", size, limit, h.recorded())
					}
					if !refused && len(h.recorded()) == 0 && out.Trap == "" {
						t.Errorf("a %d-byte argument (limit %d) was not refused but the handler never ran: %+v", size, limit, out)
					}
					switch site.answers {
					case "call":
						gotRefusal := out.CallKind == "failed" && strings.Contains(out.Payload, "over the configured limit")
						if gotRefusal != refused {
							t.Errorf("refused=%v want %v: %+v", gotRefusal, refused, out)
						}
						if refused && out.Code != uint32(callErrorInvalidRequest) {
							t.Errorf("refusal code = %d, want invalid-request (%d)", out.Code, callErrorInvalidRequest)
						}
					case "u64":
						if gotRefusal := out.U64 == errBadParam; gotRefusal != refused {
							t.Errorf("refused=%v want %v: word %#x", gotRefusal, refused, out.U64)
						}
					case "string":
						if gotRefusal := strings.Contains(out.Trap, "over the configured limit"); gotRefusal != refused {
							t.Errorf("refused=%v want %v: %+v", gotRefusal, refused, out)
						}
					}
				})
			}
		}
	}
}

// The table above is a list of sites; this makes it the LIST OF ALL of them. A new payload read in component_callbacks.go that is not in the table
// fails here, and an unbounded readStrArg anywhere in the dispatchers fails too.
func TestTheComponentPayloadTableCoversEveryPayloadRead(t *testing.T) {
	src, err := os.ReadFile("component_callbacks.go")
	if err != nil {
		t.Fatal(err)
	}
	code := regexp.MustCompile(`(?m)^\s*//.*$`).ReplaceAllString(string(src), "")
	if n := strings.Count(code, "readStrArg("); n != 0 {
		t.Errorf("%d unbounded readStrArg call(s) in component_callbacks.go: use readPayloadArg (cleat#4130)", n)
	}
	reads := strings.Count(code, "readPayloadArg(")
	want := 0
	for _, s := range payloadSites {
		want += len(s.payloadIdx)
	}
	if reads == 0 || reads != want {
		t.Errorf("component_callbacks.go has %d readPayloadArg site(s), the table covers %d: add the new site to payloadSites", reads, want)
	}
}

// The limit the sites above are tested at is a small one chosen to keep the table cheap. This runs one site at the REAL default (2 MiB, cleat#4130):
// exactly 2 MiB reaches the handler and one byte more is refused, so the default the docs and the worker flag state is the one enforced.
func TestAComponentRequestIsBoundedAtTheDefaultTwoMiB(t *testing.T) {
	// Establish the precondition rather than skip when it does not hold: this must test the DEFAULT, whatever another test left behind.
	old := MaxWasmStringLen
	MaxWasmStringLen = DefaultMaxWasmStringLen
	t.Cleanup(func() { MaxWasmStringLen = old })
	for _, tc := range []struct {
		size    int
		refused bool
	}{{DefaultMaxWasmStringLen - 1, false}, {DefaultMaxWasmStringLen, false}, {DefaultMaxWasmStringLen + 1, true}} {
		h := &mockHostHandler{}
		b := &wasmtimeBackend{handler: h}
		out, err := b.cgotestDispatchPayload("dispatchDurableCallString", "call", "svc", "op", strings.Repeat("x", tc.size))
		if err != nil {
			t.Fatal(err)
		}
		refused := out.CallKind == "failed" && strings.Contains(out.Payload, "over the configured limit")
		if refused != tc.refused || (len(h.recorded()) == 0) != tc.refused {
			t.Errorf("a %d-byte request: refused=%v, handler calls=%d; want refused=%v", tc.size, refused, len(h.recorded()), tc.refused)
		}
	}
}
