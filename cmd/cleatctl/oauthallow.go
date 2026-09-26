package main

// cleatctl oauth-allow -- manage a tenant's OAuth identity allowlist.
//
// cleat#2340. Until this command existed, oauth_allowed_identities had no
// writer anywhere in the tree: the only way to admit anyone was a hand-written
// INSERT, and the only way to remove them was a hand-written DELETE that also
// had to know to revoke the keys that row had minted. That is the state
// cleat#2371 shipped fail-closed on -- a fresh deployment denies every OAuth
// login until an operator writes a row -- and it is why the owner's decision
// named "the allowlist CLI help" as one of the places the authority warning has
// to appear.
//
// THE WARNING IS THE POINT OF THE HELP TEXT, not decoration. A minted key
// carries FULL TENANT API POWER -- `tenant_api_keys` has no scope or role, so
// an allowlisted identity can deploy workflow code, reprocess, schedule and
// reach /api/admin/* when that is enabled. The owner chose that over adding a
// role column in 0.3.0, and the price of the choice is that the operator
// granting access has to be told what they are granting. So it appears in the
// usage text AND in the output of `add`, at the moment access is given.

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/internal/tenantctx"
	"github.com/cleat-team/cleat/plugins/oauthprovider"
	"github.com/google/uuid"
)

// oauthIdentityTypes are the two kinds of key an allowlist row can carry.
// Spelled here rather than exported from the plugin because the CLI's contract
// with the operator is a pair of words they type; the plugin's constants stay
// its own.
const (
	oauthTypeEmail   = "email"
	oauthTypeSubject = "subject"
)

func runOAuthAllow(ctx context.Context, db *sql.DB, d dialect, args []string) {
	fs := flag.NewFlagSet("oauth-allow", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	provider := fs.String("provider", "", "the provider this row applies to, e.g. google, github, oidc")
	identityType := fs.String("type", oauthTypeEmail, "which kind of key this row is: email or subject")

	operands, err := parseFlagsAnywhere(fs, args)
	if err != nil {
		osExit(1)
		return
	}
	if len(operands) < 2 || *provider == "" {
		printOAuthAllowUsage()
		osExit(1)
		return
	}

	sub, tenantArg := operands[0], operands[1]
	tenant, err := uuid.Parse(tenantArg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %q is not a tenant UUID: %v\n", tenantArg, err)
		osExit(1)
		return
	}

	switch sub {
	case "list":
		runOAuthAllowList(ctx, db, tenant, *provider)
	case "add", "remove":
		if len(operands) < 3 {
			printOAuthAllowUsage()
			osExit(1)
			return
		}
		if *identityType != oauthTypeEmail && *identityType != oauthTypeSubject {
			fmt.Fprintf(os.Stderr, "error: --type must be %q or %q, got %q\n",
				oauthTypeEmail, oauthTypeSubject, *identityType)
			osExit(1)
			return
		}
		if sub == "add" {
			runOAuthAllowAdd(ctx, db, tenant, *provider, *identityType, operands[2])
		} else {
			runOAuthAllowRemove(ctx, db, d, tenant, *provider, *identityType, operands[2])
		}
	default:
		printOAuthAllowUsage()
		osExit(1)
	}
}

// oauthAllowedIdentity is one allowlist row as the CLI reports it.
type oauthAllowedIdentity struct {
	Type  string
	Value string
}

// oauthAllowList returns the tenant's rows for one provider, ordered so two
// runs print identically.
//
// Split from the printing loop below the way egressList/egressAdd/egressRemove
// are (egressallow.go): the query is the part worth testing against a real
// database, and a function that prints and calls osExit cannot be called from
// a test at all.
func oauthAllowList(ctx context.Context, db *sql.DB, tenant uuid.UUID, provider string) ([]oauthAllowedIdentity, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT identity_type, identity_value
		FROM oauth_allowed_identities
		WHERE tenant_id = $1 AND provider = $2
		ORDER BY identity_type, identity_value`, tenant, provider)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var listed []oauthAllowedIdentity
	for rows.Next() {
		var e oauthAllowedIdentity
		if err := rows.Scan(&e.Type, &e.Value); err != nil {
			return nil, err
		}
		listed = append(listed, e)
	}
	return listed, rows.Err()
}

// oauthAllowRowsFoldingTo returns the stored identity_value of every row whose
// normalised form equals want, so a removal can address each by the exact
// spelling it was written with. See oauthAllowRemove for why the comparison is
// in Go rather than in the WHERE clause.
func oauthAllowRowsFoldingTo(ctx context.Context, db *sql.DB, tenant uuid.UUID,
	provider, identityType, want string) ([]string, error) {

	rows, err := db.QueryContext(ctx, `
		SELECT identity_value
		FROM oauth_allowed_identities
		WHERE tenant_id = $1 AND provider = $2 AND identity_type = $3`, tenant, provider, identityType)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var matched []string
	for rows.Next() {
		var stored string
		if err := rows.Scan(&stored); err != nil {
			return nil, err
		}
		if oauthprovider.OAuthIdentityValue(identityType, stored) == want {
			matched = append(matched, stored)
		}
	}
	return matched, rows.Err()
}

// oauthAllowAdd admits one identity for one provider, storing the value in the
// form the MATCHER compares in.
//
// Normalised HERE, at the point an operator's typing enters the table, because
// that is what makes a later `remove` able to find the row again. The matcher
// (identityAllowed) folds both sides in Go, so an un-normalised row would still
// be admitted -- and a remove that deleted by the raw value typed would then
// miss it while the revoke computed from that same value succeeded, leaving the
// row present and its keys dead. oauthprovider.OAuthIdentityValue is that
// normaliser, shared rather than reimplemented here.
//
// ON CONFLICT DO NOTHING against the table's own primary key
// (tenant_id, provider, identity_type, identity_value): re-adding a row that is
// already there is not an error, so a setup script re-run does not have to know
// whether it is the first time.
//
// This SQL is PostgreSQL's, which is why `oauth-allow` is declared Postgres-only
// in ported.go -- MySQL spells the same statement ON DUPLICATE KEY UPDATE and
// SQL Server wants a MERGE, and OAuth login answers 501 on both anyway
// (cleat#2340 design v2 section 5).
func oauthAllowAdd(ctx context.Context, db *sql.DB, tenant uuid.UUID, provider, identityType, value string) error {
	_, err := db.ExecContext(ctx, `
		INSERT INTO oauth_allowed_identities (tenant_id, provider, identity_type, identity_value)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (tenant_id, provider, identity_type, identity_value) DO NOTHING`,
		tenant, provider, identityType, oauthprovider.OAuthIdentityValue(identityType, value))
	return err
}

// oauthRevocation is what a successful removal did: the allowlist row is gone,
// and Removed is how many rows that was (0 or 1).
//
// The two facts are reported separately because they answer different
// questions and an operator needs both. "No row matched" is not a failure of
// the revoke and must not be reported as one: a removal that matched nothing
// revokes nothing, and a caller that cannot tell those apart will read its own
// typo as a successful revocation.
type oauthRevocation struct {
	Removed int64
}

// oauthAllowRemove deletes the allowlist row(s) admitting one identity and, in
// the same command, revokes the live keys that row minted. cleat#2340 design
// item 4.
//
// THE ROWS ARE FOUND BY THE MATCHER'S RULE, NOT BY THE SPELLING TYPED, and that
// is not a refinement -- without it the command has a dead end. A row written
// by `oauth-allow add` is stored normalised, so an operator could always name it
// back. A row written by hand is whatever the operator typed, and this table's
// only writer before the CLI existed was a hand-written INSERT (the state
// cleat#2371 shipped fail-closed in). Deleting by the normalised value would
// miss those rows -- and the advice the refusal prints, "check `list` and use
// the value it shows", would be unusable, because the CLI normalises whatever
// the operator then types back to the same non-matching string. An identity
// that can be listed but not removed is the worst shape for this command.
//
// So the comparison happens in Go, on both sides, exactly as identityAllowed
// compares them -- one rule, applied by the reader and the remover both. Each
// matched row is then deleted by its OWN stored spelling, which is a primary
// key component and therefore unambiguous even when two spellings fold
// together.
//
// THE ORDER IS THE OTHER PROPERTY. Every delete runs before any revoke, and
// matching nothing returns without revoking anything, so "remove an identity
// that was never there" cannot revoke a key belonging to someone else.
// Reversed, a mistyped identity would still reach the revoke -- harmless only
// while the tag it renders also matches nothing, i.e. only while the mistake
// is in the one field that makes it harmless.
//
// THE REVOKE USES THE SAME RENDERER THE MINT USED, and that is the whole of
// cleat#2410: the column is matched by exact string equality, so a tag
// assembled here by hand would match nothing, silently, for every row not
// already normalised. Both halves call oauthprovider's own functions.
func oauthAllowRemove(ctx context.Context, db *sql.DB, d dialect, tenant uuid.UUID,
	provider, identityType, value string) (oauthRevocation, error) {

	want := oauthprovider.OAuthIdentityValue(identityType, value)
	matched, err := oauthAllowRowsFoldingTo(ctx, db, tenant, provider, identityType, want)
	if err != nil {
		return oauthRevocation{}, fmt.Errorf("reading the allowlist: %w", err)
	}

	var removed int64
	for _, stored := range matched {
		res, err := db.ExecContext(ctx, `
			DELETE FROM oauth_allowed_identities
			WHERE tenant_id = $1 AND provider = $2 AND identity_type = $3 AND identity_value = $4`,
			tenant, provider, identityType, stored)
		if err != nil {
			// The phase is IN THE ERROR, not inferred by the caller from a
			// row count. A caller that decided "was the row removed?" from
			// Removed > 0 would report a delete that failed after its first
			// row as a failed REVOKE, sending the operator to the wrong half
			// of the command.
			return oauthRevocation{Removed: removed},
				fmt.Errorf("removing the allowlist row %q: %w", stored, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return oauthRevocation{Removed: removed},
				fmt.Errorf("removing the allowlist row %q: %w", stored, err)
		}
		removed += n
	}
	if removed == 0 {
		return oauthRevocation{}, nil
	}

	// LEAVING THE KEYS ALIVE IS THE FAILURE THIS COMMAND EXISTS TO PREVENT. An
	// operator who removes someone from the list has revoked their access; a
	// credential that keeps authenticating until it expires means the operator
	// removed nothing in practice, and nothing on the deployment says so.
	store, err := auth.NewTenantStoreForDialect(db, d.name)
	if err != nil {
		return oauthRevocation{Removed: removed},
			fmt.Errorf("the allowlist row is removed, but the key store could not be opened to "+
				"revoke the keys it minted: %w", err)
	}
	if _, err := store.RevokeOAuthAPIKeys(ctx, oauthprovider.OAuthIdentityTag(provider, identityType, value)); err != nil {
		return oauthRevocation{Removed: removed},
			fmt.Errorf("the allowlist row is removed, but revoking the keys it minted failed: %w", err)
	}
	return oauthRevocation{Removed: removed}, nil
}

func runOAuthAllowList(ctx context.Context, db *sql.DB, tenant uuid.UUID, provider string) {
	listed, err := oauthAllowList(tenantctx.With(ctx, tenant), db, tenant, provider)
	if err != nil {
		fmt.Fprintf(os.Stderr, "reading the allowlist: %v\n", err)
		osExit(1)
		return
	}

	if len(listed) == 0 {
		// Said explicitly, for the reason egress-allow says it: an empty
		// listing and "this tenant admits nobody" print identically, and only
		// one of them is what an operator expects to have configured. Here it
		// is also the fail-closed state cleat#2371 shipped, so it is worth
		// naming as such rather than leaving as a blank.
		fmt.Printf("tenant %s admits NO identities for %s: every OAuth login for this pair is refused.\n",
			tenant, provider)
		return
	}
	fmt.Printf("tenant %s admits %d identit(ies) for %s:\n", tenant, len(listed), provider)
	for _, e := range listed {
		fmt.Printf("  %s\t%s\n", e.Type, e.Value)
	}
	fmt.Println()
	printOAuthAuthorityWarning(os.Stdout)
}

func runOAuthAllowAdd(ctx context.Context, db *sql.DB, tenant uuid.UUID, provider, identityType, value string) {
	if err := oauthAllowAdd(tenantctx.With(ctx, tenant), db, tenant, provider, identityType, value); err != nil {
		fmt.Fprintf(os.Stderr, "adding the identity: %v\n", err)
		osExit(1)
		return
	}

	fmt.Printf("tenant %s now admits %s for %s.\n", tenant,
		oauthprovider.OAuthIdentityTag(provider, identityType, value), provider)
	fmt.Println()
	printOAuthAuthorityWarning(os.Stdout)
}

func runOAuthAllowRemove(ctx context.Context, db *sql.DB, d dialect, tenant uuid.UUID,
	provider, identityType, value string) {

	tag := oauthprovider.OAuthIdentityTag(provider, identityType, value)
	rev, err := oauthAllowRemove(tenantctx.With(ctx, tenant), db, d, tenant, provider, identityType, value)
	if rev.Removed == 0 && err == nil {
		// Not an error, but said rather than implied: "removed nothing" and
		// "removed the row you meant" print identically otherwise, and the
		// operator's next question is whether the identity is listed under a
		// different type or a differently-spelled value.
		fmt.Fprintf(os.Stderr,
			"no row admits %s for %s (tenant %s); nothing was removed and no keys were revoked.\n"+
				"`cleatctl oauth-allow list %s --provider %s` shows what is admitted for this pair.\n",
			tag, provider, tenant, tenant, provider)
		osExit(1)
		return
	}
	if err != nil {
		// Printed unmodified: each error names the phase it came from, which
		// the caller cannot reconstruct from Removed alone. What the caller
		// adds is the consequence, and only when a row is actually gone.
		fmt.Fprintf(os.Stderr, "%v\n", err)
		if rev.Removed > 0 {
			fmt.Fprintln(os.Stderr,
				"The identity is no longer admitted. Any key it minted that was not revoked keeps "+
					"authenticating until it expires; `cleatctl revoke-api-key --list` shows them.")
		}
		osExit(1)
		return
	}

	fmt.Printf("tenant %s no longer admits %s for %s.\n", tenant, tag, provider)
}

// printOAuthAuthorityWarning is the owner's decision, in the two places an
// operator will read it: the usage text, and the output of the command that
// grants access.
func printOAuthAuthorityWarning(w io.Writer) {
	fmt.Fprint(w, `NOTE: a login admitted by this list mints an API key carrying FULL TENANT
ACCESS -- the same power as any other tenant_api_keys row, including deploying
workflow code, reprocessing runs, running scheduled jobs, and the admin API when
it is enabled. There is no scope or role to narrow it in this release. Add only
identities you would give that tenant's API key to.
`)
}

func printOAuthAllowUsage() {
	fmt.Fprint(os.Stderr, `Usage:
  cleatctl --db <dsn> oauth-allow list   <tenant-uuid> --provider <provider>
  cleatctl --db <dsn> oauth-allow add    <tenant-uuid> --provider <provider> [--type email|subject] <identity>
  cleatctl --db <dsn> oauth-allow remove <tenant-uuid> --provider <provider> [--type email|subject] <identity>

A tenant with NO rows for a (tenant, provider) pair refuses every OAuth login
for it. That is deliberate: "nobody is listed" and "this tenant has no list"
are the same answer, so a mistyped tenant id cannot silently admit anyone.

--type email   (the default) matches only an address the provider VOUCHES for.
               An address whose verification is absent reads as unverified and
               is refused, so this is the stricter of the two.
--type subject matches the provider's stable account id (an OIDC "sub", a
               GitHub numeric id). Use it when the issuer publishes no verified
               address, or to survive an address being reassigned.

remove also REVOKES every live key that row minted, in the same command.
Removing a row without revoking would leave the credential working until it
expired, which is not what an operator means by "remove".

Examples:
  cleatctl --db "$DSN" oauth-allow add 11111111-1111-4111-8111-111111111111 --provider google alice@example.com
  cleatctl --db "$DSN" oauth-allow list 11111111-1111-4111-8111-111111111111 --provider google
  cleatctl --db "$DSN" oauth-allow remove 11111111-1111-4111-8111-111111111111 --provider google alice@example.com
  cleatctl --db "$DSN" oauth-allow add 11111111-1111-4111-8111-111111111111 --provider oidc --type subject 00u1a2b3c

`)
	printOAuthAuthorityWarning(os.Stderr)
}
