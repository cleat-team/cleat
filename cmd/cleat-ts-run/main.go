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

	"github.com/cleat-team/cleat/engine"
)

// stubbedCaller answers every durable call with an empty JSON object. A workflow under test that needs a scripted answer
// needs more than this, and that is the next increment: naming it here so a green run is not read as "calls were tested".
type stubbedCaller struct{}

func (stubbedCaller) Call(context.Context, string, string, string) (string, error) { return `{}`, nil }

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
}

func unmeasured(format string, args ...any) {
	e := envelope{Events: []eventOut{}, Error: fmt.Sprintf(format, args...)}
	b, _ := json.Marshal(e)
	fmt.Println(string(b))
	fmt.Fprintln(os.Stderr, "UNMEASURED: "+e.Error)
	fmt.Fprintln(os.Stderr, "  (this is a failure of the harness, not a finding about the workflow)")
	os.Exit(2)
}

func main() {
	module := flag.String("module", "", "path to a .wasm built by packages/cleat-ts/build.sh")
	entry := flag.String("entry", "", "the ABI entry point to run (snake_case, as build.sh named it)")
	input := flag.String("input", "{}", "the workflow input, as JSON")
	workflowID := flag.String("workflow-id", "00000000-0000-0000-0000-0000000c1ea7", "workflow id (the engine derives its random stream from it)")
	flag.Parse()

	if *module == "" || *entry == "" {
		unmeasured("usage: cleat-ts-run -module <file.wasm> -entry <name> [-input <json>] [-workflow-id <id>]")
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
	e := engine.NewEngine(rt, stubbedCaller{},
		engine.WithWorkflowID(*workflowID),
		engine.WithDefName(*entry),
		engine.WithDefVersion(1),
		engine.WithBackends(engine.WasmtimeLanguages, wt),
	)

	result, history, susp, _, queryState, err := e.Execute(ctx, wasm, *entry, json.RawMessage(*input))
	out := envelope{Entry: *entry, QueryState: queryState, Events: make([]eventOut, 0, len(history))}
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
