// Package updatabletimer ports the Temporal updatable-timer sample to the
// Cleat Go SDK (github.com/rcownie/durable).
//
// It demonstrates a timer that can be dynamically updated while sleeping.
// The workflow starts with a long timer, but external signals can change the
// wake-up time. The pattern uses AwaitSignals with a calculated timeout to
// implement "sleep until X or until a signal arrives."
package updatabletimer

import (
	"encoding/json"
	"time"

	"github.com/rcownie/durable/durable"
)

// ---- Constants ----

const (
	// QueryType is the name of the query handler that returns the current
	// wake-up time.
	QueryType = "GetWakeUpTime"

	// SignalType is the name of the signal used to update the wake-up time.
	// The payload must be a JSON-encoded time.Time.
	SignalType = "UpdateWakeUpTime"

	// UpdateType is the name of the update handler (an alternative to signals)
	// for changing the wake-up time. The payload must be a JSON-encoded time.Time.
	UpdateType = "UpdateWakeUpTime"
)

// ---- Data structures ----

// UpdatableTimer is a helper that implements a durable, updatable timer.
// The timer can have its wake-up time changed via external signals or
// update handlers.
type UpdatableTimer struct {
	wakeUpTime time.Time
}

// NewUpdatableTimer creates a new UpdatableTimer with the given initial
// wake-up time.
func NewUpdatableTimer(initialWakeUpTime time.Time) *UpdatableTimer {
	return &UpdatableTimer{
		wakeUpTime: initialWakeUpTime,
	}
}

// SleepUntil blocks until the configured wake-up time is reached, or until
// a signal arrives with a new wake-up time. Returns nil when the timer fires.
// The implementation uses a loop that calls AwaitSignals with the remaining
// duration, which avoids the history-bloat problem of polling-based approaches.
//
// Key difference from Temporal: Cleat doesn't have a unified Selector that
// races a timer against a channel. Instead, AwaitSignals with a timeout value
// provides the same semantics: "block until signal OR timeout."
func (u *UpdatableTimer) SleepUntil(h durable.HostCalls) error {
	for {
		remaining := u.wakeUpTime.Sub(h.Now())
		if remaining <= 0 {
			h.DurableLog("UpdatableTimer: timer fired at " + h.Now().Format(time.RFC3339))
			return nil
		}

		h.LogKV("UpdatableTimer: sleeping",
			"wake_up_time", u.wakeUpTime.Format(time.RFC3339),
			"remaining_ms", remaining.Milliseconds())

		// Block until either a signal arrives or the remaining duration elapses.
		// This is equivalent to Temporal's Selector{AddFuture(timer)+AddReceive(ch)}.Select().
		result := h.AwaitSignals([]string{SignalType}, remaining)

		if result.Err != nil {
			return result.Err
		}
		if result.TimedOut {
			// No signal arrived --- the timer fired.
			h.DurableLog("UpdatableTimer: timer fired (timeout)")
			return nil
		}

		// Check for cancellation before processing the signal.
		cancelled, reason := h.PollCancellation()
		if cancelled {
			h.DurableLog("UpdatableTimer: cancelled: " + reason)
			return nil
		}

		// Parse the new wake-up time from the signal payload.
		var newWakeUpTime time.Time
		if err := json.Unmarshal([]byte(result.Payload), &newWakeUpTime); err != nil {
			h.DurableLog("UpdatableTimer: failed to parse wake-up time from signal: " + err.Error())
			// Continue with the current wake-up time.
			continue
		}
		u.wakeUpTime = newWakeUpTime
		h.LogKV("UpdatableTimer: wake-up time updated",
			"new_wake_up_time", u.wakeUpTime.Format(time.RFC3339))
	}
}

// GetWakeUpTime returns the current wake-up time.
func (u *UpdatableTimer) GetWakeUpTime() time.Time {
	return u.wakeUpTime
}

// Workflow sleeps until the given initialWakeUpTime (a JSON-encoded time.Time),
// unless an external "UpdateWakeUpTime" signal or update handler changes the
// wake-up time. The workflow also exposes a "GetWakeUpTime" query handler.
//
// This function is the main workflow entry point, following the Cleat SDK
// convention of a function that receives a HostCalls and a JSON input string.
func Workflow(h durable.HostCalls, initialWakeUpTimeJSON string) (string, error) {
	var initialWakeUpTime time.Time
	if err := json.Unmarshal([]byte(initialWakeUpTimeJSON), &initialWakeUpTime); err != nil {
		return "", err
	}

	timer := NewUpdatableTimer(initialWakeUpTime)

	// ---- Register query handler ----
	// External callers can invoke this via the runtime's query API.
	h.RegisterQueryHandler(QueryType, func(payloadJSON string) (string, error) {
		result, err := json.Marshal(timer.GetWakeUpTime())
		if err != nil {
			return "", err
		}
		return string(result), nil
	})

	// ---- Register update handler ----
	// This provides an alternative to signals for updating the timer.
	// The update handler runs inline when an external caller sends an update,
	// modifying the wake-up time. Note: unlike signals, update handlers do NOT
	// interrupt a blocking AwaitSignals call --- they only take effect on the
	// next loop iteration after AwaitSignals returns.
	h.RegisterUpdateHandler(UpdateType,
		// Handler: process the update payload.
		func(payloadJSON string) (string, error) {
			var newWakeUpTime time.Time
			if err := json.Unmarshal([]byte(payloadJSON), &newWakeUpTime); err != nil {
				return "", err
			}
			timer.wakeUpTime = newWakeUpTime
			h.DurableLog("Update handler: wake-up time changed to " + newWakeUpTime.Format(time.RFC3339))
			return payloadJSON, nil
		},
		// Validator: ensure the payload is a valid time.
		func(payloadJSON string) error {
			var newWakeUpTime time.Time
			return json.Unmarshal([]byte(payloadJSON), &newWakeUpTime)
		},
	)

	// Expose the initial wake-up time as queryable state.
	h.SetQueryState("wake_up_time", initialWakeUpTime.Format(time.RFC3339))
	h.SetQueryState("status", "sleeping")

	// ---- Main sleep loop ----
	if err := timer.SleepUntil(h); err != nil {
		h.SetQueryState("status", "error")
		h.SetQueryState("error", err.Error())
		return "", err
	}

	// ---- Timer fired ----
	h.SetQueryState("status", "fired")
	h.SetQueryState("fired_at", h.Now().Format(time.RFC3339))

	result, err := json.Marshal(map[string]interface{}{
		"status":       "fired",
		"fired_at":     h.Now().Format(time.RFC3339),
		"wake_up_time": timer.GetWakeUpTime().Format(time.RFC3339),
	})
	if err != nil {
		return "", err
	}

	h.DurableLog("Workflow completed. Timer fired at " + h.Now().Format(time.RFC3339))
	return string(result), nil
}
