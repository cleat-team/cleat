// TestOldStyleWriterBetweenAppendsIsAbsorbedByResync pins the gap cleat-review
// and the coordinator found in cleat#2268 round 1: verifySchema
// (cmd/cleat-worker/schema_verify.go) warns rather than refuses when the
// schema is AHEAD of the running binary, by design, so a rolling upgrade's
// deploy step (migrate first, replace workers after) doesn't wedge on the
// workers it is replacing. Until the last old-binary worker exits, it keeps
// computing MAX(sequence)+1 and inserting directly into event_stream -- it
// never writes event_stream_head at all.
//
// This test plays that writer by hand: it inserts directly into event_stream
// with insertEvent, the exact statement an old binary used before cleat#2268,
// bypassing upsertStreamHead entirely -- no event_stream_head row exists for
// the stream afterwards. It then drives a real new-code append through
// p.handleAppend and asserts the result does not collide with the row the
// "old worker" already committed, repeated twice to show the stream stays
// healthy rather than being patched once and breaking again on the next
// append (appendOnce's resync path raises the head row, so this is not
// supposed to need a second collision to notice).
//
// Falsified against the pre-resync code (upsertStreamHead with no
// isPKConflict handling in appendOnce): the first new-code append after the
// out-of-band write failed outright with a primary-key violation, exactly
// the permanent breakage this test exists to catch.
package eventstore

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
)

func TestOldStyleWriterBetweenAppendsIsAbsorbedByResync(t *testing.T) {
	for _, be := range testutil.NewPluginTestBackends(t) {
		be := be
		t.Run(be.Name, func(t *testing.T) {
			defer be.Cleanup()

			ctx := context.Background()
			dialect := plugin.Dialect(string(be.Dialect))

			testutil.SetupFullSchema(t, be.DB, be.Dialect)

			p := &Plugin{dialect: dialect, logger: slog.Default(), config: Config{MaxEventSize: 1 << 20}}
			if err := plugin.RunMigrations(ctx, be.DB, dialect, nil,
				[]*plugin.LoadedPlugin{{Plugin: p, Healthy: true}}); err != nil {
				t.Fatalf("migrations: %v", err)
			}
			p.db = &engine.SQLDBAdapter{DB: be.DB, Dialect: dialect}

			tenantID := uuid.MustParse(engine.DefaultTenantUUID)
			tenantCtx := auth.WithTenantID(context.Background(), tenantID)

			streamID := "old-style-writer-test-" + uuid.New().String()

			appendOnceHTTP := func() (code int, body string, sequence int64) {
				t.Helper()
				req := httptest.NewRequest("POST", "/events/"+streamID,
					strings.NewReader(`{"n":1}`)).WithContext(tenantCtx)
				req.SetPathValue("stream_id", streamID)
				rec := httptest.NewRecorder()
				p.handleAppend(rec, req)
				if rec.Code != http.StatusCreated {
					return rec.Code, rec.Body.String(), 0
				}
				var resp struct {
					Sequence int64 `json:"sequence"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
					t.Fatalf("decode response: %v", err)
				}
				return rec.Code, rec.Body.String(), resp.Sequence
			}

			oldStyleWrite := func(sequence int64) {
				t.Helper()
				// The exact statement an old binary used before cleat#2268 --
				// straight into event_stream, event_stream_head untouched.
				if _, err := p.db.Exec(tenantCtx, plugin.Rebind(insertEvent.For(dialect), dialect),
					tenantID, streamID, sequence, `{"old":true}`); err != nil {
					t.Fatalf("old-style write at sequence %d: %v", sequence, err)
				}
			}

			// An old-binary worker claims sequence 1 directly. No
			// event_stream_head row exists for this stream at all yet.
			oldStyleWrite(1)

			code, body, seq := appendOnceHTTP()
			if code != http.StatusCreated {
				t.Fatalf("on %s: new-code append after an old-style write got %d, want 201: %s",
					be.Name, code, body)
			}
			if seq != 2 {
				t.Fatalf("on %s: new-code append after an old-style write at sequence 1 got "+
					"sequence %d, want 2 (it must not collide with the row the old worker "+
					"already committed)", be.Name, seq)
			}

			// The stream must stay healthy on the NEXT append too -- resync
			// raises the head row, so this must not need another collision
			// to behave.
			code, body, seq = appendOnceHTTP()
			if code != http.StatusCreated {
				t.Fatalf("on %s: second new-code append got %d, want 201: %s", be.Name, code, body)
			}
			if seq != 3 {
				t.Fatalf("on %s: second new-code append got sequence %d, want 3 -- the resync "+
					"from the first collision should have stuck", be.Name, seq)
			}

			// Read back what actually landed, independent of what the
			// handler reported. Through p.db, not be.DB directly: MSSQL's
			// row-level-security filter needs the tenant-scoped transaction
			// SQLDBAdapter sets up (see TestConcurrentAppendsGetContiguousSequences's
			// identical note).
			rows, err := p.db.Query(tenantCtx,
				`SELECT sequence FROM event_stream WHERE tenant_id = $1 AND stream_id = $2 ORDER BY sequence ASC`,
				tenantID, streamID)
			if err != nil {
				t.Fatalf("read back on %s: %v", be.Name, err)
			}
			defer rows.Close()

			var seqs []int64
			for rows.Next() {
				var s int64
				if err := rows.Scan(&s); err != nil {
					t.Fatalf("scan on %s: %v", be.Name, err)
				}
				seqs = append(seqs, s)
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("rows iteration on %s: %v", be.Name, err)
			}

			want := []int64{1, 2, 3}
			if len(seqs) != len(want) {
				t.Fatalf("on %s: sequences = %v, want %v", be.Name, seqs, want)
			}
			for i, s := range seqs {
				if s != want[i] {
					t.Errorf("on %s: sequences = %v, want %v", be.Name, seqs, want)
					break
				}
			}
		})
	}
}
