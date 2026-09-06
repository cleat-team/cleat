package wasm

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The tests in this file COMPILE AND RUN the JSON helpers the adapter
// generator emits into every guest, rather than asserting that the generated
// text contains their names.
//
// That distinction is the whole reason the file exists. TestWriteManualJSONHelpers
// checks `strings.Contains(code, "func parseChildResultArray")`, which is
// satisfied by a function that is emitted and wrong -- and one was, for as long
// as fan-in has existed. parseChildResultArray split objects with
// strings.Index(json, "{") and strings.Index(json, "}"), so an outcome whose
// result is itself a JSON object
//
//	{"run_id":"a","result":"{\"tag\":\"x\"}"}
//
// was cut at the ESCAPED brace inside the result value, before its own closing
// quote. extractJSONString then read an unterminated string and returned "",
// while run_id survived because it sits before the cut. Every child of every
// AwaitAllChildren came back with an empty result, no error, and a workflow
// that reported success. A name scan cannot see that; running it can.

// generatedJSONHelpers returns the helper source as it is emitted into a guest.
//
// writeManualJSONHelpers is called directly, so those helpers are the real
// generator output. extractJSONString lives in a raw string literal inside
// GenerateExports, which needs a populated *analyzer.AnalysisResult to run, so
// it is sliced out of that literal instead -- the same bytes the generator
// writes, obtained without building an AnalysisResult. If someone moves that
// literal, this fails loudly rather than silently testing less.
func generatedJSONHelpers(t *testing.T) string {
	t.Helper()

	var buf bytes.Buffer
	writeManualJSONHelpers(&buf)

	src, err := os.ReadFile("exports.go")
	if err != nil {
		t.Fatalf("read exports.go: %v", err)
	}
	const marker = "buf.WriteString(`func extractJSONString"
	i := strings.Index(string(src), marker)
	if i < 0 {
		t.Fatalf("extractJSONString is no longer emitted from a raw literal in exports.go; " +
			"this test slices it from there and must be updated rather than skipped")
	}
	rest := string(src)[i+len("buf.WriteString(`"):]
	j := strings.Index(rest, "`")
	if j < 0 {
		t.Fatal("unterminated raw literal after extractJSONString marker")
	}
	return rest[:j] + "\n" + buf.String()
}

// runGeneratedHelpers compiles the emitted helpers together with body and
// returns the program's stdout.
//
// The program depends on nothing but strings and fmt. cleat.ChildResult is
// replaced by a locally declared struct of the same shape, which is a
// substitution of a TYPE NAME only -- the parser bodies are byte-identical to
// what a guest gets. The alternative, a throwaway module requiring
// github.com/cleat-team/cleat/cleat, drags in wasmtime, three database drivers
// and the whole OTel tree to test forty lines of string scanning, and makes a
// unit test need the network.
func runGeneratedHelpers(t *testing.T, body string) string {
	t.Helper()

	helpers := strings.ReplaceAll(generatedJSONHelpers(t), "cleat.ChildResult", "childResult")
	if strings.Contains(helpers, "cleat.") {
		t.Fatalf("the generated helpers now reference something else in the cleat package; " +
			"this harness only substitutes ChildResult and must be updated")
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module adapterexec\n\ngo 1.25\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}

	main := "package main\n\nimport (\n\t\"fmt\"\n\t\"strings\"\n)\n\n" +
		"type childResult struct {\n\tRunID  string\n\tResult string\n\tError  string\n}\n\n" +
		"var _ = strings.Index\nvar _ = fmt.Sprint\n\n" +
		helpers + "\n\nfunc main() {\n" + body + "\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(main), 0o644); err != nil {
		t.Fatalf("write main.go: %v", err)
	}

	run := exec.Command("go", "run", ".")
	run.Dir = dir
	out, err := run.CombinedOutput()
	if err != nil {
		t.Fatalf("running the generated helpers failed: %v\n%s\n\n--- program ---\n%s", err, out, main)
	}
	return string(out)
}

// TestGeneratedParseChildResultArrayKeepsObjectResults is the regression test
// for the empty-result fan-in.
//
// The payload is exactly what engine.freshAwaitAllChildren marshals: an array
// of {run_id, result} where each result is a JSON OBJECT carried as a JSON
// string, so its braces are escaped and sit inside the outer object.
func TestGeneratedParseChildResultArrayKeepsObjectResults(t *testing.T) {
	body := "\t" + `in := "[" +
		"{\"run_id\":\"child-a\",\"result\":\"{\\\"tag\\\":\\\"child-0\\\"}\"}," +
		"{\"run_id\":\"child-b\",\"result\":\"{\\\"tag\\\":\\\"child-1\\\"}\"}," +
		"{\"run_id\":\"child-c\",\"result\":\"{\\\"tag\\\":\\\"child-2\\\"}\"}" +
		"]"
	for _, r := range parseChildResultArray(in) {
		fmt.Printf("%s|%s|%s\n", r.RunID, r.Result, r.Error)
	}`

	got := strings.TrimSpace(runGeneratedHelpers(t, body))
	want := strings.Join([]string{
		`child-a|{"tag":"child-0"}|`,
		`child-b|{"tag":"child-1"}|`,
		`child-c|{"tag":"child-2"}|`,
	}, "\n")

	if got != want {
		t.Errorf("the generated parser lost or mangled child results.\n got:\n%s\nwant:\n%s\n\n"+
			"An empty result field here is the silent failure: AwaitAllChildren returns "+
			"three ChildResults with the right run IDs, no error, and nothing in them.",
			got, want)
	}
}

// TestGeneratedParseChildResultArrayHandlesAwkwardStrings covers the cases a
// brace-and-quote scanner gets wrong for the same underlying reason: it has to
// know when it is inside a string and when a quote is escaped.
func TestGeneratedParseChildResultArrayHandlesAwkwardStrings(t *testing.T) {
	body := "\t" + `cases := []string{
		// an error message containing braces
		"[{\"run_id\":\"a\",\"error\":\"unexpected } in input\"}]",
		// a result whose value contains an escaped quote
		"[{\"run_id\":\"b\",\"result\":\"{\\\"say\\\":\\\"he said \\\\\\\"hi\\\\\\\"\\\"}\"}]",
		// a nested object two levels deep
		"[{\"run_id\":\"c\",\"result\":\"{\\\"a\\\":{\\\"b\\\":1}}\"}]",
		// empty array
		"[]",
	}
	for _, in := range cases {
		rs := parseChildResultArray(in)
		fmt.Printf("n=%d", len(rs))
		for _, r := range rs {
			fmt.Printf(" [%s|%s|%s]", r.RunID, r.Result, r.Error)
		}
		fmt.Println()
	}`

	got := strings.TrimSpace(runGeneratedHelpers(t, body))
	want := strings.Join([]string{
		`n=1 [a||unexpected } in input]`,
		`n=1 [b|{"say":"he said \"hi\""}|]`,
		`n=1 [c|{"a":{"b":1}}|]`,
		`n=0`,
	}, "\n")

	if got != want {
		t.Errorf("the generated parser mishandles strings or nesting.\n got:\n%s\nwant:\n%s", got, want)
	}
}
