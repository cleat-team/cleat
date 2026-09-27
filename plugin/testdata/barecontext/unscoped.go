// FIXTURE, not code. Under testdata/ so the Go tool does not build it; it
// exists to be scanned.
//
// It issues a statement against a tenant-scoped table on a bare
// context.Background(), which is the defect the guard looks for, and one on a
// context that carries a tenant, which it must NOT report. A guard that passes
// on a clean tree is satisfied by every broken version of itself; these two are
// the cases it must get right in both directions.
package fixture

import (
	"context"

	"github.com/cleat-team/cleat/plugin"
	"github.com/google/uuid"
)

func theDefect(db plugin.PluginDB) {
	// kv_store is tenant-scoped. This carries no tenant.
	_, _ = db.Exec(context.Background(),
		`DELETE FROM kv_store WHERE tenant_id = $1`, "some-tenant")
}

func theCorrectForm(db plugin.PluginDB, tenant uuid.UUID) {
	_, _ = db.Exec(plugin.ForTenant(context.Background(), tenant),
		`DELETE FROM kv_store WHERE tenant_id = $1`, tenant)
}
