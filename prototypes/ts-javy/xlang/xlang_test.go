//go:build cgo

// Q4 (typescript-javy prototype plan): "Go history replays on the TS module and vice versa, bit-identical result".
//
// The same `place_order` exists in Go (testdata/crosslang), Rust (examples/rust-workflow) and TypeScript
// (workflows/crosslang). Each is executed through the engine on wasmtime, its history captured, and that history replayed
// against each of the others. Beyond the pass line, the histories themselves are compared event by event, and the host's
// canonical JSON is checked against Go's own encoding/json. Modules are built by phase3_build.sh; XLANG_DIR points at them.
// Same shape as tests/cross-language, which this does not modify.
package xlang

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/cleat/wasmtest"
	"github.com/cleat-team/cleat/engine"
)

const (
	goInput   = `{"input":{"user_id":"cross-test","cart":[{"sku":"SKU-001","quantity":2}]}}` // Go SDK: wrapped under the parameter name
	flatInput = `{"user_id":"cross-test","cart":[{"sku":"SKU-001","quantity":2}]}`           // Rust and TS: flat
)

var inputs = map[string]string{"go": goInput, "rust": flatInput, "ts": flatInput}

func wasmOf(t *testing.T, lang string) []byte {
	t.Helper()
	dir := os.Getenv("XLANG_DIR")
	if dir == "" {
		t.Skip("UNMEASURED: XLANG_DIR is not set (run through phase3_verify.py)")
	}
	b, err := os.ReadFile(filepath.Join(dir, lang+".wasm"))
	if err != nil {
		t.Fatalf("UNMEASURED: %v", err)
	}
	return b
}

func exec(t *testing.T, lang string) (string, []engine.EventRecord) {
	t.Helper()
	env := wasmtest.NewWasmTestEnv(t, wasmtest.WithDefName("xlang-"+lang))
	defer env.Close()
	res, hist, err := env.Execute(t, wasmOf(t, lang), "place_order", inputs[lang])
	if err != nil {
		t.Fatalf("execute %s: %v", lang, err)
	}
	if res == "" || len(hist) == 0 {
		t.Fatalf("UNMEASURED: %s produced result=%q events=%d", lang, res, len(hist))
	}
	return res, hist
}

func replay(t *testing.T, lang string, hist []engine.EventRecord) (string, error) {
	t.Helper()
	env := wasmtest.NewWasmTestEnv(t, wasmtest.WithDefName("xlang-"+lang))
	defer env.Close()
	return env.Replay(t, wasmOf(t, lang), "place_order", inputs[lang], hist)
}

func shape(h []engine.EventRecord) []string {
	var out []string
	for _, e := range h {
		out = append(out, string(e.EventType)+"|"+e.Service+"|"+e.Op)
	}
	return out
}

func TestEveryDirectionReplaysWithABitIdenticalResult(t *testing.T) {
	langs := []string{"go", "ts", "rust"}
	results := map[string]string{}
	hist := map[string][]engine.EventRecord{}
	for _, l := range langs {
		results[l], hist[l] = exec(t, l)
		t.Logf("%-4s executed: result=%q events=%d  shape=%v", l, results[l], len(hist[l]), shape(hist[l]))
	}
	for _, from := range langs {
		for _, to := range langs {
			got, err := replay(t, to, hist[from])
			kind := "cross-language"
			if from == to {
				kind = "same-language (control)"
			}
			if err != nil {
				t.Errorf("%s exec -> %s replay (%s): %v", from, to, kind, err)
				continue
			}
			same := got == results[from]
			t.Logf("%-4s exec -> %-4s replay  %-24s result=%q  bit-identical=%v", from, to, kind, got, same)
			if !same {
				t.Errorf("%s exec -> %s replay: %q != %q", from, to, got, results[from])
			}
		}
	}
}

// The histories, not just the results. Event type, service and operation must match (that is what replay consumes); the recorded
// REQUEST bytes are reported, not required: ABI 6.3 says replay matches on step index and service/operation, not request content.
func TestHistoriesAreComparableEventByEvent(t *testing.T) {
	h := map[string][]engine.EventRecord{}
	for _, l := range []string{"go", "ts", "rust"} {
		_, h[l] = exec(t, l)
	}
	ref := shape(h["go"])
	for _, l := range []string{"ts", "rust"} {
		got := shape(h[l])
		if strings.Join(got, ",") != strings.Join(ref, ",") {
			t.Errorf("%s history shape differs from go:\n go: %v\n %s: %v", l, ref, l, got)
		}
	}
	diff := 0
	for i := range h["go"] {
		for _, l := range []string{"ts", "rust"} {
			if i < len(h[l]) && h[l][i].Request != h["go"][i].Request {
				diff++
				t.Logf("REQUEST BYTES differ at event %d (%s.%s) between go and %s:\n    go:   %s\n    %-4s: %s", i, h["go"][i].Service, h["go"][i].Op, l, h["go"][i].Request, l, h[l][i].Request)
			}
		}
	}
	t.Logf("recorded request bytes that differ from the Go twin's: %d (not a replay failure: replay does not compare them)", diff)
	if h["ts"][1].Request == "" {
		t.Errorf("UNMEASURED: the TS twin recorded no request bytes to compare")
	}
}

// Red control: a tampered history must NOT replay cleanly, in either direction, or "bit-identical" above says nothing.
func TestATamperedHistoryIsRefusedInEveryDirection(t *testing.T) {
	for _, from := range []string{"go", "ts"} {
		_, hist := exec(t, from)
		var callIdx = -1
		for i, e := range hist {
			if e.Service == "payments" && e.Op == "Charge" {
				callIdx = i
			}
		}
		if callIdx < 0 {
			t.Fatalf("UNMEASURED: no payments.Charge event in the %s history", from)
		}
		tampered := append([]engine.EventRecord(nil), hist...)
		tampered[callIdx].Op = "Refund"
		for _, to := range []string{"go", "ts", "rust"} {
			_, err := replay(t, to, tampered)
			t.Logf("%-3s history with payments.Charge -> payments.Refund, replayed on %-4s: refused=%v", from, to, err != nil)
			if err == nil {
				t.Errorf("a tampered %s history replayed cleanly on %s", from, to)
			} else if !strings.Contains(err.Error(), "divergence") {
				t.Errorf("refused, but not as a replay divergence (%s on %s): %.200s", from, to, err.Error())
			}
		}
	}
}

// ABI 6.3: the host's cleat_json_stringify is Go's encoding/json. Verified independently here (Unmarshal + Marshal in Go), then the
// cases where raw JavaScript JSON.stringify differs from it are listed. The rows are the evidence.
func TestHostCanonicalJSONIsGoAndRawJSDiffers(t *testing.T) {
	env := wasmtest.NewWasmTestEnv(t, wasmtest.WithDefName("xlang-jsoncanon"))
	defer env.Close()
	b, err := os.ReadFile(filepath.Join(os.Getenv("XLANG_DIR"), "jsoncanon.wasm"))
	if err != nil {
		t.Skip("UNMEASURED: XLANG_DIR/jsoncanon.wasm not available")
	}
	res, _, err := env.Execute(t, b, "jsoncanon", `{}`)
	if err != nil {
		t.Fatalf("execute jsoncanon: %v", err)
	}
	var rows []struct {
		Name  string  `json:"name"`
		Raw   string  `json:"raw"`
		Canon *string `json:"canon"`
		Error *string `json:"error"`
	}
	if err := json.Unmarshal([]byte(res), &rows); err != nil || len(rows) < 10 {
		t.Fatalf("UNMEASURED: could not read the probe result (%v): %.200s", err, res)
	}
	var differs []string
	for _, r := range rows {
		var v interface{}
		if err := json.Unmarshal([]byte(r.Raw), &v); err != nil {
			t.Errorf("%s: raw JS output is not valid JSON for Go: %v", r.Name, err)
			continue
		}
		want, _ := json.Marshal(v)
		if r.Canon == nil {
			t.Errorf("%s: the host refused: %v", r.Name, *r.Error)
			continue
		}
		hostIsGo := *r.Canon == string(want)
		rawSame := *r.Canon == r.Raw
		t.Logf("%-20s hostMatchesGo=%-5v rawJSMatchesCanonical=%-5v\n    raw:   %s\n    canon: %s", r.Name, hostIsGo, rawSame, r.Raw, *r.Canon)
		if !hostIsGo {
			t.Errorf("%s: the host's canonical form is NOT Go's encoding/json output:\n host: %s\n go:   %s", r.Name, *r.Canon, want)
		}
		if !rawSame {
			differs = append(differs, r.Name)
		}
	}
	t.Logf("cases where raw JavaScript JSON.stringify differs from the canonical form: %v", differs)
	if len(differs) == 0 {
		t.Errorf("UNMEASURED/IMPLAUSIBLE: no case differed; the probe could not have disagreed")
	}
}

// Beyond place_order, whose single-key result happens to match raw JavaScript JSON.stringify: a result whose raw JS text is NOT
// what Go's encoding/json produces. The SDK's canonical result path must make it byte-identical to the Go twin, and the build
// with RESULT_CANONICAL=false is the red control that shows it would not be otherwise.
func TestMultiKeyResultIsByteIdenticalToGoOnlyWithCanonicalResults(t *testing.T) {
	run := func(file, entry, input string) string {
		env := wasmtest.NewWasmTestEnv(t, wasmtest.WithDefName("xlang-"+file))
		defer env.Close()
		b, err := os.ReadFile(filepath.Join(os.Getenv("XLANG_DIR"), file+".wasm"))
		if err != nil {
			t.Skipf("UNMEASURED: %v", err)
		}
		res, _, err := env.Execute(t, b, entry, input)
		if err != nil {
			t.Fatalf("execute %s: %v", file, err)
		}
		return res
	}
	goRes := run("gocontrol", "go_multi_key", `{"in":{"ms":0}}`)
	tsRes := run("multikey_ts", "multikey", `{}`)
	rawRes := run("multikey_ts_raw", "multikey", `{}`)
	t.Logf("go (encoding/json):                 %s", goRes)
	t.Logf("ts, SDK canonical result (default): %s", tsRes)
	t.Logf("ts, RAW JSON.stringify (control):   %s", rawRes)
	if tsRes != goRes {
		t.Errorf("the canonical TS result is not byte-identical to the Go twin's:\n ts: %s\n go: %s", tsRes, goRes)
	}
	if rawRes == goRes {
		t.Errorf("red control failed: raw JSON.stringify equalled the Go output, so this test could not have disagreed")
	}
}

// ts and rust both build their requests canonically, so their recorded bytes should agree with each other even where they differ
// from Go's (struct field order). Asserted, because the previous test only compared each against Go.
func TestTSAndRustRecordTheSameRequestBytes(t *testing.T) {
	_, ts := exec(t, "ts")
	_, rust := exec(t, "rust")
	for i := range ts {
		if i < len(rust) && ts[i].Request != rust[i].Request {
			t.Errorf("event %d (%s.%s) request bytes differ between ts and rust:\n ts:   %s\n rust: %s", i, ts[i].Service, ts[i].Op, ts[i].Request, rust[i].Request)
		}
	}
}

// ABI 6.6 "NaN canonicalization", measured instead of inferred. The same NaN: Go's encoding/json refuses to encode it, so the Go
// workflow FAILS; JavaScript's JSON.stringify turns it into null before the host ever sees it, so the TS workflow SUCCEEDS with a
// null. Two languages, one value, two outcomes: that is a difference in what the SDKs do, not something replay can reconcile.
func TestNaNIsAnErrorInGoAndNullInTypeScript(t *testing.T) {
	env := wasmtest.NewWasmTestEnv(t, wasmtest.WithDefName("xlang-nan"))
	defer env.Close()
	dir := os.Getenv("XLANG_DIR")
	if dir == "" {
		t.Skip("UNMEASURED: XLANG_DIR is not set")
	}
	goB, err1 := os.ReadFile(filepath.Join(dir, "gocontrol.wasm"))
	tsB, err2 := os.ReadFile(filepath.Join(dir, "nan_ts.wasm"))
	if err1 != nil || err2 != nil {
		t.Fatalf("UNMEASURED: %v %v", err1, err2)
	}
	_, _, goErr := env.Execute(t, goB, "go_not_a_number", `{"in":{"ms":0}}`)
	tsRes, _, tsErr := env.Execute(t, tsB, "nan", `{}`)
	t.Logf("go: err=%v", goErr)
	t.Logf("ts: result=%q err=%v", tsRes, tsErr)
	if goErr == nil || !strings.Contains(goErr.Error(), "NaN") {
		t.Errorf("expected the Go workflow to fail on NaN, got err=%v", goErr)
	}
	if tsErr != nil || tsRes != `{"v":null}` {
		t.Errorf("expected the TS workflow to return {\"v\":null}, got result=%q err=%v", tsRes, tsErr)
	}
}
