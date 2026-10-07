package auth

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// fakeOperatorResolver is the smallest OperatorResolver that can be wrong: it
// returns whatever it was built with and records what it was asked, so a test
// can distinguish "the credential came back" from "the credential this test
// planted came back".
type fakeOperatorResolver struct {
	op      Operator
	err     error
	gotHash []byte
	calls   int
}

func (f *fakeOperatorResolver) ResolveOperatorFromAPIKey(_ context.Context, keyHash []byte) (Operator, error) {
	f.calls++
	f.gotHash = keyHash
	return f.op, f.err
}

func TestGenerateOperatorKeyCarriesTheOperatorPrefix(t *testing.T) {
	const draws = 20
	for i := 0; i < draws; i++ {
		k := GenerateOperatorKey()
		if !IsOperatorKey(k) {
			t.Fatalf("GenerateOperatorKey() returned %q, which IsOperatorKey rejects", k)
		}
		if !strings.HasPrefix(k, OperatorKeyPrefix) {
			t.Fatalf("GenerateOperatorKey() returned %q, which does not start with %q", k, OperatorKeyPrefix)
		}
		// 9 characters of prefix + 32 bytes hex-encoded.
		if want := len(OperatorKeyPrefix) + 64; len(k) != want {
			t.Fatalf("GenerateOperatorKey() returned a %d-character key, want %d: %q", len(k), want, k)
		}
		for _, c := range k[len(OperatorKeyPrefix):] {
			if !strings.ContainsRune("0123456789abcdef", c) {
				t.Fatalf("GenerateOperatorKey() returned %q whose body is not lowercase hex (%q)", k, c)
			}
		}
	}
}

// TestGenerateOperatorKeyDrawsFreshBytesEveryTime is the other half of "it has
// the right shape": a generator that returned a constant would satisfy
// TestGenerateOperatorKeyCarriesTheOperatorPrefix in full, and every key it
// issued would be the same key.
func TestGenerateOperatorKeyDrawsFreshBytesEveryTime(t *testing.T) {
	seen := make(map[string]bool)
	const draws = 50
	for i := 0; i < draws; i++ {
		k := GenerateOperatorKey()
		if seen[k] {
			t.Fatalf("GenerateOperatorKey() returned %q twice in %d draws", k, draws)
		}
		seen[k] = true
	}
}

// TestGenerateOperatorKeyTakesItsBytesFromRandRead ties the generator to the
// package's entropy seam, the same one GenerateAPIKey uses and for the same
// reason: a key produced by anything other than randRead is not 256 bits of
// crypto/rand, whatever it looks like.
func TestGenerateOperatorKeyTakesItsBytesFromRandRead(t *testing.T) {
	originalRandRead := randRead
	t.Cleanup(func() { randRead = originalRandRead })

	const marker = 0xab
	calls := 0
	randRead = func(b []byte) (int, error) {
		calls++
		for i := range b {
			b[i] = marker
		}
		return len(b), nil
	}

	got := GenerateOperatorKey()
	if calls != 1 {
		t.Errorf("GenerateOperatorKey() read from randRead %d time(s), want exactly 1", calls)
	}
	// Not just "it called randRead": the key must BE those bytes, or the
	// generator could be drawing from randRead and discarding the result.
	if want := OperatorKeyPrefix + strings.Repeat("ab", 32); got != want {
		t.Errorf("GenerateOperatorKey() = %q, want %q (the stubbed bytes, hex-encoded)", got, want)
	}
}

// TestAnOperatorKeyIsNeverATenantKey is the property OperatorKeyPrefix exists
// for. Both generators draw 32 bytes from the same seam, so the prefixes are
// the ONLY thing separating the two credential types, and a regression that
// made them share one would leave both resolvers willing to answer for the
// other's keys.
func TestAnOperatorKeyIsNeverATenantKey(t *testing.T) {
	const draws = 20
	for i := 0; i < draws; i++ {
		tenantKey := GenerateAPIKey()
		if IsOperatorKey(tenantKey) {
			t.Fatalf("IsOperatorKey accepted a tenant key: %q", tenantKey)
		}
		operatorKey := GenerateOperatorKey()
		if strings.HasPrefix(operatorKey, "cleat_sk_") {
			t.Fatalf("GenerateOperatorKey() produced a tenant-shaped key: %q", operatorKey)
		}
	}
}

// TestAnOperatorKeyIsNotASessionToken pins the half of the fall-through this
// package can see.
//
// plugins/oauthprovider's looksLikeSessionToken accepts exactly a 64-character
// lowercase-hex string and rejects everything else, and it runs BEFORE
// auth.Middleware on the bearer path. That gate has two conditions and this
// test checks both against a real operator key, so "an operator key falls
// through to auth.Middleware" follows by construction. The gate itself is in
// that plugin and is not re-implemented here -- testing a copy of a predicate
// would only prove the copy agrees with itself.
func TestAnOperatorKeyIsNotASessionToken(t *testing.T) {
	k := GenerateOperatorKey()
	if len(k) == 64 {
		t.Errorf("an operator key is %d characters; looksLikeSessionToken accepts only len==64", len(k))
	}
	if !strings.ContainsAny(k, "ghijklmnopqrstuvwxyz_") {
		t.Errorf("an operator key is %q, which is within looksLikeSessionToken's [0-9a-f] class", k)
	}
}

func TestOperatorFromAPIKeyReturnsTheCredentialTheResolverNamed(t *testing.T) {
	want := Operator{KeyID: "6f5c1e9a-2b1f-4a3c-9d0e-77c8b5a4e012", Description: "on-call laptop"}
	store := &fakeOperatorResolver{op: want}

	got, err := OperatorFromAPIKey(context.Background(), store, []byte("hash"))
	if err != nil {
		t.Fatalf("OperatorFromAPIKey returned an error for a well-formed credential: %v", err)
	}
	if got != want {
		t.Errorf("OperatorFromAPIKey returned %+v, want the resolver's %+v", got, want)
	}
	// The hash must arrive as given: a wrapper that re-derived it from
	// something else would still pass the comparison above.
	if string(store.gotHash) != "hash" {
		t.Errorf("the resolver was asked for %q, want the hash it was given", store.gotHash)
	}
}

// TestOperatorFromAPIKeyRefusesACredentialWithNoKeyID covers the guard in
// OperatorFromAPIKey, and carries its own positive control: the same resolver
// with a KeyID present must NOT be refused. Without that control the test would
// also pass against an implementation that refused every credential.
func TestOperatorFromAPIKeyRefusesACredentialWithNoKeyID(t *testing.T) {
	refused := &fakeOperatorResolver{op: Operator{Description: "a description but no key id"}}
	if _, err := OperatorFromAPIKey(context.Background(), refused, []byte("hash")); err == nil {
		t.Error("OperatorFromAPIKey accepted a credential with no key id; its audit attribution would read \"operator:\" and name nothing")
	}

	admitted := &fakeOperatorResolver{op: Operator{KeyID: "11111111-2222-3333-4444-555555555555"}}
	if _, err := OperatorFromAPIKey(context.Background(), admitted, []byte("hash")); err != nil {
		t.Errorf("the same path refused a credential WITH a key id, so the guard above is not what the first half measured: %v", err)
	}
}

func TestOperatorFromAPIKeyPropagatesTheResolversError(t *testing.T) {
	sentinel := errors.New("no rows in result set")
	store := &fakeOperatorResolver{op: Operator{KeyID: "ignored"}, err: sentinel}

	got, err := OperatorFromAPIKey(context.Background(), store, []byte("hash"))
	if !errors.Is(err, sentinel) {
		t.Fatalf("OperatorFromAPIKey returned %v, want the resolver's own error", err)
	}
	if got != (Operator{}) {
		t.Errorf("OperatorFromAPIKey returned %+v alongside an error; a failed lookup must not carry a credential", got)
	}
}

func TestOperatorRoundTripsThroughTheContext(t *testing.T) {
	want := Operator{KeyID: "33333333-4444-5555-6666-777777777777", Description: "break-glass"}
	ctx := WithOperator(context.Background(), want)

	got, ok := OperatorFromContext(ctx)
	if !ok {
		t.Fatal("OperatorFromContext reported no operator on a context WithOperator set one on")
	}
	if got != want {
		t.Errorf("OperatorFromContext returned %+v, want %+v", got, want)
	}
}

// TestOperatorFromContextIsFalseOnATenantAuthenticatedRequest is the negative
// control for the test above: it is what makes ok load-bearing. Every request
// that is not an operator's takes the tenant path, and a handler that read the
// zero Operator instead of ok would treat all of them as operators.
func TestOperatorFromContextIsFalseOnATenantAuthenticatedRequest(t *testing.T) {
	tenantCtx := WithTenantID(context.Background(), uuid.MustParse("99999999-8888-7777-6666-555555555555"))

	if op, ok := OperatorFromContext(tenantCtx); ok {
		t.Errorf("OperatorFromContext reported an operator (%+v) on a tenant-authenticated context", op)
	}
	if _, ok := OperatorFromContext(context.Background()); ok {
		t.Error("OperatorFromContext reported an operator on a context nothing authenticated")
	}
}

// TestAnOperatorAttributionIsNotMistakableForATenantID is the property
// Operator.String's prefix is for.
//
// The field it lands in -- AdminActionEvent.Operator, an entry in the
// workflow's event history -- held a tenant UUID before operator keys existed,
// and api_admin.go still writes one when no operator authenticated the request.
// A key id is a UUID too, so an unprefixed attribution would make the two
// answers the same string, and reading an audit record could not tell a
// force-complete taken from inside a tenant from one taken by the deployment's
// operator. This asserts they differ even when the uuid is the same, which is
// the case that would otherwise be silently indistinguishable.
func TestAnOperatorAttributionIsNotMistakableForATenantID(t *testing.T) {
	id := uuid.MustParse("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")

	asTenant := id.String() // what operatorFromContext falls back to
	asOperator := Operator{KeyID: id.String()}.String()

	if asOperator == asTenant {
		t.Fatalf("an operator attribution is %q, identical to the tenant attribution for the same uuid", asOperator)
	}
	if !strings.HasPrefix(asOperator, "operator:") {
		t.Errorf("an operator attribution is %q; without the prefix it is a bare UUID and reads as a tenant", asOperator)
	}
	if strings.HasPrefix(asTenant, "operator:") {
		t.Errorf("a tenant attribution is %q; the two must not both be able to look like an operator", asTenant)
	}
}
