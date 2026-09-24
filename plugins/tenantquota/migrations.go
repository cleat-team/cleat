package tenantquota

import "github.com/cleat-team/cleat/plugin"

// Migrations returns the schema for quota definitions and their counters.
//
// TWO TABLES, AND THE SPLIT IS THE DESIGN. tenant_quota is configuration --
// read often, written by an operator. tenant_quota_counter is hot state,
// written once per metered event. Putting a running total in the config row
// would make every start contend on the row an operator also edits.
//
// THE COUNTER IS BUCKETED rather than one row per (tenant, resource, period),
// following plugins/ratelimiter's rate_counter. A single row is the contended
// shape cleat#1569 warns about; buckets spread writes across keys and make the
// read a SUM. The read cost is the trade, and it is the right one here because
// bucketing also answers "what is this tenant consuming right now" -- the
// reporting surface the issue asks for -- from the same rows.
func (p *Plugin) Migrations() []plugin.Migration {
	return []plugin.Migration{
		{
			Version: 1,
			// window_seconds is a ROLLING window, not a billing period.
			// "10,000 runs a month" is expressible as 2592000; a calendar
			// month is not, and buying that would mean carrying a tenant
			// anniversary date and a timezone before anything is proven.
			//
			// enforce defaulted to FALSE here, deliberately: the owner
			// decision on cleat#1569 was soft-first, a quota that counts and
			// reports and only refuses when someone opts in. v3 below changes
			// the column default to TRUE (cleat#2046, owner decision A) --
			// this migration is still correct as a description of what v1
			// shipped, and is left unedited per this repo's rule that a
			// recorded migration never runs again, but it is no longer a
			// description of what a fresh database ends up with. Read v3 for
			// the current default.
			Up: `
				CREATE TABLE IF NOT EXISTS tenant_quota (
					tenant_id      UUID NOT NULL,
					resource       TEXT NOT NULL,
					limit_count    BIGINT NOT NULL,
					window_seconds INTEGER NOT NULL,
					enforce        BOOLEAN NOT NULL DEFAULT FALSE,
					created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
					updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
					PRIMARY KEY (tenant_id, resource),
					CHECK (limit_count > 0),
					CHECK (window_seconds > 0)
				);
			`,
			UpMySQL: `
				CREATE TABLE IF NOT EXISTS tenant_quota (
					tenant_id      CHAR(36) NOT NULL,
					resource       VARCHAR(255) NOT NULL,
					limit_count    BIGINT NOT NULL,
					window_seconds INT NOT NULL,
					enforce        BOOLEAN NOT NULL DEFAULT FALSE,
					created_at     TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
					updated_at     TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
					PRIMARY KEY (tenant_id, resource),
					CHECK (limit_count > 0),
					CHECK (window_seconds > 0)
				);
			`,
			UpMSSQL: `
				IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'tenant_quota')
				CREATE TABLE tenant_quota (
					tenant_id      UNIQUEIDENTIFIER NOT NULL,
					resource       NVARCHAR(255) NOT NULL,
					limit_count    BIGINT NOT NULL,
					window_seconds INT NOT NULL,
					enforce        BIT NOT NULL DEFAULT 0,
					created_at     DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME(),
					updated_at     DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME(),
					PRIMARY KEY (tenant_id, resource),
					CHECK (limit_count > 0),
					CHECK (window_seconds > 0)
				);
			`,
			// Every reader of this table resolves a tenant from the request
			// context before touching it -- the middleware returns early when
			// auth.TenantIDFromContext finds none -- so a row-level policy is
			// the right protection and nothing in this plugin needs a
			// cross-tenant sweep. Without the declaration no policy is
			// installed and isolation rests entirely on this plugin's own
			// WHERE clauses, which is cleat#1277 and cleat#1512.
			TenantScoped: []string{"tenant_quota"},
			Down:         `DROP TABLE IF EXISTS tenant_quota;`,
		},
		{
			Version: 2,
			// bucket_start is truncated to the hour. rate_counter buckets by
			// SECOND because it answers questions about seconds; a quota
			// window is days or a month, so an hourly bucket keeps the SUM
			// small (744 rows for a 31-day window) while still spreading
			// writes far more than a single row would.
			Up: `
				CREATE TABLE IF NOT EXISTS tenant_quota_counter (
					tenant_id    UUID NOT NULL,
					resource     TEXT NOT NULL,
					bucket_start TIMESTAMPTZ NOT NULL,
					count        BIGINT NOT NULL DEFAULT 0,
					PRIMARY KEY (tenant_id, resource, bucket_start)
				);
			`,
			UpMySQL: `
				CREATE TABLE IF NOT EXISTS tenant_quota_counter (
					tenant_id    CHAR(36) NOT NULL,
					resource     VARCHAR(255) NOT NULL,
					bucket_start TIMESTAMP(6) NOT NULL,
					count        BIGINT NOT NULL DEFAULT 0,
					PRIMARY KEY (tenant_id, resource, bucket_start)
				);
			`,
			UpMSSQL: `
				IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'tenant_quota_counter')
				CREATE TABLE tenant_quota_counter (
					tenant_id    UNIQUEIDENTIFIER NOT NULL,
					resource     NVARCHAR(255) NOT NULL,
					bucket_start DATETIMEOFFSET NOT NULL,
					count        BIGINT NOT NULL DEFAULT 0,
					PRIMARY KEY (tenant_id, resource, bucket_start)
				);
			`,
			// Same reasoning as tenant_quota above. Worth stating separately
			// rather than by reference, because a counter is the table most
			// likely to tempt a future cross-tenant aggregate -- "usage across
			// all tenants" -- and that is exactly the reader a row-level policy
			// would refuse. If such a sweep is ever wanted it needs its own
			// deliberate path, not the removal of this line.
			TenantScoped: []string{"tenant_quota_counter"},
			Down:         `DROP TABLE IF EXISTS tenant_quota_counter;`,
		},
		{
			Version: 3,
			// cleat#2046, owner decision A: enforce now defaults to TRUE, so
			// a quota set via cleatctl without --enforce is enforced on the
			// next start. Existing rows are untouched -- this changes only
			// the COLUMN DEFAULT, which SQL applies solely to a row that
			// omits the value, and cleatctl always supplies enforce
			// explicitly (cmd/cleatctl/quota.go), so this migration's effect
			// is on any OTHER writer, present or future, that inserts a row
			// without naming it. A new tenant still gets no tenant_quota row
			// at all -- there is no tenant-creation hook that would write
			// one (cleat#1114 is Postgres-only and unrelated) -- so "no
			// limit until an operator sets one" is unchanged.
			Up:      `ALTER TABLE tenant_quota ALTER COLUMN enforce SET DEFAULT TRUE;`,
			UpMySQL: `ALTER TABLE tenant_quota ALTER COLUMN enforce SET DEFAULT TRUE;`,
			// SQL Server names a column default constraint automatically
			// unless one is given at CREATE TABLE time, and v1 gave none --
			// so changing the default means finding that generated name and
			// dropping it before a new, named one can be added; a column can
			// carry only one default constraint at a time.
			//
			// This cannot use the DECLARE/SELECT/IF/sp_executesql pattern
			// core .sql migrations use for the same problem (see
			// migrations/mssql/094 and /081): plugin.splitStatements splits
			// this string on EVERY semicolon and runs each fragment as its
			// own ExecContext call, and a local variable does not survive
			// across separate batches -- so a DECLARE in one fragment is
			// gone before a later fragment's IF could read it. The whole
			// DECLARE/SELECT/IF/EXEC sequence below therefore has to be, and
			// is, ONE fragment: no semicolon appears until after it, so
			// splitStatements leaves it as a single multi-line statement
			// sent to the server as one batch, where the local variable
			// lives for the sequence's whole duration. Measured against a
			// live SQL Server container 2026-09-24: the equivalent written
			// with semicolons between DECLARE/SELECT/IF fails with "must
			// declare the scalar variable" the moment it is split, and
			// EXEC's own grammar refuses a function call or subquery
			// in-line (EXEC(QUOTENAME(...)) and EXEC(sp_executesql-style
			// concatenation both fail with a syntax error at parse time) --
			// only EXEC(<literal-or-variable> [+ <literal-or-variable>]...)
			// is accepted, which is why the constraint name is resolved into
			// a variable first rather than inlined.
			UpMSSQL: `
				DECLARE @dfname sysname
				SELECT @dfname = dc.name
					FROM sys.default_constraints dc
					JOIN sys.columns c ON c.object_id = dc.parent_object_id AND c.column_id = dc.parent_column_id
					WHERE dc.parent_object_id = OBJECT_ID(N'tenant_quota') AND c.name = N'enforce'
				IF @dfname IS NOT NULL EXEC('ALTER TABLE tenant_quota DROP CONSTRAINT [' + @dfname + ']');

				ALTER TABLE tenant_quota ADD CONSTRAINT df_tenant_quota_enforce DEFAULT 1 FOR enforce;
			`,
			Down:      `ALTER TABLE tenant_quota ALTER COLUMN enforce SET DEFAULT FALSE;`,
			DownMySQL: `ALTER TABLE tenant_quota ALTER COLUMN enforce SET DEFAULT FALSE;`,
			// The mirror of UpMSSQL: drop the now-named constraint and add
			// an unnamed default of 0 back, the same shape v1 originally
			// created (SQL Server auto-names it again).
			DownMSSQL: `
				ALTER TABLE tenant_quota DROP CONSTRAINT df_tenant_quota_enforce;
				ALTER TABLE tenant_quota ADD DEFAULT 0 FOR enforce;
			`,
		},
	}
}
