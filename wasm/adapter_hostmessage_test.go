package wasm

import (
	"strings"
	"testing"
)

// packDurableCallResultAdapters are the adapters whose host side returns
// engine.packDurableCallResult, and they are the only ones for which the
// CallErrorCode legend means anything.
//
// That packer is `responseLen<<40 | callErrorCode<<8 | errCode`, so it is the
// only one carrying a cleat.CallErrorCode at all. Re-derive the set with:
//
//	grep -rn 'packDurableCallResult(' --include='*.go' engine/ | grep -v _test.go
//
// which reaches durablecalls.go (freshCall, replayCall, DurableCallWithRetry,
// freshCallWithRetry), heartbeats.go (freshCallWithHeartbeat,
// replayCallWithHeartbeat) and plugins.go (replayPluginCall,
// freshPluginCallInternal, streamFailure, freshPluginCallStreaming,
// replayPluginCallStreaming) -- the five adapters below and nothing else.
//
// Every other packer -- packSimpleResult, packAwaitChildResult,
// packAwaitPromiseResult, packAwaitSignalsResult, packAcquireLockResult -- has
// an errCode and no callErrorCode field, so an adapter on one of those must not
// print the legend either. That is a separate defect from this one and is not
// asserted here; see IMPROVEMENT-PLAN.md 3.200.
var packDurableCallResultAdapters = []string{
	"DurableCall",
	"DurableCallWithRetry",
	"DurableCallWithHeartbeat",
	"PluginCall",
	"PluginCallStreaming",
}

// TestDurableCallAdaptersReportTheHostsMessageNotJustACode asserts that an
// adapter on the durable-call layout hands the host's own error text to
// callErrorMessage, rather than printing a bare number against the legend.
//
// The host writes the reason into the response buffer and packs its length:
// engine/plugins.go's failure path is
//
//	written, _ := s.writeResult(ctx, m, responsePtr, errStr, responseMaxLen)
//	return packDurableCallResult(int(written), callFailureCode, 1)
//
// PluginCall and PluginCallStreaming decoded that length into responseLen, then
// returned on the error branch without ever reading responseBuf -- so a Go
// guest was told "plugin_call: error 1 (...1=timeout...)" while Rust, AS and
// Java were told "plugin function pgvector/upsert not registered". Same host,
// same bytes, three languages reading them and one not.
//
// Two further things were wrong with that number, which is why this test checks
// the message rather than the code. `result & 0xFF` is the errCode field, and
// the host hardcodes it to literal 1 on every failure path -- so the value was
// a constant, not a classification. And the legend beside it enumerates
// CallErrorCode, which lives at bits 8-39 and was never decoded: the real
// classification (callFailureCode = callErrorUnavailable = 2) was discarded
// with the message.
//
// TestHostAdapterReportsCallErrorCodeNotErrCode already pinned this for
// cleat_call. Its doc comment describes the general defect and its assertion
// names one call, so it stayed green while two other adapters on the same
// layout had the same bug. This is the general form.
func TestDurableCallAdaptersReportTheHostsMessageNotJustACode(t *testing.T) {
	const legend = "0=unknown 1=timeout"

	for _, name := range packDurableCallResultAdapters {
		def, ok := adapterDefs[name]
		if !ok {
			t.Fatalf("adapterDefs has no %q -- this list is stale, re-derive it "+
				"with the grep in packDurableCallResultAdapters' doc comment", name)
		}
		stmts := strings.Join(def.ResultStmts, "\n")

		if !strings.Contains(stmts, "callErrorMessage(") {
			t.Errorf("%s: error path never calls callErrorMessage, so the host's "+
				"message is discarded and the guest sees only a code", name)
		}
		if strings.Contains(stmts, legend) {
			t.Errorf("%s: prints the CallErrorCode legend instead of the host's "+
				"message; the host wrote a reason into the response buffer and "+
				"packed its length", name)
		}
		// The legend is only wrong because there is a real message to print
		// instead. An adapter with no response buffer could not do better, so
		// pin that this one has one.
		if !strings.Contains(stmts, "responseBuf") {
			t.Errorf("%s: no responseBuf in the error path; if this call really "+
				"has no output buffer it does not belong in "+
				"packDurableCallResultAdapters", name)
		}
	}
}

// TestPluginCallDecodesCallErrorCodeFromTheRightBits pins the shift, because
// the message and the code come from different fields of one packed word and
// only one of them is checked above.
//
// callErrorCode is bits 8-39. Decoding it as `result & 0xFF` yields errCode,
// which is 1 for every failure -- the constant that made every plugin failure
// read as a timeout.
func TestPluginCallDecodesCallErrorCodeFromTheRightBits(t *testing.T) {
	for _, name := range []string{"PluginCall", "PluginCallStreaming"} {
		stmts := strings.Join(adapterDefs[name].ResultStmts, "\n")
		want := "callErrorCode := uint32((uint64(result) >> 8) & 0xFFFFFFFF)"
		if !strings.Contains(stmts, want) {
			t.Errorf("%s: does not decode callErrorCode from bits 8-39.\nwant: %s", name, want)
		}
		if strings.Contains(stmts, "callErrorMessage(\""+adapterCallName(name)+"\", responseBuf, responseLen, errCode)") {
			t.Errorf("%s: passes errCode where callErrorCode belongs; errCode is 1 "+
				"for every failure and the legend describes CallErrorCode", name)
		}
	}
}

// adapterCallName maps an adapter field name to the host import name it reports
// in its error text.
func adapterCallName(field string) string {
	switch field {
	case "PluginCall":
		return "plugin_call"
	case "PluginCallStreaming":
		return "plugin_call_streaming"
	}
	return field
}

// TestPluginAdaptersDoNotPrefixWhatCallErrorMessageAlreadyNames pins that the
// adapter returns callErrorMessage's result verbatim rather than prefixing it
// with the call name a second time.
//
// callErrorMessage returns one of two things. When the host wrote a message it
// returns that text unchanged; when it did not, it returns its own
// "<callName>: error N (<legend>)". So a caller that wraps the result in
// "<callName>: %s" doubles the name on the fallback path --
//
//	plugin_call: plugin_call: error 2 (0=unknown 1=timeout ...)
//
// -- and on the success path prepends a name the other guests do not print.
// That second half is the one that matters: 3.200 exists to make a Go guest
// report what Rust, AssemblyScript and Java report for the same failure, and
// "plugin_call: blobstore: no tenant context" against their "blobstore: no
// tenant context" is still a divergence, just a smaller one than "error 1".
//
// Measured by WS-2 on the plugin harness after 3.200 landed: llm.chat_stream
// read "plugin_call_streaming: plugin_call_streaming: no plugin stream
// registry configured", the host message in that one case already beginning
// with the call name.
//
// The fallback keeps the call name because callErrorMessage puts it there
// itself -- which is the whole reason the wrapper must not.
func TestPluginAdaptersDoNotPrefixWhatCallErrorMessageAlreadyNames(t *testing.T) {
	for _, name := range []string{"PluginCall", "PluginCallStreaming"} {
		stmts := strings.Join(adapterDefs[name].ResultStmts, "\n")
		call := adapterCallName(name)

		if strings.Contains(stmts, `fmt.Errorf("`+call+`: %s"`) {
			t.Errorf("%s: prefixes callErrorMessage's result with %q, which doubles "+
				"the call name on the fallback path and diverges from the other "+
				"guests on the host-message path", name, call+": ")
		}
		// The property above is only meaningful while the message still comes
		// from callErrorMessage; without this a deleted call would pass.
		if !strings.Contains(stmts, "callErrorMessage(") {
			t.Errorf("%s: no longer calls callErrorMessage", name)
		}
	}
}

// TestNoAdapterPrintsTheCallErrorCodeLegendForAnotherLayout asserts that no
// adapter prints the cleat.CallErrorCode legend inline.
//
// The legend enumerates CallErrorCode values, and only packDurableCallResult
// carries a CallErrorCode -- it is `responseLen<<40 | callErrorCode<<8 |
// errCode`. Every other packer the host uses (packSimpleResult,
// packAwaitChildResult, packAwaitPromiseResult, packAwaitSignalsResult,
// packAcquireLockResult) has an errCode and no such field, so an adapter on one
// of those was printing a legend for a field that does not exist in its own
// result word.
//
// The five packDurableCallResult adapters do still print the legend, but from
// inside callErrorMessage and only when the host wrote no message -- which is
// the one case where it is both true and all there is. That is why this test
// looks for the legend in adapterDefs rather than in the generated file.
//
// What the legend cost, concretely: engine/imports.go returns errBadParam
// (0xFFFFFFFF_00000001) from 64 sites when it cannot read a guest string. Its
// low byte is 1, so every one of those surfaced as "error 1", which the legend
// reads as a timeout rather than a bad parameter. And a rejected promise --
// packAwaitPromiseResult(written, false, 1), carrying rec.PromiseError in the
// buffer -- reported "error 1 (1=timeout)" instead of the rejection reason.
func TestNoAdapterPrintsTheCallErrorCodeLegendForAnotherLayout(t *testing.T) {
	const legend = "0=unknown 1=timeout"
	for name, def := range adapterDefs {
		stmts := strings.Join(def.ResultStmts, "\n")
		if strings.Contains(stmts, legend) {
			t.Errorf("%s: prints the CallErrorCode legend inline. Only "+
				"packDurableCallResult carries a CallErrorCode; if this call is on "+
				"that layout use callErrorMessage, and otherwise use hostErrMessage "+
				"or report the bare code.", name)
		}
	}
}

// TestAdaptersWithAnOutputBufferReportWhatTheHostWroteThere asserts that an
// adapter holding an output buffer reads it on the error path.
//
// The host writes the reason into that same buffer and packs its length:
// AwaitChild's replay path is `writeResult(ctx, m, resultPtr, rec.Err,
// resultMaxLen)` then `packAwaitChildResult(written, 1)`, SideEffect's is
// `errMsg` then `packSimpleResult(1, written)`, AwaitPromise's is
// `rec.PromiseError` then `packAwaitPromiseResult(written, false, 1)`. An
// adapter that returns before reading the buffer throws that away.
//
// hostErrMessage is safe on the paths where nothing was written: it bounds-checks
// the length against the buffer, so errBadParam's 0xFFFFFFFF decodes to a length
// no buffer satisfies and it returns "no detail reported by the host" rather
// than reading out of range.
//
// Adapters with no output buffer -- ContinueAsNew, ContinueAsNewWithVersion,
// AcquireLock, AcquireLockMs, ReleaseLock -- are exempt because there is nothing
// to read. They report the bare code, without a legend describing an enum they
// do not carry.
func TestAdaptersWithAnOutputBufferReportWhatTheHostWroteThere(t *testing.T) {
	for name, def := range adapterDefs {
		stmts := strings.Join(def.ResultStmts, "\n")
		if !strings.Contains(stmts, "if errCode != 0 {") {
			continue // no error branch to check
		}
		if !strings.Contains(stmts, "Buf[") && !strings.Contains(stmts, "Buf)") {
			continue // no output buffer; exempt, see doc comment
		}
		if strings.Contains(stmts, "hostErrMessage(") || strings.Contains(stmts, "callErrorMessage(") {
			continue
		}
		t.Errorf("%s: has an output buffer and an error branch, but reads neither "+
			"hostErrMessage nor callErrorMessage from it -- the host writes the "+
			"reason into that buffer and packs its length, and this discards it", name)
	}
}
