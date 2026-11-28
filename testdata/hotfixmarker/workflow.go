// Package main is a fixture for a hot-fixed definition (cleat#3516).
//
// Its one observable effect is a DurableCall whose operation names the build:
// "MARKER-AAAAAA". The test that uses it rewrites those bytes in the compiled
// module to "MARKER-BBBBBB" -- the same length, so a cache that validates by
// length cannot tell the two apart -- and then asks which one a worker ran.
package main

import "github.com/cleat-team/cleat/cleat"

// Marker makes the call that says which build of this module is running.
func Marker(h cleat.HostCalls, input string) (string, error) {
	if _, err := h.DurableCall("marker", "MARKER-AAAAAA", `{}`); err != nil {
		return "", err
	}
	return `{"status":"ok"}`, nil
}
