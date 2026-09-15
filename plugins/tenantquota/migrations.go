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
			// enforce defaults to FALSE deliberately. The owner decision on
			// cleat#1569 is soft-first: a quota counts and reports, and only
			// refuses when someone opts in. A quota that refuses on a counter
			// nobody has watched yet is the worst possible first version.
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
	}
}
