package pluginharness

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/cleat/wasmtest"
)

type intProbeResult struct {
	AmountCents int    `json:"amountCents"`
	Note        string `json:"note"`
}

func buildIntParamsWasm(t *testing.T) []byte {
	t.Helper()
	dir := hostCallFixtureDir(t, "intparams")
	tmpDir := t.TempDir()
	cmd := commandAt(dir, cleatBinary(t), "build", "--target", "go", "-o", tmpDir, dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("cleat build (intparams) failed:\n%s\n%v", string(out), err)
	}
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatalf("reading cleat build output: %v", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".wasm" {
			b, err := os.ReadFile(filepath.Join(tmpDir, e.Name()))
			if err != nil {
				t.Fatalf("reading WASM: %v", err)
			}
			return b
		}
	}
	t.Fatalf("no .wasm in cleat build output: %s", tmpDir)
	return nil
}

// TestAnIntParameterIsDecodedRatherThanScanned is cleat#1036.
//
// int, int32 and int64 were the only parameter types that never reached a
// decoder. Every other type went through json.Unmarshal with a checked error;
// these went through extractJSONInt, a hand-rolled digit scanner:
//
//	for _, c := range rest {
//	    if c >= '0' && c <= '9' { n = n*10 + int(c-'0') } else { break }
//	}
//
// No sign handling, no error return, stops at the first non-digit. So a leading
// `-` broke the loop immediately and the parameter bound 0, having consumed
// nothing -- and a negative integer is well-formed JSON and a valid Go int.
// Any parameter whose domain includes negatives could not be passed at all,
// the -1-means-unbounded convention among them.
//
// THE CASE THAT MAKES IT MORE THAN A CURIOSITY: `amountCents` is a real
// parameter shape, and a refund is negative. The workflow would report `done`
// having charged nothing, with no error anywhere.
//
// The `note` parameter is not decoration. A run that bound nothing at all and
// a run that bound the int wrongly both show amountCents=0; note distinguishes
// them, so a zero here means "decoded as zero" rather than "never ran".
func TestAnIntParameterIsDecodedRatherThanScanned(t *testing.T) {
	env := NewTestPluginEnvInMemory(t)
	defer env.Close()

	wasmBytes := buildIntParamsWasm(t)

	wenv := wasmtest.NewWasmTestEnv(t, wasmtest.WithPluginRegistry(env.Registry))
	defer wenv.Close()

	run := func(t *testing.T, input string) (intProbeResult, string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		result, _, suspended, _, _, err := wenv.H().Execute(ctx, wasmBytes, "report_int", []byte(input))
		if err != nil {
			// A REFUSAL, not a broken harness. This was t.Fatalf until
			// 2026-09-09, which was right when the helper was written and wrong
			// from #1059 onwards: that PR fixed #1056, so a bind failure now
			// surfaces as an engine-level error instead of a `done` workflow
			// carrying the error in its result. The refusal this test asserts
			// began arriving through the other channel and the helper read it as
			// infrastructure breakage.
			//
			// Both shapes are accepted deliberately -- the result-carried form
			// below is still how a workflow-level error arrives -- so this does
			// not re-encode either as the only possibility.
			return intProbeResult{}, err.Error()
		}
		if suspended != nil {
			t.Fatalf("workflow suspended (%s); this fixture does not suspend", suspended.Reason)
		}
		// Check for a refusal FIRST, by looking for the key rather than by
		// whether the result decodes. {"error":"..."} unmarshals into
		// intProbeResult perfectly well -- unknown fields are ignored -- so a
		// decode failure is NOT how a refusal announces itself, and treating it
		// that way reads every refusal as a zero-valued success. That is what
		// the first version of this helper did.
		var probe struct {
			Error string `json:"error"`
		}
		if json.Unmarshal([]byte(result), &probe) == nil && probe.Error != "" {
			return intProbeResult{}, probe.Error
		}
		var got intProbeResult
		if uerr := json.Unmarshal([]byte(result), &got); uerr != nil {
			return intProbeResult{}, result
		}
		return got, ""
	}

	t.Run("a negative binds", func(t *testing.T) {
		got, raw := run(t, `{"amountCents":-750,"note":"refund"}`)
		if raw != "" {
			t.Fatalf("a negative int was refused: %s", raw)
		}
		if got.Note != "refund" {
			t.Fatalf("the run did not bind its string either (note=%q); this case is "+
				"not evidence about ints", got.Note)
		}
		if got.AmountCents != -750 {
			t.Errorf("amountCents bound %d, want -750.\n\n"+
				"0 here is cleat#1036: the digit scanner broke on the leading '-' and "+
				"returned 0 having consumed nothing. A refund would be recorded as a "+
				"charge of nothing, with the workflow reporting done.", got.AmountCents)
		}
	})

	t.Run("a positive still binds", func(t *testing.T) {
		// CONTROL. Without it, a build where every int bound 0 would be
		// indistinguishable from one where only the negative case fails.
		got, raw := run(t, `{"amountCents":4200,"note":"charge"}`)
		if raw != "" {
			t.Fatalf("an ordinary positive int was refused: %s", raw)
		}
		if got.AmountCents != 4200 {
			t.Errorf("amountCents bound %d, want 4200", got.AmountCents)
		}
	})

	t.Run("an ABSENT int binds zero rather than erroring", func(t *testing.T) {
		// cleat#1046 removed the digit scanner so ints reach json.Unmarshal.
		// That also moved ABSENCE: extractJSONRaw returns "" for a missing key
		// and json.Unmarshal("") fails, so an omitted int went from binding 0
		// to a hard error that stopped the body running at all -- eight cases
		// across three modules of the ports suite, found by its CI going red.
		//
		// A string parameter has always bound "" when absent. This asserts ints
		// match, which is the contract callers already relied on.
		got, raw := run(t, `{"note":"no-amount"}`)
		if raw != "" {
			t.Fatalf("an omitted int parameter was refused: %s\n\n"+
				"Absent is not malformed. The zero value is the documented default "+
				"and the body must still run.", raw)
		}
		if got.Note != "no-amount" {
			t.Fatalf("the run did not bind its string either (note=%q); the body may "+
				"not have executed at all", got.Note)
		}
		if got.AmountCents != 0 {
			t.Errorf("an omitted amountCents bound %d, want 0", got.AmountCents)
		}
	})

	t.Run("a quoted number is refused rather than silently zeroed", func(t *testing.T) {
		got, raw := run(t, `{"amountCents":"4200","note":"quoted"}`)
		if raw == "" {
			t.Errorf("a string where an int belongs bound %d and reported success.\n\n"+
				"json.Unmarshal rejects this; the scanner it replaced returned 0 with "+
				"no error, which is how a caller's type mistake became a silent zero.",
				got.AmountCents)
			return
		}
		if !strings.Contains(raw, "amountCents") {
			t.Errorf("refused, but the error does not name the parameter: %s", raw)
		}
	})
}
