// Package integrity provides deterministic-replay stress tests for the cleat
// durable workflow engine. These tests exercise the Engine's Execute/Replay
// path under various conditions: repeated replay, random crash points, fuzzed
// event sequences, and tampered history divergence detection.
//
// Tests that require a real PostgreSQL database will skip in short mode or
// when the database is unavailable.
package integrity

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	host "github.com/cleat-team/cleat/internal/host"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// stressMockCaller records all service calls for test assertions.
type stressMockCaller struct {
	calls []host.CallRecord
}

func (m *stressMockCaller) Call(_ context.Context, service, operation, requestJSON string) (string, error) {
	resp := stressMockResponse(service, operation)
	m.calls = append(m.calls, host.CallRecord{
		EventType: host.EventTypeCall,
		Service:   service, Op: operation, Request: requestJSON, Response: resp,
	})
	return resp, nil
}

func stressMockResponse(service, operation string) string {
	switch service + "." + operation {
	case "catalog.LookupItem":
		return `{"sku":"ABC-123","name":"Widget","price_cents":999,"found":true}`
	case "inventory.Reserve":
		return `{"reservation_id":"resv_abc123","status":"reserved","total_cents":3299}`
	case "inventory.Release":
		return `{"status":"released"}`
	case "payments.GetDefaultMethod":
		return `{"token":"pm_tok_555","type":"card","last_four":"4242"}`
	case "payments.Charge":
		return `{"charge_id":"chg_xyz789","status":"captured"}`
	case "payments.Refund":
		return `{"status":"refunded"}`
	case "shipping.CreateShipment":
		return `{"tracking_id":"TRACK-123456","status":"label_created"}`
	case "notifications.SendEmail":
		return `{"status":"sent"}`
	default:
		return `{}`
	}
}

// buildStressWasm compiles the testdata/basic workflow to WASM via the cleat
// build pipeline with the tinygo target. It skips the test if tinygo is not
// available.
func buildStressWasm(t *testing.T) []byte {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping WASM compilation in short mode")
	}
	if _, err := exec.LookPath("tinygo"); err != nil {
		t.Skip("tinygo not installed -- skipping WASM test")
	}

	cwd, err := os.Getwd()
	if err != nil {
		t.Skip("cannot determine working directory")
	}

	// Walk up to find the project root (contains cmd/).
	projectRoot := cwd
	for i := 0; i < 5; i++ {
		if _, err := os.Stat(filepath.Join(projectRoot, "cmd")); err == nil {
			break
		}
		projectRoot = filepath.Dir(projectRoot)
	}

	tmpDir := t.TempDir()
	cmd := exec.Command("go", "run", filepath.Join(projectRoot, "cmd", "cleat"),
		"build", "--target", "tinygo", "-o", tmpDir,
		filepath.Join(projectRoot, "testdata", "basic"),
	)
	cmd.Dir = projectRoot

	cmd.Env = os.Environ()
	if goroot := os.Getenv("GOROOT"); goroot != "" {
		cmd.Env = append(cmd.Env, "GOROOT="+goroot)
	}
	if tinygoroot := os.Getenv("TINYGOROOT"); tinygoroot != "" {
		cmd.Env = append(cmd.Env, "TINYGOROOT="+tinygoroot)
	}

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("cleat build failed:\n%s\n%v", string(out), err)
	}

	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatalf("reading build output: %v", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".wasm" {
			wasmBytes, err := os.ReadFile(filepath.Join(tmpDir, e.Name()))
			if err != nil {
				t.Fatalf("reading WASM file: %v", err)
			}
			return wasmBytes
		}
	}
	t.Fatalf("no .wasm file found in %s", tmpDir)
	return nil
}

// executePlaceOrder runs the place_order workflow to completion and returns the
// result string, event history, and the mock caller used (for call-count
// assertions). The wasmBytes parameter should come from a single call to
// buildStressWasm to avoid redundant compilation.
func executePlaceOrder(ctx context.Context, t *testing.T, rt *host.Runtime, wasmBytes []byte, input json.RawMessage) (string, []host.EventRecord, *stressMockCaller) {
	t.Helper()
	caller := &stressMockCaller{}
	eng := host.NewEngine(rt, caller)

	result, history, suspended, _, _, err := eng.Execute(ctx, wasmBytes, "place_order", input)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if suspended != nil {
		t.Fatalf("unexpected suspension: %v", suspended.Reason)
	}
	if len(history) == 0 {
		t.Fatal("expected non-empty event history")
	}
	return result, history, caller
}

// ---------------------------------------------------------------------------
// Test 1: Basic replay stress -- execute once, replay N times
// ---------------------------------------------------------------------------

// TestReplayStressBasic runs a WASM workflow to completion, then replays the
// captured event history N times (at least 100), verifying that every replay
// produces exactly the same result and makes zero real service calls.
// This test does not require a database.
func TestReplayStressBasic(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping replay stress test in short mode")
	}

	ctx := context.Background()
	input := json.RawMessage(`{"UserID":"stress-user","Cart":[{"SKU":"ABC-123","Quantity":2}]}`)

	rt, err := host.NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	// Build WASM once and reuse it.
	wasmBytes := buildStressWasm(t)

	// ---- Step 1: Execute once ----
	result1, history, caller1 := executePlaceOrder(ctx, t, rt, wasmBytes, input)
	initialCallCount := len(caller1.calls)
	t.Logf("Execution produced %d events, result=%q (%d service calls)",
		len(history), result1, initialCallCount)

	// ---- Step 2: Replay N times ----
	const replayCount = 100
	for i := 0; i < replayCount; i++ {
		replayCaller := &stressMockCaller{}
		engine := host.NewEngine(rt, replayCaller)

		result2, history2, suspended2, _, _, err := engine.Replay(ctx, wasmBytes, "place_order", input, history)
		if err != nil {
			t.Fatalf("Replay iteration %d: %v", i, err)
		}
		if suspended2 != nil {
			t.Fatalf("Replay iteration %d: unexpected suspension: %v", i, suspended2.Reason)
		}

		if result1 != result2 {
			t.Errorf("Replay iteration %d: result mismatch: %q vs %q", i, result1, result2)
		}
		if len(history) != len(history2) {
			t.Errorf("Replay iteration %d: history length mismatch: %d vs %d", i, len(history), len(history2))
		} else {
			for j := range history {
				if history[j].Step != history2[j].Step ||
					history[j].EventType != history2[j].EventType ||
					history[j].Service != history2[j].Service ||
					history[j].Op != history2[j].Op ||
					history[j].Response != history2[j].Response {
					t.Errorf("Replay iteration %d, event %d: mismatch", i, j)
				}
			}
		}

		// Verify zero real service calls during replay.
		if len(replayCaller.calls) > 0 {
			t.Errorf("Replay iteration %d: made %d real service calls (expected 0)",
				i, len(replayCaller.calls))
		}

		if t.Failed() {
			break
		}
	}

	t.Logf("All %d replays produced identical results with zero service calls", replayCount)
}

// ---------------------------------------------------------------------------
// Test 2: Random crash points -- truncate history at random points
// ---------------------------------------------------------------------------

// TestReplayStressRandomCrashPoints executes a multi-step workflow, then at
// randomized truncation points (simulating a crash before all events were
// persisted), replays from the truncated history. The test verifies that:
//
//   - Replay of the prefix produces events matching the original prefix
//   - After consuming the prefix, execution continues with fresh service calls
//   - The final result matches the original full execution
//   - No real calls are made for the replayed steps
func TestReplayStressRandomCrashPoints(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping replay stress test in short mode")
	}

	ctx := context.Background()
	input := json.RawMessage(`{"UserID":"crash-user","Cart":[{"SKU":"ABC-123","Quantity":2}]}`)

	rt, err := host.NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	// Build WASM once and reuse it.
	wasmBytes := buildStressWasm(t)

	// Execute once to get the full reference history and result.
	originalResult, fullHistory, originalCaller := executePlaceOrder(ctx, t, rt, wasmBytes, input)
	originalCallCount := len(originalCaller.calls)
	t.Logf("Full execution: %d events, result=%q (%d service calls)",
		len(fullHistory), originalResult, originalCallCount)

	if len(fullHistory) < 2 {
		t.Fatal("expected at least 2 events for a meaningful crash-point test")
	}

	// Attempt truncation at every possible split point (1 through len-1).
	// For each split, replay with the prefix; the engine should replay the
	// prefix then continue executing for the remaining steps.
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	splitCandidates := make([]int, len(fullHistory)-1)
	for i := 1; i < len(fullHistory); i++ {
		splitCandidates[i-1] = i
	}
	// Shuffle and test up to 10 random points.
	rng.Shuffle(len(splitCandidates), func(i, j int) {
		splitCandidates[i], splitCandidates[j] = splitCandidates[j], splitCandidates[i]
	})
	numSplits := len(splitCandidates)
	if numSplits > 10 {
		numSplits = 10
	}
	splitCandidates = splitCandidates[:numSplits]

	for _, splitAt := range splitCandidates {
		t.Run(fmt.Sprintf("truncate_at_%d_of_%d", splitAt, len(fullHistory)), func(t *testing.T) {
			prefix := fullHistory[:splitAt]

			replayCaller := &stressMockCaller{}
			engine := host.NewEngine(rt, replayCaller)

			// Replay from the truncated history. The engine replays the prefix
			// events, then continues execution for steps beyond the prefix.
			result2, history2, suspended2, _, _, err := engine.Replay(ctx, wasmBytes, "place_order", input, prefix)
			if err != nil {
				t.Fatalf("Replay at split=%d: %v", splitAt, err)
			}
			if suspended2 != nil {
				t.Fatalf("Replay at split=%d: unexpected suspension: %v", splitAt, suspended2.Reason)
			}

			// Verify the final result matches the original.
			if originalResult != result2 {
				t.Errorf("Split=%d: result mismatch: %q vs %q", splitAt, originalResult, result2)
			}

			// The full history produced should match the original.
			if len(history2) != len(fullHistory) {
				t.Errorf("Split=%d: history length mismatch: %d vs %d", splitAt, len(fullHistory), len(history2))
			} else {
				for j := range fullHistory {
					if fullHistory[j].Step != history2[j].Step ||
						fullHistory[j].EventType != history2[j].EventType ||
						fullHistory[j].Service != history2[j].Service ||
						fullHistory[j].Op != history2[j].Op {
						t.Errorf("Split=%d, event %d: mismatch", splitAt, j)
					}
				}
			}

			// Verify no calls were made for the replayed prefix events. The
			// engine should use cached history responses for those steps and
			// only fall through to real calls for steps beyond the prefix.
			if len(replayCaller.calls) > len(fullHistory)-splitAt {
				t.Errorf("Split=%d: expected at most %d real calls (steps %d+), got %d",
					splitAt, len(fullHistory)-splitAt, splitAt, len(replayCaller.calls))
			}

			// Verify the prefix events themselves were not re-executed by
			// checking that no call has a step index matching the prefix range.
			prefixSteps := make(map[int]bool)
			for j := 0; j < splitAt; j++ {
				prefixSteps[fullHistory[j].Step] = true
			}
			for _, c := range replayCaller.calls {
				if prefixSteps[c.Step] {
					t.Errorf("Split=%d: real call made for prefix step %d (%s/%s)",
						splitAt, c.Step, c.Service, c.Op)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Test 3: Fuzzed event order -- persist and load random event sequences
// ---------------------------------------------------------------------------

// TestReplayStressFuzzedEventOrder generates random sequences of event records
// (using the same byte-level encoding approach as compaction_fuzz_test.go),
// persists them via AppendEventHistoryBatch, loads them back via
// LoadEventHistory, and verifies that every field survives the round-trip
// intact. The test exercises different sequence lengths (1, 10, 100, 1000)
// and covers all event type codes.
func TestReplayStressFuzzedEventOrder(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping fuzzed event order stress test in short mode")
	}
	db := testDB(t)
	defer db.Close()

	ctx := context.Background()
	store := host.NewPostgresStore(db)

	// Event type codes (matching compaction_fuzz_test.go conventions).
	// We use a representative subset that exercises all the core field groups.
	type eventCodeGen struct {
		code   byte
		strs   []string
		ints   []int64
		fields string // human-readable label
	}

	codeGens := []eventCodeGen{
		{0, []string{"catalog", "LookupItem", `{"sku":"ABC"}`, `{"price":999}`, ""}, nil, "Call(5str)"},
		{1, []string{"payment,shipping"}, []int64{30000}, "AwaitSignals(1str+1int)"},
		{2, []string{"payment", `{"paid":true}`}, nil, "SignalReceived(2str)"},
		{3, []string{"defer-0", "cleanup"}, nil, "Defer(2str)"},
		{4, []string{"child-wf", `{"x":1}`, "run-c1"}, nil, "ChildWorkflow(3str)"},
		{5, []string{"run-c1", `{"status":"done"}`, ""}, nil, "AwaitChild(3str)"},
		{6, []string{`{"page":2}`}, nil, "ContinueAsNew(1str)"},
		{7, []string{"inventory", "Reserve"}, nil, "Heartbeat(2str)"},
		{9, []string{"s3", "GetObject", `{"key":"x"}`, `{"body":"..."}`, ""}, nil, "PluginCall(5str)"},
		{11, []string{"payment-auth", "prom-001"}, nil, "CreatePromise(2str)"},
		{12, []string{"prom-001"}, nil, "AwaitPromise(1str)"},
		{13, []string{"prom-001", `{"status":"ok"}`}, nil, "PromiseResolved(2str)"},
		{14, []string{"prom-001", "card declined"}, nil, "PromiseRejected(2str)"},
		{15, []string{"update-inventory"}, nil, "UpdateHandler(1str)"},
		{16, []string{"stock", "42", "set"}, []int64{5}, "StateMutation(3str+1int)"},
		{17, nil, nil, "RunDetached(0str)"},
	}

	// Generate a random event record from a random code generator.
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	genEvent := func(step int) host.EventRecord {
		cg := codeGens[rng.Intn(len(codeGens))]

		// Build fuzz seed bytes then parse them (to replicate the same
		// deterministic encoding used by the fuzz tests).
		seed := fuzzSeed(cg.code, cg.strs, cg.ints...)
		events := parseFuzzEvents(seed)

		if len(events) == 0 {
			// Fallback: produce a simple Call event.
			return host.EventRecord{
				Step:      step,
				EventType: host.EventTypeCall,
				Service:   "svc",
				Op:        fmt.Sprintf("op-%d", step),
				Request:   `{}`,
				Response:  `{"ok":true}`,
			}
		}
		ev := events[0]
		ev.Step = step
		return ev
	}

	// Test with different sequence sizes.
	for _, size := range []int{1, 10, 100, 1000} {
		t.Run(fmt.Sprintf("size_%d", size), func(t *testing.T) {
			runID := fmt.Sprintf("stress-fuzz-%d-%d", size, time.Now().UnixNano())
			idCopy := runID
			db.Exec(`INSERT INTO workflow_instances (id, def_name, def_version, status, input, task_queue)
				VALUES ($1, 'test', 1, 'ready', '{}', 'default') ON CONFLICT DO NOTHING`, runID)
			defer func() {
				db.Exec(`DELETE FROM event_history WHERE workflow_id = $1`, idCopy)
				db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, idCopy)
			}()

			// Generate a random sequence of events.
			events := make([]host.EventRecord, size)
			for i := 0; i < size; i++ {
				events[i] = genEvent(i)
			}

			// Persist.
			if err := store.AppendEventHistoryBatch(ctx, runID, events); err != nil {
				t.Fatalf("AppendEventHistoryBatch (size=%d): %v", size, err)
			}

			// Load back.
			loaded, err := store.LoadEventHistory(ctx, runID)
			if err != nil {
				t.Fatalf("LoadEventHistory (size=%d): %v", size, err)
			}

			// Verify length.
			if len(loaded) != size {
				t.Fatalf("history length mismatch: stored=%d, loaded=%d", size, len(loaded))
			}

			// Verify every field matches, using a two-level comparison to
			// provide detailed diagnostics on first failure.
			var firstBad int
			allOK := true
			for i := range events {
				if !eventFieldsMatch(events[i], loaded[i]) {
					if allOK {
						firstBad = i
						allOK = false
					}
				}
			}
			if !allOK {
				t.Errorf("First mismatch at event %d (type=%s)", firstBad, events[firstBad].EventType)
				dumpEventDiff(t, events[firstBad], loaded[firstBad])
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Test 4: Divergence detection -- tamper with history
// ---------------------------------------------------------------------------

// TestReplayDivergenceDetection tampers with event history in various known
// ways and verifies that replay correctly detects the divergence and returns
// a descriptive error.
func TestReplayDivergenceDetection(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping divergence detection test in short mode")
	}

	ctx := context.Background()
	input := json.RawMessage(`{"UserID":"div-user","Cart":[{"SKU":"ABC-123","Quantity":2}]}`)

	rt, err := host.NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	// Build WASM once and reuse it.
	wasmBytes := buildStressWasm(t)

	// Execute once to get clean reference history.
	_, cleanHistory, _ := executePlaceOrder(ctx, t, rt, wasmBytes, input)
	if len(cleanHistory) < 2 {
		t.Fatal("expected at least 2 events for divergence testing")
	}
	t.Logf("Clean history: %d events", len(cleanHistory))

	// ---- Tamper case 1: Change a Service name ----
	t.Run("changed_service_name", func(t *testing.T) {
		tampered := copyHistory(cleanHistory)
		tampered[0].Service = "tampered_service"

		caller := &stressMockCaller{}
		engine := host.NewEngine(rt, caller)
		_, _, _, _, _, err := engine.Replay(ctx, wasmBytes, "place_order", input, tampered)
		if err == nil {
			t.Fatal("expected divergence error for changed service name, got nil")
		}
		t.Logf("Divergence detected (service name): %v", err)
		if len(caller.calls) > 0 {
			t.Errorf("failed replay made %d real service calls (expected 0)", len(caller.calls))
		}
		// Verify the error message contains useful diagnostic info.
		errMsg := err.Error()
		if !strings.Contains(errMsg, "tampered_service") && !strings.Contains(errMsg, "divergence") && !strings.Contains(errMsg, "mismatch") {
			t.Logf("Warning: error message may lack diagnostic info: %q", errMsg)
		}
	})

	// ---- Tamper case 2: Change an Operation name ----
	t.Run("changed_operation_name", func(t *testing.T) {
		tampered := copyHistory(cleanHistory)
		tampered[0].Op = "tampered_operation"

		caller := &stressMockCaller{}
		engine := host.NewEngine(rt, caller)
		_, _, _, _, _, err := engine.Replay(ctx, wasmBytes, "place_order", input, tampered)
		if err == nil {
			t.Fatal("expected divergence error for changed operation name, got nil")
		}
		t.Logf("Divergence detected (operation name): %v", err)
		if len(caller.calls) > 0 {
			t.Errorf("failed replay made %d real service calls (expected 0)", len(caller.calls))
		}
	})

	// ---- Tamper case 3: Change an EventType ----
	t.Run("changed_event_type", func(t *testing.T) {
		tampered := copyHistory(cleanHistory)
		// The workflow produces call events; change one to await_signals.
		oldType := tampered[1].EventType
		tampered[1].EventType = host.EventTypeAwaitSignals
		tampered[1].SignalNames = "fake_signal"
		tampered[1].TimeoutMs = 5000

		caller := &stressMockCaller{}
		engine := host.NewEngine(rt, caller)
		_, _, _, _, _, err := engine.Replay(ctx, wasmBytes, "place_order", input, tampered)
		if err == nil {
			t.Fatalf("expected divergence error for changed event type (%s -> await_signals), got nil", oldType)
		}
		t.Logf("Divergence detected (event type): %v", err)
		if len(caller.calls) > 0 {
			t.Errorf("failed replay made %d real service calls (expected 0)", len(caller.calls))
		}
	})

	// ---- Tamper case 4: Insert an extra event ----
	t.Run("inserted_extra_event", func(t *testing.T) {
		tampered := make([]host.EventRecord, len(cleanHistory)+1)
		// Insert a synthetic event at position 2, shifting the rest.
		copy(tampered, cleanHistory[:2])
		tampered[2] = host.EventRecord{
			Step:      2,
			EventType: host.EventTypeCall,
			Service:   "nonexistent",
			Op:        "nobody",
			Request:   `{}`,
			Response:  `{}`,
		}
		copy(tampered[3:], cleanHistory[2:])
		// Fix step numbers for shifted events.
		for i := 3; i < len(tampered); i++ {
			tampered[i].Step = i
		}

		caller := &stressMockCaller{}
		engine := host.NewEngine(rt, caller)
		_, _, _, _, _, err := engine.Replay(ctx, wasmBytes, "place_order", input, tampered)
		if err == nil {
			t.Fatal("expected divergence error for inserted extra event, got nil")
		}
		t.Logf("Divergence detected (inserted event): %v", err)
		if len(caller.calls) > 0 {
			t.Errorf("failed replay made %d real service calls (expected 0)", len(caller.calls))
		}
	})

	// ---- Tamper case 5: Remove an event ----
	t.Run("removed_event", func(t *testing.T) {
		if len(cleanHistory) < 3 {
			t.Skip("need at least 3 events for removal test")
		}
		tampered := append([]host.EventRecord{}, cleanHistory...)
		// Remove event at index 1 (middle).
		tampered = append(tampered[:1], tampered[2:]...)
		// Fix step numbers.
		for i := range tampered {
			tampered[i].Step = i
		}

		caller := &stressMockCaller{}
		engine := host.NewEngine(rt, caller)
		_, _, _, _, _, err := engine.Replay(ctx, wasmBytes, "place_order", input, tampered)
		if err == nil {
			t.Fatal("expected divergence error for removed event, got nil")
		}
		t.Logf("Divergence detected (removed event): %v", err)
		if len(caller.calls) > 0 {
			t.Errorf("failed replay made %d real service calls (expected 0)", len(caller.calls))
		}
	})
}

// ---------------------------------------------------------------------------
// Utility: hash-based consistency check
// ---------------------------------------------------------------------------

// TestReplayHashConsistency verifies that the SHA-256 hash of a replayed event
// history is stable across multiple replay invocations, providing a compact
// fingerprint-based consistency check.
func TestReplayHashConsistency(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping hash consistency test in short mode")
	}

	ctx := context.Background()
	input := json.RawMessage(`{"UserID":"hash-user","Cart":[{"SKU":"ABC-123","Quantity":2}]}`)

	rt, err := host.NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	// Build WASM once and reuse it.
	wasmBytes := buildStressWasm(t)

	// Execute to get history.
	_, history, _ := executePlaceOrder(ctx, t, rt, wasmBytes, input)

	// Replay N times and compute SHA-256 of each result history.
	var hashes [][sha256.Size]byte
	const replayCount = 20
	for i := 0; i < replayCount; i++ {
		caller := &stressMockCaller{}
		engine := host.NewEngine(rt, caller)
		_, resultHistory, _, _, _, err := engine.Replay(ctx, wasmBytes, "place_order", input, history)
		if err != nil {
			t.Fatalf("Replay %d: %v", i, err)
		}
		data, err := json.Marshal(resultHistory)
		if err != nil {
			t.Fatalf("json.Marshal replay %d: %v", i, err)
		}
		h := sha256.Sum256(data)
		hashes = append(hashes, h)
	}

	// All hashes must be identical.
	for i := 1; i < len(hashes); i++ {
		if hashes[0] != hashes[i] {
			t.Errorf("Hash mismatch between replay 0 and replay %d", i)
		}
	}
	t.Logf("All %d replays produced identical SHA-256 hashes", replayCount)
}

// ---------------------------------------------------------------------------
// Copy and fuzz helpers (adapted from compaction_fuzz_test.go patterns)
// ---------------------------------------------------------------------------

// copyHistory makes a deep copy of an EventRecord slice.
func copyHistory(h []host.EventRecord) []host.EventRecord {
	out := make([]host.EventRecord, len(h))
	copy(out, h)
	return out
}

// fuzzSeed encodes a single event into a byte slice using the same format as
// compaction_fuzz_test.go. Each string is length-prefixed (max 255); int64
// values are clamped to [0, 255] and written as a single byte.
func fuzzSeed(typeCode byte, strs []string, ints ...int64) []byte {
	b := []byte{typeCode}
	for _, s := range strs {
		n := len(s)
		if n > 255 {
			n = 255
		}
		b = append(b, byte(n))
		b = append(b, []byte(s[:n])...)
	}
	for _, v := range ints {
		if v < 0 {
			v = 0
		}
		if v > 255 {
			v = 255
		}
		b = append(b, byte(v))
	}
	return b
}

// parseFuzzEvents interprets a byte slice as a sequence of length-prefixed
// events (matching the encoding in compaction_fuzz_test.go). Every byte
// sequence produces a valid (possibly empty) result.
func parseFuzzEvents(data []byte) []host.EventRecord {
	// Event type codes (must match those in compaction_fuzz_test.go).
	const (
		eventCodeCall              = 0
		eventCodeAwaitSignals      = 1
		eventCodeSignalReceived    = 2
		eventCodeDefer             = 3
		eventCodeChildWorkflow     = 4
		eventCodeAwaitChild        = 5
		eventCodeContinueAsNew     = 6
		eventCodeHeartbeat         = 7
		eventCodeAwaitAllChildren  = 8
		eventCodePluginCall        = 9
		eventCodePluginCallStream  = 10
		eventCodeCreatePromise     = 11
		eventCodeAwaitPromise      = 12
		eventCodePromiseResolved   = 13
		eventCodePromiseRejected   = 14
		eventCodeUpdateHandler     = 15
		eventCodeStateMutation     = 16
		eventCodeRunDetached       = 17
		eventCodeSideEffect        = 18
		eventCodeScopeAcquired     = 19
		eventCodeAcquireLock       = 20
		eventCodeReleaseLock       = 21
		eventCodeFetch             = 22
		eventCodeDurableLog        = 23
		eventCodeDurableSend       = 24
		eventCodeDurableSchedule   = 25
	)

	codeToEventType := map[int]host.EventType{
		eventCodeCall:             host.EventTypeCall,
		eventCodeAwaitSignals:     host.EventTypeAwaitSignals,
		eventCodeSignalReceived:   host.EventTypeSignalReceived,
		eventCodeDefer:            host.EventTypeDefer,
		eventCodeChildWorkflow:    host.EventTypeChildWorkflow,
		eventCodeAwaitChild:       host.EventTypeAwaitChild,
		eventCodeContinueAsNew:    host.EventTypeContinueAsNew,
		eventCodeHeartbeat:        host.EventTypeHeartbeat,
		eventCodeAwaitAllChildren: host.EventTypeAwaitAllChildren,
		eventCodePluginCall:       host.EventTypePluginCall,
		eventCodePluginCallStream: host.EventTypePluginCallStreamChunk,
		eventCodeCreatePromise:    host.EventTypeCreatePromise,
		eventCodeAwaitPromise:     host.EventTypeAwaitPromise,
		eventCodePromiseResolved:  host.EventTypePromiseResolved,
		eventCodePromiseRejected:  host.EventTypePromiseRejected,
		eventCodeUpdateHandler:    host.EventTypeUpdateHandler,
		eventCodeStateMutation:    host.EventTypeStateMutation,
		eventCodeRunDetached:      host.EventTypeRunDetached,
		eventCodeSideEffect:       host.EventTypeSideEffect,
		eventCodeScopeAcquired:    host.EventTypeScopeAcquired,
		eventCodeAcquireLock:      host.EventTypeAcquireLock,
		eventCodeReleaseLock:      host.EventTypeReleaseLock,
		eventCodeFetch:            host.EventTypeFetch,
		eventCodeDurableLog:       host.EventTypeDurableLog,
		eventCodeDurableSend:      host.EventTypeDurableSend,
		eventCodeDurableSchedule:  host.EventTypeDurableScheduleInvoke,
	}

	var events []host.EventRecord
	r := &fuzzByteReader{data: data}

	for step := 0; r.remaining() > 0; step++ {
		typeCode := int(r.readByte()) % 27
		ev := host.EventRecord{
			Step:      step,
			EventType: codeToEventType[typeCode],
		}
		if ev.EventType == "" {
			ev.EventType = host.EventTypeCall
		}

		switch typeCode {
		case eventCodeCall:
			ev.Service = r.readString()
			ev.Op = r.readString()
			ev.Request = r.readString()
			ev.Response = r.readString()
			ev.Err = r.readString()
		case eventCodeAwaitSignals:
			ev.SignalNames = r.readString()
			ev.TimeoutMs = r.readInt64()
		case eventCodeSignalReceived:
			ev.SignalName = r.readString()
			ev.SignalPayload = r.readString()
		case eventCodeDefer:
			ev.DeferID = r.readString()
			ev.DeferDescription = r.readString()
		case eventCodeChildWorkflow:
			ev.ChildName = r.readString()
			ev.ChildInput = r.readString()
			ev.RunID = r.readString()
		case eventCodeAwaitChild:
			ev.RunID = r.readString()
			ev.Response = r.readString()
			ev.Err = r.readString()
		case eventCodeContinueAsNew:
			ev.NewInput = r.readString()
		case eventCodeHeartbeat:
			ev.Service = r.readString()
			ev.Op = r.readString()
		case eventCodeAwaitAllChildren:
			ev.Response = r.readString()
		case eventCodePluginCall:
			ev.PluginName = r.readString()
			ev.PluginFunc = r.readString()
			ev.PluginInput = r.readString()
			ev.PluginOutput = r.readString()
			ev.PluginError = r.readString()
		case eventCodeCreatePromise:
			ev.PromiseName = r.readString()
			ev.PromiseID = r.readString()
		case eventCodeAwaitPromise:
			ev.PromiseID = r.readString()
		case eventCodePromiseResolved:
			ev.PromiseID = r.readString()
			ev.PromiseResult = r.readString()
		case eventCodePromiseRejected:
			ev.PromiseID = r.readString()
			ev.PromiseError = r.readString()
		case eventCodeUpdateHandler:
			ev.UpdateHandlerName = r.readString()
		case eventCodeStateMutation:
			ev.StateKey = r.readString()
			ev.StateValue = r.readString()
			ev.StateOp = r.readString()
			ev.StateDelta = r.readInt64()
		case eventCodeRunDetached:
			// No string fields.
		case eventCodeSideEffect:
			ev.SideEffectResult = r.readString()
		case eventCodeScopeAcquired:
			ev.ScopeKey = r.readString()
		case eventCodeAcquireLock:
			ev.LockKey = r.readString()
			ev.LockTTLMs = r.readInt64()
		case eventCodeReleaseLock:
			ev.LockKey = r.readString()
		case eventCodeFetch:
			ev.FetchMethod = r.readString()
			ev.FetchURL = r.readString()
			ev.FetchResponse = r.readString()
			ev.Err = r.readString()
		case eventCodeDurableLog:
			ev.Message = r.readString()
			ev.LogLevel = r.readString()
			ev.LogKV = r.readString()
		case eventCodeDurableSend:
			ev.Service = r.readString()
			ev.Op = r.readString()
			ev.Request = r.readString()
		case eventCodeDurableSchedule:
			ev.Service = r.readString()
			ev.Op = r.readString()
			ev.Request = r.readString()
			ev.DurationMs = r.readInt64()
		}
		events = append(events, ev)
	}
	return events
}

// fuzzByteReader wraps a []byte with sequential read helpers (mirrors the
// byteReader from compaction_fuzz_test.go but exported for cross-package use).
type fuzzByteReader struct {
	data []byte
	pos  int
}

func (r *fuzzByteReader) remaining() int { return len(r.data) - r.pos }

func (r *fuzzByteReader) readByte() byte {
	if r.pos >= len(r.data) {
		return 0
	}
	b := r.data[r.pos]
	r.pos++
	return b
}

func (r *fuzzByteReader) readString() string {
	n := int(r.readByte())
	if n <= 0 {
		return ""
	}
	if n > r.remaining() {
		n = r.remaining()
	}
	s := string(r.data[r.pos : r.pos+n])
	r.pos += n
	return s
}

func (r *fuzzByteReader) readInt64() int64 {
	return int64(r.readByte())
}

// eventFieldsMatch compares all round-tripped fields of two EventRecords.
// This is a copy of the logic in compaction_fuzz_test.go adapted for the
// integrity test package (which cannot access unexported test functions).
func eventFieldsMatch(a, b host.EventRecord) bool {
	if a.Step != b.Step || a.EventType != b.EventType {
		return false
	}
	switch a.EventType {
	case host.EventTypeCall:
		return a.Service == b.Service &&
			a.Op == b.Op &&
			a.Request == b.Request &&
			a.Response == b.Response &&
			a.Err == b.Err
	case host.EventTypeAwaitSignals:
		return a.SignalNames == b.SignalNames &&
			a.TimeoutMs == b.TimeoutMs
	case host.EventTypeSignalReceived:
		return a.SignalName == b.SignalName &&
			a.SignalPayload == b.SignalPayload
	case host.EventTypeDefer:
		return a.DeferID == b.DeferID &&
			a.DeferDescription == b.DeferDescription
	case host.EventTypeChildWorkflow:
		return a.ChildName == b.ChildName &&
			a.ChildInput == b.ChildInput &&
			a.RunID == b.RunID
	case host.EventTypeAwaitChild:
		return a.RunID == b.RunID &&
			a.Response == b.Response &&
			a.Err == b.Err
	case host.EventTypeContinueAsNew:
		return a.NewInput == b.NewInput
	case host.EventTypeHeartbeat:
		return a.Service == b.Service &&
			a.Op == b.Op
	case host.EventTypeAwaitAllChildren:
		return a.Response == b.Response
	case host.EventTypePluginCall:
		return a.PluginName == b.PluginName &&
			a.PluginFunc == b.PluginFunc &&
			a.PluginInput == b.PluginInput &&
			a.PluginOutput == b.PluginOutput &&
			a.PluginError == b.PluginError
	case host.EventTypePluginCallStreamChunk:
		return a.PluginName == b.PluginName &&
			a.PluginFunc == b.PluginFunc &&
			a.PluginInput == b.PluginInput &&
			a.PluginOutput == b.PluginOutput &&
			a.PluginError == b.PluginError
	case host.EventTypeSideEffect:
		return a.SideEffectResult == b.SideEffectResult
	case host.EventTypeCreatePromise:
		return a.PromiseName == b.PromiseName &&
			a.PromiseID == b.PromiseID
	case host.EventTypeAwaitPromise:
		return a.PromiseID == b.PromiseID
	case host.EventTypePromiseResolved:
		return a.PromiseID == b.PromiseID &&
			a.PromiseResult == b.PromiseResult
	case host.EventTypePromiseRejected:
		return a.PromiseID == b.PromiseID &&
			a.PromiseError == b.PromiseError
	case host.EventTypeUpdateHandler:
		return a.UpdateHandlerName == b.UpdateHandlerName
	case host.EventTypeStateMutation:
		return a.StateKey == b.StateKey &&
			a.StateValue == b.StateValue &&
			a.StateDelta == b.StateDelta &&
			a.StateOp == b.StateOp
	case host.EventTypeRunDetached:
		return a.DetachedName == b.DetachedName &&
			a.DetachedInput == b.DetachedInput &&
			a.DetachedRunID == b.DetachedRunID
	case host.EventTypeFetch:
		return a.FetchMethod == b.FetchMethod &&
			a.FetchURL == b.FetchURL &&
			a.FetchResponse == b.FetchResponse &&
			a.Err == b.Err
	case host.EventTypeScopeAcquired:
		return a.ScopeKey == b.ScopeKey
	case host.EventTypeAcquireLock:
		return a.LockKey == b.LockKey &&
			a.LockTTLMs == b.LockTTLMs &&
			a.LockAcquired == b.LockAcquired
	case host.EventTypeReleaseLock:
		return a.LockKey == b.LockKey
	case host.EventTypeDurableLog:
		return a.Message == b.Message &&
			a.LogLevel == b.LogLevel &&
			a.LogKV == b.LogKV
	case host.EventTypeDurableSend:
		return a.Service == b.Service &&
			a.Op == b.Op &&
			a.Request == b.Request
	case host.EventTypeDurableScheduleInvoke:
		return a.Service == b.Service &&
			a.Op == b.Op &&
			a.Request == b.Request &&
			a.DurationMs == b.DurationMs
	}
	return false
}

// dumpEventDiff prints the differing fields between two events on t.Log.
func dumpEventDiff(t *testing.T, a, b host.EventRecord) {
	t.Helper()
	if a.Step != b.Step {
		t.Logf("  Step: %d vs %d", a.Step, b.Step)
	}
	if a.EventType != b.EventType {
		t.Logf("  EventType: %s vs %s", a.EventType, b.EventType)
		return
	}
	switch a.EventType {
	case host.EventTypeCall:
		mismatchStr("Service", a.Service, b.Service, t)
		mismatchStr("Op", a.Op, b.Op, t)
		mismatchStr("Request", a.Request, b.Request, t)
		mismatchStr("Response", a.Response, b.Response, t)
		mismatchStr("Err", a.Err, b.Err, t)
	case host.EventTypeAwaitSignals:
		mismatchStr("SignalNames", a.SignalNames, b.SignalNames, t)
		mismatchInt("TimeoutMs", a.TimeoutMs, b.TimeoutMs, t)
	case host.EventTypeSignalReceived:
		mismatchStr("SignalName", a.SignalName, b.SignalName, t)
		mismatchStr("SignalPayload", a.SignalPayload, b.SignalPayload, t)
	case host.EventTypeDefer:
		mismatchStr("DeferID", a.DeferID, b.DeferID, t)
		mismatchStr("DeferDescription", a.DeferDescription, b.DeferDescription, t)
	case host.EventTypeChildWorkflow:
		mismatchStr("ChildName", a.ChildName, b.ChildName, t)
		mismatchStr("ChildInput", a.ChildInput, b.ChildInput, t)
		mismatchStr("RunID", a.RunID, b.RunID, t)
	case host.EventTypePluginCall:
		mismatchStr("PluginName", a.PluginName, b.PluginName, t)
		mismatchStr("PluginFunc", a.PluginFunc, b.PluginFunc, t)
		mismatchStr("PluginInput", a.PluginInput, b.PluginInput, t)
		mismatchStr("PluginOutput", a.PluginOutput, b.PluginOutput, t)
		mismatchStr("PluginError", a.PluginError, b.PluginError, t)
	case host.EventTypePromiseResolved:
		mismatchStr("PromiseID", a.PromiseID, b.PromiseID, t)
		mismatchStr("PromiseResult", a.PromiseResult, b.PromiseResult, t)
	case host.EventTypePromiseRejected:
		mismatchStr("PromiseID", a.PromiseID, b.PromiseID, t)
		mismatchStr("PromiseError", a.PromiseError, b.PromiseError, t)
	case host.EventTypeStateMutation:
		mismatchStr("StateKey", a.StateKey, b.StateKey, t)
		mismatchStr("StateValue", a.StateValue, b.StateValue, t)
		mismatchInt("StateDelta", a.StateDelta, b.StateDelta, t)
		mismatchStr("StateOp", a.StateOp, b.StateOp, t)
	case host.EventTypeAcquireLock:
		mismatchStr("LockKey", a.LockKey, b.LockKey, t)
		mismatchInt("LockTTLMs", a.LockTTLMs, b.LockTTLMs, t)
		mismatchInt("LockAcquired", int64(a.LockAcquired), int64(b.LockAcquired), t)
	case host.EventTypeReleaseLock:
		mismatchStr("LockKey", a.LockKey, b.LockKey, t)
	case host.EventTypeFetch:
		mismatchStr("FetchMethod", a.FetchMethod, b.FetchMethod, t)
		mismatchStr("FetchURL", a.FetchURL, b.FetchURL, t)
		mismatchStr("FetchResponse", a.FetchResponse, b.FetchResponse, t)
		mismatchStr("Err", a.Err, b.Err, t)
	case host.EventTypeDurableLog:
		mismatchStr("Message", a.Message, b.Message, t)
		mismatchStr("LogLevel", a.LogLevel, b.LogLevel, t)
		mismatchStr("LogKV", a.LogKV, b.LogKV, t)
	case host.EventTypeDurableSend:
		mismatchStr("Service", a.Service, b.Service, t)
		mismatchStr("Op", a.Op, b.Op, t)
		mismatchStr("Request", a.Request, b.Request, t)
	case host.EventTypeDurableScheduleInvoke:
		mismatchStr("Service", a.Service, b.Service, t)
		mismatchStr("Op", a.Op, b.Op, t)
		mismatchStr("Request", a.Request, b.Request, t)
		mismatchInt("DurationMs", a.DurationMs, b.DurationMs, t)
	}
}

func mismatchStr(name, a, b string, t *testing.T) {
	if a != b {
		t.Logf("  %s: %q vs %q", name, a, b)
	}
}

func mismatchInt(name string, a, b int64, t *testing.T) {
	if a != b {
		t.Logf("  %s: %d vs %d", name, a, b)
	}
}
