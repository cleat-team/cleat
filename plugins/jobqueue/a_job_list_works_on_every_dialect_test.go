package jobqueue

import (
	"context"
	"encoding/json"
	"io"
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

// TestAJobListWorksOnEveryDialect pins cleat#2206: handleListJobs
// (routes.go) built its row limit as a literal "LIMIT $N", which SQL Server
// rejects outright -- "Incorrect syntax near 'LIMIT'", a 500 on every call
// to GET /jobqueue/{queue_name}/jobs. Same bug class, same fix
// (plugin.LimitClause) as #2191's /audit/events and #2198's
// notifications/webhookingest list endpoints -- #2206's audit found this
// site missed by both.
//
// Round-tripping payload through the real routes (rather than seeding it
// directly) caught a second, unrelated MSSQL bug this test now pins too:
// handleEnqueue inserted req.Payload -- a json.RawMessage, i.e. []byte --
// straight as a driver arg. go-mssqldb maps a bare []byte to VARBINARY,
// which corrupts the NVARCHAR payload column on write, and the read side
// scanned straight into []byte/json.RawMessage instead of plugin.JSONColumn.
// The net effect was exactly plugin.JSONColumn's own documented symptom: a
// 200 with an empty body, because encoding/json failed part-way through
// writing the list response. See plugin.JSONColumn.
func TestAJobListWorksOnEveryDialect(t *testing.T) {
	for _, be := range testutil.NewPluginTestBackends(t) {
		be := be
		t.Run(be.Name, func(t *testing.T) {
			defer be.Cleanup()

			ctx := context.Background()
			dialect := plugin.Dialect(string(be.Dialect))
			quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

			testutil.SetupFullSchema(t, be.DB, be.Dialect)

			p := &Plugin{dialect: dialect, logger: quiet}
			if err := plugin.RunMigrations(ctx, be.DB, dialect, nil,
				[]*plugin.LoadedPlugin{{Plugin: p, Healthy: true}}); err != nil {
				t.Fatalf("migrations: %v", err)
			}
			p.db = &engine.SQLDBAdapter{DB: be.DB, Dialect: dialect}
			p.logger = quiet

			tenantID := uuid.MustParse(engine.DefaultTenantUUID)
			tenantCtx := auth.WithTenantID(context.Background(), tenantID)

			// A fresh, unique queue name per run: this test's own isolation
			// does not depend on the backing database being empty, which a
			// locally reused container is not.
			queueName := "list-test-queue-" + uuid.New().String()

			// Enqueue two jobs through the real route, not seeded directly,
			// so this proves handleEnqueue's own INSERT too.
			for i := 0; i < 2; i++ {
				enqueueReq := httptest.NewRequest("POST", "/jobqueue/"+queueName+"/jobs",
					strings.NewReader(`{"payload":{"n":1}}`)).WithContext(tenantCtx)
				enqueueReq.SetPathValue("queue_name", queueName)
				enqueueRec := httptest.NewRecorder()
				p.handleEnqueue(enqueueRec, enqueueReq)
				if enqueueRec.Code != http.StatusCreated {
					t.Fatalf("enqueue: want 201, got %d: %s", enqueueRec.Code, enqueueRec.Body.String())
				}
			}

			// GET /jobqueue/{queue_name}/jobs, through the real route.
			// cleat#2206's audit found this 500ing on MSSQL with "Incorrect
			// syntax near 'LIMIT'".
			listReq := httptest.NewRequest("GET", "/jobqueue/"+queueName+"/jobs", nil).WithContext(tenantCtx)
			listReq.SetPathValue("queue_name", queueName)
			listRec := httptest.NewRecorder()
			p.handleListJobs(listRec, listReq)
			if listRec.Code != http.StatusOK {
				t.Fatalf("list jobs: want 200, got %d: %s", listRec.Code, listRec.Body.String())
			}
			var jobs []JobResponse
			if err := json.Unmarshal(listRec.Body.Bytes(), &jobs); err != nil {
				t.Fatalf("decode jobs list: %v", err)
			}
			if len(jobs) != 2 {
				t.Fatalf("jobs list: got %d entries, want 2: %s", len(jobs), listRec.Body.String())
			}
			for _, j := range jobs {
				if j.QueueName != queueName {
					t.Errorf("queue_name in list: got %q, want %q", j.QueueName, queueName)
				}
				if j.Status != "pending" {
					t.Errorf("status in list: got %q, want %q", j.Status, "pending")
				}
				if got := strings.TrimSpace(string(j.Payload)); got != `{"n":1}` {
					t.Errorf("payload in list: got %q, want %q", got, `{"n":1}`)
				}
			}

			// limit=1, through the query string: proves the placeholder this
			// PR's fix actually binds and actually constrains the row count --
			// not just that the endpoint returns 200 with everything anyway.
			limitedReq := httptest.NewRequest("GET", "/jobqueue/"+queueName+"/jobs?limit=1", nil).WithContext(tenantCtx)
			limitedReq.SetPathValue("queue_name", queueName)
			limitedRec := httptest.NewRecorder()
			p.handleListJobs(limitedRec, limitedReq)
			if limitedRec.Code != http.StatusOK {
				t.Fatalf("list jobs (limit=1): want 200, got %d: %s", limitedRec.Code, limitedRec.Body.String())
			}
			var limitedJobs []JobResponse
			if err := json.Unmarshal(limitedRec.Body.Bytes(), &limitedJobs); err != nil {
				t.Fatalf("decode jobs list (limit=1): %v", err)
			}
			if len(limitedJobs) != 1 {
				t.Fatalf("jobs list (limit=1): got %d entries, want 1: %s", len(limitedJobs), limitedRec.Body.String())
			}
		})
	}
}
