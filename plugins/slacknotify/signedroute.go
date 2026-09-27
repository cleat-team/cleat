package slacknotify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
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

	// routeSigningKeyMinLen is the minimum length, in bytes, accepted for
	// slacknotify.route_signing_key (and .previous, if set) -- coordinator's
	// call after cleat-review's mutation pass found that deleting the empty
	// check on the current key left no test red. A short or empty key makes
	// the MAC guessable; 32 bytes matches the HMAC-SHA256 block-adjacent key
	// size convention used elsewhere in this codebase (e.g. slacknotify's
	// own signing_secret has no enforced minimum, which is the gap this
	// closes for the NEW secret rather than retrofitting the old one).
	// Enforced here (routeSigningKey, "at use") and in cleatctl's
	// set-deployment-secret for this specific name (cmd/cleatctl).
	routeSigningKeyMinLen = 32

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

// RouteSigningKeyMinLen exports routeSigningKeyMinLen for
// cmd/cleatctl/setdeploymentsecret.go, which enforces the same floor at
// write time -- coordinator's instruction (cleat#2230) that the minimum
// key length be "enforced at use and in cleatctl". The two live in
// different binaries; this constant is what keeps them from drifting
// apart, rather than each carrying its own copy of 32.
const RouteSigningKeyMinLen = routeSigningKeyMinLen

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

// looksLikeAlreadySignedRoute reports whether s structurally parses as a
// SIGNED route (parseSignedRoute succeeds), independent of whether its MAC
// verifies against any key we hold. At stamp time (stampNode, before any
// signing happens in this call) the only way an action_id can already have
// this shape is if the workflow author wrote it into their own Blocks JSON
// -- a forged or stale route pasted in deliberately, or, vanishingly
// unlikely, an accidental collision. Either way it is not something this
// send is vouching for, so stampNode refuses rather than passing it
// through unsigned-and-unverifiable (cleat-review + coordinator nit,
// cleat#2230).
func looksLikeAlreadySignedRoute(s string) bool {
	_, _, _, ok := parseSignedRoute(s)
	return ok
}

// routeMAC computes the truncated HMAC-SHA256 over every field a signed
// route binds: which deployment key signed it, which tenant it was minted
// for, the unsigned route itself (which is exactly "wf:<id>:sig:<name>" and
// so already carries workflow id and signal name as one unambiguous unit),
// when it was minted, and the clicked element's block_id and value --
// cleat-review's addendum requirement that a route cannot be replayed onto
// a different button by carrying its action_id to another block_id/value
// pair. Each field is written length-prefixed after a context prefix so
// that no concatenation of the parts can collide with a different
// partition of the same bytes -- see the comment inline below for why
// length-prefixing, not NUL-termination.
func routeMAC(key []byte, tenantID, unsignedRoute, issuedAtHex, blockID, value string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(routeMACContext))
	// Length-PREFIXED, not NUL-terminated: cleat-review's mutation pass
	// found the previous NUL-joined framing ambiguous -- "a\x00" + "b" and
	// "a" + "\x00b" write the identical byte stream ("a\x00\x00b\x00" ...),
	// so two different (blockID, value) splits of one payload could hash to
	// the same MAC input if either field could carry a literal NUL byte
	// (both come from a real Slack click, which this handler does not
	// otherwise restrict to be NUL-free). An 8-byte big-endian length
	// before each field is unambiguous regardless of content.
	var lenBuf [8]byte
	for _, part := range []string{tenantID, unsignedRoute, issuedAtHex, blockID, value} {
		binary.BigEndian.PutUint64(lenBuf[:], uint64(len(part)))
		mac.Write(lenBuf[:])
		mac.Write([]byte(part))
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
	if err := decodeJSONPreservingNumbers(raw, &tree); err != nil {
		return false
	}
	return findActionIDMatching(tree, looksLikeUnsignedRoute) != ""
}

// decodeJSONPreservingNumbers decodes raw the way stampBlocksWithRoutes and
// hasUnsignedRoute need to: a plain json.Unmarshal into `any` represents
// every JSON number as float64, which cannot exactly represent integers
// past 2^53 -- so a workflow author's large numeric literal anywhere in
// Blocks (a snowflake-style id in "value", say) would silently change value
// across the decode/re-encode round trip stampBlocksWithRoutes performs.
// UseNumber keeps each number as json.Number (string-backed), so re-marshal
// reproduces it exactly.
func decodeJSONPreservingNumbers(raw []byte, out any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	return dec.Decode(out)
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
	if err := decodeJSONPreservingNumbers(raw, &tree); err != nil {
		return nil, fmt.Errorf("slack-notify: blocks is not valid JSON: %w", err)
	}
	if err := stampNode(tree, key, tenantID, issuedAt, nil); err != nil {
		return nil, err
	}
	out, err := json.Marshal(tree)
	if err != nil {
		return nil, fmt.Errorf("slack-notify: re-marshal stamped blocks: %w", err)
	}
	var reparsed any
	if err := decodeJSONPreservingNumbers(out, &reparsed); err != nil {
		return nil, fmt.Errorf("slack-notify: re-parse stamped blocks: %w", err)
	}
	if survivor := findActionIDMatching(reparsed, looksLikeUnsignedRoute); survivor != "" {
		return nil, fmt.Errorf("slack-notify: an unsigned route (%q) survived stamping -- refusing to send", survivor)
	}
	return out, nil
}

// stampNode is stampBlocksWithRoutes' recursion. ancestorBlock is the
// nearest enclosing object -- a live map reference, not a copied block_id
// string -- so that a block_id INJECTED for one button (see below) is
// visible, by the same reference, to a sibling button processed afterward
// under the same block.
//
// Returns an error, and stops recursing, the moment a stamped action_id
// would exceed slackActionIDMaxLen -- a hard refusal rather than a
// truncation, per the owner's explicit instruction (cleat#2230): a truncated
// signed route would fail MAC verification on every click, silently.
//
// BLOCK_ID INJECTION (cleat-review + coordinator, cleat#2230 real-dialect
// round): a block with no author-set block_id gets one auto-generated by
// SLACK at render time, and Slack echoes THAT value back on click -- not
// empty string, which is what got signed here originally. Every button
// under such a block therefore 404'd on every real click, on all three
// dialects, because nothing this plugin ever sent matched what came back.
// The fix is to inject our OWN block_id at stamp time, into the actual
// object we are about to send, so Slack has no reason to generate one --
// it only does that when the field is ABSENT, never when it already holds
// a value. What we sign is then guaranteed to be what Slack echoes.
func stampNode(node any, key []byte, tenantID string, issuedAt time.Time, ancestorBlock map[string]any) error {
	switch v := node.(type) {
	case map[string]any:
		// isElement: this map carries its own "action_id", so it is an
		// interactive ELEMENT (button, etc.), never the layout block that
		// encloses one -- Slack's own schema never puts both on the same
		// object. Excluding it from candidacy is what makes a button not
		// its own ancestor: a button with no explicit block_id anywhere
		// above it must inject onto whatever REAL enclosing map is above
		// it, never onto itself.
		//
		// Every OTHER map -- whether or not it happens to carry a
		// "block_id" key yet -- is a valid injection candidate for
		// whatever nests beneath it. This is what the earlier version of
		// this function got wrong: it only registered a map as the
		// candidate when block_id was ALREADY present, so a block with none
		// set was invisible to the walk and a button under it fell back to
		// using ITSELF (a button object, never rendered as a block at all)
		// as the injection target -- silently landing block_id on the
		// wrong object and leaving the real block still absent one, which
		// is exactly the bug this rewrite exists to fix. See
		// TestStampBlocksWithRoutes_InjectsBlockIDWhenAbsent.
		_, isElement := v["action_id"]
		childAncestor := ancestorBlock
		if !isElement {
			childAncestor = v
		}
		if actionID, ok := v["action_id"].(string); ok {
			switch {
			case looksLikeUnsignedRoute(actionID):
				target := ancestorBlock
				if target == nil {
					// No enclosing map was ever found (a button with
					// nothing above it in the tree) -- fall back to this
					// object itself rather than losing the route entirely.
					// Not the expected shape for real Slack Blocks JSON.
					target = v
				}
				blockID, _ := target["block_id"].(string)
				if blockID == "" {
					blockID = generateBlockID()
					target["block_id"] = blockID
				}
				value, _ := v["value"].(string)
				signed := signRoute(key, actionID, tenantID, blockID, value, issuedAt)
				if len(signed) > slackActionIDMaxLen {
					return fmt.Errorf("slack-notify: signing %q would produce a %d-character action_id, over Slack's %d-character limit -- shorten the workflow id or signal name", actionID, len(signed), slackActionIDMaxLen)
				}
				v["action_id"] = signed
			case looksLikeAlreadySignedRoute(actionID):
				// A guest-authored workflow's own action_id happens to have
				// the shape of OUR signed wire format, but was not produced
				// by this call -- it carries no valid MAC under this key.
				// The design calls for refusing outright rather than
				// sending a button whose click can never verify.
				return fmt.Errorf("slack-notify: action_id %q already has the shape of a signed route but was not signed by this send -- refusing to send an unverifiable button", actionID)
			}
		}
		for _, child := range v {
			if err := stampNode(child, key, tenantID, issuedAt, childAncestor); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range v {
			if err := stampNode(child, key, tenantID, issuedAt, ancestorBlock); err != nil {
				return err
			}
		}
	}
	return nil
}

// generateBlockID produces a fresh, opaque block_id for a layout block the
// workflow author left unset. See stampNode's doc comment for why this is
// injected at stamp time rather than left for Slack to assign.
func generateBlockID() string {
	return "cleat_" + uuid.New().String()
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
	if len(cur) < routeSigningKeyMinLen {
		return nil, nil, fmt.Errorf("slack-notify: route_signing_key: %d bytes, below the %d-byte minimum", len(cur), routeSigningKeyMinLen)
	}
	prev, prevErr := p.deploymentSecrets.Get(ctx, "slacknotify.route_signing_key.previous")
	if prevErr != nil || prev == "" {
		prev = ""
	}
	// A too-short .previous is discarded rather than refused outright: it
	// only ever weakens the rotation-grace fallback (verifyRouteSignature
	// simply won't match against it any more usefully than an absent one
	// would), it never widens what a CURRENT route can be forged with, and
	// refusing here would turn an operator's mistake on the OLD key into an
	// outage for every route signed under the still-valid current one.
	if prev != "" && len(prev) < routeSigningKeyMinLen {
		p.logger.Warn("slack-notify: route_signing_key.previous is shorter than the minimum -- ignoring it", "len", len(prev), "min", routeSigningKeyMinLen)
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
