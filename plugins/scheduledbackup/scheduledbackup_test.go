package scheduledbackup

import (
	"context"
	"database/sql"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/plugin"
)

func TestInfo(t *testing.T) {
	p := &Plugin{}
	info := p.Info()
	if info.Name != "scheduled-backup" {
		t.Errorf("expected name 'scheduled-backup', got %q", info.Name)
	}
	if info.Version != "0.1.0" {
		t.Errorf("expected version '0.1.0', got %q", info.Version)
	}
	if info.Description == "" {
		t.Errorf("expected non-empty description")
	}
}

func TestInit(t *testing.T) {
	p := &Plugin{}
	env := &plugin.Environment{
		DB:     &engine.SQLDBAdapter{DB: &sql.DB{}},
		Logger: slog.Default(),
	}
	if err := p.Init(context.Background(), env); err != nil {
		t.Fatalf("Init() returned error: %v", err)
	}
	if p.db == nil {
		t.Error("expected db to be set after Init")
	}
	if p.logger == nil {
		t.Error("expected logger to be set after Init")
	}
}

func TestInitWithNilLogger(t *testing.T) {
	p := &Plugin{}
	env := &plugin.Environment{
		DB: &engine.SQLDBAdapter{DB: &sql.DB{}},
	}
	if err := p.Init(context.Background(), env); err != nil {
		t.Fatalf("Init() returned error: %v", err)
	}
	if p.logger == nil {
		t.Error("expected logger to be set after Init with nil logger")
	}
}

func TestNextRun_Every5Min(t *testing.T) {
	// */5 * * * *  — every 5 minutes
	base := time.Date(2025, 1, 15, 10, 3, 0, 0, time.UTC)
	next := nextRun("*/5 * * * *", base)
	expected := time.Date(2025, 1, 15, 10, 5, 0, 0, time.UTC)
	if !next.Equal(expected) {
		t.Errorf("*/5: expected %v, got %v", expected, next)
	}
}

func TestNextRun_Hourly(t *testing.T) {
	// 0 * * * *  — at the top of each hour
	base := time.Date(2025, 1, 15, 10, 30, 0, 0, time.UTC)
	next := nextRun("0 * * * *", base)
	expected := time.Date(2025, 1, 15, 11, 0, 0, 0, time.UTC)
	if !next.Equal(expected) {
		t.Errorf("hourly: expected %v, got %v", expected, next)
	}
}

func TestNextRun_DailyAt9am(t *testing.T) {
	// 0 9 * * *  — daily at 9am
	base := time.Date(2025, 1, 15, 10, 0, 0, 0, time.UTC)
	next := nextRun("0 9 * * *", base)
	expected := time.Date(2025, 1, 16, 9, 0, 0, 0, time.UTC)
	if !next.Equal(expected) {
		t.Errorf("daily 9am: expected %v, got %v", expected, next)
	}
}

func TestNextRun_WeekdaysAt9am(t *testing.T) {
	// 0 9 * * 1-5  — weekdays at 9am
	// Jan 15, 2025 is a Wednesday
	base := time.Date(2025, 1, 15, 10, 0, 0, 0, time.UTC)
	next := nextRun("0 9 * * 1-5", base)
	expected := time.Date(2025, 1, 16, 9, 0, 0, 0, time.UTC) // Thursday
	if !next.Equal(expected) {
		t.Errorf("weekdays 9am: expected %v, got %v", expected, next)
	}
}

func TestNextRun_ExactMinute(t *testing.T) {
	// 30 10 * * *  — at 10:30 every day
	base := time.Date(2025, 1, 15, 10, 29, 0, 0, time.UTC)
	next := nextRun("30 10 * * *", base)
	expected := time.Date(2025, 1, 15, 10, 30, 0, 0, time.UTC)
	if !next.Equal(expected) {
		t.Errorf("exact minute: expected %v, got %v", expected, next)
	}
}

func TestNextRun_InvalidCron(t *testing.T) {
	// Invalid cron expression should return zero time.
	next := nextRun("invalid", time.Now())
	if !next.IsZero() {
		t.Errorf("expected zero time for invalid cron, got %v", next)
	}
}

func TestNextRun_CommaList(t *testing.T) {
	// 15,30,45 * * * *  — at minutes 15, 30, 45
	base := time.Date(2025, 1, 15, 10, 20, 0, 0, time.UTC)
	next := nextRun("15,30,45 * * * *", base)
	expected := time.Date(2025, 1, 15, 10, 30, 0, 0, time.UTC)
	if !next.Equal(expected) {
		t.Errorf("comma list: expected %v, got %v", expected, next)
	}
}

func TestNextRun_Range(t *testing.T) {
	// 0 9-17 * * *  — every hour from 9am to 5pm
	base := time.Date(2025, 1, 15, 8, 0, 0, 0, time.UTC)
	next := nextRun("0 9-17 * * *", base)
	expected := time.Date(2025, 1, 15, 9, 0, 0, 0, time.UTC)
	if !next.Equal(expected) {
		t.Errorf("range: expected %v, got %v", expected, next)
	}
}

func TestMigrations(t *testing.T) {
	p := &Plugin{}
	migs := p.Migrations()
	if len(migs) == 0 {
		t.Fatal("expected at least one migration")
	}
	if migs[0].Version != 1 {
		t.Errorf("expected version 1, got %d", migs[0].Version)
	}
	if migs[0].Up == "" {
		t.Error("expected non-empty Up SQL")
	}

	// cleat#2247: v4 is the migration that stops backup tables being
	// tenant-scoped -- see CLAUDE.md's "when a section names the files it
	// will change" warning about trusting an EARLIER migration number for a
	// table's current shape. Found by VERSION rather than by "the last
	// element", because v5 (below) is not v4's successor in the sense of
	// replacing it -- it patches a side effect of v4's own sibling, v3 --
	// and a highest-migration check would silently start describing the
	// wrong migration the next time one is appended.
	var v4, v5 *plugin.Migration
	for i := range migs {
		switch migs[i].Version {
		case 4:
			v4 = &migs[i]
		case 5:
			v5 = &migs[i]
		}
	}
	if v4 == nil {
		t.Fatal("no migration declares Version: 4 (cleat#2247's operator-only migration)")
	}
	for name, sql := range map[string]string{"Up": v4.Up, "UpMySQL": v4.UpMySQL, "UpMSSQL": v4.UpMSSQL} {
		if !strings.Contains(sql, "tenant_id") {
			t.Errorf("v4 %s does not mention tenant_id at all -- expected it to DROP the column", name)
		}
	}
	// Owner decision 1A, 2026-09-24: --uninstall-plugin scheduled-backup
	// must keep working, so v4 carries a MINIMAL Down (drops the two
	// indexes IT added) rather than Irreversible -- it does not restore
	// tenant_id, RLS or the old indexes, see the migration's own comment.
	if !strings.Contains(v4.Down, "idx_backup_config_enabled_next") || !strings.Contains(v4.Down, "idx_backup_history_config") {
		t.Error("v4 Down does not drop both indexes v4's Up created")
	}
	if !strings.Contains(v4.DownMSSQL, "idx_backup_config_enabled_next") || !strings.Contains(v4.DownMSSQL, "idx_backup_history_config") {
		t.Error("v4 DownMSSQL does not drop both indexes v4's Up created")
	}
	// DownMySQL drops only idx_backup_config_enabled_next: MySQL refuses to
	// drop idx_backup_history_config while backup_history_config_id_fkey
	// still needs it as its supporting index (Error 1553), measured by
	// TestUninstallSchedulerBackupOnEveryDialect/mysql -- see the migration's
	// own comment on DownMySQL.
	if !strings.Contains(v4.DownMySQL, "idx_backup_config_enabled_next") {
		t.Error("v4 DownMySQL does not drop idx_backup_config_enabled_next")
	}
	if strings.Contains(v4.DownMySQL, "idx_backup_history_config") {
		t.Error("v4 DownMySQL mentions idx_backup_history_config -- MySQL cannot drop it while the FK needs it as a supporting index")
	}
	for name, sql := range map[string]string{"Down": v4.Down, "DownMySQL": v4.DownMySQL, "DownMSSQL": v4.DownMSSQL} {
		if strings.Contains(sql, "tenant_id") {
			t.Errorf("v4 %s mentions tenant_id -- this Down is deliberately minimal and must not attempt to restore it (no source of truth for a value)", name)
		}
	}
	if v4.Irreversible != "" {
		t.Error("v4 declares an Irreversible reason alongside a real Down -- the field is now stale and should be removed")
	}

	// cleat#2247: v5 removes the ON DELETE CASCADE v3 put on
	// backup_history.config_id -- found by TestBackupCommandWorksOnEveryDialect
	// (cmd/cleatctl), which showed `backup config-delete` silently erasing a
	// config's entire backup_history despite printing that history rows are
	// unaffected. Unlike v4, this one IS reversible (SET NULL back to
	// CASCADE), so it carries a real Down on every dialect instead of
	// Irreversible.
	if v5 == nil {
		t.Fatal("no migration declares Version: 5 (cleat#2247's backup_history.config_id ON DELETE fix)")
	}
	for name, sql := range map[string]string{"Up": v5.Up, "UpMySQL": v5.UpMySQL, "UpMSSQL": v5.UpMSSQL} {
		if !strings.Contains(sql, "SET NULL") {
			t.Errorf("v5 %s does not mention SET NULL -- expected it to replace CASCADE with SET NULL", name)
		}
	}
	for name, sql := range map[string]string{"Down": v5.Down, "DownMySQL": v5.DownMySQL, "DownMSSQL": v5.DownMSSQL} {
		if !strings.Contains(sql, "CASCADE") {
			t.Errorf("v5 %s does not restore CASCADE -- expected it to reverse back to v3's shape", name)
		}
	}
	if v5.Irreversible != "" {
		t.Error("v5 declares Irreversible, but it has a real Down on every dialect -- restoring CASCADE is not a data-loss decision the way v4's tenant_id drop is")
	}
}

// TestRegisterRoutes_NilMux, TestRegisterCommands, TestCLIBackupRun_NoFlags
// and TestCLIBackupList_NoFlags were removed with routes.go and commands.go:
// cleat#2247 made backup configuration operator-only, so there is no longer
// a tenant-facing HTTP surface (RegisterRoutes) or a plugin.HasCommands
// implementation (RegisterCommands, cliBackupRun, cliBackupList) --
// operator access is now cmd/cleatctl/backup.go, which talks to
// backup_config/backup_history directly the same way slackworkspace.go and
// audit.go do for their own plugins, not through the Plugin interface.
// Note RegisterCommands was never actually wired to anything: no caller
// anywhere in this tree ever type-asserted a loaded plugin against
// plugin.HasCommands, so its two CLI commands were unreachable dead code
// even before this issue.

func TestPluginRegistration(t *testing.T) {
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

// ---------------------------------------------------------------------------
// parseField edge case tests (pure function)
// ---------------------------------------------------------------------------

func TestParseField_Star(t *testing.T) {
	cf, err := parseField("*", 0, 59)
	if err != nil {
		t.Fatalf("parseField(*): unexpected error: %v", err)
	}
	if !cf.any {
		t.Error("expected any=true for *")
	}
	if cf.step != 0 {
		t.Errorf("expected step=0, got %d", cf.step)
	}
}

func TestParseField_StepInvalid(t *testing.T) {
	tests := []struct {
		name  string
		field string
	}{
		{"non-numeric step", "*/abc"},
		{"zero step", "*/0"},
		{"negative step", "*/-1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseField(tc.field, 0, 59)
			if err == nil {
				t.Errorf("parseField(%q): expected error", tc.field)
			}
		})
	}
}

func TestParseField_ListInvalid(t *testing.T) {
	tests := []struct {
		name  string
		field string
	}{
		{"non-numeric in list", "1,abc,3"},
		{"out of range in list", "100,200"},
		{"empty list element", "1,,3"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseField(tc.field, 0, 59)
			if err == nil {
				t.Errorf("parseField(%q): expected error", tc.field)
			}
		})
	}
}

func TestParseField_RangeInvalid(t *testing.T) {
	tests := []struct {
		name  string
		field string
		min   int
		max   int
	}{
		{"invalid range start", "a-5", 0, 59},
		{"invalid range end", "1-a", 0, 59},
		{"range start out of bounds", "100-200", 0, 59},
		{"range end out of bounds", "1-200", 0, 59},
		{"range end before start", "30-10", 0, 59},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseField(tc.field, tc.min, tc.max)
			if err == nil {
				t.Errorf("parseField(%q): expected error", tc.field)
			}
		})
	}
}

func TestParseField_RangeWithStep(t *testing.T) {
	cf, err := parseField("1-10/3", 0, 59)
	if err != nil {
		t.Fatalf("parseField(1-10/3): unexpected error: %v", err)
	}
	if cf.any {
		t.Error("expected any=false for range-with-step")
	}
	expected := map[int]bool{1: true, 4: true, 7: true, 10: true}
	for k, v := range expected {
		if cf.values[k] != v {
			t.Errorf("expected values[%d] = %v, got %v", k, v, cf.values[k])
		}
	}
	for v := 0; v <= 11; v++ {
		if !expected[v] && cf.values[v] {
			t.Errorf("unexpected value %d in range-with-step", v)
		}
	}
}

func TestParseField_RangeWithStepInvalid(t *testing.T) {
	tests := []struct {
		name  string
		field string
	}{
		{"invalid range step non-numeric", "1-10/abc"},
		{"invalid range step zero", "1-10/0"},
		{"invalid range step negative", "1-10/-1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseField(tc.field, 0, 59)
			if err == nil {
				t.Errorf("parseField(%q): expected error", tc.field)
			}
		})
	}
}

func TestParseField_SingleOutOfRange(t *testing.T) {
	tests := []struct {
		name  string
		field string
		min   int
		max   int
	}{
		{"below min", "-1", 0, 59},
		{"above max", "100", 0, 59},
		{"non-numeric", "abc", 0, 59},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseField(tc.field, tc.min, tc.max)
			if err == nil {
				t.Errorf("parseField(%q): expected error", tc.field)
			}
		})
	}
}

func TestParseCron_InvalidFields(t *testing.T) {
	tests := []struct {
		name string
		expr string
	}{
		{"too few fields", "* * * *"},
		{"too many fields", "* * * * * *"},
		{"invalid minute", "abc * * * *"},
		{"invalid hour", "* abc * * *"},
		{"invalid day", "* * abc * *"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseCron(tc.expr)
			if err == nil {
				t.Errorf("parseCron(%q): expected error", tc.expr)
			}
		})
	}
}

func TestMatches_StepPattern(t *testing.T) {
	cf, err := parseField("*/15", 0, 59)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		val  int
		want bool
	}{
		{0, true},
		{15, true},
		{30, true},
		{45, true},
		{5, false},
		{10, false},
	}
	for _, tc := range tests {
		got := cf.matches(tc.val)
		if got != tc.want {
			t.Errorf("matches(*/15, %d) = %v, want %v", tc.val, got, tc.want)
		}
	}
}

func TestNextRun_StepRange(t *testing.T) {
	base := time.Date(2025, 6, 1, 10, 5, 0, 0, time.UTC)
	next := nextRun("0-59/15 * * * *", base)
	expected := time.Date(2025, 6, 1, 10, 15, 0, 0, time.UTC)
	if !next.Equal(expected) {
		t.Errorf("step range: expected %v, got %v", expected, next)
	}
}
