package engine

// cleat#1992. This file proves the property the owner asked for directly on
// the issue, about the plugin.Secrets adapter in plugin_secrets.go rather
// than about SecretStore underneath it: a plugin handling tenant B's request
// cannot reach tenant A's secret through ANY ARGUMENT, and the named
// per-tenant method (ForTenant) is the only way across.
//
// engine/a_secret_never_crosses_tenants_at_the_store_test.go already proves
// the STORE refuses a mismatched CONTEXT at the database -- RLS on Postgres,
// a SECURITY POLICY on SQL Server. That is a different property from this
// one. Shape A of the #1992 design (GetSecret(ctx, tenantID, name), mirroring
// SecretStore's own signature) would have passed that test too, and still had
// the bug the owner flagged: a plugin serving tenant B's request that calls
// the wrong tenantID as a plain string argument sets that context's tenant
// FROM the argument, so RLS allows it -- the parameter is not a thing RLS can
// check, it is the thing that tells RLS what to check. Shape B, which is
// what pluginSecrets implements, removes the argument instead of trying to
// validate it: Get/Put/Retire take no tenantID at all, so there is nothing
// for a plugin bug to pass wrong on the request path. This file's job is to
// show that is actually true of the adapter's public API, not just of the
// design as described on the issue.
//
// MYSQL IS NOT IN THIS TEST'S DIALECT SET, same reason as the store-level
// test: migration 038's single-tenant unique index means newRotationEnv
// itself only ever seeds ONE tenant on MySQL (see its own comment), so
// e.tenants[1] does not exist there. See the store-level test's header for
// the CI failure (cleat#2159) this was learned from.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/internal/tenantctx"
	"github.com/cleat-team/cleat/plugin"
	"github.com/google/uuid"
)

// TestPluginSecretsCannotReachAnotherTenantByAnyArgument is the known-positive
// half deliberately kept in the same test as the refusal: tenant A's own
// request-path read succeeding, and ForTenant(tenantA) independently reading
// the same value, are both what rule out "the whole adapter is broken and
// finds nothing for anyone" standing in for a real refusal.
func TestPluginSecretsCannotReachAnotherTenantByAnyArgument(t *testing.T) {
	for _, dialect := range []testutil.Dialect{testutil.DialectPostgres, testutil.DialectMSSQL} {
		t.Run(string(dialect), func(t *testing.T) {
			e := newRotationEnv(t, dialect)
			aTenant := e.tenants[0]
			bTenant := e.tenants[1]
			e.claim(t, aTenant, "plugin_secrets_probe")

			ring, err := NewKeyRing(rotV1)
			if err != nil {
				t.Fatalf("NewKeyRing: %v", err)
			}
			secrets := NewPluginSecrets(e.store(ring))

			// Seed tenant A's secret through the request-path Put, exactly as a
			// plugin route handling tenant A's own request would call it.
			if err := secrets.Put(e.ctx(aTenant), "plugin_secrets_probe", "tenant-a-only-value"); err != nil {
				t.Fatalf("Put under tenant A: %v", err)
			}

			// Known-positive #1: tenant A's own request-path Get must succeed,
			// and with the right value, or a "not found" below proves nothing.
			got, err := secrets.Get(e.ctx(aTenant), "plugin_secrets_probe")
			if err != nil {
				t.Fatalf("UNMEASURED: tenant A could not read its own just-written secret: %v", err)
			}
			if got != "tenant-a-only-value" {
				t.Fatalf("tenant A read %q, want %q", got, "tenant-a-only-value")
			}

			// THE CLAIM UNDER TEST. secrets.Get takes no tenantID parameter --
			// the only tenant this call can possibly name is whichever one
			// e.ctx(bTenant) put in ctx. There is no argument left for a plugin
			// bug to pass as aTenant by mistake; this is the whole point of
			// Shape B over Shape A, see the file comment.
			if _, err := secrets.Get(e.ctx(bTenant), "plugin_secrets_probe"); err == nil {
				t.Fatal("tenant B's request-path Get read tenant A's secret")
			} else if !errors.Is(err, ErrSecretNotFound) {
				// Postgres/MSSQL: the database-level policy is what refuses this
				// underneath the adapter, and it typically surfaces as a
				// distinct error rather than a plain not-found -- either is an
				// acceptable refusal, a nil error is not.
				t.Logf("tenant B's read was refused with: %v", err)
			}

			// Known-positive #2, and the other half of "the named method is the
			// only way across": ForTenant(aTenant) -- named, greppable, and
			// nothing like the request-path signature -- reads the SAME row
			// that the request-path call above could not, from a caller (bTenant's
			// own ctx, unchanged) that has no other way to reach it.
			got, err = secrets.ForTenant(aTenant.String()).Get(e.ctx(bTenant), "plugin_secrets_probe")
			if err != nil {
				t.Fatalf("ForTenant(tenantA) could not read tenant A's own secret: %v", err)
			}
			if got != "tenant-a-only-value" {
				t.Fatalf("ForTenant(tenantA) read %q, want %q", got, "tenant-a-only-value")
			}
		})
	}
}

// TestPluginSecretsRetireCannotDisableAnotherTenantsRow is Retire's version
// of the claim above, on the write side rather than the read side: tenant B's
// request-path Retire must not be able to disable tenant A's row, for the
// same reason Get cannot read it -- there is no tenantID argument to name A
// with. RowsAffected is the assertion, not just the error, because a Retire
// that silently affects zero rows and returns nil is indistinguishable from
// success by error alone.
func TestPluginSecretsRetireCannotDisableAnotherTenantsRow(t *testing.T) {
	for _, dialect := range []testutil.Dialect{testutil.DialectPostgres, testutil.DialectMSSQL} {
		t.Run(string(dialect), func(t *testing.T) {
			e := newRotationEnv(t, dialect)
			aTenant := e.tenants[0]
			bTenant := e.tenants[1]
			e.claim(t, aTenant, "plugin_secrets_retire_probe")

			ring, err := NewKeyRing(rotV1)
			if err != nil {
				t.Fatalf("NewKeyRing: %v", err)
			}
			secrets := NewPluginSecrets(e.store(ring))
			if err := secrets.Put(e.ctx(aTenant), "plugin_secrets_retire_probe", "tenant-a-only-value"); err != nil {
				t.Fatalf("Put under tenant A: %v", err)
			}

			// THE CLAIM UNDER TEST: tenant B's request-path Retire, same name,
			// must affect zero rows -- it has no argument through which it
			// could possibly name tenant A's row.
			n, err := secrets.Retire(e.ctx(bTenant), "plugin_secrets_retire_probe")
			if err != nil {
				t.Fatalf("tenant B's Retire returned an error rather than affecting zero rows: %v", err)
			}
			if n != 0 {
				t.Fatalf("tenant B's request-path Retire disabled %d row(s) of tenant A's secret, want 0", n)
			}

			// Known-positive: tenant A's row must still be readable after
			// tenant B's no-op Retire, or the zero above could mean the row
			// was already gone rather than that it was protected.
			if got, err := secrets.Get(e.ctx(aTenant), "plugin_secrets_retire_probe"); err != nil || got != "tenant-a-only-value" {
				t.Fatalf("UNMEASURED: tenant A's secret did not survive tenant B's Retire attempt: got=%q err=%v", got, err)
			}

			// Tenant A's OWN Retire, through the named per-tenant method this
			// time (ForTenant, exercised here so it is not just Get's escape
			// hatch), must affect exactly one row and disable the read.
			n, err = secrets.ForTenant(aTenant.String()).Retire(e.ctx(bTenant), "plugin_secrets_retire_probe")
			if err != nil {
				t.Fatalf("ForTenant(tenantA).Retire: %v", err)
			}
			if n != 1 {
				t.Fatalf("ForTenant(tenantA).Retire affected %d row(s), want 1", n)
			}
			if _, err := secrets.Get(e.ctx(aTenant), "plugin_secrets_retire_probe"); err == nil {
				t.Fatal("tenant A's secret was still readable after ForTenant(tenantA).Retire")
			}
		})
	}
}

// TestPluginSecretsRequestPathRefusesAnUnmarkedContext is the other half of
// "no argument to get wrong": a ctx that never went through plugin.ForTenant
// at all -- the shape a background loop would produce if it mistakenly
// called the request-path method instead of reaching for ForTenant -- must be
// refused before any query runs, rather than silently scoping to nothing or,
// worse, reaching beginTenantTx's no-transaction fallback and running
// unscoped (see engine/plugindb_tenant.go). No database is needed to show
// this: the adapter's own tenant lookup fails before Get ever calls the
// store.
func TestPluginSecretsRequestPathRefusesAnUnmarkedContext(t *testing.T) {
	secrets := NewPluginSecrets(nil)
	if _, err := secrets.Get(context.Background(), "anything"); err == nil {
		t.Fatal("Get with no tenant in context returned no error")
	} else if !strings.Contains(err.Error(), "no tenant in context") {
		t.Fatalf("Get with no tenant in context: got %v, want a \"no tenant in context\" error", err)
	}
}

// TestPluginSecretsRefusesACrossTenantMarkedContext is the known-positive for
// checkNotCrossTenant (engine/plugin_secrets.go): a ctx already marked by
// plugin.AcrossAllTenants must be refused by EVERY method -- request-path
// and ForTenant alike -- rather than silently mis-scoped or bypassed. See
// errCrossTenantContext's own doc comment for the per-dialect failure modes
// this replaces, measured by cleat-review rather than assumed: PostgreSQL's
// SET LOCAL ROLE cleat_sweep drops every grant cleat_app has (42501 on every
// call), and SQL Server never re-sets its tenant_id session key on this
// path (a "not found" Get, a PK-violating or silently-wrong-tenant Put, a
// zero-rows-nil-error Retire).
//
// The marked ctx is built ON TOP OF an already tenant-A-marked one, not a
// bare context.Background() -- that is the actual shape of the bug: a
// plugin's request handler that also, on the same ctx, does an
// AcrossAllTenants sweep by mistake. A bare unmarked ctx is already covered
// by TestPluginSecretsRequestPathRefusesAnUnmarkedContext and would not
// exercise beginTenantTx's documented cross-tenant-before-tenant precedence
// at all.
func TestPluginSecretsRefusesACrossTenantMarkedContext(t *testing.T) {
	for _, dialect := range []testutil.Dialect{testutil.DialectPostgres, testutil.DialectMSSQL} {
		t.Run(string(dialect), func(t *testing.T) {
			e := newRotationEnv(t, dialect)
			aTenant := e.tenants[0]
			e.claim(t, aTenant, "plugin_secrets_crosstenant_probe")

			ring, err := NewKeyRing(rotV1)
			if err != nil {
				t.Fatalf("NewKeyRing: %v", err)
			}
			secrets := NewPluginSecrets(e.store(ring))

			// Known-positive #1: tenant A's own request-path Put/Get must work
			// on an ordinary ctx, or a refusal below proves nothing.
			if err := secrets.Put(e.ctx(aTenant), "plugin_secrets_crosstenant_probe", "tenant-a-only-value"); err != nil {
				t.Fatalf("Put under tenant A: %v", err)
			}
			if got, err := secrets.Get(e.ctx(aTenant), "plugin_secrets_crosstenant_probe"); err != nil || got != "tenant-a-only-value" {
				t.Fatalf("UNMEASURED: tenant A could not read its own secret before the cross-tenant probe: got=%q err=%v", got, err)
			}

			marked := plugin.AcrossAllTenants(e.ctx(aTenant), "test: deliberately marked")

			// THE CLAIM UNDER TEST, request-path.
			if _, err := secrets.Get(marked, "plugin_secrets_crosstenant_probe"); err == nil {
				t.Fatal("Get on a cross-tenant-marked ctx returned no error")
			}
			if err := secrets.Put(marked, "plugin_secrets_crosstenant_probe", "should-not-write"); err == nil {
				t.Fatal("Put on a cross-tenant-marked ctx returned no error")
			}
			if n, err := secrets.Retire(marked, "plugin_secrets_crosstenant_probe"); err == nil {
				t.Fatalf("Retire on a cross-tenant-marked ctx returned no error (rowsAffected=%d)", n)
			}

			// THE CLAIM UNDER TEST, ForTenant: marking wins over ForTenant's own
			// tenant per beginTenantTx's documented precedence, so ForTenant
			// must ALSO refuse a marked incoming ctx rather than silently
			// no-op past the marking.
			tenantScoped := secrets.ForTenant(aTenant.String())
			if _, err := tenantScoped.Get(marked, "plugin_secrets_crosstenant_probe"); err == nil {
				t.Fatal("ForTenant(...).Get on a cross-tenant-marked ctx returned no error")
			}
			if err := tenantScoped.Put(marked, "plugin_secrets_crosstenant_probe", "should-not-write"); err == nil {
				t.Fatal("ForTenant(...).Put on a cross-tenant-marked ctx returned no error")
			}
			if n, err := tenantScoped.Retire(marked, "plugin_secrets_crosstenant_probe"); err == nil {
				t.Fatalf("ForTenant(...).Retire on a cross-tenant-marked ctx returned no error (rowsAffected=%d)", n)
			}

			// Known-positive #2: tenant A's secret must have survived every
			// refused call above untouched, or "refused" could mean "silently
			// wrote/deleted the wrong thing, then errored anyway".
			if got, err := secrets.Get(e.ctx(aTenant), "plugin_secrets_crosstenant_probe"); err != nil || got != "tenant-a-only-value" {
				t.Fatalf("tenant A's secret did not survive the refused cross-tenant calls untouched: got=%q err=%v", got, err)
			}
		})
	}
}

// TestPluginPayloadsRefusesACrossTenantMarkedContext is Payloads' version of
// the claim above. Payloads has no ForTenant (see plugin.Payloads' doc
// comment), so only the request-path Seal/Open pair is exercised.
func TestPluginPayloadsRefusesACrossTenantMarkedContext(t *testing.T) {
	for _, dialect := range []testutil.Dialect{testutil.DialectPostgres, testutil.DialectMSSQL} {
		t.Run(string(dialect), func(t *testing.T) {
			e := newRotationEnv(t, dialect)
			aTenant := e.tenants[0]

			ring, err := NewKeyRing(rotV1)
			if err != nil {
				t.Fatalf("NewKeyRing: %v", err)
			}
			enc, err := NewPayloadEncryptionWithRing(ring)
			if err != nil {
				t.Fatalf("NewPayloadEncryptionWithRing: %v", err)
			}
			payloads := NewPluginPayloads(enc)

			// Known-positive: an ordinary ctx round-trips, or a refusal below
			// proves nothing.
			sealed, err := payloads.Seal(e.ctx(aTenant), []byte("tenant-a-only-payload"))
			if err != nil {
				t.Fatalf("Seal under tenant A: %v", err)
			}
			if got, err := payloads.Open(e.ctx(aTenant), sealed); err != nil || string(got) != "tenant-a-only-payload" {
				t.Fatalf("UNMEASURED: tenant A could not open its own payload before the cross-tenant probe: got=%q err=%v", got, err)
			}

			marked := plugin.AcrossAllTenants(e.ctx(aTenant), "test: deliberately marked")

			// THE CLAIM UNDER TEST.
			if _, err := payloads.Seal(marked, []byte("should-not-seal")); err == nil {
				t.Fatal("Seal on a cross-tenant-marked ctx returned no error")
			}
			if _, err := payloads.Open(marked, sealed); err == nil {
				t.Fatal("Open on a cross-tenant-marked ctx returned no error")
			}
		})
	}
}

// TestPluginPayloadsCannotOpenAnotherTenantsCiphertext is Payloads' version of
// TestPluginSecretsCannotReachAnotherTenantByAnyArgument, on the ordinary
// (not cross-tenant-marked) path: tenant A seals a payload on its own ctx,
// tenant B's Open of that same ciphertext -- on ITS OWN ordinary ctx, not a
// marked one -- must fail. This is the basic per-tenant-key property
// engine.PayloadEncryption's own tests already cover at that layer; this file
// exists to show the SAME thing is true through the plugin.Payloads adapter's
// public API, the way the sibling Secrets tests do for Secrets.
//
// No DB involved -- Seal/Open are pure HKDF+AES-GCM with no SQL -- so this
// does not loop over dialects the way the store-backed Secrets tests do.
func TestPluginPayloadsCannotOpenAnotherTenantsCiphertext(t *testing.T) {
	ring, err := NewKeyRing(rotV1)
	if err != nil {
		t.Fatalf("NewKeyRing: %v", err)
	}
	enc, err := NewPayloadEncryptionWithRing(ring)
	if err != nil {
		t.Fatalf("NewPayloadEncryptionWithRing: %v", err)
	}
	payloads := NewPluginPayloads(enc)

	aTenant, bTenant := uuid.New(), uuid.New()
	aCtx := tenantctx.With(context.Background(), aTenant)
	bCtx := tenantctx.With(context.Background(), bTenant)

	sealed, err := payloads.Seal(aCtx, []byte("tenant-a-only-payload"))
	if err != nil {
		t.Fatalf("Seal under tenant A: %v", err)
	}

	// Known-positive: tenant A's own Open must succeed, or tenant B's refusal
	// below proves nothing.
	if got, err := payloads.Open(aCtx, sealed); err != nil || string(got) != "tenant-a-only-payload" {
		t.Fatalf("UNMEASURED: tenant A could not open its own payload: got=%q err=%v", got, err)
	}

	// THE CLAIM UNDER TEST: tenant B's Open of tenant A's ciphertext, on an
	// ordinary ctx of its own, must fail -- the per-tenant HKDF derivation
	// (engine.PayloadEncryption.forTenant) means B's derived key cannot open
	// what A's derived key sealed.
	if _, err := payloads.Open(bCtx, sealed); err == nil {
		t.Fatal("tenant B opened tenant A's payload")
	}
}

// TestPluginPayloadsFailsClosedWithNoEncryptor is Payloads' version of
// TestPluginSecretsRequestPathRefusesAnUnmarkedContext's nil-safety check,
// for the OTHER nil this adapter accepts: NewPluginPayloads(nil), the shape
// cmd/cleat-worker/main.go produces when --encrypt-sensitive-payloads is off
// (see NewPluginPayloads's own doc comment). Seal/Open must return a clear
// error rather than panic -- PayloadEncryption.currentKey handles a nil
// receiver by design (engine/encryption.go), and this proves that reaches
// all the way through the adapter rather than being shadowed by a nil
// dereference somewhere between here and there.
func TestPluginPayloadsFailsClosedWithNoEncryptor(t *testing.T) {
	payloads := NewPluginPayloads(nil)
	ctx := tenantctx.With(context.Background(), uuid.New())

	if _, err := payloads.Seal(ctx, []byte("anything")); err == nil {
		t.Fatal("Seal with no encryptor configured returned no error")
	}
	if _, err := payloads.Open(ctx, []byte("anything")); err == nil {
		t.Fatal("Open with no encryptor configured returned no error")
	}
}

// TestPluginPayloadsCannotOpenEnginesOwnEventHistoryCiphertext is the
// known-positive for pluginPayloadKeyInfo's domain separation
// (engine/encryption.go). Before it, pluginPayloads called
// PayloadEncryption.Encrypt/Decrypt directly, which derive under
// payloadKeyInfo -- the SAME info string, master key and AAD (tenantID) that
// encodeEventForStorage uses for a workflow's own event_history fields -- so
// a plugin's Open could decrypt engine's own event_history ciphertext
// outright. Measured by cleat-review on #2163; this reproduces it directly
// rather than trusting the report, and proves the fix by round-tripping the
// SAME ciphertext through both domains.
func TestPluginPayloadsCannotOpenEnginesOwnEventHistoryCiphertext(t *testing.T) {
	ring, err := NewKeyRing(rotV1)
	if err != nil {
		t.Fatalf("NewKeyRing: %v", err)
	}
	enc, err := NewPayloadEncryptionWithRing(ring)
	if err != nil {
		t.Fatalf("NewPayloadEncryptionWithRing: %v", err)
	}
	payloads := NewPluginPayloads(enc)
	tenant := uuid.New()
	ctx := tenantctx.With(context.Background(), tenant)

	// Sealed the way encodeEventForStorage seals an event_history field:
	// engine's own Encrypt, payloadKeyInfo's domain, not the plugin's.
	engineSealed, err := enc.Encrypt(tenant.String(), []byte("engine-event-history-field"))
	if err != nil {
		t.Fatalf("engine Encrypt: %v", err)
	}

	// Known-positive: engine's own Decrypt must still open its own
	// ciphertext, or this test proves nothing about domain separation
	// specifically (as opposed to the crypto being broken generally).
	if got, err := enc.Decrypt(tenant.String(), engineSealed); err != nil || string(got) != "engine-event-history-field" {
		t.Fatalf("UNMEASURED: engine's own Decrypt could not open its own ciphertext: got=%q err=%v", got, err)
	}

	// THE CLAIM UNDER TEST: the plugin adapter's Open, same tenant, must NOT
	// open a ciphertext sealed through engine's own domain.
	if _, err := payloads.Open(ctx, engineSealed); err == nil {
		t.Fatal("plugin Payloads.Open decrypted engine's own event_history-domain ciphertext")
	}

	// And the reverse, so the separation is proven both directions rather
	// than assumed symmetric: a plugin-sealed value must not open through
	// engine's own Decrypt either.
	pluginSealed, err := payloads.Seal(ctx, []byte("plugin-payload"))
	if err != nil {
		t.Fatalf("plugin Seal: %v", err)
	}
	if got, err := enc.Decrypt(tenant.String(), pluginSealed); err == nil {
		t.Fatalf("engine's own Decrypt opened a plugin-domain ciphertext: got=%q", got)
	}
}

// TestPluginPayloadsOpenRejectsTheLegacyNilAADForm is the known-positive for
// OpenForPlugin's narrower acceptance (engine/encryption.go): unlike
// engine's own Decrypt, it must NOT fall back to PayloadFormLegacy (master
// key, nil AAD) or PayloadFormBound (master key, tenant AAD) -- those two
// exist solely to keep PRE-cleat#1776 engine rows readable, and no plugin
// has ever written through this path, so there is no plugin-written legacy
// shape to carry forward. Sealed directly with sealGCM/the ring's raw key,
// bypassing both Encrypt and SealForPlugin, to simulate exactly the
// pre-cleat#1776 shape rather than anything either current seal path
// produces.
func TestPluginPayloadsOpenRejectsTheLegacyNilAADForm(t *testing.T) {
	ring, err := NewKeyRing(rotV1)
	if err != nil {
		t.Fatalf("NewKeyRing: %v", err)
	}
	enc, err := NewPayloadEncryptionWithRing(ring)
	if err != nil {
		t.Fatalf("NewPayloadEncryptionWithRing: %v", err)
	}
	payloads := NewPluginPayloads(enc)
	tenant := uuid.New()
	ctx := tenantctx.With(context.Background(), tenant)

	legacy, err := sealGCM(rotV1.Key, []byte("pre-cleat-1776-legacy-value"), nil)
	if err != nil {
		t.Fatalf("sealGCM (simulating a legacy row): %v", err)
	}

	// Known-positive: engine's own Decrypt DOES accept this shape -- that is
	// PayloadFormLegacy's whole purpose, and confirms the fixture is actually
	// legacy-shaped rather than simply malformed.
	if got, err := enc.Decrypt(tenant.String(), legacy); err != nil || string(got) != "pre-cleat-1776-legacy-value" {
		t.Fatalf("UNMEASURED: engine's own Decrypt could not open the legacy-shaped fixture, "+
			"so this is not a valid test of the plugin path's narrower refusal: got=%q err=%v", got, err)
	}

	// THE CLAIM UNDER TEST.
	if _, err := payloads.Open(ctx, legacy); err == nil {
		t.Fatal("plugin Payloads.Open accepted a legacy nil-AAD ciphertext")
	}

	bound, err := sealGCM(rotV1.Key, []byte("cleat-1792-bound-value"), []byte(tenant.String()))
	if err != nil {
		t.Fatalf("sealGCM (simulating a bound-but-not-derived row): %v", err)
	}
	if got, err := enc.Decrypt(tenant.String(), bound); err != nil || string(got) != "cleat-1792-bound-value" {
		t.Fatalf("UNMEASURED: engine's own Decrypt could not open the bound-shaped fixture: got=%q err=%v", got, err)
	}
	if _, err := payloads.Open(ctx, bound); err == nil {
		t.Fatal("plugin Payloads.Open accepted a master-key-AAD (bound, not derived) ciphertext")
	}
}

// TestPluginSecretsRequestPathPutCreatesOwnRowAndDoesNotTouchAnother is the
// write-side counterpart TestPluginSecretsCannotReachAnotherTenantByAnyArgument
// does not cover: that test only ever Puts under tenant A. This one has
// tenant B independently Put a secret under the SAME name tenant A already
// used, through B's own request-path Put, and checks both halves of the
// claim -- B's Put must create B's OWN row (readable back under B), and must
// not touch A's existing row under that name (still readable, still A's
// value). Both directions matter: a Put that silently no-ops would pass a
// test that only checked "B didn't corrupt A", and a Put that overwrote A's
// row would pass a test that only checked "B can read back what it wrote".
func TestPluginSecretsRequestPathPutCreatesOwnRowAndDoesNotTouchAnother(t *testing.T) {
	for _, dialect := range []testutil.Dialect{testutil.DialectPostgres, testutil.DialectMSSQL} {
		t.Run(string(dialect), func(t *testing.T) {
			e := newRotationEnv(t, dialect)
			aTenant := e.tenants[0]
			bTenant := e.tenants[1]
			e.claim(t, aTenant, "plugin_secrets_put_probe")
			e.claim(t, bTenant, "plugin_secrets_put_probe")

			ring, err := NewKeyRing(rotV1)
			if err != nil {
				t.Fatalf("NewKeyRing: %v", err)
			}
			secrets := NewPluginSecrets(e.store(ring))

			if err := secrets.Put(e.ctx(aTenant), "plugin_secrets_put_probe", "tenant-a-value"); err != nil {
				t.Fatalf("Put under tenant A: %v", err)
			}

			// THE CLAIM UNDER TEST: tenant B's own request-path Put, same name,
			// must create B's OWN row rather than touching A's.
			if err := secrets.Put(e.ctx(bTenant), "plugin_secrets_put_probe", "tenant-b-value"); err != nil {
				t.Fatalf("Put under tenant B: %v", err)
			}

			if got, err := secrets.Get(e.ctx(bTenant), "plugin_secrets_put_probe"); err != nil || got != "tenant-b-value" {
				t.Fatalf("tenant B could not read back its own just-written secret: got=%q err=%v", got, err)
			}
			if got, err := secrets.Get(e.ctx(aTenant), "plugin_secrets_put_probe"); err != nil || got != "tenant-a-value" {
				t.Fatalf("tenant A's secret was touched by tenant B's Put: got=%q err=%v", got, err)
			}
		})
	}
}
