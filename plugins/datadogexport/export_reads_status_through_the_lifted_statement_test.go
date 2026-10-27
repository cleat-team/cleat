package datadogexport

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugins/plugintest"
	"github.com/google/uuid"
)

// TestExportForConfigSendsTheStatusCountOfARealRun pins the CALL SITE of
// statusCountsSQL, which TestStatusCountsReadStatusFromWorkflowLeases does not:
// that test runs the lifted statement directly, so an exportForConfig that went
// back to an inline `FROM workflow_instances` literal would leave it green
// (cleat#3401, found in review of #3400).
//
// It drives exportForConfig itself, on a real narrowed schema, with the HTTP
// client stubbed, and asserts on the series in the payload it sends. The
// behavioural suite's fake driver cannot do this job: it matches query text, so
// it would answer whichever table the statement named. Since cleat#3245 B5
// workflow_instances has no status, so the revert is a runtime
// `column "status" does not exist` and exportForConfig returns it. The status is
// one no other test writes, so the count is about this run alone.
func TestExportForConfigSendsTheStatusCountOfARealRun(t *testing.T) {
	db := testutil.TestDB(t, testutil.DialectPostgres)
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)

	if _, err := db.Exec(`
		INSERT INTO workflow_defs (name, version, wasm_bytes)
		VALUES ('datadog-callsite-def', 1, '\x00'::bytea)
		ON CONFLICT (tenant_id, name, version) DO NOTHING`); err != nil {
		t.Fatalf("seed workflow_defs: %v", err)
	}
	id := fmt.Sprintf("datadog-callsite-%d", time.Now().UnixNano())
	testutil.SeedRun(t, db, testutil.RunSeed{ID: id, DefName: "datadog-callsite-def", Status: "b3a-dd-callsite-status"})
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, id) })

	cfgID := uuid.New()
	tenant := uuid.MustParse(engine.DefaultTenantUUID)
	secrets := plugintest.NewFakeSecrets()
	secrets.Seed(tenant.String(), DatadogAPIKeySecretName(cfgID), "dd-api-key")

	var sent []byte
	p := &Plugin{
		db:     &engine.SQLDBAdapter{DB: db},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		httpClient: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				sent, _ = io.ReadAll(req.Body)
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader([]byte(`{}`)))}, nil
			}),
			Timeout: 5 * time.Second,
		},
		secrets: secrets,
	}
	cfg := ddConfigRow{ID: cfgID, TenantID: tenant, Site: "datadoghq.com", MetricsPrefix: "cleat"}

	if err := p.exportForConfig(context.Background(), cfg); err != nil {
		t.Fatalf("exportForConfig: %v", err)
	}
	var payload ddSeriesPayload
	if err := json.Unmarshal(sent, &payload); err != nil {
		t.Fatalf("decode the payload exportForConfig sent (%q): %v", sent, err)
	}
	var got float64 = -1
	for _, s := range payload.Series {
		if s.Metric == "cleat.workflows.b3a-dd-callsite-status" && len(s.Points) == 1 {
			got = s.Points[0].Value
		}
	}
	if got != 1 {
		t.Fatalf("exported series = %+v, want cleat.workflows.b3a-dd-callsite-status = 1 from workflow_leases", payload.Series)
	}
}
