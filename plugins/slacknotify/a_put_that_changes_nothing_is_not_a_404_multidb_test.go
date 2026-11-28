package slacknotify

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
)

// cleat#3550, the same defect as cleat#3541 in this plugin. PUT /slack/configs/{id} ends its SET list with
// `updated_at = now()` and read "0 rows affected" as "no such config". PostgreSQL and SQL Server count the
// rows MATCHED, so that holds there. MySQL counts rows CHANGED, and its now() has second precision: a
// second PUT within the same second of the row's last update that changes nothing else assigns the values the
// row already holds, affects 0 rows, and answered 404 for a config that exists.
//
// WHAT HAS TO HOLD:
//
//	a PUT that changes nothing answers 200 and the config, however often and however fast
//	a PUT that sets the same webhook_url, three times in a second, answers 200 each time
//	a PUT for a config that does not exist, that another tenant owns, or that is deleted, still answers 404
//	(the check on the zero path must not weaken what the 0-row answer used to refuse correctly)
//
// FALSIFIED, not assumed: with the existence check removed the first test reads 404 on the second PUT on
// MySQL, and the 404 cases read 404 on every dialect either way (they hold the check from the other side).

type putEnv struct {
	t       *testing.T
	be      testutil.PluginTestBackend
	p       *Plugin
	mux     *http.ServeMux
	dialect plugin.Dialect
}

func newPutEnv(t *testing.T, be testutil.PluginTestBackend) *putEnv {
	t.Helper()
	ctx := context.Background()
	dialect := plugin.Dialect(string(be.Dialect))
	testutil.SetupFullSchema(t, be.DB, be.Dialect)
	p := &Plugin{dialect: dialect, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := plugin.RunMigrations(ctx, be.DB, dialect, nil, []*plugin.LoadedPlugin{{Plugin: p, Healthy: true}}); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	p.db = &engine.SQLDBAdapter{DB: be.DB, Dialect: dialect}
	mux := http.NewServeMux()
	if err := p.RegisterRoutes(mux); err != nil {
		t.Fatalf("register routes: %v", err)
	}
	return &putEnv{t: t, be: be, p: p, mux: mux, dialect: dialect}
}

func (e *putEnv) do(tenant uuid.UUID, method, path string, body any) (int, []byte) {
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

// create makes a config as tenant and returns its id.
func (e *putEnv) create(tenant uuid.UUID) uuid.UUID {
	e.t.Helper()
	code, body := e.do(tenant, http.MethodPost, "/slack/configs", map[string]any{"name": "put-noop-" + uuid.NewString()[:8], "webhook_url": "https://hooks.slack.com/services/T000/B000/initial"})
	if code != http.StatusCreated {
		e.t.Fatalf("create: status %d (%s), want 201", code, body)
	}
	var out struct {
		ID uuid.UUID `json:"id"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.ID == uuid.Nil {
		e.t.Fatalf("create answered %s (%v)", body, err)
	}
	return out.ID
}

// cleanup: slacknotify keeps nothing in the secret store, and be.Cleanup clears its tables.
func (e *putEnv) cleanup() {}

func TestAPutThatChangesNothingIsNotA404(t *testing.T) {
	for _, be := range testutil.NewPluginTestBackends(t) {
		be := be
		t.Run(be.Name, func(t *testing.T) {
			defer be.Cleanup()
			e := newPutEnv(t, be)
			defer e.cleanup()
			tenant := uuid.MustParse(engine.DefaultTenantUUID)
			id := e.create(tenant)

			// The config is already enabled, so each of these changes nothing but updated_at, and they
			// follow one another well inside a second.
			for i := 1; i <= 6; i++ {
				code, body := e.do(tenant, http.MethodPut, "/slack/configs/"+id.String(), map[string]any{"enabled": true})
				if code != http.StatusOK {
					t.Fatalf("PUT #%d that changes nothing: status %d (%s), want 200", i, code, body)
				}
				var got struct {
					ID      uuid.UUID `json:"id"`
					Enabled bool      `json:"enabled"`
				}
				if err := json.Unmarshal(body, &got); err != nil || got.ID != id || !got.Enabled {
					t.Fatalf("PUT #%d answered %s, want the config %s, enabled", i, body, id)
				}
			}
		})
	}
}

// Slack keeps its webhook URL in the config row (it has no secret store entry), so "the same URL twice in a
// second" is its rotation case: nothing else in the SET list changes the row.
func TestTheSameWebhookURLTwiceInASecondIsNotA404(t *testing.T) {
	for _, be := range testutil.NewPluginTestBackends(t) {
		be := be
		t.Run(be.Name, func(t *testing.T) {
			defer be.Cleanup()
			e := newPutEnv(t, be)
			defer e.cleanup()
			tenant := uuid.MustParse(engine.DefaultTenantUUID)
			id := e.create(tenant)
			for i := 1; i <= 3; i++ {
				code, body := e.do(tenant, http.MethodPut, "/slack/configs/"+id.String(), map[string]any{"webhook_url": "https://hooks.slack.com/services/T000/B000/same"})
				if code != http.StatusOK {
					t.Fatalf("PUT #%d with the same webhook_url: status %d (%s), want 200", i, code, body)
				}
			}
		})
	}
}

// The existence check must keep refusing what the 0-row answer used to refuse correctly.
func TestAPutForAConfigThatIsNotTheirsIsStillA404(t *testing.T) {
	for _, be := range testutil.NewPluginTestBackends(t) {
		be := be
		t.Run(be.Name, func(t *testing.T) {
			defer be.Cleanup()
			e := newPutEnv(t, be)
			defer e.cleanup()
			tenant := uuid.MustParse(engine.DefaultTenantUUID)
			mine := e.create(tenant)

			cases := []struct {
				what, path string
				as         uuid.UUID
			}{
				{"an id that never existed", "/slack/configs/" + uuid.NewString(), tenant},
			}
			// MySQL is single-tenant by constraint (engine.DefaultTenantUUID): a second tenant cannot be
			// built there, so the other-tenant case is the other two dialects'. A PUT as a tenant that has
			// no row of that id needs no admin.tenants row of its own: nothing is created.
			if be.Dialect != testutil.DialectMySQL {
				cases = append(cases, struct {
					what, path string
					as         uuid.UUID
				}{"another tenant's config", "/slack/configs/" + mine.String(), uuid.New()})
			}
			for _, c := range cases {
				for i := 0; i < 3; i++ { // repeated, so a same-second repeat is covered too
					code, body := e.do(c.as, http.MethodPut, c.path, map[string]any{"enabled": true})
					if code != http.StatusNotFound {
						t.Errorf("PUT for %s (#%d): status %d (%s), want 404", c.what, i+1, code, body)
					}
				}
			}

			if code, body := e.do(tenant, http.MethodDelete, "/slack/configs/"+mine.String(), nil); code != http.StatusNoContent && code != http.StatusOK {
				t.Fatalf("DELETE: status %d (%s)", code, body)
			}
			for i := 0; i < 3; i++ {
				code, body := e.do(tenant, http.MethodPut, "/slack/configs/"+mine.String(), map[string]any{"enabled": true})
				if code != http.StatusNotFound {
					t.Errorf("PUT for a deleted config (#%d): status %d (%s), want 404", i+1, code, body)
				}
			}
		})
	}
}
