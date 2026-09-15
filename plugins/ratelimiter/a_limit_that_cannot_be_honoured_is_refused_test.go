package ratelimiter

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/plugin"
)

// cleat#1581: a deployment that asks for cluster-wide rate limiting and cannot
// have it must be refused, not quietly given the per-process limiter.
//
// WHY THE OBVIOUS TEST DOES NOT WORK, and it is the reason this file exists
// rather than one more case in ratelimiter_test.go. Asserting "p.mode is db
// after configuring db" passes against the broken build too -- the downgrade
// happens one line after the assignment, so the test and the defect agree. The
// only assertion that separates them is that Init returns an ERROR rather than
// a usable plugin.
//
// FOUR ARMS, AND EACH KILLS A DIFFERENT WRONG FIX:
//
//	db, no database     -> error      the defect itself
//	db, with a database -> no error   catches a fix that refuses unconditionally
//	unknown mode        -> error      the same defect through a different door
//	no config at all    -> no error   the compatibility guarantee
//
// The second arm is the one people leave out. A fix that returns an error from
// every "db" request satisfies the first arm perfectly and breaks every
// correctly-configured cluster deployment, which is worse than the bug.
//
// The third arm is not hypothetical padding. middleware.go asks
// `p.mode == "db"` and treats everything else as memory, so "DB", "database"
// and "postgres" all used to select per-process limiting -- the outcome the
// operator set the field to avoid. It is the likelier mistake in practice: a
// deployment that configures db mode usually HAS a database, and a typo has
// nothing to fail against.
func TestALimitThatCannotBeHonouredIsRefused(t *testing.T) {
	newDB := func(t *testing.T) plugin.PluginDB {
		t.Helper()
		store := &fakeDBStore{
			apiKeys:    make(map[string]string),
			rateLimits: make(map[string]fakeRateLimitRow),
		}
		fakeDB := sql.OpenDB(&fakeConnector{store: store})
		t.Cleanup(func() { fakeDB.Close() })
		return &engine.SQLDBAdapter{DB: fakeDB}
	}

	for _, tc := range []struct {
		name     string
		config   string
		withDB   bool
		wantErr  bool
		errNames string // a substring the message must carry, so a refusal is diagnosable
		wantMode string // checked only when Init is expected to succeed
	}{
		{
			name:     "db mode with no database is refused",
			config:   `{"mode":"db"}`,
			withDB:   false,
			wantErr:  true,
			errNames: "requires a database",
		},
		{
			name:     "db mode with a database still works",
			config:   `{"mode":"db"}`,
			withDB:   true,
			wantErr:  false,
			wantMode: modeDB,
		},
		{
			name:     "an unrecognised mode is refused",
			config:   `{"mode":"database"}`,
			withDB:   true,
			wantErr:  true,
			errNames: "unknown mode",
		},
		{
			name:     "no config is still memory, and still starts",
			config:   "",
			withDB:   true,
			wantErr:  false,
			wantMode: modeMemory,
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			env := &plugin.Environment{}
			if tc.config != "" {
				env.Config = []byte(tc.config)
			}
			if tc.withDB {
				env.DB = newDB(t)
			}

			p := &Plugin{}
			err := p.Init(context.Background(), env)

			if tc.wantErr {
				if err == nil {
					t.Fatalf("Init returned nil for %s -- the plugin started and every tenant "+
						"now gets a per-process limiter while the configuration says otherwise. "+
						"p.buckets is an in-process map, so N workers serve N times the "+
						"configured rate, which is exactly what setting this field was meant "+
						"to prevent.", tc.config)
				}
				// A refusal nobody can act on is barely better than a downgrade.
				if !strings.Contains(err.Error(), tc.errNames) {
					t.Errorf("error does not say what went wrong.\n  got:  %v\n  want it to contain: %q",
						err, tc.errNames)
				}
				return
			}

			if err != nil {
				t.Fatalf("Init returned %v for %s -- this configuration CAN be honoured, and "+
					"refusing it would break every correctly-configured deployment, which is "+
					"worse than the defect being fixed", err, tc.config)
			}
			if p.mode != tc.wantMode {
				t.Errorf("mode = %q, want %q", p.mode, tc.wantMode)
			}
		})
	}
}
