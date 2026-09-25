package slacknotify

import (
	"encoding/json"
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

	cases := []struct {
		name                                       string
		tenantID, route, issuedAtHex, blockID, val string
	}{
		{"wrong tenant", otherTenantID, unsignedRoute, issuedAtHex, "block-1", "yes"},
		{"wrong route", tenantID, "wf:wf-2:sig:approve", issuedAtHex, "block-1", "yes"},
		{"wrong issued_at", tenantID, unsignedRoute, "00000000", "block-1", "yes"},
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
