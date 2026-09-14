package auditlog

import "github.com/cleat-team/cleat/plugin"

// Migrations returns the database schema for audit events.
func (p *Plugin) Migrations() []plugin.Migration {
	return []plugin.Migration{
		{
			Version: 1,
			Up: `
				CREATE TABLE IF NOT EXISTS audit_events (
					id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
					tenant_id   UUID NOT NULL,
					timestamp   TIMESTAMPTZ NOT NULL DEFAULT now(),
					method      TEXT NOT NULL,
					path        TEXT NOT NULL,
					status_code INTEGER,
					user_id     TEXT,
					ip_address  TEXT,
					user_agent  TEXT,
					duration_ms INTEGER,
					metadata    JSONB DEFAULT '{}'
				);

				CREATE INDEX IF NOT EXISTS idx_audit_events_tenant_ts
					ON audit_events (tenant_id, timestamp DESC);
				CREATE INDEX IF NOT EXISTS idx_audit_events_tenant_path
					ON audit_events (tenant_id, path);
				CREATE INDEX IF NOT EXISTS idx_audit_events_tenant_method
					ON audit_events (tenant_id, method);
			`,
			UpMySQL: `
				CREATE TABLE IF NOT EXISTS audit_events (
					id          CHAR(36) PRIMARY KEY,
					tenant_id   CHAR(36) NOT NULL,
					` + "`" + `timestamp` + "`" + ` TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
					method      VARCHAR(255) NOT NULL,
					path        VARCHAR(700) NOT NULL,
					status_code INT,
					user_id     TEXT,
					ip_address  TEXT,
					user_agent  TEXT,
					duration_ms INT,
					metadata    JSON DEFAULT ('{}')
				);

				CREATE INDEX idx_audit_events_tenant_ts
					ON audit_events (tenant_id, ` + "`" + `timestamp` + "`" + ` DESC);
				CREATE INDEX idx_audit_events_tenant_path
					ON audit_events (tenant_id, path);
				CREATE INDEX idx_audit_events_tenant_method
					ON audit_events (tenant_id, method);
			`,
			UpMSSQL: `
				IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'audit_events')
				CREATE TABLE audit_events (
					id          UNIQUEIDENTIFIER PRIMARY KEY DEFAULT NEWID(),
					tenant_id   UNIQUEIDENTIFIER NOT NULL,
					[timestamp] DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME(),
					method      NVARCHAR(255) NOT NULL,
					path        NVARCHAR(900) NOT NULL,
					status_code INT,
					user_id     NVARCHAR(MAX),
					ip_address  NVARCHAR(MAX),
					user_agent  NVARCHAR(MAX),
					duration_ms INT,
					metadata    NVARCHAR(MAX) DEFAULT ('{}')
				);

				IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'idx_audit_events_tenant_ts' AND object_id = OBJECT_ID('audit_events'))
				CREATE INDEX idx_audit_events_tenant_ts ON audit_events (tenant_id, [timestamp] DESC);
				IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'idx_audit_events_tenant_path' AND object_id = OBJECT_ID('audit_events'))
				CREATE INDEX idx_audit_events_tenant_path ON audit_events (tenant_id, path);
				IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'idx_audit_events_tenant_method' AND object_id = OBJECT_ID('audit_events'))
				CREATE INDEX idx_audit_events_tenant_method ON audit_events (tenant_id, method);
			`,
			Down: `
				DROP TABLE IF EXISTS audit_events;
			`,
		},
		{
			// Tenant isolation for audit_events. cleat#1278.
			//
			// A separate version rather than a TenantScoped on v1, for the
			// reason kvstore's v2 gives: v1 is already recorded as applied
			// wherever auditlog runs, and a recorded migration never runs
			// again -- editing it would protect new databases and leave every
			// existing one open.
			//
			// WHAT MAKES THIS ONE DIFFERENT FROM kvstore, AND WHY IT IS THE
			// TEMPLATE TO COPY. kvstore qualified because every one of its
			// access sites is request-scoped; its own comment says a plugin
			// that also sweeps from a background loop "cannot adopt this yet".
			// auditlog does sweep, and adopts it anyway, because the policy is
			// not the obstacle -- an unnamed sweep is. The two non-request
			// writers were resolved in OPPOSITE directions, and telling them
			// apart is the whole judgement:
			//
			//	cleanupRetention  deletes by timestamp for everyone. Genuinely
			//	                  cross-tenant, so it names itself with
			//	                  plugin.AcrossAllTenants and the policy lifts.
			//
			//	recordAudit       writes ONE tenant's row and had the tenant in
			//	                  hand the whole time -- it built its insert
			//	                  context from context.Background() to survive a
			//	                  cancelled request, and dropped the tenant on
			//	                  the way. That is a LOST tenant, not a
			//	                  cross-tenant operation. It carries the tenant
			//	                  now.
			//
			// Bypassing in the second case would have worked, passed every
			// test, and silently disabled isolation for every audit write --
			// which is the failure this mechanism exists to make impossible.
			// Reach for AcrossAllTenants only when there is no tenant to be
			// had, never when there is one that went missing.
			//
			// Up is empty on purpose: the policy is emitted by the runtime from
			// TenantScoped. On MySQL and SQL Server this version is recorded
			// and does nothing, which is what the field documents.
			Version:      2,
			TenantScoped: []string{"audit_events"},
		},
	}
}
