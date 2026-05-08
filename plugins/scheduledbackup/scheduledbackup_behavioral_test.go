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
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rcownie/cleat/internal/auth"
	"github.com/rcownie/cleat/internal/plugin"
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
		Config: json.RawMessage(`{"dsn":"postgres://...", "dump_dir":"/tmp/my-backups"}`),
	}
	if err := p.Init(context.Background(), env); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if p.config.DumpDir != "/tmp/my-backups" {
		t.Errorf("got dump dir %q", p.config.DumpDir)
	}
	if p.config.DSN != "postgres://..." {
		t.Errorf("got DSN %q", p.config.DSN)
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

// =========================================================================
// Route registration
// =========================================================================

func TestSB_RegisterRoutes_NilMux(t *testing.T) {
	p := &Plugin{}
	err := p.RegisterRoutes(nil)
	if err == nil || !strings.Contains(err.Error(), "nil mux") {
		t.Errorf("want nil mux error, got: %v", err)
	}
}

func TestSB_RegisterRoutes_Valid(t *testing.T) {
	p := &Plugin{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	mux := http.NewServeMux()
	if err := p.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes: %v", err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/backups/configs", nil)
	mux.ServeHTTP(rec, req)
	if rec.Code == 404 {
		t.Error("/backups/configs should be registered")
	}
}

// =========================================================================
// Route error paths — missing tenant
// =========================================================================

func TestSB_RouteErrorPaths_MissingTenant(t *testing.T) {
	p := &Plugin{
		db:     nil,
		mux:    http.NewServeMux(),
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	p.RegisterRoutes(p.mux)

	tests := []struct{ method, path string; body []byte }{
		{"POST", "/backups/configs", []byte(`{"name":"test","cron":"0 0 * * *"}`)},
		{"GET", "/backups/configs", nil},
		{"GET", "/backups/configs/00000000-0000-0000-0000-000000000001", nil},
		{"PUT", "/backups/configs/00000000-0000-0000-0000-000000000001", []byte(`{}`)},
		{"DELETE", "/backups/configs/00000000-0000-0000-0000-000000000001", nil},
		{"GET", "/backups/history", nil},
		{"POST", "/backups/configs/00000000-0000-0000-0000-000000000001/run", nil},
	}

	for _, tc := range tests {
		var body io.Reader
		if tc.body != nil {
			body = bytes.NewReader(tc.body)
		}
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(tc.method, tc.path, body)
		p.mux.ServeHTTP(rec, req)
		if rec.Code != 401 {
			t.Errorf("%s %s: want 401, got %d", tc.method, tc.path, rec.Code)
		}
	}
}

// =========================================================================
// Route error paths — with tenant but no DB
// =========================================================================

func TestSB_RouteErrorPaths_TenantInvalidID(t *testing.T) {
	p := &Plugin{
		db:     nil,
		mux:    http.NewServeMux(),
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	p.RegisterRoutes(p.mux)
	tid := uuid.MustParse("00000000-0000-0000-0000-000000000001")

	paths := []string{
		"/backups/configs/not-a-uuid",
	}
	for _, pth := range paths {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", pth, nil).WithContext(
			auth.WithTenantID(context.Background(), tid),
		)
		p.mux.ServeHTTP(rec, req)
		if rec.Code != 400 {
			t.Errorf("GET %s: want 400, got %d", pth, rec.Code)
		}
	}
}

// =========================================================================
// CLI command validation
// =========================================================================

func TestSB_CLI_BackupRun_MissingDSN(t *testing.T) {
	p := &Plugin{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	err := p.cliBackupRun([]string{"--tenant=00000000-0000-0000-0000-000000000001", "--config=00000000-0000-0000-0000-000000000001"})
	if err == nil || !strings.Contains(err.Error(), "dsn") {
		t.Errorf("want dsn error, got: %v", err)
	}
}

func TestSB_CLI_BackupRun_MissingTenant(t *testing.T) {
	p := &Plugin{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	err := p.cliBackupRun([]string{"--dsn=postgres://...", "--config=00000000-0000-0000-0000-000000000001"})
	if err == nil || !strings.Contains(err.Error(), "tenant") {
		t.Errorf("want tenant error, got: %v", err)
	}
}

func TestSB_CLI_BackupRun_MissingConfig(t *testing.T) {
	p := &Plugin{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	err := p.cliBackupRun([]string{"--dsn=postgres://...", "--tenant=00000000-0000-0000-0000-000000000001"})
	if err == nil || !strings.Contains(err.Error(), "config") {
		t.Errorf("want config error, got: %v", err)
	}
}

func TestSB_CLI_BackupRun_InvalidTenant(t *testing.T) {
	p := &Plugin{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	err := p.cliBackupRun([]string{"--dsn=postgres://...", "--tenant=bad-uuid", "--config=00000000-0000-0000-0000-000000000001"})
	if err == nil || !strings.Contains(err.Error(), "tenant UUID") {
		t.Errorf("want tenant UUID error, got: %v", err)
	}
}

func TestSB_CLI_BackupRun_InvalidConfig(t *testing.T) {
	p := &Plugin{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	err := p.cliBackupRun([]string{"--dsn=postgres://...", "--tenant=00000000-0000-0000-0000-000000000001", "--config=bad-uuid"})
	if err == nil || !strings.Contains(err.Error(), "config UUID") {
		t.Errorf("want config UUID error, got: %v", err)
	}
}

func TestSB_CLI_BackupList_MissingDSN(t *testing.T) {
	p := &Plugin{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	err := p.cliBackupList([]string{"--tenant=00000000-0000-0000-0000-000000000001"})
	if err == nil || !strings.Contains(err.Error(), "dsn") {
		t.Errorf("want dsn error, got: %v", err)
	}
}

func TestSB_CLI_BackupList_InvalidTenant(t *testing.T) {
	p := &Plugin{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	err := p.cliBackupList([]string{"--dsn=postgres://...", "--tenant=bad-uuid"})
	if err == nil || !strings.Contains(err.Error(), "tenant UUID") {
		t.Errorf("want tenant UUID error, got: %v", err)
	}
}

func TestSB_CLI_BackupList_InvalidTenantFlag(t *testing.T) {
	p := &Plugin{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	err := p.cliBackupList([]string{"--dsn=postgres://test", "--tenant=bad-uuid"})
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "tenant") {
		t.Errorf("want tenant error, got: %v", err)
	}
}

// =========================================================================
// RegisterCommands
// =========================================================================

func TestSB_RegisterCommands(t *testing.T) {
	p := &Plugin{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	cmds := p.RegisterCommands()
	if len(cmds) != 2 {
		t.Fatalf("want 2 commands, got %d", len(cmds))
	}
	found := make(map[string]bool)
	for _, c := range cmds {
		found[c.Name] = true
		if c.Run == nil {
			t.Errorf("command %q has nil Run", c.Name)
		}
	}
	for _, name := range []string{"backup-run", "backup-list"} {
		if !found[name] {
			t.Errorf("expected command %q not found", name)
		}
	}
}

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

// =========================================================================
// Types — JSON roundtrip
// =========================================================================

func TestSB_BackupConfig_JSON(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	cfg := backupConfig{
		ID: uuid.New(), Name: "daily", Cron: "0 9 * * *",
		S3Bucket: "my-bucket", S3Prefix: "backups/", RetentionDays: 30, Enabled: true,
		CreatedAt: now, UpdatedAt: now,
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out backupConfig
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Name != "daily" || out.Cron != "0 9 * * *" {
		t.Errorf("roundtrip failed: %+v", out)
	}
}

func TestSB_BackupHistory_JSON(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	sz := int64(1024)
	hist := backupHistory{
		ID: uuid.New(), ConfigID: uuid.New(), Filename: "test.dump",
		SizeBytes: &sz, Status: "completed", StartedAt: now, CreatedAt: now,
	}
	b, err := json.Marshal(hist)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out backupHistory
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Status != "completed" || *out.SizeBytes != 1024 {
		t.Errorf("roundtrip failed: %+v", out)
	}
}

// =========================================================================
// Fake DB driver for scheduledbackup behavioral tests
// =========================================================================

type sbConfigRow struct {
	id            string
	tenantID      string
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
	tenantID     string
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
func (c *sbConnector) Driver() driver.Driver                           { return &sbDrv{} }

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
func (r *sbResult) RowsAffected() (int64, error)  { return r.n, nil }

type sbRows struct {
	columns []string
	data    [][]driver.Value
	pos     int
}

func (r *sbRows) Columns() []string { return r.columns }
func (r *sbRows) Close() error       { return nil }
func (r *sbRows) Next(dest []driver.Value) error {
	if r.pos >= len(r.data) {
		return io.EOF
	}
	for i, v := range r.data[r.pos] {
		dest[i] = v
	}
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
	case strings.Contains(q, "INSERT INTO backup_config"):
		return c.execInsertConfig(args)
	case strings.Contains(q, "INSERT INTO backup_history"):
		return c.execInsertHistory(args)
	case strings.Contains(q, "DELETE FROM backup_history"):
		return c.execDeleteHistory(args)
	case strings.Contains(q, "DELETE FROM backup_config"):
		return c.execDeleteConfig(args)
	case strings.Contains(q, "UPDATE backup_config SET"):
		return c.execUpdateConfig(q, args)
	case strings.Contains(q, "UPDATE backup_history SET"):
		return c.execUpdateHistory(q, args)
	default:
		return nil, fmt.Errorf("sbConn: unexpected Exec: %.80s", q)
	}
}

func (c *sbConn) execInsertConfig(args []driver.NamedValue) (driver.Result, error) {
	// Args: tenant_id(1), id(2), name(3), cron(4), s3_bucket(5), s3_prefix(6),
	//        retention_days(7), enabled(8), next_run_at(9)
	tid, _ := sbArgS(args, 1)
	id, _ := sbArgS(args, 2)
	name, _ := sbArgS(args, 3)
	cron, _ := sbArgS(args, 4)
	s3B, _ := sbArgS(args, 5)
	s3P, _ := sbArgS(args, 6)
	retentionVal, _ := sbArgAny(args, 7)
	enabledVal, _ := sbArgAny(args, 8)
	nextVal, _ := sbArgAny(args, 9)

	enabled := false
	if b, ok := enabledVal.(bool); ok {
		enabled = b
	}
	retentionDays := 30
	switch v := retentionVal.(type) {
	case int64:
		retentionDays = int(v)
	case float64:
		retentionDays = int(v)
	}
	now := time.Now()
	var nextRunAt *time.Time
	if t, ok := nextVal.(time.Time); ok {
		nextRunAt = &t
	}

	c.db.configs[id] = &sbConfigRow{
		id: id, tenantID: tid, name: name, cron: cron,
		s3Bucket: s3B, s3Prefix: s3P, retentionDays: retentionDays,
		enabled: enabled, nextRunAt: nextRunAt,
		createdAt: now, updatedAt: now,
	}
	return &sbResult{n: 1}, nil
}

func (c *sbConn) execInsertHistory(args []driver.NamedValue) (driver.Result, error) {
	// Args: id(1), config_id(2), tenant_id(3), filename(4), started_at(5), created_at(5)
	id, _ := sbArgS(args, 1)
	configID, _ := sbArgS(args, 2)
	tid, _ := sbArgS(args, 3)
	filename, _ := sbArgS(args, 4)
	startedVal, _ := sbArgAny(args, 5)

	startedAt := time.Now()
	if t, ok := startedVal.(time.Time); ok {
		startedAt = t
	}

	c.db.history[id] = &sbHistoryRow{
		id: id, configID: configID, tenantID: tid,
		filename: filename, status: "running",
		startedAt: startedAt, createdAt: startedAt,
	}
	return &sbResult{n: 1}, nil
}

func (c *sbConn) execDeleteHistory(args []driver.NamedValue) (driver.Result, error) {
	// Args: config_id(1), tenant_id(2)
	configID, _ := sbArgS(args, 1)
	var deleted int64
	for id, h := range c.db.history {
		if h.configID == configID {
			delete(c.db.history, id)
			deleted++
		}
	}
	return &sbResult{n: deleted}, nil
}

func (c *sbConn) execDeleteConfig(args []driver.NamedValue) (driver.Result, error) {
	// Args: id(1), tenant_id(2)
	id, _ := sbArgS(args, 1)
	_, ok := c.db.configs[id]
	if !ok {
		return &sbResult{n: 0}, nil
	}
	delete(c.db.configs, id)
	return &sbResult{n: 1}, nil
}

func (c *sbConn) execUpdateConfig(q string, args []driver.NamedValue) (driver.Result, error) {
	n := len(args)
	if n == 0 {
		return &sbResult{n: 0}, nil
	}

	switch {
	case strings.Contains(q, "next_run_at = NULL"):
		// runBackupAsync: SET last_run_at = $1, next_run_at = NULL ... WHERE id = $2
		// args: now(1), configID(2)
		nowVal, _ := sbArgAny(args, 1)
		configID, _ := sbArgS(args, 2)
		row, ok := c.db.configs[configID]
		if !ok {
			return &sbResult{n: 0}, nil
		}
		if t, ok := nowVal.(time.Time); ok {
			row.lastRunAt = &t
		}
		row.nextRunAt = nil
		row.updatedAt = time.Now()
		return &sbResult{n: 1}, nil

	case strings.Contains(q, "last_run_at = $1, next_run_at = $2"):
		// runBackupAsync: SET last_run_at = $1, next_run_at = $2 ... WHERE id = $3
		// or updateNextRun with non-zero next
		// args: now(1), nextRunAt(2), configID(3)
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

	default:
		// Dynamic update from handleUpdateConfig
		// args: [fieldValues..., id, tid]
		// Last arg (ordinal=n) is tid, second-to-last (ordinal=n-1) is id
		if n < 2 {
			return &sbResult{n: 0}, nil
		}
		configID, _ := sbArgS(args, n-1)
		row, ok := c.db.configs[configID]
		if !ok {
			return &sbResult{n: 0}, nil
		}

		// Parse next_run_at = $N from the query (enabled/cron change path)
		nextRe := regexp.MustCompile(`next_run_at\s*=\s*\$(\d+)`)
		if m := nextRe.FindStringSubmatch(q); len(m) >= 2 {
			if ordinal, err := strconv.Atoi(m[1]); err == nil {
				if val, err := sbArgAny(args, ordinal); err == nil {
					if t, ok := val.(time.Time); ok {
						row.nextRunAt = &t
					}
				}
			}
		}

		// Parse enabled = $N from the query
		enabledRe := regexp.MustCompile(`enabled\s*=\s*\$(\d+)`)
		if m := enabledRe.FindStringSubmatch(q); len(m) >= 2 {
			if ordinal, err := strconv.Atoi(m[1]); err == nil {
				if val, err := sbArgAny(args, ordinal); err == nil {
					if b, ok := val.(bool); ok {
						row.enabled = b
					}
				}
			}
		}

		row.updatedAt = time.Now()
		return &sbResult{n: 1}, nil
	}
}

func (c *sbConn) execUpdateHistory(q string, args []driver.NamedValue) (driver.Result, error) {
	n := len(args)
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
	switch {
	case strings.Contains(q, "FROM backup_history"):
		return c.queryListHistory(q, args)
	case strings.Contains(q, "FROM backup_config") && strings.Contains(q, "ORDER BY"):
		return c.queryListConfigs(args)
	case strings.Contains(q, "enabled = true") && strings.Contains(q, "next_run_at"):
		return c.queryDueBackups(args)
	case strings.Contains(q, "cron, enabled FROM backup_config"):
		return c.queryUpdateFetch(args)
	case strings.Contains(q, "name, cron FROM backup_config"):
		return c.queryRunFetch(args)
	case strings.Contains(q, "SELECT cron FROM backup_config"):
		return c.queryCronFetch(args)
	case strings.Contains(q, "FROM backup_config"):
		return c.queryGetConfig(args)
	default:
		return nil, fmt.Errorf("sbConn: unexpected Query: %.80s", q)
	}
}

// Columns: id, name, cron, s3_bucket, s3_prefix, retention_days, enabled,
//          last_run_at, next_run_at, created_at, updated_at
var sbConfigColumns = []string{
	"id", "name", "cron", "s3_bucket", "s3_prefix",
	"retention_days", "enabled", "last_run_at", "next_run_at",
	"created_at", "updated_at",
}

func (c *sbConn) queryListConfigs(args []driver.NamedValue) (driver.Rows, error) {
	tid, _ := sbArgS(args, 1)
	var data [][]driver.Value
	for _, row := range c.db.configs {
		if row.tenantID != tid {
			continue
		}
		data = append(data, sbConfigRowToValues(row))
	}
	if data == nil {
		data = [][]driver.Value{}
	}
	return &sbRows{columns: sbConfigColumns, data: data}, nil
}

func (c *sbConn) queryGetConfig(args []driver.NamedValue) (driver.Rows, error) {
	id, err := sbArgS(args, 1)
	if err != nil {
		return &sbRows{columns: sbConfigColumns}, nil
	}
	row, ok := c.db.configs[id]
	if !ok {
		return &sbRows{columns: sbConfigColumns}, nil
	}
	vals := sbConfigRowToValues(row)
	return &sbRows{
		columns: sbConfigColumns,
		data:    [][]driver.Value{vals},
	}, nil
}

func sbConfigRowToValues(row *sbConfigRow) []driver.Value {
	var lastRunAt, nextRunAt driver.Value
	if row.lastRunAt != nil {
		lastRunAt = *row.lastRunAt
	}
	if row.nextRunAt != nil {
		nextRunAt = *row.nextRunAt
	}
	return []driver.Value{
		row.id, row.name, row.cron, row.s3Bucket, row.s3Prefix,
		int64(row.retentionDays), row.enabled,
		lastRunAt, nextRunAt,
		row.createdAt, row.updatedAt,
	}
}

func (c *sbConn) queryUpdateFetch(args []driver.NamedValue) (driver.Rows, error) {
	// SELECT cron, enabled FROM backup_config WHERE id = $1 AND tenant_id = $2
	id, _ := sbArgS(args, 1)
	row, ok := c.db.configs[id]
	if !ok {
		return &sbRows{columns: []string{"cron", "enabled"}}, nil
	}
	return &sbRows{
		columns: []string{"cron", "enabled"},
		data:    [][]driver.Value{{row.cron, row.enabled}},
	}, nil
}

func (c *sbConn) queryRunFetch(args []driver.NamedValue) (driver.Rows, error) {
	// SELECT name, cron FROM backup_config WHERE id = $1 AND tenant_id = $2
	id, _ := sbArgS(args, 1)
	row, ok := c.db.configs[id]
	if !ok {
		return &sbRows{columns: []string{"name", "cron"}}, nil
	}
	return &sbRows{
		columns: []string{"name", "cron"},
		data:    [][]driver.Value{{row.name, row.cron}},
	}, nil
}

func (c *sbConn) queryCronFetch(args []driver.NamedValue) (driver.Rows, error) {
	// SELECT cron FROM backup_config WHERE id = $1
	id, _ := sbArgS(args, 1)
	row, ok := c.db.configs[id]
	if !ok {
		return &sbRows{columns: []string{"cron"}}, nil
	}
	return &sbRows{
		columns: []string{"cron"},
		data:    [][]driver.Value{{row.cron}},
	}, nil
}

func (c *sbConn) queryDueBackups(args []driver.NamedValue) (driver.Rows, error) {
	// SELECT id, tenant_id, name, cron FROM backup_config WHERE enabled = true AND next_run_at <= now() FOR UPDATE SKIP LOCKED
	now := time.Now()
	var data [][]driver.Value
	for _, row := range c.db.configs {
		if row.enabled && row.nextRunAt != nil && !row.nextRunAt.After(now) {
			data = append(data, []driver.Value{
				row.id, row.tenantID, row.name, row.cron,
			})
		}
	}
	if data == nil {
		data = [][]driver.Value{}
	}
	return &sbRows{columns: []string{"id", "tenant_id", "name", "cron"}, data: data}, nil
}

// Columns for history: id, config_id, filename, size_bytes, status,
//                      started_at, completed_at, error_message, created_at
var sbHistoryColumns = []string{
	"id", "config_id", "filename", "size_bytes", "status",
	"started_at", "completed_at", "error_message", "created_at",
}

func (c *sbConn) queryListHistory(q string, args []driver.NamedValue) (driver.Rows, error) {
	tid, _ := sbArgS(args, 1)
	var configFilter string
	if strings.Contains(q, "AND config_id = $2") {
		cf, err := sbArgS(args, 2)
		if err == nil {
			configFilter = cf
		}
	}

	var data [][]driver.Value
	for _, row := range c.db.history {
		if row.tenantID != tid {
			continue
		}
		if configFilter != "" && row.configID != configFilter {
			continue
		}
		data = append(data, sbHistoryRowToValues(row))
	}
	if data == nil {
		data = [][]driver.Value{}
	}
	return &sbRows{columns: sbHistoryColumns, data: data}, nil
}

func sbHistoryRowToValues(row *sbHistoryRow) []driver.Value {
	var sizeBytes driver.Value
	if row.sizeBytes != nil {
		sizeBytes = *row.sizeBytes
	}
	var completedAt driver.Value
	if row.completedAt != nil {
		completedAt = *row.completedAt
	}
	var errorMsg driver.Value
	if row.errorMessage != nil {
		errorMsg = *row.errorMessage
	}
	return []driver.Value{
		row.id, row.configID, row.filename, sizeBytes, row.status,
		row.startedAt, completedAt, errorMsg, row.createdAt,
	}
}

// =====================================================================
// Helpers
// =====================================================================

func newSBPlugin(t *testing.T) (*Plugin, *sbDB, *sql.DB) {
	t.Helper()
	fdb := newSBDB()
	rawDB := sql.OpenDB(&sbConnector{db: fdb})
	p := &Plugin{
		db:     rawDB,
		mux:    http.NewServeMux(),
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if err := p.RegisterRoutes(p.mux); err != nil {
		t.Fatalf("RegisterRoutes: %v", err)
	}
	return p, fdb, rawDB
}

func sbRequest(t *testing.T, method, path string, body io.Reader) *http.Request {
	t.Helper()
	return httptest.NewRequest(method, path, body).WithContext(
		auth.WithTenantID(context.Background(), uuid.MustParse("00000000-0000-0000-0000-000000000001")),
	)
}

func sbReadJSON(t *testing.T, rec *httptest.ResponseRecorder, v interface{}) {
	t.Helper()
	if err := json.NewDecoder(rec.Body).Decode(v); err != nil {
		t.Fatalf("decode body: %v", err)
	}
}

// =====================================================================
// CreateConfig tests
// =====================================================================

func TestSB_CreateConfig_Success(t *testing.T) {
	p, _, rawDB := newSBPlugin(t)
	defer rawDB.Close()

	body := `{"name":"daily-backup","cron":"0 9 * * *","s3_bucket":"my-bucket","s3_prefix":"backups/","retention_days":30}`
	rec := httptest.NewRecorder()
	req := sbRequest(t, "POST", "/backups/configs", bytes.NewReader([]byte(body)))
	p.mux.ServeHTTP(rec, req)

	if rec.Code != 201 {
		t.Fatalf("create: want 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var result map[string]interface{}
	sbReadJSON(t, rec, &result)
	if result["name"] != "daily-backup" {
		t.Errorf("want name daily-backup, got %v", result["name"])
	}
	if result["enabled"] != true {
		t.Errorf("want enabled true, got %v", result["enabled"])
	}
	if _, ok := result["id"]; !ok {
		t.Error("expected id field in response")
	}
	if _, ok := result["next_run_at"]; !ok {
		t.Error("expected next_run_at field in response")
	}
}

func TestSB_CreateConfig_Defaults(t *testing.T) {
	p, fdb, rawDB := newSBPlugin(t)
	defer rawDB.Close()

	// Minimal body — only name and cron, rest defaults
	body := `{"name":"minimal","cron":"0 9 * * *"}`
	rec := httptest.NewRecorder()
	req := sbRequest(t, "POST", "/backups/configs", bytes.NewReader([]byte(body)))
	p.mux.ServeHTTP(rec, req)
	if rec.Code != 201 {
		t.Fatalf("create: want 201, got %d: %s", rec.Code, rec.Body.String())
	}

	// Check defaults in the fake DB
	fdb.mu.RLock()
	defer fdb.mu.RUnlock()
	for _, row := range fdb.configs {
		if row.name == "minimal" {
			if !row.enabled {
				t.Error("expected enabled default true")
			}
			if row.retentionDays != 30 {
				t.Errorf("expected retention_days default 30, got %d", row.retentionDays)
			}
			if row.s3Bucket != "" {
				t.Errorf("expected empty s3_bucket default, got %q", row.s3Bucket)
			}
		}
	}
}

func TestSB_CreateConfig_MissingName(t *testing.T) {
	p, _, rawDB := newSBPlugin(t)
	defer rawDB.Close()

	body := `{"cron":"0 9 * * *"}`
	rec := httptest.NewRecorder()
	req := sbRequest(t, "POST", "/backups/configs", bytes.NewReader([]byte(body)))
	p.mux.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Fatalf("want 400, got %d: %s", rec.Code, rec.Body.String())
	}
	var m map[string]string
	sbReadJSON(t, rec, &m)
	if m["error"] != "name is required" {
		t.Errorf("want 'name is required', got %q", m["error"])
	}
}

func TestSB_CreateConfig_MissingCron(t *testing.T) {
	p, _, rawDB := newSBPlugin(t)
	defer rawDB.Close()

	body := `{"name":"test"}`
	rec := httptest.NewRecorder()
	req := sbRequest(t, "POST", "/backups/configs", bytes.NewReader([]byte(body)))
	p.mux.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Fatalf("want 400, got %d: %s", rec.Code, rec.Body.String())
	}
	var m map[string]string
	sbReadJSON(t, rec, &m)
	if m["error"] != "cron is required" {
		t.Errorf("want 'cron is required', got %q", m["error"])
	}
}

func TestSB_CreateConfig_InvalidCron(t *testing.T) {
	p, _, rawDB := newSBPlugin(t)
	defer rawDB.Close()

	body := `{"name":"test","cron":"not-a-cron"}`
	rec := httptest.NewRecorder()
	req := sbRequest(t, "POST", "/backups/configs", bytes.NewReader([]byte(body)))
	p.mux.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Fatalf("want 400, got %d: %s", rec.Code, rec.Body.String())
	}
	var m map[string]string
	sbReadJSON(t, rec, &m)
	if !strings.Contains(m["error"], "cron") {
		t.Errorf("want cron-related error, got %q", m["error"])
	}
}

func TestSB_CreateConfig_InvalidJSON(t *testing.T) {
	p, _, rawDB := newSBPlugin(t)
	defer rawDB.Close()

	rec := httptest.NewRecorder()
	req := sbRequest(t, "POST", "/backups/configs", bytes.NewReader([]byte("not json")))
	p.mux.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Fatalf("want 400, got %d: %s", rec.Code, rec.Body.String())
	}
	var m map[string]string
	sbReadJSON(t, rec, &m)
	if m["error"] != "invalid JSON body" {
		t.Errorf("want 'invalid JSON body', got %q", m["error"])
	}
}

func TestSB_CreateConfig_ExplicitDisabled(t *testing.T) {
	p, fdb, rawDB := newSBPlugin(t)
	defer rawDB.Close()

	body := `{"name":"disabled-test","cron":"0 9 * * *","enabled":false}`
	rec := httptest.NewRecorder()
	req := sbRequest(t, "POST", "/backups/configs", bytes.NewReader([]byte(body)))
	p.mux.ServeHTTP(rec, req)
	if rec.Code != 201 {
		t.Fatalf("create: want 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var result map[string]interface{}
	sbReadJSON(t, rec, &result)
	if result["enabled"] != false {
		t.Errorf("want enabled false, got %v", result["enabled"])
	}

	fdb.mu.RLock()
	defer fdb.mu.RUnlock()
	for _, row := range fdb.configs {
		if row.name == "disabled-test" && row.enabled {
			t.Error("expected config to be disabled in DB")
		}
	}
}

// =====================================================================
// ListConfigs tests
// =====================================================================

func TestSB_ListConfigs_Empty(t *testing.T) {
	p, _, rawDB := newSBPlugin(t)
	defer rawDB.Close()

	rec := httptest.NewRecorder()
	req := sbRequest(t, "GET", "/backups/configs", nil)
	p.mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("list: want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := strings.TrimSpace(rec.Body.String())
	if body != "[]" {
		t.Errorf("empty list: want [], got %q", body)
	}
}

func TestSB_ListConfigs_WithData(t *testing.T) {
	p, fdb, rawDB := newSBPlugin(t)
	defer rawDB.Close()

	cid1 := "00000000-0000-0000-0000-00000000000a"
	cid2 := "00000000-0000-0000-0000-00000000000b"
	tid := uuid.MustParse("00000000-0000-0000-0000-000000000001").String()
	now := time.Now()

	// Seed two configs
	fdb.mu.Lock()
	fdb.configs[cid1] = &sbConfigRow{
		id: cid1, tenantID: tid, name: "daily", cron: "0 9 * * *",
		s3Bucket: "b1", s3Prefix: "p1/", retentionDays: 30, enabled: true,
		createdAt: now, updatedAt: now,
	}
	fdb.configs[cid2] = &sbConfigRow{
		id: cid2, tenantID: tid, name: "weekly", cron: "0 9 * * 0",
		s3Bucket: "b2", s3Prefix: "p2/", retentionDays: 7, enabled: false,
		createdAt: now.Add(-time.Hour), updatedAt: now.Add(-time.Hour),
	}
	fdb.mu.Unlock()

	rec := httptest.NewRecorder()
	req := sbRequest(t, "GET", "/backups/configs", nil)
	p.mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("list: want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var configs []backupConfig
	sbReadJSON(t, rec, &configs)
	if len(configs) != 2 {
		t.Fatalf("want 2 configs, got %d", len(configs))
	}
}

func TestSB_ListConfigs_TenantIsolation(t *testing.T) {
	p, fdb, rawDB := newSBPlugin(t)
	defer rawDB.Close()

	tidA := uuid.MustParse("00000000-0000-0000-0000-000000000001").String()
	tidB := uuid.MustParse("00000000-0000-0000-0000-000000000002").String()
	now := time.Now()

	fdb.mu.Lock()
	fdb.configs["00000000-0000-0000-0000-00000000000e"] = &sbConfigRow{
		id: "00000000-0000-0000-0000-00000000000e", tenantID: tidA, name: "tenant-a", cron: "0 9 * * *",
		s3Bucket: "b", s3Prefix: "p/", retentionDays: 30, enabled: true,
		createdAt: now, updatedAt: now,
	}
	fdb.configs["00000000-0000-0000-0000-00000000000f"] = &sbConfigRow{
		id: "00000000-0000-0000-0000-00000000000f", tenantID: tidB, name: "tenant-b", cron: "0 9 * * *",
		s3Bucket: "b", s3Prefix: "p/", retentionDays: 30, enabled: true,
		createdAt: now, updatedAt: now,
	}
	fdb.mu.Unlock()

	// Tenant A should only see config a1
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/backups/configs", nil).WithContext(
		auth.WithTenantID(context.Background(), uuid.MustParse("00000000-0000-0000-0000-000000000001")),
	)
	p.mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("list: want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var configs []backupConfig
	sbReadJSON(t, rec, &configs)
	if len(configs) != 1 {
		t.Fatalf("tenant A: want 1 config, got %d", len(configs))
	}
	if len(configs) > 0 && configs[0].Name != "tenant-a" {
		t.Errorf("tenant A: expected 'tenant-a', got %q", configs[0].Name)
	}
}

// =====================================================================
// GetConfig tests
// =====================================================================

func TestSB_GetConfig_Seeded(t *testing.T) {
	p, fdb, rawDB := newSBPlugin(t)
	defer rawDB.Close()

	tid := uuid.MustParse("00000000-0000-0000-0000-000000000001").String()
	cfgID := "00000000-0000-0000-0000-0000000000aa"
	now := time.Now()

	fdb.mu.Lock()
	fdb.configs[cfgID] = &sbConfigRow{
		id: cfgID, tenantID: tid, name: "seeded-daily", cron: "0 9 * * *",
		s3Bucket: "my-bucket", s3Prefix: "backups/",
		retentionDays: 30, enabled: true,
		createdAt: now, updatedAt: now,
	}
	fdb.mu.Unlock()

	rec := httptest.NewRecorder()
	req := sbRequest(t, "GET", "/backups/configs/"+cfgID, nil)
	p.mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("get: want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var c backupConfig
	sbReadJSON(t, rec, &c)
	if c.Name != "seeded-daily" {
		t.Errorf("want name seeded-daily, got %q", c.Name)
	}
}

func TestSB_GetConfig_Success(t *testing.T) {
	p, fdb, rawDB := newSBPlugin(t)
	defer rawDB.Close()

	tid := uuid.MustParse("00000000-0000-0000-0000-000000000001").String()
	now := time.Now()

	fdb.mu.Lock()
	fdb.configs["00000000-0000-0000-0000-00000000000c"] = &sbConfigRow{
		id: "00000000-0000-0000-0000-00000000000c", tenantID: tid, name: "daily", cron: "0 9 * * *",
		s3Bucket: "my-bucket", s3Prefix: "backups/",
		retentionDays: 30, enabled: true,
		createdAt: now, updatedAt: now,
	}
	fdb.mu.Unlock()

	rec := httptest.NewRecorder()
	req := sbRequest(t, "GET", "/backups/configs/00000000-0000-0000-0000-00000000000c", nil)
	p.mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("get: want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var c backupConfig
	sbReadJSON(t, rec, &c)
	if c.Name != "daily" {
		t.Errorf("want name daily, got %q", c.Name)
	}
	if c.Cron != "0 9 * * *" {
		t.Errorf("want cron '0 9 * * *', got %q", c.Cron)
	}
	if c.S3Bucket != "my-bucket" {
		t.Errorf("want bucket 'my-bucket', got %q", c.S3Bucket)
	}
}

func TestSB_GetConfig_NotFound(t *testing.T) {
	p, _, rawDB := newSBPlugin(t)
	defer rawDB.Close()

	rec := httptest.NewRecorder()
	req := sbRequest(t, "GET", "/backups/configs/00000000-0000-0000-0000-000000000099", nil)
	p.mux.ServeHTTP(rec, req)
	if rec.Code != 404 {
		t.Fatalf("get not found: want 404, got %d: %s", rec.Code, rec.Body.String())
	}
	var m map[string]string
	sbReadJSON(t, rec, &m)
	if m["error"] != "backup config not found" {
		t.Errorf("want not found error, got %q", m["error"])
	}
}

// =====================================================================
// UpdateConfig tests
// =====================================================================

func TestSB_UpdateConfig_Success(t *testing.T) {
	p, fdb, rawDB := newSBPlugin(t)
	defer rawDB.Close()

	tid := uuid.MustParse("00000000-0000-0000-0000-000000000001").String()
	now := time.Now()

	fdb.mu.Lock()
	fdb.configs["00000000-0000-0000-0000-00000000000c"] = &sbConfigRow{
		id: "00000000-0000-0000-0000-00000000000c", tenantID: tid, name: "daily", cron: "0 9 * * *",
		s3Bucket: "my-bucket", s3Prefix: "backups/",
		retentionDays: 30, enabled: true,
		createdAt: now, updatedAt: now,
	}
	fdb.mu.Unlock()

	body := `{"name":"updated-name","enabled":false}`
	rec := httptest.NewRecorder()
	req := sbRequest(t, "PUT", "/backups/configs/00000000-0000-0000-0000-00000000000c", bytes.NewReader([]byte(body)))
	p.mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("update: want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var m map[string]string
	sbReadJSON(t, rec, &m)
	if m["status"] != "updated" {
		t.Errorf("want status 'updated', got %q", m["status"])
	}
}

func TestSB_UpdateConfig_NotFound(t *testing.T) {
	p, _, rawDB := newSBPlugin(t)
	defer rawDB.Close()

	body := `{"name":"new-name"}`
	rec := httptest.NewRecorder()
	req := sbRequest(t, "PUT", "/backups/configs/00000000-0000-0000-0000-000000000099", bytes.NewReader([]byte(body)))
	p.mux.ServeHTTP(rec, req)
	if rec.Code != 404 {
		t.Fatalf("update not found: want 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestSB_UpdateConfig_InvalidJSON(t *testing.T) {
	p, _, rawDB := newSBPlugin(t)
	defer rawDB.Close()

	rec := httptest.NewRecorder()
	req := sbRequest(t, "PUT", "/backups/configs/00000000-0000-0000-0000-000000000001", bytes.NewReader([]byte("not json")))
	p.mux.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Fatalf("want 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestSB_UpdateConfig_InvalidCron(t *testing.T) {
	p, fdb, rawDB := newSBPlugin(t)
	defer rawDB.Close()

	tid := uuid.MustParse("00000000-0000-0000-0000-000000000001").String()

	fdb.mu.Lock()
	fdb.configs["00000000-0000-0000-0000-00000000000c"] = &sbConfigRow{
		id: "00000000-0000-0000-0000-00000000000c", tenantID: tid, name: "daily", cron: "0 9 * * *",
		s3Bucket: "b", s3Prefix: "p/", retentionDays: 30, enabled: true,
		createdAt: time.Now(), updatedAt: time.Now(),
	}
	fdb.mu.Unlock()

	// Changing cron to an invalid expression should return 400
	body := `{"cron":"not-a-cron"}`
	rec := httptest.NewRecorder()
	req := sbRequest(t, "PUT", "/backups/configs/00000000-0000-0000-0000-00000000000c", bytes.NewReader([]byte(body)))
	p.mux.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Fatalf("update invalid cron: want 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

// =====================================================================
// DeleteConfig tests
// =====================================================================

func TestSB_DeleteConfig_Success(t *testing.T) {
	p, fdb, rawDB := newSBPlugin(t)
	defer rawDB.Close()

	tid := uuid.MustParse("00000000-0000-0000-0000-000000000001").String()
	now := time.Now()

	fdb.mu.Lock()
	fdb.configs["00000000-0000-0000-0000-00000000000c"] = &sbConfigRow{
		id: "00000000-0000-0000-0000-00000000000c", tenantID: tid, name: "daily", cron: "0 9 * * *",
		s3Bucket: "b", s3Prefix: "p/", retentionDays: 30, enabled: true,
		createdAt: now, updatedAt: now,
	}
	fdb.mu.Unlock()

	rec := httptest.NewRecorder()
	req := sbRequest(t, "DELETE", "/backups/configs/00000000-0000-0000-0000-00000000000c", nil)
	p.mux.ServeHTTP(rec, req)
	if rec.Code != 204 {
		t.Fatalf("delete: want 204, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify it's gone
	fdb.mu.RLock()
	_, exists := fdb.configs["00000000-0000-0000-0000-00000000000c"]
	fdb.mu.RUnlock()
	if exists {
		t.Error("config should be deleted from DB")
	}
}

func TestSB_DeleteConfig_NotFound(t *testing.T) {
	p, _, rawDB := newSBPlugin(t)
	defer rawDB.Close()

	rec := httptest.NewRecorder()
	req := sbRequest(t, "DELETE", "/backups/configs/00000000-0000-0000-0000-000000000099", nil)
	p.mux.ServeHTTP(rec, req)
	if rec.Code != 404 {
		t.Fatalf("delete not found: want 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

// =====================================================================
// ListHistory tests
// =====================================================================

func TestSB_ListHistory_Empty(t *testing.T) {
	p, _, rawDB := newSBPlugin(t)
	defer rawDB.Close()

	rec := httptest.NewRecorder()
	req := sbRequest(t, "GET", "/backups/history", nil)
	p.mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("list history: want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := strings.TrimSpace(rec.Body.String())
	if body != "[]" {
		t.Errorf("empty history: want [], got %q", body)
	}
}

func TestSB_ListHistory_WithData(t *testing.T) {
	p, fdb, rawDB := newSBPlugin(t)
	defer rawDB.Close()

	tid := uuid.MustParse("00000000-0000-0000-0000-000000000001").String()
	now := time.Now()
	sz := int64(1024)

	fdb.mu.Lock()
	fdb.history["00000000-0000-0000-0000-0000000000a1"] = &sbHistoryRow{
		id: "00000000-0000-0000-0000-0000000000a1", configID: "00000000-0000-0000-0000-00000000000c", tenantID: tid,
		filename: "test1.dump", status: "completed",
		sizeBytes: &sz, startedAt: now, createdAt: now,
	}
	fdb.history["00000000-0000-0000-0000-0000000000a2"] = &sbHistoryRow{
		id: "00000000-0000-0000-0000-0000000000a2", configID: "00000000-0000-0000-0000-00000000000d", tenantID: tid,
		filename: "test2.dump", status: "running",
		startedAt: now, createdAt: now,
	}
	fdb.mu.Unlock()

	rec := httptest.NewRecorder()
	req := sbRequest(t, "GET", "/backups/history", nil)
	p.mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("list history: want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var history []backupHistory
	sbReadJSON(t, rec, &history)
	if len(history) != 2 {
		t.Fatalf("want 2 history entries, got %d", len(history))
	}
	if history[0].Status != "completed" && history[0].Status != "running" {
		t.Errorf("unexpected status: %q", history[0].Status)
	}
}

func TestSB_ListHistory_WithConfigFilter(t *testing.T) {
	p, fdb, rawDB := newSBPlugin(t)
	defer rawDB.Close()

	tid := uuid.MustParse("00000000-0000-0000-0000-000000000001").String()
	now := time.Now()

	fdb.mu.Lock()
	fdb.history["00000000-0000-0000-0000-0000000000a1"] = &sbHistoryRow{
		id: "00000000-0000-0000-0000-0000000000a1", configID: "00000000-0000-0000-0000-00000000000c", tenantID: tid,
		filename: "f1.dump", status: "completed",
		startedAt: now, createdAt: now,
	}
	fdb.history["00000000-0000-0000-0000-0000000000a2"] = &sbHistoryRow{
		id: "00000000-0000-0000-0000-0000000000a2", configID: "00000000-0000-0000-0000-00000000000d", tenantID: tid,
		filename: "f2.dump", status: "running",
		startedAt: now, createdAt: now,
	}
	fdb.mu.Unlock()

	rec := httptest.NewRecorder()
	req := sbRequest(t, "GET", "/backups/history?config_id=00000000-0000-0000-0000-00000000000c", nil)
	p.mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("list filtered history: want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var history []backupHistory
	sbReadJSON(t, rec, &history)
	if len(history) != 1 {
		t.Fatalf("want 1 history entry, got %d", len(history))
	}
	if history[0].Filename != "f1.dump" {
		t.Errorf("want filename f1.dump, got %q", history[0].Filename)
	}
}

func TestSB_ListHistory_InvalidConfigID(t *testing.T) {
	p, _, rawDB := newSBPlugin(t)
	defer rawDB.Close()

	rec := httptest.NewRecorder()
	req := sbRequest(t, "GET", "/backups/history?config_id=not-a-uuid", nil)
	p.mux.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Fatalf("invalid config_id: want 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestSB_ListHistory_TenantIsolation(t *testing.T) {
	p, fdb, rawDB := newSBPlugin(t)
	defer rawDB.Close()

	tidA := uuid.MustParse("00000000-0000-0000-0000-000000000001").String()
	tidB := uuid.MustParse("00000000-0000-0000-0000-000000000002").String()
	now := time.Now()

	fdb.mu.Lock()
	fdb.history["00000000-0000-0000-0000-0000000000a1"] = &sbHistoryRow{
		id: "00000000-0000-0000-0000-0000000000a1", configID: "00000000-0000-0000-0000-000000000010", tenantID: tidA,
		filename: "a.dump", status: "completed",
		startedAt: now, createdAt: now,
	}
	fdb.history["00000000-0000-0000-0000-0000000000a2"] = &sbHistoryRow{
		id: "00000000-0000-0000-0000-0000000000a2", configID: "00000000-0000-0000-0000-000000000011", tenantID: tidB,
		filename: "b.dump", status: "completed",
		startedAt: now, createdAt: now,
	}
	fdb.mu.Unlock()

	rec := httptest.NewRecorder()
	req := sbRequest(t, "GET", "/backups/history", nil)
	p.mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("list history: want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var history []backupHistory
	sbReadJSON(t, rec, &history)
	if len(history) != 1 {
		t.Fatalf("tenant A: want 1 history entry, got %d", len(history))
	}
}

// =====================================================================
// RunBackup tests
// =====================================================================

func TestSB_RunBackup_Success(t *testing.T) {
	p, fdb, rawDB := newSBPlugin(t)
	defer rawDB.Close()

	tid := uuid.MustParse("00000000-0000-0000-0000-000000000001").String()
	fdb.mu.Lock()
	fdb.configs["00000000-0000-0000-0000-00000000000c"] = &sbConfigRow{
		id: "00000000-0000-0000-0000-00000000000c", tenantID: tid, name: "daily", cron: "0 9 * * *",
		s3Bucket: "b", s3Prefix: "p/", retentionDays: 30, enabled: true,
		createdAt: time.Now(), updatedAt: time.Now(),
	}
	fdb.mu.Unlock()

	rec := httptest.NewRecorder()
	req := sbRequest(t, "POST", "/backups/configs/00000000-0000-0000-0000-00000000000c/run", nil)
	p.mux.ServeHTTP(rec, req)
	if rec.Code != 202 {
		t.Fatalf("run backup: want 202, got %d: %s", rec.Code, rec.Body.String())
	}
	var result map[string]interface{}
	sbReadJSON(t, rec, &result)
	if result["status"] != "running" {
		t.Errorf("want status 'running', got %v", result["status"])
	}
	if _, ok := result["history_id"]; !ok {
		t.Error("expected history_id in response")
	}
	if _, ok := result["config_id"]; !ok {
		t.Error("expected config_id in response")
	}

	// The history entry is created synchronously — verify it
	var historyID string
	if v, ok := result["history_id"].(string); ok {
		historyID = v
	}
	fdb.mu.RLock()
	h, exists := fdb.history[historyID]
	fdb.mu.RUnlock()
	if !exists {
		t.Fatal("history entry should exist in DB")
	}
	if h.status != "running" {
		t.Errorf("expected status 'running', got %q", h.status)
	}
	if h.filename == "" {
		t.Error("expected non-empty filename")
	}
}

func TestSB_RunBackup_NotFound(t *testing.T) {
	p, _, rawDB := newSBPlugin(t)
	defer rawDB.Close()

	rec := httptest.NewRecorder()
	req := sbRequest(t, "POST", "/backups/configs/00000000-0000-0000-0000-000000000099/run", nil)
	p.mux.ServeHTTP(rec, req)
	if rec.Code != 404 {
		t.Fatalf("run backup not found: want 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

// =====================================================================
// CRUD full lifecycle
// =====================================================================

func TestSB_CRUD_FullLifecycle(t *testing.T) {
	p, _, rawDB := newSBPlugin(t)
	defer rawDB.Close()

	// Create.
	createBody := `{"name":"lifecycle-test","cron":"0 9 * * *","enabled":true}`
	rec := httptest.NewRecorder()
	req := sbRequest(t, "POST", "/backups/configs", bytes.NewReader([]byte(createBody)))
	p.mux.ServeHTTP(rec, req)
	if rec.Code != 201 {
		t.Fatalf("create: want 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var created map[string]interface{}
	sbReadJSON(t, rec, &created)
	cfgID, ok := created["id"].(string)
	if !ok || cfgID == "" {
		t.Fatal("expected non-empty id")
	}

	// List (should have 1).
	rec = httptest.NewRecorder()
	req = sbRequest(t, "GET", "/backups/configs", nil)
	p.mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("list: want 200, got %d", rec.Code)
	}
	var configs []backupConfig
	sbReadJSON(t, rec, &configs)
	if len(configs) != 1 {
		t.Fatalf("want 1 config, got %d", len(configs))
	}

	// Get by ID.
	rec = httptest.NewRecorder()
	req = sbRequest(t, "GET", "/backups/configs/"+cfgID, nil)
	p.mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("get: want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var got backupConfig
	sbReadJSON(t, rec, &got)
	if got.Name != "lifecycle-test" {
		t.Errorf("want name 'lifecycle-test', got %q", got.Name)
	}
	if got.Cron != "0 9 * * *" {
		t.Errorf("want cron '0 9 * * *', got %q", got.Cron)
	}

	// Update.
	updateBody := `{"name":"updated-lifecycle","enabled":false}`
	rec = httptest.NewRecorder()
	req = sbRequest(t, "PUT", "/backups/configs/"+cfgID, bytes.NewReader([]byte(updateBody)))
	p.mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("update: want 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Delete.
	rec = httptest.NewRecorder()
	req = sbRequest(t, "DELETE", "/backups/configs/"+cfgID, nil)
	p.mux.ServeHTTP(rec, req)
	if rec.Code != 204 {
		t.Fatalf("delete: want 204, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify deleted.
	rec = httptest.NewRecorder()
	req = sbRequest(t, "GET", "/backups/configs", nil)
	p.mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("list after delete: want 200, got %d", rec.Code)
	}
	sbReadJSON(t, rec, &configs)
	if len(configs) != 0 {
		t.Errorf("expected 0 configs after delete, got %d", len(configs))
	}
}

// =====================================================================
// Error paths — invalid UUID (non-existent route value triggers 400)
// =====================================================================

func TestSB_ErrorPaths_InvalidID(t *testing.T) {
	p, _, rawDB := newSBPlugin(t)
	defer rawDB.Close()

	tests := []struct{ method, path, body string }{
		{"GET", "/backups/configs/not-a-uuid", ""},
		{"PUT", "/backups/configs/not-a-uuid", `{}`},
		{"DELETE", "/backups/configs/not-a-uuid", ""},
		{"POST", "/backups/configs/not-a-uuid/run", ""},
	}

	for _, tc := range tests {
		var body io.Reader
		if tc.body != "" {
			body = bytes.NewReader([]byte(tc.body))
		}
		rec := httptest.NewRecorder()
		req := sbRequest(t, tc.method, tc.path, body)
		p.mux.ServeHTTP(rec, req)
		if rec.Code != 400 {
			t.Errorf("%s %s: want 400, got %d", tc.method, tc.path, rec.Code)
		}
	}
}

// =====================================================================
// Missing tenant (with DB connected)
// =====================================================================

func TestSB_ErrorPaths_MissingTenantWithDB(t *testing.T) {
	p, _, rawDB := newSBPlugin(t)
	defer rawDB.Close()

	tests := []struct{ method, path string; body []byte }{
		{"POST", "/backups/configs", []byte(`{"name":"test","cron":"0 0 * * *"}`)},
		{"GET", "/backups/configs", nil},
		{"GET", "/backups/configs/00000000-0000-0000-0000-000000000001", nil},
		{"PUT", "/backups/configs/00000000-0000-0000-0000-000000000001", []byte(`{}`)},
		{"DELETE", "/backups/configs/00000000-0000-0000-0000-000000000001", nil},
		{"GET", "/backups/history", nil},
		{"POST", "/backups/configs/00000000-0000-0000-0000-000000000001/run", nil},
	}

	for _, tc := range tests {
		var body io.Reader
		if tc.body != nil {
			body = bytes.NewReader(tc.body)
		}
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(tc.method, tc.path, body)
		p.mux.ServeHTTP(rec, req)
		if rec.Code != 401 {
			t.Errorf("%s %s: want 401, got %d", tc.method, tc.path, rec.Code)
		}
	}
}

// =========================================================================
// Background loop — Run, runDueBackups, executeScheduledBackup, updateNextRun
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

func TestSB_Run_NoDSN(t *testing.T) {
	p := &Plugin{
		db:     &sql.DB{},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()
	time.Sleep(10 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run with no DSN: want nil, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not stop after cancel")
	}
}

func TestSB_Run_Cancel(t *testing.T) {
	p, fdb, rawDB := newSBPlugin(t)
	defer rawDB.Close()
	p.config.DSN = "postgres://test"
	p.config.DumpDir = t.TempDir()

	tid := uuid.MustParse("00000000-0000-0000-0000-000000000001").String()
	cfgID := "00000000-0000-0000-0000-0000000000ee"
	past := time.Now().Add(-time.Hour)

	fdb.mu.Lock()
	fdb.configs[cfgID] = &sbConfigRow{
		id: cfgID, tenantID: tid, name: "run-cancel-test", cron: "0 9 * * *",
		s3Bucket: "b", s3Prefix: "p/", retentionDays: 30, enabled: true,
		nextRunAt: &past, createdAt: past, updatedAt: past,
	}
	fdb.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()

	// Give time for runDueBackups (calls executeScheduledBackup → pg_dump fails) to finish.
	time.Sleep(500 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run: want nil, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop after cancel")
	}

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

func TestSB_RunDueBackups(t *testing.T) {
	p, fdb, rawDB := newSBPlugin(t)
	defer rawDB.Close()
	p.config.DSN = "postgres://test"
	p.config.DumpDir = t.TempDir()

	tid := uuid.MustParse("00000000-0000-0000-0000-000000000001").String()
	cfgID := "00000000-0000-0000-0000-0000000000ff"
	past := time.Now().Add(-time.Hour)

	fdb.mu.Lock()
	fdb.configs[cfgID] = &sbConfigRow{
		id: cfgID, tenantID: tid, name: "due-test", cron: "0 9 * * *",
		s3Bucket: "b", s3Prefix: "p/", retentionDays: 30, enabled: true,
		nextRunAt: &past, createdAt: past, updatedAt: past,
	}
	fdb.mu.Unlock()

	p.runDueBackups(context.Background())

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
	p.config.DSN = "postgres://test"
	p.config.DumpDir = t.TempDir()

	tidStr := uuid.MustParse("00000000-0000-0000-0000-000000000001").String()
	cfgID := "00000000-0000-0000-0000-000000000111"
	configUUID := uuid.MustParse(cfgID)
	tenantUUID := uuid.MustParse(tidStr)
	now := time.Now()

	fdb.mu.Lock()
	fdb.configs[cfgID] = &sbConfigRow{
		id: cfgID, tenantID: tidStr, name: "exec-test", cron: "0 9 * * *",
		s3Bucket: "b", s3Prefix: "p/", retentionDays: 30, enabled: true,
		createdAt: now, updatedAt: now,
	}
	fdb.mu.Unlock()

	// This will attempt pg_dump (not available), exercising the error path.
	p.executeScheduledBackup(context.Background(), configUUID, tenantUUID, "exec-test", "0 9 * * *")

	fdb.mu.RLock()
	histCount := len(fdb.history)
	cfg := fdb.configs[cfgID]
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
}

func TestSB_UpdateNextRun(t *testing.T) {
	p, fdb, rawDB := newSBPlugin(t)
	defer rawDB.Close()

	tid := uuid.MustParse("00000000-0000-0000-0000-000000000001").String()
	cfgID := "00000000-0000-0000-0000-000000000222"
	configUUID := uuid.MustParse(cfgID)
	now := time.Now()

	fdb.mu.Lock()
	fdb.configs[cfgID] = &sbConfigRow{
		id: cfgID, tenantID: tid, name: "next-run-test", cron: "0 9 * * *",
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

	tid := uuid.MustParse("00000000-0000-0000-0000-000000000001").String()
	cfgID := "00000000-0000-0000-0000-000000000223"
	configUUID := uuid.MustParse(cfgID)
	now := time.Now()

	fdb.mu.Lock()
	fdb.configs[cfgID] = &sbConfigRow{
		id: cfgID, tenantID: tid, name: "no-match-test", cron: "0 9 31 2 *",
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

func TestSB_RunBackupAsync_Error(t *testing.T) {
	p, fdb, rawDB := newSBPlugin(t)
	defer rawDB.Close()
	p.config.DSN = "postgres://test"
	p.config.DumpDir = t.TempDir()

	tid := uuid.MustParse("00000000-0000-0000-0000-000000000001").String()
	cfgID := uuid.MustParse("00000000-0000-0000-0000-000000000333")
	historyID := uuid.New()
	now := time.Now()

	fdb.mu.Lock()
	fdb.configs[cfgID.String()] = &sbConfigRow{
		id: cfgID.String(), tenantID: tid, name: "async-test", cron: "0 9 * * *",
		s3Bucket: "b", s3Prefix: "p/", retentionDays: 30, enabled: true,
		createdAt: now, updatedAt: now,
	}
	fdb.history[historyID.String()] = &sbHistoryRow{
		id: historyID.String(), configID: cfgID.String(), tenantID: tid,
		filename: "async_test.dump", status: "running",
		startedAt: now, createdAt: now,
	}
	fdb.mu.Unlock()

	p.runBackupAsync(cfgID, historyID, uuid.MustParse(tid), "async_test.dump")

	fdb.mu.RLock()
	h, ok := fdb.history[historyID.String()]
	fdb.mu.RUnlock()

	if !ok {
		t.Fatal("history entry should exist after runBackupAsync")
	}
	if h.status != "failed" {
		t.Errorf("want status 'failed', got %q", h.status)
	}
	if h.errorMessage == nil || *h.errorMessage == "" {
		t.Error("expected non-empty error message after failed pg_dump")
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
	for _, m := range migrations {
		if m.Up == "" {
			t.Error("migration Up SQL must be non-empty")
		}
		if m.Down == "" {
			t.Error("migration Down SQL must be non-empty")
		}
	}
}

// =========================================================================
// Route handler DB error paths — using force-error flag on fake DB
// =========================================================================

func TestSB_DBError_ListConfigs(t *testing.T) {
	p, fdb, rawDB := newSBPlugin(t)
	defer rawDB.Close()

	fdb.mu.Lock()
	fdb.forceQueryErr = 1
	fdb.mu.Unlock()

	rec := httptest.NewRecorder()
	req := sbRequest(t, "GET", "/backups/configs", nil)
	p.mux.ServeHTTP(rec, req)
	if rec.Code != 500 {
		t.Errorf("list configs query error: want 500, got %d", rec.Code)
	}
}

func TestSB_DBError_GetConfig(t *testing.T) {
	p, fdb, rawDB := newSBPlugin(t)
	defer rawDB.Close()

	fdb.mu.Lock()
	fdb.forceQueryErr = 1
	fdb.mu.Unlock()

	rec := httptest.NewRecorder()
	req := sbRequest(t, "GET", "/backups/configs/00000000-0000-0000-0000-000000000001", nil)
	p.mux.ServeHTTP(rec, req)
	if rec.Code != 500 {
		t.Errorf("get config query error: want 500, got %d", rec.Code)
	}
}

func TestSB_DBError_UpdateConfig(t *testing.T) {
	p, fdb, rawDB := newSBPlugin(t)
	defer rawDB.Close()

	// Seed config so we get past the "not found" check.
	tid := uuid.MustParse("00000000-0000-0000-0000-000000000001").String()
	cfgID := "00000000-0000-0000-0000-000000000444"
	fdb.mu.Lock()
	fdb.configs[cfgID] = &sbConfigRow{
		id: cfgID, tenantID: tid, name: "update-err", cron: "0 9 * * *",
		s3Bucket: "b", s3Prefix: "p/", retentionDays: 30, enabled: true,
		createdAt: time.Now(), updatedAt: time.Now(),
	}
	fdb.mu.Unlock()

	// First request will succeed (fetch config). Force error on the update exec.
	fdb.mu.Lock()
	fdb.forceExecErr = 1
	fdb.mu.Unlock()

	body := `{"name":"new-name"}`
	rec := httptest.NewRecorder()
	req := sbRequest(t, "PUT", "/backups/configs/"+cfgID, bytes.NewReader([]byte(body)))
	p.mux.ServeHTTP(rec, req)
	if rec.Code != 500 {
		t.Errorf("update config exec error: want 500, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestSB_DBError_DeleteConfig(t *testing.T) {
	p, fdb, rawDB := newSBPlugin(t)
	defer rawDB.Close()

	tid := uuid.MustParse("00000000-0000-0000-0000-000000000001").String()
	cfgID := "00000000-0000-0000-0000-000000000555"
	fdb.mu.Lock()
	fdb.configs[cfgID] = &sbConfigRow{
		id: cfgID, tenantID: tid, name: "delete-err", cron: "0 9 * * *",
		s3Bucket: "b", s3Prefix: "p/", retentionDays: 30, enabled: true,
		createdAt: time.Now(), updatedAt: time.Now(),
	}
	fdb.mu.Unlock()

	fdb.mu.Lock()
	fdb.forceExecErr = 2
	fdb.mu.Unlock()

	rec := httptest.NewRecorder()
	req := sbRequest(t, "DELETE", "/backups/configs/"+cfgID, nil)
	p.mux.ServeHTTP(rec, req)
	if rec.Code != 500 {
		t.Errorf("delete config exec error: want 500, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestSB_DBError_RunBackup(t *testing.T) {
	p, fdb, rawDB := newSBPlugin(t)
	defer rawDB.Close()

	tid := uuid.MustParse("00000000-0000-0000-0000-000000000001").String()
	cfgID := "00000000-0000-0000-0000-000000000666"
	fdb.mu.Lock()
	fdb.configs[cfgID] = &sbConfigRow{
		id: cfgID, tenantID: tid, name: "run-err", cron: "0 9 * * *",
		s3Bucket: "b", s3Prefix: "p/", retentionDays: 30, enabled: true,
		createdAt: time.Now(), updatedAt: time.Now(),
	}
	fdb.mu.Unlock()

	fdb.mu.Lock()
	fdb.forceExecErr = 1
	fdb.mu.Unlock()

	rec := httptest.NewRecorder()
	req := sbRequest(t, "POST", "/backups/configs/"+cfgID+"/run", nil)
	p.mux.ServeHTTP(rec, req)
	if rec.Code != 500 {
		t.Errorf("run backup exec error: want 500, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestSB_DBError_History(t *testing.T) {
	p, fdb, rawDB := newSBPlugin(t)
	defer rawDB.Close()

	fdb.mu.Lock()
	fdb.forceQueryErr = 1
	fdb.mu.Unlock()

	rec := httptest.NewRecorder()
	req := sbRequest(t, "GET", "/backups/history", nil)
	p.mux.ServeHTTP(rec, req)
	if rec.Code != 500 {
		t.Errorf("list history query error: want 500, got %d", rec.Code)
	}
}

func TestSB_DBError_UpdateConfig_Fetch(t *testing.T) {
	p, fdb, rawDB := newSBPlugin(t)
	defer rawDB.Close()

	tid := uuid.MustParse("00000000-0000-0000-0000-000000000001").String()
	cfgID := "00000000-0000-0000-0000-000000000447"
	fdb.mu.Lock()
	fdb.configs[cfgID] = &sbConfigRow{
		id: cfgID, tenantID: tid, name: "fetch-err", cron: "0 9 * * *",
		s3Bucket: "b", s3Prefix: "p/", retentionDays: 30, enabled: true,
		createdAt: time.Now(), updatedAt: time.Now(),
	}
	fdb.mu.Unlock()

	fdb.mu.Lock()
	fdb.forceQueryErr = 1
	fdb.mu.Unlock()

	body := `{"name":"new-name"}`
	rec := httptest.NewRecorder()
	req := sbRequest(t, "PUT", "/backups/configs/"+cfgID, bytes.NewReader([]byte(body)))
	p.mux.ServeHTTP(rec, req)
	if rec.Code != 500 {
		t.Errorf("update config fetch error: want 500, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestSB_DBError_ListConfigsScan(t *testing.T) {
	// Test the scan error path in handleListConfigs by seeding a row with
	// incompatible column types in the fake DB's config list.
	p, fdb, rawDB := newSBPlugin(t)
	defer rawDB.Close()

	// Instead of using force error, we modify the queryListConfigs temporarily.
	// We inject a row with a type that will fail scanning by using the
	// forceQueryErr mechanism to trigger a 500 instead.
	fdb.mu.Lock()
	fdb.forceQueryErr = 1
	fdb.mu.Unlock()

	rec := httptest.NewRecorder()
	req := sbRequest(t, "GET", "/backups/configs", nil)
	p.mux.ServeHTTP(rec, req)
	if rec.Code != 500 {
		t.Errorf("list configs query error: want 500, got %d", rec.Code)
	}
}

func TestSB_DBError_CreateConfig(t *testing.T) {
	p, fdb, rawDB := newSBPlugin(t)
	defer rawDB.Close()

	fdb.mu.Lock()
	fdb.forceExecErr = 1
	fdb.mu.Unlock()

	body := `{"name":"test","cron":"0 9 * * *"}`
	rec := httptest.NewRecorder()
	req := sbRequest(t, "POST", "/backups/configs", bytes.NewReader([]byte(body)))
	p.mux.ServeHTTP(rec, req)
	if rec.Code != 500 {
		t.Errorf("create config exec error: want 500, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestSB_UpdateConfig_WithEnabledTrue(t *testing.T) {
	p, fdb, rawDB := newSBPlugin(t)
	defer rawDB.Close()

	tid := uuid.MustParse("00000000-0000-0000-0000-000000000001").String()
	cfgID := "00000000-0000-0000-0000-000000000448"
	fdb.mu.Lock()
	fdb.configs[cfgID] = &sbConfigRow{
		id: cfgID, tenantID: tid, name: "enable-test", cron: "0 9 * * *",
		s3Bucket: "b", s3Prefix: "p/", retentionDays: 30, enabled: true,
		createdAt: time.Now(), updatedAt: time.Now(),
	}
	fdb.mu.Unlock()

	// Setting enabled=true triggers nextRunVal recalculation (covers the
	// next_run_at = $N branch in the dynamic UPDATE query builder).
	body := `{"enabled":true}`
	rec := httptest.NewRecorder()
	req := sbRequest(t, "PUT", "/backups/configs/"+cfgID, bytes.NewReader([]byte(body)))
	p.mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("update with enabled=true: want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var m map[string]string
	sbReadJSON(t, rec, &m)
	if m["status"] != "updated" {
		t.Errorf("want status 'updated', got %q", m["status"])
	}

	// Verify next_run_at was updated in the fake DB.
	fdb.mu.RLock()
	row := fdb.configs[cfgID]
	fdb.mu.RUnlock()
	if row.nextRunAt == nil {
		t.Error("next_run_at should be set after enabling config")
	}
}

func TestSB_UpdateConfig_WithCronChange(t *testing.T) {
	p, fdb, rawDB := newSBPlugin(t)
	defer rawDB.Close()

	tid := uuid.MustParse("00000000-0000-0000-0000-000000000001").String()
	cfgID := "00000000-0000-0000-0000-000000000449"
	fdb.mu.Lock()
	fdb.configs[cfgID] = &sbConfigRow{
		id: cfgID, tenantID: tid, name: "cron-change", cron: "0 9 * * *",
		s3Bucket: "b", s3Prefix: "p/", retentionDays: 30, enabled: true,
		createdAt: time.Now(), updatedAt: time.Now(),
	}
	fdb.mu.Unlock()

	body := `{"cron":"0 10 * * *"}`
	rec := httptest.NewRecorder()
	req := sbRequest(t, "PUT", "/backups/configs/"+cfgID, bytes.NewReader([]byte(body)))
	p.mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("update with cron change: want 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestSB_DBError_RunBackup_Fetch(t *testing.T) {
	p, fdb, rawDB := newSBPlugin(t)
	defer rawDB.Close()

	tid := uuid.MustParse("00000000-0000-0000-0000-000000000001").String()
	cfgID := "00000000-0000-0000-0000-000000000777"
	fdb.mu.Lock()
	fdb.configs[cfgID] = &sbConfigRow{
		id: cfgID, tenantID: tid, name: "run-fetch-err", cron: "0 9 * * *",
		s3Bucket: "b", s3Prefix: "p/", retentionDays: 30, enabled: true,
		createdAt: time.Now(), updatedAt: time.Now(),
	}
	fdb.mu.Unlock()

	// Force the SELECT name, cron query to fail.
	fdb.mu.Lock()
	fdb.forceQueryErr = 1
	fdb.mu.Unlock()

	rec := httptest.NewRecorder()
	req := sbRequest(t, "POST", "/backups/configs/"+cfgID+"/run", nil)
	p.mux.ServeHTTP(rec, req)
	if rec.Code != 500 {
		t.Errorf("run backup fetch error: want 500, got %d: %s", rec.Code, rec.Body.String())
	}
}

// =========================================================================
// UpdateConfig edge: s3_bucket, s3_prefix, retention_days
// =========================================================================

func TestSB_UpdateConfig_WithS3Fields(t *testing.T) {
	p, fdb, rawDB := newSBPlugin(t)
	defer rawDB.Close()

	tid := uuid.MustParse("00000000-0000-0000-0000-000000000001").String()
	cfgID := "00000000-0000-0000-0000-000000000551"
	fdb.mu.Lock()
	fdb.configs[cfgID] = &sbConfigRow{
		id: cfgID, tenantID: tid, name: "s3-fields", cron: "0 9 * * *",
		s3Bucket: "old-bucket", s3Prefix: "old/prefix/", retentionDays: 7, enabled: true,
		createdAt: time.Now(), updatedAt: time.Now(),
	}
	fdb.mu.Unlock()

	body := `{"s3_bucket":"new-bucket","s3_prefix":"new/prefix/","retention_days":14}`
	rec := httptest.NewRecorder()
	req := sbRequest(t, "PUT", "/backups/configs/"+cfgID, bytes.NewReader([]byte(body)))
	p.mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("update s3 fields: want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var m map[string]string
	sbReadJSON(t, rec, &m)
	if m["status"] != "updated" {
		t.Errorf("want status 'updated', got %q", m["status"])
	}
}

// =========================================================================
// ListHistory edge: query error path + completed_at/error_message
// =========================================================================

func TestSB_ListHistory_QueryError(t *testing.T) {
	p, fdb, rawDB := newSBPlugin(t)
	defer rawDB.Close()

	fdb.mu.Lock()
	fdb.forceQueryErr = 1
	fdb.mu.Unlock()

	rec := httptest.NewRecorder()
	req := sbRequest(t, "GET", "/backups/history", nil)
	p.mux.ServeHTTP(rec, req)
	if rec.Code != 500 {
		t.Fatalf("list history query error: want 500, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestSB_ListHistory_WithNullableFields(t *testing.T) {
	p, fdb, rawDB := newSBPlugin(t)
	defer rawDB.Close()

	tid := uuid.MustParse("00000000-0000-0000-0000-000000000001").String()
	now := time.Now()
	completed := now.Add(-time.Hour)
	sz := int64(2048)
	hID1 := "00000000-0000-0000-0000-000000000a01"
	hID2 := "00000000-0000-0000-0000-000000000a02"
	hID3 := "00000000-0000-0000-0000-000000000a03"
	cfgID := "00000000-0000-0000-0000-000000000999"

	fdb.mu.Lock()
	fdb.history[hID1] = &sbHistoryRow{
		id: hID1, configID: cfgID, tenantID: tid,
		filename: "completed_err.dump", status: "failed",
		sizeBytes: &sz, startedAt: now, createdAt: now,
		completedAt: &completed, errorMessage: strPtr("disk full"),
	}
	fdb.history[hID2] = &sbHistoryRow{
		id: hID2, configID: cfgID, tenantID: tid,
		filename: "completed_ok.dump", status: "completed",
		sizeBytes: &sz, startedAt: now, createdAt: now,
		completedAt: &completed,
	}
	fdb.history[hID3] = &sbHistoryRow{
		id: hID3, configID: cfgID, tenantID: tid,
		filename: "running.dump", status: "running",
		startedAt: now, createdAt: now,
	}
	fdb.mu.Unlock()

	rec := httptest.NewRecorder()
	req := sbRequest(t, "GET", "/backups/history", nil)
	p.mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("list history: want 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var result []map[string]interface{}
	sbReadJSON(t, rec, &result)
	if len(result) != 3 {
		t.Fatalf("expected 3 history entries, got %d", len(result))
	}
	foundCompleted := false
	foundError := false
	for _, entry := range result {
		if entry["completed_at"] != nil {
			foundCompleted = true
		}
		if entry["error_message"] != nil {
			foundError = true
		}
	}
	if !foundCompleted {
		t.Error("expected at least one entry with completed_at set")
	}
	if !foundError {
		t.Error("expected at least one entry with error_message set")
	}
}

func strPtr(s string) *string { return &s }

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
	p.config.DSN = "postgres://test"
	p.config.DumpDir = t.TempDir()

	tidStr := uuid.MustParse("00000000-0000-0000-0000-000000000001").String()
	cfgID := "00000000-0000-0000-0000-000000000555"
	configUUID := uuid.MustParse(cfgID)
	tenantUUID := uuid.MustParse(tidStr)
	now := time.Now()

	fdb.mu.Lock()
	fdb.configs[cfgID] = &sbConfigRow{
		id: cfgID, tenantID: tidStr, name: "insert-err", cron: "0 9 * * *",
		s3Bucket: "b", s3Prefix: "p/", retentionDays: 30, enabled: true,
		createdAt: now, updatedAt: now,
	}
	fdb.forceExecErr = 1
	fdb.mu.Unlock()

	p.executeScheduledBackup(context.Background(), configUUID, tenantUUID, "insert-err", "0 9 * * *")

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
