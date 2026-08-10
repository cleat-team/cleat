package wasm

// IMPROVEMENT-PLAN 3.22 step 4. The generated entry-point wrapper turns a
// workflow's returned error into a JSON result string. It built that string by
// wrapping encodeJSONString's output in quotes -- and encodeJSONString already
// wraps its argument in quotes, so the result was
//
//	{"error":""durable call payments.Ship: [0] [AMBIGUOUS] ...""}
//
// which is not valid JSON. FinalizeWorkflowSegment replaces an unstorable
// result with {}, so every error a workflow returned through this path was
// dropped: the crash scenario in tests/crash saw a workflow that could not know
// the outcome of a charge recorded as `done` with an empty result.
//
// These tests execute the emitted expressions rather than pattern-matching the
// emitted text, because the defect was in what the code *evaluates to*, not in
// how it reads.

import (
	"encoding/json"
	"strings"
	"testing"
)

// encodeJSONStringRef is the helper the generator emits into every generated
// package, copied verbatim from GenerateMemory's template. The copy is the
// point: these tests are about the interaction between that helper and its
// call sites, so it has to behave exactly as the emitted one does.
//
// TestEmittedHelperMatchesReference below fails if the two drift apart.
func encodeJSONStringRef(s string) string {
	var buf []byte
	buf = append(buf, '"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '"' || c == '\\' {
			buf = append(buf, '\\')
		}
		buf = append(buf, c)
	}
	buf = append(buf, '"')
	return string(buf)
}

func TestEmittedHelperMatchesReference(t *testing.T) {
	// The emitted source must still define the helper this file mirrors. If
	// the generator's copy changes, the tests below stop describing reality.
	//
	// A hard failure rather than a skip: "the helper moved" is exactly when
	// these tests silently stop meaning anything, which is the state a skip
	// would preserve.
	result, _ := loadBasic(t)
	code := string(GenerateExports("mypkg", result, "go"))
	if !strings.Contains(code, "func encodeJSONString(s string) string {") {
		t.Fatal("GenerateExports no longer emits encodeJSONString; find where it went and repoint " +
			"encodeJSONStringRef in this file, because the tests below assume they are identical")
	}
	for _, fragment := range []string{`buf = append(buf, '"')`, "if c == '\"' || c == '\\\\' {"} {
		if !strings.Contains(code, fragment) {
			t.Errorf("the emitted encodeJSONString no longer contains %q, so encodeJSONStringRef in this "+
				"file may not match it any more", fragment)
		}
	}
}

// TestErrorResultIsValidJSON is the defect itself: the shape the wrapper builds
// must parse, for the error messages workflows actually produce.
func TestErrorResultIsValidJSON(t *testing.T) {
	for _, msg := range []string{
		// The one that produced 3.22.
		`durable call payments.Ship: [0] [AMBIGUOUS] call outcome unknown at step 2: the external ` +
			`call to payments.Ship was dispatched but the response was not recorded before a crash.`,
		`plain failure`,
		`a message with "embedded quotes"`,
		`a message with a \ backslash`,
		`invalid character '"' looking for beginning of object key string`,
		``,
	} {
		// What the generator emits now: quotes around the key only, because
		// encodeJSONString supplies the value's own.
		fixed := `{"error":` + encodeJSONStringRef(msg) + `}`
		if !json.Valid([]byte(fixed)) {
			t.Errorf("the emitted form is not valid JSON for %q:\n  %s", msg, fixed)
			continue
		}
		var decoded struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal([]byte(fixed), &decoded); err != nil {
			t.Errorf("unmarshal %s: %v", fixed, err)
			continue
		}
		if decoded.Error != msg {
			t.Errorf("round trip changed the message:\n  got  %q\n  want %q", decoded.Error, msg)
		}

		// What it emitted before, kept as the control: without it, a fix that
		// happened to produce valid JSON for the wrong reason would still pass.
		broken := `{"error":"` + encodeJSONStringRef(msg) + `"}`
		if json.Valid([]byte(broken)) {
			t.Errorf("the double-quoted form is valid JSON for %q, so this test cannot detect the "+
				"defect it exists for:\n  %s", msg, broken)
		}
	}
}

// TestGeneratedExportsEmitsSingleQuotedErrorResult pins the call site, so a
// future edit cannot reintroduce the wrapping quotes while these tests keep
// passing on the reference helper alone.
func TestGeneratedExportsEmitsSingleQuotedErrorResult(t *testing.T) {
	result, cr := loadBasic(t)
	usage := AnalyzeUsage(result, cr)
	code := string(GenerateExports("mypkg", result, "go")) + string(GenerateHostAdapter("mypkg", usage, "go"))

	if strings.Contains(code, `"{\"error\":\"" + encodeJSONString(`) {
		t.Error(`the generated code wraps encodeJSONString's output in quotes again, producing ` +
			`{"error":""msg""}; the quotes belong around the key only`)
	}
	if strings.Contains(code, "`\"}`)") && strings.Contains(code, "err.Error() +") {
		t.Error("the generated code concatenates err.Error() into a JSON string without escaping; " +
			"an unmarshal error containing a quote makes the result unstorable")
	}
}
