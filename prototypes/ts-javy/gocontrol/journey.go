// Package gocontrol is the Go twin of the TypeScript `journey` and `sleeper` workflows, built only to answer one question
// (PHASE2.md): does the engine log "replay history is shorter than the instance's recorded event count" for the same
// sleep -> signal -> child sequence when the guest is Go? Same steps, same durations, same inputs.
package gocontrol

import (
	"encoding/json"
	"math"
	"time"

	"github.com/cleat-team/cleat/cleat"
)

type JourneyInput struct {
	SleepMs int `json:"sleepMs"`
	ChildMs int `json:"childMs"`
}

type SleeperInput struct {
	Ms int `json:"ms"`
}

// GoJourney sleeps, waits for the signal "go", starts a child that itself sleeps, and returns what it saw.
func GoJourney(h cleat.HostCalls, in JourneyInput) (string, error) {
	h.DurableSleep(time.Duration(in.SleepMs) * time.Millisecond)
	sig := h.AwaitSignals([]string{"go"}, 10*time.Minute)
	// The module has two entry points, so the child start must name its own (an input without __entry_point is refused).
	childIn, _ := json.Marshal(map[string]any{"in": map[string]int{"ms": in.ChildMs}, "__entry_point": "go_sleeper"})
	run, err := h.ChildWorkflow("go_sleeper", string(childIn))
	if err != nil {
		return "", err
	}
	res, err := h.AwaitChild(run)
	if err != nil {
		return "", err
	}
	out, _ := json.Marshal(map[string]string{"signal": sig.Payload, "child": res})
	return string(out), nil
}

// GoSleeper is a durable sleep.
func GoSleeper(h cleat.HostCalls, in SleeperInput) (string, error) {
	h.DurableSleep(time.Duration(in.Ms) * time.Millisecond)
	out, _ := json.Marshal(map[string]int{"slept": in.Ms})
	return string(out), nil
}

// GoMultiKey is the Go twin of the TypeScript `multikey` workflow: json.Marshal of the same map.
func GoMultiKey(h cleat.HostCalls, in SleeperInput) (string, error) {
	out, _ := json.Marshal(map[string]interface{}{
		"b": 1, "a": 2, "html": "<x>&", "nested": map[string]interface{}{"z": 1.5, "y": []interface{}{true, nil}}, "n": 100,
	})
	return string(out), nil
}

type XJourneyInput struct {
	SleepMs int `json:"sleepMs"`
	ChildMs int `json:"childMs"`
}

// XJourney is the Go twin of the TypeScript `xJourney`: sleep, wait for the signal "go", run the child "xsleeper", and return a
// result with no run-specific ids in it, so the two languages' results can be compared byte for byte.
func XJourney(h cleat.HostCalls, in XJourneyInput) (string, error) {
	if in.SleepMs > 0 {
		h.DurableSleep(time.Duration(in.SleepMs) * time.Millisecond)
	}
	sig := h.AwaitSignals([]string{"go"}, 10*time.Minute)
	childIn, _ := json.Marshal(map[string]int{"ms": in.ChildMs})
	run, err := h.ChildWorkflow("xsleeper", string(childIn))
	if err != nil {
		return "", err
	}
	res, err := h.AwaitChild(run)
	if err != nil {
		return "", err
	}
	var c struct {
		Slept int `json:"slept"`
	}
	if err := json.Unmarshal([]byte(res), &c); err != nil {
		return "", err
	}
	out, _ := json.Marshal(map[string]interface{}{"signal": json.RawMessage(sig.Payload), "slept": c.Slept})
	return string(out), nil
}

// GoNotANumber returns json.Marshal of a value holding NaN: Go's encoding/json refuses it (UnsupportedValueError). The TypeScript
// twin `nan` produces the same NaN with 0/0.
func GoNotANumber(h cleat.HostCalls, in SleeperInput) (string, error) {
	out, err := json.Marshal(map[string]interface{}{"v": math.NaN()})
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// GoInfinity is GoNotANumber for +Inf.
func GoInfinity(h cleat.HostCalls, in SleeperInput) (string, error) {
	out, err := json.Marshal(map[string]interface{}{"v": math.Inf(1)})
	if err != nil {
		return "", err
	}
	return string(out), nil
}
