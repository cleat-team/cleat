//go:build cgo

package csharp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/plugin"
)

// Group H of cleat#4080's host-call coverage: plugin-call and plugin-call-streaming, driven through the real engine and wasmtime backend by
// named entries of the conformance component (packages/cleat-csharp/conformance/GroupH.cs). Nothing here claims C# is supported.

type pluginSeen struct {
	mu     sync.Mutex
	inputs []string
}

func (p *pluginSeen) add(in string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.inputs = append(p.inputs, in)
}

func (p *pluginSeen) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.inputs)
}

func registryWith(t *testing.T, seen *pluginSeen, fn plugin.PluginFunc) []engine.EngineOption {
	t.Helper()
	pr := engine.NewPluginRegistry()
	if err := pr.Register("echo-plugin", "echo", func(ctx context.Context, in string) (string, error) {
		seen.add(in)
		return fn(ctx, in)
	}); err != nil {
		t.Fatal(err)
	}
	return []engine.EngineOption{engine.WithPluginRegistry(pr)}
}

func streamRegistry(t *testing.T, fn plugin.PluginStreamFunc) []engine.EngineOption {
	t.Helper()
	psr := engine.NewPluginStreamRegistry()
	if err := psr.Register("echo-plugin", "tokens", fn); err != nil {
		t.Fatal(err)
	}
	return []engine.EngineOption{engine.WithPluginStreamRegistry(psr)}
}

func emit(events ...plugin.StreamEvent) plugin.PluginStreamFunc {
	return func(context.Context, string) (<-chan plugin.StreamEvent, error) {
		ch := make(chan plugin.StreamEvent, len(events))
		for _, e := range events {
			ch <- e
		}
		close(ch)
		return ch, nil
	}
}

const callInput = `{"plugin":"echo-plugin","function":"echo","input":"{\"n\":1}"}`

func TestCSharpGroupHPluginCallPassesTheInputAndReturnsTheReply(t *testing.T) {
	seen := &pluginSeen{}
	opts := registryWith(t, seen, func(_ context.Context, in string) (string, error) { return `{"echoed":` + in + `}`, nil })
	m := mustFinish(t, runConformanceEntry(t, "h_plugin_call", entryRun{Input: callInput, Options: opts}))
	if m["reply"] != `{"echoed":{"n":1}}` {
		t.Errorf("reply = %v, want the plugin's response", m["reply"])
	}
	if len(seen.inputs) != 1 || seen.inputs[0] != `{"n":1}` {
		t.Errorf("the plugin saw %v, want the input unchanged", seen.inputs)
	}
}

// The host classifies a plugin failure, and here (unlike the simple-packed calls) the code is meaningful: a plugin that is not registered and a
// plugin that ran and errored are reported as the class the host chose. Asserted as the exception family and operation; the exact class is the
// host's table (callFailureCode) and is read, not assumed.
func TestCSharpGroupHPluginCallAnUnregisteredFunctionIsAnErrorForThatPlugin(t *testing.T) {
	m := mustFinish(t, runConformanceEntry(t, "h_plugin_call", entryRun{Input: `{"plugin":"nope","function":"echo","input":"{}"}`,
		Options: registryWith(t, &pluginSeen{}, nil)}))
	if m["failed"] != true || m["service"] != "plugin:nope" || m["operation"] != "echo" || !strings.Contains(fmt.Sprint(m["message"]), "not registered") {
		t.Errorf("got %v, want a failure for plugin:nope/echo saying the function is not registered", m)
	}
}

func TestCSharpGroupHPluginCallAPluginErrorIsAnErrorCarryingItsText(t *testing.T) {
	opts := registryWith(t, &pluginSeen{}, func(context.Context, string) (string, error) { return "", errors.New("the quota service is down") })
	m := mustFinish(t, runConformanceEntry(t, "h_plugin_call", entryRun{Input: callInput, Options: opts}))
	if m["failed"] != true || m["service"] != "plugin:echo-plugin" || !strings.Contains(fmt.Sprint(m["message"]), "the quota service is down") {
		t.Errorf("got %v, want an error for plugin:echo-plugin carrying the plugin's text", m)
	}
}

// A name the host cannot accept is refused before the registry is reached.
func TestCSharpGroupHPluginCallAnInvalidNameNeverReachesThePlugin(t *testing.T) {
	seen := &pluginSeen{}
	opts := registryWith(t, seen, func(context.Context, string) (string, error) { return `{}`, nil })
	m := mustFinish(t, runConformanceEntry(t, "h_plugin_call", entryRun{Input: `{"plugin":"echo plugin","function":"echo","input":"{}"}`, Options: opts}))
	if m["failed"] != true {
		t.Errorf("got %v, want the invalid name refused", m)
	}
	if seen.count() != 0 {
		t.Errorf("the plugin ran %d time(s) for a refused name", seen.count())
	}
}

// Replay answers from the record. A plugin registered with the default policy does not run again on replay, and the replay's plugin answers
// differently to prove it: the recorded reply must win.
func TestCSharpGroupHPluginCallReplayReturnsTheRecordedReply(t *testing.T) {
	fresh := runConformanceEntry(t, "h_plugin_call", entryRun{WorkflowID: "wf-h-1", Input: callInput,
		Options: registryWith(t, &pluginSeen{}, func(context.Context, string) (string, error) { return `{"v":"recorded"}`, nil })})
	if mustFinish(t, fresh)["reply"] != `{"v":"recorded"}` {
		t.Fatal("the fresh run did not return the reply")
	}
	seen := &pluginSeen{}
	replay := runConformanceEntry(t, "h_plugin_call", entryRun{WorkflowID: "wf-h-1", Input: callInput, Replay: fresh.History,
		Options: registryWith(t, seen, func(context.Context, string) (string, error) { return `{"v":"LIVE"}`, nil })})
	if mustFinish(t, replay)["reply"] != `{"v":"recorded"}` {
		t.Errorf("the replay returned %v, want the recorded reply", replay.Result)
	}
	if seen.count() != 0 {
		t.Errorf("the replay ran the plugin %d time(s), want none", seen.count())
	}
}

// The typed overload: a record goes out as JSON through the source-generated context and the reply comes back as a record.
func TestCSharpGroupHPluginCallTypedRoundTripsARecord(t *testing.T) {
	seen := &pluginSeen{}
	opts := registryWith(t, seen, func(_ context.Context, in string) (string, error) { return in, nil })
	m := mustFinish(t, runConformanceEntry(t, "h_plugin_call_typed", entryRun{
		Input: `{"plugin":"echo-plugin","function":"echo","text":"hi \"there\"","times":3}`, Options: opts}))
	if m["text"] != `hi "there"` || m["times"] != float64(3) {
		t.Errorf("got %v, want the record back", m)
	}
	// Whatever escaping the caller's own JsonSerializerContext chose (the default writes the quote as \u0022), it must be JSON that decodes to the record.
	var sent map[string]any
	if len(seen.inputs) != 1 || json.Unmarshal([]byte(seen.inputs[0]), &sent) != nil || sent["Text"] != `hi "there"` || sent["Times"] != float64(3) {
		t.Errorf("the plugin saw %v, want JSON that decodes to the record", seen.inputs)
	}
}

// A reply that is not the response type is an error for that plugin, not a crash and not a default value.
func TestCSharpGroupHPluginCallTypedAReplyOfTheWrongShapeIsAnError(t *testing.T) {
	opts := registryWith(t, &pluginSeen{}, func(context.Context, string) (string, error) { return `{"Text":5}`, nil })
	m := mustFinish(t, runConformanceEntry(t, "h_plugin_call_typed", entryRun{
		Input: `{"plugin":"echo-plugin","function":"echo","text":"x","times":1}`, Options: opts}))
	if m["failed"] != true || m["code"] != float64(0) || !strings.Contains(fmt.Sprint(m["message"]), "not a PluginEcho") {
		t.Errorf("got %v, want an error saying the reply is not a PluginEcho", m)
	}
}

func TestCSharpGroupHPluginStreamReturnsTheChunksInOrder(t *testing.T) {
	opts := streamRegistry(t, emit(plugin.StreamEvent{Index: 0, Content: "he"}, plugin.StreamEvent{Index: 1, Content: "llo"}, plugin.StreamEvent{Index: 2, Content: "!", Finish: true}))
	m := mustFinish(t, runConformanceEntry(t, "h_plugin_stream", entryRun{Input: `{"plugin":"echo-plugin","function":"tokens","input":"{}"}`, Options: opts}))
	want := `[map[c:he f:false i:0] map[c:llo f:false i:1] map[c:! f:true i:2]]`
	if got := fmt.Sprint(m["chunks"]); got != want {
		t.Errorf("chunks = %s, want %s", got, want)
	}
}

// A stream that produced nothing is an empty list (the host answers null).
func TestCSharpGroupHPluginStreamWithNoChunksIsAnEmptyList(t *testing.T) {
	m := mustFinish(t, runConformanceEntry(t, "h_plugin_stream", entryRun{Input: `{"plugin":"echo-plugin","function":"tokens","input":"{}"}`, Options: streamRegistry(t, emit())}))
	if got, ok := m["chunks"].([]any); !ok || len(got) != 0 {
		t.Errorf("chunks = %v, want an empty list", m["chunks"])
	}
}

func TestCSharpGroupHPluginStreamAnUnregisteredFunctionIsAnError(t *testing.T) {
	m := mustFinish(t, runConformanceEntry(t, "h_plugin_stream", entryRun{Input: `{"plugin":"echo-plugin","function":"missing","input":"{}"}`, Options: streamRegistry(t, emit())}))
	if m["failed"] != true || m["service"] != "plugin:echo-plugin" || !strings.Contains(fmt.Sprint(m["message"]), "not registered") {
		t.Errorf("got %v, want a failure saying the stream function is not registered", m)
	}
}

func TestCSharpGroupHPluginStreamReplayReturnsTheRecordedChunks(t *testing.T) {
	in := `{"plugin":"echo-plugin","function":"tokens","input":"{}"}`
	fresh := runConformanceEntry(t, "h_plugin_stream", entryRun{WorkflowID: "wf-h-2", Input: in,
		Options: streamRegistry(t, emit(plugin.StreamEvent{Index: 0, Content: "a"}, plugin.StreamEvent{Index: 1, Content: "b", Finish: true}))})
	mustFinish(t, fresh)
	replay := runConformanceEntry(t, "h_plugin_stream", entryRun{WorkflowID: "wf-h-2", Input: in, Replay: fresh.History,
		Options: streamRegistry(t, emit(plugin.StreamEvent{Index: 0, Content: "LIVE", Finish: true}))})
	if a, b := fmt.Sprint(mustFinish(t, fresh)["chunks"]), fmt.Sprint(mustFinish(t, replay)["chunks"]); a != b {
		t.Errorf("the replay returned %s, want the recorded %s", b, a)
	}
}

// A response type the caller's context does not declare fails BEFORE the plugin runs: a recorded side effect must not be the price of finding out.
func TestCSharpGroupHPluginCallTypedAnUndeclaredResponseTypeFailsBeforeThePluginRuns(t *testing.T) {
	seen := &pluginSeen{}
	opts := registryWith(t, seen, func(context.Context, string) (string, error) { return `"x"`, nil })
	res := runConformanceEntry(t, "h_plugin_call_undeclared", entryRun{Input: `{"plugin":"echo-plugin","function":"echo"}`, Options: opts})
	if res.Err == nil || !strings.Contains(res.Err.Error(), "does not declare [JsonSerializable(typeof(PluginUndeclared))]") {
		t.Errorf("err = %v result = %q, want the run to fail naming the undeclared type", res.Err, res.Result)
	}
	if seen.count() != 0 {
		t.Errorf("the plugin ran %d time(s) before the type was found to be undeclared", seen.count())
	}
}
