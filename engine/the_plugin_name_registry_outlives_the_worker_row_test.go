package engine

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

// cleat#3600 (owner D52): the record of registered plugin names is not admin.workers. A
// worker's row there is deleted on a clean shutdown and by the expiry sweep, so a "met"
// that read it would be "some worker is live now", and a deploy would fail across a rolling
// restart. These pin that the plugin names outlive both deletions, on all three dialects.
func TestThePluginNameRegistryOutlivesTheWorkerRow(t *testing.T) {
	for _, tc := range []struct {
		name     string
		tdialect testutil.Dialect
		edialect Dialect
	}{
		{"postgres", testutil.DialectPostgres, DialectPostgres},
		{"mysql", testutil.DialectMySQL, DialectMySQL},
		{"mssql", testutil.DialectMSSQL, DialectMSSQL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testutil.TestDB(t, tc.tdialect)
			t.Cleanup(func() { db.Close() })
			testutil.SetupFullSchema(t, db, tc.tdialect)
			ctx := context.Background()
			workers := &WorkerRegistry{DB: db, Dialect: tc.edialect}
			plugins := &PluginNameRegistry{DB: db, Dialect: tc.edialect}

			stamp := time.Now().UnixNano()
			nameA := fmt.Sprintf("plugin-a-%d-%s", stamp, tc.name)
			nameB := fmt.Sprintf("plugin-b-%d-%s", stamp, tc.name)
			never := fmt.Sprintf("plugin-never-%d-%s", stamp, tc.name)
			forget := func() {
				for _, n := range []string{nameA, nameB, never} {
					_, _ = db.Exec(`DELETE FROM `+tc.edialect.registeredPluginsTable()+` WHERE plugin_name = `+tc.edialect.placeholder(1), n)
				}
			}
			forget()
			t.Cleanup(forget)

			known := func(t *testing.T, names ...string) map[string]bool {
				t.Helper()
				k, err := plugins.KnownPlugins(ctx, names)
				if err != nil {
					t.Fatalf("KnownPlugins: %v", err)
				}
				return k
			}

			// Before anything registers, nothing is known: the control that the answers
			// below come from the registration and not from a fixture left in the table.
			if k := known(t, nameA, nameB, never); len(k) != 0 {
				t.Fatalf("known before any registration = %v, want none", k)
			}

			workerID := fmt.Sprintf("plugin-test-worker-%d-%s", stamp, tc.name)
			t.Cleanup(func() { _ = workers.Deregister(context.Background(), workerID) })
			if err := workers.Register(ctx, WorkerRegistration{WorkerID: workerID, Hostname: "h", PID: 1, Plugins: []string{nameA, nameB}}); err != nil {
				t.Fatalf("Register: %v", err)
			}
			k := known(t, nameA, nameB, never)
			if !k[nameA] || !k[nameB] || k[never] {
				t.Fatalf("known after registering A and B = %v, want A and B and not %s", k, never)
			}
			// Asking about a name twice, and about none, are both fine.
			if k := known(t, nameA, nameA); len(k) != 1 || !k[nameA] {
				t.Errorf("a repeated name = %v, want just A", k)
			}
			if k := known(t); len(k) != 0 {
				t.Errorf("asking about no names = %v, want empty", k)
			}

			workerRows := func() int {
				var n int
				if err := db.QueryRow(`SELECT count(*) FROM `+workers.table()+` WHERE worker_id = `+tc.edialect.placeholder(1), workerID).Scan(&n); err != nil {
					t.Fatalf("count the worker row: %v", err)
				}
				return n
			}
			if workerRows() != 1 {
				t.Fatal("the registered worker has no row, so the deletions below would prove nothing")
			}

			// A clean shutdown deletes the worker's row. The plugin names stay.
			if err := workers.Deregister(ctx, workerID); err != nil {
				t.Fatalf("Deregister: %v", err)
			}
			if workerRows() != 0 {
				t.Fatal("Deregister left the worker's row, so this does not test what it claims to")
			}
			if k := known(t, nameA, nameB); !k[nameA] || !k[nameB] {
				t.Errorf("after Deregister known = %v, want A and B still known", k)
			}

			// The expiry sweep deletes a worker whose heartbeat is old. The names stay.
			if err := workers.Register(ctx, WorkerRegistration{WorkerID: workerID, Hostname: "h", PID: 1, Plugins: []string{nameA}}); err != nil {
				t.Fatalf("re-Register: %v", err)
			}
			if _, err := db.Exec(`UPDATE `+workers.table()+` SET last_heartbeat_at = `+tc.edialect.intervalExpr(1)+` WHERE worker_id = `+tc.edialect.placeholder(2), 3600, workerID); err != nil {
				t.Fatalf("back-date the heartbeat: %v", err)
			}
			if n, err := workers.SweepExpired(ctx, time.Minute); err != nil || n < 1 {
				t.Fatalf("SweepExpired removed %d rows (err %v), want at least the back-dated worker", n, err)
			}
			if workerRows() != 0 {
				t.Fatal("the sweep left the back-dated worker's row")
			}
			if k := known(t, nameA, nameB); !k[nameA] || !k[nameB] {
				t.Errorf("after SweepExpired known = %v, want A and B still known", k)
			}

			// first_seen_at is set once and last_seen_at moves forward.
			readSeen := func() (time.Time, time.Time) {
				var first, last time.Time
				if err := db.QueryRow(`SELECT first_seen_at, last_seen_at FROM `+tc.edialect.registeredPluginsTable()+` WHERE plugin_name = `+tc.edialect.placeholder(1), nameA).Scan(&first, &last); err != nil {
					t.Fatalf("read the seen times: %v", err)
				}
				return first, last
			}
			first1, last1 := readSeen()
			time.Sleep(1100 * time.Millisecond)
			if err := plugins.Record(ctx, []string{nameA}); err != nil {
				t.Fatalf("Record again: %v", err)
			}
			first2, last2 := readSeen()
			if !first2.Equal(first1) {
				t.Errorf("first_seen_at moved from %v to %v on a second registration", first1, first2)
			}
			if !last2.After(last1) {
				t.Errorf("last_seen_at did not advance on a second registration (%v then %v)", last1, last2)
			}
		})
	}
}

// A name the table's CHECK would reject must be refused before any statement runs: a
// rejected row inside a PostgreSQL transaction aborts the transaction, and registration runs
// inside one. And it must not leave a half-registered worker on a plain connection.
func TestARegistrationWithAnUnrecordablePluginNameRegistersNothing(t *testing.T) {
	db := testutil.TestDB(t, testutil.DialectPostgres)
	t.Cleanup(func() { db.Close() })
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)
	ctx := context.Background()
	workers := &WorkerRegistry{DB: db, Dialect: DialectPostgres}
	for _, bad := range []string{"", strings.Repeat("p", maxRegisteredPluginNameLen+1)} {
		id := fmt.Sprintf("bad-plugin-worker-%d", time.Now().UnixNano())
		t.Cleanup(func() { _ = workers.Deregister(context.Background(), id) })
		err := workers.Register(ctx, WorkerRegistration{WorkerID: id, Hostname: "h", PID: 1, Plugins: []string{"fine-plugin", bad}})
		if err == nil {
			t.Fatalf("a plugin name of %d bytes registered without error", len(bad))
		}
		var n int
		if qerr := db.QueryRow(`SELECT count(*) FROM admin.workers WHERE worker_id = $1`, id).Scan(&n); qerr != nil || n != 0 {
			t.Errorf("a refused registration left %d worker rows (err %v), want none", n, qerr)
		}
	}
}

// "Nothing deletes from this table" is a privilege on PostgreSQL and SQL Server, not only a
// comment: the application role may read, insert and update, and may not delete. Read from
// the catalog, because the stand-in role the other tests connect as is granted by the test
// harness and would only prove what the harness says.
func TestTheApplicationRoleCannotDeleteFromThePluginNameRegistry(t *testing.T) {
	t.Run("postgres", func(t *testing.T) {
		db := testutil.TestDB(t, testutil.DialectPostgres)
		t.Cleanup(func() { db.Close() })
		testutil.SetupFullSchema(t, db, testutil.DialectPostgres)
		want := map[string]bool{"SELECT": true, "INSERT": true, "UPDATE": true, "DELETE": false}
		for priv, allowed := range want {
			var has bool
			if err := db.QueryRow(`SELECT has_table_privilege('cleat_app', 'admin.registered_plugins', $1)`, priv).Scan(&has); err != nil {
				t.Fatalf("has_table_privilege %s: %v", priv, err)
			}
			if has != allowed {
				t.Errorf("cleat_app %s on admin.registered_plugins = %v, want %v", priv, has, allowed)
			}
		}
	})
	t.Run("mssql", func(t *testing.T) {
		db := testutil.TestDB(t, testutil.DialectMSSQL)
		t.Cleanup(func() { db.Close() })
		testutil.SetupFullSchema(t, db, testutil.DialectMSSQL)
		var state string
		err := db.QueryRow(`
			SELECT state_desc FROM sys.database_permissions
			WHERE class = 1 AND major_id = OBJECT_ID(N'admin.registered_plugins')
			  AND grantee_principal_id = DATABASE_PRINCIPAL_ID(N'cleat_app_role') AND permission_name = N'DELETE'`).Scan(&state)
		if err != nil {
			t.Fatalf("read the DELETE permission: %v (no row means DELETE is not denied)", err)
		}
		if state != "DENY" {
			t.Errorf("cleat_app_role DELETE on admin.registered_plugins is %s, want DENY", state)
		}
	})
}
