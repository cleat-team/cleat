package oauthprovider

// cleat#2340 design v2 §(7): the background loop also soft-disables
// OAuth-minted API keys whose expiry has passed.
//
// WHAT THIS TEST IS AND IS NOT FOR. Expiry itself is enforced by
// resolveAPIKeyStmt at READ time, on every dialect, and is covered by
// auth/TestExpiredKeyCannotAuthenticate -- a worker that never ran this loop
// still would not authenticate an expired key. What this pins is the narrower
// invariant the sweep exists to keep: for a key OAuth login minted,
// `disabled_at IS NULL` keeps meaning "this key authenticates". Delete the
// sweep and this test goes red while that one stays green, which is the
// distinction worth being able to see.
//
// It also pins the two boundaries the predicate draws, because both are
// cases a plausible wrong version would get wrong in the direction that
// leaves dead rows looking live:
//
//   - a manually-provisioned key with an operator-set expires_at is NOT
//     swept, even though its expiry has passed -- the statement is scoped to
//     oauth_identity IS NOT NULL, and un-revoking is not a supported
//     operation in this release (see auth.TenantStore.RevokeExpiredOAuthAPIKeys).
//   - a key already disabled keeps its original disabled_at rather than
//     being rewritten to the sweep's now() -- the audit trail records when
//     the credential was retired, not when a loop next noticed it.
//
// And it pins the dialect gate: the sweep is Postgres-only, because §(5)
// makes OAuth login itself Postgres-only, so on MySQL and SQL Server no key can
// have been minted and the correct behaviour is to make no host call at all.
//
// IT PINS THAT BY COUNTING CALLS, NOT BY CHECKING ROWS, and the distinction was
// measured rather than assumed. Deleting the gate leaves every seeded row on
// MySQL exactly as it was -- because the host function refuses on those
// dialects anyway -- so a rows-only assertion reports the mutation green. The
// gate and the refusal produce identical tables and are told apart by nothing
// observable in the data. Counting the calls is what separates "this loop asked
// nothing of the host" from "this loop asked and was refused", and only the
// first is what §(5) asks for.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
	"github.com/cleat-team/cleat/plugins/plugintest"
)

// apiKeysTable names the table the sweep writes, per dialect. PostgreSQL and
// SQL Server put it in the admin schema; MySQL's tenant_api_keys lives in the
// worker's own database, where `admin.` would name a database rather than a
// schema (the prefix trap cmd/cleat-worker/apikeycount.go documents from
// cleat#1963). Spelled per dialect here for the same reason the sweep is
// gated rather than written three ways: an unqualified name on PostgreSQL
// resolves through search_path, which is how an earlier cleanup entry for
// this table sat inert for weeks (engine/testutil/schema.go).
func apiKeysTable(dialect plugin.Dialect) string {
	if dialect == plugin.DialectMySQL {
		return "tenant_api_keys"
	}
	return "admin.tenant_api_keys"
}

// sweptKey is one seeded row plus what the sweep must do to it.
type sweptKey struct {
	name string
	why  string

	// The values seeded, so the assertion can compare rather than merely
	// check presence -- see the disabled_at case in the test's own comment.
	oauthIdentity sql.NullString
	expiresAt     sql.NullTime
	disabledAt    sql.NullTime

	// wantSwept is what the sweep must do on POSTGRES, where it runs. On the
	// dialects §(5) excludes it is false for every row including the first.
	wantSwept bool
}

func TestSweepDisablesOnlyExpiredOAuthMintedKeys(t *testing.T) {
	for _, be := range testutil.NewPluginTestBackends(t) {
		be := be
		t.Run(be.Name, func(t *testing.T) {
			defer be.Cleanup()

			ctx := context.Background()
			dialect := plugin.Dialect(string(be.Dialect))
			testutil.SetupFullSchema(t, be.DB, be.Dialect)

			tenantID := uuid.MustParse(engine.DefaultTenantUUID)
			fixtureDB := be.CrossTenantConn(t, ctx,
				"sweep fixture: seeds tenant_api_keys rows directly, bypassing the OAuth mint "+
					"path (plugin.MintOAuthAPIKey, cmd/cleat-worker) this test is not exercising")

			now := time.Now()
			past := now.Add(-2 * time.Hour)
			future := now.Add(2 * time.Hour)
			// A fixed instant in the past, so "the sweep did not rewrite it"
			// is a comparison rather than a guess at how much time passed.
			disabledLongAgo := now.Add(-48 * time.Hour)

			// One description per run, so cleanup removes exactly this run's
			// rows and two concurrent runs cannot delete each other's seed.
			marker := "sweep-fixture-" + uuid.NewString()

			rows := []sweptKey{
				{
					name: "an expired OAuth-minted key is disabled",
					why: "this is the row §(7) added the statement for: a credential whose expiry " +
						"has passed stops reading as live",
					oauthIdentity: sql.NullString{String: "google:user@example.com", Valid: true},
					expiresAt:     sql.NullTime{Time: past, Valid: true},
					wantSwept:     true,
				},
				{
					name: "a live OAuth-minted key is left alone",
					why: "the predicate is expires_at < now(), and a key inside its window is exactly " +
						"the credential a login just handed out",
					oauthIdentity: sql.NullString{String: "google:live@example.com", Valid: true},
					expiresAt:     sql.NullTime{Time: future, Valid: true},
				},
				{
					name: "an OAuth-minted key with no expiry is left alone",
					why: "expires_at IS NULL means 'no expiry' on this table (migration 105), so NULL " +
						"must not read as 'expired in 1970'",
					oauthIdentity: sql.NullString{String: "google:forever@example.com", Valid: true},
				},
				{
					name: "an already-disabled key keeps the instant it was retired",
					why: "disabled_at IS NULL is in the predicate, so the sweep must not touch this " +
						"row at all -- rewriting it to now() would move an administrative revoke's " +
						"timestamp forward every five minutes",
					oauthIdentity: sql.NullString{String: "google:revoked@example.com", Valid: true},
					expiresAt:     sql.NullTime{Time: past, Valid: true},
					disabledAt:    sql.NullTime{Time: disabledLongAgo, Valid: true},
				},
				{
					name: "an expired key this feature did not mint is left alone",
					why: "the predicate is scoped to oauth_identity IS NOT NULL; a service key whose " +
						"operator-set expiry has passed is still refused at read time, and un-revoking " +
						"is not supported, so flipping it would be a surprise with no way back",
					expiresAt: sql.NullTime{Time: past, Valid: true},
				},
			}

			// seed inserts one row and returns its key_id, which is the only
			// column that identifies it unambiguously on all three dialects
			// (key_hash is not unique; description is shared by the whole run).
			seed := func(k sweptKey) uuid.UUID {
				t.Helper()
				id := uuid.New()
				// The hash must be 32 bytes: this column is BYTEA on
				// PostgreSQL but BINARY(32) on MySQL, where a longer value is
				// a 1406 rather than a truncated row. Nothing here resolves a
				// key by it, so its content is arbitrary -- its LENGTH is not.
				hash := sha256.Sum256([]byte(id.String()))
				stmt := fmt.Sprintf(`INSERT INTO %s
					(tenant_id, key_id, key_hash, description, expires_at, disabled_at, oauth_identity)
					VALUES ($1, $2, $3, $4, $5, $6, $7)`, apiKeysTable(dialect))
				if _, err := plugintest.ExecRebound(t, ctx, fixtureDB, dialect, stmt,
					tenantID, id, hash[:], marker,
					k.expiresAt, k.disabledAt, k.oauthIdentity); err != nil {
					t.Fatalf("seed %s (%s): %v", k.name, be.Name, err)
				}
				return id
			}

			ids := make([]uuid.UUID, len(rows))
			for i, k := range rows {
				ids[i] = seed(k)
			}

			// A plain defer rather than t.Cleanup, matching this file's
			// neighbour (a_sweep_only_deletes_abandoned_logins_test.go): the
			// two mechanisms run at different times, and the ordering matters
			// when a backend's own cleanup drops the table.
			defer func() {
				if _, err := plugintest.ExecRebound(t, context.Background(), fixtureDB, dialect,
					fmt.Sprintf(`DELETE FROM %s WHERE description = $1`, apiKeysTable(dialect)),
					marker); err != nil {
					t.Errorf("cleanup %s rows on %s: %v", marker, be.Name, err)
				}
			}()

			// The logger reaches the test's own output rather than
			// io.Discard, and that is load-bearing rather than tidy: the
			// sweep logs an error and RETURNS, so a statement PostgreSQL
			// refuses looks exactly like a predicate that matched nothing --
			// both leave every row untouched. Discarding the log is how the
			// more interesting of those two reads as the duller one, which is
			// what happened the first time this test was run.
			//
			// THE REAL STORE, not a fake, and wired the way main.go wires it.
			// The sweep's write is host-delegated precisely because this
			// plugin's own cross-tenant path cannot reach the table (42501 --
			// see sweepExpiredOAuthKeys), so a fake that recorded a call
			// would prove the plugin asks and prove nothing about whether the
			// answer can be carried out. A deployment where this store's
			// UPDATE is refused is a deployment where the OAuth MINT is
			// refused too, on the same connection with the same grant.
			store, err := auth.NewTenantStoreForDialect(be.DB, string(dialect))
			if err != nil {
				t.Fatalf("open the key store on %s: %v", be.Name, err)
			}
			// The call count is what makes the dialect gate observable at all
			// -- see this file's header. The real store still does the work, so
			// the count rides on top of production's own implementation rather
			// than replacing it.
			var calls int
			p := &Plugin{dialect: dialect, logger: slog.New(slog.NewTextHandler(&testLogWriter{t: t}, nil))}
			p.revokeExpiredOAuthAPIKeys = func(ctx context.Context) (int64, error) {
				calls++
				return store.RevokeExpiredOAuthAPIKeys(ctx)
			}

			// Read the rows BEFORE the sweep, so "the sweep left this one
			// alone" is an exact comparison against what it actually found
			// rather than against a reconstruction of what was seeded. The
			// reconstruction is what a re-typed timestamp would be, and it
			// differs the moment either side's rendering does.
			before := make([]keyState, len(rows))
			for i := range rows {
				before[i] = readKeyState(t, ctx, fixtureDB, dialect, ids[i])
			}

			// sweepRan is §(5)'s scope, restated as the test's own expectation
			// rather than asked of the code under test: a helper that read the
			// plugin's dialect would follow a change to the gate instead of
			// catching it.
			sweepRan := dialect == plugin.DialectPostgres

			p.sweepExpiredOAuthKeys(ctx)

			wantCalls := 0
			if sweepRan {
				wantCalls = 1
			}
			if calls != wantCalls {
				t.Errorf("on %s: the sweep asked the host to disable expired keys %d time(s), want %d. "+
					"A call on a dialect where nothing can have minted a key is not a harmless extra: "+
					"it is a privilege question asked of a table this plugin cannot read, once per "+
					"tenant per tick, forever.", be.Name, calls, wantCalls)
			}

			for i, k := range rows {
				got := readKeyState(t, ctx, fixtureDB, dialect, ids[i])
				was := before[i]

				// Row identity first, so a failure below is never about the
				// seed having gone somewhere unexpected. oauth_identity is the
				// column the predicate keys on, so it is the one worth
				// confirming.
				if got.OAuthIdentity != was.OAuthIdentity {
					t.Fatalf("on %s: %s: seeded oauth_identity changed underneath the test: %v -> %v",
						be.Name, k.name, was.OAuthIdentity, got.OAuthIdentity)
				}
				if got.ExpiresAtString != was.ExpiresAtString {
					t.Fatalf("on %s: %s: expires_at changed underneath the test: %v -> %v -- "+
						"nothing in this plugin writes it, so something else is on this database",
						be.Name, k.name, was.ExpiresAtString, got.ExpiresAtString)
				}

				changed := got.DisabledAt.Valid != was.DisabledAt.Valid ||
					(got.DisabledAt.Valid && got.DisabledAt.String != was.DisabledAt.String)

				switch {
				case sweepRan && k.wantSwept:
					if !changed || !got.DisabledAt.Valid {
						t.Errorf("on %s: %s -- %s\n\n"+
							"the sweep did not disable this key (disabled_at was %v, is %v), so it "+
							"still counts as a live credential in every query that filters on "+
							"disabled_at alone",
							be.Name, k.name, k.why, was.DisabledAt, got.DisabledAt)
					}
				case changed:
					t.Errorf("on %s: %s -- %s\n\ndisabled_at was %v before the sweep and %v after; "+
						"this row must be left exactly as it was found (the sweep ran on this "+
						"dialect: %t)",
						be.Name, k.name, k.why, was.DisabledAt, got.DisabledAt, sweepRan)
				}
			}
		})
	}
}

// TestSweepExpiredOAuthKeysWithNoHostIsASilentNoOp pins the nil arm, which
// nothing else in this file reaches because every other case wires a host.
//
// A worker whose Environment carries no key store -- cleattest, the embedded
// runner, or the worker after someone unwires the field -- reaches this arm on
// every tick. Without the guard it is a call through a nil func: a panic in the
// background goroutine, which takes the oauth_sessions sweep down with it and
// reports it as neither an error nor a log. Asserted by running the arm rather
// than by reading the guard, since a guard is exactly the thing a reader
// believes without checking.
func TestSweepExpiredOAuthKeysWithNoHostIsASilentNoOp(t *testing.T) {
	var logged bytes.Buffer
	p := &Plugin{
		dialect: plugin.DialectPostgres,
		logger:  slog.New(slog.NewTextHandler(&logged, nil)),
	}

	p.sweepExpiredOAuthKeys(context.Background())

	if logged.Len() != 0 {
		t.Errorf("an absent host produced a log line, which reads as a failure rather than as an "+
			"arm that is absent by design:\n%s", logged.String())
	}
}

// testLogWriter forwards the plugin's own slog output into the test log, so a
// sweep that fails is visible in the failure it causes.
type testLogWriter struct{ t *testing.T }

func (w *testLogWriter) Write(b []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(b), "\n"))
	return len(b), nil
}

// keyState is the row as it stands after the sweep. The two columns the
// sweep does not write are read back too, and deliberately: when an assertion
// about disabled_at fails, the first question is whether the ROW was the one
// the test thinks it seeded, and a failure message that cannot answer it
// sends the reader to the seed path to find out by hand.
//
// EVERY TIMESTAMP IS READ AS A STRING, and that is a portability decision
// rather than a preference: MySQL hands a DATETIME back as []uint8 unless the
// DSN sets parseTime, and this test does not control the DSN -- it comes from
// CLEAT_TEST_MYSQL. Scanning into a time would make the test pass or fail on
// which DSN the machine happened to export, which is the "a green that
// measured nothing" family reached through the driver.
//
// It costs nothing in strictness, because nothing here needs to know what the
// instant WAS. "The sweep disabled this row" is a comparison against the same
// row read before the sweep, and equality between two renderings of one
// column on one dialect is exactly the question -- and a stronger one than
// comparing against a timestamp the test re-typed.
type keyState struct {
	DisabledAt      sql.NullString
	ExpiresAtString sql.NullString
	OAuthIdentity   sql.NullString
}

func readKeyState(t *testing.T, ctx context.Context, conn *sql.Conn, dialect plugin.Dialect, id uuid.UUID) keyState {
	t.Helper()
	var s keyState
	stmt := fmt.Sprintf(
		`SELECT disabled_at, expires_at, oauth_identity FROM %s WHERE key_id = $1`, apiKeysTable(dialect))
	if err := plugintest.QueryRowRebound(t, ctx, conn, dialect, stmt, id).Scan(
		&s.DisabledAt, &s.ExpiresAtString, &s.OAuthIdentity); err != nil {
		t.Fatalf("read back key %s: %v", id, err)
	}
	return s
}
