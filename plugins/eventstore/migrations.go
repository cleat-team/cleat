package eventstore

import "github.com/cleat-team/cleat/plugin"

// Migrations returns the database schema for event streams. Tables are
// idempotent (IF NOT EXISTS) and safe to run multiple times.
func (p *Plugin) Migrations() []plugin.Migration {
	return []plugin.Migration{
		{
			Version: 1,
			Up: `
				CREATE TABLE IF NOT EXISTS event_stream (
					tenant_id   UUID NOT NULL,
					stream_id   TEXT NOT NULL,
					sequence    BIGINT NOT NULL,
					event       JSONB NOT NULL,
					created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
					PRIMARY KEY (tenant_id, stream_id, sequence)
				);

				CREATE INDEX IF NOT EXISTS idx_event_stream_lookup
					ON event_stream (tenant_id, stream_id, sequence);
			`,
			UpMySQL: `
				CREATE TABLE IF NOT EXISTS event_stream (
					tenant_id   CHAR(36) NOT NULL,
					stream_id   VARCHAR(255) NOT NULL,
					sequence    BIGINT NOT NULL,
					event       JSON NOT NULL,
					created_at  TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
					PRIMARY KEY (tenant_id, stream_id, sequence)
				);

				CREATE INDEX idx_event_stream_lookup
					ON event_stream (tenant_id, stream_id, sequence);
			`,
			UpMSSQL: `
				IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'event_stream')
				CREATE TABLE event_stream (
					tenant_id   UNIQUEIDENTIFIER NOT NULL,
					stream_id   NVARCHAR(255) NOT NULL,
					sequence    BIGINT NOT NULL,
					event       NVARCHAR(MAX) NOT NULL,
					created_at  DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME(),
					PRIMARY KEY (tenant_id, stream_id, sequence)
				);

				IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'idx_event_stream_lookup' AND object_id = OBJECT_ID('event_stream'))
				CREATE INDEX idx_event_stream_lookup
					ON event_stream (tenant_id, stream_id, sequence);
			`,
			Down: `
				DROP TABLE IF EXISTS event_stream;
			`,
		},
		{
			// Tenant isolation for event_stream. cleat#1512.
			//
			// A NEW VERSION, NEVER AN EDIT TO v1. A recorded migration never
			// runs again, so editing v1 would protect databases created after
			// this lands and leave every existing one open.
			//
			// Up and Down are empty on purpose. The runtime emits ENABLE /
			// FORCE / the policy from TenantScoped (plugin.applyTenantScoping),
			// using cleat.tenant_row_is_visible so a sweep that named itself
			// through plugin.AcrossAllTenants is admitted and an unmarked one
			// still fails closed. There is no statement for an author to write
			// and no policy an author could drop -- the runtime owns it. On
			// MySQL and SQL Server this version is recorded and installs
			// nothing, which is what the field means.
			//
			// EVERY ACCESS SITE MUST CARRY A TENANT, because the policy calls
			// cleat.assert_tenant_set(), which RAISEs when none is in scope.
			// There are five, and they fall into three kinds rather than the
			// two cleat#1512's table implies:
			//
			//   - the cleanup in background.go, which is genuinely global and
			//     says so via plugin.AcrossAllTenants;
			//   - three handlers in routes.go on r.Context(), which the auth
			//     middleware has already populated (auth/middleware.go:108);
			//   - the poll loop inside handleSSE, which LOOKS like a background
			//     loop -- a 1s ticker inside for/select -- and is not. Its ctx
			//     is r.Context(), so it already carries the tenant. Marking it
			//     would silently widen every SSE read to every tenant, which is
			//     the class of answer this mechanism exists to make impossible.
			//
			// eventstore registers no host calls, so the cleat#1492 path that
			// featureflags needed does not arise here.
			Version:      2,
			TenantScoped: []string{"event_stream"},
		},
	}
}
