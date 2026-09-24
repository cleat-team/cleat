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
		{
			// The per-tenant hash chain (cleat#2047). See chain.go for what is hashed.
			//
			// seq, prev_hash and row_hash are NULLABLE: a row written before this
			// version has none, and is reported by verify as unchained rather than
			// broken. 0.3.0 needs a fresh database, so on a real deployment they are
			// filled for every row.
			//
			// UNIQUE (tenant_id, seq) is what makes a forked chain impossible to
			// store: two appenders that both read the same head cannot both insert
			// seq+1. PostgreSQL and MySQL let a unique index hold many NULLs, so the
			// unchained rows do not collide there; SQL Server treats NULLs as equal,
			// so its index is filtered to chained rows.
			//
			// audit_chain_heads has one row per tenant: the last seq and hash, and the
			// floor. The head row is also the per-tenant lock (SELECT ... FOR UPDATE /
			// UPDLOCK), which is why it is a table and not an advisory lock: a chain
			// alone cannot see that its last rows were deleted, and the head can.
			// floor_seq / floor_hash record what retention removed, so that a deleted
			// prefix is a recorded fact and not a hole.
			Version: 3,
			Up: `
				ALTER TABLE audit_events ADD COLUMN IF NOT EXISTS seq BIGINT;
				ALTER TABLE audit_events ADD COLUMN IF NOT EXISTS prev_hash CHAR(64);
				ALTER TABLE audit_events ADD COLUMN IF NOT EXISTS row_hash CHAR(64);

				CREATE UNIQUE INDEX IF NOT EXISTS idx_audit_events_tenant_seq
					ON audit_events (tenant_id, seq) WHERE seq IS NOT NULL;

				CREATE TABLE IF NOT EXISTS audit_chain_heads (
					tenant_id  UUID PRIMARY KEY,
					seq        BIGINT NOT NULL,
					hash       CHAR(64) NOT NULL,
					floor_seq  BIGINT NOT NULL DEFAULT 0,
					floor_hash CHAR(64) NOT NULL DEFAULT '0000000000000000000000000000000000000000000000000000000000000000'
				);
			`,
			UpMySQL: `
				ALTER TABLE audit_events
					ADD COLUMN seq BIGINT NULL,
					ADD COLUMN prev_hash CHAR(64) NULL,
					ADD COLUMN row_hash CHAR(64) NULL;

				CREATE UNIQUE INDEX idx_audit_events_tenant_seq ON audit_events (tenant_id, seq);

				CREATE TABLE IF NOT EXISTS audit_chain_heads (
					tenant_id  CHAR(36) PRIMARY KEY,
					seq        BIGINT NOT NULL,
					hash       CHAR(64) NOT NULL,
					floor_seq  BIGINT NOT NULL DEFAULT 0,
					floor_hash CHAR(64) NOT NULL DEFAULT '0000000000000000000000000000000000000000000000000000000000000000'
				);
			`,
			UpMSSQL: `
				IF COL_LENGTH('audit_events', 'seq') IS NULL
					ALTER TABLE audit_events ADD seq BIGINT NULL, prev_hash CHAR(64) NULL, row_hash CHAR(64) NULL;

				IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'idx_audit_events_tenant_seq' AND object_id = OBJECT_ID('audit_events'))
				CREATE UNIQUE INDEX idx_audit_events_tenant_seq ON audit_events (tenant_id, seq) WHERE seq IS NOT NULL;

				IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'audit_chain_heads')
				CREATE TABLE audit_chain_heads (
					tenant_id  UNIQUEIDENTIFIER PRIMARY KEY,
					seq        BIGINT NOT NULL,
					hash       CHAR(64) NOT NULL,
					floor_seq  BIGINT NOT NULL DEFAULT 0,
					floor_hash CHAR(64) NOT NULL DEFAULT '0000000000000000000000000000000000000000000000000000000000000000'
				);
			`,
			Down: `
				DROP TABLE IF EXISTS audit_chain_heads;
			`,
			TenantScoped: []string{"audit_chain_heads"},
		},
	}
}
