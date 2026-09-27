package blobstore

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

// TestABlobRoutesWorkOnEveryDialect pins cleat#2206, two bugs deep.
//
// 1. handleList built its row limit as a literal "LIMIT $N", which SQL
// Server rejects outright -- "Incorrect syntax near 'LIMIT'", a 500 on
// every call to GET /blobs. Same bug class, same fix (plugin.LimitClause)
// as #2191's /audit/events and #2198's notifications/webhookingest list
// endpoints -- #2206's audit found this site missed by both.
//
// 2. Fixing that uncovered a second, more severe bug this test now pins
// too: handleGet, handleHead, handleDelete and handleList all referenced
// the blob_index table's key column as a bare "i.key"/"key". key is a
// reserved word in MySQL and SQL Server both, and an unquoted reference
// 500ed on every one of these routes on SQL Server -- not just list, the
// entire read/delete path. plugin.QuoteIdent exists for exactly this
// (kvstore and featureflags already carry the fix); blobstore/routes.go
// was missed. See plugin.QuoteIdent.
func TestABlobRoutesWorkOnEveryDialect(t *testing.T) {
	for _, be := range testutil.NewPluginTestBackends(t) {
		be := be
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
			tenantCtx := auth.WithTenantID(context.Background(), tenantID)

			// A fresh, unique key prefix per run: this test's own isolation
			// does not depend on the backing database being empty, which a
			// locally reused container is not.
			prefix := "list-test-" + uuid.New().String() + "/"
			keyOne, keyTwo := prefix+"one", prefix+"two"

			// PUT two blobs through the real route, not seeded directly, so
			// this proves handlePut's own writes too.
			for _, key := range []string{keyOne, keyTwo} {
				putReq := httptest.NewRequest("PUT", "/blobs/"+key,
					strings.NewReader("blob body for "+key)).WithContext(tenantCtx)
				putReq.SetPathValue("key", key)
				putRec := httptest.NewRecorder()
				p.handlePut(putRec, putReq)
				if putRec.Code != http.StatusCreated {
					t.Fatalf("put %s: want 201, got %d: %s", key, putRec.Code, putRec.Body.String())
				}
			}

			// GET /blobs/{key}, through the real route. cleat#2206's audit
			// found this 500ing on MSSQL with "Incorrect syntax near the
			// keyword 'key'" -- unrelated to LIMIT, and not caught by
			// fixing only handleList.
			getReq := httptest.NewRequest("GET", "/blobs/"+keyOne, nil).WithContext(tenantCtx)
			getReq.SetPathValue("key", keyOne)
			getRec := httptest.NewRecorder()
			p.handleGet(getRec, getReq)
			if getRec.Code != http.StatusOK {
				t.Fatalf("get %s: want 200, got %d: %s", keyOne, getRec.Code, getRec.Body.String())
			}
			if got := getRec.Body.String(); got != "blob body for "+keyOne {
				t.Errorf("get %s body: got %q, want %q", keyOne, got, "blob body for "+keyOne)
			}

			// HEAD /blobs/{key}, same bug, same fix.
			headReq := httptest.NewRequest("HEAD", "/blobs/"+keyOne, nil).WithContext(tenantCtx)
			headReq.SetPathValue("key", keyOne)
			headRec := httptest.NewRecorder()
			p.handleHead(headRec, headReq)
			if headRec.Code != http.StatusOK {
				t.Fatalf("head %s: want 200, got %d", keyOne, headRec.Code)
			}

			// GET /blobs?prefix=..., through the real route. cleat#2206's
			// audit found this 500ing on MSSQL with "Incorrect syntax near
			// 'LIMIT'".
			listReq := httptest.NewRequest("GET", "/blobs?prefix="+prefix, nil).WithContext(tenantCtx)
			listRec := httptest.NewRecorder()
			p.handleList(listRec, listReq)
			if listRec.Code != http.StatusOK {
				t.Fatalf("list blobs: want 200, got %d: %s", listRec.Code, listRec.Body.String())
			}
			var listed []map[string]any
			if err := json.Unmarshal(listRec.Body.Bytes(), &listed); err != nil {
				t.Fatalf("decode blob list: %v", err)
			}
			if len(listed) != 2 {
				t.Fatalf("blob list: got %d entries, want 2: %s", len(listed), listRec.Body.String())
			}

			// limit=1, through the query string: proves the placeholder this
			// PR's fix actually binds and actually constrains the row count --
			// not just that the endpoint returns 200 with everything anyway.
			limitedReq := httptest.NewRequest("GET", "/blobs?prefix="+prefix+"&limit=1", nil).WithContext(tenantCtx)
			limitedRec := httptest.NewRecorder()
			p.handleList(limitedRec, limitedReq)
			if limitedRec.Code != http.StatusOK {
				t.Fatalf("list blobs (limit=1): want 200, got %d: %s", limitedRec.Code, limitedRec.Body.String())
			}
			var limitedListed []map[string]any
			if err := json.Unmarshal(limitedRec.Body.Bytes(), &limitedListed); err != nil {
				t.Fatalf("decode blob list (limit=1): %v", err)
			}
			if len(limitedListed) != 1 {
				t.Fatalf("blob list (limit=1): got %d entries, want 1: %s", len(limitedListed), limitedRec.Body.String())
			}

			// DELETE /blobs/{key}, through the real route -- same bug, same
			// fix -- then confirm the soft-delete actually took (a
			// subsequent GET 404s).
			deleteReq := httptest.NewRequest("DELETE", "/blobs/"+keyTwo, nil).WithContext(tenantCtx)
			deleteReq.SetPathValue("key", keyTwo)
			deleteRec := httptest.NewRecorder()
			p.handleDelete(deleteRec, deleteReq)
			if deleteRec.Code != http.StatusNoContent {
				t.Fatalf("delete %s: want 204, got %d: %s", keyTwo, deleteRec.Code, deleteRec.Body.String())
			}

			getDeletedReq := httptest.NewRequest("GET", "/blobs/"+keyTwo, nil).WithContext(tenantCtx)
			getDeletedReq.SetPathValue("key", keyTwo)
			getDeletedRec := httptest.NewRecorder()
			p.handleGet(getDeletedRec, getDeletedReq)
			if getDeletedRec.Code != http.StatusNotFound {
				t.Fatalf("get %s after delete: want 404, got %d: %s", keyTwo, getDeletedRec.Code, getDeletedRec.Body.String())
			}
		})
	}
}
