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
// item 2: Health is driven by the attempt path (runDueBackups actually
// failing to resolve scheduledbackup.dsn), not by a query run for its own
// sake, mirroring auditlog's lost-event health signal
// (plugins/auditlog/queue.go).
//
// Drives the real runDueBackups dispatch path against real PostgreSQL,
// rather than calling Health after hand-setting the field directly, so this
// proves the wiring in background.go actually calls Health's own
// bookkeeping -- a test that only exercised Health() in isolation could not
// catch background.go forgetting to record the failure at all.
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

	// Negative control: the window has not elapsed, and a healthy dispatch
	// (secret now resolves) does not itself clear the signal -- an operator
	// needs to see that SOMETHING failed recently even if the next attempt
	// succeeds, since the failed attempt's backup was never taken.
	p.deploymentSecrets = &fakeBackupDeploymentSecrets{dsn: testBackupDSN}
	configID2 := uuid.New()
	mustInsertDueConfig(t, db, dialect, configID2, "cleat-2246-health-2", past)
	p.runDueBackups(ctx)
	waitForBgBackups(t, p)
	if err := p.Health(); err == nil {
		t.Fatalf("Health after a later successful attempt: nil, want the earlier failure to still be reported (window has not elapsed)")
	}
}

// TestHealthRecoversAfterTheWindowElapses confirms the signal decays rather
// than latching, the same shape as auditlog's: an operator should not see a
// permanently-red worker over one transient DSN outage. Manipulates the
// recorded time directly rather than sleeping healthWindow (5 minutes) in a
// test -- Health's own arithmetic is what is under test, not the clock.
func TestHealthRecoversAfterTheWindowElapses(t *testing.T) {
	p := &Plugin{}

	p.lastDSNUnavailable.Store(time.Now().Add(-healthWindow + time.Minute).UnixNano())
	if err := p.Health(); err == nil {
		t.Fatalf("Health with a failure inside the window: nil, want an error")
	}

	p.lastDSNUnavailable.Store(time.Now().Add(-healthWindow - time.Second).UnixNano())
	if err := p.Health(); err != nil {
		t.Fatalf("Health with a failure past the window: %v, want nil", err)
	}
}
