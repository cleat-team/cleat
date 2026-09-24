package main

import (
	"net/http"
	"time"
)

// The health surface (cleat#2007).
//
//	/livez    the process and its background loops are ticking. It does not look at the database, so a
//	          database outage never restarts workers (restarting cannot fix it). 503 only when a loop is stuck.
//	/readyz   /livez, AND started, AND not draining, AND the database answered its last bounded call.
//	          503 means "do not send this worker traffic"; the process is left alone.
//	/healthz  an alias of /livez, kept because callers exist.
//	/api/admin/health  the same facts with the detail, behind authentication.
//
// The three unauthenticated bodies carry ONLY ok, degraded and reason codes (owner decision 2A on #2007):
// no loop names, plugin names or messages, and no database error text. Degraded states (memory pressure,
// an unhealthy plugin) are report-only, 200 (owner decision 1A, consistent with cleat#2168): a degraded
// worker is still serving, and taking every worker out of rotation over an audit-table problem would turn
// it into an outage.

// Reason codes. They are the whole public vocabulary.
const (
	reasonLoopStuck       = "background_loop_stuck"
	reasonDatabase        = "database_unreachable"
	reasonStarting        = "starting"
	reasonDraining        = "draining"
	reasonMemoryPressure  = "memory_pressure"
	reasonPluginUnhealthy = "plugin_unhealthy"
)

// healthReport is everything the four handlers report, computed once per request from state that is
// already in memory: reading it never touches the database and never runs plugin code.
type healthReport struct {
	// notLive: why /livez is 503 (a stuck loop). notReady: why /readyz is 503, which includes notLive.
	notLive  []string
	notReady []string
	// degraded reasons are report-only.
	degraded []string

	// Detail, for the authenticated admin route only.
	staleLoops []string
	// blockedOnDatabase: the stale loops are a consequence of an unreachable database, not a fault of
	// their own, so they do not fail /livez.
	blockedOnDatabase bool
	plugins           map[string]string
	memoryPressure    float64
	draining          bool
	db                dbSnapshot
}

func (w *Worker) healthReport() healthReport {
	var r healthReport
	r.staleLoops = w.healthTracker.staleLoops()
	r.db = w.dbReach.snapshot()
	// A loop that is stuck INSIDE a database call is stuck because the database is not answering, and
	// restarting the worker cannot fix that, which is the one thing /livez must never ask for. Measured
	// against a real `docker pause` (cleat#2007): the dispatch loop's call hung in the driver, the loop
	// went stale after six of its intervals, and /livez answered 503, so a kubelet would have restarted
	// every worker for a database outage. So while the database is known to be unreachable, stale loops
	// are reported on the admin route only (blockedOnDatabase), and /readyz already says
	// database_unreachable. A loop that is stuck while the database answers still fails /livez.
	r.blockedOnDatabase = len(r.staleLoops) > 0 && r.db.Known && !r.db.Reachable
	if len(r.staleLoops) > 0 && !r.blockedOnDatabase {
		r.notLive = append(r.notLive, reasonLoopStuck)
		r.notReady = append(r.notReady, reasonLoopStuck)
	}
	r.draining = w.draining.Load()
	if r.draining {
		r.notReady = append(r.notReady, reasonDraining)
	}
	switch {
	case !r.db.Known:
		r.notReady = append(r.notReady, reasonStarting)
	case !r.db.Reachable:
		r.notReady = append(r.notReady, reasonDatabase)
	}
	if w.memoryController != nil {
		if p := w.memoryController.Pressure(); p > 0 {
			r.memoryPressure = p
			r.degraded = append(r.degraded, reasonMemoryPressure)
		}
	}
	if r.plugins = w.unhealthyPlugins(); len(r.plugins) > 0 {
		r.degraded = append(r.degraded, reasonPluginUnhealthy)
	}
	return r
}

// publicBody is the unauthenticated body: ok, degraded and reason codes, nothing else. `reason` is the
// first code and `reasons` all of them (a worker under memory pressure no longer hides an unhealthy plugin).
func publicBody(ok bool, notOK, degraded []string) map[string]any {
	body := map[string]any{"ok": ok}
	if len(degraded) > 0 {
		body["degraded"] = true
	}
	reasons := append(append([]string{}, notOK...), degraded...)
	if len(reasons) > 0 {
		body["reason"] = reasons[0]
		body["reasons"] = reasons
	}
	return body
}

func (s *apiServer) handleLivez(w http.ResponseWriter, r *http.Request) {
	rep := s.worker.healthReport()
	if len(rep.notLive) > 0 {
		s.writeJSON(w, http.StatusServiceUnavailable, publicBody(false, rep.notLive, rep.degraded))
		return
	}
	s.writeJSON(w, http.StatusOK, publicBody(true, nil, rep.degraded))
}

// handleHealthz is /livez under its old name: same answer, same body.
func (s *apiServer) handleHealthz(w http.ResponseWriter, r *http.Request) { s.handleLivez(w, r) }

func (s *apiServer) handleReadyz(w http.ResponseWriter, r *http.Request) {
	rep := s.worker.healthReport()
	if len(rep.notReady) > 0 {
		s.writeJSON(w, http.StatusServiceUnavailable, publicBody(false, rep.notReady, rep.degraded))
		return
	}
	s.writeJSON(w, http.StatusOK, publicBody(true, nil, rep.degraded))
}

// handleAdminHealth is GET /api/admin/health: the facts the public bodies leave out. It is behind the
// same authentication as the other /api/admin routes, and is exactly as open as they are.
func (s *apiServer) handleAdminHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	rep := s.worker.healthReport()
	db := map[string]any{
		"known":                rep.db.Known,
		"reachable":            rep.db.Reachable,
		"consecutive_failures": rep.db.Failures,
		"last_error":           rep.db.LastError,
		"last_probe_ms":        rep.db.LastElapsed.Milliseconds(),
	}
	if !rep.db.LastSuccess.IsZero() {
		db["last_success"] = rep.db.LastSuccess.UTC().Format(time.RFC3339)
	}
	if !rep.db.FailingSince.IsZero() {
		db["unreachable_since"] = rep.db.FailingSince.UTC().Format(time.RFC3339)
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"live":                      len(rep.notLive) == 0,
		"ready":                     len(rep.notReady) == 0,
		"not_ready":                 rep.notReady,
		"degraded":                  rep.degraded,
		"stale_loops":               rep.staleLoops,
		"loops_blocked_on_database": rep.blockedOnDatabase,
		"plugins":                   rep.plugins,
		"memory_pressure":           rep.memoryPressure,
		"draining":                  rep.draining,
		"database":                  db,
	})
}
