package testutil

import (
	"sync"
	"testing"
)

// TestApplyPostgresSchemaFileConcurrent reproduces IMPROVEMENT-PLAN §2.21.
//
// `go test ./plugins/...` runs distinct packages in parallel (-p defaults to
// NumCPU) and every one of them points at the same CLEAT_TEST_POSTGRES
// database, so several call applyPostgresSchemaFile at once. Nothing used to
// serialise them, and PostgreSQL's IF NOT EXISTS forms are not atomic: two
// sessions both see the object missing, both insert the catalog row, and one
// loses on a unique index:
//
//	pq: duplicate key value violates unique constraint
//	"pg_extension_name_index" (23505)
//
// That flake blocked real merges twice (PRs #225 and #227) before it was
// fixed, which is the cost of a "known flake" nobody owns.
//
// This drives the same shape deliberately: N goroutines applying the schema
// to one database at once. Against the unlocked version it fails with the
// 23505 above; with the advisory lock it passes.
func TestApplyPostgresSchemaFileConcurrent(t *testing.T) {
	// TestDB rather than a hand-rolled sql.Open: it already draws the only
	// distinction that matters -- no database asked for is a skip, a
	// configured-but-unreachable one is a failure -- and reusing it avoids
	// adding a second skip site that scripts/check-skips.sh would have to
	// carry (Start here item 1 / §2.12).
	db := SuiteTestDB(t, "testutil")
	defer db.Close()

	// Enough concurrency to lose the IF NOT EXISTS race reliably. Each needs
	// its own connection, so raise the pool limit to match.
	const concurrency = 8
	db.SetMaxOpenConns(concurrency + 2)

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // release them together, to overlap the CREATEs
			applyPostgresSchemaFile(t, db)
		}()
	}
	close(start)
	wg.Wait()
}
