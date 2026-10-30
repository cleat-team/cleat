package engine

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

// cleat#3600 (owner D52), the upgrade window. Migration 008 creates the plugin-name registry
// EMPTY, and only a worker that carries the registration writes to it: an older worker writes
// nothing, and admin.workers has no plugin set to backfill from. So between the migration and
// the first upgraded worker starting, a deploy or install that declares a plugin is refused,
// whether or not an (older) worker that has the plugin is running. These pin that, so it is a
// stated property of the change and not something an operator discovers.
func TestBeforeTheFirstUpgradedWorkerStartsADeclaredPluginIsRefused(t *testing.T) {
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
			plugin := fmt.Sprintf("window-plugin-%d-%s", stamp, tc.name)
			forget := func() {
				_, _ = db.Exec(`DELETE FROM `+tc.edialect.registeredPluginsTable()+` WHERE plugin_name = `+tc.edialect.placeholder(1), plugin)
			}
			forget()
			t.Cleanup(forget)
			registryRows := func() int {
				var n int
				if err := db.QueryRow(`SELECT count(*) FROM ` + tc.edialect.registeredPluginsTable()).Scan(&n); err != nil {
					t.Fatalf("count the registry: %v", err)
				}
				return n
			}
			deps := map[string]string{plugin: ">=1"}

			// An older worker: it registers, and says nothing about plugins, which is what a
			// worker without the registration does. It may well carry the plugin; the registry
			// has no way to know.
			before := registryRows()
			oldWorker := fmt.Sprintf("window-old-%d-%s", stamp, tc.name)
			t.Cleanup(func() { _ = workers.Deregister(context.Background(), oldWorker) })
			if err := workers.Register(ctx, WorkerRegistration{WorkerID: oldWorker, Hostname: "old", PID: 1}); err != nil {
				t.Fatalf("register the older worker: %v", err)
			}
			if after := registryRows(); after != before {
				t.Fatalf("an older worker's registration changed the registry from %d to %d rows; it carries no plugin names and must add none", before, after)
			}

			// So both entry points refuse, with the older worker live.
			if err := CheckPluginDeps(ctx, plugins, deps, nil); !errors.Is(err, ErrUnmetPluginDeps) {
				t.Fatalf("a deploy check with only an older worker registered = %v, want ErrUnmetPluginDeps", err)
			}
			err := CheckMemberPluginDeps(ctx, plugins, "lib", []MemberPluginDeps{{Name: "m", Version: 1, Raw: []byte(fmt.Sprintf(`{%q:">=1"}`, plugin))}})
			if !errors.Is(err, ErrUnmetPluginDeps) {
				t.Fatalf("an install check with only an older worker registered = %v, want ErrUnmetPluginDeps", err)
			}

			// The first upgraded worker starts: the same checks pass.
			newWorker := fmt.Sprintf("window-new-%d-%s", stamp, tc.name)
			t.Cleanup(func() { _ = workers.Deregister(context.Background(), newWorker) })
			if err := workers.Register(ctx, WorkerRegistration{WorkerID: newWorker, Hostname: "new", PID: 2, Plugins: []string{plugin}}); err != nil {
				t.Fatalf("register the upgraded worker: %v", err)
			}
			if err := CheckPluginDeps(ctx, plugins, deps, nil); err != nil {
				t.Errorf("a deploy check after an upgraded worker registered the plugin = %v, want nil", err)
			}
			err = CheckMemberPluginDeps(ctx, plugins, "lib", []MemberPluginDeps{{Name: "m", Version: 1, Raw: []byte(fmt.Sprintf(`{%q:">=1"}`, plugin))}})
			if err != nil {
				t.Errorf("an install check after an upgraded worker registered the plugin = %v, want nil", err)
			}
		})
	}
}
