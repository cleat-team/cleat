package tenantquota

import (
	"testing"

	"github.com/cleat-team/cleat/plugin"
)

// Compile-time proof that this plugin satisfies the OPTIONAL interfaces
// everything about it depends on.
//
// Both are discovered by type assertion, so a signature that drifts does not
// fail to compile -- the plugin simply stops being selected, silently:
//
//   - HasMigrations: engine/plugin_migrations_test.go filters plugins with
//     `if _, ok := lp.Plugin.(plugin.HasMigrations); !ok { continue }`. Drift
//     here removes this plugin from the only place its three dialects of SQL
//     are executed, and that test still passes -- it verifies the tables of
//     the plugins it DID migrate.
//   - HasMiddleware: cmd/cleat-worker/main.go wraps with
//     `if p, ok := lp.Plugin.(plugin.HasMiddleware); ok`. Drift here means no
//     start is ever metered and nothing anywhere reports a problem.
//
// Both failure modes are the quiet kind: the worker runs, the tests pass, and
// the feature is absent. This file is three lines and turns either into a
// compile error.
var (
	_ plugin.Plugin        = (*Plugin)(nil)
	_ plugin.HasMigrations = (*Plugin)(nil)
	_ plugin.HasMiddleware = (*Plugin)(nil)
)

// TestPluginIsDiscoverableWithItsMigrations guards the registration itself,
// which the assertions above cannot see: a plugin can satisfy every interface
// and still never be registered.
func TestPluginIsDiscoverableWithItsMigrations(t *testing.T) {
	p := New()
	if got := p.Info().Name; got != "tenant-quota" {
		t.Fatalf("plugin name %q, want %q -- the name is what engine's migration "+
			"test and the operator's plugin list both key on", got, "tenant-quota")
	}
	hm, ok := p.(plugin.HasMigrations)
	if !ok {
		t.Fatal("the plugin does not implement HasMigrations")
	}
	if len(hm.Migrations()) == 0 {
		t.Error("no migrations declared, so the tables this plugin reads would " +
			"never be created and every quota read would fail")
	}
}
