package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// writePluginFixture writes a script through the same loader the harness uses, so a test cannot pass on a fixture the
// harness would reject.
func writePluginFixture(t *testing.T, body string) string {
	t.Helper()
	return writeFixture(t, t.TempDir(), "plugins.json", body)
}

// pluginStoreFor builds the store and hands back the registries its option installs. Reaching them directly is the
// cheapest way to ask whether a scripted function answers; a full wasm run asks the same question through four more
// layers.
func pluginStoreFor(t *testing.T, path string) (*scriptedPlugins, error) {
	t.Helper()
	scripts, err := loadPluginScript(path)
	if err != nil {
		return nil, err
	}
	p := &scriptedPlugins{scripts: scripts}
	if _, err := p.install(); err != nil {
		return nil, err
	}
	return p, nil
}

// The store's whole job: a scripted function ANSWERS from the script, and the call is RECORDED. Both halves matter --
// a registry that answers but records nothing leaves a test unable to tell a call that reached it from one the
// workflow never made.
func TestScriptedPluginAnswersAndRecordsTheCall(t *testing.T) {
	path := writePluginFixture(t, `{"demo/echo": {"result": {"said": "hello"}}}`)
	sps, err := pluginStoreFor(t, path)
	if err != nil {
		t.Fatal(err)
	}
	fn, _, _, ok := sps.pr.Lookup("demo", "echo")
	if !ok {
		t.Fatal("demo/echo was not registered; the fixture named it")
	}
	out, err := fn(context.Background(), `{"in": 1}`)
	if err != nil {
		t.Fatalf("scripted call failed: %v", err)
	}
	// Compared as JSON, not as text: the script is a json.RawMessage, so the fixture's own spacing is preserved and a
	// byte comparison would be asserting on the fixture rather than on the answer.
	var got map[string]string
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("out = %q, which is not JSON: %v", out, err)
	}
	if got["said"] != "hello" {
		t.Fatalf("out = %q, want the fixture's result", out)
	}

	// The call the test just made must be IN the list the Extra reports. Asserting on a fresh instance instead would
	// pass whether or not install and Extra share state -- the wiring this file exists to check.
	spec, err := newScriptedPlugins(path)
	if err != nil {
		t.Fatal(err)
	}
	k, v := spec.Extra()
	if k != "pluginCalls" {
		t.Fatalf("extras key = %q, want pluginCalls", k)
	}
	if got, _ := v.([]string); len(got) != 0 {
		t.Fatalf("pluginCalls = %v, want empty on a NEW instance: that one served no call", v)
	}
	if got := sps.callList(); len(got) != 1 || got[0] != "demo/echo" {
		t.Fatalf("pluginCalls = %v, want the call this test made", got)
	}
}

// A scripted ERROR reaches the guest as an error, and the call is still recorded -- a failed call is one the registry
// received, and hiding it would make "reached the host and failed" indistinguishable from "never called".
func TestAScriptedErrorIsAnErrorAndStillRecorded(t *testing.T) {
	path := writePluginFixture(t, `{"demo/fail": {"error": "plugin exploded"}}`)
	sps, err := pluginStoreFor(t, path)
	if err != nil {
		t.Fatal(err)
	}
	fn, _, _, ok := sps.pr.Lookup("demo", "fail")
	if !ok {
		t.Fatal("demo/fail was not registered")
	}
	if _, err := fn(context.Background(), `{}`); err == nil || !strings.Contains(err.Error(), "plugin exploded") {
		t.Fatalf("err = %v, want the scripted message", err)
	}
	if got := sps.callList(); len(got) != 1 || got[0] != "demo/fail" {
		t.Fatalf("pluginCalls = %v, want the failed call recorded", got)
	}
}

// An UNSCRIPTED function must be a miss. Otherwise nothing would show the registry is consulted at all -- the defect
// this guards is a store that answers whatever it is asked.
func TestAnUnscriptedPluginFunctionIsAMiss(t *testing.T) {
	path := writePluginFixture(t, `{"demo/echo": {"result": 1}}`)
	sps, err := pluginStoreFor(t, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, ok := sps.pr.Lookup("demo", "not-scripted"); ok {
		t.Fatal("an unscripted function was registered")
	}
	if _, _, _, ok := sps.pr.Lookup("other", "echo"); ok {
		t.Fatal("a function was reachable under a plugin name the fixture never named")
	}
}

// A script that answers two ways is refused AT LOAD, because the fixture would otherwise silently pick one and a test
// asserting the other would fail for a reason the fixture hid.
func TestAPluginScriptThatAnswersTwoWaysIsRefused(t *testing.T) {
	for name, body := range map[string]string{
		"result and error":  `{"demo/f": {"result": 1, "error": "boom"}}`,
		"result and chunks": `{"demo/f": {"result": 1, "chunks": ["a"]}}`,
		"nothing at all":    `{"demo/f": {}}`,
		"empty script":      `{}`,
		"key without slash": `{"demo": {"result": 1}}`,
	} {
		if _, err := newScriptedPlugins(writePluginFixture(t, body)); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// A chunk-list script goes on the STREAM registry only. Registering it on the plain one as well would answer
// plugin_call for a function whose scripted answer is a stream, which no fixture asked for.
func TestAChunkScriptRegistersOnlyOnTheStreamRegistry(t *testing.T) {
	path := writePluginFixture(t, `{"demo/stream": {"chunks": ["a", "b"]}}`)
	sps, err := pluginStoreFor(t, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, ok := sps.pr.Lookup("demo", "stream"); ok {
		t.Fatal("a chunked function was registered on the plain registry")
	}
	fn, _, ok := sps.psr.Lookup("demo", "stream")
	if !ok {
		t.Fatal("a chunked function was not registered on the stream registry")
	}
	ch, err := fn(context.Background(), `{}`)
	if err != nil {
		t.Fatalf("streamed call failed: %v", err)
	}
	var got []string
	for ev := range ch {
		got = append(got, ev.Content)
	}
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("chunks = %v, want the fixture's two", got)
	}
}
