package main

import "testing"

// Unit coverage for the two pure helpers
// TestNoPluginMigrationWritesATenantScopedTableWithoutTheCrossTenantBypass
// relies on, against synthetic SQL rather than the live plugin tree -- so
// these can fail independently of whatever the tree currently contains, and
// keep discriminating after cleat#2828's actual fix makes the live-tree
// test above go green.

func TestDmlLinesAgainstTable_FindsEachStatement(t *testing.T) {
	sql := `
		ALTER TABLE event_awaiters ADD COLUMN IF NOT EXISTS id UUID;
		UPDATE event_awaiters SET id = gen_random_uuid() WHERE id IS NULL;
		UPDATE event_awaiters SET registration_key = '' WHERE registration_key IS NULL;
	`
	lines := dmlLinesAgainstTable(sql, "event_awaiters")
	if len(lines) != 2 {
		t.Fatalf("dmlLinesAgainstTable found %d lines, want 2: %v", len(lines), lines)
	}
}

func TestDmlLinesAgainstTable_IgnoresDDL(t *testing.T) {
	sql := `
		ALTER TABLE event_awaiters ADD COLUMN IF NOT EXISTS id UUID;
		CREATE INDEX IF NOT EXISTS idx_x ON event_awaiters(id);
		DROP POLICY IF EXISTS event_awaiters_tenant_isolation ON event_awaiters;
	`
	if lines := dmlLinesAgainstTable(sql, "event_awaiters"); len(lines) != 0 {
		t.Errorf("dmlLinesAgainstTable found %d DDL-only lines as DML: %v", len(lines), lines)
	}
}

// Negative control: a table whose name is a PREFIX of the scoped table (or
// vice versa) must not match -- exact identifier, not substring.
func TestDmlLinesAgainstTable_ExactIdentifierNotSubstring(t *testing.T) {
	sql := `UPDATE event_awaiters_backup SET id = gen_random_uuid();`
	if lines := dmlLinesAgainstTable(sql, "event_awaiters"); len(lines) != 0 {
		t.Errorf("dmlLinesAgainstTable(%q, event_awaiters) = %v, want no match against "+
			"event_awaiters_backup", sql, lines)
	}
	sql2 := `UPDATE event_awaiters SET id = gen_random_uuid();`
	if lines := dmlLinesAgainstTable(sql2, "event_awaiters_backup"); len(lines) != 0 {
		t.Errorf("dmlLinesAgainstTable(%q, event_awaiters_backup) = %v, want no match against "+
			"the shorter name event_awaiters", sql2, lines)
	}
}

func TestDmlLinesAgainstTable_CaseInsensitiveVerb(t *testing.T) {
	sql := `update event_awaiters set id = gen_random_uuid();`
	if lines := dmlLinesAgainstTable(sql, "event_awaiters"); len(lines) != 1 {
		t.Errorf("dmlLinesAgainstTable found %d lines for lowercase 'update', want 1: %v", len(lines), lines)
	}
}

func TestDmlLinesAgainstTable_InsertAndDelete(t *testing.T) {
	sql := `
		INSERT INTO event_awaiters (id) VALUES (gen_random_uuid());
		DELETE FROM event_awaiters WHERE id IS NULL;
	`
	lines := dmlLinesAgainstTable(sql, "event_awaiters")
	if len(lines) != 2 {
		t.Fatalf("dmlLinesAgainstTable found %d lines, want 2 (one INSERT, one DELETE): %v", len(lines), lines)
	}
}

func TestMssqlCrossTenantRe_MatchesTheWorkedExample(t *testing.T) {
	// The exact statement shape plugins/eventtriggers/migrations.go v6's
	// UpMSSQL already uses.
	sql := `EXEC sp_set_session_context @key = N'cross_tenant', @value = N'event-triggers migration 6 backfill, cleat#2625';`
	if !mssqlCrossTenantRe.MatchString(sql) {
		t.Errorf("mssqlCrossTenantRe did not match the worked example: %q", sql)
	}
}

func TestMssqlCrossTenantRe_DoesNotMatchTenantIdKey(t *testing.T) {
	// The FILTER predicate's OTHER key, 'tenant_id' -- setting a specific
	// tenant is not the cross-tenant bypass, and a check that treated it as
	// one would accept a migration that merely claims to be one tenant
	// rather than one that is actually exempted.
	sql := `EXEC sp_set_session_context @key = N'tenant_id', @value = @p1;`
	if mssqlCrossTenantRe.MatchString(sql) {
		t.Errorf("mssqlCrossTenantRe matched a plain tenant_id session-context call: %q", sql)
	}
}
