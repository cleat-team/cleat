package slacknotify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

// Signed routes bind a Slack button's action_id to the tenant that posted
// it (cleat#2230, owner decision "signed routes (A)", design addendum on
// that issue). Without this, a button minted by workflow X for tenant A
// works identically if pasted into tenant B's channel or a leaked webhook
// URL -- the route wf:<id>:sig:<name> names a workflow and a signal, never
// a tenant, so nothing before this stopped a click from resolving under
// whichever tenant slack_workspace maps the CLICKING workspace to.
//
// Format: the existing unsigned route (wf:<workflow_id>:sig:<signal_name>,
// unchanged, still parsed by parseCallbackRoute) plus a fixed-width tail:
//
//	wf:<workflow_id>:sig:<signal_name>:<issued_at hex8>:<mac>
//
// The tail is read from the END of the string, never by splitting on ':'
// from the left -- signal_name is not restricted from containing ':', and
// parseCallbackRoute's own SplitN(s, ":", 4) already relies on that.
const (
	routeMACContext = "cleat-slack-route\x00"

	routeMACRawBytes = 16 // truncated HMAC-SHA256 output
	routeMACLen      = 22 // base64url, no padding, of 16 bytes
	routeIssuedAtLen = 8  // hex-encoded uint32 unix seconds

	// ":" + issued_at + ":" + mac
	routeTailLen = 1 + routeIssuedAtLen + 1 + routeMACLen

	// slackActionIDMaxLen is Slack's own documented bound on a block
	// element's action_id. A signed route adds routeTailLen fixed
	// characters (32) on top of the unsigned "wf:<id>:sig:<name>" a
	// workflow author writes -- with a UUID workflow id (36 chars) that
	// leaves roughly 179 characters of headroom for signal_name before
	// stamping would produce an action_id Slack itself would reject. The
	// owner's explicit instruction (cleat#2230) is to refuse at send time
	// when this is exceeded, not to truncate -- a truncated signed route
	// would fail MAC verification on every click, silently, with no
	// button ever working and no error anywhere near the cause. See
	// stampNode.
	slackActionIDMaxLen = 255

	// defaultRouteMaxAge is owner decision 2A on cleat#2230: clicks older
	// than this are refused. Configurable via Config.RouteMaxAgeDays.
	defaultRouteMaxAge = 30 * 24 * time.Hour

	// routeClockSkewAllowance tolerates a route whose issued-at is slightly
	// in the future -- clock skew between workers, not a design point on its
	// own. Generous relative to NTP drift, tight relative to
	// defaultRouteMaxAge.
	routeClockSkewAllowance = 5 * time.Minute
)

// looksLikeUnsignedRoute reports whether s is an UNSIGNED wf:<id>:sig:<name>
// route -- the shape a workflow author writes into their own Blocks JSON,
// before sendMessage stamps it. Reuses parseCallbackRoute rather than a
// second pattern, so the two can never disagree about what counts as a
// route.
//
// EXCLUDES anything that ALSO parses as a valid signed route. This matters
// because parseCallbackRoute's SplitN(s, ":", 4) is deliberately permissive
// about what comes after the third colon (signal_name may itself contain
// ':') -- so an already-signed action_id, wf:<id>:sig:<name>:<issued_at>:<mac>,
// trivially ALSO satisfies "parses as wf:...:sig:...", with the whole
// ":<issued_at>:<mac>" tail absorbed into signal_name. Measured directly:
// without this exclusion, stampBlocksWithRoutes' own post-walk "nothing
// unsigned survived" check flagged its OWN freshly-signed output as still
// unsigned. The exclusion trades one theoretical false negative -- an
// author's plain signal_name that, by pure coincidence, ends in something
// shaped exactly like ":<8 lowercase hex>:<22 base64url chars>" would be
// treated as "already signed" and left untouched, so it goes out unsigned
// and simply never routes (verifyRouteSignature fails MAC-fresh, without
// the real key) -- for a security property that must never fail open: this
// makes the failure mode "this specific button silently never fires",
// never "an unsigned route passes as signed".
func looksLikeUnsignedRoute(s string) bool {
	if _, _, ok := parseCallbackRoute(s); !ok {
		return false
	}
	if _, _, _, ok := parseSignedRoute(s); ok {
		return false
	}
	return true
}

// routeMAC computes the truncated HMAC-SHA256 over every field a signed
// route binds: which deployment key signed it, which tenant it was minted
// for, the unsigned route itself (which is exactly "wf:<id>:sig:<name>" and
// so already carries workflow id and signal name as one unambiguous unit),
// when it was minted, and the clicked element's block_id and value --
// cleat-review's addendum requirement that a route cannot be replayed onto
// a different button by carrying its action_id to another block_id/value
// pair. Each field is written null-terminated after a context prefix so
// that no concatenation of the parts can collide with a different
// partition of the same bytes.
func routeMAC(key []byte, tenantID, unsignedRoute, issuedAtHex, blockID, value string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(routeMACContext))
	for _, part := range []string{tenantID, unsignedRoute, issuedAtHex, blockID, value} {
		mac.Write([]byte(part))
		mac.Write([]byte{0})
	}
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil)[:routeMACRawBytes])
}

// signRoute stamps unsignedRoute with an issued-at timestamp and a MAC,
// producing the string that goes into a button's action_id on the wire.
//
// tenantID MUST come from the host call context
// (plugin.CallContextFromContext(ctx).TenantID) -- never from a field the
// workflow itself supplies in its Blocks JSON, or any other source. That is
// the whole security property: the tenant a button is bound to is the
// tenant the ENGINE says is running the workflow, not a claim the workflow
// makes about itself. See stampBlocksWithRoutes and its caller in
// host_functions.go.
//
// Callers only invoke this after confirming looksLikeUnsignedRoute(unsignedRoute);
// it does not re-validate, so a malformed unsignedRoute here would produce a
// tail-shaped-but-meaningless string rather than fail loudly -- acceptable
// because stampNode is the only caller and always checks first.
func signRoute(key []byte, unsignedRoute, tenantID, blockID, value string, issuedAt time.Time) string {
	issuedAtHex := fmt.Sprintf("%08x", uint32(issuedAt.Unix()))
	mac := routeMAC(key, tenantID, unsignedRoute, issuedAtHex, blockID, value)
	return unsignedRoute + ":" + issuedAtHex + ":" + mac
}

// parseSignedRoute splits a signed action_id/callback_id into its unsigned
// route and signing tail, validating that the unsigned portion parses via
// parseCallbackRoute. It does NOT verify the MAC or the age -- that needs
// the resolved tenant, which this route's caller does not have yet (see
// handleInteractiveCallback: extraction happens before tenant resolution,
// verification after).
func parseSignedRoute(s string) (unsignedRoute, issuedAtHex, mac string, ok bool) {
	if len(s) <= routeTailLen {
		return "", "", "", false
	}
	unsignedRoute = s[:len(s)-routeTailLen]
	tail := s[len(s)-routeTailLen:]
	if tail[0] != ':' || tail[1+routeIssuedAtLen] != ':' {
		return "", "", "", false
	}
	issuedAtHex = tail[1 : 1+routeIssuedAtLen]
	mac = tail[1+routeIssuedAtLen+1:]
	if _, _, ok := parseCallbackRoute(unsignedRoute); !ok {
		return "", "", "", false
	}
	return unsignedRoute, issuedAtHex, mac, true
}

// verifyRouteSignature checks a signed route's MAC (under currentKey, then
// previousKey if set and currentKey didn't match -- the rotation grace
// period from the design addendum) and its age. blockID and value must be
// the ones from the ACTUAL click, not re-derived from the route string --
// they are part of what the MAC binds, precisely so a route cannot be
// replayed onto a different block_id/value pair than the one it was minted
// for.
func verifyRouteSignature(currentKey, previousKey []byte, tenantID, unsignedRoute, issuedAtHex, blockID, value, mac string, maxAge time.Duration, now time.Time) (usedPrevious, ok bool) {
	matched := hmac.Equal([]byte(mac), []byte(routeMAC(currentKey, tenantID, unsignedRoute, issuedAtHex, blockID, value)))
	if !matched && len(previousKey) > 0 {
		if hmac.Equal([]byte(mac), []byte(routeMAC(previousKey, tenantID, unsignedRoute, issuedAtHex, blockID, value))) {
			matched = true
			usedPrevious = true
		}
	}
	if !matched {
		return usedPrevious, false
	}
	secs, err := strconv.ParseUint(issuedAtHex, 16, 32)
	if err != nil {
		return usedPrevious, false
	}
	issuedAt := time.Unix(int64(secs), 0)
	if now.Sub(issuedAt) > maxAge {
		return usedPrevious, false
	}
	if issuedAt.Sub(now) > routeClockSkewAllowance {
		return usedPrevious, false
	}
	return usedPrevious, true
}

// hasUnsignedRoute reports whether raw (an outbound Blocks JSON tree)
// contains any "action_id" whose value looks like an unsigned route --
// i.e., whether sendMessage needs to fetch the signing key and stamp
// anything at all. A message with no buttons, or buttons with plain,
// non-routing action_ids, costs no deployment-secret lookup.
func hasUnsignedRoute(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var tree any
	if err := json.Unmarshal(raw, &tree); err != nil {
		return false
	}
	return findActionIDMatching(tree, looksLikeUnsignedRoute) != ""
}

// stampBlocksWithRoutes finds every "action_id" in raw that looks like an
// unsigned wf:<id>:sig:<name> route and rewrites it to the signed form.
//
// GENERIC ON PURPOSE (cleat-review's addendum requirement): rather than
// assuming Block Kit's usual blocks[].elements[] or
// attachments[].blocks[].elements[] nesting, this walks every map and slice
// in the decoded tree unconditionally, regardless of the surrounding key
// names. Two things follow:
//
//   - "block_id" is Slack's convention of living on the block object that
//     ENCLOSES the button element carrying "action_id" and "value" -- so the
//     walk carries the nearest ancestor's block_id DOWN through the
//     recursion (inherited by descendants, overridden by a closer one)
//     rather than reading one fixed relative path.
//   - an action_id is stamped wherever it appears, including inside
//     "attachments" or any nesting this plugin has never specifically
//     handled.
//
// issuedAt is a parameter rather than time.Now() internally so a test can
// pin it.
//
// After the walk, a second, independent pass over the RE-MARSHALED output
// confirms no unsigned route survives anywhere -- defense in depth against
// a walker bug, rather than trusting the recursion's own completeness. The
// owner's instruction is that an unstamped route must never pass through,
// not "should not, so far as this code can tell".
func stampBlocksWithRoutes(raw json.RawMessage, key []byte, tenantID string, issuedAt time.Time) (json.RawMessage, error) {
	if len(raw) == 0 {
		return raw, nil
	}
	var tree any
	if err := json.Unmarshal(raw, &tree); err != nil {
		return nil, fmt.Errorf("slack-notify: blocks is not valid JSON: %w", err)
	}
	if err := stampNode(tree, key, tenantID, issuedAt, ""); err != nil {
		return nil, err
	}
	out, err := json.Marshal(tree)
	if err != nil {
		return nil, fmt.Errorf("slack-notify: re-marshal stamped blocks: %w", err)
	}
	var reparsed any
	if err := json.Unmarshal(out, &reparsed); err != nil {
		return nil, fmt.Errorf("slack-notify: re-parse stamped blocks: %w", err)
	}
	if survivor := findActionIDMatching(reparsed, looksLikeUnsignedRoute); survivor != "" {
		return nil, fmt.Errorf("slack-notify: an unsigned route (%q) survived stamping -- refusing to send", survivor)
	}
	return out, nil
}

// stampNode is stampBlocksWithRoutes' recursion. ancestorBlockID is the
// nearest enclosing object's "block_id", inherited unless this node has its
// own. Returns an error, and stops recursing, the moment a stamped
// action_id would exceed slackActionIDMaxLen -- a hard refusal rather than a
// truncation, per the owner's explicit instruction (cleat#2230): a truncated
// signed route would fail MAC verification on every click, silently.
func stampNode(node any, key []byte, tenantID string, issuedAt time.Time, ancestorBlockID string) error {
	switch v := node.(type) {
	case map[string]any:
		blockID := ancestorBlockID
		if bid, ok := v["block_id"].(string); ok && bid != "" {
			blockID = bid
		}
		if actionID, ok := v["action_id"].(string); ok && looksLikeUnsignedRoute(actionID) {
			value, _ := v["value"].(string)
			signed := signRoute(key, actionID, tenantID, blockID, value, issuedAt)
			if len(signed) > slackActionIDMaxLen {
				return fmt.Errorf("slack-notify: signing %q would produce a %d-character action_id, over Slack's %d-character limit -- shorten the workflow id or signal name", actionID, len(signed), slackActionIDMaxLen)
			}
			v["action_id"] = signed
		}
		for _, child := range v {
			if err := stampNode(child, key, tenantID, issuedAt, blockID); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range v {
			if err := stampNode(child, key, tenantID, issuedAt, ancestorBlockID); err != nil {
				return err
			}
		}
	}
	return nil
}

// findActionIDMatching walks node the same way stampNode does and returns
// the first "action_id" string value for which match returns true, or ""
// if none. Shared by hasUnsignedRoute (pre-scan, saves a secret lookup for
// plain messages) and stampBlocksWithRoutes' post-walk defense-in-depth
// check.
func findActionIDMatching(node any, match func(string) bool) string {
	switch v := node.(type) {
	case map[string]any:
		if actionID, ok := v["action_id"].(string); ok && match(actionID) {
			return actionID
		}
		for _, child := range v {
			if found := findActionIDMatching(child, match); found != "" {
				return found
			}
		}
	case []any:
		for _, child := range v {
			if found := findActionIDMatching(child, match); found != "" {
				return found
			}
		}
	}
	return ""
}

// routeSigningKey fetches the deployment's current signed-route MAC key,
// plus the previous one if the operator has set one for a rotation grace
// period (design addendum, cleat#2230 §3). Same per-request, no-cache
// convention as signingSecret (verifySlackSignature's key) -- a rotated or
// retired key takes effect on the very next request in either direction.
//
// previous is "", nil error when unset -- that is the normal, default
// state (no rotation in progress), not a failure.
func (p *Plugin) routeSigningKey(ctx context.Context) (current, previous []byte, err error) {
	if p.deploymentSecrets == nil {
		return nil, nil, fmt.Errorf("slack-notify: no deployment secret store configured")
	}
	cur, err := p.deploymentSecrets.Get(ctx, "slacknotify.route_signing_key")
	if err != nil {
		return nil, nil, fmt.Errorf("slack-notify: route_signing_key: %w", err)
	}
	if cur == "" {
		return nil, nil, fmt.Errorf("slack-notify: route_signing_key: empty")
	}
	prev, prevErr := p.deploymentSecrets.Get(ctx, "slacknotify.route_signing_key.previous")
	if prevErr != nil || prev == "" {
		prev = ""
	}
	return []byte(cur), []byte(prev), nil
}

// routeMaxAge is owner decision 2A: a configurable maximum age, default 30
// days. RouteMaxAgeDays <= 0 (including unset) means the default.
func (c Config) routeMaxAge() time.Duration {
	if c.RouteMaxAgeDays <= 0 {
		return defaultRouteMaxAge
	}
	return time.Duration(c.RouteMaxAgeDays) * 24 * time.Hour
}
