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
	// blockedOnDatabase: every stale loop is explained by the database (databaseExplainsStaleLoop), so
	// none of them fails /livez.
	blockedOnDatabase bool
	plugins           map[string]string
	memoryPressure    float64
	draining          bool
	db                dbSnapshot
}

func (w *Worker) healthReport() healthReport {
	var r healthReport
	stale := w.healthTracker.staleLoopDetails()
	r.db = w.dbReach.snapshot()
	// A loop that is stuck INSIDE a database call is stuck because the database is not answering, and
	// restarting the worker cannot fix that, which is the one thing /livez must never ask for. Measured
	// against a real `docker pause` (cleat#2007): the dispatch loop's call hung in the driver, the loop
	// went stale after six of its intervals, and /livez answered 503, so a kubelet would have restarted
	// every worker for a database outage. So a stale loop the database explains is reported on the admin
	// route only (blockedOnDatabase), and /readyz says database_unreachable. A loop that is stuck while
	// the database answers still fails /livez. See databaseExplainsStaleLoop for what "explains" means.
	wedged := 0
	for _, l := range stale {
		r.staleLoops = append(r.staleLoops, l.Name)
		if !w.databaseExplainsStaleLoop(r.db, l) {
			wedged++
		}
	}
	r.blockedOnDatabase = len(stale) > 0 && wedged == 0
	if wedged > 0 {
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

// dbRecoveryGrace is how long after the database answers again a stale loop is still put down to the
// outage. The loops that were stuck inside a call resume when the driver returns, and until each has
// ticked once it still reads stale: measured against a real `docker pause`, /livez answered 503 for up to
// five seconds after the database came back, which a kubelet counts as a failed probe.
var dbRecoveryGrace = 30 * time.Second

// databaseExplainsStaleLoop reports whether a stale loop is to be read as "blocked on the database" and
// not as "wedged". The claim is a causal one, so it is made per loop, from three facts:
//
//  1. The database has NOT answered since the loop went quiet: no successful bounded call began later than
//     one interval after the loop's last tick. If it did, the database was fine while the loop stayed
//     silent, and the loop is wedged on something else. (This replaced "the database is currently known
//     to be unreachable": a paused database is not KNOWN unreachable until the first bounded call misses
//     its deadline, up to a heartbeat interval plus that deadline after the pause, and /livez answered 503
//     in that window.)
//  2. The evidence is still being collected: a bounded call is in flight (a paused database holds the
//     call, which is the only observation there is) or the last observation is recent (twice the
//     heartbeat interval plus the call deadline). Otherwise nothing is watching the database, "it has not
//     answered" is only the absence of asking, and the loop is wedged. Without this an unwatched verdict
//     never expires.
//  3. Or the database answered again within dbRecoveryGrace: the loops it was holding resume only when the
//     driver returns.
//
// Before anything has been observed there is nothing to explain a loop with.
func (w *Worker) databaseExplainsStaleLoop(db dbSnapshot, l staleLoop) bool {
	if !db.Known {
		return false
	}
	now := w.dbReach.now()
	if !db.RecoveredAt.IsZero() && db.Reachable && now.Sub(db.RecoveredAt) < dbRecoveryGrace {
		return true
	}
	if db.LastSuccessStart.After(l.Since.Add(l.Interval)) {
		return false
	}
	freshness := 2 * (w.heartbeatInterval + w.dbCallDeadline())
	return db.InFlight > 0 || now.Sub(db.LastObserved) <= freshness
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
