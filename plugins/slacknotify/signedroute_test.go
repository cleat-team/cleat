package slacknotify

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

const testSignedRouteKey = "unit-test-signed-route-key-abc123"

// TestSignRouteParseVerifyRoundTrip pins the basic shape: sign, parse, and
// verify agree on a fresh route.
func TestSignRouteParseVerifyRoundTrip(t *testing.T) {
	unsignedRoute := "wf:wf-1:sig:approve"
	tenantID := uuid.New().String()
	now := time.Now()
	signed := signRoute([]byte(testSignedRouteKey), unsignedRoute, tenantID, "block-1", "yes", now)

	gotUnsigned, issuedAtHex, mac, ok := parseSignedRoute(signed)
	if !ok {
		t.Fatalf("parseSignedRoute(%q) ok=false", signed)
	}
	if gotUnsigned != unsignedRoute {
		t.Errorf("parsed unsigned route = %q, want %q", gotUnsigned, unsignedRoute)
	}

	usedPrevious, sigOK := verifyRouteSignature([]byte(testSignedRouteKey), nil, tenantID, gotUnsigned, issuedAtHex, "block-1", "yes", mac, defaultRouteMaxAge, now)
	if !sigOK || usedPrevious {
		t.Errorf("verifyRouteSignature = (usedPrevious=%v, ok=%v), want (false, true)", usedPrevious, sigOK)
	}
}

// TestVerifyRouteSignature_TamperedFieldsRefuse checks that the MAC actually
// binds each field it claims to: tenant, unsigned route, issued-at,
// block_id, and value. Flipping any ONE must break verification against the
// values used to sign.
//
// "wrong issued_at" uses a FRESH-but-different timestamp (now+1 second), not
// epoch zero. Epoch zero is also refused by verifyRouteSignature's separate
// max-age check, so a case built that way passes whether or not the MAC
// actually binds issuedAtHex at all -- cleat-review's mutation pass found
// exactly this: deleting issuedAtHex from routeMAC's inputs left this test
// green, because "wrong issued_at" was never isolated from "old issued_at".
// A timestamp one second later than the one actually signed is still
// comfortably inside defaultRouteMaxAge and the clock-skew allowance, so
// this case can only fail via the MAC, never the age check.
func TestVerifyRouteSignature_TamperedFieldsRefuse(t *testing.T) {
	tenantID := uuid.New().String()
	otherTenantID := uuid.New().String()
	now := time.Now()
	unsignedRoute := "wf:wf-1:sig:approve"
	signed := signRoute([]byte(testSignedRouteKey), unsignedRoute, tenantID, "block-1", "yes", now)
	_, issuedAtHex, mac, ok := parseSignedRoute(signed)
	if !ok {
		t.Fatalf("parseSignedRoute(%q) ok=false", signed)
	}
	issuedAtSecs, err := strconv.ParseUint(issuedAtHex, 16, 32)
	if err != nil {
		t.Fatalf("parsing issuedAtHex %q: %v", issuedAtHex, err)
	}
	freshButDifferentIssuedAtHex := fmt.Sprintf("%08x", issuedAtSecs+1)
	if freshButDifferentIssuedAtHex == issuedAtHex {
		t.Fatal("test fixture bug: freshButDifferentIssuedAtHex must differ from issuedAtHex")
	}

	cases := []struct {
		name                                       string
		tenantID, route, issuedAtHex, blockID, val string
	}{
		{"wrong tenant", otherTenantID, unsignedRoute, issuedAtHex, "block-1", "yes"},
		{"wrong route", tenantID, "wf:wf-2:sig:approve", issuedAtHex, "block-1", "yes"},
		{"wrong issued_at (epoch zero -- also caught by the age check alone)", tenantID, unsignedRoute, "00000000", "block-1", "yes"},
		{"wrong issued_at (fresh, isolates MAC binding from the age check)", tenantID, unsignedRoute, freshButDifferentIssuedAtHex, "block-1", "yes"},
		{"wrong block_id", tenantID, unsignedRoute, issuedAtHex, "block-2", "yes"},
		{"wrong value", tenantID, unsignedRoute, issuedAtHex, "block-1", "no"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, sigOK := verifyRouteSignature([]byte(testSignedRouteKey), nil, tc.tenantID, tc.route, tc.issuedAtHex, tc.blockID, tc.val, mac, defaultRouteMaxAge, now)
			if sigOK {
				t.Errorf("verifyRouteSignature unexpectedly succeeded with %s tampered", tc.name)
			}
		})
	}
}

// TestVerifyRouteSignature_PreviousKeyGrace pins the rotation grace period
// directly: a route signed under the OLD key verifies (usedPrevious=true)
// once the current key has moved on, and stops verifying once the previous
// key is retired (nil/empty).
func TestVerifyRouteSignature_PreviousKeyGrace(t *testing.T) {
	oldKey := []byte("old-key-0123456789abcdef")
	newKey := []byte("new-key-0123456789abcdef")
	tenantID := uuid.New().String()
	now := time.Now()
	signed := signRoute(oldKey, "wf:wf-1:sig:go", tenantID, "", "", now)
	_, issuedAtHex, mac, _ := parseSignedRoute(signed)

	usedPrevious, ok := verifyRouteSignature(newKey, oldKey, tenantID, "wf:wf-1:sig:go", issuedAtHex, "", "", mac, defaultRouteMaxAge, now)
	if !ok || !usedPrevious {
		t.Errorf("expected verification to succeed via the previous key, got ok=%v usedPrevious=%v", ok, usedPrevious)
	}

	if _, ok := verifyRouteSignature(newKey, nil, tenantID, "wf:wf-1:sig:go", issuedAtHex, "", "", mac, defaultRouteMaxAge, now); ok {
		t.Error("expected verification to fail once the previous key is retired (nil)")
	}
}

// TestVerifyRouteSignature_ClockSkew pins routeClockSkewAllowance: a route
// issued slightly in the future (worker clock skew) still verifies; one
// issued far in the future does not.
func TestVerifyRouteSignature_ClockSkew(t *testing.T) {
	tenantID := uuid.New().String()
	now := time.Now()
	unsignedRoute := "wf:wf-1:sig:go"

	withinSkew := signRoute([]byte(testSignedRouteKey), unsignedRoute, tenantID, "", "", now.Add(2*time.Minute))
	_, issuedAtHex, mac, _ := parseSignedRoute(withinSkew)
	if _, ok := verifyRouteSignature([]byte(testSignedRouteKey), nil, tenantID, unsignedRoute, issuedAtHex, "", "", mac, defaultRouteMaxAge, now); !ok {
		t.Error("expected a route issued 2 minutes in the future (within skew allowance) to verify")
	}

	farFuture := signRoute([]byte(testSignedRouteKey), unsignedRoute, tenantID, "", "", now.Add(time.Hour))
	_, issuedAtHex, mac, _ = parseSignedRoute(farFuture)
	if _, ok := verifyRouteSignature([]byte(testSignedRouteKey), nil, tenantID, unsignedRoute, issuedAtHex, "", "", mac, defaultRouteMaxAge, now); ok {
		t.Error("expected a route issued an hour in the future to be refused")
	}
}

// TestLooksLikeUnsignedRoute_ExcludesSignedRoutes is the defense-in-depth
// property signedroute.go's own doc comment names: a freshly signed route
// must NOT be reported as "still unsigned" by the same function
// stampBlocksWithRoutes uses to check its own output, or the post-walk
// safety check would refuse every message it just correctly stamped.
func TestLooksLikeUnsignedRoute_ExcludesSignedRoutes(t *testing.T) {
	unsignedRoute := "wf:wf-1:sig:approve"
	if !looksLikeUnsignedRoute(unsignedRoute) {
		t.Errorf("expected %q to look unsigned", unsignedRoute)
	}
	signed := signRoute([]byte(testSignedRouteKey), unsignedRoute, uuid.New().String(), "", "", time.Now())
	if looksLikeUnsignedRoute(signed) {
		t.Errorf("expected the freshly signed route %q to NOT look unsigned", signed)
	}
}

// TestStampBlocksWithRoutes_GenericWalk is cleat-review's addendum
// requirement, pinned directly: the walk stamps action_id wherever it
// appears -- inside blocks[].elements[], inside attachments[].blocks[], and
// anywhere else -- without assuming a fixed nesting shape, and carries the
// nearest enclosing block_id down through the recursion.
func TestStampBlocksWithRoutes_GenericWalk(t *testing.T) {
	tenantID := uuid.New().String()
	raw := json.RawMessage(`{
		"blocks": [
			{"type": "section", "block_id": "b1", "accessory": {
				"type": "button", "action_id": "wf:wf-1:sig:a", "value": "v1"
			}}
		],
		"attachments": [
			{"blocks": [
				{"type": "actions", "block_id": "b2", "elements": [
					{"type": "button", "action_id": "wf:wf-2:sig:b", "value": "v2"}
				]}
			]}
		]
	}`)

	stamped, err := stampBlocksWithRoutes(raw, []byte(testSignedRouteKey), tenantID, time.Now())
	if err != nil {
		t.Fatalf("stampBlocksWithRoutes: %v", err)
	}

	var tree any
	if err := json.Unmarshal(stamped, &tree); err != nil {
		t.Fatalf("stamped output is not valid JSON: %v", err)
	}
	if survivor := findActionIDMatching(tree, looksLikeUnsignedRoute); survivor != "" {
		t.Fatalf("an unsigned route survived stamping: %q", survivor)
	}

	// Walk the two action_ids out by hand and check each one verifies
	// under the block_id/value it actually sits beside -- proving the
	// walk attached the RIGHT ancestor block_id to each, not just any.
	m := map[string]any{}
	if err := json.Unmarshal(stamped, &m); err != nil {
		t.Fatal(err)
	}
	accessory := m["blocks"].([]any)[0].(map[string]any)["accessory"].(map[string]any)
	signed1 := accessory["action_id"].(string)
	unsigned1, issuedAtHex1, mac1, ok := parseSignedRoute(signed1)
	if !ok || unsigned1 != "wf:wf-1:sig:a" {
		t.Fatalf("first action_id parsed as (%q, ok=%v), want wf:wf-1:sig:a", unsigned1, ok)
	}
	if _, ok := verifyRouteSignature([]byte(testSignedRouteKey), nil, tenantID, unsigned1, issuedAtHex1, "b1", "v1", mac1, defaultRouteMaxAge, time.Now()); !ok {
		t.Error("first action_id did not verify under its own section's block_id b1 and value v1")
	}

	elements := m["attachments"].([]any)[0].(map[string]any)["blocks"].([]any)[0].(map[string]any)["elements"].([]any)
	signed2 := elements[0].(map[string]any)["action_id"].(string)
	unsigned2, issuedAtHex2, mac2, ok := parseSignedRoute(signed2)
	if !ok || unsigned2 != "wf:wf-2:sig:b" {
		t.Fatalf("second action_id parsed as (%q, ok=%v), want wf:wf-2:sig:b", unsigned2, ok)
	}
	if _, ok := verifyRouteSignature([]byte(testSignedRouteKey), nil, tenantID, unsigned2, issuedAtHex2, "b2", "v2", mac2, defaultRouteMaxAge, time.Now()); !ok {
		t.Error("second action_id did not verify under its own actions block's block_id b2 and value v2")
	}
}

// TestStampBlocksWithRoutes_OversizedActionIDRefuses is the owner's explicit
// instruction on cleat#2230: a computed signed action_id over Slack's
// 255-character action_id limit must hard-refuse the send, never silently
// truncate -- a truncated route would fail MAC verification on every click,
// with the button simply never working and nothing at send time explaining
// why.
func TestStampBlocksWithRoutes_OversizedActionIDRefuses(t *testing.T) {
	tenantID := uuid.New().String()
	// unsignedRoute = "wf:" + wfID + ":sig:" + sigName -- pad sigName so
	// the SIGNED length (unsigned + 32-char tail) comfortably exceeds 255.
	longSignalName := strings.Repeat("x", 230)
	raw := json.RawMessage(`{"blocks":[{"type":"actions","block_id":"b1","elements":[
		{"type":"button","action_id":"wf:wf-1:sig:` + longSignalName + `"}
	]}]}`)

	_, err := stampBlocksWithRoutes(raw, []byte(testSignedRouteKey), tenantID, time.Now())
	if err == nil {
		t.Fatal("expected stampBlocksWithRoutes to refuse an oversized signed action_id, got nil error")
	}
	if !strings.Contains(err.Error(), "255") {
		t.Errorf("expected the refusal to mention the 255-character limit, got: %v", err)
	}
}

// TestStampBlocksWithRoutes_UnderBoundaryPasses is the negative control for
// the oversized-refusal test above: a route just under the boundary must
// still be stamped and pass through normally, so the check above is known
// to test the boundary rather than something else about long signal names.
func TestStampBlocksWithRoutes_UnderBoundaryPasses(t *testing.T) {
	tenantID := uuid.New().String()
	// unsigned = 8 + len("wf-1") + len(signalName) = 12 + len(signalName).
	// signed = unsigned + 32. Aim comfortably under 255.
	shortSignalName := strings.Repeat("x", 150)
	raw := json.RawMessage(`{"blocks":[{"type":"actions","block_id":"b1","elements":[
		{"type":"button","action_id":"wf:wf-1:sig:` + shortSignalName + `"}
	]}]}`)

	stamped, err := stampBlocksWithRoutes(raw, []byte(testSignedRouteKey), tenantID, time.Now())
	if err != nil {
		t.Fatalf("stampBlocksWithRoutes: unexpected error for an under-boundary route: %v", err)
	}
	if survivor := func() string {
		var tree any
		json.Unmarshal(stamped, &tree)
		return findActionIDMatching(tree, looksLikeUnsignedRoute)
	}(); survivor != "" {
		t.Errorf("an unsigned route survived stamping: %q", survivor)
	}
}

// TestHasUnsignedRoute pins the cheap pre-scan sendMessage uses to decide
// whether it needs the signing key at all.
func TestHasUnsignedRoute(t *testing.T) {
	tests := []struct {
		name string
		raw  json.RawMessage
		want bool
	}{
		{"empty", nil, false},
		{"plain text block, no buttons", json.RawMessage(`{"blocks":[{"type":"section","text":{"type":"mrkdwn","text":"hi"}}]}`), false},
		{"non-routing action_id", json.RawMessage(`{"blocks":[{"type":"actions","elements":[{"type":"button","action_id":"just-a-label"}]}]}`), false},
		{"routing action_id", json.RawMessage(`{"blocks":[{"type":"actions","elements":[{"type":"button","action_id":"wf:wf-1:sig:go"}]}]}`), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasUnsignedRoute(tc.raw); got != tc.want {
				t.Errorf("hasUnsignedRoute(%s) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

// TestConfigRouteMaxAge pins owner decision 2A: <= 0 (including unset) means
// the 30-day default, and a positive value overrides it.
func TestConfigRouteMaxAge(t *testing.T) {
	tests := []struct {
		days int
		want time.Duration
	}{
		{0, defaultRouteMaxAge},
		{-5, defaultRouteMaxAge},
		{7, 7 * 24 * time.Hour},
	}
	for _, tc := range tests {
		c := Config{RouteMaxAgeDays: tc.days}
		if got := c.routeMaxAge(); got != tc.want {
			t.Errorf("Config{RouteMaxAgeDays: %d}.routeMaxAge() = %v, want %v", tc.days, got, tc.want)
		}
	}
}

// TestStampBlocksWithRoutes_InjectsBlockIDWhenAbsent is the direct pin for
// cleat-review + coordinator's real-dialect finding: a block with no
// author-set block_id got signed with blockID="", but Slack auto-generates
// its OWN block_id at render time when the field is absent and echoes THAT
// back on click -- never empty string -- so every such button 404'd on
// every real click, on all three dialects. The fix injects a block_id at
// STAMP time so the field is never absent, and this asserts the injected
// value actually lands on the block object (not just internally
// remembered), and that the action_id verifies under it.
func TestStampBlocksWithRoutes_InjectsBlockIDWhenAbsent(t *testing.T) {
	tenantID := uuid.New().String()
	raw := json.RawMessage(`{"blocks":[{"type":"actions","elements":[
		{"type":"button","action_id":"wf:wf-1:sig:approve","value":"yes"}
	]}]}`)

	stamped, err := stampBlocksWithRoutes(raw, []byte(testSignedRouteKey), tenantID, time.Now())
	if err != nil {
		t.Fatalf("stampBlocksWithRoutes: %v", err)
	}

	var tree map[string]any
	if err := json.Unmarshal(stamped, &tree); err != nil {
		t.Fatalf("stamped output is not valid JSON: %v", err)
	}
	block := tree["blocks"].([]any)[0].(map[string]any)
	blockID, _ := block["block_id"].(string)
	if blockID == "" {
		t.Fatal("expected stampBlocksWithRoutes to inject a block_id, block still has none")
	}

	elem := block["elements"].([]any)[0].(map[string]any)
	signed := elem["action_id"].(string)
	value := elem["value"].(string)
	unsigned, issuedAtHex, mac, ok := parseSignedRoute(signed)
	if !ok {
		t.Fatalf("parseSignedRoute(%q) ok=false", signed)
	}

	// The whole point: verification against the block_id ACTUALLY WRITTEN
	// onto the object (what Slack will echo back) must succeed...
	if _, ok := verifyRouteSignature([]byte(testSignedRouteKey), nil, tenantID, unsigned, issuedAtHex, blockID, value, mac, defaultRouteMaxAge, time.Now()); !ok {
		t.Error("action_id did not verify under the block_id stampBlocksWithRoutes injected onto the block")
	}
	// ...and verification against an EMPTY block_id -- what the pre-fix code
	// signed with, and what Slack never actually sends -- must now fail,
	// demonstrating this is a real behavior change and not a no-op.
	if _, ok := verifyRouteSignature([]byte(testSignedRouteKey), nil, tenantID, unsigned, issuedAtHex, "", value, mac, defaultRouteMaxAge, time.Now()); ok {
		t.Error("action_id unexpectedly verified under an empty block_id -- injection did not change what was signed")
	}
}

// TestStampBlocksWithRoutes_SiblingButtonsShareInjectedBlockID: two buttons
// under ONE block with no author-set block_id must converge on the SAME
// injected value -- not each mint their own -- because block_id lives on
// the enclosing block object and Slack will echo back whatever ONE value
// actually ended up there, for either button's click.
func TestStampBlocksWithRoutes_SiblingButtonsShareInjectedBlockID(t *testing.T) {
	tenantID := uuid.New().String()
	raw := json.RawMessage(`{"blocks":[{"type":"actions","elements":[
		{"type":"button","action_id":"wf:wf-1:sig:approve","value":"yes"},
		{"type":"button","action_id":"wf:wf-1:sig:reject","value":"no"}
	]}]}`)

	stamped, err := stampBlocksWithRoutes(raw, []byte(testSignedRouteKey), tenantID, time.Now())
	if err != nil {
		t.Fatalf("stampBlocksWithRoutes: %v", err)
	}

	var tree map[string]any
	if err := json.Unmarshal(stamped, &tree); err != nil {
		t.Fatal(err)
	}
	block := tree["blocks"].([]any)[0].(map[string]any)
	blockID, _ := block["block_id"].(string)
	if blockID == "" {
		t.Fatal("expected an injected block_id, got none")
	}
	elements := block["elements"].([]any)

	for i, wantRoute := range []string{"wf:wf-1:sig:approve", "wf:wf-1:sig:reject"} {
		elem := elements[i].(map[string]any)
		signed := elem["action_id"].(string)
		value := elem["value"].(string)
		unsigned, issuedAtHex, mac, ok := parseSignedRoute(signed)
		if !ok || unsigned != wantRoute {
			t.Fatalf("element %d: parsed (%q, ok=%v), want %q", i, unsigned, ok, wantRoute)
		}
		if _, ok := verifyRouteSignature([]byte(testSignedRouteKey), nil, tenantID, unsigned, issuedAtHex, blockID, value, mac, defaultRouteMaxAge, time.Now()); !ok {
			t.Errorf("element %d did not verify under the shared injected block_id %q", i, blockID)
		}
	}
}

// TestStampNode_RefusesGuestSuppliedSignedShapedActionID is the nit
// coordinator asked for: an action_id that ALREADY has the structural shape
// of a signed route (parseSignedRoute succeeds) but was not produced by
// THIS send carries no valid MAC under any key this deployment holds --
// stampNode must refuse the whole send rather than pass it through
// untouched, which is what looksLikeUnsignedRoute's own exclusion would
// otherwise cause (it is deliberately not "unsigned", so it would just be
// skipped and shipped as-is).
func TestStampNode_RefusesGuestSuppliedSignedShapedActionID(t *testing.T) {
	// Signed under a DIFFERENT key -- exactly what a workflow author pasting
	// in a stale or forged route would produce: valid shape, no valid MAC
	// under the deployment's actual key.
	forged := signRoute([]byte("a-completely-different-key-000000"), "wf:wf-1:sig:approve", uuid.New().String(), "b1", "yes", time.Now())
	raw := json.RawMessage(`{"blocks":[{"type":"actions","block_id":"b1","elements":[
		{"type":"button","action_id":"` + forged + `","value":"yes"}
	]}]}`)

	_, err := stampBlocksWithRoutes(raw, []byte(testSignedRouteKey), uuid.New().String(), time.Now())
	if err == nil {
		t.Fatal("expected stampBlocksWithRoutes to refuse a guest-supplied signed-shaped action_id, got nil error")
	}
	if !strings.Contains(err.Error(), "already has the shape of a signed route") {
		t.Errorf("expected the refusal to name the reason, got: %v", err)
	}
}

// TestRouteMAC_LengthPrefixedFramingAvoidsFieldBoundaryCollision is
// cleat-review's exact collision, pinned directly: under the OLD
// NUL-terminated framing, blockID="a\x00"+value="b" and blockID="a"+
// value="\x00b" write the identical byte stream, so their MACs would have
// been equal despite differing at a real click-controlled field boundary.
// The length-prefixed framing must NOT collide on this pair.
func TestRouteMAC_LengthPrefixedFramingAvoidsFieldBoundaryCollision(t *testing.T) {
	key := []byte(testSignedRouteKey)
	tenantID := "tenant-1"
	route := "wf:wf-1:sig:go"
	issuedAtHex := "00000001"

	mac1 := routeMAC(key, tenantID, route, issuedAtHex, "a\x00", "b")
	mac2 := routeMAC(key, tenantID, route, issuedAtHex, "a", "\x00b")
	if mac1 == mac2 {
		t.Fatalf("routeMAC collided across a (blockID, value) field boundary: %q vs %q both produced %q", `"a\x00","b"`, `"a","\x00b"`, mac1)
	}
}

// TestStampBlocksWithRoutes_PreservesLargeIntegers pins the UseNumber
// decode: a JSON integer literal anywhere in Blocks JSON, including well
// outside anything this plugin reads, must survive the
// decode-stamp-re-encode round trip byte-for-byte. Plain json.Unmarshal
// into `any` represents every number as float64, which cannot exactly hold
// integers past 2^53 -- 9223372036854775807 (2^63-1) would silently become
// 9223372036854775808 without UseNumber.
func TestStampBlocksWithRoutes_PreservesLargeIntegers(t *testing.T) {
	tenantID := uuid.New().String()
	const bigInt = "9223372036854775807"
	raw := json.RawMessage(`{"blocks":[{"type":"actions","block_id":"b1","elements":[
		{"type":"button","action_id":"wf:wf-1:sig:go","value":"yes"}
	]}],"metadata":{"event_payload":{"amount":` + bigInt + `}}}`)

	stamped, err := stampBlocksWithRoutes(raw, []byte(testSignedRouteKey), tenantID, time.Now())
	if err != nil {
		t.Fatalf("stampBlocksWithRoutes: %v", err)
	}
	if !strings.Contains(string(stamped), bigInt) {
		t.Errorf("large integer literal did not survive stamping intact; stamped output: %s", stamped)
	}
}

// TestRouteSigningKey pins routeSigningKey's own validation: the pre-existing
// empty-current-key refusal (cleat-review's mutation pass found this had no
// test at all), the new 32-byte minimum on the current key, and a
// too-short previous key being discarded (logged, not refused -- see
// routeSigningKey's doc comment for why a bad OLD key must not take down
// routes signed under a still-valid current one).
func TestRouteSigningKey(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	validKey := strings.Repeat("k", routeSigningKeyMinLen)
	shortKey := strings.Repeat("k", routeSigningKeyMinLen-1)

	t.Run("empty current key refuses", func(t *testing.T) {
		p := &Plugin{logger: quiet, deploymentSecrets: &fakeInteractiveDeploymentSecrets{routeKey: ""}}
		if _, _, err := p.routeSigningKey(context.Background()); err == nil {
			t.Fatal("expected an error for an empty route_signing_key, got nil")
		}
	})

	t.Run("short current key refuses", func(t *testing.T) {
		p := &Plugin{logger: quiet, deploymentSecrets: &fakeInteractiveDeploymentSecrets{routeKey: shortKey}}
		_, _, err := p.routeSigningKey(context.Background())
		if err == nil {
			t.Fatal("expected an error for a route_signing_key under the minimum length, got nil")
		}
		if !strings.Contains(err.Error(), "minimum") {
			t.Errorf("expected the refusal to mention the minimum, got: %v", err)
		}
	})

	t.Run("valid current key, no previous", func(t *testing.T) {
		p := &Plugin{logger: quiet, deploymentSecrets: &fakeInteractiveDeploymentSecrets{routeKey: validKey}}
		cur, prev, err := p.routeSigningKey(context.Background())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if string(cur) != validKey {
			t.Errorf("current key = %q, want %q", cur, validKey)
		}
		if len(prev) != 0 {
			t.Errorf("expected no previous key, got %q", prev)
		}
	})

	t.Run("short previous key is discarded, not refused", func(t *testing.T) {
		p := &Plugin{logger: quiet, deploymentSecrets: &fakeInteractiveDeploymentSecrets{
			routeKey:         validKey,
			routeKeyPrevious: shortKey,
		}}
		cur, prev, err := p.routeSigningKey(context.Background())
		if err != nil {
			t.Fatalf("a short PREVIOUS key must not refuse the call: %v", err)
		}
		if string(cur) != validKey {
			t.Errorf("current key = %q, want %q", cur, validKey)
		}
		if len(prev) != 0 {
			t.Errorf("expected the short previous key to be discarded (empty), got %q", prev)
		}
	})

	t.Run("valid previous key is kept", func(t *testing.T) {
		validPrev := strings.Repeat("p", routeSigningKeyMinLen)
		p := &Plugin{logger: quiet, deploymentSecrets: &fakeInteractiveDeploymentSecrets{
			routeKey:         validKey,
			routeKeyPrevious: validPrev,
		}}
		_, prev, err := p.routeSigningKey(context.Background())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if string(prev) != validPrev {
			t.Errorf("previous key = %q, want %q", prev, validPrev)
		}
	})
}
