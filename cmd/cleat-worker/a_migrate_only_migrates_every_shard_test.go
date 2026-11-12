package main

// cleat#3718. `cleat-worker --shards-file <f> --migrate-only` migrated the first shard
// and exited 0, leaving every other shard with no schema: the migration runner was
// built on the admin shard's database and nothing ever opened a runner on the rest. The
// failure surfaced later, on a normal start, as a missing-table error that names no cause.
//
// This runs the real binary against real databases, because the property is about what
// the PROCESS did -- its exit status and what it left in each database -- and a unit
// test of a helper would pass with the flag wired to nothing.
//
// PostgreSQL only: a shards file is a PostgreSQL feature (the sharded branch opens
// "postgres" pools), so there is no other dialect to run.
//
// # The cases, and what each one is the known-positive for
//
//   - every shard migrated: the issue. Shard 0 passes on the old code and shard 1 does not,
//     so a pass on shard 0 alone is the control that the run did migrate something.
//   - a second run changes nothing: the fix must stay idempotent on shards it already did.
//   - a shard that carries another release's migration chain refuses the run, names the
//     shard, exits non-zero, and leaves EVERY shard as it was: the first shard is not
//     migrated and the failure reported afterwards.
//   - a shard whose migration fails (past the chain check) exits non-zero and names the shard.
//   - a shard with its own `schema` is migrated into that schema, the one its stores read
//     (cleat#1287 one shard over).
//   - a NORMAL start refuses a shard on another release's chain, naming it (cleat#3405's rule,
//     "at --migrate-only and at boot alike", now applied to every shard), and starts when the
//     chain is the shipped one.
//   - the migrated deployment starts: a sharded worker boots on shards a --migrate-only
//     run prepared, which is the thing the deploy step is for.

import (
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestMigrateOnlyMigratesEveryShard(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the worker binary")
	}
	c := deployDialect{"postgres", "CLEAT_TEST_POSTGRES", "postgres"}
	if c.admin() == "" {
		t.Skipf("%s not set, skipping the sharded --migrate-only test", c.env)
	}
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "cleat-worker")
	build := exec.Command("go", "build", "-o", bin, "./cmd/cleat-worker")
	build.Dir = repoRoot
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build ./cmd/cleat-worker: %v\n%s", err, out)
	}

	// shardsFile writes a shards file naming the databases in order; the first is the
	// admin shard.
	shardsFile := func(t *testing.T, dsns ...string) string {
		t.Helper()
		type shard struct {
			Name    string `json:"name"`
			ConnStr string `json:"conn_str"`
		}
		var shards []shard
		for i, d := range dsns {
			shards = append(shards, shard{Name: "shard-" + string(rune('0'+i)), ConnStr: d})
		}
		b, err := json.Marshal(shards)
		if err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(t.TempDir(), "shards.json")
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	// shardsFileSchemas is shardsFile with a schema per shard ("" leaves it unset).
	shardsFileSchemas := func(t *testing.T, dsns, schemas []string) string {
		t.Helper()
		type shard struct {
			Name    string `json:"name"`
			ConnStr string `json:"conn_str"`
			Schema  string `json:"schema,omitempty"`
		}
		var shards []shard
		for i, d := range dsns {
			shards = append(shards, shard{Name: "shard-" + string(rune('0'+i)), ConnStr: d, Schema: schemas[i]})
		}
		b, err := json.Marshal(shards)
		if err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(t.TempDir(), "shards.json")
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	inSchema := func(t *testing.T, db *sql.DB, schema, table string) int {
		t.Helper()
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM information_schema.tables WHERE table_schema = $1 AND table_name = $2`, schema, table).Scan(&n); err != nil {
			t.Fatalf("looking for %s.%s: %v", schema, table, err)
		}
		return n
	}
	ledger := func(t *testing.T, db *sql.DB) int {
		t.Helper()
		if tableCount(t, db, "postgres", "schema_migrations") == 0 {
			return 0
		}
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM schema_migrations`).Scan(&n); err != nil {
			t.Fatalf("reading schema_migrations: %v", err)
		}
		return n
	}

	t.Run("every shard is migrated, and a second run changes nothing", func(t *testing.T) {
		dsn0, db0 := deployScratchNamed(t, c, "cleat_3718_s0_")
		dsn1, db1 := deployScratchNamed(t, c, "cleat_3718_s1_")
		dsn2, db2 := deployScratchNamed(t, c, "cleat_3718_s2_")
		args := []string{"--driver=postgres", "--shards-file=" + shardsFile(t, dsn0, dsn1, dsn2), "--migrate-only"}

		code, out := runWorker(t, bin, nil, args...)
		if code != 0 {
			t.Fatalf("--migrate-only on a three-shard deployment exited %d:\n%s", code, out)
		}
		first := ledger(t, db0)
		if first == 0 {
			t.Fatalf("--migrate-only exited 0 but the admin shard has no migrations recorded: the run measured nothing.\n%s", out)
		}
		for i, db := range []*sql.DB{db1, db2} {
			if got := ledger(t, db); got != first {
				t.Errorf("shard-%d has %d migration(s) recorded after --migrate-only exited 0, the admin shard has %d. "+
					"A deploy step that reports success has to have migrated every shard it was given (cleat#3718).",
					i+1, got, first)
			}
		}
		// The plugin tables live on the admin shard only, as docs/reference/worker-config.md says.
		if tableCount(t, db0, "postgres", "audit_events") == 0 {
			t.Errorf("the admin shard has no plugin tables after --migrate-only (audit_events is missing)")
		}
		if tableCount(t, db1, "postgres", "audit_events") != 0 {
			t.Errorf("a non-admin shard got plugin tables; plugin migrations run on the admin shard only")
		}

		code, out = runWorker(t, bin, nil, args...)
		if code != 0 {
			t.Fatalf("second --migrate-only exited %d:\n%s", code, out)
		}
		for i, db := range []*sql.DB{db0, db1, db2} {
			if got := ledger(t, db); got != first {
				t.Errorf("a second --migrate-only changed shard-%d's ledger: %d -> %d", i, first, got)
			}
		}

		// The point of the deploy step: a sharded worker boots on what it prepared.
		if ok, out := startsHealthy(t, bin, "--driver=postgres", "--shards-file="+shardsFile(t, dsn0, dsn1, dsn2)); !ok {
			t.Errorf("a sharded worker did not start on shards --migrate-only had migrated:\n%s", out)
		}
	})

	t.Run("a shard on another release's chain refuses the run before any shard is changed", func(t *testing.T) {
		dsn0, db0 := deployScratchNamed(t, c, "cleat_3718_f0_")
		dsn1, db1 := deployScratchNamed(t, c, "cleat_3718_f1_")
		// A schema_migrations table with no identity columns is what another release's
		// runner leaves; the chain check refuses it (cleat#3405).
		if _, err := db1.Exec(`CREATE TABLE schema_migrations (version TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
			t.Fatal(err)
		}
		if _, err := db1.Exec(`INSERT INTO schema_migrations (version) VALUES ('1')`); err != nil {
			t.Fatal(err)
		}
		code, out := runWorker(t, bin, nil, "--driver=postgres", "--shards-file="+shardsFile(t, dsn0, dsn1), "--migrate-only")
		if code == 0 {
			t.Fatalf("--migrate-only exited 0 with a shard on another release's migration chain:\n%s", out)
		}
		if !strings.Contains(out, "shard-1") {
			t.Errorf("the refusal must name the shard so an operator knows which database it is; got:\n%s", out)
		}
		if got := ledger(t, db0); got != 0 {
			t.Errorf("the admin shard was migrated (%d migration(s) recorded) although another shard was refused; "+
				"every shard's chain must be checked before any is changed", got)
		}
	})

	t.Run("a shard whose migration fails makes the run exit non-zero and names the shard", func(t *testing.T) {
		dsn0, _ := deployScratchNamed(t, c, "cleat_3718_m0_")
		dsn1, db1 := deployScratchNamed(t, c, "cleat_3718_m1_")
		// No ledger, so the chain check passes; a table the first migration creates, so the
		// migration itself fails. This is the path the chain check cannot reach: the one
		// where a shard is accepted and then cannot be migrated.
		if _, err := db1.Exec(`CREATE TABLE workflow_instances (id int)`); err != nil {
			t.Fatal(err)
		}
		code, out := runWorker(t, bin, nil, "--driver=postgres", "--shards-file="+shardsFile(t, dsn0, dsn1), "--migrate-only")
		if code == 0 {
			t.Fatalf("--migrate-only exited 0 although shard-1's migration failed:\n%s", out)
		}
		if !strings.Contains(out, "shard-1") {
			t.Errorf("the failure must name the shard; got:\n%s", out)
		}
	})

	t.Run("a shard with its own schema is migrated into that schema", func(t *testing.T) {
		dsn0, db0 := deployScratchNamed(t, c, "cleat_3718_c0_")
		dsn1, db1 := deployScratchNamed(t, c, "cleat_3718_c1_")
		const schema = "shard_one_3718"
		file := shardsFileSchemas(t, []string{dsn0, dsn1}, []string{"", schema})
		code, out := runWorker(t, bin, nil, "--driver=postgres", "--shards-file="+file, "--migrate-only")
		if code != 0 {
			t.Fatalf("--migrate-only with a per-shard schema exited %d:\n%s", code, out)
		}
		// The control: the first shard, with no schema of its own, is in public, so the
		// query can see a ledger where one is.
		if inSchema(t, db0, "public", "schema_migrations") != 1 {
			t.Fatalf("the first shard has no schema_migrations in public: the run measured nothing.\n%s", out)
		}
		if inSchema(t, db1, schema, "schema_migrations") != 1 || inSchema(t, db1, schema, "workflow_instances") != 1 {
			t.Errorf("shard-1 was not migrated into its own schema %q, which is the schema its stores read "+
				"(NewPostgresStoreFactory(sdb, cfg.Schema)).\n%s", schema, out)
		}
		if inSchema(t, db1, "public", "schema_migrations") != 0 || inSchema(t, db1, "public", "workflow_instances") != 0 {
			t.Errorf("shard-1 got tables in public although the shards file gives it schema %q (cleat#1287 one shard over)", schema)
		}
	})

	t.Run("a normal start refuses a shard on another release's chain, and starts when it is the shipped one", func(t *testing.T) {
		dsn0, _ := deployScratchNamed(t, c, "cleat_3718_n0_")
		dsn1, db1 := deployScratchNamed(t, c, "cleat_3718_n1_")
		file := shardsFile(t, dsn0, dsn1)
		if code, out := runWorker(t, bin, nil, "--driver=postgres", "--shards-file="+file, "--migrate-only"); code != 0 {
			t.Fatalf("--migrate-only exited %d:\n%s", code, out)
		}
		// The known-positive: before the shard is damaged, a normal start on this very
		// deployment starts, so the refusal below is the chain and nothing else.
		if ok, out := startsHealthy(t, bin, "--driver=postgres", "--shards-file="+file); !ok {
			t.Fatalf("a sharded worker did not start on a fully migrated deployment:\n%s", out)
		}
		if _, err := db1.Exec(`DROP TABLE schema_migrations`); err != nil {
			t.Fatal(err)
		}
		if _, err := db1.Exec(`CREATE TABLE schema_migrations (version TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
			t.Fatal(err)
		}
		if _, err := db1.Exec(`INSERT INTO schema_migrations (version) VALUES ('1')`); err != nil {
			t.Fatal(err)
		}
		code, out := runWorker(t, bin, nil, "--driver=postgres", "--shards-file="+file)
		if code == 0 {
			t.Fatalf("a normal start exited 0 with shard-1 on another release's chain:\n%s", out)
		}
		if !strings.Contains(out, "refusing to start (shard shard-1)") {
			t.Errorf("the refusal must be the chain check and must name the shard; got:\n%s", out)
		}
	})
}
