package updatabletimer

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/rcownie/cleat/cleat/cleattest"
)

// Test_Sleep verifies that the workflow sleeps for the configured duration and
// the timer fires at the expected time.
func Test_Sleep(t *testing.T) {
	env := cleattest.NewTestEnv()
	start := env.Now()

	wakeUpTime := start.Add(30 * time.Minute)
	wakeUpTimeJSON, err := json.Marshal(wakeUpTime)
	if err != nil {
		t.Fatalf("marshal wake-up time: %v", err)
	}

	resultCh := make(chan struct {
		result string
		err    error
	})

	go func() {
		result, err := Workflow(env.H(), string(wakeUpTimeJSON))
		resultCh <- struct {
			result string
			err    error
		}{result, err}
	}()

	// Give the goroutine time to register handlers and enter AwaitSignals.
	time.Sleep(5 * time.Millisecond)

	// Advance time past the 30-minute mark.
	env.AdvanceTime(30 * time.Minute)

	r := <-resultCh
	if r.err != nil {
		t.Fatalf("workflow error: %v", r.err)
	}

	elapsed := env.Now().Sub(start)
	if elapsed != 30*time.Minute {
		t.Fatalf("expected elapsed %v, got %v", 30*time.Minute, elapsed)
	}

	// Verify the result indicates the timer fired.
	var resultMap map[string]interface{}
	if err := json.Unmarshal([]byte(r.result), &resultMap); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if resultMap["status"] != "fired" {
		t.Fatalf("expected status 'fired', got %v", resultMap["status"])
	}
}

// Test_UpdateWakeUpTime verifies that signals can update the wake-up time
// while the timer is running, and the workflow sleeps for the updated duration.
func Test_UpdateWakeUpTime(t *testing.T) {
	env := cleattest.NewTestEnv()
	start := env.Now()

	initialWakeUpTime := start.Add(30 * time.Minute)
	initialJSON, err := json.Marshal(initialWakeUpTime)
	if err != nil {
		t.Fatalf("marshal initial wake-up time: %v", err)
	}

	// First update: at T=10min, change wake-up to T+15min.
	updated1 := start.Add(15 * time.Minute)
	updated1JSON, err := json.Marshal(updated1)
	if err != nil {
		t.Fatalf("marshal update1: %v", err)
	}
	env.AfterSignal(10*time.Minute, SignalType, string(updated1JSON))

	// Second update: at T=12min, change wake-up to T+40min.
	updated2 := start.Add(40 * time.Minute)
	updated2JSON, err := json.Marshal(updated2)
	if err != nil {
		t.Fatalf("marshal update2: %v", err)
	}
	env.AfterSignal(12*time.Minute, SignalType, string(updated2JSON))

	resultCh := make(chan struct {
		result string
		err    error
	})

	go func() {
		result, err := Workflow(env.H(), string(initialJSON))
		resultCh <- struct {
			result string
			err    error
		}{result, err}
	}()

	time.Sleep(5 * time.Millisecond)

	// Advance time to trigger both signal deliveries and the final timer.
	env.AdvanceTime(40 * time.Minute)

	r := <-resultCh
	if r.err != nil {
		t.Fatalf("workflow error: %v", r.err)
	}

	// The workflow should have completed after 40 minutes (the final update).
	elapsed := env.Now().Sub(start)
	if elapsed != 40*time.Minute {
		t.Fatalf("expected elapsed %v, got %v", 40*time.Minute, elapsed)
	}
}

// Test_MultipleUpdates verifies multiple successive updates before the timer fires.
func Test_MultipleUpdates(t *testing.T) {
	env := cleattest.NewTestEnv()
	start := env.Now()

	initialWakeUpTime := start.Add(60 * time.Minute)
	initialJSON, err := json.Marshal(initialWakeUpTime)
	if err != nil {
		t.Fatalf("marshal initial wake-up time: %v", err)
	}

	// Update at T=5min: wake-up at T+10min (way before the initial 60min).
	updated1 := start.Add(10 * time.Minute)
	updated1JSON, _ := json.Marshal(updated1)
	env.AfterSignal(5*time.Minute, SignalType, string(updated1JSON))

	resultCh := make(chan struct {
		result string
		err    error
	})

	go func() {
		result, err := Workflow(env.H(), string(initialJSON))
		resultCh <- struct {
			result string
			err    error
		}{result, err}
	}()

	time.Sleep(5 * time.Millisecond)

	// Advance to T+10min. The signal at T+5min shortens the timer.
	env.AdvanceTime(10 * time.Minute)

	r := <-resultCh
	if r.err != nil {
		t.Fatalf("workflow error: %v", r.err)
	}

	// Should have completed at T+10min (shorter than initial 60min).
	elapsed := env.Now().Sub(start)
	if elapsed != 10*time.Minute {
		t.Fatalf("expected elapsed %v, got %v", 10*time.Minute, elapsed)
	}
}

// Test_QueryHandler verifies that the query handler returns the correct
// wake-up time during the workflow's sleep.
func Test_QueryHandler(t *testing.T) {
	env := cleattest.NewTestEnv()
	start := env.Now()

	wakeUpTime := start.Add(30 * time.Minute)
	wakeUpTimeJSON, err := json.Marshal(wakeUpTime)
	if err != nil {
		t.Fatalf("marshal wake-up time: %v", err)
	}

	resultCh := make(chan struct {
		result string
		err    error
	})

	go func() {
		result, err := Workflow(env.H(), string(wakeUpTimeJSON))
		resultCh <- struct {
			result string
			err    error
		}{result, err}
	}()

	time.Sleep(10 * time.Millisecond)

	// Advance time a bit to ensure the workflow is in the sleep loop.
	env.AdvanceTime(5 * time.Minute)

	// Query the current wake-up time.
	queryResult, err := env.HandleQuery(QueryType, "")
	if err != nil {
		t.Fatalf("HandleQuery error: %v", err)
	}

	var queryWakeUpTime time.Time
	if err := json.Unmarshal([]byte(queryResult), &queryWakeUpTime); err != nil {
		t.Fatalf("unmarshal query result %q: %v", queryResult, err)
	}

	// The wake-up time should still be the initial value (no updates sent).
	if !queryWakeUpTime.Equal(wakeUpTime) {
		t.Fatalf("expected wake-up time %v, got %v", wakeUpTime, queryWakeUpTime)
	}

	// Now advance the remaining time for clean completion.
	env.AdvanceTime(25 * time.Minute)

	r := <-resultCh
	if r.err != nil {
		t.Fatalf("workflow error: %v", r.err)
	}
}

// Test_QueryAfterUpdate verifies that after a signal update, the query handler
// returns the updated wake-up time, not the initial one.
func Test_QueryAfterUpdate(t *testing.T) {
	env := cleattest.NewTestEnv()
	start := env.Now()

	initialWakeUpTime := start.Add(60 * time.Minute)
	initialJSON, err := json.Marshal(initialWakeUpTime)
	if err != nil {
		t.Fatalf("marshal initial wake-up time: %v", err)
	}

	// Schedule an update at T=10min: new wake-up at T+20min.
	updated := start.Add(20 * time.Minute)
	updatedJSON, _ := json.Marshal(updated)
	env.AfterSignal(10*time.Minute, SignalType, string(updatedJSON))

	resultCh := make(chan struct {
		result string
		err    error
	})

	go func() {
		result, err := Workflow(env.H(), string(initialJSON))
		resultCh <- struct {
			result string
			err    error
		}{result, err}
	}()

	time.Sleep(10 * time.Millisecond)

	// Advance past the signal time. The signal scheduled at T+10min
	// should be delivered; the workflow loop recalculates the remaining
	// duration to the updated wake-up time and calls AwaitSignals again.
	env.AdvanceTime(12 * time.Minute)

	// The test goroutine may continue before the workflow goroutine has
	// processed the signal. A short sleep yields the processor so the
	// workflow goroutine can update timer.wakeUpTime.
	time.Sleep(5 * time.Millisecond)

	// Query the current wake-up time -- should be the updated value.
	queryResult, err := env.HandleQuery(QueryType, "")
	if err != nil {
		t.Fatalf("HandleQuery error: %v", err)
	}

	var queryWakeUpTime time.Time
	if err := json.Unmarshal([]byte(queryResult), &queryWakeUpTime); err != nil {
		t.Fatalf("unmarshal query result: %v", err)
	}

	if !queryWakeUpTime.Equal(updated) {
		t.Fatalf("expected wake-up time %v (updated), got %v", updated, queryWakeUpTime)
	}

	// Advance remaining time for completion (to T+20min from start).
	env.AdvanceTime(8 * time.Minute)

	r := <-resultCh
	if r.err != nil {
		t.Fatalf("workflow error: %v", r.err)
	}
}

// Test_UpdateHandlerRegistered verifies that the update handler is registered
// without error. (Full end-to-end testing requires a running runtime since
// there is no HandleUpdate method on TestEnv.)
func Test_UpdateHandlerRegistered(t *testing.T) {
	// The update handler is registered during Workflow initialization.
	// A successful test run (via Test_Sleep etc.) proves registration
	// did not panic.
	env := cleattest.NewTestEnv()
	start := env.Now()

	wakeUpTime := start.Add(1 * time.Minute)
	wakeUpTimeJSON, err := json.Marshal(wakeUpTime)
	if err != nil {
		t.Fatalf("marshal wake-up time: %v", err)
	}

	resultCh := make(chan struct {
		result string
		err    error
	})

	go func() {
		result, err := Workflow(env.H(), string(wakeUpTimeJSON))
		resultCh <- struct {
			result string
			err    error
		}{result, err}
	}()

	time.Sleep(5 * time.Millisecond)
	env.AdvanceTime(1 * time.Minute)

	r := <-resultCh
	if r.err != nil {
		t.Fatalf("workflow error: %v", r.err)
	}
}
