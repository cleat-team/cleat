package main

import "testing"

// The engine validates a deployed row's version against the binary's own
// metadata at run time (error_op "version_check"). So the version a deploy
// writes is not bookkeeping -- disagreeing with the WASM produces a workflow
// that deploys cleanly and then fails every run with:
//
//	version mismatch: workflow instance <id> expects def_version 2 but WASM
//	binary metadata reports version 1 (def=<name>). The workflow_defs row and
//	the deployed WASM binary are out of sync.
//
// deploy-workflow used to auto-increment unconditionally, so this happened on
// the SECOND deploy of any workflow whose bytes had changed. It matters more
// than it looks: cmd/cleat's deploy refuses a MySQL or SQL Server DSN by
// design and names this binary as the alternative, so this is the only
// multi-dialect deploy path cleat has, and on those two dialects it produced
// an unrunnable workflow on every redeploy.
//
// Found by pointing the port harness at this binary and running twice -- the
// first run passed.
func TestChooseDeployVersion(t *testing.T) {
	for _, tc := range []struct {
		name     string
		embedded int
		next     int
		want     int
		why      string
	}{
		{
			name:     "embedded version wins over the next free number",
			embedded: 1, next: 2, want: 1,
			why: "the defect: a rebuilt binary still reporting v1 was deployed as v2",
		},
		{
			name:     "embedded version wins even when it is ahead",
			embedded: 5, next: 2, want: 5,
			why: "the author bumped the version in the workflow; the row must follow the binary",
		},
		{
			name:     "no embedded version falls back to the next free number",
			embedded: 0, next: 3, want: 3,
			why: "metadata is optional -- a binary built by an older toolchain has none",
		},
		{
			name:     "no embedded version on a first deploy",
			embedded: 0, next: 1, want: 1,
			why: "nothing deployed yet",
		},
		{
			name:     "a redeploy of the same embedded version is idempotent",
			embedded: 2, next: 3, want: 2,
			why: "deploying v2 twice must update the v2 row, not create a v3 the binary disowns",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := chooseDeployVersion(tc.embedded, tc.next); got != tc.want {
				t.Errorf("chooseDeployVersion(%d, %d) = %d, want %d -- %s",
					tc.embedded, tc.next, got, tc.want, tc.why)
			}
		})
	}
}
