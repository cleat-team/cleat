package notifications

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
)

// cleat#3541. PUT /webhooks/{id} ends its SET list with `updated_at = now()` and read "0 rows affected" as
// "no such webhook". PostgreSQL and SQL Server count the rows MATCHED, so that holds. MySQL counts rows
// CHANGED, and its now() has second precision: a second PUT within the same second of the row's last update
// that changes nothing else assigns the value the row already holds, affects 0 rows, and answered 404 for a
// webhook that exists. A client that PUTs the same desired state on every start (clew's ensureDispatchWebhook)
// got a 404 when it restarted within a second.
//
// WHAT HAS TO HOLD:
//
//	a PUT that changes nothing answers 200 and the webhook, however often and however fast
//	a PUT that rotates only the secret, twice in a second, answers 200 both times and the second secret is stored
//	a PUT for a webhook that does not exist, that another tenant owns, or that is deleted, still answers 404
//	(the last three are what the existence check on the zero path must not weaken)
//
// FALSIFIED, not assumed: with the existence check removed the first test reads 404 on the second PUT on
// MySQL, and the 404 cases below read 404 on every dialect either way (they hold the check from the other side).

func TestAPutThatChangesNothingIsNotA404(t *testing.T) {
	for _, be := range testutil.NewPluginTestBackends(t) {
		be := be
		t.Run(be.Name, func(t *testing.T) {
			defer be.Cleanup()
			defer removeLeakedWebhookSecrets(t, be)
			e := newNameEnv(t, be)
			tenant := uuid.MustParse(engine.DefaultTenantUUID)
			name := uniqueWebhookName("noop")
			defer e.clearPrefix(tenant, "noop-")()

			hook := e.mustCreate(tenant, &name)
			path := "/webhooks/" + hook.ID

			// The row is already enabled, so each of these changes nothing but updated_at, and the
			// four follow one another well inside a second.
			for i := 1; i <= 6; i++ {
				code, body := e.do(tenant, http.MethodPut, path, map[string]any{"enabled": true})
				if code != http.StatusOK {
					t.Fatalf("PUT #%d that changes nothing: status %d (%s), want 200", i, code, body)
				}
				if got := e.decode(code, body, http.StatusOK); got.ID != hook.ID || !got.Enabled {
					t.Fatalf("PUT #%d answered %+v, want the webhook %s, enabled", i, got, hook.ID)
				}
			}
		})
	}
}

func TestASecretRotationTwiceInASecondIsNotA404(t *testing.T) {
	for _, be := range testutil.NewPluginTestBackends(t) {
		be := be
		t.Run(be.Name, func(t *testing.T) {
			defer be.Cleanup()
			defer removeLeakedWebhookSecrets(t, be)
			e := newNameEnv(t, be)
			tenant := uuid.MustParse(engine.DefaultTenantUUID)
			name := uniqueWebhookName("noop")
			defer e.clearPrefix(tenant, "noop-")()

			hook := e.mustCreate(tenant, &name)
			path := "/webhooks/" + hook.ID
			id := uuid.MustParse(hook.ID)

			// A rotation carries no SET clause of its own, so it reaches the row only as updated_at = now().
			for _, secret := range []string{"rotated-secret-one", "rotated-secret-two"} {
				code, body := e.do(tenant, http.MethodPut, path, map[string]any{"secret": secret})
				if code != http.StatusOK {
					t.Fatalf("PUT rotating the secret to %q: status %d (%s), want 200", secret, code, body)
				}
				got, err := e.p.secrets.ForTenant(tenant.String()).Get(plugin.ForTenant(context.Background(), tenant), WebhookSecretName(id))
				if err != nil {
					t.Fatalf("reading the stored secret: %v", err)
				}
				if got != secret {
					t.Fatalf("after rotating to %q the stored secret is %q", secret, got)
				}
			}
		})
	}
}

// The existence check must keep refusing what the 0-row answer used to refuse correctly.
func TestAPutForAWebhookThatIsNotTheirsIsStillA404(t *testing.T) {
	for _, be := range testutil.NewPluginTestBackends(t) {
		be := be
		t.Run(be.Name, func(t *testing.T) {
			defer be.Cleanup()
			defer removeLeakedWebhookSecrets(t, be)
			e := newNameEnv(t, be)
			tenant := uuid.MustParse(engine.DefaultTenantUUID)
			name := uniqueWebhookName("noop")
			defer e.clearPrefix(tenant, "noop-")()

			mine := e.mustCreate(tenant, &name)

			cases := []struct {
				what, path string
				as         uuid.UUID
			}{
				{"an id that never existed", "/webhooks/" + uuid.NewString(), tenant},
			}
			// MySQL is single-tenant by constraint (engine.DefaultTenantUUID): a second tenant cannot be
			// built there, so the other-tenant case is the other two dialects'.
			if be.Dialect != testutil.DialectMySQL {
				other := uuid.New()
				defer e.seedTenants(other)()
				cases = append(cases, struct {
					what, path string
					as         uuid.UUID
				}{"another tenant's webhook", "/webhooks/" + mine.ID, other})
			}
			for _, c := range cases {
				for i := 0; i < 3; i++ { // repeated, so a same-second repeat is covered too
					code, body := e.do(c.as, http.MethodPut, c.path, map[string]any{"enabled": true})
					if code != http.StatusNotFound || !strings.Contains(string(body), "webhook not found") {
						t.Errorf("PUT for %s (#%d): status %d (%s), want 404 webhook not found", c.what, i+1, code, body)
					}
				}
			}

			// Deleted: a soft-deleted webhook is gone for PUT, however many times it is asked.
			if code, body := e.do(tenant, http.MethodDelete, "/webhooks/"+mine.ID, nil); code != http.StatusNoContent && code != http.StatusOK {
				t.Fatalf("DELETE: status %d (%s)", code, body)
			}
			for i := 0; i < 3; i++ {
				code, body := e.do(tenant, http.MethodPut, "/webhooks/"+mine.ID, map[string]any{"enabled": true})
				if code != http.StatusNotFound {
					t.Errorf("PUT for a deleted webhook (#%d): status %d (%s), want 404", i+1, code, body)
				}
			}
		})
	}
}
