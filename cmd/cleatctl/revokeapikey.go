package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// revoke-api-key command
// ---------------------------------------------------------------------------
//
// Why this exists. auth.TenantStore.RevokeAPIKey has been implemented and
// well tested since it was written -- five tests, including one asserting a
// revoked key can no longer authenticate -- and had ZERO production callers.
// No CLI subcommand, no HTTP route. So the one operation an operator needs
// during a credential incident was the only one with no way to invoke it,
// and rotating a leaked key meant hand-writing UPDATE statements against
// admin.tenant_api_keys under time pressure. That is the same "implemented
// but unreachable" shape as admin.drop_tenant before Finding S3 gave it a
// caller, and it is worse here because of when you need it.
//
// Surface choice: DBA-only, same as drop-tenant, check-db and versions
// purge. See droptenant.go's header for the full argument -- the short
// version is that cleat's HTTP auth resolves a request to exactly one
// tenant and has no notion of a platform operator, so there is nothing to
// gate a cross-tenant credential operation on. Authenticating by requiring
// a privileged PostgreSQL DSN is honest about what this is.
//
// Guard rails, deliberately LIGHTER than drop-tenant's, and the asymmetry
// is the point:
//
//   - drop-tenant destroys data irreversibly, so it demands the tenant ID
//     typed back exactly. This command sets a revoked_at timestamp. It is
//     reversible by an operator with the same access (UPDATE ... SET
//     revoked_at = NULL), and it is what you reach for *during* an
//     incident, when every extra prompt is a reason to reach for raw SQL
//     instead -- which is precisely the behaviour this command exists to
//     stop. Making a safety action slow makes people route around it.
//   - It still always prints the row it is about to revoke first, so an
//     operator sees which tenant and which description they are cutting
//     off before it happens. --dry-run stops there.
//
// There is deliberately NO --key flag taking the raw cleat_sk_ value.
// Command-line arguments are visible in ps and /proc/<pid>/cmdline to any
// co-resident user, so accepting a live credential there would leak it to
// exactly the audience you are revoking it from. Stream M fixed this same
// pattern in plugins/scheduledbackup, which passed a DSN with a password
// as an exec argument. Use --key-stdin, which reads the key on stdin and
// hashes it locally without it ever reaching argv.

const revokeAPIKeyUsage = `Usage: cleatctl revoke-api-key [flags]

Revokes a cleat API key so it can no longer authenticate. Exactly one of
--key-id, --key-hash or --key-stdin identifies the key.

Flags:
  --key-id <uuid>    Revoke by key_id (see --list).
  --key-hash <hex>   Revoke by sha256 hex of the key. Use when you have the
                     hash from a log or an incident report but not the key.
  --key-stdin        Read the raw key (cleat_sk_...) on stdin and hash it
                     locally. Never pass a live key as a command-line
                     argument -- argv is world-readable via ps.
  --list <tenant>    List a tenant's keys (never prints key material) and exit.
  --dry-run          Show what would be revoked, change nothing.

Examples:
  cleatctl --db "$DSN" revoke-api-key --list 00000000-0000-0000-0000-000000000000
  cleatctl --db "$DSN" revoke-api-key --key-id 3f2a...
  printf '%s' "$LEAKED_KEY" | cleatctl --db "$DSN" revoke-api-key --key-stdin
`

func runRevokeAPIKey(ctx context.Context, db *sql.DB, args []string) {
	fs := flag.NewFlagSet("revoke-api-key", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { fmt.Fprintf(os.Stderr, "%s", revokeAPIKeyUsage) }

	keyID := fs.String("key-id", "", "key_id (uuid) to revoke")
	keyHashHex := fs.String("key-hash", "", "sha256 hex of the key to revoke")
	keyStdin := fs.Bool("key-stdin", false, "read the raw key on stdin and hash it locally")
	list := fs.String("list", "", "list a tenant's API keys and exit")
	dryRun := fs.Bool("dry-run", false, "show what would be revoked, change nothing")

	if err := fs.Parse(args); err != nil {
		osExit(2)
		return
	}

	if *list != "" {
		if err := listAPIKeys(ctx, db, *list); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			osExit(1)
		}
		return
	}

	selector, err := revokeSelector(*keyID, *keyHashHex, *keyStdin, os.Stdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n\n", err)
		fmt.Fprintf(os.Stderr, "%s", revokeAPIKeyUsage)
		osExit(2)
		return
	}

	// Always show the row before touching it. An operator who mistypes a
	// hash should find out by seeing the wrong description here, not by
	// discovering later that the wrong integration stopped working.
	row, err := findAPIKey(ctx, db, selector)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		osExit(1)
		return
	}
	if row == nil {
		// Not an error worth exit 1: on a rotation you often cannot tell in
		// advance whether a leaked key was ever provisioned in THIS database,
		// and "no such key here" is a useful, expected answer. It is also the
		// answer you get if you point --db at the wrong deployment, which is
		// why the message says so.
		fmt.Println("No matching API key in this database (already revoked keys are still shown, so this means no such key_hash/key_id exists here).")
		fmt.Println("If you expected a match, check that --db points at the deployment that issued the key.")
		return
	}

	printAPIKeyRows([]apiKeyRow{*row})

	if row.revokedAt.Valid {
		fmt.Printf("\nAlready revoked at %s. Nothing to do.\n", row.revokedAt.Time.Format("2006-01-02 15:04:05 MST"))
		return
	}
	if *dryRun {
		fmt.Println("\n--dry-run: no change made.")
		return
	}

	res, err := db.ExecContext(ctx,
		`UPDATE admin.tenant_api_keys SET revoked_at = now()
		 WHERE key_id = $1 AND revoked_at IS NULL`, row.keyID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: revoke: %v\n", err)
		osExit(1)
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		// Lost a race with a concurrent revoke. Same end state, so not an
		// error -- but say so rather than printing a success that implies
		// this invocation is what did it.
		fmt.Println("\nKey was revoked concurrently by someone else. End state is correct.")
		return
	}

	fmt.Printf("\nRevoked key_id %s (tenant %s).\n", row.keyID, row.tenantID)
	fmt.Println("Effective immediately: every lookup path filters `revoked_at IS NULL`.")
	fmt.Printf("Issue a replacement with:\n  cleat-worker --generate-api-key %s --db \"$DSN\"\n", row.tenantID)
}

// revokeKeySelector is either a key_id or a key_hash. Exactly one is set.
type revokeKeySelector struct {
	keyID   uuid.UUID
	keyHash []byte
}

// revokeSelector validates that exactly one selector was given and turns it
// into a lookup. Split out from runRevokeAPIKey so the mutual exclusion and
// the stdin hashing are testable without a database.
func revokeSelector(keyID, keyHashHex string, keyStdin bool, stdin io.Reader) (revokeKeySelector, error) {
	n := 0
	if keyID != "" {
		n++
	}
	if keyHashHex != "" {
		n++
	}
	if keyStdin {
		n++
	}
	switch {
	case n == 0:
		return revokeKeySelector{}, fmt.Errorf("one of --key-id, --key-hash or --key-stdin is required")
	case n > 1:
		return revokeKeySelector{}, fmt.Errorf("--key-id, --key-hash and --key-stdin are mutually exclusive")
	}

	switch {
	case keyID != "":
		id, err := uuid.Parse(keyID)
		if err != nil {
			return revokeKeySelector{}, fmt.Errorf("--key-id %q is not a uuid: %w", keyID, err)
		}
		return revokeKeySelector{keyID: id}, nil

	case keyHashHex != "":
		h, err := hex.DecodeString(strings.TrimSpace(keyHashHex))
		if err != nil {
			return revokeKeySelector{}, fmt.Errorf("--key-hash is not valid hex: %w", err)
		}
		if len(h) != sha256.Size {
			return revokeKeySelector{}, fmt.Errorf("--key-hash must be %d hex bytes (sha256), got %d", sha256.Size, len(h))
		}
		return revokeKeySelector{keyHash: h}, nil

	default: // keyStdin
		raw, err := io.ReadAll(io.LimitReader(stdin, 4096))
		if err != nil {
			return revokeKeySelector{}, fmt.Errorf("read key from stdin: %w", err)
		}
		// Trim only trailing newlines: `echo` adds one and `printf` does not,
		// and an operator should not have to know which they used. Everything
		// else is preserved -- the hash must be over the exact key bytes, and
		// silently stripping interior whitespace would produce a hash that
		// matches nothing with no indication why.
		key := strings.TrimRight(string(raw), "\r\n")
		if key == "" {
			return revokeKeySelector{}, fmt.Errorf("--key-stdin: no key on stdin")
		}
		sum := sha256.Sum256([]byte(key))
		return revokeKeySelector{keyHash: sum[:]}, nil
	}
}

type apiKeyRow struct {
	keyID       uuid.UUID
	tenantID    uuid.UUID
	description string
	createdAt   sql.NullTime
	revokedAt   sql.NullTime
}

// findAPIKey looks a key up by whichever selector was given. It deliberately
// does NOT filter on revoked_at: reporting "already revoked at <time>" is more
// useful during an incident than reporting "not found", which an operator
// would reasonably read as "wrong database".
func findAPIKey(ctx context.Context, db *sql.DB, sel revokeKeySelector) (*apiKeyRow, error) {
	var (
		row apiKeyRow
		err error
	)
	q := `SELECT key_id, tenant_id, description, created_at, revoked_at
	      FROM admin.tenant_api_keys WHERE `
	if sel.keyHash != nil {
		err = db.QueryRowContext(ctx, q+`key_hash = $1`, sel.keyHash).
			Scan(&row.keyID, &row.tenantID, &row.description, &row.createdAt, &row.revokedAt)
	} else {
		err = db.QueryRowContext(ctx, q+`key_id = $1`, sel.keyID).
			Scan(&row.keyID, &row.tenantID, &row.description, &row.createdAt, &row.revokedAt)
	}
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("look up api key: %w", err)
	}
	return &row, nil
}

func listAPIKeys(ctx context.Context, db *sql.DB, tenant string) error {
	tid, err := uuid.Parse(tenant)
	if err != nil {
		return fmt.Errorf("--list %q is not a tenant uuid: %w", tenant, err)
	}
	rows, err := db.QueryContext(ctx,
		`SELECT key_id, tenant_id, description, created_at, revoked_at
		 FROM admin.tenant_api_keys WHERE tenant_id = $1
		 ORDER BY created_at DESC`, tid)
	if err != nil {
		return fmt.Errorf("list api keys: %w", err)
	}
	defer rows.Close()

	var out []apiKeyRow
	for rows.Next() {
		var r apiKeyRow
		if err := rows.Scan(&r.keyID, &r.tenantID, &r.description, &r.createdAt, &r.revokedAt); err != nil {
			return fmt.Errorf("scan api key row: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate api keys: %w", err)
	}
	if len(out) == 0 {
		fmt.Printf("No API keys for tenant %s.\n", tid)
		return nil
	}
	printAPIKeyRows(out)
	return nil
}

// printAPIKeyRows prints key metadata only. key_hash is never printed: it is
// not the key, but it is the exact value an attacker needs to match against a
// stolen key to confirm they have a live one, and there is no operator task
// that needs it on screen.
func printAPIKeyRows(rows []apiKeyRow) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "KEY_ID\tTENANT\tSTATUS\tCREATED\tDESCRIPTION")
	for _, r := range rows {
		status := "active"
		if r.revokedAt.Valid {
			status = "revoked " + r.revokedAt.Time.Format("2006-01-02")
		}
		created := "-"
		if r.createdAt.Valid {
			created = r.createdAt.Time.Format("2006-01-02")
		}
		desc := r.description
		if desc == "" {
			desc = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", r.keyID, r.tenantID, status, created, desc)
	}
	_ = w.Flush()
}
