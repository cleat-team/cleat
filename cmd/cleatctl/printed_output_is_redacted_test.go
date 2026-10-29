package main

import (
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// cleat#3581, D49-1a: what `cleatctl debug` and `replay` PRINT is redacted by
// default and raw under --no-redact. Only the printed text: the events handed to
// the engine are the stored ones, so printing must not modify them.

const printSecret = `{"api_key":"sk-live-SECRET123","version":"1.2.3","max_tokens":5}`

func withPrintRaw(t *testing.T, raw bool) {
	t.Helper()
	old := printRaw
	printRaw = raw
	t.Cleanup(func() { printRaw = old })
}

func secretEvent() engine.EventRecord {
	return engine.EventRecord{
		Step: 2, EventType: engine.EventTypeCall, Service: "billing", Op: "charge",
		Request: printSecret, Response: printSecret, Err: printSecret, SignalPayload: printSecret,
	}
}

func TestPrintedOutputIsRedactedByDefaultAndRawWhenAsked(t *testing.T) {
	if got := engine.Redact(printSecret); strings.Contains(got, "sk-live-SECRET123") {
		t.Fatalf("CONTROL: engine.Redact does not rewrite the secret in %s", printSecret)
	}

	t.Run("default", func(t *testing.T) {
		withPrintRaw(t, false)
		ev := secretEvent()
		for what, got := range map[string]string{
			"formatEvent":    formatEvent(ev),
			"printable":      printable(ev).Request + printable(ev).Response + printable(ev).Err + printable(ev).SignalPayload,
			"printableValue": printableValue(printSecret),
			"printableMap":   printableMap(map[string]string{"k": printSecret})["k"],
		} {
			if strings.Contains(got, "sk-live-SECRET123") {
				t.Errorf("%s printed the secret by default: %s", what, got)
			}
			if !strings.Contains(got, "[REDACTED]") {
				t.Errorf("%s shows no [REDACTED] marker: %s", what, got)
			}
		}
	})

	t.Run("--no-redact", func(t *testing.T) {
		withPrintRaw(t, true)
		ev := secretEvent()
		for what, got := range map[string]string{
			"formatEvent":    formatEvent(ev),
			"printableValue": printableValue(printSecret),
			"printableMap":   printableMap(map[string]string{"k": printSecret})["k"],
		} {
			if !strings.Contains(got, "sk-live-SECRET123") {
				t.Errorf("%s did not print the stored value under --no-redact: %s", what, got)
			}
		}
	})
}

// Printing must not change the history the engine is replaying.
func TestPrintingDoesNotModifyTheEventsTheEngineIsGiven(t *testing.T) {
	withPrintRaw(t, false)
	events := []engine.EventRecord{secretEvent(), secretEvent()}
	_ = formatEvent(events[0])
	_ = printable(events[1])
	for i, ev := range events {
		if ev.Request != printSecret || ev.Response != printSecret || ev.Err != printSecret || ev.SignalPayload != printSecret {
			t.Errorf("event %d was modified by printing: %+v", i, ev)
		}
	}
	m := map[string]string{"k": printSecret}
	_ = printableMap(m)
	if m["k"] != printSecret {
		t.Error("printableMap modified the map it was given")
	}
}

func TestBothCommandsParseNoRedact(t *testing.T) {
	withPrintRaw(t, false)
	if f := parseDebugFlags([]string{"wf-1", "--entry-point", "Run"}); f == nil || printRaw {
		t.Fatalf("without --no-redact debug must not print raw: flags=%v printRaw=%v", f, printRaw)
	}
	if f := parseDebugFlags([]string{"wf-1", "--entry-point", "Run", "--no-redact"}); f == nil || !printRaw {
		t.Fatalf("debug --no-redact did not set printRaw: flags=%v printRaw=%v", f, printRaw)
	}

	withPrintRaw(t, false)
	if f := parseReplayFlags([]string{"wf-1", "--entry-point", "Run"}); f == nil || printRaw {
		t.Fatalf("without --no-redact replay must not print raw: flags=%v printRaw=%v", f, printRaw)
	}
	if f := parseReplayFlags([]string{"wf-1", "--entry-point", "Run", "--no-redact"}); f == nil || !printRaw {
		t.Fatalf("replay --no-redact did not set printRaw: flags=%v printRaw=%v", f, printRaw)
	}
}

// The step-through display is the print site people actually read.
func TestTheStepDisplayRedactsWhatItPrintsUnlessAskedNot(t *testing.T) {
	step := func() string {
		ev := secretEvent()
		ds := &debugState{events: []engine.EventRecord{ev}}
		return captureStdout(t, func() {
			ds.displayStep(debugStepInfo{step: 0, event: &ev, qs: map[string]string{"k": printSecret}})
		})
	}

	withPrintRaw(t, false)
	got := step()
	if strings.Contains(got, "sk-live-SECRET123") || !strings.Contains(got, "[REDACTED]") {
		t.Errorf("the step display printed the secret by default:\n%s", got)
	}

	withPrintRaw(t, true)
	if got := step(); !strings.Contains(got, "sk-live-SECRET123") {
		t.Errorf("--no-redact did not print the stored value:\n%s", got)
	}
}
