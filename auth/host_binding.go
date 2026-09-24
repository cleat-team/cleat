package auth

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"
)

// DomainResolver turns a hostname into the tenant that owns it.
//
// Narrow on purpose, in the same spirit as TenantResolver above it: the
// middleware needs one question answered and nothing else, so a store that
// cannot answer anything else can implement it.
//
// Implementations MUST carry their own `AND tenant_id = ?` predicate rather
// than relying on a row-level policy to filter. All three dialects do scope
// tenant_domains -- PostgreSQL by RLS policy, SQL Server by a SECURITY POLICY
// filter predicate, MySQL by putting each tenant in its own database -- so the
// predicate is belt and braces rather than the only protection. It is still
// required: a lookup that is correct only because some other layer happens to
// be filtering is the defect shape this repository keeps meeting, which is why
// the featureflags suite asserts its rows are scoped by a policy and NOT ONLY
// by the query.
//
// An earlier version of this comment said SQL Server had no policy at all,
// citing engine/mssql_lifecycle.go. That file says the predicate is the whole
// of the scoping for ONE query; it is not a statement about the dialect.
type DomainResolver interface {
	// TenantForHost returns the tenant owning hostname, scoped to want.
	// found is false when no row matches -- which must be the answer both for
	// a hostname nobody owns and for one owned by a different tenant.
	TenantForHost(ctx context.Context, hostname string, want uuid.UUID) (found bool, err error)
}

// NormalizeHost strips the port and lowercases a Host header value.
//
// Exported because the table's CHECK constraints forbid both an uppercase
// letter and a colon, so whoever inserts a row needs the identical
// normalisation -- a row that cannot match a normalised lookup presents as
// "my domain is configured and cleat refuses it", which is a bad hour.
//
// net.SplitHostPort is deliberately NOT used: it errors on a bare hostname,
// which is the common case, and it would have to be called speculatively. An
// IPv6 literal arrives bracketed ("[::1]:8080"), so the last colon is the port
// separator only when it follows the closing bracket.
func NormalizeHost(host string) string {
	h := strings.TrimSpace(host)
	if i := strings.LastIndex(h, "]"); i >= 0 {
		// Bracketed IPv6. Keep the brackets: they are part of how the
		// authority is written, and a row would be configured that way.
		if j := strings.Index(h[i:], ":"); j >= 0 {
			h = h[:i+j]
		}
	} else if strings.Count(h, ":") == 1 {
		// EXACTLY ONE COLON, which is what separates host:port from a bare
		// IPv6 address -- an unbracketed IPv6 literal always has more.
		//
		// An earlier version stripped at the LAST colon whenever what followed
		// was all digits, and the test below caught it turning "2001:db8::1"
		// into "2001:db8:". A bare address ending in a numeric group is not a
		// rare input, and the mangled value would never match a configured row,
		// presenting as "my domain is configured and cleat refuses it".
		if i := strings.Index(h, ":"); i >= 0 {
			h = h[:i]
		}
	}
	return strings.ToLower(h)
}

// HostBindingMiddleware refuses a request whose Host does not belong to the
// authenticated tenant.
//
// THE RULE IT IMPLEMENTS. An API key is proved; a Host header is asserted. The
// tenant comes from the credential -- which auth.Middleware has already put in
// the context by the time this runs -- and the Host is then checked against it.
// Without that check, tenant A's valid key works against tenant B's URL, which
// is a confused deputy and the seed of multi-tenant cache poisoning.
//
// MUST BE INSTALLED INSIDE auth.Middleware, so the context tenant is the
// credential-derived one. Installed outside, it would read whatever the
// tenant-resolver middleware put there -- which on some paths is a value the
// CLIENT supplied -- and would then be checking a header against a header.
//
// exempt paths are skipped entirely. Two kinds qualify, and both are named
// rather than inferred:
//
//   - the infrastructure paths (IsInfrastructurePath), addressed by infrastructure rather than by a
//     tenant hostname. A load balancer probing a worker by IP has no tenant
//     URL to present.
//   - auth.Middleware's public patterns, which are reached by third parties
//     holding no cleat credential -- an inbound webhook, an IdP's OAuth
//     redirect. There is no authenticated tenant on those requests, so there
//     is nothing to compare a Host against.
//
// A request with no tenant in context is PASSED THROUGH rather than refused.
// That is not a hole: with --require-auth (the default) such a request has
// already been answered 401 by auth.Middleware and never reaches here. Refusing
// it here instead would make this middleware the thing that breaks
// --require-auth=false deployments, which is a separate decision from the one
// this implements.
func HostBindingMiddleware(resolver DomainResolver, publicPatterns ...string) func(http.Handler) http.Handler {
	publicMatcher := buildPublicMatcher(publicPatterns)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			path := r.URL.Path
			if IsInfrastructurePath(path) {
				next.ServeHTTP(w, r)
				return
			}
			if publicMatcher != nil {
				if _, pattern := publicMatcher.Handler(r); pattern != "" {
					next.ServeHTTP(w, r)
					return
				}
			}

			tenantID, ok := TenantIDFromContext(r.Context())
			if !ok {
				next.ServeHTTP(w, r)
				return
			}

			host := NormalizeHost(r.Host)
			if host == "" {
				refuseHost(w)
				return
			}

			found, err := resolver.TenantForHost(r.Context(), host, tenantID)
			if err != nil {
				// Fail CLOSED. This middleware exists to stop one tenant
				// reaching another's URL, and a database error is not a reason
				// to stop doing that. Contrast the rate limiter, which fails
				// open deliberately because its job is availability-shaped;
				// this one's job is not.
				http.Error(w, `{"error":"host binding unavailable"}`, http.StatusServiceUnavailable)
				return
			}
			if !found {
				refuseHost(w)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// refuseHost answers the SAME way for "no tenant owns this hostname" and "this
// hostname belongs to a different tenant".
//
// Distinguishing them would turn this route into an oracle for which hostnames
// are registered, and therefore for which tenants exist -- the same argument
// handleDeadLetterTerminate makes for workflow ids, where answering 404 for a
// foreign id and 404 for a missing one is deliberate. The message names no
// hostname for the same reason.
func refuseHost(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	http.Error(w, `{"error":"this host is not configured for your tenant"}`, http.StatusNotFound)
}

// TenantForHost implements DomainResolver on the tenant store.
//
// The predicate carries `AND tenant_id = ?` UNCONDITIONALLY, on every dialect,
// rather than reasoning about which of them has a policy. Each does scope this
// table by its own mechanism (see the DomainResolver doc above), so writing the
// predicate everywhere removes the question of which layer is holding the check
// up rather than answering it differently per dialect.
//
// It returns only whether a row matched. The caller does not need the tenant
// back -- it already has the authenticated one, and returning the owner of a
// hostname it does not own is the oracle this design is avoiding.
func (s *TenantStore) TenantForHost(ctx context.Context, hostname string, want uuid.UUID) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, tenantForHostStmt(s.dialect), hostname, want.String()).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		// No row is the answer for BOTH "nobody owns this hostname" and "another
		// tenant owns it". They are deliberately indistinguishable here, and
		// HostBindingMiddleware answers identically for each.
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func tenantForHostStmt(dialect string) string {
	switch dialect {
	case DialectMySQL:
		return `SELECT 1 FROM tenant_domains WHERE hostname = ? AND tenant_id = ?`
	case DialectMSSQL:
		return `SELECT 1 FROM tenant_domains WHERE hostname = @p1 AND tenant_id = @p2`
	default:
		return `SELECT 1 FROM tenant_domains WHERE hostname = $1 AND tenant_id = $2`
	}
}

// CountTenantDomains reports how many hostname mappings exist across all
// tenants.
//
// It exists for ONE caller: the worker's startup check. Enabling host binding
// on a deployment that has configured no domains would refuse every
// authenticated request at runtime, which presents as "cleat is broken" rather
// than as "this is misconfigured". Refusing to start says which it is.
//
// That is the lesson from cleat#1581, where the rate limiter warns and
// downgrades instead of refusing, and an operator who asked for a cluster-wide
// limit silently gets a per-process one. checkConnectionBudget is the
// precedent going the other way: a budget the fixed pools cannot honour is
// refused at boot rather than breached under load.
//
// Deliberately NOT tenant-scoped -- it is asked before any request exists and
// therefore has no tenant. On PostgreSQL that means it must run on a connection
// the policy does not apply to, which the worker's startup connection is.
func (s *TenantStore) CountTenantDomains(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM tenant_domains`).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}
