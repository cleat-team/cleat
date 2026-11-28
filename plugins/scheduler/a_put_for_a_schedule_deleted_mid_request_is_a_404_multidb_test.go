package scheduler

import (
	"bytes"
	"context"
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
	"github.com/cleat-team/cleat/plugins/plugintest"
)

// cleat#3551. PUT /schedules/{id} reads the schedule (`SELECT cron, enabled ...`, 404 on no row), computes
// next_run_at from what it read, and then runs the UPDATE as `_, err = p.db.Exec(...)`: the row count was
// thrown away. A schedule deleted between the two statements made the UPDATE match nothing, return no error,
// and the handler answered 200 {"status":"updated"} for a schedule that no longer exists.
//
// This is a check-then-act race and is the same on every dialect. It is NOT cleat#3550 / cleat#3541, which is
// MySQL counting rows CHANGED where a handler read "0" as "not found". The two meet in the fix: a handler that
// starts reading the count must not turn a PUT that changes nothing into a 404 on MySQL, so on 0 rows it asks
// whether the schedule exists. Case 3 below is the guard on that side.
//
// THE HARD PART, which the issue names: a delete between two statements of one request. No sleep and no second
// goroutine: interceptDB wraps the plugin's real database handle and, on the first UPDATE of schedules, runs the
// DELETE through the same handle first, so the row is gone at exactly that point on every run.
//
// WHAT HAS TO HOLD:
//
//	1. a schedule deleted between the read and the write answers 404, not 200
//	2. the same request with nothing deleted answers 200 and the hook fired zero times (the 404 in 1 is the
//	   delete's doing, not the harness's)
//	3. a PUT that changes nothing answers 200, however often and however fast (MySQL's rows-changed count
//	   is 0 for it)
//	4. a schedule that never existed, or that another tenant owns, is still a 404
//
// FALSIFIED, not assumed: see the commit message for the mutations.

// interceptDB runs onUpdate once, before the first UPDATE of schedules, through the real database handle.
type interceptDB struct {
	plugin.PluginDB
	onUpdate func(ctx context.Context)
	fired    int
}

func (d *interceptDB) Exec(ctx context.Context, query string, args ...any) (int64, error) {
	if d.onUpdate != nil && d.fired == 0 && strings.HasPrefix(strings.TrimSpace(query), "UPDATE schedules") {
		d.fired++
		d.onUpdate(ctx)
	}
	return d.PluginDB.Exec(ctx, query, args...)
}

type putEnv struct {
	t       *testing.T
	be      testutil.PluginTestBackend
	dialect plugin.Dialect
	db      *interceptDB
	mux     *http.ServeMux
}

func newPutEnv(t *testing.T, be testutil.PluginTestBackend) *putEnv {
	t.Helper()
	ctx := context.Background()
	dialect := plugin.Dialect(string(be.Dialect))
	testutil.SetupFullSchema(t, be.DB, be.Dialect)
	p := &Plugin{dialect: dialect, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := plugin.RunMigrations(ctx, be.DB, dialect, nil, []*plugin.LoadedPlugin{{Plugin: p, Healthy: true}}); err != nil {
		t.Fatalf("scheduler migrations on %s: %v", be.Name, err)
	}
	real := &engine.SQLDBAdapter{DB: be.DB, Dialect: dialect}
	db := &interceptDB{PluginDB: real}
	p.db = db
	mux := http.NewServeMux()
	if err := p.RegisterRoutes(mux); err != nil {
		t.Fatalf("register routes: %v", err)
	}
	return &putEnv{t: t, be: be, dialect: dialect, db: db, mux: mux}
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

// create makes a schedule as tenant and returns its id. The cron fires once a year, so a PUT that re-sends
// enabled recomputes next_run_at to the SAME value: it changes nothing but updated_at.
func (e *putEnv) create(tenant uuid.UUID) uuid.UUID {
	e.t.Helper()
	code, body := e.do(tenant, http.MethodPost, "/schedules", map[string]any{
		"name": "put-race-" + uuid.NewString()[:8], "cron": "0 0 1 1 *", "workflow_name": "some-workflow",
	})
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

func (e *putEnv) exists(tenant, id uuid.UUID) bool {
	e.t.Helper()
	code, _ := e.do(tenant, http.MethodGet, "/schedules/"+id.String(), nil)
	return code == http.StatusOK
}

// withFixtureCleanup deletes this run's tenants' rows from outside the plugin, on a handle that can reach them.
func withFixtureCleanup(t *testing.T, be testutil.PluginTestBackend, dialect plugin.Dialect, tenants ...uuid.UUID) func() {
	fixtureDB := be.CrossTenantConn(t, context.Background(), "scheduler fixture: removes the rows of tenants it invented")
	return func() {
		for _, tn := range tenants {
			if _, err := plugintest.ExecRebound(t, context.Background(), fixtureDB, dialect,
				`DELETE FROM schedules WHERE tenant_id = $1`, tn); err != nil {
				t.Errorf("cleanup schedules on %s: %v", be.Name, err)
			}
		}
	}
}

func TestAScheduleDeletedBetweenThePutsReadAndWriteIsA404(t *testing.T) {
	for _, be := range testutil.NewPluginTestBackends(t) {
		be := be
		t.Run(be.Name, func(t *testing.T) {
			defer be.Cleanup()
			e := newPutEnv(t, be)
			tenant := uuid.New()
			defer withFixtureCleanup(t, be, e.dialect, tenant)()

			// 1. THE RACE. The hook deletes the schedule through the plugin's own handle, between the
			// handler's existence read (which found it) and its UPDATE (which will now match nothing).
			id := e.create(tenant)
			e.db.fired = 0
			e.db.onUpdate = func(ctx context.Context) {
				if n, err := e.db.PluginDB.Exec(ctx, `DELETE FROM schedules WHERE id = $1 AND tenant_id = $2`, id, tenant); err != nil || n != 1 {
					t.Errorf("the hook's delete removed %d rows (%v), want exactly 1", n, err)
				}
			}
			code, body := e.do(tenant, http.MethodPut, "/schedules/"+id.String(), map[string]any{"enabled": true})
			if e.db.fired != 1 {
				t.Fatalf("the hook fired %d times, want 1: the UPDATE was never reached, so this proves nothing", e.db.fired)
			}
			if e.exists(tenant, id) {
				t.Fatalf("the schedule is still there after the hook deleted it; the race was not created")
			}
			if code != http.StatusNotFound {
				t.Errorf("PUT for a schedule deleted between its read and its write: status %d (%s), want 404. "+
					"The UPDATE matched nothing and the handler said it updated", code, body)
			}

			// 2. THE CONTROL: the same request with nothing deleted answers 200, and the hook never fires.
			// Without this row, the 404 above could be the harness's.
			id2 := e.create(tenant)
			e.db.fired, e.db.onUpdate = 0, nil
			if code, body := e.do(tenant, http.MethodPut, "/schedules/"+id2.String(), map[string]any{"enabled": true}); code != http.StatusOK || e.db.fired != 0 {
				t.Errorf("the same PUT with nothing deleted: status %d (%s), hook fired %d times; want 200 and 0", code, body, e.db.fired)
			}
		})
	}
}

func TestAPutThatChangesNothingIsNotA404OnAnySchedule(t *testing.T) {
	for _, be := range testutil.NewPluginTestBackends(t) {
		be := be
		t.Run(be.Name, func(t *testing.T) {
			defer be.Cleanup()
			e := newPutEnv(t, be)
			tenant := uuid.New()
			defer withFixtureCleanup(t, be, e.dialect, tenant)()
			id := e.create(tenant)

			// 3. The schedule is enabled and its cron fires once a year, so each of these assigns the values
			// the row already holds; only updated_at moves, and `now()` has second precision. MySQL reports
			// rows CHANGED, so for a PUT inside the same second it reports 0 for a schedule that exists.
			// A fix that read 0 as "gone" would answer 404 here on MySQL only.
			for i := 1; i <= 6; i++ {
				if code, body := e.do(tenant, http.MethodPut, "/schedules/"+id.String(), map[string]any{"enabled": true}); code != http.StatusOK {
					t.Fatalf("PUT #%d that changes nothing: status %d (%s), want 200", i, code, body)
				}
			}
		})
	}
}

func TestAPutForAScheduleThatIsNotThereIsStillA404(t *testing.T) {
	for _, be := range testutil.NewPluginTestBackends(t) {
		be := be
		t.Run(be.Name, func(t *testing.T) {
			defer be.Cleanup()
			e := newPutEnv(t, be)
			owner, other := uuid.New(), uuid.New()
			defer withFixtureCleanup(t, be, e.dialect, owner, other)()
			id := e.create(owner)

			// 4. What the existence read refused correctly before must still be refused: an id that never
			// existed, and another tenant's schedule.
			for name, tc := range map[string]struct {
				tenant uuid.UUID
				id     uuid.UUID
			}{
				"an id that never existed":  {owner, uuid.New()},
				"another tenant's schedule": {other, id},
			} {
				if code, body := e.do(tc.tenant, http.MethodPut, "/schedules/"+tc.id.String(), map[string]any{"enabled": true}); code != http.StatusNotFound {
					t.Errorf("PUT for %s: status %d (%s), want 404", name, code, body)
				}
			}
			if !e.exists(owner, id) {
				t.Error("the owner's schedule was changed or removed by the other tenant's PUT")
			}
		})
	}
}
