// OIDC discovery and ID-token validation for the generic `oidc` provider.
//
// cleat#1582, implementing docs/enterprise-identity-decision.md: cleat speaks
// OIDC and the customer terminates SAML. A SAML proxy presents itself as an
// OIDC provider, so "support any IdP" and "support SAML" are one mechanism.
//
// WHO IS AUTHORITATIVE FOR IDENTITY. The userinfo endpoint is, here as for the
// three named providers -- that path already works and is what the callback
// has always used. The ID token is validated IN ADDITION, whenever the IdP
// returns one, and a token that fails validation is a hard failure rather than
// a fallback to userinfo. The reasoning is the decision document's own: a
// permissive validator is an authentication bypass, which is the failure class
// the decision avoids by not writing SAML. A validator that can be skipped on
// error is exactly that permissive validator.
//
// EVERY FETCH HERE GOES THROUGH p.httpClient, whose Transport is the egress
// guard (cleat#1565, a hard prerequisite of this feature rather than an
// adjacent nicety). The issuer URL is tenant-supplied and cleat fetches it,
// which is textbook SSRF: without the guard a tenant could point it at
// link-local and have the worker fetch cloud credentials.
package oauthprovider

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// discoveryTTL is how long a discovery document and its JWKS are reused.
//
// Short enough that an IdP rotating a signing key recovers without a restart,
// long enough that a login is not two extra round trips. Key rotation is also
// handled directly: an unknown `kid` forces a refetch before the token is
// rejected, so a rotation mid-TTL costs one fetch rather than an outage.
const discoveryTTL = 5 * time.Minute

// maxDiscoveryBody caps what is read from an issuer. A discovery document is a
// small JSON object; a tenant-supplied URL is not trusted to be one.
const maxDiscoveryBody = 1 << 20

// discoveryDoc is the subset of the OpenID Provider Metadata this plugin uses.
type discoveryDoc struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	UserinfoEndpoint      string `json:"userinfo_endpoint"`
	JWKSURI               string `json:"jwks_uri"`
}

type cachedDiscovery struct {
	doc       *discoveryDoc
	keys      *jwt.VerificationKeySet
	fetchedAt time.Time
}

// oidcCache memoises discovery documents and JWKS by issuer URL.
type oidcCache struct {
	mu      sync.Mutex
	entries map[string]*cachedDiscovery
	now     func() time.Time // overridable in tests
}

func newOIDCCache() *oidcCache {
	return &oidcCache{entries: map[string]*cachedDiscovery{}, now: time.Now}
}

// normaliseIssuer trims the trailing slash so the configured issuer and the
// value inside the discovery document compare equal when they differ only
// there. OIDC Discovery requires them to match exactly; a trailing slash is
// the one difference that is a formatting artefact rather than a mismatch.
func normaliseIssuer(s string) string {
	return strings.TrimRight(strings.TrimSpace(s), "/")
}

// discoveryURL builds the well-known URL for an issuer.
//
// Per OIDC Discovery the path is APPENDED to the issuer, including any path
// component the issuer already has -- https://idp.example/tenant1 discovers at
// https://idp.example/tenant1/.well-known/openid-configuration, not at the
// host root. Resolving it as a relative reference would silently drop the
// path and discover the wrong provider.
func discoveryURL(issuer string) (string, error) {
	u, err := url.Parse(normaliseIssuer(issuer))
	if err != nil {
		return "", fmt.Errorf("issuer is not a URL: %w", err)
	}
	if u.Scheme != "https" {
		// Not a style rule. The issuer is tenant-supplied and everything this
		// plugin trusts about the IdP arrives over this connection, so plain
		// http would make the whole flow forgeable by anyone on the path.
		return "", fmt.Errorf("issuer must be https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("issuer has no host: %q", issuer)
	}
	return strings.TrimRight(u.String(), "/") + "/.well-known/openid-configuration", nil
}

// discover fetches and caches an issuer's metadata and signing keys.
func (p *Plugin) discover(ctx context.Context, issuer string) (*discoveryDoc, *jwt.VerificationKeySet, error) {
	key := normaliseIssuer(issuer)

	p.cache().mu.Lock()
	if e, ok := p.cache().entries[key]; ok && p.cache().now().Sub(e.fetchedAt) < discoveryTTL {
		p.cache().mu.Unlock()
		return e.doc, e.keys, nil
	}
	p.cache().mu.Unlock()

	doc, keys, err := p.fetchDiscovery(ctx, key)
	if err != nil {
		return nil, nil, err
	}

	p.cache().mu.Lock()
	p.cache().entries[key] = &cachedDiscovery{doc: doc, keys: keys, fetchedAt: p.cache().now()}
	p.cache().mu.Unlock()
	return doc, keys, nil
}

func (p *Plugin) fetchDiscovery(ctx context.Context, issuer string) (*discoveryDoc, *jwt.VerificationKeySet, error) {
	wellKnown, err := discoveryURL(issuer)
	if err != nil {
		return nil, nil, err
	}

	var doc discoveryDoc
	if err := p.getJSON(ctx, wellKnown, &doc); err != nil {
		return nil, nil, fmt.Errorf("discovery: %w", err)
	}

	// The document must claim the issuer we asked about. Without this an IdP
	// -- or anything that can answer for that host -- could hand back metadata
	// pointing token and userinfo at a third party, and the flow would follow
	// it. OIDC Discovery requires the comparison; it is the one check that
	// makes the rest of the document safe to act on.
	if normaliseIssuer(doc.Issuer) != normaliseIssuer(issuer) {
		return nil, nil, fmt.Errorf("discovery document declares issuer %q, want %q", doc.Issuer, issuer)
	}
	if doc.AuthorizationEndpoint == "" || doc.TokenEndpoint == "" {
		return nil, nil, fmt.Errorf("discovery document is missing authorization_endpoint or token_endpoint")
	}

	keys := &jwt.VerificationKeySet{}
	if doc.JWKSURI != "" {
		keys, err = p.fetchJWKS(ctx, doc.JWKSURI)
		if err != nil {
			return nil, nil, err
		}
	}
	return &doc, keys, nil
}

// getJSON performs a guarded GET and decodes a bounded JSON body.
func (p *Plugin) getJSON(ctx context.Context, target string, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return fmt.Errorf("build request for %s: %w", target, err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("fetch %s: %w", target, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch %s: status %d", target, resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxDiscoveryBody)).Decode(into); err != nil {
		return fmt.Errorf("decode %s: %w", target, err)
	}
	return nil
}

// fetchJWKS retrieves an issuer's signing keys.
func (p *Plugin) fetchJWKS(ctx context.Context, jwksURI string) (*jwt.VerificationKeySet, error) {
	var raw struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := p.getJSON(ctx, jwksURI, &raw); err != nil {
		return nil, fmt.Errorf("jwks: %w", err)
	}

	set := &jwt.VerificationKeySet{}
	for _, k := range raw.Keys {
		pub, kid, err := parseJWK(k)
		if err != nil {
			// One unusable key is not a reason to reject the others: a set
			// routinely carries key types this plugin does not verify with.
			p.logger.Debug("oauth: skipping unusable JWK", "error", err)
			continue
		}
		set.Keys = append(set.Keys, jwt.VerificationKey(pub))
		_ = kid
	}
	if len(set.Keys) == 0 {
		return nil, fmt.Errorf("jwks: no usable keys at %s", jwksURI)
	}
	return set, nil
}

// parseJWK converts one JSON Web Key into a public key.
//
// RSA and the three NIST EC curves, which is what an OIDC provider signs with
// in practice. Symmetric keys are deliberately absent: an `oct` key in a
// published JWKS would be a shared secret, and accepting HMAC here is how a
// validator becomes an authentication bypass (see validMethods below).
func parseJWK(raw json.RawMessage) (crypto.PublicKey, string, error) {
	var k struct {
		Kty string `json:"kty"`
		Kid string `json:"kid"`
		Use string `json:"use"`
		N   string `json:"n"`
		E   string `json:"e"`
		Crv string `json:"crv"`
		X   string `json:"x"`
		Y   string `json:"y"`
	}
	if err := json.Unmarshal(raw, &k); err != nil {
		return nil, "", fmt.Errorf("malformed JWK: %w", err)
	}
	// "enc" keys are for encryption, not signatures. Using one to verify would
	// be a category error rather than a weakness, but it is still wrong.
	if k.Use != "" && k.Use != "sig" {
		return nil, k.Kid, fmt.Errorf("JWK use=%q is not a signing key", k.Use)
	}

	switch k.Kty {
	case "RSA":
		n, err := base64.RawURLEncoding.DecodeString(k.N)
		if err != nil {
			return nil, k.Kid, fmt.Errorf("RSA modulus: %w", err)
		}
		e, err := base64.RawURLEncoding.DecodeString(k.E)
		if err != nil {
			return nil, k.Kid, fmt.Errorf("RSA exponent: %w", err)
		}
		if len(e) == 0 || len(e) > 8 {
			return nil, k.Kid, fmt.Errorf("RSA exponent is %d bytes", len(e))
		}
		var exp int
		for _, b := range e {
			exp = exp<<8 | int(b)
		}
		pub := &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: exp}
		// A short modulus is not merely weak, it is forgeable with commodity
		// hardware. Rejecting it here is cheaper than explaining it later.
		if pub.N.BitLen() < 2048 {
			return nil, k.Kid, fmt.Errorf("RSA key is %d bits, want >= 2048", pub.N.BitLen())
		}
		return pub, k.Kid, nil

	case "EC":
		var curve elliptic.Curve
		switch k.Crv {
		case "P-256":
			curve = elliptic.P256()
		case "P-384":
			curve = elliptic.P384()
		case "P-521":
			curve = elliptic.P521()
		default:
			return nil, k.Kid, fmt.Errorf("unsupported EC curve %q", k.Crv)
		}
		x, err := base64.RawURLEncoding.DecodeString(k.X)
		if err != nil {
			return nil, k.Kid, fmt.Errorf("EC x: %w", err)
		}
		y, err := base64.RawURLEncoding.DecodeString(k.Y)
		if err != nil {
			return nil, k.Kid, fmt.Errorf("EC y: %w", err)
		}
		// ecdsa.ParseUncompressedPublicKey, not a struct literal plus IsOnCurve: it is the API the standard
		// library names for turning a point into a key (setting X and Y directly is deprecated), it also refuses
		// the point at infinity, and it is what the rest of crypto/ecdsa is tested against (cleat#2300).
		//
		// A coordinate is read as a NUMBER, exactly as before: RFC 7518 wants full-size coordinates, some providers
		// strip the leading zero byte, and a few pad extra ones, so a value that fits the curve is accepted however
		// it is padded. What does not fit is refused before it can be truncated into a different point.
		size := (curve.Params().BitSize + 7) / 8
		xi, yi := new(big.Int).SetBytes(x), new(big.Int).SetBytes(y)
		if xi.BitLen() > size*8 || yi.BitLen() > size*8 {
			return nil, k.Kid, fmt.Errorf("EC point is not on %s: a coordinate is larger than the curve's %d bytes", k.Crv, size)
		}
		point := make([]byte, 1+2*size)
		point[0] = 4 // SEC 1 uncompressed
		xi.FillBytes(point[1 : 1+size])
		yi.FillBytes(point[1+size:])
		pub, err := ecdsa.ParseUncompressedPublicKey(curve, point)
		if err != nil {
			return nil, k.Kid, fmt.Errorf("EC point is not on %s: %w", k.Crv, err)
		}
		return pub, k.Kid, nil

	default:
		return nil, k.Kid, fmt.Errorf("unsupported key type %q", k.Kty)
	}
}

// validMethods is an ALLOWLIST of signature algorithms, and it is the single
// most important line in this file.
//
// Without it a token header saying {"alg":"none"} parses as a valid token with
// no signature at all, and {"alg":"HS256"} invites the classic confusion where
// an RSA public key -- which is public -- is used as an HMAC secret, letting
// anyone who can read the JWKS mint tokens. Both are authentication bypasses,
// and both are what "a permissive validator" means in
// docs/enterprise-identity-decision.md.
var validMethods = []string{"RS256", "RS384", "RS512", "ES256", "ES384", "ES512", "PS256", "PS384", "PS512"}

// idTokenClaims is the part of an ID token this plugin reads itself. Issuer,
// audience and expiry are checked by the parser via the options below, not
// here, so that the comparison is the library's rather than a hand-rolled one.
//
// EmailVerified is a verificationFlag rather than a *bool so that the string
// form some issuers emit ("true") reads as verified instead of failing the
// parse, and so that anything unreadable reads as UNVERIFIED -- the direction
// that cannot admit anyone. Subject comes from jwt.RegisteredClaims, which
// already carries `sub`; it is not redeclared here.
type idTokenClaims struct {
	Nonce         string           `json:"nonce"`
	Email         string           `json:"email"`
	EmailVerified verificationFlag `json:"email_verified"`
	jwt.RegisteredClaims
}

// validateIDToken verifies an ID token against the issuer's discovered keys.
//
// Returns an error for every failure. There is deliberately no path that
// downgrades a failed check into a warning: the caller treats any error here
// as a failed login, which is what keeps this from being the permissive
// validator the decision document warns about.
func (p *Plugin) validateIDToken(ctx context.Context, rawToken, issuer, clientID, wantNonce string) (*idTokenClaims, error) {
	doc, keys, err := p.discover(ctx, issuer)
	if err != nil {
		return nil, err
	}
	if doc.JWKSURI == "" || len(keys.Keys) == 0 {
		return nil, fmt.Errorf("issuer %s publishes no signing keys, so its ID token cannot be verified", issuer)
	}

	claims := &idTokenClaims{}
	parse := func(ks *jwt.VerificationKeySet) error {
		_, err := jwt.ParseWithClaims(rawToken, claims,
			func(*jwt.Token) (any, error) { return *ks, nil },
			jwt.WithValidMethods(validMethods),
			jwt.WithIssuer(normaliseIssuer(issuer)),
			jwt.WithAudience(clientID),
			jwt.WithExpirationRequired(),
		)
		return err
	}

	err = parse(keys)
	if err != nil {
		// A rotated signing key is the ordinary reason a known-good issuer
		// stops verifying, and it is indistinguishable here from a forged
		// token. Refetch ONCE and retry: a rotation costs one fetch, and a
		// forgery still fails on the second attempt.
		if fresh, ferr := p.refreshKeys(ctx, issuer); ferr == nil {
			claims = &idTokenClaims{}
			err = parse(fresh)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("ID token verification failed: %w", err)
	}

	// The nonce is the replay check, and it is ours to make: the parser has no
	// opinion about it. It ties this token to the login that started THIS
	// flow, so a token captured from another session cannot be replayed here.
	if wantNonce != "" && claims.Nonce != wantNonce {
		return nil, fmt.Errorf("ID token nonce does not match the one issued for this login")
	}
	return claims, nil
}

// refreshKeys forces a refetch of an issuer's JWKS, bypassing the TTL.
func (p *Plugin) refreshKeys(ctx context.Context, issuer string) (*jwt.VerificationKeySet, error) {
	doc, keys, err := p.fetchDiscovery(ctx, normaliseIssuer(issuer))
	if err != nil {
		return nil, err
	}
	p.cache().mu.Lock()
	p.cache().entries[normaliseIssuer(issuer)] = &cachedDiscovery{doc: doc, keys: keys, fetchedAt: p.cache().now()}
	p.cache().mu.Unlock()
	return keys, nil
}
