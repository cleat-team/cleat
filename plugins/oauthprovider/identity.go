package oauthprovider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/cleat-team/cleat/plugin"
	"github.com/google/uuid"
)

// verificationFlag is the type of the OIDC `email_verified` claim -- and of the
// same claim in a userinfo response, which OIDC defines by reference to the
// ID token's.
//
// It exists because the claim is OPTIONAL and, when present, is emitted as a
// JSON boolean by most issuers and as the STRING "true"/"false" by some. A
// plain *bool would turn the string form into a json.Unmarshal error, which
// decode would surface as a failed userinfo parse and therefore a 502 -- a
// hard login failure caused by a cosmetic issuer quirk, on a deployment that
// may not even have an allowlist enabled.
//
// So this type never errors. Anything it does not positively recognise as
// true -- absent, null, the string "true", a number, a truncated body -- reads
// as false. That direction is deliberate and it is the whole reason this is a
// named type rather than a bool: an unreadable verification claim must make an
// address INELIGIBLE to match an allowlist row, never eligible. The cost of
// being wrong that way is one operator adding a subject row (see
// resolvedIdentity); the cost of being wrong the other way is admitting an
// address whoever controls its domain says is theirs and no one has proved.
type verificationFlag bool

func (v *verificationFlag) UnmarshalJSON(b []byte) error {
	// Two trims, and the order matters: the outer one removes whitespace
	// around the JSON value, the inner one removes whitespace INSIDE the quotes
	// after they come off. `" true "` -- a string form with padding, which is
	// what a template-driven issuer emits -- is the case the inner trim exists
	// for, and it is unreachable if the quotes are not stripped first.
	//
	// Only the word "true" is true. A bare 1, a null, and a number are all read
	// as false, because OIDC says this claim is a boolean and a value that is
	// not one is a value this code cannot vouch for.
	s := strings.TrimSpace(string(b))
	s = strings.TrimSpace(strings.Trim(s, `"`))
	*v = verificationFlag(strings.EqualFold(s, "true"))
	return nil
}

// The two kinds of key an oauth_allowed_identities row can carry. They are the
// values the migration documents and the values the check below switches on;
// an unrecognised identity_type matches nothing.
const (
	identityTypeEmail   = "email"
	identityTypeSubject = "subject"
)

// resolvedIdentity is what a completed OAuth callback knows about the person on
// the other end, once every source has been consulted and every source-specific
// rule applied.
//
// The three fields are not interchangeable and the distinction is the point of
// this type:
//
//   - Email is stored on the session row (oauth_sessions.user_email) and shown
//     by GET /oauth/sessions. It is the human-readable label, nothing more.
//   - EmailVerified says whether a SOURCE THAT CHECKS asserted this address
//     belongs to this person. Only a verified address may match an `email` row
//     of the allowlist. An address whose verification is absent or unreadable
//     is carried (so the session row and the operator's log both name it) but
//     is not eligible to match.
//   - Subject is the provider's stable identifier for the account -- an OIDC
//     `sub`, GitHub's numeric id. It is not reassignable, unlike an address,
//     and it is what admits a person on an issuer that does not publish
//     email_verified at all (see the 403 path in finishLogin).
//
// Subject is an ADDITIONAL allowlist key, never a substitute for the email: a
// callback that resolves no address at all is still refused before any of this
// matters. Relaxing that would admit logins that fail today on every
// deployment, with no operator opt-in, which is the wrong direction for a
// change whose whole purpose is to narrow who gets in.
type resolvedIdentity struct {
	Email         string
	EmailVerified bool
	Subject       string
}

// normalizeEmail folds an address for COMPARISON.
//
// Lowercasing is a decision with a cost and the cost is why it is here rather
// than in the SQL. RFC 5321 says the local part of an address is the mail
// server's to interpret and may be case-sensitive; in practice no mail provider
// cleat will meet treats it that way, and an operator who writes the ON CONFLICT
// INSERT by hand -- which is the only way rows get in today, see the migration --
// will write "Ada@Example.com" while the provider asserts "ada@example.com".
// Comparing raw would refuse a person who is on the list, which is a support
// ticket whose cause is invisible in both the row and the log.
//
// The cost is that two addresses differing only in case are treated as one. That
// is a real narrowing of the operator's control, and it is stated here rather
// than discovered: an allowlist cannot express "admit Ada@ but not ada@" under
// this rule. No provider here can express it either, which is why the fold is
// safe in practice.
func normalizeEmail(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// normalizeSubject trims and does NOT lowercase.
//
// OIDC Core §2 defines `sub` as a case-sensitive string, so folding it would
// merge two distinct accounts and admit one under the other's row -- the
// fail-open direction, on the one key that is supposed to be unforgeable and
// un-reassignable. Trim only, and the trim is there because a value pasted into
// the INSERT from a JSON response can carry a newline.
func normalizeSubject(s string) string {
	return strings.TrimSpace(s)
}

// gitHubEmail is one entry of GitHub's GET /user/emails response.
type gitHubEmail struct {
	Email    string `json:"email"`
	Primary  bool   `json:"primary"`
	Verified bool   `json:"verified"`
}

// selectGitHubVerifiedEmail picks the address GitHub will vouch for, or refuses.
//
// GitHub returns every address on the account, each tagged with primary and
// verified, and a listed address nobody has proved is exactly the case
// cleat#2340 item 1 is about -- a person can add an address they do not control
// and it appears here immediately, unverified. So the selection requires BOTH:
// primary, because that is the account's own choice of address, and verified,
// because a primary that has not been proved is still unproved.
//
// Refusing (empty string, nil error) when nothing qualifies is a real answer,
// not a failure: the caller then falls back to the `email` field of /user, which
// carries no verification claim and is therefore not eligible to match an
// allowlist row. A login on such an account still succeeds on a deployment with
// no allowlist, exactly as it does today.
//
// This is a pure function over the response BODY so the decision is testable
// without redirecting api.github.com -- see TestSelectGitHubVerifiedEmail.
func selectGitHubVerifiedEmail(body []byte) (string, error) {
	var list []gitHubEmail
	if err := json.Unmarshal(body, &list); err != nil {
		return "", fmt.Errorf("parse /user/emails: %w", err)
	}
	for _, e := range list {
		if e.Primary && e.Verified && strings.TrimSpace(e.Email) != "" {
			return strings.TrimSpace(e.Email), nil
		}
	}
	return "", nil
}

// githubVerifiedEmail fetches /user/emails and returns the address GitHub
// vouches for, or "" when it vouches for none.
//
// An error is returned only for a transport or parse failure, and the caller
// treats it as "no verified address" rather than as a failed login: the whole
// point of consulting this endpoint is that it upgrades an address from
// unverified to verified, and a lookup that cannot run must not be worse for
// the user than not having tried.
func (p *Plugin) githubVerifiedEmail(ctx context.Context, accessToken, emailsURL string) (string, error) {
	if emailsURL == "" {
		return "", nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, emailsURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("github /user/emails returned %s", resp.Status)
	}
	// Bounded read. This is a third-party response body arriving on a login
	// path, and an address list is a few hundred bytes; a megabyte is two
	// orders of magnitude of headroom and still a bound.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	return selectGitHubVerifiedEmail(body)
}

// allowedIdentity is the allowlist ROW that admitted a login: the identity type
// and value an operator wrote, normalised the same way the comparison
// normalises them.
//
// It is returned rather than a bare bool because a minted key has to record
// which row let its owner in (see oauthIdentityTag). Reporting only "allowed"
// would leave nothing to revoke BY: design item 4 removes an identity's live
// keys when its allowlist row goes, and a key that does not name the row it
// came from cannot be found by that deletion.
type allowedIdentity struct {
	Type  string
	Value string
}

// identityKind canonicalises an allowlist row's identity_type.
//
// ONE PLACE decides which of the two kinds a row is, because the matcher and
// the renderer must agree about it. identityAllowed dispatches on
// strings.TrimSpace(identityType) in a Go switch, so the comparison is
// case-SENSITIVE whatever the column's collation is; anything that is not
// "subject" is the address kind, which is safe only because identityAllowed
// admits no row whose trimmed type is neither. A row spelled "Subject" or
// "EMAIL" never matches there, so it is never rendered here.
func identityKind(identityType string) string {
	if strings.TrimSpace(identityType) == identityTypeSubject {
		return identityTypeSubject
	}
	return identityTypeEmail
}

// OAuthIdentityValue returns the identity_value an allowlist row stores, and
// that OAuthIdentityTag renders into a minted key's tag. cleat#2340.
//
// EXPORTED WITH THE TAG, deliberately, because cmd/cleatctl writes rows and
// revokes keys and must not normalise differently from the matcher. The
// matcher compares in Go -- normaliseEmail on both sides -- so a row stored
// un-normalised still MATCHES; but a `remove` that deleted by the raw value an
// operator typed would miss a row stored under a different spelling while the
// revoke it computed from that same value succeeded, leaving the row present
// and the keys dead. Storing this form and deleting by this form keeps the two
// halves of one operation looking at the same string.
func OAuthIdentityValue(identityType, identityValue string) string {
	switch identityKind(identityType) {
	case identityTypeSubject:
		return normalizeSubject(identityValue)
	default:
		return normalizeEmail(identityValue)
	}
}

// OAuthIdentityTag renders an allowlist row into the value stored on
// admin.tenant_api_keys.oauth_identity.
//
// ONE FUNCTION, USED BY EVERY CALLER THAT WRITES OR MATCHES THAT COLUMN: the
// mint, and the revoke that runs when an operator removes an allowlist row.
// The column is compared by exact string equality, so if two sides rendered a
// tag even slightly differently -- a case difference, a different separator --
// the revoke would silently match nothing and the keys it meant to kill would
// keep authenticating until they expired on their own.
//
// EXPORTED BECAUSE THE REVOKE PATH IS NOT IN THIS PACKAGE. cmd/cleatctl removes
// the allowlist row and then calls auth.TenantStore.RevokeOAuthAPIKeys, and it
// has to render the tag HERE rather than assembling "provider:identity" itself.
// A caller that hand-builds the string re-opens cleat#2410 with the fix sitting
// unused beside it -- which is why this is exported rather than reimplemented.
//
// IT NORMALISES ITS INPUT, AND THAT IS NOT REDUNDANT WITH ITS CALLERS. Sharing
// a function shares the LAYOUT; it does not by itself share the VALUE, and the
// two sides read theirs from different places: the mint tags the identity the
// login PRESENTED, which identityAllowed has already run through the
// normaliser, while a revoke renders the STORED ROW -- exactly what an operator
// typed, which the comparison tolerates but does not rewrite. A row written
// "  Alice@Example.COM  " is admitted, and would mint "google:alice@example.com"
// while rendering as "google:  Alice@Example.COM  ". Normalising here means a
// caller CANNOT get it wrong, which is a stronger guarantee than a comment
// asking every caller to remember.
//
// The shape is design v2's, verbatim: "<provider>:<identity>", with the subject
// kind spelled out, because an address and a subject are different namespaces
// that could otherwise collide as strings -- "github:alice@example.com" for an
// address, "oidc:subject:110169..." for a subject.
func OAuthIdentityTag(provider, identityType, identityValue string) string {
	kind := identityKind(identityType)
	v := OAuthIdentityValue(kind, identityValue)
	if kind == identityTypeSubject {
		return provider + ":subject:" + v
	}
	return provider + ":" + v
}

// identityAllowed reports WHICH row of oauth_allowed_identities admits id, and
// whether any does.
//
// The rows are read and COMPARED IN GO rather than matched in SQL, and that is
// the one design decision in this function worth stating. The stored value is
// whatever an operator typed into an INSERT -- there is no writer in cleat yet,
// so every row is hand-written, which means a mixed-case address is not a
// hypothetical. Comparing in SQL would need LOWER() on both sides, spelled
// per-dialect, and would still not cover the subject/key split. Reading the
// handful of rows a (tenant, provider) allowlist can hold and applying
// normalizeEmail/normalizeSubject to both sides costs nothing measurable and
// keeps the comparison rule in one place, in Go, where it is unit-tested.
//
// No match is NOT "admit" -- it is "no row matches", and the caller decides
// what that means. finishLogin asks UNCONDITIONALLY: cleat#2371 removed the
// oauth_config.allowlist_enabled opt-in that used to gate the call, so "no
// rows" denies rather than admits, on every deployment.
//
// Read that together with the paragraph above. The table still has no writer in
// cleat -- every row is hand-written -- so a tenant that has just configured
// OAuth cannot sign anyone in until an operator inserts a row for them. That is
// the intended fail-closed behaviour (cleat#2340 design v1 section 2, "no escape
// hatch"), not an oversight; the allowlist CLI that gives that sentence a
// happier ending is part of the same work.
func (p *Plugin) identityAllowed(ctx context.Context, tid uuid.UUID, provider string, id resolvedIdentity) (allowedIdentity, bool, error) {
	// The tenant is the one the state row named, exactly as in finishLogin's
	// UPDATE: this runs on an unauthenticated callback, so the value in hand is
	// the only reliable one.
	ctx = plugin.ForTenant(ctx, tid)

	rows, err := p.db.Query(ctx, plugin.Rebind(`
			SELECT identity_type, identity_value
			FROM oauth_allowed_identities
			WHERE tenant_id = $1 AND provider = $2
		`, p.dialect), tid, provider)
	if err != nil {
		return allowedIdentity{}, false, err
	}
	defer rows.Close()

	email := normalizeEmail(id.Email)
	subject := normalizeSubject(id.Subject)

	for rows.Next() {
		var identityType, identity string
		if err := plugin.ScanRow(rows, &identityType, &identity); err != nil {
			return allowedIdentity{}, false, err
		}
		switch strings.TrimSpace(identityType) {
		case identityTypeEmail:
			if !id.EmailVerified || email == "" {
				continue
			}
			if normalizeEmail(identity) == email {
				return allowedIdentity{Type: identityTypeEmail, Value: email}, true, nil
			}
		case identityTypeSubject:
			if subject == "" {
				continue
			}
			if normalizeSubject(identity) == subject {
				return allowedIdentity{Type: identityTypeSubject, Value: subject}, true, nil
			}
		}
	}
	return allowedIdentity{}, false, rows.Err()
}
