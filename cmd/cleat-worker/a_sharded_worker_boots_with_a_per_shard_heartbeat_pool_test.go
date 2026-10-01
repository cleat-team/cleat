package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/lib/pq"
)

// cleat#2195 (template from cleat-review's #2925 probe): boot the REAL worker with a 2-shard --shards-file.
func TestAShardedWorkerBootsWithAPerShardHeartbeatPool(t *testing.T) {
	admin := dbTestAdminDSN(t)
	if admin == "" {
		t.Skip("no postgres")
	}
	adb, err := sql.Open("postgres", admin)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("cleat_2195_sharded_boot_%d", time.Now().UnixNano()%1_000_000_000)
	if _, err := adb.Exec(`CREATE DATABASE ` + name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// adb is closed HERE, after the drop -- a `defer adb.Close()` above
		// would run first and make both statements below silently fail
		// (cleat-review's probe leaked its databases exactly that way).
		_, _ = adb.Exec(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()`, name)
		if _, err := adb.Exec(`DROP DATABASE IF EXISTS ` + name); err != nil {
			t.Errorf("drop %s: %v", name, err)
		}
		adb.Close()
	})
	ownerDSN := replaceDBName(t, admin, name)
	if code, out := runBootSubprocess(t, []string{"--driver=postgres", "--db=" + ownerDSN, "--migrate-only"}, ""); code != 0 {
		t.Fatalf("--migrate-only exited %d:\n%s", code, out)
	}
	owner, err := sql.Open("postgres", ownerDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	dsn := pgAppRoleDSN(t, owner, ownerDSN)
	sf := filepath.Join(t.TempDir(), "shards.json")
	if err := os.WriteFile(sf, []byte(fmt.Sprintf(`[{"name":"s0","conn_str":%q},{"name":"s1","conn_str":%q}]`, dsn, dsn)), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"--driver=postgres", "--db=" + dsn, "--migrate-db=" + ownerDSN, "--shards-file=" + sf,
		fmt.Sprintf("--api-addr=127.0.0.1:%d", freePort(t))}
	code, out := runBootSubprocess(t, args, "")
	if code != -1 {
		t.Fatalf("sharded worker exited %d instead of being killed while serving:\n%s", code, out)
	}
	if !strings.Contains(out, "sharded heartbeat DB pools configured") {
		t.Fatalf("sharded worker served but never opened its per-shard heartbeat pools:\n%s", out)
	}
	if !strings.Contains(out, "heartbeat=6") { // 2 shards x default --heartbeat-max-connections=3
		t.Errorf("connection budget does not charge 2 x 3 heartbeat connections:\n%s", out)
	}
}
