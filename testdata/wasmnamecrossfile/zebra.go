package main

import "github.com/cleat-team/cleat/cleat"

// Aardvark sorts alphabetically before Zookeeper (mango.go), so
// result.EntryPoints[0] names THIS function -- deliberately, to prove
// wasmOutputName resolves to the FILE the selected entry point is declared
// in (zebra.go, alphabetically the LAST of the two filenames) rather than
// to the alphabetically-first filename (mango.go) or to Aardvark's own
// name. cleat#2407.
func Aardvark(h cleat.HostCalls, input string) (string, error) {
	return input, nil
}
