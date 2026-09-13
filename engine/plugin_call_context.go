package engine

import (
	"context"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/internal/tenantctx"
	"github.com/cleat-team/cleat/plugin"
)

// pluginCallContext builds the context a plugin function is invoked with.
// cleat#1278.
//
// ONE FUNCTION BECAUSE THERE ARE TWO CALL SITES, and the second is the reason
// this exists rather than a third copy of the same eleven lines. PluginCall and
// PluginCallStreaming each built this block inline, identically, and a tenant
// added to one of them would have shipped a plugin that is scoped over a unary
// call and unscoped over a streaming one -- with no test able to tell, because
// every test drives one path or the other. See
// TestBothPluginCallPathsCarryTheTenant, which drives both.
//
// THE TENANT IS CARRIED TWICE, DELIBERATELY, AND THE TWO ARE NOT REDUNDANT.
//
//   - plugin.CallContext.TenantID is a string field a plugin reads when it
//     wants to know whose workflow it is running for. It is advisory: a plugin
//     that ignores it still works.
//   - tenantctx is what engine/plugindb_tenant.go's beginTenantTx reads, and
//     it is what puts `cleat.tenant_id` on the connection running the
//     statement. Without it no row-level policy can be satisfied.
//
// Before this, only the first was set. The value had already arrived -- it was
// on the session, one line above -- and was simply not in the carrier the
// database gate reads. The measured consequence, on a table carrying the
// wrapper policy plugin/migration.go:500 creates, read by a role that does not
// bypass RLS:
//
//	empty table, read    n=0  err=<nil>   <- succeeds, and means nothing:
//	                                         a USING clause over zero rows is
//	                                         never evaluated (cleat#1285)
//	table with rows,read n=0  err=pq: cleat.tenant_id is not set (P0001)
//	write                     err=pq: cleat.tenant_id is not set (P0001)
//
// So this is a FIX, not a tightening: a plugin reaching a policied table over a
// host call was already failing, and failing only once the table was non-empty,
// which is the shape that survives a test suite.
//
// AN UNPARSEABLE TENANT IS LOGGED, NOT SWALLOWED AND NOT FATAL. Every tenant in
// this system is a UUID string -- engine.DefaultTenantUUID is the zero UUID and
// is a real tenant with real rows, not a sentinel, so it is bridged like any
// other. A value that will not parse means something upstream is wrong, and the
// two bad answers are opposite: failing the call turns a logging-level defect
// into an outage, and dropping it silently reintroduces exactly the gap this
// function closes, invisibly. Warn and continue unscoped, which is what the
// call did before this existed.
func (s *execSession) pluginCallContext(ctx context.Context) context.Context {
	cc := &plugin.CallContext{}
	if s.tenantID != "" {
		cc.TenantID = s.tenantID
	}
	if s.workflowID != "" {
		cc.WorkflowID = s.workflowID
	}
	if s.engine.db != nil {
		cc.DB = s.engine.db
	}
	ctx = plugin.WithCallContext(ctx, cc)

	if s.tenantID == "" {
		return ctx
	}
	tid, err := uuid.Parse(s.tenantID)
	if err != nil {
		s.engine.log().WarnContext(ctx,
			"the workflow's tenant is not a UUID, so plugin statements on this call run unscoped "+
				"and any row-level policy on a plugin table will refuse them",
			"workflow_id", s.workflowID, "tenant_id", s.tenantID, "error", err)
		return ctx
	}
	return tenantctx.With(ctx, tid)
}
