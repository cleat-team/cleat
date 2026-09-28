package tenantlifecycle

import "github.com/cleat-team/cleat/plugin"

// Migrations returns the database schema for tenant_trials.
func (p *Plugin) Migrations() []plugin.Migration {
	return []plugin.Migration{
		{
			// tenant_trials is one row per tenant with a trial in flight.
			// PRIMARY KEY tenant_id, not an id column plus a unique index --
			// a tenant has at most one trial window at a time by
			// construction (cleatctl set-tenant-trial upserts), so there is
			// nothing a second key would let a query express that this one
			// cannot.
			//
			// handled is a plain flag rather than deleting the row on
			// suspension, so an operator running `cleatctl set-tenant-trial`
			// again after a sweep already fired can tell (from the row
			// existing) that this tenant HAD a trial rather than never
			// having one -- and see background.go's comment on why the
			// sweep clears it back to false when it does.
			Version: 1,
			Up: `
				CREATE TABLE IF NOT EXISTS tenant_trials (
					tenant_id  UUID NOT NULL,
					expires_at TIMESTAMPTZ NOT NULL,
					handled    BOOLEAN NOT NULL DEFAULT false,
					PRIMARY KEY (tenant_id)
				);
			`,
			// DATETIME(6), not TIMESTAMP(6): TIMESTAMP overflows in 2038
			// (plugins/auditlog/migrations.go:163-171 made the same fix, for
			// the same reason, on an already-deployed column -- this one is
			// new, so it starts right rather than needing a second
			// migration). DATETIME also holds whatever wall-clock value it is
			// given verbatim, with no session-timezone reinterpretation on
			// read -- see background.go's comment on why the comparison binds
			// a Go-computed `now` rather than using MySQL's own NOW(), which
			// is session-timezone-dependent and would not agree with it.
			UpMySQL: `
				CREATE TABLE IF NOT EXISTS tenant_trials (
					tenant_id  CHAR(36) NOT NULL,
					expires_at DATETIME(6) NOT NULL,
					handled    BOOLEAN NOT NULL DEFAULT false,
					PRIMARY KEY (tenant_id)
				);
			`,
			UpMSSQL: `
				IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'tenant_trials')
				CREATE TABLE tenant_trials (
					tenant_id  UNIQUEIDENTIFIER NOT NULL,
					expires_at DATETIMEOFFSET NOT NULL,
					handled    BIT NOT NULL DEFAULT 0,
					PRIMARY KEY (tenant_id)
				);
			`,
			Down: `
				DROP TABLE IF EXISTS tenant_trials;
			`,
			// Declared on Version 1 directly rather than a later version like
			// kvstore's TenantScoped migration: this table is new, so there is
			// no already-deployed v1 to leave unprotected the way kvstore's
			// history forced a separate version.
			TenantScoped: []string{"tenant_trials"},
		},
	}
}
