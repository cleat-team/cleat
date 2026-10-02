package crash

// cleat#1983's acceptance test: an agent run as the shipped agent WORKFLOW,
// started as a child by a client workflow, crashed mid-loop and resumed.
//
// WHAT IS BEING MEASURED, and why the counts are chosen the way they are. The
// claim is not "the agent completes" -- that is true of an agent that re-runs
// its whole loop after every crash, and it is the claim a single-count test
// would confirm. The claim is that the loop's COMPLETED steps are durable: an
// LLM turn that already happened is not asked again, a service tool that
// already ran is not called again, and a workflow tool whose child already
// finished is not started again.
//
// So the scenario arranges for the crash to land on the LAST LLM turn, after
// both tools have run and completed. Every count is then read three ways:
//
//	                      counts the contract predicts   a loop with no durable
//	                                                     history would give
//	service tool calls               1                          2
//	workflow child runs              1                          2
//	LLM calls, before the crash      3                          3
//	LLM calls, after the crash       1                          3
//
// The last row is the acceptance sentence verbatim -- "resumes without a
// second LLM call for completed turns" -- and the two candidate readings differ
// by three. Any row coming out at 0 means the run did not resume at all, which
// is a different failure and is reported as one.
//
// THE RIGHT-HAND COLUMN IS MEASURED, NOT ARGUED, and measuring it is what
// caught the first version of this test being vacuous. Falsified 2026-10-02 by
// deleting the agent's five event_history rows between the kill and the
// restart, so replay had nothing to consume: the run went red on all three
// rows at 3 / 2 / 2, exactly as tabulated. Deleted rows verified non-zero and
// zero-remaining by the mutation itself, because a falsification that silently
// removes nothing reads as a passing one.
//
// Two things had to be true for that to work, and only the first was obvious.
// The history has to be the thing the replay consumes -- so deleting it makes
// the loop start over. And the STUB must answer the conversation rather than
// the call index: under an index-keyed script a re-run's first call is the
// fourth request, which a script says is the final answer, so the re-run asks
// nothing, calls no tool, and produces counts identical to a correct replay.
// The test passed against its own falsification until that was fixed. See
// llmStub's doc comment.
//
// ASSERTED ON RECORDED EVENTS, NOT ON TIMING, which is the acceptance's own
// wording. Two records are consulted: the parent's event_history, read while
// the workflow is still running (a terminal workflow's history is deleted by
// finalize_workflow_status, so after completion it always reads 0 and says
// nothing), and the two stubs' own request logs. Nothing here sleeps and
// hopes, and nothing reads a duration.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// agentConfigJSON is the agent's configuration, and it is ONE configuration for
// both clients. The acceptance is that a Go workflow and a Python workflow run
// the SAME config; a copy per language would be two configs that agree on the
// day they are written, so the test hands this exact string to each client as
// its workflow input.
//
// It is input rather than code on purpose, which is the design being tested:
// the deployed agent declares no tools, so one deployment serves every
// caller's tool set. If tools were compiled into the agent, this string could
// not exist.
const agentConfigJSON = `{
  "provider": "openai",
  "model": "stub-model",
  "max_steps": 10,
  "message": "How warm is Tokyo?",
  "tools": [
    {
      "name": "lookup_weather",
      "kind": "service",
      "service": "weather",
      "operation": "get",
      "description": "Look up the weather for a city",
      "parameters": {"type": "object", "properties": {"city": {"type": "string"}}}
    },
    {
      "name": "summarise",
      "kind": "workflow",
      "workflow": "summarise",
      "description": "Summarise a passage of text"
    }
  ]
}`

// toolServiceName and toolOperation are the service tool's dispatch, spelled
// out because the stub counts by operation name and a silent rename would
// otherwise read as "the tool was never called".
const (
	toolServiceName = "weather"
	toolOperation   = "get"

	// childToolOperation is what the summarise CHILD reports with. It is a
	// durable call from inside the child, not from the parent, which is what
	// makes it evidence about the child rather than about the parent's replay.
	childToolOperation = "summarise"
)

// TestCrashMidAgentLoopResumesWithoutReaskingTheModel is the acceptance.
func TestCrashMidAgentLoopResumesWithoutReaskingTheModel(t *testing.T) {
	db := ownerDB(t)
	defer db.Close()

	suffix := uniqueSuffix()
	taskQueue := "queue-agent-" + suffix
	wfID := "agent-wf-" + suffix

	// The tools service. Reused from this suite rather than re-implemented: it
	// already counts per operation and already knows the worker forwards
	// unknown services to --bench-svc-url as POST /call/{service}/{operation},
	// which is exactly how the agent's service tool reaches it.
	svc := newChargeService(t)

	// The model. Scripted, so "which turn is it" is a fact about the stub
	// rather than about elapsed time.
	llm := newLLMStub(t)

	deployAgentScenario(t, db, taskQueue)
	bin := buildWorker(t)

	// Hold the turn that follows both tools: the only window that separates
	// "completed steps are durable" from "the loop re-runs". A crash before the
	// first tool has nothing durable to preserve, so the two hypotheses predict
	// the same counts there.
	release := llm.holdFinalTurn()
	// Deferred as well as called on the happy path, because the held request is
	// a live goroutine inside httptest.Server: a Fatalf below returns from the
	// test without ever releasing it, httptest's own Close then waits for the
	// connection, and an assertion failure becomes a ten-minute package timeout
	// with the message buried in a goroutine dump. The failure this test
	// reports most often is the one it is built to catch, so the failure path
	// has to be the cheap one. release is idempotent, so the explicit call
	// below and this one do not conflict.
	defer release()

	first := startWorker(t, bin, taskQueue, svc.srv.URL,
		"--plugin-config", llm.configFile(t),
		"--plugin-egress-allow-private", "127.0.0.1")
	startWorkflowWithInput(t, db, wfID, agentConfigJSON, taskQueue)

	llm.awaitHeldCall(t, first, startBudget)

	// THE RECORDED EVENTS, read while the workflow is still running. Two turns
	// have completed and their results are durable, so the history must already
	// name them: a crash cannot lose an LLM turn whose answer the worker
	// already handed to the guest.
	//
	// WHICH HISTORY, because this is the assertion that a first draft got
	// wrong. The client starts the agent as a CHILD, so the loop's events --
	// the LLM turns and the tool work -- are recorded against the agent's own
	// run id, not the client's. Reading the parent finds child_workflow and
	// await_child and nothing else, and reports "the LLM turns are not durable"
	// about a run in which they plainly are. The two histories are consulted
	// deliberately: the child's for the loop, the parent's for the fact that
	// the loop was a child at all.
	agentRun := childRunID(t, db, wfID, "agent")

	if got := eventTypeCount(t, db, agentRun, "plugin_call"); got != 2 {
		t.Fatalf("before the crash the agent's history has %d plugin_call events, want 2 -- "+
			"the two completed LLM turns are not durable, so nothing about recovery can be "+
			"concluded from this run\n--- recorded event types ---\n%s\n--- worker log ---\n%s",
			got, eventTypeCensus(t, db, agentRun), first.output())
	}
	if got := eventTypeCount(t, db, agentRun, "child_workflow"); got != 1 {
		t.Fatalf("before the crash the agent's history has %d child_workflow events, want 1 -- "+
			"the workflow tool's child was not recorded against the agent\n--- recorded event types ---\n%s\n--- worker log ---\n%s",
			got, eventTypeCensus(t, db, agentRun), first.output())
	}
	if got := eventTypeCount(t, db, wfID, "child_workflow"); got != 1 {
		t.Fatalf("before the crash the client's history has %d child_workflow events, want 1 -- "+
			"the client did not start the agent as a child, so this run is not exercising the "+
			"shipped agent workflow\n--- recorded event types ---\n%s\n--- worker log ---\n%s",
			got, eventTypeCensus(t, db, wfID), first.output())
	}
	// Both tools must have finished BEFORE the crash, or the counts below
	// measure a crash at the wrong point and the discrimination is lost.
	if w, c := svc.count(toolOperation), svc.count(childToolOperation); w != 1 || c != 1 {
		t.Fatalf("before the crash: %s.%s=%d child=%d, want 1/1 -- the loop did not reach the "+
			"third turn with both tools complete, so this run cannot distinguish a durable "+
			"loop from one that re-runs everything\n--- worker log ---\n%s",
			toolServiceName, toolOperation, w, c, first.output())
	}
	if got := llm.received(); got != 3 {
		t.Fatalf("before the crash the model was asked %d times, want 3 (two completed turns "+
			"plus the one held in flight)\n--- worker log ---\n%s", got, first.output())
	}

	// The crash. The worker dies holding an HTTP response it never received.
	first.kill()
	afterCrash := llm.received()

	// Release the held request so its handler goroutine does not outlive the
	// test. Nothing is listening -- the worker is gone.
	release()

	second := startWorker(t, bin, taskQueue, svc.srv.URL,
		"--plugin-config", llm.configFile(t),
		"--plugin-egress-allow-private", "127.0.0.1")

	status, errMsg := awaitTerminal(t, db, wfID, completeBudget)
	if status != "done" && status != "completed" {
		t.Fatalf("workflow ended %q (%s) after the restart\n--- worker log ---\n%s",
			status, errMsg, second.output())
	}

	// THE ACCEPTANCE SENTENCE. Exactly one more LLM call than before the crash:
	// the interrupted turn, re-asked because the engine cannot know whether it
	// was answered. Three would mean the loop re-ran from the top.
	if delta := llm.received() - afterCrash; delta != 1 {
		t.Errorf("the model was asked %d more times after the restart, want 1 -- the "+
			"interrupted turn and no other. %d means the loop re-asked for turns it had "+
			"already completed; 0 means it never resumed.\n%s\n--- worker log ---\n%s",
			delta, delta, llm.diagnose(), second.output())
	}
	// The tools. Neither may be re-run: both completed before the crash, and a
	// re-run is a duplicated external side effect, not a duplicated question.
	if got := svc.count(toolOperation); got != 1 {
		t.Errorf("the service tool %s.%s ran %d times across the crash, want 1 -- a "+
			"completed DurableCall was re-issued on replay\n%s",
			toolServiceName, toolOperation, got, svc.diagnose())
	}
	if got := svc.count(childToolOperation); got != 1 {
		t.Errorf("the workflow tool's child ran %d times across the crash, want 1 -- the "+
			"parent re-started a child it had already finished, which duplicates every "+
			"side effect inside it\n%s", got, svc.diagnose())
	}

	// The agent's own answer survived: the result is the final turn's, not a
	// replay artifact and not an error.
	answer := agentAnswer(t, db, wfID)
	if answer != "Tokyo is warm." {
		t.Errorf("the workflow's result is %q, want %q", answer, "Tokyo is warm.")
	}
}

// ---------------------------------------------------------------------------
// Scenario deployment
// ---------------------------------------------------------------------------

// deployAgentScenario registers the three definitions this scenario needs.
//
// Three, not one, because the acceptance names two different tool kinds and
// both have to be real deployments:
//
//	agent         the shipped agent workflow (examples/agent)
//	agentclient   the Go client that starts it as a child (testdata/agentclient)
//	summarise     the workflow tool's child (testdata/summarise)
//
// Each declares exactly one entry point, which matters: a child is started by
// NAME (h.ChildWorkflow("summarise", ...)), and the engine resolves the entry
// point from the module's own metadata only when there is exactly one. A
// second entry point in the same artifact would make every child start
// ambiguous, so these are separate builds rather than one artifact with
// several entries.
func deployAgentScenario(t *testing.T, db *sql.DB, taskQueue string) {
	t.Helper()

	deployOneDef(t, db, "agent", buildWorkflowWASM(t, "examples/agent"), taskQueue)
	deployOneDef(t, db, "agentclient", buildWorkflowWASM(t, "testdata/agentclient"), taskQueue)
	deployOneDef(t, db, "summarise", buildWorkflowWASM(t, "testdata/summarise"), taskQueue)

	grantLoopbackEgress(t, db)
}

// grantLoopbackEgress lets the default tenant reach the two stubs.
//
// THIS IS THE SECOND OF TWO PERMISSIONS, and finding only the first is the
// mistake this function exists to prevent. Plugin egress is refused unless the
// operator permits the destination AND the requesting tenant permits it
// (pluginEgressTransport builds one guard with both hooks). The operator half
// is the worker's --plugin-egress-allow-private flag; the tenant half is this
// row, and neither substitutes for the other.
//
// The worker says so, in the failure message, and it is worth reading twice:
//
//	egress to 127.0.0.1 is refused by cleat's network policy:
//	host is not on this tenant's egress allowlist
//
// A run that sets only the flag fails here, on a clean tree, with an error that
// names the mechanism rather than the message it is easy to misread as "the
// flag did not work".
//
// The grant is for the DEFAULT tenant, which is the one every row in this
// suite is written under. It is a real deployment mechanism -- `cleatctl
// egress-allow` writes the same row -- rather than a test-only back door, so
// the path being exercised is the shipped one.
func grantLoopbackEgress(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`
		INSERT INTO admin.tenant_egress_allow (tenant_id, host)
		VALUES ($1, '127.0.0.1')
		ON CONFLICT (tenant_id, host) DO NOTHING`, defaultTenant); err != nil {
		t.Fatalf("granting loopback egress to the default tenant: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM admin.tenant_egress_allow WHERE tenant_id = $1 AND host = '127.0.0.1'`, defaultTenant)
	})
}

// buildWorkflowWASM compiles a workflow directory in this repository to WASM.
//
// The directory is built in-place against the LOCAL modules, which is what
// makes it possible to deploy a workflow that imports an unreleased SDK
// package -- `cleat/agentworkflow` here. A fixture that resolved the SDK from
// the module proxy, as a scaffolded project does, could not.
func buildWorkflowWASM(t *testing.T, relDir string) []byte {
	t.Helper()
	root := repoRoot(t)
	outDir := t.TempDir()

	cmd := exec.Command("go", "run", filepath.Join(root, "cmd", "cleat"),
		"build", "--target", "go", "-o", outDir, filepath.Join(root, relDir))
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building %s: %v\n%s", relDir, err, out)
	}

	entries, err := os.ReadDir(outDir)
	if err != nil {
		t.Fatalf("reading build output for %s: %v", relDir, err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".wasm" {
			b, err := os.ReadFile(filepath.Join(outDir, e.Name()))
			if err != nil {
				t.Fatalf("reading %s: %v", e.Name(), err)
			}
			return b
		}
	}
	t.Fatalf("no .wasm in the build output for %s", relDir)
	return nil
}

// deployOneDef registers one definition the way deployFixture registers
// crashcall, and for the same reasons: the tenant must be explicit (the RLS
// policy is `tenant_id = assert_tenant_set()` and NULL does not satisfy it, so
// a row without one is invisible and the symptom is "wasm not found"), and the
// conflict target is the full primary key.
func deployOneDef(t *testing.T, db *sql.DB, name string, wasm []byte, taskQueue string) {
	t.Helper()
	if _, err := db.Exec(`
		INSERT INTO workflow_defs
			(name, version, wasm_bytes, entry_points, min_version,
			 max_history_length, dag_spec, task_queue, abi_version, plugin_deps, tenant_id)
		VALUES ($1, 1, $2, ARRAY[$1], 1, 10000, '{}'::jsonb, $3, 1, '{}'::jsonb, $4)
		ON CONFLICT (tenant_id, name, version) DO UPDATE SET
			wasm_bytes = EXCLUDED.wasm_bytes,
			entry_points = EXCLUDED.entry_points,
			task_queue = EXCLUDED.task_queue`,
		name, wasm, taskQueue, defaultTenant); err != nil {
		t.Fatalf("deploying the %s definition: %v", name, err)
	}
}

// startWorkflowWithInput queues one instance whose input is the given JSON
// verbatim.
//
// Not startWorkflowEntry, which wraps its argument in {"__entry_point": ...}:
// the agent client's input IS the agent's config, and a wrapper field would
// reach the agent as part of it. Each of these definitions declares one entry
// point, so the engine resolves it from the module metadata and no
// __entry_point is needed -- which is also the mechanism the child starts
// depend on, so the scenario exercises it rather than sidestepping it.
func startWorkflowWithInput(t *testing.T, db *sql.DB, id, inputJSON, taskQueue string) {
	t.Helper()
	if _, err := db.Exec(`
		INSERT INTO workflow_instances (id, def_name, def_version, status, input, task_queue, tenant_id)
		VALUES ($1, 'agentclient', 1, 'ready', $2::jsonb, $3, $4)`,
		id, inputJSON, taskQueue, defaultTenant); err != nil {
		t.Fatalf("queueing workflow %s: %v", id, err)
	}
	t.Cleanup(func() {
		// Descendants first, and TWO levels of them. The client's child is the
		// agent; the agent's child is the workflow tool's summarise. Deleting
		// only direct children leaves the grandchild's instances and events
		// behind, and a later run counting instances by definition name would
		// find them.
		_, _ = db.Exec(`DELETE FROM event_history WHERE workflow_id = $1
			OR workflow_id IN (SELECT id FROM workflow_instances WHERE parent_workflow_id = $1
				OR parent_workflow_id IN (SELECT id FROM workflow_instances WHERE parent_workflow_id = $1))`, id)
		_, _ = db.Exec(`DELETE FROM workflow_instances WHERE parent_workflow_id = $1
			OR parent_workflow_id IN (SELECT id FROM workflow_instances WHERE parent_workflow_id = $1)`, id)
		_, _ = db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, id)
	})
}

// ---------------------------------------------------------------------------
// Reading the recorded events
// ---------------------------------------------------------------------------

// childRunID returns the run id of one definition's child of a parent.
//
// Fatal rather than returning "", because every caller uses this to pick the
// history it then asserts about: an empty id would make eventTypeCount report
// 0 and the failure would read as "the events are missing" when the truth is
// "the child was never started". Those need different fixes.
func childRunID(t *testing.T, db *sql.DB, parentID, defName string) string {
	t.Helper()
	var id string
	err := db.QueryRow(
		`SELECT id FROM workflow_instances WHERE parent_workflow_id = $1 AND def_name = $2`,
		parentID, defName).Scan(&id)
	if err == sql.ErrNoRows {
		t.Fatalf("%s never started a child named %q; the workflow it is being read for "+
			"did not reach the point this test measures", parentID, defName)
	}
	if err != nil {
		t.Fatalf("finding the %q child of %s: %v", defName, parentID, err)
	}
	return id
}

// eventTypeCount counts the parent's recorded events of one type.
func eventTypeCount(t *testing.T, db *sql.DB, id, eventType string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT count(*) FROM event_history WHERE workflow_id = $1 AND event_type = $2`,
		id, eventType).Scan(&n); err != nil {
		t.Fatalf("counting %s events for %s: %v", eventType, id, err)
	}
	return n
}

// eventTypeCensus reports what the history DOES contain, for the failure
// message. "0 plugin_call events" has two very different causes -- the events
// were never written, or they were written under another type -- and the
// census separates them without a second run.
func eventTypeCensus(t *testing.T, db *sql.DB, id string) string {
	t.Helper()
	rows, err := db.Query(
		`SELECT event_type, count(*) FROM event_history WHERE workflow_id = $1
		 GROUP BY event_type ORDER BY event_type`, id)
	if err != nil {
		return fmt.Sprintf("(census failed: %v)", err)
	}
	defer rows.Close()
	var out string
	for rows.Next() {
		var et string
		var n int
		if err := rows.Scan(&et, &n); err != nil {
			return fmt.Sprintf("(census failed: %v)", err)
		}
		out += fmt.Sprintf("  %-20s %d\n", et, n)
	}
	if out == "" {
		out = "  (no events recorded at all)\n"
	}
	return out
}

// agentAnswer reads the model's final answer out of the stored result.
//
// It unwraps rather than decoding once, because a cleat entry point returns a
// STRING and the engine stores that string's JSON encoding: the agent's result
// is an object, that object is a string field of the client's return value, and
// the client's return value is itself a string as far as the engine is
// concerned. One unmarshal reaches the client's string, a second reaches the
// agent's object. The loop is bounded and stops as soon as a level is not a
// quoted string, so a result shape that changes does not make this spin.
func agentAnswer(t *testing.T, db *sql.DB, id string) string {
	t.Helper()
	raw := workflowResult(t, db, id)
	if raw == "" {
		return ""
	}
	for i := 0; i < 3; i++ {
		var s string
		if json.Unmarshal([]byte(raw), &s) != nil {
			break
		}
		raw = s
	}
	var answer struct {
		Answer string `json:"answer"`
	}
	if err := json.Unmarshal([]byte(raw), &answer); err != nil {
		t.Fatalf("the recorded result of %s is not the agent's result object: %v\nraw: %s",
			id, err, workflowResult(t, db, id))
	}
	return answer.Answer
}

// ---------------------------------------------------------------------------
// The model
// ---------------------------------------------------------------------------

// llmStub is an OpenAI-compatible chat endpoint whose replies are a function
// of the CONVERSATION, plus a counter.
//
// SCRIPTING ON THE CALL INDEX IS THE OBVIOUS DESIGN AND IT MAKES THE WHOLE
// TEST VACUOUS. The first version of this stub replied to the Nth request with
// the Nth reply: tool, tool, answer. Under that script a loop that re-ran
// completely after the crash produced *exactly* the counts a correct replay
// produces -- its first call is the fourth request, which the script answers
// with the final answer, so the re-run asks nothing more, calls no tool, and
// finishes. Measured: deleting the agent's entire durable history before the
// restart left the test passing, and it passed in a 5-line log that said
// nothing.
//
// What was missing is that a real model answers what it is asked. So the reply
// is chosen by how many TOOL RESULTS the request already carries:
//
//	0 tool results  -> ask for lookup_weather
//	1 tool result   -> ask for summarise
//	>= 2            -> answer
//
// The same three-way choice then discriminates in the direction the test
// claims it does. A re-run starts at zero tool results, asks again, and the
// service counts go to 2 -- which is the red the first version could not
// produce.
//
// It stands up as a real HTTP server because the thing under test is the real
// cleat-worker subprocess: it reaches plugins/llm, which reaches this over the
// network with the config it was handed. Nothing here is a library call, and
// nothing is a mock of the plugin.
type llmStub struct {
	srv *httptest.Server

	mu sync.Mutex
	// bodies is the request log in arrival order. It is what the acceptance's
	// "no second LLM call for completed turns" counts.
	bodies []string

	// holdAtToolResults is the conversation state whose first request blocks
	// (0 = hold nothing). State, not an index -- see the doc comment above.
	holdAtToolResults int
	holding           bool
	gate              chan struct{}
	hold              chan struct{}
	gateOnce          sync.Once
}

// newLLMStub starts the stub.
//
// The path is /chat/completions because that is what providers.OpenAIChat
// appends to the configured base_url. The shape of the answer is the
// provider's, not the plugin's own: the plugin forwards the provider's
// response through, so a correctly-shaped provider reply is what the agent
// sees.
func newLLMStub(t *testing.T) *llmStub {
	t.Helper()
	s := &llmStub{gate: make(chan struct{}), hold: make(chan struct{})}
	s.srv = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *llmStub) handle(w http.ResponseWriter, r *http.Request) {
	raw := ""
	if r.Body != nil {
		buf := make([]byte, 0, 1<<16)
		tmp := make([]byte, 4096)
		for {
			n, err := r.Body.Read(tmp)
			buf = append(buf, tmp[:n]...)
			if err != nil {
				break
			}
		}
		raw = string(buf)
	}

	toolResults := toolResultsIn(raw)

	s.mu.Lock()
	s.bodies = append(s.bodies, raw)
	shouldHold := s.holdAtToolResults != 0 && toolResults == s.holdAtToolResults && !s.holding
	if shouldHold {
		s.holding = true
	}
	hold := s.hold
	s.mu.Unlock()

	// Record the request BEFORE blocking, for the same reason chargeService
	// does: the crash window is a call the model has received and the worker
	// has not been told the answer to.
	if shouldHold {
		s.gateOnce.Do(func() { close(s.gate) })
		<-hold
	}

	body := answer(toolResults)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(body))
}

// toolResultsIn counts the tool results the conversation already carries.
//
// A tool result is a message with role "tool", which is the OpenAI wire form
// the agent appends after each call. Counting them rather than the assistant's
// tool_calls is deliberate: it is the results that tell the model what has
// actually happened, and it is precisely the thing a re-run would have fewer
// of.
func toolResultsIn(body string) int {
	var req struct {
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		// A request this stub cannot read is a request about which nothing
		// can be said -- reported as zero tool results, which asks for the
		// first tool and so shows up as a duplicated service call rather than
		// as a silently-correct answer.
		return 0
	}
	n := 0
	for _, m := range req.Messages {
		if m.Role == "tool" {
			n++
		}
	}
	return n
}

// answer is the script: a function of the conversation, never of the call
// index. See the llmStub doc comment for why that distinction is the whole
// test.
func answer(toolResults int) string {
	type functionCall struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	}
	type toolCall struct {
		ID       string       `json:"id"`
		Type     string       `json:"type"`
		Function functionCall `json:"function"`
	}
	type message struct {
		Role      string     `json:"role"`
		Content   string     `json:"content"`
		ToolCalls []toolCall `json:"tool_calls,omitempty"`
	}
	type choice struct {
		Message      message `json:"message"`
		FinishReason string  `json:"finish_reason"`
	}
	reply := struct {
		Model   string   `json:"model"`
		Choices []choice `json:"choices"`
		Usage   struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}{Model: "stub-model"}
	reply.Usage.PromptTokens, reply.Usage.CompletionTokens, reply.Usage.TotalTokens = 10, 5, 15

	switch {
	case toolResults == 0:
		reply.Choices = []choice{{FinishReason: "tool_calls", Message: message{
			Role: "assistant",
			ToolCalls: []toolCall{{ID: "call_weather", Type: "function", Function: functionCall{
				Name: "lookup_weather", Arguments: `{"city":"Tokyo"}`,
			}}},
		}}}
	case toolResults == 1:
		reply.Choices = []choice{{FinishReason: "tool_calls", Message: message{
			Role: "assistant",
			ToolCalls: []toolCall{{ID: "call_summarise", Type: "function", Function: functionCall{
				Name: "summarise", Arguments: `{"input":"tokyo is warm"}`,
			}}},
		}}}
	default:
		reply.Choices = []choice{{FinishReason: "stop", Message: message{
			Role: "assistant", Content: "Tokyo is warm.",
		}}}
	}

	out, err := json.Marshal(reply)
	if err != nil {
		// Unreachable: every field above is a plain string or int.
		panic(err)
	}
	return string(out)
}

// configFile writes a --plugin-config naming this stub as the openai provider.
//
// requires_deployment_key is false on purpose and is the field that makes this
// possible at all: the plugin's default is that an enabled non-ollama provider
// demands a deployment secret at boot, so a keyless local endpoint would
// refuse the whole worker at startup. `false` is the documented opt-out for a
// self-hosted base_url.
//
// The file is written once per stub rather than per worker so that the two
// workers in this test are configured identically -- they have to be, since
// the second is the same deployment picking up the first one's work.
func (s *llmStub) configFile(t *testing.T) string {
	t.Helper()
	return writePluginConfig(t, s.srv.URL)
}

// holdFinalTurn makes the first request carrying two tool results block until
// the returned release func is called.
//
// Identified by conversation state rather than by position: that is the turn
// the agent reaches only after BOTH tools have completed, which is the only
// point at which "completed steps are durable" and "the loop re-runs" predict
// different counts. Release is idempotent.
func (s *llmStub) holdFinalTurn() (release func()) {
	s.mu.Lock()
	s.holdAtToolResults = 2
	h := s.hold
	s.mu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { close(h) }) }
}

func (s *llmStub) received() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.bodies)
}

// awaitHeldCall blocks until the held call has arrived.
func (s *llmStub) awaitHeldCall(t *testing.T, w *worker, budget time.Duration) {
	t.Helper()
	select {
	case <-s.gate:
	case <-time.After(budget):
		t.Fatalf("no request carrying %d tool results arrived within %v; the agent loop "+
			"never reached the turn this test crashes on, so there is no crash window"+
			"\n%s\n--- worker log ---\n%s", s.holdAtToolResults, budget, s.diagnose(), w.output())
	}
}

// toolsSeen returns the tool names the model was shown, from the first request.
//
// Read from the REQUEST rather than from the fixture, because the claim is
// that the tool set travels in the run's input. A test that asked the fixture
// what it sent would pass against a fixture whose config never reached the
// agent.
func (s *llmStub) toolsSeen(t *testing.T) []string {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.bodies) == 0 {
		return nil
	}
	var req struct {
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal([]byte(s.bodies[0]), &req); err != nil {
		t.Fatalf("the model's first request is not the shape the agent sends: %v\n%s", err, s.bodies[0])
	}
	names := make([]string, 0, len(req.Tools))
	for _, tool := range req.Tools {
		names = append(names, tool.Function.Name)
	}
	return names
}

// diagnose reports what the stub actually received. A count of 0 has two very
// different causes -- the loop never called out, or it called with a shape
// this handler recorded as something else -- and they need different fixes.
func (s *llmStub) diagnose() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := fmt.Sprintf("  the model was asked %d times; the first request was:\n", len(s.bodies))
	if len(s.bodies) == 0 {
		return out + "  (nothing arrived)\n"
	}
	b := s.bodies[0]
	if len(b) > 600 {
		b = b[:600] + "…"
	}
	return out + "  " + b + "\n"
}

// writePluginConfig writes the shared --plugin-config the worker hands to every
// linked plugin. There is no per-plugin section: each plugin reads the fields
// it knows and reports ErrNotConfigured for a config that names none of them,
// so the llm section is written flat, exactly as plugins/llm's Config reads it.
func writePluginConfig(t *testing.T, baseURL string) string {
	t.Helper()
	cfg := map[string]any{
		"providers": map[string]any{
			"openai": map[string]any{
				"enabled":                 true,
				"base_url":                baseURL,
				"default_model":           "stub-model",
				"requires_deployment_key": false,
			},
		},
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshalling the plugin config: %v", err)
	}
	path := filepath.Join(t.TempDir(), "plugin-config.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("writing the plugin config: %v", err)
	}
	return path
}
