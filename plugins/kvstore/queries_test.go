package kvstore

import (
	"strings"
	"testing"

	"github.com/cleat-team/cleat/plugin"
)

// TestUpsertKVMSSQLMergeUsesHoldlock is the deterministic half of cleat#2890's
// guard against a regression. TestConcurrentMergesToABrandNewKeyRaceWithoutHoldlock
// (same package) is a real, timing-based reproduction of the hazard WITH
// (HOLDLOCK) fixes, but it is PROBABILISTIC and measured environment-dependent
// (cleat-review: 0/800 on one SQL Server build where this file's own
// measurement got 28/800 on another) -- it can read clean on the unfixed
// query depending on what machine CI happens to land on. This test cannot:
// it is a static assertion on the query text, needs no database, and fails
// the instant WITH (HOLDLOCK) is removed, on every environment identically.
func TestUpsertKVMSSQLMergeUsesHoldlock(t *testing.T) {
	stmt := upsertKV.For(plugin.DialectMSSQL)
	if !strings.Contains(stmt, "WITH (HOLDLOCK)") {
		t.Fatalf("upsertKV's MSSQL statement no longer contains WITH (HOLDLOCK) -- "+
			"this reopens cleat#2890's race (two concurrent MERGEs against a brand-new "+
			"key can both take the INSERT branch and collide on kv_store's primary key). "+
			"Statement:\n%s", stmt)
	}
}
