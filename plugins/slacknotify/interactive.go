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

// slackBlockAction is one element of a block_actions payload's "actions"
// array -- the shape Slack actually sends for a Block Kit button click.
// Only the fields this plugin forwards are decoded; the rest (type, text,
// style, ...) are ignored. Value and BlockID are decoded even though
// routing no longer reads them (see extractCallbackRoute) because both are
// still part of the scoped payload a workflow receives.
type slackBlockAction struct {
	ActionID string `json:"action_id"`
	BlockID  string `json:"block_id"`
	Value    string `json:"value"`
	ActionTS string `json:"action_ts"`
}

// parseCallbackRoute extracts a workflow ID and signal name from a string
// carrying the "wf:<workflow_id>:sig:<signal_name>" convention. Shared by
// the legacy top-level callback_id and the per-action fields of a
// block_actions payload -- both are just strings a workflow author chose to
// embed the same convention in.
func parseCallbackRoute(s string) (wfID, sigName string, ok bool) {
	if s == "" {
		return "", "", false
	}
	parts := strings.SplitN(s, ":", 4)
	if len(parts) != 4 || parts[0] != "wf" || parts[2] != "sig" {
		return "", "", false
	}
	return parts[1], parts[3], true
}

// extractCallbackRoute finds a wf:<id>:sig:<name> route in a Slack
// interactive payload: buttons only, first action only, route in
// actions[0].action_id (Slack bounds action_id to 255 characters) -- and,
// for backward compatibility with Slack's older "attachment"
// interactive_message payloads, the legacy top-level callback_id.
//
// The plugin's OWN button-sending path (sendMessage's opaque Blocks
// json.RawMessage, host_functions.go) never populates callback_id -- a
// Block Kit message's routing has nowhere to live except inside the
// block itself, so a real click on a button this plugin sent arrives as a
// block_actions payload with an EMPTY top-level callback_id and the route
// embedded in the clicked action's action_id instead. cleat#2230(a): the
// original handler read only callback_id, so a click on a real button
// silently routed nowhere.
//
// action_id ONLY -- not value or block_id. cleat#2230(a) originally fell
// back action_id -> value -> block_id, per the owner's initial #2230 spec;
// coordinator narrowed it after cleat-review's #2253 review, since a
// workflow author who names a button's action_id something with no
// routing intent (a plain label) while a DIFFERENT field happens to parse
// as a route creates an ambiguous, undocumented second way to route a
// click. One documented field is simpler to reason about and to audit.
//
// action is returned alongside the route (nil for the legacy callback_id
// path, which carries no per-action fields) so the caller can build the
// scoped signal payload without re-parsing payload.Actions itself.
func extractCallbackRoute(payload slackInteractivePayload) (wfID, sigName string, action *slackBlockAction, ok bool) {
	if wfID, sigName, ok := parseCallbackRoute(payload.CallbackID); ok {
		return wfID, sigName, nil, true
	}
	if len(payload.Actions) == 0 {
		return "", "", nil, false
	}
	var actions []slackBlockAction
	if err := json.Unmarshal(payload.Actions, &actions); err != nil || len(actions) == 0 {
		return "", "", nil, false
	}
	if wfID, sigName, ok := parseCallbackRoute(actions[0].ActionID); ok {
		return wfID, sigName, &actions[0], true
	}
	return "", "", nil, false
}

// scopedInteractionPayload is what actually gets marshaled into a
// workflow's signal payload for a block_actions click. cleat-review's
// #2253 finding: the original handler forwarded the FULL raw Slack
// payload verbatim, which includes response_url (a Slack bearer
// capability, valid to post into the channel unauthenticated for about 30
// minutes), trigger_id, and the complete message text -- none of which had
// ever been scoped down before, because before cleat#2230(a) nothing ever
// routed successfully enough to reach signalWorkflow at all. Deliberately
// narrow: only what a workflow needs to know which button was clicked and
// who clicked it. If a workflow needs to reply via response_url, that is a
// later feature in which the plugin itself holds and uses it -- it must
// not be handed to tenant-visible workflow code.
type scopedInteractionPayload struct {
	ActionID  string `json:"action_id"`
	BlockID   string `json:"block_id,omitempty"`
	Value     string `json:"value,omitempty"`
	UserID    string `json:"user_id,omitempty"`
	TeamID    string `json:"team_id,omitempty"`
	ChannelID string `json:"channel_id,omitempty"`
	ActionTS  string `json:"action_ts,omitempty"`
}

// slackObjectID extracts just the "id" field from one of a Slack
// payload's user/team/channel objects, which this plugin otherwise treats
// as opaque json.RawMessage. Best-effort: a missing or malformed object
// yields an empty ID rather than an error, since by the time this is
// called the route has already been resolved and refusing delivery over an
// unparsable, non-routing field would be a stranger failure than simply
// omitting it.
func slackObjectID(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var v struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return ""
	}
	return v.ID
}

// buildScopedPayload assembles the narrow payload actually delivered to a
// workflow's signal (see scopedInteractionPayload's doc comment). action is
// nil on the legacy callback_id path, which carries no per-action fields.
func buildScopedPayload(payload slackInteractivePayload, action *slackBlockAction) ([]byte, error) {
	scoped := scopedInteractionPayload{
		UserID:    slackObjectID(payload.User),
		TeamID:    slackObjectID(payload.Team),
		ChannelID: slackObjectID(payload.Channel),
	}
	if action != nil {
		scoped.ActionID = action.ActionID
		scoped.BlockID = action.BlockID
		scoped.Value = action.Value
		scoped.ActionTS = action.ActionTS
	}
	return json.Marshal(scoped)
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
// Routing convention: wf:<workflow_id>:sig:<signal_name>, either as the
// legacy top-level callback_id (older "attachment" interactive_message
// payloads) or as a block_actions payload's actions[0].action_id --
// buttons only, first action only, route in action_id (Slack bounds it to
// 255 characters); value and block_id are never read for routing. See
// extractCallbackRoute's doc comment for why action_id alone, and
// scopedInteractionPayload's for what of the click actually reaches the
// workflow.
//
// cleat#2230(a)/(b): this route cannot resolve a tenant yet (see the
// tenant-refusal block below), so every click refuses with 404 until
// cleat#2230(b) lands the slack_workspace lookup.
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

	// Extract workflow signal, trying the legacy top-level callback_id and
	// then a block_actions payload's actions[0].action_id.
	// Convention: wf:<workflowID>:sig:<signalName>
	// action (extractCallbackRoute's 3rd return) is unused here: the
	// scoped-payload build that consumed it moved out of this handler along
	// with signal delivery, both dead until cleat#2230(b) resolves a real
	// tenant. buildScopedPayload itself stays defined and directly
	// unit-tested (TestBuildScopedPayload) so (b) does not have to
	// reconstruct it.
	wfID, sigName, _, ok := extractCallbackRoute(payload)
	if !ok {
		// Nothing to route. Return 200 OK per Slack requirements.
		switch {
		case payload.CallbackID != "":
			p.logger.Warn("slack-notify: unrecognized callback_id format", "callback_id", payload.CallbackID)
		case len(payload.Actions) > 0:
			// A real block_actions click that just doesn't carry a
			// wf:...:sig:... route in action_id -- an ordinary button a
			// workflow author never meant to route, not a malformed
			// request. Debug rather than Warn: this is the expected shape
			// for any button that isn't wired to a signal.
			p.logger.Debug("slack-notify: block_actions payload has no routable action_id")
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
		return
	}

	// cleat#2230(a): refuse EVERY click, unconditionally, until cleat#2230(b)
	// lands. This handler MUST NOT read a tenant from the request or its
	// context by any means -- not auth.TenantIDFromRequest, not any other
	// source. cleat-review's second pass on this PR found why: this route is
	// auth-exempt (cleat#2172), but a deployment running
	// --tenant-resolver header:X-Tenant-ID resolves a tenant from an
	// arbitrary request header on EVERY route, including exempt ones -- that
	// resolver sits outside auth.Middleware's public-pattern short-circuit
	// (cmd/cleat-worker/main.go), so an "auth-exempt" request still ends up
	// with a tenant in ctx if that resolver is configured. Slack's HMAC
	// signature (verified above) covers the payload, never headers, so
	// whoever controls the header -- a gateway in front of the worker, not
	// Slack -- would pick which tenant every click signals. Measured live:
	// header mode + X-Tenant-ID: B -> 200, and the signal landed in B's
	// workflow_signals, for a button that named no tenant at all. The
	// earlier version of this fix read auth.TenantIDFromRequest and refused
	// only when THAT returned no tenant, which is exactly the gap: it
	// trusted whatever the resolver chain had already put in context instead
	// of establishing its own. cleat#2230(b) adds the only source this
	// handler will trust -- team_id resolved through slack_workspace, read
	// fresh in this handler and never taken from ctx. Until then there is no
	// safe tenant to signal under, so every click refuses. ids only in the
	// log -- no payload contents, which may carry a user-controlled
	// value/block_id.
	p.interactiveNoTenantRefusals.Add(1)
	p.logger.Warn("slack-notify: interactive callbacks are refused until cleat#2230(b) resolves a workspace mapping",
		"workflow_id", wfID, "signal", sigName)
	p.writeError(w, http.StatusNotFound, "workspace not mapped")
}
