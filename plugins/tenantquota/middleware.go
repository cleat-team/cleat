package tenantquota

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/plugin"
)

var errNoDatabase = errors.New(
	"tenant-quota: no database is configured -- refusing to start rather than " +
		"running with every quota silently unenforced")

// quota is one tenant's limit for one resource.
type quota struct {
	limitCount    int64
	windowSeconds int
	enforce       bool
}

// Middleware meters workflow starts and, where a quota says so, refuses them.
//
// # Why this sees a core route at all
//
// cmd/cleat-worker/main.go registers the core route table on the PLUGIN mux
// (`mux := plugMux`, then `registerRoutes(mux, api)`) and serves `plugHandler`,
// which is that mux wrapped in each plugin's Middleware. So this sees
// POST /api/workflows/:name/start. The wiring reads the other way at a glance
// -- `plugHandler = p.Middleware(plugHandler)` looks plugin-only -- which is
// worth knowing before concluding a quota needs core changes.
//
// # Counting happens AFTER the handler, and only for an accepted start
//
// The obvious order is to count first and then serve. That charges a tenant
// for starts the API rejected -- a malformed body, an unknown workflow, a
// failed idempotency precondition -- so a client looping on a 400 would exhaust
// a month's quota without ever starting a workflow. The status is recorded and
// only 2xx increments.
//
// The cost of that choice, stated rather than hidden: the check and the
// increment are not atomic, so concurrent starts can both observe the same
// under-limit total and both proceed. A quota is an accounting bound over
// hours or days, not a mutual-exclusion primitive; overshooting by the number
// of simultaneously in-flight starts is acceptable where refusing a legitimate
// start is not. plugins/ratelimiter makes the opposite trade for the opposite
// reason.
//
// # Failing open
//
// A quota that cannot be read does not refuse. The fallback direction matches
// what engine.tenantSettings does with an unreadable settings row: an
// infrastructure problem in the metering path should not take down the ability
// to start workflows. It is recorded, not silent.
func (p *Plugin) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isWorkflowStart(r) {
			next.ServeHTTP(w, r)
			return
		}
		tid, ok := auth.TenantIDFromContext(r.Context())
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		ctx := r.Context()

		q, found, err := p.quotaFor(ctx, tid, ResourceWorkflowStarts)
		if err != nil {
			p.logger.WarnContext(ctx, "tenant-quota: reading the quota failed; allowing the start",
				"tenant_id", tid, "resource", ResourceWorkflowStarts, "error", err)
			next.ServeHTTP(w, r)
			return
		}
		if !found {
			// No quota configured for this tenant and resource. Nothing to
			// meter: counting unconditionally would fill the counter table for
			// every deployment that never asked for quotas.
			next.ServeHTTP(w, r)
			return
		}

		used, err := p.usage(ctx, tid, ResourceWorkflowStarts, q.windowSeconds)
		if err != nil {
			p.logger.WarnContext(ctx, "tenant-quota: reading usage failed; allowing the start",
				"tenant_id", tid, "resource", ResourceWorkflowStarts, "error", err)
			next.ServeHTTP(w, r)
			return
		}

		if used >= q.limitCount {
			if q.enforce {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("X-Quota-Limit", itoa(q.limitCount))
				w.Header().Set("X-Quota-Used", itoa(used))
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"error":"tenant quota exceeded","resource":"` +
					ResourceWorkflowStarts + `"}`))
				p.logger.WarnContext(ctx, "tenant-quota: refused a start over quota",
					"tenant_id", tid, "resource", ResourceWorkflowStarts,
					"used", used, "limit", q.limitCount)
				return
			}
			// SOFT: over the limit, recorded, not refused. This is the default
			// and the reason the counting half can be trusted before anything
			// refuses on it.
			p.logger.WarnContext(ctx, "tenant-quota: over quota, not enforced",
				"tenant_id", tid, "resource", ResourceWorkflowStarts,
				"used", used, "limit", q.limitCount)
		}

		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		if rec.status >= 200 && rec.status < 300 {
			if err := p.increment(ctx, tid, ResourceWorkflowStarts); err != nil {
				// The start already happened; losing the increment undercounts
				// rather than breaking anything, so it is logged and not
				// surfaced to the caller.
				p.logger.WarnContext(ctx, "tenant-quota: could not record a start",
					"tenant_id", tid, "resource", ResourceWorkflowStarts, "error", err)
			}
		}
	})
}

// isWorkflowStart matches POST /api/workflows/:name/start, the route
// cmd/cleat-worker/server.go dispatches with
// `len(parts) == 2 && parts[1] == "start" && r.Method == http.MethodPost`.
func isWorkflowStart(r *http.Request) bool {
	if r.Method != http.MethodPost {
		return false
	}
	const prefix = "/api/workflows/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		return false
	}
	parts := strings.Split(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, prefix), "/"), "/")
	return len(parts) == 2 && parts[1] == "start"
}

// statusRecorder captures the status code so only an accepted start is counted.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.wroteHeader {
		s.status = code
		s.wroteHeader = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	s.wroteHeader = true // an implicit 200
	return s.ResponseWriter.Write(b)
}

// quotaFor reads one tenant's quota. found=false means none is configured,
// which is not an error.
func (p *Plugin) quotaFor(ctx context.Context, tid uuid.UUID, resource string) (quota, bool, error) {
	q := plugin.Rebind(`
		SELECT limit_count, window_seconds, enforce
		FROM tenant_quota WHERE tenant_id = $1 AND resource = $2
	`, p.dialect)
	var out quota
	row := p.db.QueryRow(ctx, q, tid, resource)
	if err := row.Scan(&out.limitCount, &out.windowSeconds, &out.enforce); err != nil {
		if isNoRows(err) {
			return quota{}, false, nil
		}
		return quota{}, false, err
	}
	return out, true, nil
}

// usage sums the buckets inside the rolling window.
func (p *Plugin) usage(ctx context.Context, tid uuid.UUID, resource string, windowSeconds int) (int64, error) {
	cutoff := time.Now().UTC().Add(-time.Duration(windowSeconds) * time.Second)
	q := plugin.Rebind(`
		SELECT COALESCE(SUM(count), 0) FROM tenant_quota_counter
		WHERE tenant_id = $1 AND resource = $2 AND bucket_start > $3
	`, p.dialect)
	var used int64
	if err := p.db.QueryRow(ctx, q, tid, resource, cutoff).Scan(&used); err != nil {
		return 0, err
	}
	return used, nil
}

// increment adds one to this hour's bucket.
//
// Two statements rather than a dialect-specific upsert: an INSERT that may
// collide, then an UPDATE. ON CONFLICT, ON DUPLICATE KEY and MERGE are three
// different spellings across the three dialects, and this plugin would need
// all three. The INSERT's duplicate-key error is the expected case after the
// first event in an hour, so it is tolerated rather than reported -- the same
// shape plugins/ratelimiter uses for rate_counter.
func (p *Plugin) increment(ctx context.Context, tid uuid.UUID, resource string) error {
	bucket := time.Now().UTC().Truncate(time.Hour)

	ins := plugin.Rebind(`
		INSERT INTO tenant_quota_counter (tenant_id, resource, bucket_start, count)
		VALUES ($1, $2, $3, 0)
	`, p.dialect)
	if _, err := p.db.Exec(ctx, ins, tid, resource, bucket); err != nil && !isDuplicateKey(err) {
		return err
	}

	upd := plugin.Rebind(`
		UPDATE tenant_quota_counter SET count = count + 1
		WHERE tenant_id = $1 AND resource = $2 AND bucket_start = $3
	`, p.dialect)
	_, err := p.db.Exec(ctx, upd, tid, resource, bucket)
	return err
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [24]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// isNoRows reports whether a scan found nothing.
//
// database/sql's sentinel, matched with errors.Is rather than by string: the
// plugin DB wraps the driver but propagates sql.ErrNoRows.
func isNoRows(err error) bool { return errors.Is(err, sql.ErrNoRows) }

// isDuplicateKey reports whether an INSERT collided with an existing row.
//
// DELIBERATELY DIALECT-BLIND, and that is a narrowing rather than a shortcut.
// plugins/ratelimiter's equivalent switches on the dialect and checks a
// per-dialect substring; this checks all of them at once because the ONLY
// INSERT it guards is the bucket row, whose collision is the expected case
// after the first event in an hour. A false positive here can only swallow an
// error on that one statement, and the UPDATE that follows reports any real
// failure. Matching a dialect wrongly -- which a per-dialect switch does
// silently when the dialect is misdetected -- would turn that expected
// collision into a returned error on every metered start.
func isDuplicateKey(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, s := range []string{"duplicate key", "23505", "duplicate entry", "1062", "primary key", "2627"} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}
