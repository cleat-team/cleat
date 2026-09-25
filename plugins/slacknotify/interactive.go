package slacknotify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cleat-team/cleat/plugin"
	"github.com/google/uuid"
)

// interactiveMaxBodySize bounds POST /slack/interactive, whose payloads are a
// few KB of URL-encoded JSON. cleat-review's #2231 review measured a 256 MiB
// anonymous POST allocating 828 MiB via the unbounded io.ReadAll this
// replaced, BEFORE the signature check that would have refused it -- and the
// route being newly public (cleat#2172's auth-middleware exemption, this
// same PR) is what makes that reachable without authentication at all.
//
// cleat#2232: declared at REGISTRATION via plugin.MaxBody(interactiveMaxBodySize,
// ...) in RegisterRoutes (routes.go), rather than applied inline here with a
// hand-rolled http.MaxBytesReader -- this route's own ceiling happens to equal
// the host adapter's 1 MiB default, but pinning it here means it stays 1 MiB
// even if an operator raises --plugin-max-body-size for some other route's
// sake. The handler below just calls plugin.ReadBody, which reads whatever
// ceiling the registration-time wrap already applied.
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

// extractCallbackRoute finds a SIGNED wf:<id>:sig:<name>:<issued_at>:<mac>
// route in a Slack interactive payload: buttons only, first action only,
// route in actions[0].action_id (Slack bounds action_id to 255 characters)
// -- and, for backward compatibility with Slack's older "attachment"
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
// Uses parseSignedRoute, not parseCallbackRoute directly: cleat#2230
// ("signed routes") requires every route reaching the handler to already
// carry the issued-at/MAC tail. The MAC itself is not checked here -- see
// parseSignedRoute's doc comment for why that has to wait for tenant
// resolution.
//
// action is returned alongside the route (nil for the legacy callback_id
// path, which carries no per-action fields) so the caller can build the
// scoped signal payload, and verify the MAC's block_id/value binding,
// without re-parsing payload.Actions itself.
func extractCallbackRoute(payload slackInteractivePayload) (unsignedRoute, issuedAtHex, mac string, action *slackBlockAction, ok bool) {
	if unsignedRoute, issuedAtHex, mac, ok := parseSignedRoute(payload.CallbackID); ok {
		return unsignedRoute, issuedAtHex, mac, nil, true
	}
	if len(payload.Actions) == 0 {
		return "", "", "", nil, false
	}
	var actions []slackBlockAction
	if err := json.Unmarshal(payload.Actions, &actions); err != nil || len(actions) == 0 {
		return "", "", "", nil, false
	}
	if unsignedRoute, issuedAtHex, mac, ok := parseSignedRoute(actions[0].ActionID); ok {
		return unsignedRoute, issuedAtHex, mac, &actions[0], true
	}
	return "", "", "", nil, false
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
//
// unsignedRoute, not action.ActionID: a click's real action_id carries the
// internal signed wire format (wf:<id>:sig:<name>:<issued_at>:<mac>), which
// is this plugin's implementation detail, not something a workflow should
// ever see or be able to depend on the shape of.
func buildScopedPayload(payload slackInteractivePayload, unsignedRoute string, action *slackBlockAction) ([]byte, error) {
	scoped := scopedInteractionPayload{
		ActionID:  unsignedRoute,
		UserID:    slackObjectID(payload.User),
		TeamID:    slackObjectID(payload.Team),
		ChannelID: slackObjectID(payload.Channel),
	}
	if action != nil {
		scoped.BlockID = action.BlockID
		scoped.Value = action.Value
		scoped.ActionTS = action.ActionTS
	}
	return json.Marshal(scoped)
}

// slackUserTeamID extracts a Slack Connect shared-channel click's
// user.team_id -- the team the CLICKING user belongs to, which can differ
// from payload.team.id (the team that owns the CHANNEL) when a message is
// posted into a Slack Connect shared channel. resolveSlackTenant uses this
// to refuse a click whose user and channel resolve to different tenants,
// rather than trusting the channel's workspace alone.
func slackUserTeamID(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var v struct {
		TeamID string `json:"team_id"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return ""
	}
	return v.TeamID
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
// cleat#2230 ("signed routes"): the route itself must additionally carry a
// valid signature, minted by sendMessage/stampBlocksWithRoutes for the
// tenant that sent the message, and is verified here against the tenant
// resolveSlackTenant resolves from the CLICKING workspace -- so a button
// only works for the tenant it was minted for, never for whatever tenant
// happens to map to the workspace it is clicked from. See signedroute.go.
func (p *Plugin) handleInteractiveCallback(w http.ResponseWriter, r *http.Request) {
	// The body is already bounded to interactiveMaxBodySize by the
	// plugin.MaxBody wrap RegisterRoutes registered this handler under --
	// this route lost its auth middleware gate in this same change
	// (cleat#2172's exemption, so the signature check below is reachable at
	// all), so an unbounded read here would be unbounded for anyone, not
	// just an authenticated caller. cleat-review measured a 256 MiB
	// anonymous POST allocating 828 MiB before the signature check ever ran.
	bodyBytes, ok := plugin.ReadBody(w, r)
	if !ok {
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

	// Extract a SIGNED route, trying the legacy top-level callback_id and
	// then a block_actions payload's actions[0].action_id. This only
	// recognises the shape (wf:<id>:sig:<name>:<issued_at>:<mac>) -- the MAC
	// itself is checked below, after the tenant is resolved, since
	// verifying it needs the tenant as an input.
	unsignedRoute, issuedAtHex, routeMACValue, action, ok := extractCallbackRoute(payload)
	if !ok {
		// Nothing to route. Return 200 OK per Slack requirements.
		switch {
		case payload.CallbackID != "":
			p.logger.Warn("slack-notify: unrecognized callback_id format", "callback_id", payload.CallbackID)
		case len(payload.Actions) > 0:
			// A real block_actions click that just doesn't carry a signed
			// route in action_id -- an ordinary button a workflow author
			// never meant to route, not a malformed request. Debug rather
			// than Warn: this is the expected shape for any button that
			// isn't wired to a signal.
			p.logger.Debug("slack-notify: block_actions payload has no routable action_id")
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
		return
	}

	// Resolve the tenant fresh, from slack_workspace, NEVER from ctx by any
	// means -- not auth.TenantIDFromRequest, not any other source.
	// cleat-review's second pass on an earlier version of this PR found why:
	// this route is auth-exempt (cleat#2172), but a deployment running
	// --tenant-resolver header:X-Tenant-ID resolves a tenant from an
	// arbitrary request header on EVERY route, including exempt ones -- that
	// resolver sits outside auth.Middleware's public-pattern short-circuit
	// (cmd/cleat-worker/main.go), so an "auth-exempt" request still ends up
	// with a tenant in ctx if that resolver is configured. Slack's HMAC
	// signature (verified above) covers the payload, never headers, so
	// whoever controls the header -- a gateway in front of the worker, not
	// Slack -- would pick which tenant every click signals. Measured live
	// against the pre-cleat#2230(b) handler: header mode + X-Tenant-ID: B ->
	// 200, and the signal landed in B's workflow_signals, for a button that
	// named no tenant at all.
	tenantID, ok := p.resolveSlackTenant(r.Context(), payload)
	if !ok {
		p.interactiveNoTenantRefusals.Add(1)
		p.logger.Warn("slack-notify: interactive callback refused -- unmapped or ambiguous workspace",
			"team_id", slackObjectID(payload.Team), "user_team_id", slackUserTeamID(payload.User))
		p.writeError(w, http.StatusNotFound, "workspace not mapped")
		return
	}

	// Verify the route's MAC under the RESOLVED tenant -- this is the whole
	// point of signed routes (cleat#2230, "signed routes (A)"): a button
	// only works for the tenant that posted it, not for whichever tenant
	// the clicking workspace happens to map to today. blockID/value come
	// from the ACTUAL click (action, nil on the legacy callback_id path),
	// never re-derived from the route string -- they are part of what the
	// MAC binds.
	var blockID, value string
	if action != nil {
		blockID, value = action.BlockID, action.Value
	}
	routeKey, prevRouteKey, err := p.routeSigningKey(r.Context())
	if err != nil {
		p.logger.Error("slack-notify: route signing key unavailable, refusing /slack/interactive", "error", err)
		p.writeError(w, http.StatusNotFound, "workspace not mapped")
		return
	}
	usedPrevious, sigOK := verifyRouteSignature(routeKey, prevRouteKey, tenantID, unsignedRoute, issuedAtHex, blockID, value, routeMACValue,
		p.config.routeMaxAge(), time.Now())
	if !sigOK {
		p.interactiveNoTenantRefusals.Add(1)
		p.logger.Warn("slack-notify: interactive callback refused -- invalid or expired route signature",
			"team_id", slackObjectID(payload.Team))
		p.writeError(w, http.StatusNotFound, "workspace not mapped")
		return
	}
	if usedPrevious {
		// The operator's own signal that slacknotify.route_signing_key.previous
		// is now safe to retire -- once this line stops appearing, no
		// outstanding Slack message still needs the old key (design
		// addendum, cleat#2230 §3).
		p.logger.Warn("slack-notify: route validated under the previous signing key", "team_id", slackObjectID(payload.Team))
	}

	wfID, sigName, ok := parseCallbackRoute(unsignedRoute)
	if !ok {
		// Unreachable: extractCallbackRoute already validated unsignedRoute
		// via this same parser. Kept as a defensive check rather than a
		// panic -- see signRoute's doc comment for why this package avoids
		// panicking on a caller invariant instead of a user input.
		p.writeError(w, http.StatusNotFound, "workspace not mapped")
		return
	}

	scopedPayload, err := buildScopedPayload(payload, unsignedRoute, action)
	if err != nil {
		p.logger.Error("slack-notify: building scoped signal payload", "error", err)
		p.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	// ForTenant, not a bare ctx: DeliverSignal (via signalPluginWorkflow,
	// cmd/cleat-worker/main.go) scopes its statement to whatever tenant ctx
	// carries, and this is the ONLY point in this handler that ever sets
	// one -- resolved above, fresh, from slack_workspace.
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		p.logger.Error("slack-notify: resolved tenant is not a UUID", "error", err)
		p.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	ctx := plugin.ForTenant(r.Context(), tid)
	if err := p.signalWorkflow(ctx, wfID, sigName, string(scopedPayload)); err != nil {
		// plugin.ErrWorkflowNotFound (cleat#2239) collapses "no such
		// workflow", "purged", and "belongs to a different tenant" into one
		// error deliberately -- telling those three apart would reopen the
		// existence oracle DeliverSignal was changed to avoid (cleat#2218).
		// But that one collapsed case IS now distinguishable from a genuine
		// delivery failure, which it was not before #2239 (both returned
		// nil), so the 1B spec's "map missing workflow to 404, not 500" is
		// satisfiable at exactly the granularity the store allows: 404 for
		// the collapsed not-found/wrong-tenant case, 500 for anything else.
		if errors.Is(err, plugin.ErrWorkflowNotFound) {
			p.interactiveNoTenantRefusals.Add(1)
			p.logger.Warn("slack-notify: interactive callback refused -- workflow not visible under the resolved tenant",
				"team_id", slackObjectID(payload.Team))
			p.writeError(w, http.StatusNotFound, "workflow not found")
			return
		}
		p.logger.Error("slack-notify: delivering signal", "workflow_id", wfID, "signal", sigName, "error", err)
		p.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

// resolveSlackTenant resolves a Slack interactive payload's team.id (and,
// for Slack Connect, user.team_id) to the ONE cleat tenant slack_workspace
// maps it to. Refuses (ok=false) if team.id is missing or null (Enterprise
// Grid org-wide installs are unsupported and the docs say so), if it maps
// to no tenant, or if user.team_id is present, different from team.id, and
// maps to a DIFFERENT tenant -- a Slack Connect click must not be trusted
// just because the CHANNEL's own workspace happens to be mapped.
func (p *Plugin) resolveSlackTenant(ctx context.Context, payload slackInteractivePayload) (tenantID string, ok bool) {
	teamID := slackObjectID(payload.Team)
	if teamID == "" {
		return "", false
	}
	if err := p.db.QueryRow(ctx, plugin.Rebind(
		`SELECT CAST(tenant_id AS CHAR(36)) FROM slack_workspace WHERE team_id = $1`, p.dialect,
	), teamID).Scan(&tenantID); err != nil {
		return "", false
	}
	// Canonicalize before this value is used for anything -- MSSQL's
	// CAST(... AS CHAR(36)) renders the UUID UPPERCASE where postgres and
	// mysql return whatever case was written, and this string is exactly
	// what gets fed into routeMAC on the verify side. Measured on real
	// MSSQL (cleat-review + coordinator, cleat#2230): a button stamped
	// under host_functions.go's lowercase-canonicalized tenantID 404'd on
	// every click here before this line existed, because the two sides
	// disagreed on case. uuid.Parse accepts either case; .String() always
	// emits canonical lowercase, so this converges with sendMessage's
	// canonicalization regardless of which dialect answered.
	if parsed, parseErr := uuid.Parse(tenantID); parseErr == nil {
		tenantID = parsed.String()
	}
	if userTeamID := slackUserTeamID(payload.User); userTeamID != "" && userTeamID != teamID {
		var userTenantID string
		if err := p.db.QueryRow(ctx, plugin.Rebind(
			`SELECT CAST(tenant_id AS CHAR(36)) FROM slack_workspace WHERE team_id = $1`, p.dialect,
		), userTeamID).Scan(&userTenantID); err != nil {
			return "", false
		}
		if parsed, parseErr := uuid.Parse(userTenantID); parseErr == nil {
			userTenantID = parsed.String()
		}
		// EqualFold stays as defense-in-depth even though both sides are
		// now canonicalized to the same case: a value that fails to
		// uuid.Parse (never observed, but not provably impossible against
		// a hand-edited row) falls through unchanged, and this still
		// tolerates a case mismatch in that fallback case rather than
		// refusing a legitimate Slack Connect click over it.
		if !strings.EqualFold(userTenantID, tenantID) {
			return "", false
		}
	}
	return tenantID, true
}
