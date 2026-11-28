package notifications

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
)

// cleat#3538, migration v8. Three properties of the migration itself, each on every dialect, using the
// migration's own SQL read from p.Migrations() rather than a copy:
//
//	rows that exist before it get a NULL name, and are neither lost nor named
//	running it again changes nothing and fails nothing (MySQL's DDL is not transactional, so a crash
//	between two of its steps is re-run on the next boot)
//	the uniqueness it creates is enforced on the rows that were already there
//
// plugin.RunMigrations skips a recorded version, so calling it twice cannot exercise the second property;
// this runs Down and then Up directly.

// migrationSQL returns the up and down text for version v on the backend's dialect.
func migrationSQL(t *testing.T, p *Plugin, d plugin.Dialect, v int) (up, down string) {
	t.Helper()
	for _, m := range p.Migrations() {
		if m.Version != v {
			continue
		}
		up, down = m.Up, m.Down
		switch d {
		case plugin.DialectMySQL:
			up, down = m.UpMySQL, m.DownMySQL
		case plugin.DialectMSSQL:
			up, down = m.UpMSSQL, m.DownMSSQL
		}
		if up == "" || down == "" {
			t.Fatalf("migration v%d has no up/down text for %s", v, d)
		}
		return up, down
	}
	t.Fatalf("no migration v%d", v)
	return "", ""
}

func TestWebhookNameMigrationKeepsExistingRowsUnnamedAndIsIdempotent(t *testing.T) {
	for _, be := range testutil.NewPluginTestBackends(t) {
		be := be
		t.Run(be.Name, func(t *testing.T) {
			defer be.Cleanup()
			defer removeLeakedWebhookSecrets(t, be)
			e := newNameEnv(t, be)
			ctx := context.Background()
			tenant := uuid.MustParse(engine.DefaultTenantUUID)
			tctx := plugin.ForTenant(ctx, tenant)
			up, down := migrationSQL(t, e.p, e.dialect, 8)
			defer e.clearPrefix(tenant, "mig-")()
			// Whatever happens below, leave the schema as the other tests expect it: v8 applied. Deferred after
			// be.Cleanup, so it runs while the connection is open.
			defer runStatements(t, ctx, be, up)

			// Take v8 off, then put a webhook where one would have been before it existed.
			runStatements(t, ctx, be, down)
			before := uuid.New()
			now := time.Now()
			if _, err := e.p.db.Exec(tctx, `
				INSERT INTO webhook_config (tenant_id, id, url, secret_configured, events, enabled, created_at, updated_at)
				VALUES ($1, $2, $3, $4, $5, true, $6, $7)
			`, tenant, before, nameTestURL, true, `["dispatch"]`, now, now); err != nil {
				t.Fatalf("seed a pre-v8 webhook: %v", err)
			}

			// Apply v8 twice. The second is the crash-and-reboot case.
			runStatements(t, ctx, be, up)
			runStatements(t, ctx, be, up)

			var name *string
			var count int
			if err := e.p.db.QueryRow(tctx, `SELECT name FROM webhook_config WHERE id = $1`, before).Scan(&name); err != nil {
				t.Fatalf("read the pre-v8 webhook after v8: %v", err)
			}
			if name != nil {
				t.Fatalf("a webhook that existed before v8 has name %q, want NULL", *name)
			}
			if err := e.p.db.QueryRow(tctx, `SELECT COUNT(*) FROM webhook_config WHERE id = $1`, before).Scan(&count); err != nil || count != 1 {
				t.Fatalf("the pre-v8 webhook is gone after v8: count %d, %v", count, err)
			}

			// The index exists and bites, on a table that already had a row in it.
			n := uniqueWebhookName("mig")
			e.mustCreate(tenant, &n)
			if code, body := e.create(tenant, &n); code != http.StatusConflict {
				t.Fatalf("after v8 was applied over an existing row, a duplicate name gave %d %s, want 409", code, body)
			}
			// A second unnamed webhook next to the pre-v8 one: NULL is not a name, on every dialect.
			if w := e.mustCreate(tenant, nil); w.Name != nil {
				t.Fatalf("an unnamed webhook came back named %q", *w.Name)
			}
		})
	}
}

// The classifier asks for the unique index by name, so it must recognise what each real driver says when that
// index refuses a write, and must not call any other duplicate a taken name. The messages are not written
// here: they are produced by the dialects and handed to the function.
func TestTheDuplicateNameClassifierRecognisesEachDialectsOwnMessage(t *testing.T) {
	for _, be := range testutil.NewPluginTestBackends(t) {
		be := be
		t.Run(be.Name, func(t *testing.T) {
			defer be.Cleanup()
			defer removeLeakedWebhookSecrets(t, be)
			e := newNameEnv(t, be)
			ctx := context.Background()
			tenant := uuid.MustParse(engine.DefaultTenantUUID)
			tctx := plugin.ForTenant(ctx, tenant)
			defer e.clearPrefix(tenant, "cls-")()

			name := uniqueWebhookName("cls")
			now := time.Now()
			insert := func(id uuid.UUID, name *string) error {
				_, err := e.p.db.Exec(tctx, `
					INSERT INTO webhook_config (tenant_id, id, url, secret_configured, events, enabled, created_at, updated_at, name)
					VALUES ($1, $2, $3, $4, $5, true, $6, $7, $8)
				`, tenant, id, nameTestURL, true, `[]`, now, now, name)
				return err
			}
			first := uuid.New()
			if err := insert(first, &name); err != nil {
				t.Fatalf("seed: %v", err)
			}

			// The name index refusing a second row with the same name.
			err := insert(uuid.New(), &name)
			if err == nil {
				t.Fatal("a second row with the same name was accepted")
			}
			t.Logf("%s says: %v", be.Name, err)
			if !isDuplicateWebhookName(err) {
				t.Errorf("the classifier does not recognise %s's own message for the name index: %v", be.Name, err)
			}

			// A different duplicate, the primary key, is a duplicate but not a taken name.
			other := uniqueWebhookName("cls")
			pkErr := insert(first, &other)
			if pkErr == nil {
				t.Fatal("a second row with the same id was accepted")
			}
			t.Logf("%s says: %v", be.Name, pkErr)
			if isDuplicateWebhookName(pkErr) {
				t.Errorf("the classifier calls a primary-key collision a taken name: %v", pkErr)
			}
			if isDuplicateWebhookName(nil) {
				t.Error("the classifier reports a nil error as a taken name")
			}
		})
	}
}
