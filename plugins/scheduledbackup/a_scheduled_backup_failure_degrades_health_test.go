package scheduledbackup

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
)

// TestHealthDegradesWhenAScheduledBackupCannotResolveItsDSN is cleat#2246
// item 2: Health is driven by the attempt path (executeScheduledBackup
// actually failing to resolve scheduledbackup.dsn, background.go), not by a
// query run for its own sake, mirroring auditlog's lost-event health signal
// (plugins/auditlog/queue.go).
//
// Drives the real runDueBackups dispatch path (which dispatches
// executeScheduledBackup on its own goroutine) against real PostgreSQL,
// rather than calling Health after hand-setting the fields directly, so this
// proves the wiring in background.go actually calls Health's own
// bookkeeping -- a test that only exercised Health() in isolation could not
// catch background.go forgetting to record anything at all.
func TestHealthDegradesWhenAScheduledBackupCannotResolveItsDSN(t *testing.T) {
	db := testutil.TestDB(t, testutil.DialectPostgres)
	ctx := context.Background()
	dialect := plugin.Dialect(string(testutil.DialectPostgres))
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)

	seedPlugin := &Plugin{dialect: dialect, logger: quiet}
	if err := plugin.RunMigrations(ctx, db, dialect, nil,
		[]*plugin.LoadedPlugin{{Plugin: seedPlugin, Healthy: true}}); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	p := &Plugin{dialect: dialect, logger: quiet}
	p.db = &engine.SQLDBAdapter{DB: db, Dialect: dialect}
	p.config.DumpDir = t.TempDir()

	// Known-positive: before any attempt has run, a freshly-constructed
	// Plugin must report healthy -- the zero value of lastDSNUnavailable
	// must not itself read as "just failed".
	if err := p.Health(); err != nil {
		t.Fatalf("Health before any attempt: %v, want nil", err)
	}

	// No deploymentSecrets configured at all -- backupDSN's first branch --
	// is the known-positive for "an attempt fails to resolve the DSN".
	configID := uuid.New()
	past := time.Now().Add(-time.Hour)
	mustInsertDueConfig(t, db, dialect, configID, "cleat-2246-health", past)

	p.runDueBackups(ctx)
	waitForBgBackups(t, p)

	if err := p.Health(); err == nil {
		t.Fatalf("Health after a DSN-unavailable attempt: nil, want an error naming it")
	} else if !strings.Contains(err.Error(), "scheduledbackup.dsn") {
		t.Fatalf("Health error does not mention scheduledbackup.dsn: %v", err)
	}

	// Negative control: a later successful attempt does not INSTANTLY clear
	// the signal -- it starts the decay window (see Health's doc comment for
	// why a fixed decay from the ORIGINAL failure is wrong for something
	// that recurs once per cron period). Immediately after the success, the
	// window has not elapsed, so Health must still report an error.
	p.deploymentSecrets = &fakeBackupDeploymentSecrets{dsn: testBackupDSN}
	configID2 := uuid.New()
	mustInsertDueConfig(t, db, dialect, configID2, "cleat-2246-health-2", past)
	p.runDueBackups(ctx)
	waitForBgBackups(t, p)
	if err := p.Health(); err == nil {
		t.Fatalf("Health immediately after a later successful attempt: nil, want the decay window to not have elapsed yet")
	}

	// Assert the SUCCESS was actually recorded, not just that Health still
	// errors -- "err != nil" alone is satisfied identically by the
	// never-resolved latch branch, so it cannot tell "background.go recorded
	// the success and we're still inside its decay window" from
	// "background.go never called lastDSNResolved.Store at all". Confirmed:
	// deleting that Store call from background.go leaves this whole test
	// green without this check.
	failedAt := p.lastDSNUnavailable.Load()
	resolvedAt := p.lastDSNResolved.Load()
	if resolvedAt <= failedAt {
		t.Fatalf("lastDSNResolved (%d) is not after lastDSNUnavailable (%d) -- "+
			"the successful attempt's resolution was not recorded", resolvedAt, failedAt)
	}
}

// TestHealthStaysDegradedWithNoSuccessSinceTheFailure is cleat-review's R1 on
// #2764: a fixed decay window borrowed from auditlog is wrong here.
// auditlog's losses recur on every request, so "5 minutes since the last
// one" is a reasonable proxy for "still happening". A scheduled backup
// recurs once per cron period -- a daily schedule with no DSN set would be
// degraded for 5 of every 1,440 minutes under a naive decay, well under any
// `for:` duration an alert would use, so the signal would never fire in
// practice. Health must instead stay degraded indefinitely as long as the
// latest attempt is the failure itself, with no decay purely from elapsed
// time.
func TestHealthStaysDegradedWithNoSuccessSinceTheFailure(t *testing.T) {
	p := &Plugin{}

	// A failure far older than healthWindow, and nothing has ever resolved
	// it (lastDSNResolved is still the zero value) -- this is the daily-cron
	// case cleat-review named: elapsed time alone must not heal it.
	p.lastDSNUnavailable.Store(time.Now().Add(-24 * time.Hour).UnixNano())
	if err := p.Health(); err == nil {
		t.Fatalf("Health with a day-old failure and no success since: nil, want an error -- time alone must not decay this")
	}
}

// TestHealthDecaysForTheWindowAfterARecoveringAttempt covers the other half
// of the same fix: once a LATER attempt actually resolves the secret, the
// signal decays for healthWindow measured from that success, not from the
// original failure -- covering both sides of that window without sleeping 5
// minutes in a test.
func TestHealthDecaysForTheWindowAfterARecoveringAttempt(t *testing.T) {
	failedAt := time.Now().Add(-24 * time.Hour)

	p := &Plugin{}
	p.lastDSNUnavailable.Store(failedAt.UnixNano())
	p.lastDSNResolved.Store(time.Now().Add(-healthWindow + time.Minute).UnixNano())
	if err := p.Health(); err == nil {
		t.Fatalf("Health with a recovery inside the window: nil, want an error")
	}

	p.lastDSNResolved.Store(time.Now().Add(-healthWindow - time.Second).UnixNano())
	if err := p.Health(); err != nil {
		t.Fatalf("Health with a recovery past the window: %v, want nil", err)
	}
}
