package oauthprovider

// cleat#2340: what a successful login MINTS, and the two ways it must not
// complete at all.
//
// A 200 from the callback says only that the login finished. Everything the
// rest of the design acts on is invisible in the response body: the identity
// tag that design item 4 revokes by when its allowlist row is deleted, and the
// expiry that the sweep selects by. So these assert on the mint the HOST WAS
// ASKED FOR -- the request, not the page -- and on the rules that a refusal and
// a failed mint each leave nothing behind.

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

const userinfoVerifiedEmail = `{"email":"user@example.com","email_verified":true}`

// mints reads the recorded mints under the store's lock.
func (c *allowlistCase) mints() []mintedKey {
	c.store.mu.RLock()
	defer c.store.mu.RUnlock()
	return append([]mintedKey(nil), c.store.mints...)
}

// TestOA_Callback_MintsAKeyTaggedWithTheAdmittingEmailRow pins the exact tag
// string, because the revoke path renders the same row through
// oauthIdentityTag and matches this column by string equality. A tag that
// differs here by so much as a case is a revoke that silently matches nothing,
// and the keys it meant to kill keep authenticating until they expire.
func TestOA_Callback_MintsAKeyTaggedWithTheAdmittingEmailRow(t *testing.T) {
	c := newAllowlistCase(t, "google", "mint-email", userinfoVerifiedEmail, nil)
	c.store.AddAllowedIdentity(testTenantID, "google", identityTypeEmail, "user@example.com")

	rec := c.callback(t, "google")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	mints := c.mints()
	if len(mints) != 1 {
		t.Fatalf("minted %d keys, want exactly 1", len(mints))
	}
	m := mints[0]

	const wantTag = "google:user@example.com"
	if m.OAuthIdentity != wantTag {
		t.Errorf("oauth_identity = %q, want %q", m.OAuthIdentity, wantTag)
	}
	if m.TenantID != testTenantID {
		t.Errorf("minted for tenant %s, want %s", m.TenantID, testTenantID)
	}
	if m.RawKey == "" {
		t.Error("the minter was asked for a key with no plaintext to hand back")
	}
	if !strings.Contains(rec.Body.String(), m.RawKey) {
		t.Errorf("the page does not carry the key that was minted (%s), so the caller has no "+
			"way to authenticate:\n%s", m.RawKey, rec.Body.String())
	}
}

// TestOA_Callback_MintsAKeyTaggedWithTheAdmittingSubjectRow is the same
// property for design item 1's other arm: an operator who lists a stable
// subject rather than an address. The tag spells the kind out, because an
// address and a subject are different namespaces that could otherwise collide
// as strings.
func TestOA_Callback_MintsAKeyTaggedWithTheAdmittingSubjectRow(t *testing.T) {
	c := newAllowlistCase(t, "google", "mint-subject",
		`{"email":"ada@example.com","sub":"00u1a2b3c"}`, nil)
	c.store.AddAllowedIdentity(testTenantID, "google", identityTypeSubject, "00u1a2b3c")

	rec := c.callback(t, "google")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	mints := c.mints()
	if len(mints) != 1 {
		t.Fatalf("minted %d keys, want exactly 1", len(mints))
	}
	const wantTag = "google:subject:00u1a2b3c"
	if mints[0].OAuthIdentity != wantTag {
		t.Errorf("oauth_identity = %q, want %q", mints[0].OAuthIdentity, wantTag)
	}
}

// TestOA_Allowlist_RefusalMintsNothing is the ordering rule, asserted rather
// than assumed. Minting before the allowlist and refusing after would hand a
// credential to someone the tenant's own list just turned away, and the refusal
// response would look identical.
func TestOA_Allowlist_RefusalMintsNothing(t *testing.T) {
	c := newAllowlistCase(t, "google", "mint-refused", userinfoVerifiedEmail, nil)
	// No allowlist row: cleat#2371 made that deny on every deployment.

	if rec := c.callback(t, "google"); rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
	if mints := c.mints(); len(mints) != 0 {
		t.Errorf("a refused login minted %d key(s): %+v -- the refusal must leave no credential "+
			"behind to expire, sweep or revoke", len(mints), mints)
	}
}

// TestOA_Callback_AMintFailureDoesNotCompleteTheLogin. A key that could not be
// recorded is not a credential, so completing the login around it would answer
// 200 with a page carrying something that authenticates nothing.
func TestOA_Callback_AMintFailureDoesNotCompleteTheLogin(t *testing.T) {
	c := newAllowlistCase(t, "google", "mint-fails", userinfoVerifiedEmail, nil)
	c.store.AddAllowedIdentity(testTenantID, "google", identityTypeEmail, "user@example.com")

	c.store.mu.Lock()
	c.store.mintErr = errors.New("the key store is unreachable")
	c.store.mu.Unlock()

	rec := c.callback(t, "google")
	if rec.Code == http.StatusOK {
		t.Fatalf("the login completed while the mint failed; body:\n%s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "cleat_sk_") {
		t.Errorf("a failed mint still produced a key on the page:\n%s", rec.Body.String())
	}
}

// TestOA_Callback_AnUnwiredMinterRefusesTheLogin is the NIL MEANS "THIS HOST
// CANNOT MINT" convention, which is what stops a deployment that forgot to wire
// the Environment field from answering 200 with an unusable credential.
//
// It is NOT evidence that production wires it: this test removes the field by
// hand, and setupTestPlugin puts it back for every other test in this package.
// The wiring itself is a boot test's job.
func TestOA_Callback_AnUnwiredMinterRefusesTheLogin(t *testing.T) {
	c := newAllowlistCase(t, "google", "mint-unwired", userinfoVerifiedEmail, nil)
	c.store.AddAllowedIdentity(testTenantID, "google", identityTypeEmail, "user@example.com")

	c.plugin.mintOAuthAPIKey = nil

	rec := c.callback(t, "google")
	if rec.Code == http.StatusOK {
		t.Fatalf("an unwired minter completed the login anyway; body:\n%s", rec.Body.String())
	}
}

// TestOAKeyExpiry pins the cap and the floor together, because they are the
// same constant doing two jobs and either one alone looks like a bug: a
// provider reporting a week must not mint a week-long key, and a provider
// reporting nothing at all must not mint a permanent one.
func TestOAKeyExpiry(t *testing.T) {
	for _, tc := range []struct {
		name      string
		expiresIn int
		want      time.Duration
	}{
		{"absent expires_in is bounded, not unlimited", 0, oauthKeyExpiryMax},
		{"a negative lifetime is treated as absent", -1, oauthKeyExpiryMax},
		{"a short lifetime is honoured", 300, 300 * time.Second},
		{"a week is capped to the maximum", 7 * 24 * 3600, oauthKeyExpiryMax},
		{"exactly the cap is left alone", int(oauthKeyExpiryMax / time.Second), oauthKeyExpiryMax},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := time.Now()
			got := oauthKeyExpiry(tc.expiresIn)
			// The value is always in the future and always bounded: there is no
			// zero time and no "no expiry" to return, which is the property the
			// store's CreateOAuthAPIKey signature also relies on.
			if !got.After(before) {
				t.Fatalf("oauthKeyExpiry(%d) = %s, which is not in the future", tc.expiresIn, got)
			}
			if d := got.Sub(before); d > tc.want+2*time.Second || d < tc.want-2*time.Second {
				t.Errorf("oauthKeyExpiry(%d) is %s from now, want about %s", tc.expiresIn, d, tc.want)
			}
		})
	}
}

// TestOA_Callback_TheKeyPageIsNotFramableOrCacheable. The page displays a live
// credential, so its headers are part of the credential's handling rather than
// boilerplate: a cache that keeps it, a Referer that carries it onward, or a
// frame it can be rendered inside are each a way for someone else to read it.
func TestOA_Callback_TheKeyPageIsNotFramableOrCacheable(t *testing.T) {
	c := newAllowlistCase(t, "google", "mint-headers", userinfoVerifiedEmail, nil)
	c.store.AddAllowedIdentity(testTenantID, "google", identityTypeEmail, "user@example.com")

	rec := c.callback(t, "google")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	for header, want := range map[string]string{
		"Cache-Control":          "no-store",
		"Referrer-Policy":        "no-referrer",
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
	} {
		if got := rec.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}

	csp := rec.Header().Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'none'", "frame-ancestors 'none'", "base-uri 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("Content-Security-Policy %q is missing %q", csp, want)
		}
	}
	// There is no script on this page, which is what makes the postMessage
	// question moot -- see writeOAuthKeyPage's doc comment.
	if strings.Contains(rec.Body.String(), "<script") {
		t.Errorf("the key page carries a script, which writeOAuthKeyPage's delivery decision "+
			"does not account for:\n%s", rec.Body.String())
	}
}

// TestOA_Callback_TheIdentityTagNormalisesHoweverItIsCalled. cleat#2410.
//
// The mint tags the identity the login PRESENTED, which identityAllowed has
// already run through normalizeEmail; the revoke path renders the STORED ROW,
// which is whatever an operator typed. Sharing oauthIdentityTag shares the
// LAYOUT, and without normalising inside it the two produce different strings
// for the same person -- so the revoke's `DELETE ... WHERE oauth_identity = $1`
// matches nothing and the keys it meant to kill keep authenticating until they
// expire on their own.
//
// THE FIXTURE IS DELIBERATELY PADDED AND MIXED-CASE, which no other test in
// this package does, and that is the point: a row written plainly is admitted
// and rendered identically whether or not the renderer normalises, so every
// other test here is blind to this by construction.
func TestOA_Callback_TheIdentityTagNormalisesHoweverItIsCalled(t *testing.T) {
	// user@example.com is the address the mock IdP presents, spelled here the
	// way an operator plausibly types it into an INSERT.
	const storedRow = "  User@Example.COM  "
	const wantTag = "google:user@example.com"

	// The rendering the revoke path will do, on the row exactly as stored.
	if got := oauthIdentityTag("google", allowedIdentity{
		Type: identityTypeEmail, Value: storedRow,
	}); got != wantTag {
		t.Errorf("rendering the stored row %q gives %q, want %q -- a revoke matching on that "+
			"string finds no keys and they keep authenticating until they expire",
			storedRow, got, wantTag)
	}

	// And the same through the real login, so the property is asserted on the
	// path that actually mints rather than only on the helper.
	c := newAllowlistCase(t, "google", "mint-padded", userinfoVerifiedEmail, nil)
	c.store.AddAllowedIdentity(testTenantID, "google", identityTypeEmail, storedRow)

	rec := c.callback(t, "google")
	if rec.Code != http.StatusOK {
		t.Fatalf("a padded, mixed-case row must still be admitted -- identityAllowed trims and "+
			"folds both sides -- got %d: %s", rec.Code, rec.Body.String())
	}
	mints := c.mints()
	if len(mints) != 1 {
		t.Fatalf("minted %d keys, want exactly 1", len(mints))
	}
	if mints[0].OAuthIdentity != wantTag {
		t.Errorf("the mint tagged %q, want %q -- the tag must not depend on how the row was "+
			"typed", mints[0].OAuthIdentity, wantTag)
	}
}

// The subject arm of the same property. normalizeSubject TRIMS but does NOT
// fold -- OIDC Core section 2 defines `sub` as case-sensitive, so folding would
// admit one account under another's row -- and the type is trimmed to match
// identityAllowed's own comparison of it.
func TestOAIdentityTagNormalisesTheSubjectArm(t *testing.T) {
	for _, tc := range []struct {
		name  string
		match allowedIdentity
		want  string
	}{
		{
			"a padded subject value is trimmed, not folded",
			allowedIdentity{Type: identityTypeSubject, Value: "  00u1a2b3c  "},
			"google:subject:00u1a2b3c",
		},
		{
			"a padded identity TYPE still renders as the subject kind",
			allowedIdentity{Type: " subject ", Value: "00u1a2b3c"},
			"google:subject:00u1a2b3c",
		},
		{
			"an address keeps its case folded and its padding trimmed",
			allowedIdentity{Type: identityTypeEmail, Value: "  BOB@Example.COM "},
			"google:bob@example.com",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := oauthIdentityTag("google", tc.match); got != tc.want {
				t.Errorf("oauthIdentityTag(%q, %+v) = %q, want %q",
					"google", tc.match, got, tc.want)
			}
		})
	}
}
