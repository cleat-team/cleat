package auditlog

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// recordAudit writes one audit event now, with a single attempt, and is how tests put rows on a
// chain. It is here rather than in middleware.go because nothing in production calls it any more:
// the queue's workers call recordOnce, and a non-test method that only tests reach is what
// scripts/check-test-only-code.sh refuses. A failure is counted and logged (lose), not swallowed.
func (p *Plugin) recordAudit(ctx context.Context, tenantID uuid.UUID, userID, method, path string, statusCode int, ipAddress, userAgent string, duration time.Duration) {
	p.recordOnce(newQueuedEvent(tenantID, userID, method, path, statusCode, ipAddress, userAgent, duration))
}
