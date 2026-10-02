package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cleat-team/cleat/engine"
)

// internalHoldsResponse is the answer to a reaper's "do you still hold this
// run" question -- cleat#2196's veto channel. Held true means "yes, and the
// generation asked about is the one this worker is currently executing";
// the reaper (step 4, not yet built) treats anything else -- false, a
// non-200, or no answer at all -- as "no veto", the same as a worker that
// is actually gone. A missing vote must never block a reclaim: the veto
// channel is an optimization over the existing fencing, not a new source of
// truth, and a worker that cannot be reached is exactly the case the
// existing reclaim-on-timeout behaviour already has to handle.
type internalHoldsResponse struct {
	Held bool `json:"held"`
}

// handleInternalHolds answers GET /internal/holds/{run_id}?generation=G.
//
// Answers from w.inflight ONLY, never by re-querying the database. The
// scenario this channel exists for (cleat#2196's issue body) is a worker
// whose own path to the database is degraded while the worker process
// itself is healthy and making progress -- re-querying the database from
// inside this handler would be exactly as blind as the heartbeat the
// reaper is already not trusting. inflight is this worker's own in-process
// bookkeeping of what it is actually executing right now (set in
// setup.go's claim loop, cleared when an execution ends), which a degraded
// database connection does not affect either way.
func (w *Worker) handleInternalHolds(secret string) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		if !validInternalAuth(r, secret) {
			http.Error(rw, "unauthorized", http.StatusUnauthorized)
			return
		}
		runID := r.PathValue("run_id")
		if runID == "" {
			http.Error(rw, "missing run id", http.StatusBadRequest)
			return
		}
		generation, err := strconv.ParseInt(r.URL.Query().Get("generation"), 10, 64)
		if err != nil {
			http.Error(rw, "missing or invalid generation", http.StatusBadRequest)
			return
		}

		held := false
		if v, ok := w.inflight.Load(runID); ok {
			if wf, ok := v.(*engine.WorkflowInstance); ok && wf.Generation == generation {
				held = true
			}
		}

		rw.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(rw).Encode(internalHoldsResponse{Held: held})
	}
}

// validInternalAuth checks the Authorization: Bearer <secret> header
// against secret using subtle.ConstantTimeCompare rather than ==, so that a
// caller inside the cluster network cannot learn the secret one byte at a
// time from comparison timing -- the same reasoning as the HMAC checks
// elsewhere in this repo (plugins/slacknotify/signedroute.go,
// plugins/webhookingest/routes.go). This is a bearer secret rather than a
// signed payload, so there is nothing to HMAC over; the comparison itself
// is the only step that needs to run in constant time.
//
// secret == "" always returns false, never true: an empty comparison value
// must not be satisfiable by an empty or missing header, which
// ConstantTimeCompare alone would allow (two zero-length slices are equal).
// This function is never reached with the listener started at all unless
// CLEAT_INTERNAL_AUTH_KEY was non-empty at startup (see newInternalHoldsServer),
// but it does not trust that invariant to hold forever just because it
// holds today.
func validInternalAuth(r *http.Request, secret string) bool {
	if secret == "" {
		return false
	}
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		return false
	}
	got := strings.TrimPrefix(h, prefix)
	return subtle.ConstantTimeCompare([]byte(got), []byte(secret)) == 1
}

// newInternalHoldsServer builds the listener for --internal-addr. Separate
// from main.go's startup sequence so it can be unit-tested without a full
// worker boot: callers needing a real round trip construct a *Worker with
// nothing but w.inflight populated (the only field this handler reads).
func newInternalHoldsServer(addr, secret string, w *Worker) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /internal/holds/{run_id}", w.handleInternalHolds(secret))
	return &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
}

// askInternalHolds is the reaper-side half of the channel (dialing, not
// serving): it asks address whether it still holds runID at generation,
// over --internal-addr. Built here, alongside the server, so the two stay
// in exact agreement about the request/response shape; step 4 (cleat#2196)
// is its first caller.
//
// Every failure to get a definite "yes" -- a dial error, a non-200, a
// malformed body, a context timeout -- returns held=false, nil, never an
// error the caller has to remember to treat as "no veto". The one thing
// that can veto a reclaim is an explicit, authenticated "held": true from
// the worker named in the stale row; everything else is silence, and
// silence must never block a reclaim (see internalHoldsResponse's doc
// comment).
func askInternalHolds(ctx context.Context, client *http.Client, address, secret, runID string, generation int64) (held bool, err error) {
	url := fmt.Sprintf("http://%s/internal/holds/%s?generation=%d", address, runID, generation)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, nil
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	resp, err := client.Do(req)
	if err != nil {
		return false, nil
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return false, nil
	}
	var body internalHoldsResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return false, nil
	}
	return body.Held, nil
}
