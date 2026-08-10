package engine

import (
	"errors"
	"fmt"

	"github.com/lib/pq"
)

// Workflow definitions are keyed by (name, version) on every dialect, with no
// tenant in the key, and definition names are chosen by whoever deploys. All
// three DeployWorkflowDef implementations upsert on that key, so before this
// the second tenant to deploy a given name did not collide with the first --
// it overwrote it, silently, and the first tenant's workflows then executed the
// second tenant's code. IMPROVEMENT-PLAN 3.12.
//
// The bounded fix is here: a definition records the tenant that deployed it,
// and a deploy over a definition owned by someone else is refused rather than
// applied. What it deliberately does not do is make two tenants able to hold
// the same name -- that needs the tenant in the primary key, and with it three
// foreign keys per dialect and an audit of ~96 query sites. So the name remains
// a global namespace; squatting one is now loud instead of silent.

// ErrWorkflowDefOwnedByAnotherTenant is returned by DeployWorkflowDef when a
// definition of that name and version already exists and belongs to a
// different tenant. Callers can test for it with errors.Is.
var ErrWorkflowDefOwnedByAnotherTenant = errors.New("workflow definition is owned by another tenant")

// defOwnershipError wraps ErrWorkflowDefOwnedByAnotherTenant with the
// definition it refers to.
//
// The owning tenant is deliberately NOT named in the message: the caller is by
// definition a different tenant, and which other customer holds a name is not
// theirs to learn. That the name is taken is unavoidable -- it is the
// consequence of a shared namespace, and is the reason the namespace should
// eventually carry the tenant.
func defOwnershipError(name string, version int) error {
	return fmt.Errorf("deploy workflow def %q version %d: %w",
		name, version, ErrWorkflowDefOwnedByAnotherTenant)
}

// canAdoptDef reports whether deployer may write over an existing definition
// owned by existingOwner.
//
// Three cases say yes, and the last two are what make this deployable rather
// than merely correct:
//
//   - The deployer already owns it. The ordinary redeploy.
//   - Nobody owns it: an empty or NULL tenant_id. `workflow_defs.tenant_id` is
//     nullable on MySQL, so this is a real state and not a defensive branch.
//   - The default tenant owns it. Every definition in every database that
//     predates this change is owned by the default tenant, because
//     PostgresStore hardcoded it and MSSQLStore's MERGE omitted the column
//     entirely. Refusing those would break the first redeploy after an
//     upgrade for every tenant at once, which is a worse defect than the one
//     being fixed. Such a row is adopted by the first tenant to redeploy it.
//
// The adoption case is the bounded fix's soft edge and is worth stating
// plainly: until a definition has been redeployed once, a tenant other than
// the one that created it can still take it over. After that it is owned and
// the guard holds.
func canAdoptDef(existingOwner, deployer string) bool {
	switch existingOwner {
	case "", DefaultTenantUUID, deployer:
		return true
	}
	return false
}

// isPostgresUniqueViolation reports whether err is SQLSTATE 23505.
//
// The other two dialects already have this (isDuplicateKeyError for MySQL
// 1062, isMSSQLDuplicateKey for SQL Server 2627/2601); PostgreSQL did not,
// because nothing had needed to tell a unique violation apart from any other
// write failure. Under RLS it is load-bearing: a row belonging to another
// tenant is invisible to the SELECT and unavoidable at the INSERT, so 23505 is
// how "someone else owns this name" arrives.
func isPostgresUniqueViolation(err error) bool {
	var pqErr *pq.Error
	if errors.As(err, &pqErr) {
		return pqErr.Code == "23505"
	}
	return false
}
