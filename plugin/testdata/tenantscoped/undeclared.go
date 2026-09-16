// Package tenantscoped is a FIXTURE, not a plugin. It is never built: Go
// ignores testdata/, and nothing imports it.
//
// It exists so TestTheTenantScopedScannerReportsAnUndeclaredTable has a case
// that is already known to be broken. A guard that passes a clean tree has only
// shown that it agrees with itself; the question that matters is whether it
// reports something genuinely wrong, and every plugin in the real tree is
// correct today -- so without this file the guard would be green whether or not
// it worked at all.
//
// Both shapes are here on purpose. theDefect must be reported and
// theCorrectForm must not, so the fixture proves the scanner DISCRIMINATES
// rather than merely flagging everything it sees.
package tenantscoped

// Migration mirrors the shape plugin.Migration presents to the scanner. It is
// declared locally rather than imported because this file is parsed, never
// compiled, and an import would suggest otherwise.
type Migration struct {
	Version      int
	Up           string
	UpMySQL      string
	UpMSSQL      string
	TenantScoped []string
}

// theDefect: a table carrying tenant_id with no TenantScoped declaration. On a
// real plugin this gets no row-level security policy on any dialect, silently.
var theDefect = []Migration{{
	Version: 1,
	Up: `CREATE TABLE IF NOT EXISTS fixture_undeclared_rows (
		tenant_id UUID NOT NULL,
		k TEXT NOT NULL,
		payload JSONB NOT NULL DEFAULT '{}'::jsonb,
		PRIMARY KEY (tenant_id, k))`,
}}

// theCorrectForm: the same table, declared. The scanner must leave this alone.
var theCorrectForm = []Migration{{
	Version: 2,
	Up: `CREATE TABLE IF NOT EXISTS fixture_declared_rows (
		tenant_id UUID NOT NULL,
		k TEXT NOT NULL,
		PRIMARY KEY (tenant_id, k))`,
	TenantScoped: []string{"fixture_declared_rows"},
}}

// theConcatenatedForm: a tenant table whose DDL is split by a backtick-quoted
// identifier, which is how MySQL arms are written when a column name is
// reserved. A regex-based scan captures the first fragment and stops, so it
// would never see the tenant_id column here and would report this table as not
// needing a declaration. The go/ast reading evaluates the concatenation.
var theConcatenatedForm = []Migration{{
	Version: 3,
	UpMySQL: `CREATE TABLE IF NOT EXISTS fixture_concat_rows (
		tenant_id CHAR(36) NOT NULL,
		` + "`key`" + ` VARCHAR(255) NOT NULL,
		PRIMARY KEY (tenant_id, ` + "`key`" + `))`,
}}
