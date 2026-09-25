package scheduledbackup

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/plugin"
	"github.com/cleat-team/cleat/plugins/plugintest"
	"github.com/google/uuid"
)

// =========================================================================
// Cron parser — additional edge cases
// =========================================================================

func TestParseCron_InvalidFieldCount(t *testing.T) {
	tests := []string{
		"",
		"* * * *",
		"* * * * * *", // 6 fields
	}
	for _, expr := range tests {
		_, err := parseCron(expr)
		if err == nil {
			t.Errorf("%q: expected error", expr)
		}
	}
}

func TestParseCron_StepPatterns(t *testing.T) {
	ce, err := parseCron("*/5 */2 */3 */1 */2")
	if err != nil {
		t.Fatalf("parseCron: %v", err)
	}
	if ce.minute.step != 5 || ce.minute.stepMin != 0 {
		t.Error("minute step=5 min=0")
	}
	if !ce.minute.matches(5) || !ce.minute.matches(10) || ce.minute.matches(3) {
		t.Error("minute step matching wrong")
	}
	if ce.hour.step != 2 || ce.hour.stepMin != 0 {
		t.Error("hour step=2 min=0")
	}
	if ce.dayOfMonth.step != 3 || ce.dayOfMonth.stepMin != 1 {
		t.Error("dayOfMonth step=3 min=1")
	}
}

func TestParseCron_CommaList(t *testing.T) {
	ce, err := parseCron("0,15,30,45 * * * *")
	if err != nil {
		t.Fatalf("parseCron: %v", err)
	}
	if !ce.minute.matches(0) || !ce.minute.matches(15) || !ce.minute.matches(30) {
		t.Error("comma list minute matching wrong")
	}
	if ce.minute.matches(10) {
		t.Error("10 should not match")
	}
}

func TestParseCron_RangeWithStep(t *testing.T) {
	ce, err := parseCron("0-30/10 * * * *")
	if err != nil {
		t.Fatalf("parseCron: %v", err)
	}
	if !ce.minute.matches(0) || !ce.minute.matches(10) || !ce.minute.matches(20) || !ce.minute.matches(30) {
		t.Error("range/step minute matching wrong")
	}
	if ce.minute.matches(5) || ce.minute.matches(15) || ce.minute.matches(25) {
		t.Error("5/15/25 should not match")
	}
}

func TestParseCron_SingleValue(t *testing.T) {
	ce, err := parseCron("42 14 * * 1")
	if err != nil {
		t.Fatalf("parseCron: %v", err)
	}
	if !ce.minute.matches(42) {
		t.Error("minute 42 should match")
	}
	if ce.minute.matches(43) {
		t.Error("minute 43 should not match")
	}
	if !ce.hour.matches(14) {
		t.Error("hour 14 should match")
	}
	if !ce.dayOfWeek.matches(1) { // Monday
		t.Error("Monday should match")
	}
}

func TestParseCron_InvalidValues(t *testing.T) {
	tests := []struct{ expr, contains string }{
		{"60 * * * *", "minute"},
		{"* 24 * * *", "hour"},
		{"* * 0 * *", "day of month"},
		{"* * * 0 *", "month"},
		{"* * * * 7", "day of week"},
		{"abc * * * *", "invalid"},
		{"* * * * abc", "invalid"},
	}
	for _, tc := range tests {
		_, err := parseCron(tc.expr)
		if err == nil {
			t.Errorf("%q: expected error", tc.expr)
			continue
		}
		if !strings.Contains(strings.ToLower(err.Error()), tc.contains) {
			t.Logf("%q error: %v (wanted containing %q)", tc.expr, err, tc.contains)
		}
	}
}

func TestParseCron_InvalidStep(t *testing.T) {
	_, err := parseCron("*/0 * * * *")
	if err == nil {
		t.Error("step 0 should fail")
	}
	_, err = parseCron("*/abc * * * *")
	if err == nil {
		t.Error("step abc should fail")
	}
}

func TestNextRun_Midnight(t *testing.T) {
	from := time.Date(2026, 5, 8, 0, 0, 0, 0, time.UTC)
	next := nextRun("0 0 * * *", from)
	expected := time.Date(2026, 5, 9, 0, 0, 0, 0, time.UTC)
	if !next.Equal(expected) {
		t.Errorf("want %v, got %v", expected, next)
	}
}

func TestNextRun_Feb29NonLeapYear(t *testing.T) {
	// Feb 29 in a non-leap year shouldn't match, should roll to Mar 1.
	// nextRun iterates minute by minute so it should skip Feb 29.
	from := time.Date(2025, 2, 28, 23, 59, 0, 0, time.UTC)
	next := nextRun("0 0 29 2 *", from)
	if next.IsZero() {
		t.Skip("Feb 29 in 2025: no match found in 1 year window")
	}
	t.Logf("Feb 29 cron from Feb 28 2025: %v", next)
}

func TestNextRun_WrapYear(t *testing.T) {
	from := time.Date(2026, 12, 31, 23, 59, 0, 0, time.UTC)
	next := nextRun("0 0 1 1 *", from) // Jan 1 at 00:00
	if next.IsZero() {
		t.Error("Jan 1 should be found within a year")
	}
	if next.Year() != 2027 {
		t.Errorf("want 2027, got %v", next)
	}
}

// =========================================================================
// Plugin lifecycle
// =========================================================================

func TestSB_Info(t *testing.T) {
	p := &Plugin{}
	info := p.Info()
	if info.Name != "scheduled-backup" {
		t.Errorf("want scheduled-backup, got %s", info.Name)
	}
	if info.Version == "" {
		t.Error("version should not be empty")
	}
}

func TestSB_Init_DefaultsDumpDir(t *testing.T) {
	p := &Plugin{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	env := &plugin.Environment{
		DB:     nil,
		Mux:    http.NewServeMux(),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if err := p.Init(context.Background(), env); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if p.config.DumpDir != "/tmp/cleat-backups" {
		t.Errorf("default dump dir: got %q", p.config.DumpDir)
	}
}

func TestSB_Init_WithConfig(t *testing.T) {
	p := &Plugin{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	env := &plugin.Environment{
		DB:     nil,
		Mux:    http.NewServeMux(),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config: json.RawMessage(`{"dump_dir":"/tmp/my-backups"}`),
	}
	if err := p.Init(context.Background(), env); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if p.config.DumpDir != "/tmp/my-backups" {
		t.Errorf("got dump dir %q", p.config.DumpDir)
	}
}

// TestSB_InitWarnsOnLeftoverDSN is the mirror of slacknotify's
// TestSN_InitWarnsOnLeftoverSigningSecret: a dsn left over in
// --plugin-config from before cleat#1992 part 1b no longer does anything --
// Config has no field for it -- so Init must WARN naming the dead field and
// the replacement command, rather than silently ignoring it.
func TestSB_InitWarnsOnLeftoverDSN(t *testing.T) {
	var buf bytes.Buffer
	p := &Plugin{}
	env := &plugin.Environment{
		Logger: slog.New(slog.NewTextHandler(&buf, nil)),
		Config: json.RawMessage(`{"dsn":"postgres://old-secret", "dump_dir":"/tmp/my-backups"}`),
	}
	if err := p.Init(context.Background(), env); err != nil {
		t.Fatalf("Init: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "dsn") || !strings.Contains(got, "no longer read") {
		t.Errorf("expected a WARN naming the dead dsn field, got log output: %q", got)
	}
	if !strings.Contains(got, "set-deployment-secret") {
		t.Errorf("expected the WARN to name the replacement command, got: %q", got)
	}
}

// TestSB_InitNoWarnWithoutLeftoverDSN is the negative control: a config with
// no dsn field at all (or none) must not log the leftover-key WARN.
func TestSB_InitNoWarnWithoutLeftoverDSN(t *testing.T) {
	var buf bytes.Buffer
	p := &Plugin{}
	env := &plugin.Environment{
		Logger: slog.New(slog.NewTextHandler(&buf, nil)),
		Config: json.RawMessage(`{"dump_dir":"/tmp/my-backups"}`),
	}
	if err := p.Init(context.Background(), env); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if got := buf.String(); strings.Contains(got, "no longer read") {
		t.Errorf("did not expect a leftover-dsn WARN with no dsn field in config, got: %q", got)
	}
}

// TestSB_RequiredDeploymentSecrets_NoLegacyKey is the "ordinary deployment"
// case, mirroring slacknotify's TestSN_RequiredDeploymentSecrets_NoLegacyKey:
// no legacy dsn anywhere in --plugin-config (whether scheduled-backup has no
// config section at all, or an empty one) must not require
// scheduledbackup.dsn -- a deployment with no history of scheduled backups
// must still boot with no DSN configured.
func TestSB_RequiredDeploymentSecrets_NoLegacyKey(t *testing.T) {
	p := &Plugin{}
	for _, cfg := range [][]byte{nil, []byte(``), []byte(`{}`)} {
		names, err := p.RequiredDeploymentSecrets(cfg)
		if err != nil {
			t.Fatalf("RequiredDeploymentSecrets(%q): %v", cfg, err)
		}
		if len(names) != 0 {
			t.Errorf("RequiredDeploymentSecrets(%q) = %v, want none (no legacy key present)", cfg, names)
		}
	}
}

// TestSB_RequiredDeploymentSecrets_LegacyKeyPresent is the upgrade case:
// --plugin-config still carries dsn from before cleat#1992 part 1b, proving
// this deployment ran scheduled backups against a real database. Without
// this, scheduledbackup.dsn being unset would let the worker boot and then
// silently fail every backup attempt, with nothing at boot saying why.
func TestSB_RequiredDeploymentSecrets_LegacyKeyPresent(t *testing.T) {
	p := &Plugin{}
	cfg := []byte(`{"dsn": "postgres://old-secret"}`)
	names, err := p.RequiredDeploymentSecrets(cfg)
	if err != nil {
		t.Fatalf("RequiredDeploymentSecrets: %v", err)
	}
	if len(names) != 1 || names[0] != "scheduledbackup.dsn" {
		t.Errorf("RequiredDeploymentSecrets(legacy key present) = %v, want [scheduledbackup.dsn]", names)
	}
}

func TestSB_Init_InvalidConfig(t *testing.T) {
	p := &Plugin{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	env := &plugin.Environment{
		DB:     nil,
		Mux:    http.NewServeMux(),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config: json.RawMessage(`bad config`),
	}
	err := p.Init(context.Background(), env)
	if err == nil || !strings.Contains(err.Error(), "invalid config") {
		t.Errorf("want invalid config error, got: %v", err)
	}
}

// TestSB_RegisterRoutes_NilMux, TestSB_RegisterRoutes_Valid,
// TestSB_RouteErrorPaths_MissingTenant, TestSB_RouteErrorPaths_TenantInvalidID,
// TestSB_CLI_BackupRun_*, TestSB_CLI_BackupList_*, TestSB_RegisterCommands,
// TestSB_BackupConfig_JSON, TestSB_BackupHistory_JSON, TestSB_CreateConfig_*,
// TestSB_ListConfigs_*, TestSB_GetConfig_*, TestSB_UpdateConfig_* (route
// variants), TestSB_DeleteConfig_*, TestSB_ListHistory_* (route variants),
// TestSB_RunBackup_Success/NotFound, TestSB_CRUD_FullLifecycle,
// TestSB_ErrorPaths_InvalidID, TestSB_ErrorPaths_MissingTenantWithDB,
// TestSB_RunBackupAsync_Error, TestSB_RunBackupAsync_NoDeploymentSecrets and
// every TestSB_DBError_* aimed at a route handler were removed with
// routes.go and commands.go: cleat#2247 made backup configuration
// operator-only, so there is no longer a tenant-facing HTTP surface
// (RegisterRoutes), a plugin.HasCommands implementation (RegisterCommands,
// cliBackupRun, cliBackupList), or a runBackupAsync (the HTTP "run now"
// handler's async half) to test. Operator access is now
// cmd/cleatctl/backup.go, which talks to backup_config/backup_history
// directly the same way slackworkspace.go and audit.go do for their own
// plugins, not through the Plugin interface. Note RegisterCommands was never
// actually wired to anything: no caller anywhere in this tree ever
// type-asserted a loaded plugin against plugin.HasCommands, so its two CLI
// commands were unreachable dead code even before this issue (cleat#2255).

// =========================================================================
// Migrations
// =========================================================================

func TestSB_Migrations(t *testing.T) {
	p := &Plugin{}
	migrations := p.Migrations()
	if len(migrations) == 0 {
		t.Error("expected migrations")
	}
	hasBackupConfig := false
	hasBackupHistory := false
	for _, m := range migrations {
		if m.Version == 0 {
			t.Error("migration version must be non-zero")
		}
		if strings.Contains(m.Up, "backup_config") {
			hasBackupConfig = true
		}
		if strings.Contains(m.Up, "backup_history") {
			hasBackupHistory = true
		}
	}
	if !hasBackupConfig {
		t.Error("missing backup_config migration")
	}
	if !hasBackupHistory {
		t.Error("missing backup_history migration")
	}
}

// =========================================================================
// nextRun edge cases
// =========================================================================

func TestNextRun_SameMinute(t *testing.T) {
	// At 09:00:30, "0 9 * * *" should match 09:00 tomorrow, not today.
	from := time.Date(2026, 5, 8, 9, 0, 30, 0, time.UTC)
	next := nextRun("0 9 * * *", from)
	expected := time.Date(2026, 5, 9, 9, 0, 0, 0, time.UTC)
	if !next.Equal(expected) {
		t.Errorf("want %v, got %v", expected, next)
	}
}

func TestPluginRegistrationBehavioral(t *testing.T) {
	plugins, err := plugin.Discover()
	if err != nil {
		t.Fatalf("Discover() returned error: %v", err)
	}
	found := false
	for _, lp := range plugins {
		if lp.Plugin.Info().Name == "scheduled-backup" {
			found = true
			break
		}
	}
	if !found {
		t.Error("scheduled-backup plugin not found after Discover")
	}
}

// =========================================================================
// Fake DB driver for scheduledbackup behavioral tests
//
// No tenant_id anywhere below (cleat#2247): backup_config and backup_history
// stopped being tenant-scoped in the v4 migration, and background.go's
// queries dropped every tenant_id column and parameter to match. This fake
// driver only needs to model what the background loop actually reads and
// writes now -- runDueBackups' claim query, executeScheduledBackup's history
// insert and status updates, and updateNextRun/updateNextRunTx's config
// update. Everything that modeled the deleted HTTP handlers' SQL (list,
// get, dynamic update, delete, and the tenant-filtered forms of all of
// them) went with routes.go.
// =========================================================================

type sbConfigRow struct {
	id            string
	name          string
	cron          string
	s3Bucket      string
	s3Prefix      string
	retentionDays int
	enabled       bool
	lastRunAt     *time.Time
	nextRunAt     *time.Time
	createdAt     time.Time
	updatedAt     time.Time
}

type sbHistoryRow struct {
	id           string
	configID     string
	filename     string
	status       string
	sizeBytes    *int64
	startedAt    time.Time
	createdAt    time.Time
	completedAt  *time.Time
	errorMessage *string
}

type sbDB struct {
	mu            sync.RWMutex
	configs       map[string]*sbConfigRow
	history       map[string]*sbHistoryRow
	forceQueryErr int // decrementing counter; fail when > 0
	forceExecErr  int // decrementing counter; fail when > 0
}

func newSBDB() *sbDB {
	return &sbDB{
		configs: make(map[string]*sbConfigRow),
		history: make(map[string]*sbHistoryRow),
	}
}

// ---- driver interfaces ----

type sbConnector struct{ db *sbDB }

func (c *sbConnector) Connect(_ context.Context) (driver.Conn, error) { return &sbConn{db: c.db}, nil }
func (c *sbConnector) Driver() driver.Driver                          { return &sbDrv{} }

type sbDrv struct{}

func (*sbDrv) Open(_ string) (driver.Conn, error) { return nil, fmt.Errorf("not supported") }

type sbConn struct {
	db *sbDB
}

func (*sbConn) Prepare(_ string) (driver.Stmt, error) { return nil, fmt.Errorf("unexpected Prepare") }
func (*sbConn) Close() error                          { return nil }
func (*sbConn) Begin() (driver.Tx, error)             { return &sbTx{}, nil }

type sbTx struct{}

func (*sbTx) Commit() error   { return nil }
func (*sbTx) Rollback() error { return nil }

type sbResult struct{ n int64 }

func (r *sbResult) LastInsertId() (int64, error) { return 0, nil }
func (r *sbResult) RowsAffected() (int64, error) { return r.n, nil }

type sbRows struct {
	columns []string
	data    [][]driver.Value
	pos     int
}

func (r *sbRows) Columns() []string { return r.columns }
func (r *sbRows) Close() error      { return nil }
func (r *sbRows) Next(dest []driver.Value) error {
	if r.pos >= len(r.data) {
		return io.EOF
	}
	copy(dest, r.data[r.pos])
	r.pos++
	return nil
}

// ---- arg helpers ----

func sbArgS(args []driver.NamedValue, ordinal int) (string, error) {
	for _, a := range args {
		if a.Ordinal == ordinal {
			switch v := a.Value.(type) {
			case string:
				return v, nil
			case []byte:
				return string(v), nil
			case uuid.UUID:
				return v.String(), nil
			case [16]byte:
				return fmt.Sprintf("%x", v), nil
			default:
				return fmt.Sprintf("%v", v), nil
			}
		}
	}
	return "", fmt.Errorf("arg %d not found", ordinal)
}

func sbArgAny(args []driver.NamedValue, ordinal int) (driver.Value, error) {
	for _, a := range args {
		if a.Ordinal == ordinal {
			return a.Value, nil
		}
	}
	return nil, fmt.Errorf("arg %d not found", ordinal)
}

// =====================================================================
// ExecContext
// =====================================================================

func (c *sbConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.db.mu.Lock()
	forceErr := c.db.forceExecErr > 0
	if forceErr {
		c.db.forceExecErr--
	}
	c.db.mu.Unlock()
	if forceErr {
		return nil, fmt.Errorf("sbConn: forced exec error")
	}

	c.db.mu.Lock()
	defer c.db.mu.Unlock()

	q := strings.Join(strings.Fields(query), " ")
	switch {
	case strings.Contains(q, "INSERT INTO backup_history"):
		return c.execInsertHistory(args)
	case strings.Contains(q, "UPDATE backup_config SET"):
		return c.execUpdateConfig(args)
	case strings.Contains(q, "UPDATE backup_history SET"):
		return c.execUpdateHistory(q, args)
	default:
		return nil, fmt.Errorf("sbConn: unexpected Exec: %.80s", q)
	}
}

func (c *sbConn) execInsertHistory(args []driver.NamedValue) (driver.Result, error) {
	// INSERT INTO backup_history (id, config_id, filename, status,
	// started_at, created_at) VALUES ($1, $2, $3, 'running', $4, $4)
	// -- Args: id(1), config_id(2), filename(3), started_at(4)
	id, _ := sbArgS(args, 1)
	configID, _ := sbArgS(args, 2)
	filename, _ := sbArgS(args, 3)
	startedVal, _ := sbArgAny(args, 4)

	startedAt := time.Now()
	if t, ok := startedVal.(time.Time); ok {
		startedAt = t
	}

	c.db.history[id] = &sbHistoryRow{
		id: id, configID: configID,
		filename: filename, status: "running",
		startedAt: startedAt, createdAt: startedAt,
	}
	return &sbResult{n: 1}, nil
}

// execUpdateConfig handles the only UPDATE backup_config statement left
// once routes.go's dynamic UPDATE and the deleted runBackupAsync's
// next_run_at=NULL form went with it:
// updateNextRun/updateNextRunTx's SET last_run_at = $1, next_run_at = $2
// WHERE id = $3.
func (c *sbConn) execUpdateConfig(args []driver.NamedValue) (driver.Result, error) {
	if len(args) < 3 {
		return &sbResult{n: 0}, nil
	}
	nowVal, _ := sbArgAny(args, 1)
	nextVal, _ := sbArgAny(args, 2)
	configID, _ := sbArgS(args, 3)
	row, ok := c.db.configs[configID]
	if !ok {
		return &sbResult{n: 0}, nil
	}
	if t, ok := nowVal.(time.Time); ok {
		row.lastRunAt = &t
	}
	if t, ok := nextVal.(time.Time); ok {
		row.nextRunAt = &t
	}
	row.updatedAt = time.Now()
	return &sbResult{n: 1}, nil
}

func (c *sbConn) execUpdateHistory(q string, args []driver.NamedValue) (driver.Result, error) {
	n := len(args)

	// Handle no-arg bulk UPDATE: cleanup orphaned history.
	// Query contains "'running'" and "started_at <" when called from
	// cleanupOrphanedHistory.
	if n == 0 && strings.Contains(q, "'running'") && strings.Contains(q, "started_at <") {
		now := time.Now()
		cutoff := now.Add(-1 * time.Hour)
		var affected int64
		for _, row := range c.db.history {
			if row.status == "running" && row.startedAt.Before(cutoff) {
				row.status = "failed"
				errMsg := "worker crashed or timed out"
				row.errorMessage = &errMsg
				row.completedAt = &now
				affected++
			}
		}
		return &sbResult{n: affected}, nil
	}

	if n == 0 {
		return &sbResult{n: 0}, nil
	}
	// Last arg is always history ID
	historyID, _ := sbArgS(args, n)
	row, ok := c.db.history[historyID]
	if !ok {
		return &sbResult{n: 0}, nil
	}

	if strings.Contains(q, "status = 'completed'") {
		row.status = "completed"
		if sizeVal, err := sbArgAny(args, 1); err == nil {
			if s, ok := sizeVal.(int64); ok {
				row.sizeBytes = &s
			}
		}
		now := time.Now()
		row.completedAt = &now
	} else if strings.Contains(q, "status = 'failed'") {
		row.status = "failed"
		// Since cleat#2247 this is one of the stable error codes
		// (backupErrDSNUnavailable, backupErrUnsafePath,
		// backupErrPgDumpFailed), not raw error text -- see
		// background.go's doc comment on those constants. The fake driver
		// does not care which: it stores whatever string arrives, same as
		// the real column does.
		if errMsg, err := sbArgS(args, 1); err == nil {
			row.errorMessage = &errMsg
		}
		now := time.Now()
		row.completedAt = &now
	}
	return &sbResult{n: 1}, nil
}

// =====================================================================
// QueryContext
// =====================================================================

func (c *sbConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.db.mu.Lock()
	forceErr := c.db.forceQueryErr > 0
	if forceErr {
		c.db.forceQueryErr--
	}
	c.db.mu.Unlock()
	if forceErr {
		return nil, fmt.Errorf("sbConn: forced query error")
	}

	c.db.mu.RLock()
	defer c.db.mu.RUnlock()

	q := strings.Join(strings.Fields(query), " ")
	if strings.Contains(q, "enabled = true") && strings.Contains(q, "next_run_at") {
		return c.queryDueBackups(args)
	}
	return nil, fmt.Errorf("sbConn: unexpected Query: %.80s", q)
}

func (c *sbConn) queryDueBackups(_ []driver.NamedValue) (driver.Rows, error) {
	// SELECT id, name, cron FROM backup_config WHERE enabled = true AND
	// next_run_at <= now() FOR UPDATE SKIP LOCKED -- no tenant_id column
	// (cleat#2247), no WHERE argument to read.
	now := time.Now()
	var data [][]driver.Value
	for _, row := range c.db.configs {
		if row.enabled && row.nextRunAt != nil && !row.nextRunAt.After(now) {
			data = append(data, []driver.Value{row.id, row.name, row.cron})
		}
	}
	if data == nil {
		data = [][]driver.Value{}
	}
	return &sbRows{columns: []string{"id", "name", "cron"}, data: data}, nil
}

// =====================================================================
// Helpers
// =====================================================================

// testBackupDSN is the value newSBPlugin's default fakeBackupDeploymentSecrets
// answers for "scheduledbackup.dsn" -- matching what every pre-cleat#1992-part-1b
// test in this file set directly via p.config.DSN. Tests exercising the
// missing/failing-lookup path override p.deploymentSecrets (or clear it)
// after newSBPlugin returns.
const testBackupDSN = "postgres://test"

// fakeBackupDeploymentSecrets is a plugin.DeploymentSecrets that answers one
// fixed value for "scheduledbackup.dsn" and an error for anything else, or
// always errors if errOnGet is set -- the same shape as email's, llm's and
// slacknotify's fakeDeploymentSecrets test doubles.
type fakeBackupDeploymentSecrets struct {
	dsn      string
	errOnGet error
}

func (f *fakeBackupDeploymentSecrets) Get(ctx context.Context, name string) (string, error) {
	if f.errOnGet != nil {
		return "", f.errOnGet
	}
	if name == "scheduledbackup.dsn" {
		return f.dsn, nil
	}
	return "", fmt.Errorf("fakeBackupDeploymentSecrets: %q not set", name)
}

// newSBPlugin builds a Plugin wired to an in-memory fake database, with no
// HTTP mux (cleat#2247: there are no routes to register any more).
func newSBPlugin(t *testing.T) (*Plugin, *sbDB, *sql.DB) {
	t.Helper()
	fdb := newSBDB()
	rawDB := sql.OpenDB(&sbConnector{db: fdb})
	p := &Plugin{
		db:                &engine.SQLDBAdapter{DB: rawDB},
		logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		deploymentSecrets: &fakeBackupDeploymentSecrets{dsn: testBackupDSN},
	}
	// A real dump directory, which this harness never set (cleat#1305).
	//
	// It mattered as soon as SafeDumpPath started refusing an unconfigured one:
	// `filepath.Join("", name)` is a RELATIVE path, so the previous behaviour
	// was `pg_dump -f manual_x.dump` into the worker's working directory. One
	// test at :2016 already set this; the shared harness did not.
	p.config.DumpDir = t.TempDir()
	return p, fdb, rawDB
}

// installFakePgDump puts a fake pg_dump executable first on PATH for the
// rest of the test (restored via t.Cleanup), so runPgDump's
// exec.CommandContext runs it instead of a real pg_dump. body is the rest of
// the script after the shebang; it runs after the fake binary has already
// signalled it started (see the returned path).
//
// Returns the path to a marker file the fake binary creates as its first
// action, before body runs -- so a test can wait for the fake process to
// actually be running before acting on it (e.g. cancelling a context),
// without a fixed sleep.
func installFakePgDump(t *testing.T, body string) (startedMarker string) {
	t.Helper()
	fakeDir := t.TempDir()
	fakeBin := filepath.Join(fakeDir, "pg_dump")
	startedMarker = filepath.Join(fakeDir, "started.marker")
	script := "#!/bin/bash\n" +
		"echo started > " + startedMarker + "\n" +
		body
	if err := os.WriteFile(fakeBin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	oldPath := os.Getenv("PATH")
	if err := os.Setenv("PATH", fakeDir+string(os.PathListSeparator)+oldPath); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Setenv("PATH", oldPath) })
	if p, err := exec.LookPath("pg_dump"); err != nil || p != fakeBin {
		t.Fatalf("fake pg_dump not first on PATH: %v %v", p, err)
	}
	return startedMarker
}

// waitForFile polls for path to exist, failing the test if it doesn't appear
// within timeout.
func waitForFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s to appear", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitForBgBackups waits for p's in-flight scheduled backups to finish,
// failing the test rather than hanging forever if they don't.
//
// A deterministic wait, not a sleep: since cleat#2055, a due backup runs on
// its own goroutine that Run does not wait for, so nothing about Run
// returning tells you whether the backup it dispatched has finished. This is
// the replacement for the fixed sleep TestSB_Run_Cancel used to need before
// checking backup_history/backup_config.
func waitForBgBackups(t *testing.T, p *Plugin) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		p.bgBackups.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for dispatched backup to finish")
	}
}

// =========================================================================
// Background loop
// =========================================================================

func TestSB_Run_NilDB(t *testing.T) {
	p := &Plugin{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()
	time.Sleep(10 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run with nil DB: want nil, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not stop after cancel")
	}
}

// TestSB_Run_PollsEvenWithNoDeploymentSecretsWired is the replacement for
// what used to be TestSB_Run_NoDSN. Before cleat#1992 part 1b, Run refused
// to start its loop at all when p.config.DSN was empty at boot -- parking on
// <-ctx.Done() exactly like the p.db==nil case still does a few lines up.
// That gate is gone (see Run's own doc comment for why): setting a DSN via
// `cleatctl set-deployment-secret` after the worker has already started must
// take effect on the very next attempt, which an early exit here would have
// defeated. So Run must now poll unconditionally, and a due backup with an
// unresolvable DSN must fail per-attempt -- recorded in backup_history --
// rather than the whole loop going quiet.
//
// This is the known-positive for that: p.deploymentSecrets is nil, so
// backupDSN can only error, and Run's own "run once immediately on startup"
// call is what proves the loop was entered at all -- no need to wait for the
// 60-second ticker.
func TestSB_Run_PollsEvenWithNoDeploymentSecretsWired(t *testing.T) {
	p, fdb, rawDB := newSBPlugin(t)
	defer rawDB.Close()
	p.deploymentSecrets = nil

	cfgID := "00000000-0000-0000-0000-0000000000fe"
	past := time.Now().Add(-time.Hour)
	fdb.mu.Lock()
	fdb.configs[cfgID] = &sbConfigRow{
		id: cfgID, name: "no-secrets-test", cron: "0 9 * * *",
		s3Bucket: "b", s3Prefix: "p/", retentionDays: 30, enabled: true,
		nextRunAt: &past, createdAt: past, updatedAt: past,
	}
	fdb.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()

	// Run's own goroutine, not this test's, is what calls runDueBackups -- so
	// wait for the history row to actually land rather than racing
	// waitForBgBackups against Run's startup, which could observe
	// p.bgBackups at its zero value before Run has claimed anything.
	deadline := time.Now().Add(5 * time.Second)
	for {
		fdb.mu.RLock()
		n := len(fdb.history)
		fdb.mu.RUnlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for Run's immediate on-startup pass to record a history entry")
		}
		time.Sleep(5 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run with no deployment secrets wired: want nil, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not stop after cancel")
	}

	fdb.mu.RLock()
	defer fdb.mu.RUnlock()
	if len(fdb.history) == 0 {
		t.Fatal("expected Run's immediate on-startup pass to attempt the due backup and record a history entry")
	}
	for _, h := range fdb.history {
		if h.status != "failed" {
			t.Errorf("expected the due backup to fail cleanly with no deployment secret store, got status %q", h.status)
		}
		if h.errorMessage == nil || *h.errorMessage != backupErrDSNUnavailable {
			t.Errorf("expected the stable error code %q, got: %v",
				backupErrDSNUnavailable, h.errorMessage)
		}
	}
}

func TestSB_Run_Cancel(t *testing.T) {
	// A fake, deliberately slow pg_dump: relying on the real binary failing
	// fast against the bogus DSN "postgres://test" is exactly the timing
	// dependency that made TestSB_Run_Cancel flaky in CI in the first place
	// (cleat#2055) -- sometimes fast (masks the bug), sometimes slow (trips
	// it). A dump that reliably takes longer than this test's assertion
	// window makes the outcome depend on Run's behavior, not on DNS timing
	// or whether pg_dump is even installed.
	installFakePgDump(t, "sleep 1\nexit 1\n")

	p, fdb, rawDB := newSBPlugin(t)
	defer rawDB.Close()
	p.config.DumpDir = t.TempDir()

	cfgID := "00000000-0000-0000-0000-0000000000ee"
	past := time.Now().Add(-time.Hour)

	fdb.mu.Lock()
	fdb.configs[cfgID] = &sbConfigRow{
		id: cfgID, name: "run-cancel-test", cron: "0 9 * * *",
		s3Bucket: "b", s3Prefix: "p/", retentionDays: 30, enabled: true,
		nextRunAt: &past, createdAt: past, updatedAt: past,
	}
	fdb.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()

	// Wait for the claim to have happened -- not a fixed sleep, and not tied
	// to pg_dump's duration: executeScheduledBackup creates its
	// backup_history row (status "running") as the very first thing it does,
	// before touching pg_dump at all, so a history row appearing is a fast,
	// deterministic signal that Run's initial runDueBackups call claimed the
	// due config and dispatched the backup goroutine. Only then does
	// cancelling ctx actually exercise "Run returns promptly while a backup
	// is in flight" -- cancelling any earlier could abort the claim
	// transaction itself and the test would show nothing was ever dispatched.
	deadline := time.Now().Add(2 * time.Second)
	for {
		fdb.mu.RLock()
		n := len(fdb.history)
		fdb.mu.RUnlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the due backup to be claimed and dispatched")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run: want nil, got %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Run did not stop promptly after cancel")
	}

	// The dispatched backup keeps running after Run returns (cleat#2055):
	// wait for it deterministically before checking its effects.
	waitForBgBackups(t, p)

	fdb.mu.RLock()
	histCount := len(fdb.history)
	cfg := fdb.configs[cfgID]
	fdb.mu.RUnlock()

	if histCount == 0 {
		t.Error("expected at least 1 history entry after Run")
	}
	if cfg == nil {
		t.Fatal("config should exist after Run")
	}
	if cfg.lastRunAt == nil {
		t.Error("last_run_at should be set after Run (via updateNextRun)")
	}
}

// TestSB_ExecuteScheduledBackup_KilledMidway_LeavesNoFinalArtifactAndNoSuccess
// is the data-safety half of cleat#2055's fix: whatever interrupts pg_dump --
// worker shutdown, a crash, here a direct context cancellation -- must never
// leave a file at the dump's final name, and must never record the backup as
// 'completed'.
func TestSB_ExecuteScheduledBackup_KilledMidway_LeavesNoFinalArtifactAndNoSuccess(t *testing.T) {
	// Writes some bytes to its -f target immediately (so a bug that renames
	// unconditionally would be caught), then sleeps long enough to be
	// reliably still running when the test cancels it.
	//
	// "exec sleep 30" rather than "sleep 30": exec replaces the shell's own
	// process image, so the process exec.CommandContext kills IS the sleep,
	// with no separate child. A plain "sleep 30" forks a grandchild that
	// inherits the stderr pipe; killing the shell then leaves that orphaned
	// sleep holding the pipe's write end open, and cmd.Wait() -- which reads
	// stderr to a bytes.Buffer via a pipe -- blocks until that grandchild
	// exits on its own, defeating the kill entirely.
	startedMarker := installFakePgDump(t, "echo partial-data > \"$2\"\nexec sleep 30\n")

	p, fdb, rawDB := newSBPlugin(t)
	defer rawDB.Close()
	p.config.DumpDir = t.TempDir()

	cfgID := uuid.MustParse("00000000-0000-0000-0000-0000000000ee")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		p.executeScheduledBackup(ctx, cfgID, "kill-midway-test", "0 9 * * *")
		close(done)
	}()

	waitForFile(t, startedMarker, 3*time.Second)
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("executeScheduledBackup did not return after its context was cancelled")
	}

	entries, err := os.ReadDir(p.config.DumpDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".partial") {
			t.Errorf("found a final-named artifact after a killed backup: %s", e.Name())
		}
	}

	fdb.mu.RLock()
	defer fdb.mu.RUnlock()
	if len(fdb.history) == 0 {
		t.Fatal("expected a history entry to have been created")
	}
	for _, h := range fdb.history {
		if h.status == "completed" {
			t.Errorf("a killed backup must not be recorded as completed, got history entry %+v", h)
		}
	}
}

func TestSB_RunDueBackups(t *testing.T) {
	p, fdb, rawDB := newSBPlugin(t)
	defer rawDB.Close()
	p.config.DumpDir = t.TempDir()

	cfgID := "00000000-0000-0000-0000-0000000000ff"
	past := time.Now().Add(-time.Hour)

	fdb.mu.Lock()
	fdb.configs[cfgID] = &sbConfigRow{
		id: cfgID, name: "due-test", cron: "0 9 * * *",
		s3Bucket: "b", s3Prefix: "p/", retentionDays: 30, enabled: true,
		nextRunAt: &past, createdAt: past, updatedAt: past,
	}
	fdb.mu.Unlock()

	p.runDueBackups(context.Background())

	// runDueBackups only claims and dispatches (cleat#2055); the backup
	// itself runs on its own goroutine.
	waitForBgBackups(t, p)

	fdb.mu.RLock()
	histCount := len(fdb.history)
	cfg := fdb.configs[cfgID]
	fdb.mu.RUnlock()

	if histCount == 0 {
		t.Error("expected at least 1 history entry after runDueBackups")
	}
	if cfg == nil {
		t.Fatal("config should exist after runDueBackups")
	}
	if cfg.lastRunAt == nil {
		t.Error("last_run_at should be set after runDueBackups")
	}
}

func TestSB_ExecuteScheduledBackup_Error(t *testing.T) {
	p, fdb, rawDB := newSBPlugin(t)
	defer rawDB.Close()
	p.config.DumpDir = t.TempDir()

	cfgID := "00000000-0000-0000-0000-000000000111"
	configUUID := uuid.MustParse(cfgID)
	now := time.Now()

	fdb.mu.Lock()
	fdb.configs[cfgID] = &sbConfigRow{
		id: cfgID, name: "exec-test", cron: "0 9 * * *",
		s3Bucket: "b", s3Prefix: "p/", retentionDays: 30, enabled: true,
		createdAt: now, updatedAt: now,
	}
	fdb.mu.Unlock()

	// This will attempt pg_dump (not available), exercising the error path.
	p.executeScheduledBackup(context.Background(), configUUID, "exec-test", "0 9 * * *")

	fdb.mu.RLock()
	histCount := len(fdb.history)
	cfg := fdb.configs[cfgID]
	var gotStatus string
	var gotErr *string
	for _, h := range fdb.history {
		gotStatus = h.status
		gotErr = h.errorMessage
	}
	fdb.mu.RUnlock()

	if histCount == 0 {
		t.Error("expected at least 1 history entry after executeScheduledBackup")
	}
	if cfg == nil {
		t.Fatal("config should exist after executeScheduledBackup")
	}
	if cfg.lastRunAt == nil {
		t.Error("last_run_at should be set after executeScheduledBackup (via updateNextRun)")
	}
	if gotStatus != "failed" {
		t.Errorf("want status 'failed' (no real pg_dump on PATH), got %q", gotStatus)
	}
	if gotErr == nil || *gotErr != backupErrPgDumpFailed {
		t.Errorf("want error_message %q, got %v", backupErrPgDumpFailed, gotErr)
	}
}

func TestSB_UpdateNextRun(t *testing.T) {
	p, fdb, rawDB := newSBPlugin(t)
	defer rawDB.Close()

	cfgID := "00000000-0000-0000-0000-000000000222"
	configUUID := uuid.MustParse(cfgID)
	now := time.Now()

	fdb.mu.Lock()
	fdb.configs[cfgID] = &sbConfigRow{
		id: cfgID, name: "next-run-test", cron: "0 9 * * *",
		s3Bucket: "b", s3Prefix: "p/", retentionDays: 30, enabled: true,
		createdAt: now, updatedAt: now,
	}
	fdb.mu.Unlock()

	p.updateNextRun(context.Background(), configUUID, "0 9 * * *", now)

	fdb.mu.RLock()
	row := fdb.configs[cfgID]
	fdb.mu.RUnlock()

	if row == nil {
		t.Fatal("config should exist after updateNextRun")
	}
	if row.lastRunAt == nil {
		t.Error("last_run_at should be set after updateNextRun")
	}
	if row.nextRunAt == nil {
		t.Error("next_run_at should be set (cron has future match)")
	}
	if row.nextRunAt != nil && row.nextRunAt.Before(now) {
		t.Error("next_run_at should be in the future")
	}
}

func TestSB_UpdateNextRun_NoMatch(t *testing.T) {
	p, fdb, rawDB := newSBPlugin(t)
	defer rawDB.Close()

	cfgID := "00000000-0000-0000-0000-000000000223"
	configUUID := uuid.MustParse(cfgID)
	now := time.Now()

	fdb.mu.Lock()
	fdb.configs[cfgID] = &sbConfigRow{
		id: cfgID, name: "no-match-test", cron: "0 9 31 2 *",
		s3Bucket: "b", s3Prefix: "p/", retentionDays: 30, enabled: true,
		createdAt: now, updatedAt: now,
	}
	fdb.mu.Unlock()

	// Feb 31 won't match, so next_run_at should stay nil.
	p.updateNextRun(context.Background(), configUUID, "0 9 31 2 *", now)

	fdb.mu.RLock()
	row := fdb.configs[cfgID]
	fdb.mu.RUnlock()

	if row == nil {
		t.Fatal("config should exist after updateNextRun")
	}
	if row.lastRunAt == nil {
		t.Error("last_run_at should be set even with no cron match")
	}
	if row.nextRunAt != nil {
		t.Error("next_run_at should be nil when cron has no future match within window")
	}
}

// =========================================================================
// Migrations — Down SQL
// =========================================================================

func TestSB_Migrations_DownSQL(t *testing.T) {
	p := &Plugin{}
	migrations := p.Migrations()
	if len(migrations) == 0 {
		t.Fatal("expected migrations")
	}
	// One shared predicate for what a migration must do, rather than a copy
	// per plugin. Thirteen plugins carried their own and they had already
	// drifted -- three checked Up and not Down. A TenantScoped migration has
	// no SQL in either direction by design, so the old wording rejected it by
	// construction. cleat#1278.
	plugintest.AssertMigrationsDoSomething(t, migrations)
}

// =========================================================================
// updateNextRun ExecContext error path
// =========================================================================

func TestSB_UpdateNextRun_ExecError(t *testing.T) {
	p, fdb, rawDB := newSBPlugin(t)
	defer rawDB.Close()

	cfgID := uuid.MustParse("00000000-0000-0000-0000-000000000553")
	now := time.Now()

	fdb.mu.Lock()
	fdb.forceExecErr = 1
	fdb.mu.Unlock()

	// updateNextRun should log the error but not panic/return error.
	p.updateNextRun(context.Background(), cfgID, "0 9 * * *", now)
	// No panic = success.
}

// =========================================================================
// runDueBackups query error path
// =========================================================================

func TestSB_RunDueBackups_QueryError(t *testing.T) {
	p, fdb, rawDB := newSBPlugin(t)
	defer rawDB.Close()

	fdb.mu.Lock()
	fdb.forceQueryErr = 1
	fdb.mu.Unlock()

	// runDueBackups should log the error but not panic.
	p.runDueBackups(context.Background())
	// No panic = success.
}

// =========================================================================
// executeScheduledBackup INSERT error path
// =========================================================================

func TestSB_ExecuteScheduledBackup_InsertError(t *testing.T) {
	p, fdb, rawDB := newSBPlugin(t)
	defer rawDB.Close()
	p.config.DumpDir = t.TempDir()

	cfgID := "00000000-0000-0000-0000-000000000555"
	configUUID := uuid.MustParse(cfgID)
	now := time.Now()

	fdb.mu.Lock()
	fdb.configs[cfgID] = &sbConfigRow{
		id: cfgID, name: "insert-err", cron: "0 9 * * *",
		s3Bucket: "b", s3Prefix: "p/", retentionDays: 30, enabled: true,
		createdAt: now, updatedAt: now,
	}
	fdb.forceExecErr = 1
	fdb.mu.Unlock()

	p.executeScheduledBackup(context.Background(), configUUID, "insert-err", "0 9 * * *")

	fdb.mu.RLock()
	histCount := len(fdb.history)
	cfg := fdb.configs[cfgID]
	fdb.mu.RUnlock()
	if histCount != 0 {
		t.Error("expected 0 history entries after INSERT error")
	}
	if cfg == nil {
		t.Fatal("config should still exist")
	}
	if cfg.lastRunAt != nil {
		t.Error("last_run_at should NOT be set when INSERT fails")
	}
}

// =========================================================================
// Cron parseField edge cases
// =========================================================================

func TestParseCron_StepRange(t *testing.T) {
	cronStr := "*/15 * * * *"
	nxt := nextRun(cronStr, time.Date(2025, 6, 15, 10, 0, 0, 0, time.UTC))
	if nxt.IsZero() {
		t.Fatal("*/15 should produce a valid next run")
	}
	if nxt.Minute() != 15 {
		t.Errorf("expected next run at minute 15, got minute %d", nxt.Minute())
	}
}

func TestParseCron_ComplexStep(t *testing.T) {
	cronStr := "0 9 1-15 * 1-5"
	nxt := nextRun(cronStr, time.Date(2025, 6, 10, 10, 0, 0, 0, time.UTC))
	if nxt.IsZero() {
		t.Fatal("complex step cron should produce a valid next run")
	}
	if nxt.Hour() != 9 || nxt.Day() != 11 {
		t.Errorf("expected next run at 09:00 on day 11, got %v", nxt)
	}
}

func TestParseCron_ListField(t *testing.T) {
	cronStr := "0 9,15 * * *"
	nxt := nextRun(cronStr, time.Date(2025, 6, 15, 10, 0, 0, 0, time.UTC))
	if nxt.IsZero() {
		t.Fatal("list field cron should produce a valid next run")
	}
	if nxt.Hour() != 15 {
		t.Errorf("expected next run at hour 15, got hour %d", nxt.Hour())
	}
}

func TestParseCron_StepWithRange(t *testing.T) {
	cronStr := "0 9 1-15/2 * *"
	nxt := nextRun(cronStr, time.Date(2025, 6, 10, 10, 0, 0, 0, time.UTC))
	if nxt.IsZero() {
		t.Fatal("step-with-range cron should produce a valid next run")
	}
	if nxt.Day() != 11 {
		t.Errorf("expected day 11 (next odd day after 10), got day %d", nxt.Day())
	}
}

func TestParseCron_AllWeekdays(t *testing.T) {
	cronStr := "0 9 * * 1-5"
	nxt := nextRun(cronStr, time.Date(2025, 6, 14, 10, 0, 0, 0, time.UTC)) // Saturday
	if nxt.IsZero() {
		t.Fatal("weekday cron should produce a valid next run")
	}
	if nxt.Weekday() != time.Monday {
		t.Errorf("expected next run on Monday, got %v", nxt.Weekday())
	}
	if nxt.Day() != 16 {
		t.Errorf("expected next run on day 16 (Monday), got day %d", nxt.Day())
	}
}

func TestParseCron_EmptyField(t *testing.T) {
	nxt := nextRun("", time.Now())
	if !nxt.IsZero() {
		t.Error("empty cron string should return zero time")
	}
}
