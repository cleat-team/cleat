package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine/testutil"
)

// cleat#3552, B3b-2b. DELETE /api/admin/tenants/{tenant}/tombstone, through composeAuthChain.
//
// The fake-provisioner tests are about WHO may do it and what is checked before the store is touched. The
// last test puts the real store behind the real handler on PostgreSQL and SQL Server, because "the reply
// equals what the row held" is a claim about a row, and a fake can only repeat what it was given.

const tombTenantID = "aaaaaaaa-0000-4000-8000-0000000000b2"

func tombPath(tenant string) string { return "/api/admin/tenants/" + tenant + "/tombstone" }

func heldTombstone() auth.Tombstone {
	seq, hash := int64(41), strings.Repeat("cd", 32)
	return auth.Tombstone{
		TenantID: uuid.MustParse(tombTenantID), Name: "acme", OrgID: uuid.MustParse("bbbbbbbb-0000-4000-8000-000000000002"),
		CreatedBy: "clew-service", DroppedBy: "operator:somebody",
		DroppedAt:     time.Date(2026, 10, 30, 4, 0, 0, 123456000, time.UTC),
		AuditChainSeq: &seq, AuditChainHash: &hash,
	}
}

// Nothing is read or changed until the caller has typed the id back. The control is the same request with
// the right header, without which every refusal is satisfied by a route that refuses everything.
func TestATombstoneDeleteNeedsTheTenantIdTypedBack(t *testing.T) {
	f, fp := buildProvisionHTTP(t, nil)
	fp.tombstone = heldTombstone()

	for name, confirm := range map[string]string{
		"no header":        "",
		"another tenant":   "bbbbbbbb-0000-4000-8000-000000000002",
		"the word confirm": "confirm",
		"upper case":       strings.ToUpper(tombTenantID),
		"trailing space":   tombTenantID + " ",
	} {
		if w := doDrop(f.chainFixture, tombPath(tombTenantID), f.adminKey, confirm); w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s; want 400", name, w.Code, w.Body.String())
		}
	}
	if fp.tombstoneCalls != 0 {
		t.Fatalf("the store was asked to delete a tombstone %d time(s) without a valid X-Confirm", fp.tombstoneCalls)
	}
	if w := doDrop(f.chainFixture, tombPath(tombTenantID), f.adminKey, tombTenantID); w.Code != http.StatusOK {
		t.Fatalf("CONTROL FAILED: the right header = %d %s", w.Code, w.Body.String())
	}
	if fp.tombstoneCalls != 1 || fp.gotTombstoneID.String() != tombTenantID {
		t.Errorf("the store was asked %d time(s) for %v; want once for %s", fp.tombstoneCalls, fp.gotTombstoneID, tombTenantID)
	}
}

// A provision-scoped key can never reach this: it is the whole reason reuse is its own route. The gate
// refuses it, and the handler refuses it too, tested separately so that neither layer is the only thing
// holding the line.
func TestAProvisionKeyCanNeverDeleteATombstone(t *testing.T) {
	f, fp := buildProvisionHTTP(t, nil)
	fp.tombstone = heldTombstone()

	// Through the chain, with the RIGHT header: the scope gate answers.
	if w := doDrop(f.chainFixture, tombPath(tombTenantID), f.provisionKey, tombTenantID); w.Code != http.StatusForbidden {
		t.Errorf("a provision key through the gate = %d %s; want 403", w.Code, w.Body.String())
	}
	// With a wrong header too: the gate comes first, so it does not even learn the header was wrong.
	if w := doDrop(f.chainFixture, tombPath(tombTenantID), f.provisionKey, "nope"); w.Code != http.StatusForbidden {
		t.Errorf("a provision key, wrong header = %d; want 403 from the gate", w.Code)
	}

	// The handler alone, with a provision-scoped operator in the context and NO gate in front of it.
	req := httptest.NewRequest(http.MethodDelete, tombPath(tombTenantID), nil)
	req.Header.Set("X-Confirm", tombTenantID)
	req = req.WithContext(auth.WithOperator(req.Context(), auth.Operator{KeyID: "dddddddd-0000-4000-8000-000000000004", Scopes: auth.ScopeProvision, Principal: "clew-service"}))
	w := httptest.NewRecorder()
	f.api.handleDeleteTombstone(w, req, tombTenantID)
	if w.Code != http.StatusForbidden {
		t.Errorf("the handler for a provision-scoped operator, no gate = %d %s; want 403", w.Code, w.Body.String())
	}
	if fp.tombstoneCalls != 0 {
		t.Fatalf("a provision-scoped key reached the store %d time(s)", fp.tombstoneCalls)
	}

	// CONTROL: the admin key, same path, same header, is let through.
	if w := doDrop(f.chainFixture, tombPath(tombTenantID), f.adminKey, tombTenantID); w.Code != http.StatusOK {
		t.Fatalf("CONTROL FAILED: admin = %d %s", w.Code, w.Body.String())
	}
	// A tenant key and no key are not operators.
	for name, key := range map[string]string{"a tenant key": f.tenantAKey, "no key": ""} {
		if w := doDrop(f.chainFixture, tombPath(tombTenantID), key, tombTenantID); w.Code != http.StatusUnauthorized && w.Code != http.StatusForbidden {
			t.Errorf("%s = %d %s; want a refusal", name, w.Code, w.Body.String())
		}
	}
	if fp.tombstoneCalls != 1 {
		t.Errorf("the store was reached %d times; want only the admin's one", fp.tombstoneCalls)
	}
}

func TestATombstoneDeleteAnswersEachOutcomeWithItsOwnShape(t *testing.T) {
	f, fp := buildProvisionHTTP(t, nil)
	del := func(path, confirm string) *httptest.ResponseRecorder {
		return doDrop(f.chainFixture, path, f.adminKey, confirm)
	}

	fp.tombstoneErr = auth.ErrTombstoneNotFound
	if w := del(tombPath(tombTenantID), tombTenantID); w.Code != http.StatusNotFound {
		t.Errorf("no tombstone = %d %s; want 404 (a repeat is not a 200)", w.Code, w.Body.String())
	}

	// An unexplained error is a 500 that does not repeat itself.
	fp.tombstoneErr = errors.New("pq: relation \"admin.tenant_tombstones\" is on fire")
	w := del(tombPath(tombTenantID), tombTenantID)
	if w.Code != http.StatusInternalServerError || strings.Contains(w.Body.String(), "on fire") || strings.Contains(w.Body.String(), "tenant_tombstones") {
		t.Errorf("a store error = %d %s; want a 500 that does not repeat the error", w.Code, w.Body.String())
	}

	fp.tombstoneErr = nil
	fp.tombstone = heldTombstone()
	before := fp.tombstoneCalls
	for name, c := range map[string]struct{ path, confirm string }{
		"not a uuid":     {tombPath("not-a-uuid"), "not-a-uuid"},
		"the nil tenant": {tombPath(uuid.Nil.String()), uuid.Nil.String()},
	} {
		if w := del(c.path, c.confirm); w.Code != http.StatusBadRequest {
			t.Errorf("%s = %d %s; want 400", name, w.Code, w.Body.String())
		}
	}
	// Only DELETE is the route.
	for _, m := range []string{http.MethodGet, http.MethodPost, http.MethodPut} {
		req := httptest.NewRequest(m, tombPath(tombTenantID), nil)
		req.Header.Set("Authorization", "Bearer "+f.adminKey)
		req.Header.Set("X-Confirm", tombTenantID)
		rec := httptest.NewRecorder()
		f.handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s = %d %s; want 405", m, rec.Code, rec.Body.String())
		}
	}
	if fp.tombstoneCalls != before {
		t.Errorf("a malformed or wrong-method request reached the store (%d call(s))", fp.tombstoneCalls-before)
	}
}

type tombstoneReply struct {
	TenantID         string         `json:"tenant_id"`
	TombstoneDeleted bool           `json:"tombstone_deleted"`
	Tombstone        map[string]any `json:"tombstone"`
}

// The reply carries EVERYTHING the row held, because it is the last place that exists, and a tombstone with
// no chain and no creator says so with nulls and not with made-up values.
func TestTheTombstoneReplyCarriesEverythingTheRowHeld(t *testing.T) {
	f, fp := buildProvisionHTTP(t, nil)

	fp.tombstone = heldTombstone()
	w := doDrop(f.chainFixture, tombPath(tombTenantID), f.adminKey, tombTenantID)
	var got tombstoneReply
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != http.StatusOK {
		t.Fatalf("%d %q: %v", w.Code, w.Body.String(), err)
	}
	if !got.TombstoneDeleted || got.TenantID != tombTenantID {
		t.Errorf("reply = %+v", got)
	}
	want := map[string]any{
		"tenant_id": tombTenantID, "name": "acme", "org_id": "bbbbbbbb-0000-4000-8000-000000000002",
		"created_by": "clew-service", "dropped_by": "operator:somebody", "dropped_at": "2026-10-30T04:00:00.123456Z",
		"audit_chain": map[string]any{"seq": float64(41), "hash": strings.Repeat("cd", 32)},
	}
	wantJSON, _ := json.Marshal(want)
	gotJSON, _ := json.Marshal(got.Tombstone)
	if string(wantJSON) != string(gotJSON) {
		t.Errorf("tombstone in the reply:\n got %s\nwant %s", gotJSON, wantJSON)
	}

	// No chain and no creator: null, not "" and not zero.
	bare := heldTombstone()
	bare.CreatedBy, bare.AuditChainSeq, bare.AuditChainHash = "", nil, nil
	fp.tombstone = bare
	w = doDrop(f.chainFixture, tombPath(tombTenantID), f.adminKey, tombTenantID)
	got = tombstoneReply{}
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.Tombstone["audit_chain"] != nil || got.Tombstone["created_by"] != nil {
		t.Errorf("a tombstone with no chain and no creator = %v; want both null", got.Tombstone)
	}
}

// The log carries the same fields (observability data under D49-1a: ids, a name, a position, a hash), and the
// audit row goes on the default tenant's chain because the handler names no target.
func TestTheTombstoneDeleteIsLoggedAndNamesNoAuditTarget(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })

	f, fp := buildProvisionHTTP(t, slotAround)
	fp.tombstone = heldTombstone()

	// CONTROL: this harness can see a handler name a target. Without it "noted nothing" below is satisfied by
	// a harness that records nothing at all.
	fp.result = okResult(auth.ProvisionCreated, true)
	doBody(f.chainFixture, http.MethodPost, "/api/admin/tenants", f.provisionKey, `{"org_name":"x"}`)
	if !f.noted.set {
		t.Fatalf("UNMEASURED: a create that names its tenant left the audit slot unset, so the harness cannot see a target")
	}
	*f.noted = notedTarget{}

	if w := doDrop(f.chainFixture, tombPath(tombTenantID), f.adminKey, tombTenantID); w.Code != http.StatusOK {
		t.Fatalf("delete = %d %s", w.Code, w.Body.String())
	}
	out := logs.String()
	if !strings.Contains(out, "deleted a tenant tombstone") {
		t.Fatalf("UNMEASURED: the handler logged nothing, so the checks below prove nothing: %q", out)
	}
	for _, field := range []string{tombTenantID, "acme", "bbbbbbbb-0000-4000-8000-000000000002", "clew-service", "operator:somebody",
		"2026-10-30T04:00:00.123456Z", "audit_chain_seq=41", "audit_chain_hash=" + strings.Repeat("cd", 32), "deleted_by="} {
		if !strings.Contains(out, field) {
			t.Errorf("the WARN line does not carry %q: %s", field, out)
		}
	}
	for _, secret := range []string{f.adminKey, f.provisionKey} {
		if strings.Contains(out, secret) {
			t.Errorf("an operator key reached the log")
		}
	}
	if f.noted.set {
		t.Errorf("the handler noted audit target %v: the row would be filed under a chain the drop already deleted", f.noted.tenant)
	}
}

// The whole thing on real databases: the real store behind the real handler. The expected values are read from
// the table with plain SQL BEFORE the delete, by code that shares nothing with the store, so "the reply equals the
// row" compares two readings and not the store with itself.
func TestATombstoneIsDeletedOverHTTPAgainstRealDatabases(t *testing.T) {
	for _, dialect := range []string{auth.DialectPostgres, auth.DialectMSSQL} {
		dialect := dialect
		t.Run(dialect, func(t *testing.T) {
			db := testutil.TestDB(t, testutil.Dialect(dialect))
			ts, err := auth.NewTenantStoreForDialect(db, dialect)
			if err != nil {
				t.Fatal(err)
			}
			f, _ := buildProvisionHTTP(t, slotAround)
			f.api.provisioner = ts
			ctx := context.Background()

			ph := func(n int) string {
				if dialect == auth.DialectMSSQL {
					return "@p" + string(rune('0'+n))
				}
				return "$" + string(rune('0'+n))
			}
			target, bystander := uuid.New(), uuid.New()
			orgID := uuid.New()
			orgName := "b3b2b-http-" + uuid.NewString()
			seed := func(id uuid.UUID, createdBy any, seq, hash any) {
				t.Helper()
				if _, err := db.ExecContext(ctx, `INSERT INTO admin.tenant_tombstones (dropped_tenant_id, name, org_id, created_by, dropped_by, audit_chain_seq, audit_chain_hash)
					VALUES (`+ph(1)+`,`+ph(2)+`,`+ph(3)+`,`+ph(4)+`,`+ph(5)+`,`+ph(6)+`,`+ph(7)+`)`,
					id.String(), "tomb-"+id.String(), orgID.String(), createdBy, "operator:seed", seq, hash); err != nil {
					t.Fatalf("seed tombstone (%s): %v", dialect, err)
				}
			}
			count := func(id uuid.UUID) int {
				var n int
				if err := db.QueryRowContext(ctx, `SELECT count(*) FROM admin.tenant_tombstones WHERE dropped_tenant_id = `+ph(1), id.String()).Scan(&n); err != nil {
					t.Fatal(err)
				}
				return n
			}
			t.Cleanup(func() {
				for _, id := range []uuid.UUID{target, bystander} {
					_, _ = db.Exec(`DELETE FROM admin.tenant_tombstones WHERE dropped_tenant_id = `+ph(1), id.String())
					_, _ = db.Exec(`DELETE FROM admin.tenant_api_keys WHERE tenant_id = `+ph(1), id.String())
					_, _ = db.Exec(`DELETE FROM admin.tenants WHERE tenant_id = `+ph(1), id.String())
				}
				_, _ = db.Exec(`DELETE FROM admin.orgs WHERE name = `+ph(1), orgName)
			})
			seed(target, "clew-service", int64(41), chainHash)
			seed(bystander, nil, nil, nil)

			// The row, read with plain SQL, before anything happens.
			var name, org, createdBy, droppedBy string
			var createdByNull sql.NullString
			var droppedAt time.Time
			var seq sql.NullInt64
			var hash sql.NullString
			orgCol := "org_id::text"
			if dialect == auth.DialectMSSQL {
				orgCol = "CONVERT(NVARCHAR(36), org_id)"
			}
			if err := db.QueryRowContext(ctx, `SELECT name, `+orgCol+`, created_by, dropped_by, dropped_at, audit_chain_seq, audit_chain_hash
				FROM admin.tenant_tombstones WHERE dropped_tenant_id = `+ph(1), target.String()).
				Scan(&name, &org, &createdByNull, &droppedBy, &droppedAt, &seq, &hash); err != nil {
				t.Fatalf("read the row before the delete: %v", err)
			}
			createdBy = createdByNull.String
			if name == "" || droppedBy != "operator:seed" || createdBy != "clew-service" || !seq.Valid || !hash.Valid {
				t.Fatalf("UNMEASURED: the seeded row is not the one the checks below describe: %q %q %q %v %v", name, droppedBy, createdBy, seq, hash)
			}

			// KNOWN POSITIVE: while the tombstone stands, the id cannot be provisioned.
			var conflict *auth.ProvisionConflictError
			_, err = ts.ProvisionTenant(ctx, auth.ProvisionRequest{TenantID: target, OrgName: orgName, Creator: "clew-service"})
			if !errors.As(err, &conflict) || conflict.Reason != auth.ConflictTenantIDDropped {
				t.Fatalf("provisioning a tombstoned id = %v; want tenant_id_dropped", err)
			}

			*f.noted = notedTarget{}
			w := doDrop(f.chainFixture, tombPath(target.String()), f.adminKey, target.String())
			if w.Code != http.StatusOK {
				t.Fatalf("delete on %s = %d %s", dialect, w.Code, w.Body.String())
			}
			var got tombstoneReply
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode %q: %v", w.Body.String(), err)
			}
			tb := got.Tombstone
			chain, _ := tb["audit_chain"].(map[string]any)
			orgGot, _ := uuid.Parse(tb["org_id"].(string))
			orgWant, _ := uuid.Parse(org)
			switch {
			case tb["tenant_id"] != target.String() || tb["name"] != name || orgGot != orgWant || tb["created_by"] != createdBy ||
				tb["dropped_by"] != droppedBy || tb["dropped_at"] != droppedAt.UTC().Format(tombstoneTimeFormat) ||
				chain["seq"] != float64(seq.Int64) || chain["hash"] != strings.TrimSpace(hash.String):
				t.Errorf("the reply is not the row that was deleted:\n reply %v\n row   name=%q org=%s created_by=%q dropped_by=%q dropped_at=%s seq=%d hash=%s",
					tb, name, org, createdBy, droppedBy, droppedAt.UTC().Format(tombstoneTimeFormat), seq.Int64, strings.TrimSpace(hash.String))
			}
			if f.noted.set {
				t.Errorf("the delete noted audit target %v", f.noted.tenant)
			}

			// The tombstone is gone, a bystander's is not (the negative control).
			if count(target) != 0 || count(bystander) != 1 {
				t.Errorf("tombstones: target=%d bystander=%d; want 0 and 1", count(target), count(bystander))
			}

			// A repeat is a 404 and changes nothing.
			if w := doDrop(f.chainFixture, tombPath(target.String()), f.adminKey, target.String()); w.Code != http.StatusNotFound {
				t.Errorf("a repeat = %d %s; want 404", w.Code, w.Body.String())
			}
			// An id that was never dropped is the same 404.
			if w := doDrop(f.chainFixture, tombPath(uuid.NewString()), f.adminKey, ""); w.Code != http.StatusBadRequest {
				t.Errorf("no X-Confirm = %d; want 400 before anything is read", w.Code)
			}

			// REUSE AFTER DELETION: the id can now be provisioned, and by a provision key's principal.
			res, err := ts.ProvisionTenant(ctx, auth.ProvisionRequest{TenantID: target, OrgName: orgName, Creator: "clew-service"})
			if err != nil || res.Tenant.ID != target {
				t.Fatalf("provisioning the id after the tombstone was deleted = %+v, %v; want it created", res, err)
			}
		})
	}
}
