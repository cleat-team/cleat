package slacknotify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// interactiveMaxBodySize bounds POST /slack/interactive, whose payloads are a
// few KB of URL-encoded JSON. cleat-review's #2231 review measured a 256 MiB
// anonymous POST allocating 828 MiB via the unbounded io.ReadAll this
// replaced, BEFORE the signature check that would have refused it -- and the
// route being newly public (cleat#2172's auth-middleware exemption, this
// same PR) is what makes that reachable without authentication at all.
const interactiveMaxBodySize = 1 << 20 // 1 MiB

// slackInteractivePayload represents a Slack interactive message callback.
type slackInteractivePayload struct {
	Type        string          `json:"type"`
	CallbackID  string          `json:"callback_id"`
	TriggerID   string          `json:"trigger_id"`
	User        json.RawMessage `json:"user,omitempty"`
	Channel     json.RawMessage `json:"channel,omitempty"`
	Actions     json.RawMessage `json:"actions,omitempty"`
	ResponseURL string          `json:"response_url,omitempty"`
	Team        json.RawMessage `json:"team,omitempty"`
	Message     json.RawMessage `json:"message,omitempty"`
}

// signingSecret fetches the current Slack request-signing secret. Called at
// the moment of use, on every request, rather than cached at Init -- the
// same "PER-USE, NOT PER-Init" convention as email's sendGridAPIKey
// (plugins/email/plugin.go) -- so a secret rotated with
// `cleatctl set-deployment-secret` takes effect on the very next request,
// and a secret that is retired takes effect just as fast: the next request
// refuses instead of verifying against a value that should no longer work.
func (p *Plugin) signingSecret(ctx context.Context) (string, error) {
	if p.deploymentSecrets == nil {
		return "", fmt.Errorf("slack-notify: no deployment secret store configured")
	}
	secret, err := p.deploymentSecrets.Get(ctx, "slacknotify.signing_secret")
	if err != nil {
		return "", fmt.Errorf("slack-notify: signing_secret: %w", err)
	}
	// An empty string is not a usable HMAC key -- hmac.New([]byte(""), ...)
	// still computes and compares a (wrong-but-deterministic) digest, so
	// without this an empty stored value would verify as "correctly signed
	// with the empty key" rather than refusing like every other unusable
	// secret.
	if secret == "" {
		return "", fmt.Errorf("slack-notify: signing_secret: empty")
	}
	return secret, nil
}

// handleInteractiveCallback receives Slack interactive payloads (button clicks, etc.),
// verifies the request, and delivers a signal to the relevant workflow.
//
// Callback ID convention used by workflows: wf:<workflow_id>:sig:<signal_name>
func (p *Plugin) handleInteractiveCallback(w http.ResponseWriter, r *http.Request) {
	// Bound the body before reading any of it. This route lost its auth
	// middleware gate in this same change (cleat#2172's exemption, so the
	// signature check below is reachable at all), so an unbounded ReadAll
	// here is unbounded for anyone, not just an authenticated caller --
	// cleat-review measured a 256 MiB anonymous POST allocating 828 MiB
	// before the signature check ever ran. A Slack interactive payload is a
	// few KB of URL-encoded JSON; interactiveMaxBodySize gives it headroom
	// without giving an anonymous request the run of the heap.
	r.Body = http.MaxBytesReader(w, r.Body, interactiveMaxBodySize)
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			p.writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		p.writeError(w, http.StatusBadRequest, "cannot read body")
		return
	}
	r.Body = io.NopCloser(strings.NewReader(string(bodyBytes)))

	if err := r.ParseForm(); err != nil {
		p.writeError(w, http.StatusBadRequest, "invalid form data")
		return
	}

	// Signature-header presence and freshness first, deployment-secret
	// lookup second -- deliberately in that order. The lookup is a database
	// read plus a decrypt; checking the cheap, request-only conditions first
	// means a request with no signature headers at all (the common case for
	// drive-by anonymous traffic against a now-public route) never pays for
	// one.
	timestamp := r.Header.Get("X-Slack-Request-Timestamp")
	signature := r.Header.Get("X-Slack-Signature")
	if timestamp == "" || signature == "" {
		p.writeError(w, http.StatusUnauthorized, "missing Slack signature headers")
		return
	}

	// Reject requests more than 5 minutes away from now, in EITHER
	// direction. This used to be `time.Now().Unix()-ts > 300`, which only
	// rejects a request whose timestamp is in the past -- a validly signed
	// request stamped an hour in the future passed. Slack's own
	// recommendation is a symmetric window; nothing about replay protection
	// is one-sided.
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		p.writeError(w, http.StatusUnauthorized, "stale request")
		return
	}
	if age := time.Now().Unix() - ts; age > 300 || age < -300 {
		p.writeError(w, http.StatusUnauthorized, "stale request")
		return
	}

	// Verify the Slack signing signature. cleat#2172 option A: this route is
	// public (see cmd/cleat-worker/main.go's auth-middleware exemption), so
	// the signature is the ONLY gate, and it is never skipped. Earlier this
	// checked `if p.slackSigningSecret != ""` and fell through to accepting
	// the request unsigned when it was empty -- exactly the hole #2172
	// reported. There is no such fallthrough here: a signing secret that is
	// absent, unreadable, empty, or retired refuses the request the same way
	// a bad signature does.
	secret, err := p.signingSecret(r.Context())
	if err != nil {
		p.logger.Error("slack-notify: signing secret unavailable, refusing /slack/interactive", "error", err)
		p.writeError(w, http.StatusUnauthorized, "signing secret unavailable")
		return
	}

	basestring := fmt.Sprintf("v0:%s:%s", timestamp, string(bodyBytes))
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(basestring))
	expected := "v0=" + hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(signature), []byte(expected)) {
		p.writeError(w, http.StatusUnauthorized, "invalid signature")
		return
	}

	// Parse the Slack payload (URL-encoded JSON in the "payload" form field)
	payloadStr := r.PostForm.Get("payload")
	if payloadStr == "" {
		p.writeError(w, http.StatusBadRequest, "missing payload")
		return
	}

	var payload slackInteractivePayload
	if err := json.Unmarshal([]byte(payloadStr), &payload); err != nil {
		p.writeError(w, http.StatusBadRequest, "invalid payload JSON: "+err.Error())
		return
	}

	// Extract workflow signal from callback_id
	// Convention: wf:<workflowID>:sig:<signalName>
	if payload.CallbackID == "" {
		// No callback_id — nothing to route. Return 200 OK per Slack requirements.
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
		return
	}

	parts := strings.SplitN(payload.CallbackID, ":", 4)
	if len(parts) != 4 || parts[0] != "wf" || parts[2] != "sig" {
		p.logger.Warn("slack-notify: unrecognized callback_id format", "callback_id", payload.CallbackID)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
		return
	}

	wfID := parts[1]
	sigName := parts[3]

	// Deliver as workflow signal
	sigPayload, _ := json.Marshal(payload)
	if p.signalWorkflow != nil {
		if err := p.signalWorkflow(r.Context(), wfID, sigName, string(sigPayload)); err != nil {
			p.logger.Error("slack-notify: failed to deliver signal", "workflow_id", wfID, "signal", sigName, "error", err)
			p.writeError(w, http.StatusInternalServerError, "failed to deliver signal")
			return
		}
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}
