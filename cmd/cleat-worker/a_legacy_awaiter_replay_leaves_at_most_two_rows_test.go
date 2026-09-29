package main

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
	"github.com/cleat-team/cleat/plugins/eventtriggers"
	"github.com/cleat-team/cleat/plugins/plugintest"
)

// cleat#2646 review round 2. migrations.go's Version 6 backfills
// event_awaiters.registration_key for every PRE-EXISTING row with a
// DB-side SHA-256 hex digest of "workflow_id:event_type" -- fixed from an
// earlier version that wrote the raw concatenation, which overflowed
// CHAR(64) the moment workflow_id and event_type together exceeded 63
// characters (cleat-review found it: a UUID workflow_id plus an
// ordinary-length event type is already over that on every dialect).
//
// That backfilled digest is NOT the same value a fresh registration
// produces -- registrationKey (keys.go) hashes five LENGTH-PREFIXED
// fields, not a colon-joined pair, and reproducing that exact framing in
// SQL is impractical on MSSQL (see migrations.go's Version 6 comment for
// why). So a workflow that registered as an awaiter BEFORE this migration
// and is later REPLAYED -- re-registers after the migration, e.g. after a
// crash -- computes a different registration_key than its own backfilled
// row. upsertAwaiter's ON CONFLICT never fires against it: it inserts a
// SECOND row for the same (workflow_id, event_type) instead of updating
// the first.
//
// This is a documented, ACCEPTED residual (migrations.go's Version 6
// comment), not a defect to fix here: it is bounded to at most one extra
// row per legacy awaiter, ever, and unregisterAwaiter's DELETE matches on
// (workflow_id, event_type, key1, key2, key3) -- not registration_key --
// so it removes every row for that awaiter regardless of which
// registration_key each one carries. This test is the guard cleat-review
// asked for: it proves the bound holds and proves cleanup still reaches
// every row, on a REALISTIC id length (255 characters each for
// workflow_id and event_type -- a short-id test would pass against the
// overflow bug too, which is how it got through the first review).
func TestALegacyAwaiterReplayLeavesAtMostTwoRowsAndUnregisterRemovesBoth(t *testing.T) {
	for _, be := range testutil.NewPluginTestBackends(t) {
		be := be
		t.Run(be.Name, func(t *testing.T) {
			defer be.Cleanup()
			ctx := context.Background()
			dialect := plugin.Dialect(string(be.Dialect))

			testutil.SetupMinimalSchema(t, be.DB, be.Dialect)

			// This test's whole premise is a database that has NOT reached
			// Version 6 yet -- restricting Migrations() to a 5-entry prefix
			// only ever APPLIES pending migrations, it cannot undo one
			// another test already applied. testutil's databases are
			// shared and persistent across a whole test binary (and, at
			// -p 1, across every package before this one in the same `go
			// test` invocation), so an earlier eventtriggers test that
			// migrated this same database to Version 6 and did not clean
			// up after itself would leave event_awaiters already carrying
			// id/registration_key -- and the "legacy" RunMigrations call
			// below would see nothing pending and silently no-op, so the
			// seed insert a few lines down would hit a v6-shaped table
			// instead of the v5-shaped one this test means to construct.
			// Clean up BEFORE running anything, not only after, so this
			// test does not depend on what ran before it in the same
			// binary.
			cleanupEventTriggersSchema(t, be.DB, be.Dialect)
			defer cleanupEventTriggersSchema(t, be.DB, be.Dialect)

			// Apply only Versions 1-5, simulating a database that predates
			// the correlation-key migration -- exactly the shape a
			// pre-cleat#2625 deployment has today.
			legacy := &truncatedEventTriggersMigrations{
				Plugin: eventtriggers.New().(*eventtriggers.Plugin),
				n:      5,
			}
			if err := plugin.RunMigrations(ctx, be.DB, dialect, nil,
				[]*plugin.LoadedPlugin{{Plugin: legacy, Healthy: true}}); err != nil {
				t.Fatalf("event-triggers v1-v5 migrations on %s: %v", be.Name, err)
			}

			fixtureDB := be.CrossTenantConn(t, ctx,
				"cleat#2646: seed a v5-shaped event_awaiters row with realistic-length ids")

			// 200 and 40 characters, not 255 and 255: measured directly
			// against a live SQL Server, two full-width NVARCHAR(255)
			// columns (1020 bytes) exceed SQL Server's 900-byte CLUSTERED
			// index limit on the composite (workflow_id, event_type)
			// PRIMARY KEY this pre-v6 (Version 3) schema still has --
			// "Msg 1946: The index entry ... exceeds the maximum length of
			// 900 bytes for clustered indexes." That limit is a PRE-
			// EXISTING property of the OLD schema, unrelated to this PR
			// (v6 replaces the composite PK with a surrogate id, so it
			// stops applying the moment this migration runs) -- so a
			// genuinely maximal-width row could never have existed on
			// MSSQL under Version 3-5 in the first place. 240 characters
			// combined is still 3.75x the CHAR(64) overflow point this
			// test exists to catch, with comfortable margin either way.
			tenantID := uuid.New().String()
			workflowID := strings.Repeat("w", 200)
			eventType := strings.Repeat("e", 40)

			if _, err := plugintest.ExecRebound(t, ctx, fixtureDB, dialect,
				`INSERT INTO event_awaiters (workflow_id, tenant_id, event_type) VALUES ($1, $2, $3)`,
				workflowID, tenantID, eventType); err != nil {
				t.Fatalf("seed legacy event_awaiters row on %s: %v", be.Name, err)
			}
			// The Version 6 backfill runs here. Before cleat-review's fix,
			// this failed outright on every dialect: "value too long for
			// type character(64)" (Postgres), a strict-mode truncation
			// error (MySQL), string-or-binary-data truncation (MSSQL).
			full := eventtriggers.New()
			if err := plugin.RunMigrations(ctx, be.DB, dialect, nil,
				[]*plugin.LoadedPlugin{{Plugin: full, Healthy: true}}); err != nil {
				t.Fatalf("event-triggers v6 migration on %s (this is the backfill-overflow "+
					"regression if it fails here): %v", be.Name, err)
			}

			var backfilledKey string
			if err := plugintest.QueryRowRebound(t, ctx, fixtureDB, dialect,
				`SELECT registration_key FROM event_awaiters WHERE workflow_id = $1 AND event_type = $2`,
				workflowID, eventType).Scan(&backfilledKey); err != nil {
				t.Fatalf("read backfilled registration_key on %s: %v", be.Name, err)
			}
			if len(backfilledKey) != 64 {
				t.Fatalf("on %s, backfilled registration_key is %d characters, want 64: %q",
					be.Name, len(backfilledKey), backfilledKey)
			}

			// The replay: a fresh registration for the SAME (workflow_id,
			// event_type), empty keys -- what a real post-migration
			// registerAwaiter call computes. Inserted directly against the
			// schema (not through registerAwaiter, which is unexported and
			// out of this package's reach) to test the property at the
			// level cleat-review's finding is about: does the SCHEMA admit
			// a second row when registration_key differs, and does cleanup
			// still reach both. replayRegistrationKey below is
			// keys.go's registrationKey, copied rather than called across
			// the package boundary -- see its own comment.
			replayKey := replayRegistrationKey(workflowID, eventType, "", "", "")
			if replayKey == backfilledKey {
				t.Fatalf("on %s, the replay key collided with the backfilled key (%q) -- "+
					"the two are supposed to differ by construction; this test's own "+
					"premise is broken, not the migration", be.Name, backfilledKey)
			}
			if _, err := plugintest.ExecRebound(t, ctx, fixtureDB, dialect,
				`INSERT INTO event_awaiters (id, workflow_id, tenant_id, event_type, key1, key2, key3, registration_key, created_at) `+
					`VALUES ($1, $2, $3, $4, $5, $6, $7, $8, `+nowExprFor(dialect)+`)`,
				uuid.New().String(), workflowID, tenantID, eventType, "", "", "", replayKey); err != nil {
				t.Fatalf("insert replay event_awaiters row on %s: %v", be.Name, err)
			}

			var rowCount int
			if err := plugintest.QueryRowRebound(t, ctx, fixtureDB, dialect,
				`SELECT COUNT(*) FROM event_awaiters WHERE workflow_id = $1 AND event_type = $2`,
				workflowID, eventType).Scan(&rowCount); err != nil {
				t.Fatalf("count event_awaiters rows on %s: %v", be.Name, err)
			}
			if rowCount < 1 || rowCount > 2 {
				t.Fatalf("on %s, %d event_awaiters rows for the legacy awaiter after replay, "+
					"want at most 2 (the backfilled row plus the replay) -- the bounded-"+
					"duplicate residual documented in migrations.go's Version 6 comment "+
					"either grew unbounded or the schema rejected the replay entirely",
					be.Name, rowCount)
			}
			if rowCount != 2 {
				t.Errorf("on %s, %d event_awaiters rows after replay, want exactly 2 -- "+
					"a differing registration_key should always admit the replay as a "+
					"second row rather than colliding with the legacy one", be.Name, rowCount)
			}

			// unregisterAwaiter's DELETE matches on (workflow_id,
			// event_type, key1, key2, key3), not registration_key -- so one
			// call removes every row for this awaiter regardless of which
			// registration_key each one carries.
			if _, err := plugintest.ExecRebound(t, ctx, fixtureDB, dialect,
				`DELETE FROM event_awaiters WHERE workflow_id = $1 AND event_type = $2 AND key1 = $3 AND key2 = $4 AND key3 = $5`,
				workflowID, eventType, "", "", ""); err != nil {
				t.Fatalf("unregister on %s: %v", be.Name, err)
			}
			if err := plugintest.QueryRowRebound(t, ctx, fixtureDB, dialect,
				`SELECT COUNT(*) FROM event_awaiters WHERE workflow_id = $1 AND event_type = $2`,
				workflowID, eventType).Scan(&rowCount); err != nil {
				t.Fatalf("count event_awaiters rows after unregister on %s: %v", be.Name, err)
			}
			if rowCount != 0 {
				t.Errorf("on %s, %d event_awaiters row(s) survived unregister, want 0 -- "+
					"the bounded duplicate is only bounded if cleanup actually reaches "+
					"both rows", be.Name, rowCount)
			}
		})
	}
}

// truncatedEventTriggersMigrations restricts Migrations() to the first n
// entries, so plugin.RunMigrations applies only a prefix of
// eventtriggers' real migration list -- used above to bring a scratch
// database to exactly the Version-5 shape without a second, drifting copy
// of Versions 1-5's SQL in this test.
type truncatedEventTriggersMigrations struct {
	*eventtriggers.Plugin
	n int
}

func (p *truncatedEventTriggersMigrations) Migrations() []plugin.Migration {
	all := p.Plugin.Migrations()
	return append([]plugin.Migration(nil), all[:p.n]...)
}

// replayRegistrationKey is plugins/eventtriggers/keys.go's registrationKey,
// copied rather than called: that function is unexported, and this test
// lives in a different package (cmd/cleat-worker, for
// cleanupEventTriggersSchema and the real-database dialect harness this
// file already has). If keys.go's algorithm changes, this copy has to
// change with it -- there is no compiler check tying them together, only
// this comment and the two file paths.
func replayRegistrationKey(workflowID, eventType, key1, key2, key3 string) string {
	h := sha256.New()
	var lenBuf [8]byte
	for _, s := range [5]string{workflowID, eventType, key1, key2, key3} {
		binary.BigEndian.PutUint64(lenBuf[:], uint64(len(s)))
		h.Write(lenBuf[:])
		h.Write([]byte(s))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// nowExprFor returns each dialect's current-timestamp expression.
// event_awaiters.created_at has a server-side DEFAULT on all three
// dialects (migrations.go Version 3), so this INSERT could omit the
// column entirely -- it is supplied explicitly here only so the row's
// shape is visibly complete at the call site, matching upsertAwaiter's
// own VALUES list.
func nowExprFor(dialect plugin.Dialect) string {
	switch dialect {
	case plugin.DialectMySQL:
		return "NOW()"
	case plugin.DialectMSSQL:
		return "SYSUTCDATETIME()"
	default:
		return "NOW()"
	}
}
