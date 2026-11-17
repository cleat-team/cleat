// cleat-ts-run executes a BUILT TypeScript workflow module in the engine and prints what happened (cleat#3828).
//
// What it is FOR. Go workflows have cleat/cleattest, which is a Go API. A TypeScript author needs the same thing from
// their side of the boundary, and the issue sets the constraint that decides this program's whole shape: the harness
// must run the module that gets DEPLOYED, not a reimplementation of the host calls. So this takes a .wasm built by
// packages/cleat-ts/build.sh and runs it in the same engine a worker runs, and packages/cleat-ts/testing.ts is a thin
// wrapper that shells out to it.
//
// It is one binary per purpose, like cleat-ts-check and cleat-ts-meta beside it: check grades SOURCE CONFORMANCE, this
// grades MODULE BEHAVIOUR. Merging them would make one binary answer two questions, and the exit status below would then
// mean two different things depending on which mode the caller invoked.
//
// Three outcomes, one exit status each, for the same reason cleat-ts-check has three:
//
//	0  ran        stdout is the JSON envelope; the workflow returned a result or suspended
//	1  failed     stdout is the JSON envelope with "error"; the workflow itself failed
//	2  UNMEASURED the module could not be read, the backend is unavailable, or the flags are wrong -- a failure of the
//	              harness, not a finding about the workflow
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/cleat-team/cleat/engine"
)

// stubbedCaller answers every durable call with an empty JSON object.
//
// It is what a run gets when the test supplied no -answers file. A workflow that depends on a call's RESULT cannot be
// tested this way, and gets `{}` silently -- which is why the envelope carries stubbedCalls, so a green run is not read
// as "the calls were tested" (cleat#3828).
type stubbedCaller struct{}

func (stubbedCaller) Call(context.Context, string, string, string) (string, error) { return `{}`, nil }

// scriptedCaller answers durable calls from a test-supplied script (cleat#3828).
//
// AN UNSCRIPTED CALL IS AN ERROR, NOT `{}`. The point of the script is that the test said what each call returns; a
// call it forgot would otherwise get an empty object and the workflow could pass on an answer nobody chose. The error
// travels through the engine as a workflow failure, and the envelope's error names the key to add.
type scriptedCaller struct {
	answers map[string]string
	asked   []string
}

func answerKey(service, operation string) string { return service + "/" + operation }

func (c *scriptedCaller) Call(_ context.Context, service, operation, _ string) (string, error) {
	key := answerKey(service, operation)
	out, ok := c.answers[key]
	if !ok {
		return "", fmt.Errorf("the test scripted no answer for %q: add it to the answers file "+
			"(this is the test's setup, not a workflow failure)", key)
	}
	c.asked = append(c.asked, key)
	return out, nil
}

// loadAnswers reads the -answers file: a JSON object whose keys are "service/operation" and whose values are the JSON
// text the call returns.
//
// Each value is validated as JSON here rather than at the call site, because the engine hands the string straight to
// the guest: `{not json}` would surface as a guest-side parse error inside the workflow, which reads like a workflow
// bug rather than a malformed fixture.
func loadAnswers(path string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading the answers file: %w", err)
	}
	var answers map[string]string
	if err := json.Unmarshal(b, &answers); err != nil {
		return nil, fmt.Errorf("the answers file is not a JSON object of strings: %w", err)
	}
	for key, value := range answers {
		if !json.Valid([]byte(value)) {
			return nil, fmt.Errorf("the answer for %q is not valid JSON: %s", key, value)
		}
		if !strings.Contains(key, "/") {
			return nil, fmt.Errorf("the answer key %q must be \"<service>/<operation>\"", key)
		}
	}
	return answers, nil
}

type eventOut struct {
	Step     int    `json:"step"`
	Type     string `json:"type"`
	Service  string `json:"service,omitempty"`
	Op       string `json:"operation,omitempty"`
	StateKey string `json:"stateKey,omitempty"`
	Error    string `json:"error,omitempty"`
}

type envelope struct {
	OK            bool              `json:"ok"`
	Entry         string            `json:"entry"`
	Result        string            `json:"result,omitempty"`
	Suspended     bool              `json:"suspended"`
	SuspendReason string            `json:"suspendReason,omitempty"`
	QueryState    map[string]string `json:"queryState,omitempty"`
	Events        []eventOut        `json:"events"`
	Error         string            `json:"error,omitempty"`
	// StubbedCalls is true when the run had NO -answers file, so every durable call returned `{}` unread. It is in
	// the envelope so a test (and a reader) can tell "the workflow used a call and nobody scripted it" from "the
	// workflow was actually exercised against answers" (cleat#3828).
	StubbedCalls bool `json:"stubbedCalls,omitempty"`
	// Promises and Signals are what the STORES saw, not what the workflow returned: the promise names it created, in
	// order, and the signal names it consumed. A test that asserted only on the workflow's result could pass without
	// the store ever being touched -- the same reason StubbedCalls exists (cleat#3828).
	Promises []string `json:"promises,omitempty"`
	Signals  []string `json:"signalsConsumed,omitempty"`
	// Children is the child workflow names this run started, in order, from the store.
	Children []string `json:"childrenStarted,omitempty"`
	// Extras is the generic place a REGISTERED store reports what it saw, keyed by the store. It exists so a
	// new store needs no change to this struct: stores_<group>.go names its own key (cleat#3827 F1).
	Extras map[string]any `json:"extras,omitempty"`
}

func unmeasured(format string, args ...any) {
	e := envelope{Events: []eventOut{}, Error: fmt.Sprintf(format, args...)}
	b, _ := json.Marshal(e)
	fmt.Println(string(b))
	fmt.Fprintln(os.Stderr, "UNMEASURED: "+e.Error)
	fmt.Fprintln(os.Stderr, "  (this is a failure of the harness, not a finding about the workflow)")
	os.Exit(2)
}

// buildStores turns the three fixture flags into the stores the engine is given (cleat#3828).
//
// EXTRACTED SO THE WIRING IS TESTABLE, which a review note earned: the loaders have their own tests, but nothing
// asserted that `-promise-rejects` reaches loadMessageMap rather than loadStringMap. Putting the original wrong loader
// back left `go test ./cmd/cleat-ts-run/` green, because every test called the loaders directly. A wiring test is the
// only thing that can fail on that.
//
// The stores are ALWAYS returned, even when nothing was scripted: leaving them nil would make a workflow that touches a
// promise fail with errNoPromiseStore rather than suspend, so a test could not tell "this workflow uses no store" from
// "I forgot to script it" -- and the second is the mistake a harness should make loud.
func buildStores(promisesPath, rejectsPath, signalsPath, childrenPath string) (*memoryPromises, *memorySignals, *memoryChildren, error) {
	var resolves, rejects map[string]string
	var payloads map[string][]string
	var outcomes map[string]engine.ChildOutcome
	var err error
	if promisesPath != "" {
		if resolves, err = loadStringMap(promisesPath, "promises"); err != nil {
			return nil, nil, nil, err
		}
	}
	if rejectsPath != "" {
		// loadMessageMap, NOT loadStringMap: a rejection is an error message, not JSON the guest parses.
		if rejects, err = loadMessageMap(rejectsPath, "promise-rejects"); err != nil {
			return nil, nil, nil, err
		}
	}
	if signalsPath != "" {
		if payloads, err = loadSignalScript(signalsPath); err != nil {
			return nil, nil, nil, err
		}
	}
	if childrenPath != "" {
		if outcomes, err = loadChildScript(childrenPath); err != nil {
			return nil, nil, nil, err
		}
	}
	return newMemoryPromises(resolves, rejects), newMemorySignals(payloads), newMemoryChildren(outcomes), nil
}

func main() {
	module := flag.String("module", "", "path to a .wasm built by packages/cleat-ts/build.sh")
	entry := flag.String("entry", "", "the ABI entry point to run (snake_case, as build.sh named it)")
	input := flag.String("input", "{}", "the workflow input, as JSON")
	workflowID := flag.String("workflow-id", "00000000-0000-0000-0000-0000000c1ea7", "workflow id (the engine derives its random stream from it)")
	answersPath := flag.String("answers", "", "optional JSON file: {\"<service>/<operation>\": <JSON text the call returns>}; without it every durable call returns {} unread")
	promisesPath := flag.String("promises", "", "optional JSON file: {\"<promise name>\": <JSON text it resolves with>}; a promise created under another name stays pending and the workflow suspends")
	promiseRejectsPath := flag.String("promise-rejects", "", "optional JSON file: {\"<promise name>\": <rejection message>}")
	signalsPath := flag.String("signals", "", "optional JSON file: {\"<signal name>\": [<payload JSON>, ...]}, delivered before the run starts")
	childrenPath := flag.String("children", "", "optional JSON file: {\"<child name>\": {\"result\": <any>}} or {\"<child name>\": {\"error\": \"...\"}}; an unscripted child stays running and the parent suspends")
	// Repeatable: -script updates=a.json -script locks=b.json. Each name comes from a store registered by
	// an init() in stores_<group>.go, so a new store adds a file and does not touch this one (cleat#3827 F1).
	var scripts scriptFlag
	flag.Var(&scripts, "script", "repeatable <name>=<file>: configure a registered store from a fixture")
	flag.Parse()

	if *module == "" || *entry == "" {
		unmeasured("usage: cleat-ts-run -module <file.wasm> -entry <name> [-input <json>] [-workflow-id <id>] " +
			"[-answers <file.json>] [-promises <file.json>] [-promise-rejects <file.json>] [-signals <file.json>] " +
			"[-children <file.json>]")
	}
	if !json.Valid([]byte(*input)) {
		unmeasured("-input is not valid JSON: %s", *input)
	}
	wasm, err := os.ReadFile(*module)
	if err != nil {
		unmeasured("reading the module: %v", err)
	}

	ctx := context.Background()
	rt, err := engine.NewRuntime(ctx, 0, 0)
	if err != nil {
		unmeasured("engine.NewRuntime: %v", err)
	}
	defer func() { _ = rt.Close(ctx) }()
	wt, err := engine.NewWasmtimeBackend(ctx)
	if err != nil {
		unmeasured("engine.NewWasmtimeBackend (is this built with CGO?): %v", err)
	}
	var caller engine.ServiceCaller
	if *answersPath != "" {
		answers, err := loadAnswers(*answersPath)
		if err != nil {
			unmeasured("%v", err)
		}
		caller = &scriptedCaller{answers: answers}
	} else {
		caller = stubbedCaller{}
	}
	promises, signals, children, err := buildStores(*promisesPath, *promiseRejectsPath, *signalsPath, *childrenPath)
	if err != nil {
		unmeasured("%v", err)
	}
	baseOpts := []engine.EngineOption{
		engine.WithWorkflowID(*workflowID),
		engine.WithDefName(*entry),
		engine.WithDefVersion(1),
		engine.WithBackends(engine.WasmtimeLanguages, wt),
		engine.WithPromiseStore(promises),
		engine.WithSignalStore(signals),
		engine.WithChildWorkflowStore(children),
	}
	// Resolved BEFORE the engine is built, and appended to its options rather than applied to it after:
	// an EngineOption applied to a live engine is a mutation with an ordering question, and this change is
	// not the place to introduce one.
	scriptOpts, extras, err := resolveScripts(scripts.pairs)
	if err != nil {
		unmeasured("%v", err)
	}
	e := engine.NewEngine(rt, caller, append(baseOpts, scriptOpts...)...)

	result, history, susp, _, queryState, err := e.Execute(ctx, wasm, *entry, json.RawMessage(*input))
	out := envelope{
		Entry: *entry, QueryState: queryState, Events: make([]eventOut, 0, len(history)),
		StubbedCalls: *answersPath == "",
		Promises:     promises.nameList(),
		Signals:      signals.consumedList(),
		Children:     children.startedList(),
		Extras:       extras,
	}
	for _, ev := range history {
		out.Events = append(out.Events, eventOut{
			Step: ev.Step, Type: string(ev.EventType),
			Service: ev.Service, Op: ev.Op, StateKey: ev.StateKey, Error: ev.Err,
		})
	}
	switch {
	case err != nil:
		out.OK = false
		out.Error = err.Error()
		emit(out)
		os.Exit(1)
	case susp != nil:
		out.OK = true
		out.Suspended = true
		out.SuspendReason = susp.Reason
		emit(out)
	default:
		out.OK = true
		out.Result = result
		emit(out)
	}
}

func emit(e envelope) {
	b, err := json.Marshal(e)
	if err != nil {
		unmeasured("encoding the result: %v", err)
	}
	fmt.Println(string(b))
}
