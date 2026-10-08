package catalogdiff

// cleat#2882. snapshotMySQL never queried information_schema.triggers before
// this change -- a trigger silently dropped or changed presented identical
// rows to every query the snapshot made, which is live today:
// migrations/mysql/003_procedures.sql defines tenants_org_id_immutable
// (BEFORE UPDATE on tenants, enforcing org_id immutability). This is the
// known-positive the comparator change needs, built the same way
// TestTheMSSQLSnapshotSeesTriggers establishes its MSSQL counterpart: two
// scratch databases differing in exactly one trigger, the difference
// confirmed independently of Snapshot before Diff is ever asked about it.

import (
	"context"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/migration"
)

func TestTheMySQLSnapshotSeesATrigger(t *testing.T) {
	a := mysqlScratchDB(t, "cleat_cd_trig_a")
	b := mysqlScratchDB(t, "cleat_cd_trig_b")
	ctx := context.Background()

	const table = `CREATE TABLE trig_target (id INT PRIMARY KEY, org_id INT NOT NULL)`
	if _, err := a.ExecContext(ctx, table); err != nil {
		t.Fatalf("build A: %v", err)
	}
	if _, err := b.ExecContext(ctx, table); err != nil {
		t.Fatalf("build B: %v", err)
	}

	// Only A gets the trigger -- same shape as this repo's real
	// tenants_org_id_immutable, kept short since the content of the body is
	// not what this control is about.
	const trigger = `CREATE TRIGGER trig_cleat_2882 BEFORE UPDATE ON trig_target FOR EACH ROW BEGIN
		IF NEW.org_id <> OLD.org_id THEN
			SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'org_id is immutable';
		END IF;
	END`
	if _, err := a.ExecContext(ctx, trigger); err != nil {
		t.Fatalf("create trigger on A: %v", err)
	}

	var countA, countB int
	q := `SELECT COUNT(*) FROM information_schema.triggers
		WHERE trigger_schema = DATABASE() AND trigger_name = 'trig_cleat_2882'`
	if err := a.QueryRow(q).Scan(&countA); err != nil {
		t.Fatalf("count A's triggers: %v", err)
	}
	if err := b.QueryRow(q).Scan(&countB); err != nil {
		t.Fatalf("count B's triggers: %v", err)
	}
	if countA == 0 || countB != 0 {
		t.Fatalf("the two databases do not actually differ in trigger presence (A=%d B=%d), "+
			"so a zero diff would prove nothing", countA, countB)
	}
	t.Logf("established independently: A has the trigger, B does not")

	ca, err := Snapshot(ctx, a, migration.DialectMySQL)
	if err != nil {
		t.Fatalf("snapshot A: %v", err)
	}
	cb, err := Snapshot(ctx, b, migration.DialectMySQL)
	if err != nil {
		t.Fatalf("snapshot B: %v", err)
	}
	d := Diff(ca, cb)
	if len(d) == 0 {
		t.Fatalf("Diff reported ZERO differences over a pair that demonstrably differs in " +
			"a trigger -- snapshotMySQL is still blind to cleat#2882's gap")
	}
	joined := strings.Join(d, "\n")
	if !strings.Contains(joined, "trig_cleat_2882") {
		t.Fatalf("Diff reported differences but none names the trigger:\n%s", joined)
	}
	t.Logf("Diff reported %d line(s):\n%s", len(d), joined)
}
