package typescript

// The utilities bindings, cleat#3827 group J: `cleat_fetch` and `cleat_json_parse`.
//
// Both are synchronous, and that is half of what these tests hold: `ctx.fetch(...)` must return the answer itself, not
// a promise for it. A promise would make `.status` undefined rather than wrong, which reads as a mistake in the
// workflow rather than in the binding.
//
// The third test is the one that matters most and is easiest to get wrong: a REFUSED request must surface as an error.
// The host reports an egress refusal (and an engine with no Fetcher at all) as a call failure, so an answer carrying a
// status would make "the policy said no" indistinguishable from "the server said 403".

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// scriptedHTTP answers from a map and refuses anything absent from it -- the refusal is the egress case.
type scriptedHTTP struct {
	answers map[string]string
	seen    []string
}

func (s *scriptedHTTP) Fetch(_ context.Context, method, url, headersJSON, body string) (string, error) {
	s.seen = append(s.seen, method+" "+url)
	key := method + " " + url
	if s.answersFor(key) == "" {
		return "", fmt.Errorf("egress policy refused %s", url)
	}
	return s.answersFor(key), nil
}

func (s *scriptedHTTP) answersFor(key string) string {
	if s.answers == nil {
		return ""
	}
	return s.answers[key]
}

const utilitiesEntries = "utilities,utilities_refused,utilities_late,utilities_big,utilities_bad_json,utilities_once"

func buildUtilities(t *testing.T, entries string) []byte {
	t.Helper()
	root := needToolchain(t)
	src := "tests/typescript/testdata/workflows/utilities.ts"
	res := build(t, root, src, entries)
	if res.exit != 0 {
		t.Fatalf("building %s failed (exit %d):\n%s", src, res.exit, res.output)
	}
	wasm, err := os.ReadFile(res.module)
	if err != nil {
		t.Fatal(err)
	}
	return wasm
}

func TestTypeScriptFetchAndJsonParse(t *testing.T) {
	wasm := buildUtilities(t, utilitiesEntries)
	fetcher := &scriptedHTTP{answers: map[string]string{
		"POST https://example.test/a": `{"status":201,"headers":{"x-seen":"1"},"body":"pong"}`,
	}}
	e, closeRT := newEngineWith(t, engine.WithFetcher(fetcher))
	defer closeRT()

	out, _, _, _, _, err := e.Execute(context.Background(), wasm, "utilities", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("running utilities: %v", err)
	}
	var got struct {
		Answer struct {
			Status  int               `json:"status"`
			Headers map[string]string `json:"headers"`
			Body    string            `json:"body"`
		} `json:"answer"`
		Parsed map[string]any `json:"parsed"`
		Keys   []string       `json:"keys"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("the workflow returned %s: %v", out, err)
	}

	// The FETCH was made with the method and URL the workflow chose, which is also what the script is keyed on: a binding
	// that dropped the method would ask for "GET https://example.test/a" and be refused.
	if len(fetcher.seen) != 1 || fetcher.seen[0] != "POST https://example.test/a" {
		t.Fatalf("requests = %v, want the one scripted request", fetcher.seen)
	}
	if got.Answer.Status != 201 || got.Answer.Headers["x-seen"] != "1" || got.Answer.Body != "pong" {
		t.Fatalf("answer = %+v, want the scripted response", got.Answer)
	}

	// jsonParse returns the host's canonical form, so the workflow's own JSON.stringify is what lands here -- but the
	// parse had to succeed for it to be an object at all rather than a string.
	if got.Parsed["a"] != float64(1) || got.Parsed["b"] != float64(2) {
		t.Fatalf("parsed = %v, want both keys", got.Parsed)
	}
	if len(got.Keys) != 2 || got.Keys[0] != "a" || got.Keys[1] != "b" {
		t.Fatalf("keys = %v, want [a b]: the input lists b first, so this order can only come from the host's canonical form",
			got.Keys)
	}
}

// A refused request throws, with the host's message. The workflow records the message, so an accepted-but-empty answer
// would leave it empty and a status would show up as a number.
func TestTypeScriptARefusedFetchThrows(t *testing.T) {
	wasm := buildUtilities(t, utilitiesEntries)
	fetcher := &scriptedHTTP{answers: map[string]string{}}
	e, closeRT := newEngineWith(t, engine.WithFetcher(fetcher))
	defer closeRT()

	out, _, _, _, _, err := e.Execute(context.Background(), wasm, "utilities_refused", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("running utilities_refused: %v", err)
	}
	var got struct{ Message string }
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("the workflow returned %s: %v", out, err)
	}
	if !strings.Contains(got.Message, "egress policy refused") {
		t.Fatalf("message = %q, want the fetcher's refusal; a status-shaped answer would leave this empty", got.Message)
	}
}

// A response larger than 255 bytes arrives whole. This is the case the defect mangled in the OTHER direction: under 256 bytes the
// length decoded as zero, and above it as the length divided by 256, so the body came back truncated to a wrong length (or the JSON
// envelope around it failed to parse). The body is 1500 bytes, and the tail is checked as well as the length.
func TestTypeScriptAResponseLargerThan255BytesArrivesWhole(t *testing.T) {
	wasm := buildUtilities(t, utilitiesEntries)
	body := strings.Repeat("0123456789", 149) + "ENDMARKER" // 1499 bytes
	answer, _ := json.Marshal(map[string]any{"status": 200, "headers": map[string]string{}, "body": body})
	fetcher := &scriptedHTTP{answers: map[string]string{"GET https://example.test/big": string(answer)}}
	e, closeRT := newEngineWith(t, engine.WithFetcher(fetcher))
	defer closeRT()
	out, _, _, _, _, err := e.Execute(context.Background(), wasm, "utilities_big", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("running utilities_big: %v", err)
	}
	var got struct {
		Length int    `json:"length"`
		Tail   string `json:"tail"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("the workflow returned %s: %v", out, err)
	}
	if got.Length != len(body) || got.Tail != "ENDMARKER"[1:] {
		t.Fatalf("length=%d tail=%q, want %d and %q: the body was truncated on the way in", got.Length, got.Tail, len(body), "NDMARKER")
	}
}

func TestTypeScriptJsonParseRefusesTextThatIsNotJson(t *testing.T) {
	wasm := buildUtilities(t, utilitiesEntries)
	e, closeRT := newEngineWith(t)
	defer closeRT()
	out, _, _, _, _, err := e.Execute(context.Background(), wasm, "utilities_bad_json", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("running utilities_bad_json: %v", err)
	}
	if !strings.Contains(out, "cleat_json_parse refused the input as JSON") || !strings.Contains(out, "{not json") {
		t.Fatalf("result %s, want a refusal naming the input; an empty message would mean the text was accepted", out)
	}
}

// A recorded fetch is returned on replay and the fetcher is not asked again: the engine matches the request against the history.
func TestTypeScriptAFetchIsNotRepeatedOnReplay(t *testing.T) {
	wasm := buildUtilities(t, utilitiesEntries)
	fetcher := &scriptedHTTP{answers: map[string]string{"GET https://example.test/once": `{"status":202,"headers":{},"body":"first"}`}}
	e, closeRT := newEngineWith(t, engine.WithFetcher(fetcher))
	defer closeRT()
	out, history, _, _, _, err := e.Execute(context.Background(), wasm, "utilities_once", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(fetcher.seen) != 1 {
		t.Fatalf("the first run made %d requests, want 1", len(fetcher.seen))
	}
	// The remote answers differently now. A replay that asked again would return "second".
	fetcher.answers["GET https://example.test/once"] = `{"status":500,"headers":{},"body":"second"}`
	out2, _, _, _, _, err := e.Replay(context.Background(), wasm, "utilities_once", json.RawMessage(`{}`), history)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(fetcher.seen) != 1 {
		t.Errorf("replay asked the fetcher again (%v); a recorded fetch is returned from the history", fetcher.seen)
	}
	if out2 != out {
		t.Errorf("replay returned %s, the first run %s", out2, out)
	}
}

// What `guarded` on cleat_fetch buys: after the call that suspended the segment, no event is recorded and the fetcher is not
// asked. Without it, an outbound HTTP request would go out for a run that is parked.
func TestTypeScriptAFetchAfterASuspensionRecordsNothing(t *testing.T) {
	wasm := buildUtilities(t, utilitiesEntries)
	fetcher := &scriptedHTTP{answers: map[string]string{"GET https://example.test/late": `{"status":200,"headers":{},"body":"x"}`}}
	e, closeRT := newEngineWith(t, engine.WithFetcher(fetcher))
	defer closeRT()
	_, history, susp, _, _, err := e.Execute(context.Background(), wasm, "utilities_late", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if susp == nil || !strings.Contains(susp.Reason, "cleat_sleep(60000ms)") {
		t.Fatalf("the workflow did not suspend at the sleep (%v); the ordering below is then unmeasured", susp)
	}
	if len(history) != 0 {
		var types []string
		for _, ev := range history {
			types = append(types, string(ev.EventType))
		}
		t.Fatalf("the segment recorded %v AFTER the call that suspended it; cleat_fetch must be guarded", types)
	}
	if len(fetcher.seen) != 0 {
		t.Fatalf("a fetch made after the suspension reached the fetcher: %v", fetcher.seen)
	}
}
