// TestBlobGetHostFunctionWorksOnEveryDialect pins cleat#2257: the blobGet
// host function (host_functions.go) referenced the blob_index table's key
// column as a bare "i.key". key is a reserved word in MySQL and SQL Server
// both, and this 500ed -- "Incorrect syntax near the keyword 'key'. (156)"
// -- on every guest call to blob_get on SQL Server. #2256 fixed the same bug
// in the HTTP routes (handleGet, handleHead, handleDelete, handleList) via
// plugin.QuoteIdent but missed this host function, which is a separate call
// site with its own query. Fixed the same way here.
package blobstore

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
)

func TestBlobGetHostFunctionWorksOnEveryDialect(t *testing.T) {
	for _, be := range testutil.NewPluginTestBackends(t) {
		t.Run(be.Name, func(t *testing.T) {
			defer be.Cleanup()

			ctx := context.Background()
			dialect := plugin.Dialect(string(be.Dialect))

			testutil.SetupFullSchema(t, be.DB, be.Dialect)

			p := &Plugin{dialect: dialect, logger: slog.Default(), backend: newTestMemBackend()}
			if err := plugin.RunMigrations(ctx, be.DB, dialect, nil,
				[]*plugin.LoadedPlugin{{Plugin: p, Healthy: true}}); err != nil {
				t.Fatalf("migrations: %v", err)
			}
			p.db = &engine.SQLDBAdapter{DB: be.DB, Dialect: dialect}

			tenantID := uuid.MustParse(engine.DefaultTenantUUID)
			cc := &plugin.CallContext{TenantID: tenantID.String()}
			// Both are required, and they do different jobs: WithCallContext
			// lets the host function read cc.TenantID for its own WHERE
			// clause; auth.WithTenantID (delegating to internal/tenantctx)
			// is what SQLDBAdapter's tenantTx reads to set cleat.tenant_id
			// for row-level security -- see engine/plugindb_tenant.go. A
			// direct call like this one skips engine.pluginCallContext,
			// which bridges both at once on the real host-call path, so
			// without this the INSERT in blobPut hits the RLS block
			// predicate on Postgres/MSSQL (error 33504 on MSSQL).
			hostCtx := plugin.WithCallContext(auth.WithTenantID(ctx, tenantID), cc)

			// A fresh, unique key per run -- see a_blob_routes_work_on_every_
			// dialect_test.go's identical rationale.
			key := "host-get-test-" + uuid.New().String()
			body := "blob body for " + key

			putInput, err := json.Marshal(blobPutInput{
				Key:         key,
				ContentType: "text/plain",
				Data:        []byte(body),
			})
			if err != nil {
				t.Fatalf("marshal put input: %v", err)
			}
			if _, err := p.blobPut(hostCtx, string(putInput)); err != nil {
				t.Fatalf("blobPut on %s: %v", be.Name, err)
			}

			// blobGet, the actual bug this test pins. cleat#2206's audit found
			// this 500ing on MSSQL with "Incorrect syntax near the keyword
			// 'key'".
			getInput, err := json.Marshal(blobGetInput{Key: key})
			if err != nil {
				t.Fatalf("marshal get input: %v", err)
			}
			outJSON, err := p.blobGet(hostCtx, string(getInput))
			if err != nil {
				t.Fatalf("blobGet on %s: %v", be.Name, err)
			}

			var out blobGetOutput
			if err := json.Unmarshal([]byte(outJSON), &out); err != nil {
				t.Fatalf("decode blobGet output on %s: %v", be.Name, err)
			}
			if out.Key != key {
				t.Errorf("blobGet on %s: key = %q, want %q", be.Name, out.Key, key)
			}
			if out.ContentType != "text/plain" {
				t.Errorf("blobGet on %s: content_type = %q, want %q", be.Name, out.ContentType, "text/plain")
			}
			// Data comes through as base64 inside outJSON via the []byte tag;
			// after json.Unmarshal into blobGetOutput it is already decoded
			// bytes. Confirm the round-trip directly rather than assuming the
			// tag behaved -- json.RawMessage/[]byte handling is exactly this
			// file's subject.
			if got := string(out.Data); got != body {
				t.Errorf("blobGet on %s: data = %q, want %q", be.Name, got, body)
			}

			// Independently confirm Data really was base64 in the wire JSON
			// (encoding/json's documented []byte behavior), so a future
			// change to blobGetOutput's tag is caught here rather than only
			// by the struct-level assertion above.
			var raw map[string]json.RawMessage
			if err := json.Unmarshal([]byte(outJSON), &raw); err != nil {
				t.Fatalf("decode raw output on %s: %v", be.Name, err)
			}
			var b64 string
			if err := json.Unmarshal(raw["data"], &b64); err != nil {
				t.Fatalf("data field on %s is not a JSON string: %v", be.Name, err)
			}
			if decoded, err := base64.StdEncoding.DecodeString(b64); err != nil || string(decoded) != body {
				t.Errorf("blobGet on %s: wire data %q did not base64-decode to %q", be.Name, b64, body)
			}
		})
	}
}
