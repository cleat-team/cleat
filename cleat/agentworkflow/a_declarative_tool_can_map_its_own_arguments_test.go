package agentworkflow_test

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/cleat/agentworkflow"
	"github.com/cleat-team/cleat/cleat/cleattest"
)

// cleat#3169: KindPlugin and KindApproval otherwise hand the model's own
// tool-call JSON to the plugin VERBATIM. examples/ai-agent-platform's
// save_report needs base64 the model should not have to produce, and
// request_approval needs two fields that never vary and the model should
// not be asked for at all -- both closed here without the model ever
// supplying either.

func TestAnArgTransformBase64EncodesAPlainTextFieldForThePlugin(t *testing.T) {
	env := cleattest.NewTestEnv()
	env.OnPluginCall("blobstore", "put").Return(`{"key":"report","sha256":"ab","size":5}`, nil)
	env.OnPluginCall("llm", "chat").
		Return(calls([]string{"c1"}, []string{"save_report"}, []string{`{"body":"draft report"}`}), nil).
		Return(answer("saved."), nil)

	saveReport := []agentworkflow.Tool{{
		Name: "save_report", Kind: agentworkflow.KindPlugin,
		Plugin: "blobstore", Function: "put",
		ArgTransforms: []agentworkflow.ArgTransform{
			{FromField: "body", ToField: "data", Encoding: "base64"},
		},
		StaticArgs: json.RawMessage(`{"key":"report"}`),
	}}

	res, err := run(t, env, agentworkflow.Input{Message: "write a report", Tools: saveReport, MaxSteps: 2})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Answer != "saved." {
		t.Errorf("Answer = %q, want %q", res.Answer, "saved.")
	}

	// THE PLUGIN RECEIVED base64, NOT THE MODEL'S PLAIN TEXT -- the whole
	// point. A version that forgot the transform would send "draft report"
	// verbatim under "data", which blobstore's own []byte field would then
	// reject or corrupt; this asserts the actual bytes crossing the host
	// call, not just that the run finished.
	var reqSeen string
	for _, rec := range env.CallHistory() {
		if rec.Service == "blobstore" && rec.Operation == "put" {
			reqSeen = rec.Request
		}
	}
	if reqSeen == "" {
		t.Fatal("blobstore.put was never called")
	}
	var req struct {
		Key  string `json:"key"`
		Data string `json:"data"`
	}
	if err := json.Unmarshal([]byte(reqSeen), &req); err != nil {
		t.Fatalf("blobstore.put request is not valid JSON: %v", err)
	}
	if req.Key != "report" {
		t.Errorf("key = %q, want %q (from static_args)", req.Key, "report")
	}
	decoded, err := base64.StdEncoding.DecodeString(req.Data)
	if err != nil {
		t.Fatalf("data = %q is not base64: %v", req.Data, err)
	}
	if string(decoded) != "draft report" {
		t.Errorf("decoded data = %q, want the model's plain-text body %q", decoded, "draft report")
	}
	// And the model itself was never asked for base64 -- it supplied plain
	// text, which is the regression this issue exists to avoid.
	if strings.Contains(reqSeen, "draft report") {
		t.Errorf("blobstore.put request carries the PLAIN TEXT body unencoded:\n%s", reqSeen)
	}
}

func TestStaticArgsOverrideModelSuppliedFieldsOfTheSameName(t *testing.T) {
	env := cleattest.NewTestEnv()
	env.OnPluginCall("approvals", "poll").Return(`{"found":true,"approved":true}`, nil)
	env.OnPluginCall("llm", "chat").
		// The model supplies its OWN event_type, which static_args must win over.
		Return(calls([]string{"c1"}, []string{"request_approval"}, []string{`{"event_type":"whatever-the-model-guessed","reason":"ship it"}`}), nil).
		Return(answer("approved."), nil)

	requestApproval := []agentworkflow.Tool{{
		Name: "request_approval", Kind: agentworkflow.KindApproval,
		Plugin: "approvals", Function: "poll",
		StaticArgs: json.RawMessage(`{"event_type":"agent.approval","timeout_ms":0}`),
	}}

	_, err := run(t, env, agentworkflow.Input{Message: "ask first", Tools: requestApproval, MaxSteps: 2})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	var reqSeen string
	for _, rec := range env.CallHistory() {
		if rec.Service == "approvals" && rec.Operation == "poll" {
			reqSeen = rec.Request
		}
	}
	if reqSeen == "" {
		t.Fatal("approvals.poll was never called")
	}
	var req struct {
		EventType string `json:"event_type"`
		TimeoutMs int    `json:"timeout_ms"`
	}
	if err := json.Unmarshal([]byte(reqSeen), &req); err != nil {
		t.Fatalf("approvals.poll request is not valid JSON: %v", err)
	}
	if req.EventType != "agent.approval" {
		t.Errorf("event_type = %q, want the STATIC value to win over the model's own %q", req.EventType, "whatever-the-model-guessed")
	}
	if req.TimeoutMs != 0 {
		t.Errorf("timeout_ms = %d, want 0", req.TimeoutMs)
	}
}

// A TOOL WITH NEITHER FIELD SET DISPATCHES UNCHANGED -- not even round-tripped
// through JSON. Proven by a payload whose key ORDER would not survive an
// unmarshal/marshal round trip unless Go's map-ordered re-encoding happened
// to preserve it, which it does not: re-encoding `{"z":1,"a":2}` through
// map[string]any yields key order "a","z" (alphabetical), not "z","a".
func TestATooWithNoMappingDispatchesByteForByteUnchanged(t *testing.T) {
	env := cleattest.NewTestEnv()
	const literalArgs = `{"z":1,"a":2}`
	env.OnPluginCall("svc", "do").Return(`{}`, nil)
	env.OnPluginCall("llm", "chat").
		Return(calls([]string{"c1"}, []string{"t"}, []string{literalArgs}), nil).
		Return(answer("done."), nil)

	plain := []agentworkflow.Tool{{Name: "t", Kind: agentworkflow.KindPlugin, Plugin: "svc", Function: "do"}}
	_, err := run(t, env, agentworkflow.Input{Message: "go", Tools: plain, MaxSteps: 2})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	var reqSeen string
	for _, rec := range env.CallHistory() {
		if rec.Service == "svc" && rec.Operation == "do" {
			reqSeen = rec.Request
		}
	}
	if reqSeen != literalArgs {
		t.Errorf("request = %q, want the model's argument string byte-for-byte unchanged: %q", reqSeen, literalArgs)
	}
}

func TestAnArgTransformIsSkippedWhenTheModelDidNotSupplyTheField(t *testing.T) {
	env := cleattest.NewTestEnv()
	env.OnPluginCall("blobstore", "put").Return(`{"key":"report"}`, nil)
	env.OnPluginCall("llm", "chat").
		// No "body" at all -- the model omitted the optional argument.
		Return(calls([]string{"c1"}, []string{"save_report"}, []string{`{}`}), nil).
		Return(answer("saved."), nil)

	saveReport := []agentworkflow.Tool{{
		Name: "save_report", Kind: agentworkflow.KindPlugin,
		Plugin: "blobstore", Function: "put",
		ArgTransforms: []agentworkflow.ArgTransform{
			{FromField: "body", ToField: "data", Encoding: "base64"},
		},
		StaticArgs: json.RawMessage(`{"key":"report"}`),
	}}

	if _, err := run(t, env, agentworkflow.Input{Message: "x", Tools: saveReport, MaxSteps: 2}); err != nil {
		t.Fatalf("a missing optional field must not fail the run: %v", err)
	}

	var reqSeen string
	for _, rec := range env.CallHistory() {
		if rec.Service == "blobstore" && rec.Operation == "put" {
			reqSeen = rec.Request
		}
	}
	if strings.Contains(reqSeen, `"data"`) {
		t.Errorf("request = %q, want no \"data\" field: the transform's source field was never supplied", reqSeen)
	}
	if !strings.Contains(reqSeen, `"key":"report"`) {
		t.Errorf("request = %q, want static_args' key to still be present", reqSeen)
	}
}

// A MAPPING ERROR GOES THROUGH THE SAME ToolErrorMode EVERY OTHER TOOL ERROR
// DOES, deliberately not special-cased: applyArgMapping's error is returned
// from callTool exactly like an unknown tool's or a failing tool's, so it is
// injected back to the model under the default and only ends the run under
// ToolErrorFail. This test uses ToolErrorFail to get a hard failure to
// assert on; TestTheDefaultInjectsTheToolErrorToTheModel already covers the
// inject path for this same callTool error shape.
func TestABase64TransformRefusesANonStringField(t *testing.T) {
	env := cleattest.NewTestEnv()
	env.OnPluginCall("llm", "chat").
		// "body" is a number, not a string -- base64 cannot be applied to it.
		Return(calls([]string{"c1"}, []string{"save_report"}, []string{`{"body":42}`}), nil)

	saveReport := []agentworkflow.Tool{{
		Name: "save_report", Kind: agentworkflow.KindPlugin,
		Plugin: "blobstore", Function: "put",
		ArgTransforms: []agentworkflow.ArgTransform{
			{FromField: "body", ToField: "data", Encoding: "base64"},
		},
	}}

	_, err := run(t, env, agentworkflow.Input{
		Message: "x", Tools: saveReport, MaxSteps: 2, ToolErrorMode: agentworkflow.ToolErrorFail,
	})
	if err == nil {
		t.Fatal("a base64 transform on a non-string field must error, not silently stringify it")
	}
	if !strings.Contains(err.Error(), "save_report") {
		t.Errorf("error = %v, want it to name the tool", err)
	}
}

// ---- declaration-time validation --------------------------------------

func TestAnArgTransformNeedsBothFieldNames(t *testing.T) {
	env := cleattest.NewTestEnv()
	_, err := run(t, env, agentworkflow.Input{
		Message: "x",
		Tools: []agentworkflow.Tool{{
			Name: "t", Kind: agentworkflow.KindPlugin, Plugin: "p", Function: "f",
			ArgTransforms: []agentworkflow.ArgTransform{{FromField: "a"}},
		}},
	})
	if err == nil {
		t.Fatal("an arg_transform with no to_field must be refused at declaration")
	}
	if !strings.Contains(err.Error(), "arg_transform") {
		t.Errorf("error = %v, want it to name what's wrong", err)
	}
}

func TestAnArgTransformRejectsAnUnknownEncoding(t *testing.T) {
	env := cleattest.NewTestEnv()
	_, err := run(t, env, agentworkflow.Input{
		Message: "x",
		Tools: []agentworkflow.Tool{{
			Name: "t", Kind: agentworkflow.KindPlugin, Plugin: "p", Function: "f",
			ArgTransforms: []agentworkflow.ArgTransform{{FromField: "a", ToField: "b", Encoding: "rot13"}},
		}},
	})
	if err == nil {
		t.Fatal("an unrecognised encoding must be refused at declaration, not ignored")
	}
	if !strings.Contains(err.Error(), "rot13") {
		t.Errorf("error = %v, want it to name the encoding it refused", err)
	}
}

func TestStaticArgsMustBeAJSONObject(t *testing.T) {
	env := cleattest.NewTestEnv()
	_, err := run(t, env, agentworkflow.Input{
		Message: "x",
		Tools: []agentworkflow.Tool{{
			Name: "t", Kind: agentworkflow.KindPlugin, Plugin: "p", Function: "f",
			StaticArgs: json.RawMessage(`[1,2,3]`),
		}},
	})
	if err == nil {
		t.Fatal("static_args that is not a JSON object must be refused at declaration")
	}
	if !strings.Contains(err.Error(), "static_args") {
		t.Errorf("error = %v, want it to name the field", err)
	}
}
