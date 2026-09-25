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

// TestAnEmptyBodyEnqueuesWithADefaultPayload pins cleat#2278, a regression
// from #2261. That PR switched handleEnqueue's INSERT from a hand-built
// NULL-safe form to plugin.JSONColumn{Raw: req.Payload} -- and
// JSONColumn.Value() turns a zero-length Raw into an explicit SQL NULL, not
// the column default. payload is NOT NULL DEFAULT '{}' on Postgres and SQL
// Server (MySQL's was relaxed to nullable in cleat#1622), and a DEFAULT only
// fires when the column is OMITTED, not when it is written as an explicit
// NULL -- so an empty or absent body, which ReadOptionalJSONBody's contract
// treats as a legitimate enqueue, hit the NOT NULL constraint and 500'd on
// two of the three dialects.
//
// Both shapes a caller can send for "no payload" are covered, because they
// reach handleEnqueue by different paths: a zero-length body never reaches
// json.Unmarshal at all (len(body) > 0 guards it), while "{}" unmarshals to
// a non-nil req but leaves req.Payload nil -- enqueueRequest has no Payload
// field of its own for the top level "{}"; payload only exists as the
// nested "payload" key, so a body of literal "{}" also leaves req.Payload
// unset. Both must enqueue successfully and read back as "{}", not as a
// JSON null and not as a 500.
func TestAnEmptyBodyEnqueuesWithADefaultPayload(t *testing.T) {
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

			for _, body := range []struct {
				name string
				body string
			}{
				{"empty_body", ""},
				{"empty_object", "{}"},
			} {
				body := body
				t.Run(body.name, func(t *testing.T) {
					queueName := "empty-payload-" + body.name + "-" + uuid.New().String()

					var reader io.Reader
					if body.body != "" {
						reader = strings.NewReader(body.body)
					}
					enqueueReq := httptest.NewRequest("POST", "/jobqueue/"+queueName+"/jobs", reader).WithContext(tenantCtx)
					enqueueReq.SetPathValue("queue_name", queueName)
					enqueueRec := httptest.NewRecorder()
					p.handleEnqueue(enqueueRec, enqueueReq)
					if enqueueRec.Code != http.StatusCreated {
						t.Fatalf("enqueue: want 201, got %d: %s", enqueueRec.Code, enqueueRec.Body.String())
					}
					var enqueued struct {
						JobID string `json:"job_id"`
					}
					if err := json.Unmarshal(enqueueRec.Body.Bytes(), &enqueued); err != nil {
						t.Fatalf("decode enqueue response: %v", err)
					}

					getReq := httptest.NewRequest("GET", "/jobqueue/"+queueName+"/jobs/"+enqueued.JobID, nil).WithContext(tenantCtx)
					getReq.SetPathValue("queue_name", queueName)
					getReq.SetPathValue("job_id", enqueued.JobID)
					getRec := httptest.NewRecorder()
					p.handleGetJob(getRec, getReq)
					if getRec.Code != http.StatusOK {
						t.Fatalf("get job: want 200, got %d: %s", getRec.Code, getRec.Body.String())
					}
					var job JobResponse
					if err := json.Unmarshal(getRec.Body.Bytes(), &job); err != nil {
						t.Fatalf("decode job: %v", err)
					}
					if got := strings.TrimSpace(string(job.Payload)); got != "{}" {
						t.Errorf("payload: got %q, want %q", got, "{}")
					}
				})
			}
		})
	}
}
