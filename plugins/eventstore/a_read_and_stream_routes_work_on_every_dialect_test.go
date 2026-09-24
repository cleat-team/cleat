// TestReadAndStreamRoutesWorkOnEveryDialect pins cleat#2257: handleRead and
// handleSSE both scanned the event column into a bare json.RawMessage.
// json.RawMessage is a named []byte type, and database/sql's convertAssign
// fast path does not convert a driver string into one -- go-mssqldb returns
// NVARCHAR as string, so every row failed to scan on SQL Server. handleRead
// logged the error and skipped the row, returning an empty list; handleSSE
// did the same inside its poll loop, so a stream client on SQL Server never
// received an event at all. Found during #2257's sweep for the same bug
// shape #2206 fixed elsewhere in this tree; fixed the same way, via
// plugin.JSONColumn.
package eventstore

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
)

func TestReadAndStreamRoutesWorkOnEveryDialect(t *testing.T) {
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

			streamID := "read-test-" + uuid.New().String()
			eventBody := `{"n":1}`

			// Seeded directly, not through handleAppend: #2257's sweep found
			// handleAppend's own INSERT (insertEventReturning) broken on
			// MySQL for an unrelated reason -- "You can't specify target
			// table 'event_stream' for update in FROM clause" (error 1093),
			// from its MAX(sequence) subquery reading the same table it
			// inserts into, which MySQL disallows. That is a real,
			// previously-undiscovered bug (filed separately, cleat#2260 --
			// see WORKSTREAM discussion), but it is a write-path defect and
			// this test's subject is the read path's scan. Seeding directly
			// keeps the two independent, the same reasoning #2257 itself
			// used to scope cleat#2259 out rather than widen this PR.
			insertTestEvent(t, tenantCtx, p, be.Name, tenantID.String(), streamID, 1, eventBody)

			// handleRead, through the real route. cleat#2257's sweep found
			// this returning an empty list on SQL Server -- unrelated to
			// the append succeeding above.
			readReq := httptest.NewRequest("GET", "/events/"+streamID, nil).WithContext(tenantCtx)
			readReq.SetPathValue("stream_id", streamID)
			readRec := httptest.NewRecorder()
			p.handleRead(readRec, readReq)
			if readRec.Code != http.StatusOK {
				t.Fatalf("read: want 200, got %d: %s", readRec.Code, readRec.Body.String())
			}

			var events []struct {
				Sequence int64           `json:"sequence"`
				Event    json.RawMessage `json:"event"`
			}
			if err := json.Unmarshal(readRec.Body.Bytes(), &events); err != nil {
				t.Fatalf("decode read response: %v", err)
			}
			if len(events) != 1 {
				t.Fatalf("handleRead on %s: got %d events, want 1 -- this is exactly the scan "+
					"this test pins", be.Name, len(events))
			}
			assertSameJSON(t, be.Name, "handleRead", events[0].Event, eventBody)

			// handleSSE, through the real route. Same bug shape, in the poll
			// loop's own scan. The handler blocks on a context and polls
			// once a second, so this exercises real timing: start the
			// stream, wait for its initial "current sequence" query to
			// settle, append a SECOND event (handleSSE only streams events
			// after the sequence it saw at start), then wait past one
			// tick and read what was flushed.
			sseCtx, cancel := context.WithTimeout(tenantCtx, 2500*time.Millisecond)
			defer cancel()
			sseReq := httptest.NewRequest("GET", "/events/"+streamID+"/stream", nil).WithContext(sseCtx)
			sseReq.SetPathValue("stream_id", streamID)
			sseRec := httptest.NewRecorder()

			done := make(chan struct{})
			go func() {
				defer close(done)
				p.handleSSE(sseRec, sseReq)
			}()

			time.Sleep(200 * time.Millisecond)

			secondBody := `{"n":2}`
			insertTestEvent(t, tenantCtx, p, be.Name, tenantID.String(), streamID, 2, secondBody)

			<-done // the context timeout stops handleSSE; this waits for that.

			body := sseRec.Body.String()
			line, ok := findSSEDataLine(body, "sequence")
			if !ok {
				t.Fatalf("handleSSE on %s: no SSE data line found in body: %q", be.Name, body)
			}
			var msg struct {
				Sequence int64           `json:"sequence"`
				Event    json.RawMessage `json:"event"`
			}
			if err := json.Unmarshal([]byte(line), &msg); err != nil {
				t.Fatalf("handleSSE on %s: decode SSE payload %q: %v", be.Name, line, err)
			}
			assertSameJSON(t, be.Name, "handleSSE", msg.Event, secondBody)
		})
	}
}

// findSSEDataLine returns the payload of the first "data: " line in an SSE
// body that decodes as JSON containing the given key.
func findSSEDataLine(body, key string) (string, bool) {
	for _, line := range strings.Split(body, "\n") {
		payload, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		if strings.Contains(payload, `"`+key+`"`) {
			return payload, true
		}
	}
	return "", false
}

// assertSameJSON compares parsed JSON structure, not raw bytes: Postgres's
// jsonb column re-serializes on the way back out (a space after ":"), which
// is real, harmless reformatting -- not the bug either test above pins.
func assertSameJSON(t *testing.T, dialectName, route string, got json.RawMessage, want string) {
	t.Helper()
	var gotParsed, wantParsed any
	if err := json.Unmarshal(got, &gotParsed); err != nil {
		t.Fatalf("%s on %s: event %q is not valid JSON: %v", route, dialectName, got, err)
	}
	if err := json.Unmarshal([]byte(want), &wantParsed); err != nil {
		t.Fatalf("invalid want JSON: %v", err)
	}
	if !jsonEqual(gotParsed, wantParsed) {
		t.Errorf("%s on %s: event = %s, want %s", route, dialectName, got, want)
	}
}

func jsonEqual(a, b any) bool {
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return string(ab) == string(bb)
}

var insertTestEventQuery = plugin.Query{
	Default: `INSERT INTO event_stream (tenant_id, stream_id, sequence, event) VALUES ($1, $2, $3, $4::jsonb)`,
	MySQL:   `INSERT INTO event_stream (tenant_id, stream_id, sequence, event) VALUES ($1, $2, $3, $4)`,
	MSSQL:   `INSERT INTO event_stream (tenant_id, stream_id, sequence, event) VALUES ($1, $2, $3, $4)`,
}

// insertTestEvent seeds one event_stream row directly, bypassing handleAppend
// (and cleat#2260's MySQL INSERT bug) so this file's read-side tests stay
// independent of the write path's own correctness. It goes through p.db
// (SQLDBAdapter) under a tenant-bearing context, not be.DB directly, because
// a bare pool connection carries no cleat.tenant_id -- SQL Server's RLS
// block predicate rejects the INSERT outright (error 33504) without it. See
// engine/plugindb_tenant.go and this package's own event_stream_rows_are_
// scoped_by_a_policy_test.go.
func insertTestEvent(t *testing.T, tenantCtx context.Context, p *Plugin, backendName,
	tenantID, streamID string, sequence int64, event string) {
	t.Helper()
	if _, err := p.db.Exec(tenantCtx, plugin.Rebind(insertTestEventQuery.For(p.dialect), p.dialect),
		tenantID, streamID, sequence, event); err != nil {
		t.Fatalf("insertTestEvent on %s: %v", backendName, err)
	}
}
