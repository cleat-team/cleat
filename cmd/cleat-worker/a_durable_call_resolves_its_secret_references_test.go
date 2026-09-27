package main

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/internal/tenantctx"
	"github.com/google/uuid"
)

// A store with a nil DB: every lookup fails. That failure is the observable
// signal that a lookup was ATTEMPTED, which is what these tests need -- an
// implementation that did not resolve would pass the reference through as
// literal text and return no error at all.
//
// The same device is used by TestTheWrapperSubstitutesInsideTheCalleeNotBeforeIt
// for the plugin path, and for the same reason: it distinguishes "resolved" from
// "never tried" without a database.
func nilDBSecretStore(t *testing.T) *engine.SecretStore {
	t.Helper()
	master, err := engine.MasterKeyFromEnv(
		base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")))
	if err != nil {
		t.Fatalf("master key: %v", err)
	}
	store, err := engine.NewSecretStore(nil, "postgres", master)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	return store
}

func tenantCtx() context.Context {
	return tenantctx.With(context.Background(),
		uuid.MustParse("11111111-1111-1111-1111-111111111111"))
}

// TestADurableCallResolvesItsSecretReference covers the gap this change closes.
//
// Before it, ResolveSecretRefs had exactly one caller -- withSecrets, wrapping
// PLUGIN functions -- so `${secret:name}` in an http.fetch request was never
// resolved. It was set verbatim as the outbound header value, which meant the
// only way to authenticate a DurableCall was to put the real credential in the
// request, where it is recorded.
func TestADurableCallResolvesItsSecretReference(t *testing.T) {
	c := &dbServiceCaller{secrets: nilDBSecretStore(t)}

	const req = `{"url":"https://api.example.com","headers":{"Authorization":"Bearer ${secret:api_key}"}}`
	_, err := c.call(tenantCtx(), "http", "fetch", req, "")

	if err == nil {
		t.Fatal("a request carrying ${secret:...} returned no error against a store " +
			"that cannot look anything up; the reference was not resolved, which is " +
			"the defect this change exists to fix")
	}
	if !strings.Contains(err.Error(), "api_key") {
		t.Errorf("error does not name the unresolvable secret, so an operator cannot "+
			"tell which reference failed: %v", err)
	}
}

// TestBothCallEntryPointsResolve pins that the resolution sits in the shared
// funnel rather than on one path.
//
// Call and CallWithIdempotencyKey are separate entry points that both delegate
// to call. Resolving in only one would leave exactly-once callers -- the ones
// whose operations were deemed important enough to flag -- sending unresolved
// references.
func TestBothCallEntryPointsResolve(t *testing.T) {
	const req = `{"headers":{"Authorization":"${secret:tok}"}}`

	for _, tc := range []struct {
		name string
		call func(*dbServiceCaller) error
	}{
		{"Call", func(c *dbServiceCaller) error {
			_, err := c.Call(tenantCtx(), "http", "fetch", req)
			return err
		}},
		{"CallWithIdempotencyKey", func(c *dbServiceCaller) error {
			_, err := c.CallWithIdempotencyKey(tenantCtx(), "http", "fetch", req, "key-1")
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &dbServiceCaller{secrets: nilDBSecretStore(t)}
			if err := tc.call(c); err == nil {
				t.Error("entry point did not resolve its secret references")
			}
		})
	}
}

// TestAMissingSecretIsPermanent asserts the error class, not just the error.
//
// Retrying cannot create a secret the tenant does not have. Classifying this
// retryable spends the whole retry budget on a typo and reports the failure as
// a timeout, which names nothing.
func TestAMissingSecretIsPermanent(t *testing.T) {
	c := &dbServiceCaller{secrets: nilDBSecretStore(t)}

	_, err := c.call(tenantCtx(), "http", "fetch", `{"k":"${secret:nope}"}`, "")
	if err == nil {
		t.Fatal("want an error")
	}
	var ce *engine.CleatError
	if !errors.As(err, &ce) {
		t.Fatalf("want a *engine.CleatError so the retry path can classify it, got %T: %v", err, err)
	}
	if ce.Code != engine.ErrPermanent {
		t.Errorf("a missing secret is classified %v rather than permanent, so a typo will "+
			"burn the retry budget and surface as a timeout: %v", ce.Code, err)
	}
}

// TestATenantlessCallPassesTheReferenceThrough mirrors withSecrets' own choice,
// and the reasoning is worth restating because "fail closed" looks right here
// and is not.
//
// One worker serves many tenants. A reference with no tenant in context cannot
// be resolved against "some default tenant" without risking sending ANOTHER
// tenant's credential. Passing the literal text through fails at the far end,
// loudly, against the right blast radius.
func TestATenantlessCallPassesTheReferenceThrough(t *testing.T) {
	c := &dbServiceCaller{secrets: nilDBSecretStore(t)}

	got, err := c.resolveSecrets(context.Background(), "http", "fetch", `{"k":"${secret:x}"}`)
	if err != nil {
		t.Fatalf("a tenantless call must not fail on secrets: %v", err)
	}
	if !strings.Contains(got, "${secret:x}") {
		t.Errorf("the reference was altered without a tenant to resolve it against: %q", got)
	}
}

// TestAWorkerWithoutSecretsLeavesRequestsAlone: a deployment started with no
// CLEAT_SECRET_MASTER_KEY has a nil store, and must keep working exactly as it
// did before this change.
func TestAWorkerWithoutSecretsLeavesRequestsAlone(t *testing.T) {
	c := &dbServiceCaller{}

	const req = `{"k":"${secret:x}"}`
	got, err := c.resolveSecrets(tenantCtx(), "http", "fetch", req)
	if err != nil {
		t.Fatalf("nil secret store must be a pass-through: %v", err)
	}
	if got != req {
		t.Errorf("request altered by a nil store: %q", got)
	}
}

// TestARequestWithoutReferencesIsUntouched guards the cost of the feature on
// the overwhelmingly common path: ResolveSecretRefs short-circuits on a
// substring check before any regexp or lookup, and this pins that a plain
// request is returned byte-identical.
func TestARequestWithoutReferencesIsUntouched(t *testing.T) {
	c := &dbServiceCaller{secrets: nilDBSecretStore(t)}

	const req = `{"url":"https://api.example.com","body":"{\"a\":1}"}`
	got, err := c.resolveSecrets(tenantCtx(), "http", "fetch", req)
	if err != nil {
		t.Fatalf("a request with no references must not fail: %v", err)
	}
	if got != req {
		t.Errorf("request altered: %q", got)
	}
}
