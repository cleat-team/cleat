package engine

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"
)

// TenantEgressStore reads a tenant's egress allowlist, with a short cache.
//
// cleat#1565. The guard consults this on every DIAL, including every redirect
// hop, so an uncached read would put a database round-trip in front of each
// one. The cache is what makes a per-tenant policy affordable at the point it
// has to be enforced.
//
// A MISS IS NOT A GRANT. Every failure path here returns an error rather than
// an empty list, because an empty list is a meaningful answer -- it denies
// everything -- and a database that is briefly unreachable must not be
// indistinguishable from a tenant that has configured nothing. The caller
// refuses on error, so both deny; the difference is that one says why.
type TenantEgressStore struct {
	DB      *sql.DB
	Dialect Dialect

	// TTL bounds how stale a decision may be. Zero means the default.
	//
	// Short on purpose: this is the window in which a REVOKED host is still
	// reachable. Lengthening it trades a security property for a query rate,
	// which is the wrong direction for the one table whose whole job is to say
	// what a guest may reach.
	TTL time.Duration

	now func() time.Time // tests

	mu    sync.Mutex
	cache map[string]cachedAllowlist
}

type cachedAllowlist struct {
	list *HostAllowlist
	at   time.Time
}

const defaultEgressCacheTTL = 30 * time.Second

func (s *TenantEgressStore) ttl() time.Duration {
	if s.TTL > 0 {
		return s.TTL
	}
	return defaultEgressCacheTTL
}

func (s *TenantEgressStore) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// The two statements, spelled out COMPLETE rather than built by concatenating
// a table name in.
//
// Concatenation here trips gosec's G202, and the honest fix is not a
// suppression: there are exactly two table names and no reason to assemble
// either at run time. MySQL has no schemas, so its administrative tables are
// unprefixed. Same shape as cmd/cleatctl's plugin.Query arms for the same
// table, and as engine/mssql_schedules.go, which this repository already
// settled on for the identical problem.
const (
	egressSelectAdmin = `SELECT host FROM admin.tenant_egress_allow WHERE tenant_id = `
	egressSelectPlain = `SELECT host FROM tenant_egress_allow WHERE tenant_id = `
)

// selectHosts is the statement for this dialect, with its placeholder.
func (s *TenantEgressStore) selectHosts() string {
	if s.Dialect == DialectMySQL {
		return egressSelectPlain + s.Dialect.placeholder(1)
	}
	return egressSelectAdmin + s.Dialect.placeholder(1)
}

// For returns the tenant's allowlist, from cache when fresh.
func (s *TenantEgressStore) For(ctx context.Context, tenantID string) (*HostAllowlist, error) {
	if tenantID == "" {
		// Not an empty list: no tenant means there is nobody whose policy this
		// would be. Reported so the refusal says that rather than "not on the
		// allowlist", which would send someone to edit a list that is not the
		// problem.
		return nil, fmt.Errorf("egress allowlist: no tenant in context")
	}

	s.mu.Lock()
	if c, ok := s.cache[tenantID]; ok && s.clock().Sub(c.at) < s.ttl() {
		s.mu.Unlock()
		return c.list, nil
	}
	s.mu.Unlock()

	rows, err := s.DB.QueryContext(ctx, s.selectHosts(), tenantID)
	if err != nil {
		return nil, fmt.Errorf("egress allowlist for tenant %s: %w", tenantID, err)
	}
	defer func() { _ = rows.Close() }()

	var hosts []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, fmt.Errorf("egress allowlist for tenant %s: scan: %w", tenantID, err)
		}
		hosts = append(hosts, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("egress allowlist for tenant %s: %w", tenantID, err)
	}

	list := NewHostAllowlist(hosts...)
	s.mu.Lock()
	if s.cache == nil {
		s.cache = map[string]cachedAllowlist{}
	}
	s.cache[tenantID] = cachedAllowlist{list: list, at: s.clock()}
	s.mu.Unlock()
	return list, nil
}

// Invalidate drops a tenant's cached list, so a change takes effect at once in
// the process that made it. Other workers still wait out their TTL -- this is
// a cache, not a coordination mechanism, and pretending otherwise would be the
// more dangerous claim.
func (s *TenantEgressStore) Invalidate(tenantID string) {
	s.mu.Lock()
	delete(s.cache, tenantID)
	s.mu.Unlock()
}
