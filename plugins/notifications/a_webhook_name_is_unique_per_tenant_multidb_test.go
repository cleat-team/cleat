package notifications

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
	"github.com/cleat-team/cleat/plugins/plugintest"
)

// cleat#3538. A webhook had no identity a caller could choose, so an app that needed to find its own row
// again had to match on `url`, which changes, or abuse `events` (clew#317). `name` is that identity:
// optional, caller-set, unique per tenant among rows that are not deleted.
//
// WHAT HAS TO HOLD, and what each test below covers:
//
//	a duplicate name in one tenant is refused, with a 409 and nothing written
//	the same name in two tenants is allowed
//	a webhook with no name is not named NULL: any number of them coexist
//	a deleted webhook releases its name
//	the name can be looked up, set and changed, not cleared, and only a canonical one is accepted
//
// THE UNIQUENESS IS FALSIFIED, not assumed: with the unique index removed from migration v8 the duplicate
// test reads 201 for the second create, on every dialect.

// nameEnv is the plugin over a migrated schema, served through a real mux so a path parameter and a query
// string reach the handler the way they do in the worker.
type nameEnv struct {
	t       *testing.T
	be      testutil.PluginTestBackend
	p       *Plugin
	mux     *http.ServeMux
	dialect plugin.Dialect
}

func newNameEnv(t *testing.T, be testutil.PluginTestBackend) *nameEnv {
	t.Helper()
	ctx := context.Background()
	dialect := plugin.Dialect(string(be.Dialect))
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	testutil.SetupFullSchema(t, be.DB, be.Dialect)

	p := &Plugin{dialect: dialect, logger: quiet}
	if err := plugin.RunMigrations(ctx, be.DB, dialect, nil,
		[]*plugin.LoadedPlugin{{Plugin: p, Healthy: true}}); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	key := make([]byte, 32)
	for i := range key {
		key[i] = 0x5a
	}
	ring, err := engine.NewKeyRing(engine.VersionedKey{Version: 1, Key: key})
	if err != nil {
		t.Fatalf("build key ring: %v", err)
	}
	p.db = &engine.SQLDBAdapter{DB: be.DB, Dialect: dialect}
	p.secrets = engine.NewPluginSecrets(engine.NewSecretStoreWithRing(be.DB, string(dialect), ring))

	mux := http.NewServeMux()
	if err := p.RegisterRoutes(mux); err != nil {
		t.Fatalf("register routes: %v", err)
	}
	return &nameEnv{t: t, be: be, p: p, mux: mux, dialect: dialect}
}

type namedWebhook struct {
	ID      string  `json:"id"`
	URL     string  `json:"url"`
	Name    *string `json:"name"`
	Enabled bool    `json:"enabled"`
	Error   string  `json:"error"`
}

// do sends a request as tenant and returns the status and body.
func (e *nameEnv) do(tenant uuid.UUID, method, path string, body any) (int, []byte) {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			e.t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, rd).WithContext(auth.WithTenantID(context.Background(), tenant))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

// decode reads a webhook from a response that must have the given status.
func (e *nameEnv) decode(code int, body []byte, want int) namedWebhook {
	e.t.Helper()
	if code != want {
		e.t.Fatalf("status %d, want %d: %s", code, want, body)
	}
	var w namedWebhook
	if err := json.Unmarshal(body, &w); err != nil {
		e.t.Fatalf("decode %s: %v", body, err)
	}
	return w
}

// mustCreate creates a webhook that must succeed.
func (e *nameEnv) mustCreate(tenant uuid.UUID, name *string) namedWebhook {
	e.t.Helper()
	code, body := e.create(tenant, name)
	return e.decode(code, body, http.StatusCreated)
}

func (e *nameEnv) get(tenant uuid.UUID, id string) namedWebhook {
	e.t.Helper()
	code, body := e.do(tenant, http.MethodGet, "/webhooks/"+id, nil)
	return e.decode(code, body, http.StatusOK)
}

func (e *nameEnv) create(tenant uuid.UUID, name *string) (int, []byte) {
	b := map[string]any{"url": nameTestURL, "secret": "name-test-secret", "events": []string{"dispatch"}}
	if name != nil {
		b["name"] = *name
	}
	return e.do(tenant, http.MethodPost, "/webhooks", b)
}

func (e *nameEnv) byName(tenant uuid.UUID, name string) []namedWebhook {
	e.t.Helper()
	code, body := e.do(tenant, http.MethodGet, "/webhooks?name="+url.QueryEscape(name), nil)
	if code != http.StatusOK {
		e.t.Fatalf("GET ?name=%s: %d %s", name, code, body)
	}
	var out []namedWebhook
	if err := json.Unmarshal(body, &out); err != nil {
		e.t.Fatalf("decode %s: %v", body, err)
	}
	return out
}

// unique returns a name no other run of this test, or other test, has used: the database is shared.
func uniqueWebhookName(label string) string { return label + "-" + uuid.NewString()[:8] }

func ptr(s string) *string { return &s }

// seedTenants registers tenants the test made up (a tenant's secrets need its admin.tenants row) and returns
// the function that removes their webhooks, secrets and rows. Defer it AFTER be.Cleanup so it runs first:
// a t.Cleanup callback runs after the test's deferred calls, when the connection is already closed.
func (e *nameEnv) seedTenants(tenants ...uuid.UUID) func() {
	ctx := context.Background()
	for _, tn := range tenants {
		if _, err := plugintest.ExecRebound(e.t, ctx, e.be.DB, e.dialect,
			`INSERT INTO admin.tenants (tenant_id, name) VALUES ($1, $2)`, tn, "webhook-name-test-"+tn.String()[:8]); err != nil {
			e.t.Fatalf("seed admin.tenants for %s: %v", tn, err)
		}
	}
	return func() {
		for _, tn := range tenants {
			if _, err := e.p.db.Exec(plugin.ForTenant(ctx, tn), `DELETE FROM webhook_config WHERE tenant_id = $1`, tn); err != nil {
				e.t.Errorf("could not remove tenant %s's webhooks: %v", tn, err)
			}
			plugintest.RemoveTenantSecretsWithPrefix(e.t, e.be.DB, e.be.Dialect, tn.String(), secretPrefix())
			if _, err := plugintest.ExecRebound(e.t, ctx, e.be.DB, e.dialect,
				`DELETE FROM admin.tenants WHERE tenant_id = $1`, tn); err != nil {
				e.t.Errorf("could not remove admin.tenants row for %s: %v", tn, err)
			}
		}
	}
}

// clearPrefix removes the default tenant's webhooks whose name starts with prefix, and the unnamed ones this
// test made (recognised by their url), so a run leaves nothing for the next one to collide with.
//
// It returns the function to defer, and it must be deferred AFTER be.Cleanup so that it runs first: a
// t.Cleanup callback runs after the test's deferred calls, by which time the connection is closed.
func (e *nameEnv) clearPrefix(tenant uuid.UUID, prefix string) func() {
	return func() {
		ctx := plugin.ForTenant(context.Background(), tenant)
		if _, err := e.p.db.Exec(ctx, `DELETE FROM webhook_config WHERE tenant_id = $1 AND (name LIKE $2 OR url = $3)`,
			tenant, prefix+"%", nameTestURL); err != nil {
			e.t.Errorf("could not remove %s*: %v", prefix, err)
		}
	}
}

const nameTestURL = "https://example.com/name-test-hook"

// A duplicate name in one tenant is refused with a 409, nothing is written, and the first webhook is
// untouched. Any number of webhooks with no name coexist: NULL is not a name.
func TestADuplicateWebhookNameInOneTenantIsRefused(t *testing.T) {
	for _, be := range testutil.NewPluginTestBackends(t) {
		be := be
		t.Run(be.Name, func(t *testing.T) {
			defer be.Cleanup()
			defer removeLeakedWebhookSecrets(t, be)
			e := newNameEnv(t, be)
			tenant := uuid.MustParse(engine.DefaultTenantUUID)
			name := uniqueWebhookName("dup")
			defer e.clearPrefix(tenant, "dup-")()

			first := e.mustCreate(tenant, &name)
			if first.Name == nil || *first.Name != name {
				t.Fatalf("created webhook %+v, want name %q echoed", first, name)
			}

			code, body := e.create(tenant, &name)
			if code != http.StatusConflict {
				t.Fatalf("second create of %q: status %d (%s), want 409", name, code, body)
			}
			if !strings.Contains(string(body), name) {
				t.Errorf("the 409 does not name the taken name: %s", body)
			}
			if got := e.byName(tenant, name); len(got) != 1 || got[0].ID != first.ID {
				t.Fatalf("after the refused create, %q finds %+v, want only %s", name, got, first.ID)
			}

			// A different name is fine, and so is no name at all, any number of times.
			other := uniqueWebhookName("dup")
			e.mustCreate(tenant, &other)
			a, b := e.mustCreate(tenant, nil), e.mustCreate(tenant, nil)
			if a.Name != nil || b.Name != nil || a.ID == b.ID {
				t.Fatalf("two unnamed webhooks came back as %+v and %+v", a, b)
			}
		})
	}
}

// The name can be looked up: the answer is a list of one webhook or none, and a name that could never exist
// is a 400 rather than an empty list.
func TestAWebhookIsFoundByItsName(t *testing.T) {
	for _, be := range testutil.NewPluginTestBackends(t) {
		be := be
		t.Run(be.Name, func(t *testing.T) {
			defer be.Cleanup()
			defer removeLeakedWebhookSecrets(t, be)
			e := newNameEnv(t, be)
			tenant := uuid.MustParse(engine.DefaultTenantUUID)
			name := uniqueWebhookName("find")
			defer e.clearPrefix(tenant, "find-")()

			w := e.mustCreate(tenant, &name)
			e.mustCreate(tenant, nil) // another webhook, with no name, that a lookup must not return

			got := e.byName(tenant, name)
			if len(got) != 1 || got[0].ID != w.ID || got[0].Name == nil || *got[0].Name != name {
				t.Fatalf("?name=%s found %+v, want %s", name, got, w.ID)
			}
			if g := e.get(tenant, w.ID); g.Name == nil || *g.Name != name {
				t.Fatalf("GET by id lost the name: %+v", g)
			}

			code, body := e.do(tenant, http.MethodGet, "/webhooks?name="+url.QueryEscape(uniqueWebhookName("absent")), nil)
			if code != http.StatusOK || strings.TrimSpace(string(body)) != "[]" {
				t.Fatalf("a name nothing has: %d %s, want 200 []", code, body)
			}
			for _, bad := range []string{"", "UPPER", "has space", "-lead"} {
				if code, body := e.do(tenant, http.MethodGet, "/webhooks?name="+url.QueryEscape(bad), nil); code != http.StatusBadRequest {
					t.Errorf("?name=%q: %d %s, want 400", bad, code, body)
				}
			}
		})
	}
}

// A name can be set on an unnamed webhook and changed, but not cleared and not given to a second webhook;
// a refused rename changes nothing, and an update that leaves the name out leaves it alone.
func TestAWebhookCanBeNamedAndRenamedButNotClearedOrDuplicated(t *testing.T) {
	for _, be := range testutil.NewPluginTestBackends(t) {
		be := be
		t.Run(be.Name, func(t *testing.T) {
			defer be.Cleanup()
			defer removeLeakedWebhookSecrets(t, be)
			e := newNameEnv(t, be)
			tenant := uuid.MustParse(engine.DefaultTenantUUID)
			defer e.clearPrefix(tenant, "ren-")()
			n1, n2, n3 := uniqueWebhookName("ren"), uniqueWebhookName("ren"), uniqueWebhookName("ren")

			w := e.mustCreate(tenant, nil)
			put := func(id string, body map[string]any) (int, []byte) {
				return e.do(tenant, http.MethodPut, "/webhooks/"+id, body)
			}
			mustPut := func(id string, body map[string]any) namedWebhook {
				t.Helper()
				code, b := put(id, body)
				return e.decode(code, b, http.StatusOK)
			}

			if g := mustPut(w.ID, map[string]any{"name": n1}); g.Name == nil || *g.Name != n1 {
				t.Fatalf("naming an unnamed webhook: %+v", g)
			}
			if got := e.byName(tenant, n1); len(got) != 1 || got[0].ID != w.ID {
				t.Fatalf("?name=%s finds %+v", n1, got)
			}

			taken := e.mustCreate(tenant, &n2)
			code, body := put(w.ID, map[string]any{"name": n2})
			if code != http.StatusConflict {
				t.Fatalf("renaming onto a taken name: %d %s, want 409", code, body)
			}
			if g := e.get(tenant, w.ID); g.Name == nil || *g.Name != n1 {
				t.Fatalf("a refused rename changed the webhook: %+v", g)
			}
			if g := e.get(tenant, taken.ID); g.Name == nil || *g.Name != n2 {
				t.Fatalf("a refused rename changed the other webhook: %+v", g)
			}

			if g := mustPut(w.ID, map[string]any{"name": n3}); g.Name == nil || *g.Name != n3 {
				t.Fatalf("renaming to a free name: %+v", g)
			}
			if old := e.byName(tenant, n1); len(old) != 0 {
				t.Fatalf("the old name still finds %+v", old)
			}
			// Naming it what it already is must not collide with itself. The url changes in the same request
			// on purpose: an update that changes nothing, twice in a second, is answered 404 on MySQL
			// (cleat#3541), which has nothing to do with names and would hide what this asserts.
			if g := mustPut(w.ID, map[string]any{"name": n3, "url": nameTestURL + "-2"}); g.Name == nil || *g.Name != n3 {
				t.Fatalf("setting the name it already has: %+v", g)
			}

			// Leaving the name out is not clearing it.
			if g := mustPut(w.ID, map[string]any{"enabled": false}); g.Name == nil || *g.Name != n3 || g.Enabled {
				t.Fatalf("an update without a name: %+v, want the name kept and the webhook disabled", g)
			}
			// And clearing it is refused, as clearing a secret is.
			if code, body := put(w.ID, map[string]any{"name": ""}); code != http.StatusBadRequest {
				t.Fatalf(`"name": "" gave %d %s, want 400`, code, body)
			}
			if g := e.get(tenant, w.ID); g.Name == nil || *g.Name != n3 {
				t.Fatalf("a refused clear changed the name: %+v", g)
			}
		})
	}
}

// Only a canonical name is accepted, on create and on update. The rule is narrow so that no two names can
// be equal on one dialect and different on another: MySQL and SQL Server compare case-insensitively.
func TestOnlyACanonicalWebhookNameIsAccepted(t *testing.T) {
	for _, be := range testutil.NewPluginTestBackends(t) {
		be := be
		t.Run(be.Name, func(t *testing.T) {
			defer be.Cleanup()
			defer removeLeakedWebhookSecrets(t, be)
			e := newNameEnv(t, be)
			tenant := uuid.MustParse(engine.DefaultTenantUUID)
			defer e.clearPrefix(tenant, "canon-")()
			defer e.clearPrefix(tenant, "a")()
			defer e.clearPrefix(tenant, "9")()
			base := e.mustCreate(tenant, nil)

			for _, bad := range []string{
				"Upper", "UPPER-case", "", " lead", "trail ", "-lead", ".lead", "has space", "slash/x", "é", "tab\t",
				strings.Repeat("a", 129),
			} {
				bad := bad
				if code, body := e.create(tenant, &bad); code != http.StatusBadRequest {
					t.Errorf("create with name %q: %d %s, want 400", bad, code, body)
				}
				if code, body := e.do(tenant, http.MethodPut, "/webhooks/"+base.ID, map[string]any{"name": bad}); code != http.StatusBadRequest {
					t.Errorf("update to name %q: %d %s, want 400", bad, code, body)
				}
			}
			if g := e.get(tenant, base.ID); g.Name != nil {
				t.Fatalf("a refused update named the webhook %q", *g.Name)
			}

			for _, good := range []string{
				"canon-" + uuid.NewString()[:8], "a.b_c-d" + uuid.NewString()[:4], "9lives" + uuid.NewString()[:4],
				strings.Repeat("a", 120) + uuid.NewString()[:8], // 128 characters, the longest accepted
			} {
				good := good
				if len(good) > 128 {
					t.Fatalf("test bug: %q is %d characters", good, len(good))
				}
				w := e.mustCreate(tenant, &good)
				if w.Name == nil || *w.Name != good {
					t.Errorf("create with name %q came back %+v", good, w)
				}
			}
		})
	}
}

// A deleted webhook releases its name: the row survives as a soft delete (v6), and a unique index that
// counted it would hold the name for ever behind a webhook nobody can list.
func TestADeletedWebhooksNameCanBeUsedAgain(t *testing.T) {
	for _, be := range testutil.NewPluginTestBackends(t) {
		be := be
		t.Run(be.Name, func(t *testing.T) {
			defer be.Cleanup()
			defer removeLeakedWebhookSecrets(t, be)
			e := newNameEnv(t, be)
			tenant := uuid.MustParse(engine.DefaultTenantUUID)
			defer e.clearPrefix(tenant, "del-")()
			name := uniqueWebhookName("del")

			old := e.mustCreate(tenant, &name)
			if code, body := e.create(tenant, &name); code != http.StatusConflict {
				t.Fatalf("setup: a second create while it is live gave %d %s, want 409", code, body)
			}
			if code, body := e.do(tenant, http.MethodDelete, "/webhooks/"+old.ID, nil); code != http.StatusNoContent {
				t.Fatalf("delete: %d %s", code, body)
			}
			if got := e.byName(tenant, name); len(got) != 0 {
				t.Fatalf("a deleted webhook is still found by name: %+v", got)
			}

			again := e.mustCreate(tenant, &name)
			if again.ID == old.ID {
				t.Fatal("the new webhook has the deleted one's id")
			}
			if got := e.byName(tenant, name); len(got) != 1 || got[0].ID != again.ID {
				t.Fatalf("after reusing the name %q finds %+v, want only %s", name, got, again.ID)
			}
			// And the name is taken again, now by the new one.
			if code, body := e.create(tenant, &name); code != http.StatusConflict {
				t.Fatalf("the reused name was not protected: %d %s, want 409", code, body)
			}
		})
	}
}

// The same name in two tenants is allowed, and each tenant finds its own. MySQL is single-tenant by
// constraint here (engine.DefaultTenantUUID is its only tenant), so the case cannot be built there.
func TestTheSameWebhookNameInTwoTenantsIsAllowed(t *testing.T) {
	for _, be := range testutil.NewPluginTestBackends(t) {
		be := be
		t.Run(be.Name, func(t *testing.T) {
			defer be.Cleanup()
			if be.Dialect == testutil.DialectMySQL {
				t.Skip("MySQL is single-tenant by constraint (engine.DefaultTenantUUID, all zeros): a second tenant cannot be constructed")
			}
			defer removeLeakedWebhookSecrets(t, be)
			e := newNameEnv(t, be)
			a, b := uuid.New(), uuid.New()
			defer e.seedTenants(a, b)()
			name := uniqueWebhookName("shared")

			wa := e.mustCreate(a, &name)
			wb := e.mustCreate(b, &name)
			if wa.ID == wb.ID {
				t.Fatal("two tenants got one webhook")
			}
			if got := e.byName(a, name); len(got) != 1 || got[0].ID != wa.ID {
				t.Fatalf("tenant a finds %+v, want only its own %s", got, wa.ID)
			}
			if got := e.byName(b, name); len(got) != 1 || got[0].ID != wb.ID {
				t.Fatalf("tenant b finds %+v, want only its own %s", got, wb.ID)
			}
			// Within each tenant it is still unique.
			for _, tn := range []uuid.UUID{a, b} {
				if code, body := e.create(tn, &name); code != http.StatusConflict {
					t.Errorf("tenant %s: a duplicate gave %d %s, want 409", tn, code, body)
				}
			}
		})
	}
}
