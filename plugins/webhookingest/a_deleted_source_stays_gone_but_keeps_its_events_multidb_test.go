package webhookingest

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
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

// TestADeletedSourceStaysGoneButKeepsItsEvents is cleat#2199's route-level
// pin, across all three real dialects.
//
// webhook_events.source_id REFERENCES webhook_sources(id) with no ON DELETE
// action, so a real DELETE FROM webhook_sources 500s on PostgreSQL and SQL
// Server for any source with at least one event, and on MySQL succeeds while
// orphaning that source's webhook_events rows (InnoDB ignores an
// inline-column REFERENCES). handleDeleteSource no longer removes the row at
// all: it sets enabled = false and deleted_at, so the FK is never exercised
// on any dialect.
//
// This test walks the whole lifecycle through the real routes -- create,
// ingest, delete -- and checks every consequence of that choice in one place:
// the source becomes unreachable (404, not 403 -- deleted reads as gone, not
// merely disabled), a second delete is idempotent-as-404 rather than a 500,
// new ingestion is refused, and -- the point of soft-delete over cascade --
// the event ingested before the delete is still there afterward.
func TestADeletedSourceStaysGoneButKeepsItsEvents(t *testing.T) {
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

			// A real SecretStore, same fixed test key as this package's sibling
			// multidb tests -- reproduced here rather than shared, for the same
			// reason given there: nothing in this package can import a helper
			// from the harness without cycling.
			key := make([]byte, 32)
			for i := range key {
				key[i] = 0x5a
			}
			ring, err := engine.NewKeyRing(engine.VersionedKey{Version: 1, Key: key})
			if err != nil {
				t.Fatalf("build key ring: %v", err)
			}
			secretStore := engine.NewSecretStoreWithRing(be.DB, string(dialect), ring)
			p.db = &engine.SQLDBAdapter{DB: be.DB, Dialect: dialect}
			p.secrets = engine.NewPluginSecrets(secretStore)
			p.env = &plugin.Environment{Dialect: dialect, Logger: quiet}

			tenantID := uuid.MustParse(engine.DefaultTenantUUID)
			tenantCtx := auth.WithTenantID(context.Background(), tenantID)

			// Create a signed source through the real route.
			const rawSecret = "delete-lifecycle-secret"
			createBody := `{"name":"deleted-source","source_type":"github","secret":"` + rawSecret + `"}`
			createReq := httptest.NewRequest("POST", "/ingest/sources",
				strings.NewReader(createBody)).WithContext(tenantCtx)
			createRec := httptest.NewRecorder()
			p.handleCreateSource(createRec, createReq)
			if createRec.Code != http.StatusCreated {
				t.Fatalf("create source: want 201, got %d: %s", createRec.Code, createRec.Body.String())
			}
			var created map[string]any
			if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
				t.Fatalf("decode create response: %v", err)
			}
			sourceID := created["id"].(string)

			// Ingest one event BEFORE the delete, through the real route -- this
			// is the row that has to survive.
			payload := []byte(`{"pre":"delete"}`)
			mac := hmac.New(sha256.New, []byte(rawSecret))
			mac.Write(payload)
			sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

			ingestBefore := httptest.NewRequest("POST", "/ingest/"+sourceID, bytes.NewReader(payload))
			ingestBefore.SetPathValue("source_id", sourceID)
			ingestBefore.Header.Set("X-Hub-Signature-256", sig)
			ingestBefore.Header.Set("X-Event-Type", "pre.delete")
			ingestBeforeRec := httptest.NewRecorder()
			p.handleIngestWebhook(ingestBeforeRec, ingestBefore)
			if ingestBeforeRec.Code != http.StatusCreated {
				t.Fatalf("ingest before delete: want 201, got %d: %s",
					ingestBeforeRec.Code, ingestBeforeRec.Body.String())
			}

			// PRECONDITION: the source is visible in both GET and LIST before the
			// delete. Scanned by id rather than assuming list length or emptiness
			// -- webhook_sources has no per-test isolation, so other tests'
			// sources from earlier runs against this database may well be
			// present too.
			if code := getSourceCode(t, p, tenantCtx, sourceID); code != http.StatusOK {
				t.Fatalf("PRECONDITION FAILED: GET source before delete: want 200, got %d", code)
			}
			if !listContainsSource(t, p, tenantCtx, sourceID) {
				t.Fatalf("PRECONDITION FAILED: source missing from the list before it was ever deleted")
			}

			// The delete itself.
			deleteReq := httptest.NewRequest("DELETE", "/ingest/sources/"+sourceID, nil).WithContext(tenantCtx)
			deleteReq.SetPathValue("id", sourceID)
			deleteRec := httptest.NewRecorder()
			p.handleDeleteSource(deleteRec, deleteReq)
			if deleteRec.Code != http.StatusNoContent {
				t.Fatalf("delete source: want 204, got %d: %s", deleteRec.Code, deleteRec.Body.String())
			}

			// GET now reads 404 -- gone, not merely disabled.
			if code := getSourceCode(t, p, tenantCtx, sourceID); code != http.StatusNotFound {
				t.Errorf("GET source after delete: want 404, got %d", code)
			}

			// The row itself SURVIVES, checked directly against the database
			// rather than through any route -- every route filters deleted_at
			// IS NULL, so a 404 above is consistent with either a soft delete
			// (row present, deleted_at set) or a real DELETE (row gone). Only
			// this distinguishes them, and it is the one assertion that would
			// have passed against the pre-cleat#2199 hard DELETE on MySQL:
			// InnoDB ignores an inline-column REFERENCES, so a hard DELETE
			// there succeeds and every route-level check above would read
			// exactly the same regardless of which code path produced it.
			// CrossTenantConn, not be.DB directly: webhook_sources is
			// TenantScoped and SQL Server's policy applies to sysadmin/dbo
			// just as it does to anyone else (unlike PostgreSQL, where a
			// superuser connection bypasses RLS unconditionally) -- a bare
			// be.DB read here would find 0 rows whether the row was deleted
			// or merely hidden, which is exactly the ambiguity this
			// assertion exists to resolve.
			readConn := be.CrossTenantConn(t, ctx, "cleat#2199: confirming a deleted source's row survives")
			var rowStillThere int
			if err := readConn.QueryRowContext(ctx, plugin.Rebind(
				`SELECT count(*) FROM webhook_sources WHERE id = $1`, dialect),
				sourceID).Scan(&rowStillThere); err != nil {
				t.Fatalf("count webhook_sources row after delete: %v", err)
			}
			if rowStillThere != 1 {
				t.Errorf("webhook_sources row after delete: got %d, want 1 -- "+
					"the delete must not remove the row (that is what makes it FK-safe "+
					"on every dialect); it should be marked deleted, not gone", rowStillThere)
			}

			// The list no longer contains it.
			if listContainsSource(t, p, tenantCtx, sourceID) {
				t.Errorf("source still present in the list after delete")
			}

			// A second delete of the same source is idempotent-as-404: the row
			// was never removed, so "not found" is exactly what a first-time
			// delete of a nonexistent id already returned before cleat#2199 --
			// this is not a new contract, just one no longer tied to the row
			// having been physically removed.
			deleteAgainReq := httptest.NewRequest("DELETE", "/ingest/sources/"+sourceID, nil).WithContext(tenantCtx)
			deleteAgainReq.SetPathValue("id", sourceID)
			deleteAgainRec := httptest.NewRecorder()
			p.handleDeleteSource(deleteAgainRec, deleteAgainReq)
			if deleteAgainRec.Code != http.StatusNotFound {
				t.Errorf("second delete: want 404, got %d: %s", deleteAgainRec.Code, deleteAgainRec.Body.String())
			}

			// New ingestion is refused -- this is the security-critical half:
			// even with a perfectly valid signature (the secret was retired by
			// the delete, but the check that matters here is deleted_at, so this
			// probes it with a correctly-signed request rather than relying on
			// the secret check to be the one that catches it).
			payload2 := []byte(`{"post":"delete"}`)
			mac2 := hmac.New(sha256.New, []byte(rawSecret))
			mac2.Write(payload2)
			sig2 := "sha256=" + hex.EncodeToString(mac2.Sum(nil))
			ingestAfter := httptest.NewRequest("POST", "/ingest/"+sourceID, bytes.NewReader(payload2))
			ingestAfter.SetPathValue("source_id", sourceID)
			ingestAfter.Header.Set("X-Hub-Signature-256", sig2)
			ingestAfter.Header.Set("X-Event-Type", "post.delete")
			ingestAfterRec := httptest.NewRecorder()
			p.handleIngestWebhook(ingestAfterRec, ingestAfter)
			if ingestAfterRec.Code != http.StatusNotFound {
				t.Errorf("ingest after delete: want 404, got %d: %s",
					ingestAfterRec.Code, ingestAfterRec.Body.String())
			}

			// The point of soft-delete over cascade: the event ingested BEFORE
			// the delete is still readable afterward. GET /ingest/events is
			// deliberately not filtered by the owning source's deleted state --
			// see handleListEvents's doc comment.
			listEventsReq := httptest.NewRequest("GET", "/ingest/events?source_id="+sourceID, nil).
				WithContext(tenantCtx)
			listEventsRec := httptest.NewRecorder()
			p.handleListEvents(listEventsRec, listEventsReq)
			if listEventsRec.Code != http.StatusOK {
				t.Fatalf("list events after delete: want 200, got %d: %s",
					listEventsRec.Code, listEventsRec.Body.String())
			}
			var events []map[string]any
			if err := json.Unmarshal(listEventsRec.Body.Bytes(), &events); err != nil {
				t.Fatalf("decode events list: %v", err)
			}
			if len(events) != 1 {
				t.Fatalf("events list after delete: got %d entries, want 1 (the pre-delete event) -- "+
					"a count of 2 would mean the post-delete POST was wrongly accepted; a count of 0 "+
					"would mean the delete destroyed history it should have kept: %s",
					len(events), listEventsRec.Body.String())
			}
			if events[0]["event_type"] != "pre.delete" {
				t.Errorf("surviving event_type: got %v, want %q", events[0]["event_type"], "pre.delete")
			}
		})
	}
}

func getSourceCode(t *testing.T, p *Plugin, tenantCtx context.Context, sourceID string) int {
	t.Helper()
	req := httptest.NewRequest("GET", "/ingest/sources/"+sourceID, nil).WithContext(tenantCtx)
	req.SetPathValue("id", sourceID)
	rec := httptest.NewRecorder()
	p.handleGetSource(rec, req)
	return rec.Code
}

func listContainsSource(t *testing.T, p *Plugin, tenantCtx context.Context, sourceID string) bool {
	t.Helper()
	req := httptest.NewRequest("GET", "/ingest/sources", nil).WithContext(tenantCtx)
	rec := httptest.NewRecorder()
	p.handleListSources(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list sources: want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var sources []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &sources); err != nil {
		t.Fatalf("decode sources list: %v", err)
	}
	for _, s := range sources {
		if s["id"] == sourceID {
			return true
		}
	}
	return false
}
