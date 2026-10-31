package auditlog

// cleat#3578: an operator handler can put a document on its audit row (auth.NoteAuditMetadata), and
// the row stores it in the metadata column AND the chain hash covers it.
//
// The first caller is the recovery-key route, which records the reason an operator gave. The claim
// worth testing is not "the column is written" but "it is written where the tenant's own chain can
// vouch for it": a reason an operator could later edit without the chain noticing is a log entry and
// not an audit record. So this goes through the real middleware into a REAL database, then verifies the
// chain, then edits the stored metadata and verifies that the chain now reports a break.
//
// Controls, because each claim needs the case beside it that is still what it was: a request that notes
// nothing stores the "{}" every row stored before this existed, and a request that is not an operator's
// ignores the slot (only an operator request listens to it, as with the target).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/plugin"
)

func TestAnOperatorHandlersNotedMetadataIsStoredAndHashed(t *testing.T) {
	forEachChainDialect(t, func(t *testing.T, e *chainEnv) {
		p := e.plugin()
		withTarget := uuid.New()
		noteless := uuid.New()
		notOperator := uuid.New()

		const reason = "locked out: the last key was revoked with force=true on 2026-10-30"
		doc, _ := json.Marshal(map[string]string{"reason": reason})
		mux := http.NewServeMux()
		mux.HandleFunc("/api/admin/with-reason", func(_ http.ResponseWriter, r *http.Request) {
			auth.NoteAuditTarget(r.Context(), withTarget)
			auth.NoteAuditMetadata(r.Context(), string(doc))
			auth.NoteAuditMetadata(r.Context(), `{"reason":"a later step must not overwrite the first"}`)
		})
		mux.HandleFunc("/api/admin/no-reason", func(_ http.ResponseWriter, r *http.Request) {
			auth.NoteAuditTarget(r.Context(), noteless)
		})
		mux.HandleFunc("/api/admin/tenant-key-notes", func(_ http.ResponseWriter, r *http.Request) {
			auth.NoteAuditMetadata(r.Context(), string(doc))
		})
		audited := p.Middleware(mux)

		serve := func(path string, asOperator bool, tenant uuid.UUID) {
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(""))
			ctx := req.Context()
			if asOperator {
				ctx = auth.WithOperator(ctx, auth.Operator{KeyID: "8d264f39-9c52-4623-aa1d-258c7bcc34d7", Scopes: auth.ScopeAdmin})
			} else {
				ctx = auth.WithSubject(auth.WithTenantID(ctx, tenant), "key-of-a-tenant")
			}
			audited.ServeHTTP(httptest.NewRecorder(), req.WithContext(ctx))
		}
		stored := func(tenant uuid.UUID) string {
			var m string
			e.scan(tenant, `SELECT metadata FROM audit_events WHERE tenant_id = $1`, []any{tenant.String()}, &m)
			return m
		}

		serve("/api/admin/with-reason", true, uuid.Nil)
		serve("/api/admin/no-reason", true, uuid.Nil)
		serve("/api/admin/tenant-key-notes", false, notOperator)

		// 1. The reason is on the TARGET tenant's chain, as the first document noted, and decodes to itself.
		var got map[string]string
		if err := json.Unmarshal([]byte(stored(withTarget)), &got); err != nil || got["reason"] != reason {
			t.Fatalf("the noted reason is stored as %q (%v), want %q: the middleware did not carry it to the row", stored(withTarget), err, reason)
		}
		// 2. Hashed: the chain verifies as written ...
		if rep := e.verify(withTarget); !rep.OK() || rep.Checked != 1 {
			t.Fatalf("a chain nobody tampered with does not verify: %+v", rep)
		}
		// ... and editing the stored reason breaks it. THE KNOWN POSITIVE: without this a chain that ignored
		// the metadata would pass the line above too.
		upd := map[plugin.Dialect]string{
			plugin.DialectPostgres: `UPDATE audit_events SET metadata = '{"reason":"nothing happened"}' WHERE tenant_id = $1`,
			plugin.DialectMySQL:    `UPDATE audit_events SET metadata = '{"reason":"nothing happened"}' WHERE tenant_id = $1`,
			plugin.DialectMSSQL:    `UPDATE audit_events SET metadata = N'{"reason":"nothing happened"}' WHERE tenant_id = $1`,
		}[e.d.dialect]
		e.mustChange(withTarget, upd, withTarget.String())
		if rep := e.verify(withTarget); rep.OK() || rep.Break.Kind != BreakEdited {
			t.Fatalf("an edited reason verifies as %+v: the chain hash does not cover the metadata", rep)
		}

		// 3. Controls: nothing noted stores {} as before, and a non-operator request ignores the slot.
		if m := strings.ReplaceAll(stored(noteless), " ", ""); m != "{}" {
			t.Errorf("a request that noted no metadata stored %q, want {}", m)
		}
		if rep := e.verify(noteless); !rep.OK() {
			t.Errorf("a row with no metadata does not verify: %+v", rep)
		}
		if m := strings.ReplaceAll(stored(notOperator), " ", ""); m != "{}" {
			t.Errorf("a non-operator request stored %q: only an operator request may record a document", m)
		}
	})
}
