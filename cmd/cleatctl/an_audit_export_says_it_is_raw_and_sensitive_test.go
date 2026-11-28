package main

import (
	"strings"
	"testing"
)

// cleat#3581 (D49-1a): every other surface that shows workflow data is redacted on
// its way out, and `audit export` is the one that is not, because a redacted export
// cannot be verified against the hash chain. An operator who has learned that
// cleatctl redacts what it prints would otherwise pass the file around. The help is
// where they would learn it is not, so it is pinned here.
func TestAuditExportHelpSaysTheOutputIsRawAndSensitive(t *testing.T) {
	for _, want := range []string{
		"NOT redacted",
		"cannot be verified against the chain",
		"record_event",
		"sensitive, like a secret",
	} {
		if !strings.Contains(auditUsage, want) {
			t.Errorf("auditUsage no longer says %q:\n%s", want, auditUsage)
		}
	}
}
