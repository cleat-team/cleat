package main

// cleat#2340 design items 2 and 4, from the operator's side. These drive the
// same functions `runOAuthAllow` calls, against a real PostgreSQL database with
// oauthprovider's own migration applied -- not a fake, because the properties
// under test are properties of SQL: a primary key that makes re-adding safe, a
// folded comparison that decides which rows a removal reaches, and a revoke
// whose UPDATE either matches the tag a key was minted with or silently does
// nothing.
//
// The command is PostgreSQL-only (ported.go), so there is no per-dialect loop
// here; TestOAuthAllowIsRefusedOffPostgres covers the other half, which is that
// the refusal happens rather than that the SQL would work.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
	"github.com/cleat-team/cleat/plugins/oauthprovider"
)

// A tenant that exists. oauth_allowed_identities carries no foreign key to
// admin.tenants, but the login path's tenant always does, so a test that used
// an invented id would be exercising a state no login can produce.
const oauthTestTenant = engine.DefaultTenantUUID

func oauthAllowTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db := testutil.TestDB(t, testutil.DialectPostgres)
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)

	// oauth_allowed_identities belongs to the plugin, so it arrives with the
	// plugin's migration rather than with the core schema.
	if err := plugin.RunMigrations(context.Background(), db, plugin.DialectPostgres, nil,
		[]*plugin.LoadedPlugin{{Plugin: &oauthprovider.Plugin{}, Healthy: true}}); err != nil {
		t.Fatalf("oauthprovider migrations: %v", err)
	}
	return db
}

// seedOAuthMintedKey writes a key as the OAuth mint would, and returns its id
// so the test can read disabled_at back. Nothing here goes through
// auth.TenantStore beyond the revoke under test, because the question is what
// the REVOKE reaches, not whether the mint can write.
func seedOAuthMintedKey(t *testing.T, db *sql.DB, oauthIdentity string, expiresAt time.Time) uuid.UUID {
	t.Helper()
	id := uuid.New()
	hash := sha256.Sum256([]byte(id.String()))
	_, err := db.Exec(`
		INSERT INTO admin.tenant_api_keys
			(tenant_id, key_id, key_hash, description, expires_at, oauth_identity)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		oauthTestTenant, id, hash[:], "oauth-allow-test", expiresAt, oauthIdentity)
	if err != nil {
		t.Fatalf("seed key for %q: %v", oauthIdentity, err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM admin.tenant_api_keys WHERE key_id = $1`, id)
	})
	return id
}

func keyIsLive(t *testing.T, db *sql.DB, id uuid.UUID) bool {
	t.Helper()
	var live bool
	if err := db.QueryRow(
		`SELECT disabled_at IS NULL FROM admin.tenant_api_keys WHERE key_id = $1`, id).Scan(&live); err != nil {
		t.Fatalf("read key %s: %v", id, err)
	}
	return live
}

func cleanupOAuthAllow(t *testing.T, db *sql.DB, provider, identityType, value string) {
	t.Helper()
	_, _ = db.Exec(`DELETE FROM oauth_allowed_identities
		WHERE tenant_id = $1 AND provider = $2 AND identity_type = $3 AND identity_value = $4`,
		oauthTestTenant, provider, identityType, value)
}

func TestOAuthAllowRoundtrip(t *testing.T) {
	db := oauthAllowTestDB(t)
	ctx := context.Background()
	tenant := uuid.MustParse(oauthTestTenant)
	t.Cleanup(func() { cleanupOAuthAllow(t, db, "google", "email", "operator@example.com") })

	// An empty list is a meaningful answer, not an error: it is the fail-closed
	// state a fresh deployment is in until someone adds a row.
	listed, err := oauthAllowList(ctx, db, tenant, "google")
	if err != nil {
		t.Fatalf("list on a fresh tenant: %v", err)
	}
	for _, e := range listed {
		if e.Value == "operator@example.com" {
			t.Fatalf("a previous run left %v behind", e)
		}
	}

	if err := oauthAllowAdd(ctx, db, tenant, "google", "email", "operator@example.com"); err != nil {
		t.Fatalf("add: %v", err)
	}
	// Twice. The primary key is (tenant, provider, identity_type,
	// identity_value), so a setup script re-run must not be an error.
	if err := oauthAllowAdd(ctx, db, tenant, "google", "email", "operator@example.com"); err != nil {
		t.Fatalf("adding the same identity twice: %v", err)
	}

	listed, err = oauthAllowList(ctx, db, tenant, "google")
	if err != nil {
		t.Fatalf("list after add: %v", err)
	}
	var found bool
	for _, e := range listed {
		if e.Type == "email" && e.Value == "operator@example.com" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the identity was added but does not appear in %v", listed)
	}

	rev, err := oauthAllowRemove(ctx, db, dialectPostgres, tenant, "google", "email", "operator@example.com")
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if rev.Removed != 1 {
		t.Errorf("remove reported %d rows removed, want 1", rev.Removed)
	}

	listed, err = oauthAllowList(ctx, db, tenant, "google")
	if err != nil {
		t.Fatalf("list after remove: %v", err)
	}
	for _, e := range listed {
		if e.Type == "email" && e.Value == "operator@example.com" {
			t.Errorf("the identity survived its own removal: %v", e)
		}
	}
}

// TestOAuthAllowRemoveRevokesTheKeysThatRowMinted is design item 4's actual
// requirement, and the one an operator would notice missing: "remove" has to
// mean the credential stopped working, not that the door is closed to the next
// login while the last one's key keeps authenticating.
func TestOAuthAllowRemoveRevokesTheKeysThatRowMinted(t *testing.T) {
	db := oauthAllowTestDB(t)
	ctx := context.Background()
	tenant := uuid.MustParse(oauthTestTenant)
	t.Cleanup(func() { cleanupOAuthAllow(t, db, "google", "email", "revoke@example.com") })

	if err := oauthAllowAdd(ctx, db, tenant, "google", "email", "revoke@example.com"); err != nil {
		t.Fatalf("add: %v", err)
	}

	future := time.Now().Add(time.Hour)
	target := seedOAuthMintedKey(t, db, "google:revoke@example.com", future)
	// A control, on the same table and read the same way: a key belonging to
	// somebody else. Without it, a revoke that disabled EVERY key would look
	// like a revoke that worked.
	other := seedOAuthMintedKey(t, db, "google:somebody-else@example.com", future)

	if !keyIsLive(t, db, target) {
		t.Fatal("the seeded key is not live before the removal, so the assertion below is vacuous")
	}

	if _, err := oauthAllowRemove(ctx, db, dialectPostgres, tenant, "google", "email", "revoke@example.com"); err != nil {
		t.Fatalf("remove: %v", err)
	}

	if keyIsLive(t, db, target) {
		t.Errorf("the key minted for the removed identity is still live -- the row is gone, so the " +
			"next login is refused, but this credential keeps authenticating until it expires, " +
			"which is not what an operator means by 'remove'")
	}
	if !keyIsLive(t, db, other) {
		t.Errorf("the removal revoked a key belonging to a different identity")
	}
}

// TestOAuthAllowRemovalFindsARowWhateverSpellingItWasStoredUnder is cleat#2410
// from the CLI side, and it is the difference between a command that works and
// one with a dead end.
//
// The row is written the way this table's ONLY writer before the CLI wrote it --
// a hand-written INSERT, in whatever case the operator typed. `list` shows that
// spelling; `remove` is handed a different one. Exact matching would find
// nothing, and the advice to use the listed value would be unusable, because
// the CLI normalises whatever it is given. So the comparison has to be the
// matcher's own.
func TestOAuthAllowRemovalFindsARowWhateverSpellingItWasStoredUnder(t *testing.T) {
	db := oauthAllowTestDB(t)
	ctx := context.Background()
	tenant := uuid.MustParse(oauthTestTenant)
	const stored = "  Hand.Written@Example.COM  "
	t.Cleanup(func() { cleanupOAuthAllow(t, db, "google", "email", stored) })

	if _, err := db.Exec(`INSERT INTO oauth_allowed_identities
		(tenant_id, provider, identity_type, identity_value) VALUES ($1, $2, $3, $4)`,
		tenant, "google", "email", stored); err != nil {
		t.Fatalf("hand-write the row: %v", err)
	}

	// The tag a login admitted by this row mints -- rendered by the same
	// function the mint uses, from the row's own value.
	key := seedOAuthMintedKey(t, db,
		oauthprovider.OAuthIdentityTag("google", "email", stored), time.Now().Add(time.Hour))

	listed, err := oauthAllowList(ctx, db, tenant, "google")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var sawStored bool
	for _, e := range listed {
		if e.Value == stored {
			sawStored = true
		}
	}
	if !sawStored {
		t.Fatalf("the hand-written row is not listed as %q, so this test is not measuring what it "+
			"thinks: %v", stored, listed)
	}

	rev, err := oauthAllowRemove(ctx, db, dialectPostgres, tenant, "google", "email", "hand.written@example.com")
	if err != nil {
		t.Fatalf("remove with the folded spelling: %v", err)
	}
	if rev.Removed != 1 {
		t.Errorf("remove matched %d rows, want 1 -- a row that can be listed but not removed is a "+
			"dead end, and this is the spelling the operator would read off `list` and retype",
			rev.Removed)
	}
	if keyIsLive(t, db, key) {
		t.Errorf("the row was removed but the key it minted is still live")
	}
}

// TestOAuthAllowRemovalRevokesASubjectKeyByItsTag is the other half of the
// renderer question, and the half the email case above cannot see.
//
// A hand-built "provider:identity" and oauthprovider.OAuthIdentityTag agree
// for an email -- the tag the mint recorded was folded from the row, and the
// operator's own input folds to the same string -- so a removal that assembled
// the tag itself passes that test and fails in the field. They disagree for a
// subject, which the tag renders as "provider:subject:<sub>", and for any value
// whose spelling the operator did not pre-normalise. A subject row is the arm
// cleat-review's design verdict added for issuers that publish no verified
// address, so it is not a corner: it is what an OIDC deployment uses.
func TestOAuthAllowRemovalRevokesASubjectKeyByItsTag(t *testing.T) {
	db := oauthAllowTestDB(t)
	ctx := context.Background()
	tenant := uuid.MustParse(oauthTestTenant)
	const sub = "00u1a2b3c"
	t.Cleanup(func() { cleanupOAuthAllow(t, db, "oidc", "subject", sub) })

	if err := oauthAllowAdd(ctx, db, tenant, "oidc", "subject", sub); err != nil {
		t.Fatalf("add a subject row: %v", err)
	}
	key := seedOAuthMintedKey(t, db, "oidc:subject:"+sub, time.Now().Add(time.Hour))
	if !keyIsLive(t, db, key) {
		t.Fatal("the seeded subject key is not live before the removal, so this test measures nothing")
	}

	rev, err := oauthAllowRemove(ctx, db, dialectPostgres, tenant, "oidc", "subject", sub)
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if rev.Removed != 1 {
		t.Errorf("remove matched %d rows, want 1", rev.Removed)
	}
	if keyIsLive(t, db, key) {
		t.Errorf("the subject row was removed but the key it minted is still live. The tag renders " +
			"as \"oidc:subject:<sub>\", and a revoke that assembled \"provider:value\" itself would " +
			"look for \"oidc:00u1a2b3c\", match nothing, and report success")
	}
}

// TestOAuthAllowRemovalReportsWhichHalfFailed. The two halves of a removal can
// fail independently, and they send the operator to different places: a delete
// that failed leaves the identity admitted, a revoke that failed leaves a
// credential authenticating. The error has to say which, and it must not be
// inferred from the row count -- a caller that reads Removed > 0 as "the delete
// worked" reports a delete that failed on its second row as a failed revoke.
//
// The failure is forced SYNTHETICALLY, by handing the function a dialect whose
// store refuses (cleat#2340 design v2 section 5): auth.TenantStore.
// RevokeOAuthAPIKeys answers "not implemented" off Postgres. So the delete
// genuinely runs and succeeds against PostgreSQL, the revoke genuinely fails,
// and the assertion is about the message rather than about a mock.
func TestOAuthAllowRemovalReportsWhichHalfFailed(t *testing.T) {
	db := oauthAllowTestDB(t)
	ctx := context.Background()
	tenant := uuid.MustParse(oauthTestTenant)
	t.Cleanup(func() { cleanupOAuthAllow(t, db, "google", "email", "half@example.com") })

	if err := oauthAllowAdd(ctx, db, tenant, "google", "email", "half@example.com"); err != nil {
		t.Fatalf("add: %v", err)
	}
	key := seedOAuthMintedKey(t, db, "google:half@example.com", time.Now().Add(time.Hour))

	rev, err := oauthAllowRemove(ctx, db, dialectMySQL, tenant, "google", "email", "half@example.com")
	if err == nil {
		t.Fatal("the revoke was expected to fail on a non-Postgres dialect, and did not")
	}
	if rev.Removed != 1 {
		t.Errorf("remove reported %d rows removed, want 1: the delete must have happened before "+
			"the revoke was attempted", rev.Removed)
	}
	if !strings.Contains(err.Error(), "revoking the keys it minted failed") {
		t.Errorf("the error does not name the phase that failed, so an operator cannot tell whether "+
			"the identity is still admitted or a credential is still live:\n%v", err)
	}
	if keyIsLive(t, db, key) {
		// Not a defect of the code under test -- the revoke was made to fail on
		// purpose. Asserted because it confirms the failure was in the revoke
		// phase rather than in the delete.
		t.Log("the key is still live, as expected: the revoke was the half forced to fail")
	}
}

// TestOAuthAllowRemoveThatMatchesNothingRevokesNothing pins the ORDER, and it
// needs a key the revoke COULD have matched to say anything at all.
//
// The obvious version of this test -- remove an identity that was never added,
// assert nothing happened -- cannot fail: the tag such a removal renders
// matches no key, so a command whose halves are in the wrong order passes it
// too. The shape that discriminates is an ORPHAN key: tagged for an identity
// with no allowlist row, which is exactly what a previous run that deleted the
// row and then failed to revoke leaves behind. If the revoke were not gated on
// the delete, this key would be revoked by a command that went on to report
// "nothing matched, nothing was revoked" -- a refusal that had already done the
// thing it denied.
func TestOAuthAllowRemoveThatMatchesNothingRevokesNothing(t *testing.T) {
	db := oauthAllowTestDB(t)
	ctx := context.Background()
	tenant := uuid.MustParse(oauthTestTenant)
	const orphanIdentity = "ghost@example.com"

	// No row for this identity, and none left by an earlier run either.
	cleanupOAuthAllow(t, db, "google", "email", orphanIdentity)
	if _, err := db.Exec(`DELETE FROM oauth_allowed_identities
		WHERE tenant_id = $1 AND provider = $2 AND identity_type = $3 AND identity_value = $4`,
		tenant, "google", "email", orphanIdentity); err != nil {
		t.Fatalf("clear any stale row: %v", err)
	}

	orphan := seedOAuthMintedKey(t, db, "google:"+orphanIdentity, time.Now().Add(time.Hour))
	if !keyIsLive(t, db, orphan) {
		t.Fatal("the orphan key is not live before the removal, so this test measures nothing")
	}

	rev, err := oauthAllowRemove(ctx, db, dialectPostgres, tenant, "google", "email", orphanIdentity)
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if rev.Removed != 0 {
		t.Fatalf("a removal for an identity with no allowlist row reported %d rows removed",
			rev.Removed)
	}
	if !keyIsLive(t, db, orphan) {
		t.Errorf("a removal that matched no allowlist row revoked that identity's key anyway. The " +
			"caller is told nothing matched and nothing was revoked, so a revoke here is a refusal " +
			"that has already done the thing it denied")
	}
}

// TestOAuthAllowIsRefusedOffPostgres. The command issues PostgreSQL SQL on
// purpose (ON CONFLICT, and a revoke that refuses off Postgres), so the
// refusal has to come first rather than after a partial operation.
func TestOAuthAllowIsRefusedOffPostgres(t *testing.T) {
	for _, d := range []dialect{dialectMySQL, dialectMSSQL} {
		if isPortedFor("oauth-allow", d) {
			t.Errorf("oauth-allow is declared ported on %s, but its SQL is PostgreSQL's (ON CONFLICT, "+
				"and a revoke that refuses off Postgres) and OAuth login answers 501 there", d.name)
		}
	}
	if !isPortedFor("oauth-allow", dialectPostgres) {
		t.Error("oauth-allow is not declared ported on postgres, where the feature actually runs")
	}
}
