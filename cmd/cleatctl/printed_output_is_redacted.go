package main

import "github.com/cleat-team/cleat/engine"

// What `cleatctl debug` and `cleatctl replay` PRINT is observability: it lands in
// a terminal, a scrollback, a pasted bug report. It is redacted by default, and
// --no-redact on either command prints the stored value for an operator who needs
// it (cleat#3581, D49-1a).
//
// Only the printed text is affected. Both commands hand the engine the raw stored
// history, because a replay that read a redacted copy would be replaying a
// different run; a test pins that the engine is given what was stored.
var printRaw bool

// printable returns ev as it is shown on the terminal.
func printable(ev engine.EventRecord) engine.EventRecord {
	if printRaw {
		return ev
	}
	return engine.RedactEventRecordForEgress(ev)
}

// printableValue returns one published value (a query-state entry, a replay
// result) as it is shown on the terminal.
func printableValue(s string) string {
	if printRaw {
		return s
	}
	return engine.Redact(s)
}

// printableMap returns a query-state map as it is shown on the terminal, leaving
// the argument alone.
func printableMap(m map[string]string) map[string]string {
	if printRaw || len(m) == 0 {
		return m
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = engine.Redact(v)
	}
	return out
}
