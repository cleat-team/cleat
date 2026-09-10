package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
)

// cleat#1130. The endpoint exists so retention is observable from outside the
// engine, and the window override is what makes it more than a button.
//
// THE CASE THAT MATTERS IS TestASweepWithNoOverrideCannotReachAFreshRun. The
// configured window is integer DAYS, so the smallest cutoff the flags can
// express is 24 hours ago, and the predicate is `completed_at < cutoff`. An
// endpoint that only ran the configured sweep would match nothing for anything
// completed today -- on every call -- and report success. That is a feature
// that ships working and is provably inert, and it is the reason the override
// is not a convenience.

func sweepReq(t *testing.T, s *apiServer, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(http.MethodPost, "/api/admin/retention/sweep", nil)
	} else {
		r = httptest.NewRequest(http.MethodPost, "/api/admin/retention/sweep", strings.NewReader(body))
	}
	w := httptest.NewRecorder()
	s.handleRetentionSweep(w, r)
	return w
}

func TestSweepRejectsABadOrNonPositiveWindow(t *testing.T) {
	on := true
	old := enableAdminAPI
	enableAdminAPI = &on
	defer func() { enableAdminAPI = old }()
	s := &apiServer{}

	for _, tc := range []struct{ name, body string }{
		{"unparseable", `{"older_than":"soon"}`},
		{"zero", `{"older_than":"0s"}`},
		{"negative", `{"older_than":"-5s"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sweepReq(t, s, tc.body).Code; got != 400 {
				t.Errorf("older_than=%s answered %d, want 400. A non-positive window "+
					"puts the cutoff at or after now, which would sweep live work -- "+
					"the flags cannot express that and neither may this.", tc.body, got)
			}
		})
	}
}

// The gate is inherited, not re-decided.
func TestSweepIsGatedOnTheAdminAPIFlag(t *testing.T) {
	off := false
	old := enableAdminAPI
	enableAdminAPI = &off
	defer func() { enableAdminAPI = old }()

	if got := sweepReq(t, &apiServer{}, "").Code; got != 404 {
		t.Errorf("with --enable-admin-api off the sweep answered %d, want 404. It is a "+
			"destructive operator endpoint and must inherit that exposure decision "+
			"rather than making a new one.", got)
	}
}

func TestSweepRejectsNonPost(t *testing.T) {
	on := true
	old := enableAdminAPI
	enableAdminAPI = &on
	defer func() { enableAdminAPI = old }()

	r := httptest.NewRequest(http.MethodGet, "/api/admin/retention/sweep", nil)
	w := httptest.NewRecorder()
	(&apiServer{}).handleRetentionSweep(w, r)
	if w.Code != 405 {
		t.Errorf("GET answered %d, want 405", w.Code)
	}
}

// A disabled arm must be named, not silently reported as zero.
func TestADisabledArmIsNamedRatherThanReportedAsZero(t *testing.T) {
	var res retentionSweepResult
	// Shape assertion only: the result type must be able to say "skipped".
	// A zero count with no explanation cannot distinguish "this arm is off"
	// from "this arm found nothing", and the two defaults differ -- 30 on,
	// 0 off -- so an operator reading `0` needs to know which.
	res.Skipped = append(res.Skipped, "completed_workflows (--completed-workflow-retention-days is 0)")
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), "skipped") {
		t.Errorf("the result does not carry a skipped list: %s", b)
	}
	var empty retentionSweepResult
	b2, _ := json.Marshal(empty)
	if strings.Contains(string(b2), "skipped") {
		t.Errorf("an empty skip list must be omitted, or every response implies "+
			"something was disabled: %s", b2)
	}
}

// The endpoint actually deletes, and — the half that matters — it does NOT
// without the override.
//
// This is the whole claim of cleat#1130 reduced to two requests against a real
// database. The configured window is 30 days; a run completed a moment ago is
// not `completed_at < now-30d`, so the sweep the flags describe can never reach
// it. If the first sub-test passed and the second did too, the endpoint would be
// a button that reports success and changes nothing — which is what it would
// have been without `older_than`.
func TestTheSweepDeletesOnlyWithTheOverride(t *testing.T) {
	db := testutil.SuiteTestDB(t, "cleat_worker")
	store := engine.NewPostgresStore(db)
	ctx := context.Background()

	on := true
	oldGate, oldDays := enableAdminAPI, completedWorkflowRetentionDays
	days := 30
	enableAdminAPI, completedWorkflowRetentionDays = &on, &days
	defer func() { enableAdminAPI, completedWorkflowRetentionDays = oldGate, oldDays }()

	w := &Worker{store: store, ctx: ctx, Metrics: newTestPrometheus(),
		logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	s := &apiServer{worker: w}

	newCompletedRun := func(t *testing.T) string {
		t.Helper()
		const def = "retention-sweep-endpoint"
		if err := store.DeployWorkflowDef(ctx, &engine.WorkflowDef{
			Name: def, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
			ABIVersion: 1, MinVersion: 1,
		}); err != nil {
			t.Fatalf("deploy: %v", err)
		}
		id := "rse-" + strconv.FormatInt(time.Now().UnixNano(), 10)
		if _, _, err := store.StartNewRun(ctx, id, def, 1, json.RawMessage(`{}`), "",
			engine.DefaultTenantUUID, 0); err != nil {
			t.Fatalf("StartNewRun: %v", err)
		}
		// Terminal and completed now. Set through the same columns
		// DeleteCompletedWorkflows selects on, against the schema the migrations
		// built -- not a table invented for this test.
		if _, err := db.ExecContext(ctx,
			`UPDATE workflow_instances SET status = 'done', completed_at = now() WHERE id = $1`, id); err != nil {
			t.Fatalf("mark completed: %v", err)
		}
		return id
	}
	exists := func(t *testing.T, id string) bool {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx,
			`SELECT count(*) FROM workflow_instances WHERE id = $1`, id).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		return n > 0
	}

	t.Run("no override cannot reach a fresh run", func(t *testing.T) {
		id := newCompletedRun(t)
		if got := sweepReq(t, s, "").Code; got != 200 {
			t.Fatalf("sweep answered %d, want 200", got)
		}
		if !exists(t, id) {
			t.Error("the configured sweep deleted a run that completed moments ago. " +
				"The window is 30 integer days and the predicate is completed_at < " +
				"cutoff, so this cannot happen -- and if it can, the endpoint is " +
				"sweeping live work.")
		}
	})

	t.Run("override reaches it", func(t *testing.T) {
		id := newCompletedRun(t)
		if got := sweepReq(t, s, `{"older_than":"1ns"}`).Code; got != 200 {
			t.Fatalf("sweep answered %d, want 200", got)
		}
		if exists(t, id) {
			t.Error("the run survived a sweep with older_than=1ns. Without a window " +
				"override the endpoint can never reach anything completed today, on " +
				"any call, while reporting success -- a feature that ships working " +
				"and is provably inert (cleat#1130).")
		}
	})
}

// An override must not enable an arm the configuration disabled.
//
// This test exists because a mutation found it missing. Flipping the guard to
// `completedWorkflowRetentionDays > 0 || window > 0` — letting a request body
// switch on a disabled sweep — was caught by nothing.
//
// It matters more than the other constraints. --completed-workflow-retention-days
// deletes the workflow_instances row itself: the status, result, error and
// def_name, not just the step history. It is off by default for exactly that
// reason, and its own flag help calls that "materially more destructive". A
// request body is not where a deployment's decision to leave it off gets
// reversed — an operator triggering a sweep to apply a config change would
// silently get a destructive one they never enabled.
func TestAnOverrideDoesNotEnableADisabledArm(t *testing.T) {
	db := testutil.SuiteTestDB(t, "cleat_worker")
	store := engine.NewPostgresStore(db)
	ctx := context.Background()

	on := true
	zero := 0
	oldGate, oldDays := enableAdminAPI, completedWorkflowRetentionDays
	enableAdminAPI, completedWorkflowRetentionDays = &on, &zero
	defer func() { enableAdminAPI, completedWorkflowRetentionDays = oldGate, oldDays }()

	w := &Worker{store: store, ctx: ctx, Metrics: newTestPrometheus(),
		logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	s := &apiServer{worker: w}

	const def = "retention-sweep-disabled-arm"
	if err := store.DeployWorkflowDef(ctx, &engine.WorkflowDef{
		Name: def, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
		ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	id := "rsd-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	if _, _, err := store.StartNewRun(ctx, id, def, 1, json.RawMessage(`{}`), "",
		engine.DefaultTenantUUID, 0); err != nil {
		t.Fatalf("StartNewRun: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE workflow_instances SET status = 'done', completed_at = now() WHERE id = $1`, id); err != nil {
		t.Fatalf("mark completed: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM workflow_instances WHERE id = $1`, id)
	})

	rec := sweepReq(t, s, `{"older_than":"1ns"}`)
	if rec.Code != 200 {
		t.Fatalf("sweep answered %d, want 200", rec.Code)
	}

	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM workflow_instances WHERE id = $1`, id).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n == 0 {
		t.Error("an older_than override deleted a workflow record while " +
			"--completed-workflow-retention-days is 0. That flag is off by default " +
			"because it destroys the run's outcome, not just its history; a request " +
			"body must not switch it on.")
	}

	var res retentionSweepResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var named bool
	for _, sk := range res.Skipped {
		if strings.Contains(sk, "completed_workflows") {
			named = true
		}
	}
	if !named {
		t.Errorf("the disabled arm is not named in `skipped`: %v.\n\n"+
			"Its count is 0 either way, so without this an operator cannot tell "+
			"\"that sweep is off\" from \"it found nothing\" -- and the two arms "+
			"default differently, 30 on and 0 off.", res.Skipped)
	}
}
