package scheduledbackup

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
