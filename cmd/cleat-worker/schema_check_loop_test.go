package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/migration"
)

// TestRunSchemaCheck_DetectsBehindAndRecovers is cleat#2264's regression test.
// Before this change, nothing after worker startup ever re-verified the
// schema: a database restored from an older backup, or rolled back, while
// this worker was already running and serving traffic was invisible to
// /readyz forever, because the check cleat#2117 added runs exactly once, at
// boot.
//
// It runs against a real, already-migrated database (engine/testutil's
// TestDB) for the "current" case, and a SEPARATE runner -- built with
// Runner.WithFS pointed at an in-memory filesystem carrying one fake,
// unapplied migration -- for the "behind" case. That second runner never
// touches the database beyond Verify's own read-only queries (no table
// creation, no lock, nothing applied -- see migration.Runner.Verify's own
// doc comment), so this does not mutate the shared TestDB that other tests
// run concurrently against, which CLAUDE.md's "Use -p 1" section warns is the
// hazard with anything that writes to it.
func TestRunSchemaCheck_DetectsBehindAndRecovers(t *testing.T) {
	for _, dialect := range []testutil.Dialect{testutil.DialectPostgres, testutil.DialectMySQL, testutil.DialectMSSQL} {
		t.Run(string(dialect), func(t *testing.T) {
			db := testutil.TestDB(t, dialect)
			driver := string(dialect)

			w := newTestWorker(&mockStore{})
			w.db = db
			w.dbDialect = driver
			w.plugList = nil
			w.schemaName = ""
			// The /readyz assertions below are about the schema_behind reason
			// specifically, not about database reachability -- without this,
			// healthReport's own !r.db.Known branch reports "starting" first
			// and the schema reason, while present, is never the thing being
			// tested. One successful observe() is what a real worker's own
			// heartbeat would have recorded by the time anyone asks /readyz.
			now := w.dbReach.now()
			w.dbReach.observe(now, now, 0, nil)

			currentRunner := migration.NewRunner(db, migration.Dialect(driver), "../../migrations")

			// Positive control: against the real, shipped migrations this
			// binary carries, a database TestDB already migrated to current
			// must not be reported behind. Without this, a schema_check that
			// always reports "behind" would pass the test below just as
			// happily.
			w.schemaMigrator = currentRunner
			w.runSchemaCheck()
			if w.schemaBehind.Load() {
				t.Fatalf("%s: runSchemaCheck reported behind against TestDB's own migrated schema; "+
					"last error: %v", driver, w.schemaLastErr)
			}

			// Now point the SAME worker at a runner whose migration set
			// carries one fake, never-applied, high-versioned file. Verify
			// is read-only (migration.Runner.Verify's doc comment), so this
			// cannot and does not change what currentRunner sees of the real
			// database -- it only changes which migrations THIS runner
			// believes are shipped.
			fakeFS := fstest.MapFS{
				driver + "/999999_cleat_2264_fake_pending.sql": &fstest.MapFile{
					Data: []byte("-- never applied; exists only so Verify sees a pending migration\n"),
				},
			}
			behindRunner := migration.NewRunner(db, migration.Dialect(driver), ".").WithFS(fakeFS)
			w.schemaMigrator = behindRunner
			w.runSchemaCheck()
			if !w.schemaBehind.Load() {
				t.Fatalf("%s: runSchemaCheck did not report behind against a runner carrying an "+
					"unapplied fake migration; last error: %v", driver, w.schemaLastErr)
			}
			if len(w.schemaLastCheck.Pending) != 1 || w.schemaLastCheck.Pending[0] != "999999_cleat_2264_fake_pending.sql" {
				t.Errorf("%s: schemaLastCheck.Pending = %v, want exactly the fake migration",
					driver, w.schemaLastCheck.Pending)
			}

			// /readyz must reflect it: this is the whole point of the loop
			// existing, not an incidental detail of the internal field.
			api := &apiServer{store: &mockStore{}, worker: w, maxBodySize: 1 << 20, factory: &fakeStoreFactory{fallback: &mockStore{}}}
			req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
			rec := httptest.NewRecorder()
			api.handleReadyz(rec, req)
			if rec.Code != http.StatusServiceUnavailable {
				t.Errorf("%s: /readyz = %d while schema is behind, want 503: %s", driver, rec.Code, rec.Body.String())
			}
			if body := rec.Body.String(); !strings.Contains(body, reasonSchemaBehind) {
				t.Errorf("%s: /readyz body %q does not name %q", driver, body, reasonSchemaBehind)
			}

			// Recovery: point back at the current runner and confirm the
			// loop clears the flag rather than latching it -- a database
			// that gets migrated forward again (the remediation the error
			// message itself recommends) must bring /readyz back to 200
			// without a worker restart.
			w.schemaMigrator = currentRunner
			w.runSchemaCheck()
			if w.schemaBehind.Load() {
				t.Fatalf("%s: runSchemaCheck still reports behind after switching back to the current "+
					"migration set; schema_behind must clear on recovery, not latch", driver)
			}
			rec = httptest.NewRecorder()
			api.handleReadyz(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
			if rec.Code != http.StatusOK {
				t.Errorf("%s: /readyz = %d after schema recovered, want 200: %s", driver, rec.Code, rec.Body.String())
			}
		})
	}
}
