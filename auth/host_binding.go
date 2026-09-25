package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
	// Under the tenant it asks about, not on the bare pool: as the role that ships (cleat_app on
	// PostgreSQL, the app login on SQL Server) this table is scoped by a policy that reads the tenant
	// the CONNECTION carries, and a predicate does not stand in for it. An unscoped read raised
	// "cleat.tenant_id is not set" on PostgreSQL and matched no row on SQL Server, so with host binding
	// on, every authenticated request was answered 503 or 404. cleat#2258.
	err := s.scopedRead(ctx, want, func(q querier) error {
		return q.QueryRowContext(ctx, tenantForHostStmt(s.dialect), hostname, want.String()).Scan(&one)
	})
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

// CountTenantDomains reports whether any hostname mapping exists across all tenants, as a count that is
// EXACT ONLY AT ZERO: it stops at the first tenant that has a row and returns that tenant's count, since
// the one caller asks only whether it is zero and the walk is otherwise one query per tenant (measured
// about a millisecond each, 9.9s at 10,000 tenants, which is startup-probe territory over a network).
// Only the refusal case, no domain anywhere, reads every tenant.
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
// It is asked before any request exists, so it has no tenant of its own. It therefore reads each
// tenant's rows under THAT tenant, and adds them up. This used to be one unscoped SELECT under a comment
// saying it "must run on a connection the policy does not apply to, which the worker's startup connection
// is". That is true of a superuser and false of cleat_app, the role a deployment is told to run as: there
// the read raised "cleat.tenant_id is not set" (P0001) on PostgreSQL, and on SQL Server the security policy
// filtered every row and it returned 0, which the boot check reports as "tenant_domains is empty". So a
// worker that followed the docs could not start with host binding on. cleat#2258.
//
// The tenants are enumerated from admin.tenants, which needs no exemption for cleat_app, and every tenant
// counts, suspended included: the question is whether ANY hostname is registered. This is the same shape
// as engine.SecretStore.CountSecrets (cleat#2123), and for the same reason it does not use a cross-tenant
// role or marker: it needs no grant beyond the ones the worker already has, and it does not widen what the
// role can read. MySQL is a database per tenant, so the base database's table is the whole answer.
func (s *TenantStore) CountTenantDomains(ctx context.Context) (int, error) {
	if s.dialect == DialectMySQL {
		var n int
		if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM tenant_domains`).Scan(&n); err != nil {
			return 0, err
		}
		return n, nil
	}
	ids, err := s.allTenantIDs(ctx)
	if err != nil {
		return 0, err
	}
	total := 0
	for _, id := range ids {
		var n int
		if err := s.scopedRead(ctx, id, func(q querier) error {
			return q.QueryRowContext(ctx, `SELECT count(*) FROM tenant_domains`).Scan(&n)
		}); err != nil {
			return 0, fmt.Errorf("count tenant_domains for tenant %s: %w", id, err)
		}
		total += n
		if total > 0 {
			return total, nil
		}
	}
	return total, nil
}

// allTenantIDs lists every tenant, suspended or not. The statements are literals at the call site, which
// is what the guard that reads query strings out of the source needs in order to check them.
func (s *TenantStore) allTenantIDs(ctx context.Context) ([]uuid.UUID, error) {
	var rows *sql.Rows
	var err error
	if s.dialect == DialectMSSQL {
		rows, err = s.db.QueryContext(ctx, `SELECT CONVERT(varchar(36), tenant_id) FROM admin.tenants`)
	} else {
		rows, err = s.db.QueryContext(ctx, `SELECT tenant_id FROM admin.tenants`)
	}
	if err != nil {
		return nil, fmt.Errorf("list tenants: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []uuid.UUID
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("list tenants: %w", err)
		}
		// The canonical form, whatever the database printed: SQL Server returns a UNIQUEIDENTIFIER in
		// upper case, and a tenant id is compared and bound as uuid.UUID.String() everywhere else.
		id, perr := uuid.Parse(raw)
		if perr != nil {
			return nil, fmt.Errorf("tenant id %q is not a UUID: %w", raw, perr)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list tenants: %w", err)
	}
	return out, nil
}

// querier is the subset of *sql.DB and *sql.Tx a scoped read needs.
type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// scopedRead runs fn on a connection that carries tenant, where the dialect scopes by the connection.
//
// PostgreSQL: a transaction with set_config('cleat.tenant_id', ..., true), which reverts with the
// transaction and cannot follow the connection back into the pool. SQL Server: sp_set_session_context
// inside a transaction, exactly as the engine does (engine.setTenantOnTx). MySQL scopes by database, so the
// read is direct.
func (s *TenantStore) scopedRead(ctx context.Context, tenant uuid.UUID, fn func(q querier) error) error {
	if s.dialect == DialectMySQL {
		return fn(s.db)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if s.dialect == DialectMSSQL {
		_, err = tx.ExecContext(ctx, `EXEC sp_set_session_context @key = N'tenant_id', @value = @p1`, tenant.String())
	} else {
		_, err = tx.ExecContext(ctx, `SELECT set_config('cleat.tenant_id', $1, true)`, tenant.String())
	}
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("scope the read to tenant %s: %w", tenant, err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}
