package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"
)

// Go's flag package stops parsing at the first non-flag argument. set-secret and
// suspend-tenant both call fs.Parse(args) with args[0] holding the tenant UUID, so
// every flag written after it was silently dropped -- and the commands then blamed
// the operator for omitting a flag that was passed (cleat#1933).
//
// The order these tests put FIRST is the broken one, because it is the order the
// usage text and docs/how-to/use-secrets.md print. The reversed order is the
// control: it worked before the fix and must keep working after it.
//
// Every case here asserts the two orders produce the SAME result rather than
// asserting a particular message. A test pinned to one message passes if the
// command starts failing both orders identically, which is the regression most
// likely to be introduced by a rewrite of the parsing.

const testTenantUUID = "5911bb8d-1c2f-4f5a-9b0e-2b3f4c5d6e7f"

// failingConnector yields a DB whose every query fails, which gives the
// suspend-tenant cases a determinate stopping point past argument parsing
// without needing a live database.
type failingConnector struct{}

func (failingConnector) Connect(context.Context) (driver.Conn, error) {
	return nil, errors.New("mock db error")
}
func (failingConnector) Driver() driver.Driver { return failingDriver{} }

type failingDriver struct{}

func (failingDriver) Open(string) (driver.Conn, error) { return nil, errors.New("mock db error") }

func TestSetSecret_AcceptsFlagsAfterTheTenantArgument(t *testing.T) {
	t.Setenv("CLEAT_SECRET_MASTER_KEY", "")

	documented := withExitPanic(t, func() {
		runSetSecret(context.Background(), nil, dialect{}, []string{testTenantUUID, "--name", "openai"})
	})
	working := withExitPanic(t, func() {
		runSetSecret(context.Background(), nil, dialect{}, []string{"--name", "openai", testTenantUUID})
	})

	// The specific symptom this issue was filed for: --name was passed and the
	// command reported it missing by printing its own usage.
	if strings.Contains(documented, "Usage: cleatctl --db <dsn> set-secret") {
		t.Errorf("set-secret rejected the documented argument order as if --name were missing:\n%s", documented)
	}
	if documented != working {
		t.Errorf("the two argument orders behave differently.\n--- <uuid> --name openai ---\n%s\n--- --name openai <uuid> ---\n%s",
			documented, working)
	}
}

func TestSetSecret_AcceptsFromFileAfterTheTenantArgument(t *testing.T) {
	t.Setenv("CLEAT_SECRET_MASTER_KEY", "")
	path := t.TempDir() + "/value"

	documented := withExitPanic(t, func() {
		runSetSecret(context.Background(), nil, dialect{},
			[]string{testTenantUUID, "--name", "openai", "--from-file", path})
	})
	working := withExitPanic(t, func() {
		runSetSecret(context.Background(), nil, dialect{},
			[]string{"--name", "openai", "--from-file", path, testTenantUUID})
	})

	if strings.Contains(documented, "Usage: cleatctl --db <dsn> set-secret") {
		t.Errorf("set-secret rejected --from-file written after the tenant:\n%s", documented)
	}
	if documented != working {
		t.Errorf("the two argument orders behave differently.\n--- documented ---\n%s\n--- working ---\n%s",
			documented, working)
	}
}

func TestSuspendTenant_AcceptsYesAfterTheTenantArgument(t *testing.T) {
	db := sql.OpenDB(failingConnector{})
	defer db.Close()

	documented := withExitPanic(t, func() {
		runSuspendTenant(context.Background(), db, dialect{}, []string{testTenantUUID, "--yes"}, true)
	})
	working := withExitPanic(t, func() {
		runSuspendTenant(context.Background(), db, dialect{}, []string{"--yes", testTenantUUID}, true)
	})

	// This is the nastier half of cleat#1933: `suspend-tenant <uuid>` works, so
	// adding the documented --yes turned a working command into a failing one.
	if strings.Contains(documented, "exactly one tenant id is required") {
		t.Errorf("suspend-tenant read `<uuid> --yes` as two tenant ids:\n%s", documented)
	}
	if documented != working {
		t.Errorf("the two argument orders behave differently.\n--- <uuid> --yes ---\n%s\n--- --yes <uuid> ---\n%s",
			documented, working)
	}
}

// ---------------------------------------------------------------------------
// The limits of the fix, asserted rather than assumed.
//
// Accepting flags after the operand must not turn into accepting anything, so
// each of these pins a rejection the commands must keep making. The first four
// passed BEFORE the fix as well, which is what makes them controls: a parser
// rewrite cannot buy the cases above by dropping the checks that make the
// commands safe.
//
// The last two did NOT pass before the fix, so they are a second symptom rather
// than controls, and they are left here because they belong with the rejections.
// An unknown flag written after the tenant used to be swallowed as a positional
// and ignored entirely. Measured on origin/develop:
//
//	set-secret --name openai <uuid> --nonesuch
//	  -> stock: "error: CLEAT_SECRET_MASTER_KEY is not set"
//	  -> fixed: "flag provided but not defined: -nonesuch"
//
// The stock run reached the master-key check, which is to say that with a key
// set it would have written the secret and never mentioned the flag the
// operator got wrong.
// ---------------------------------------------------------------------------

func TestSetSecret_StillRejectsAMissingTenant(t *testing.T) {
	t.Setenv("CLEAT_SECRET_MASTER_KEY", "")
	stderr := withExitPanic(t, func() {
		runSetSecret(context.Background(), nil, dialect{}, []string{"--name", "openai"})
	})
	if !strings.Contains(stderr, "Usage: cleatctl --db <dsn> set-secret") {
		t.Errorf("set-secret accepted an invocation with no tenant at all:\n%s", stderr)
	}
}

func TestSetSecret_StillRejectsAMissingName(t *testing.T) {
	t.Setenv("CLEAT_SECRET_MASTER_KEY", "")
	stderr := withExitPanic(t, func() {
		runSetSecret(context.Background(), nil, dialect{}, []string{testTenantUUID})
	})
	if !strings.Contains(stderr, "Usage: cleatctl --db <dsn> set-secret") {
		t.Errorf("set-secret accepted an invocation with no --name:\n%s", stderr)
	}
}

func TestSuspendTenant_StillRejectsAMissingTenant(t *testing.T) {
	db := sql.OpenDB(failingConnector{})
	defer db.Close()
	stderr := withExitPanic(t, func() {
		runSuspendTenant(context.Background(), db, dialect{}, []string{"--yes"}, true)
	})
	if !strings.Contains(stderr, "exactly one tenant id is required") {
		t.Errorf("suspend-tenant accepted an invocation with no tenant:\n%s", stderr)
	}
}

func TestSuspendTenant_StillRejectsTwoTenants(t *testing.T) {
	db := sql.OpenDB(failingConnector{})
	defer db.Close()
	stderr := withExitPanic(t, func() {
		runSuspendTenant(context.Background(), db, dialect{},
			[]string{testTenantUUID, "--yes", "11111111-2222-3333-4444-555555555555"}, true)
	})
	if !strings.Contains(stderr, "exactly one tenant id is required") {
		t.Errorf("suspend-tenant accepted two tenant ids:\n%s", stderr)
	}
}

func TestSetSecret_StillRejectsAnUnknownFlagAfterAValidInvocation(t *testing.T) {
	// The flags-FIRST order, which parsed correctly before the fix and so ran
	// the command for real. The unknown flag landed past the point where Parse
	// stopped and was never looked at.
	t.Setenv("CLEAT_SECRET_MASTER_KEY", "")
	stderr := withExitPanic(t, func() {
		runSetSecret(context.Background(), nil, dialect{},
			[]string{"--name", "openai", testTenantUUID, "--nonesuch"})
	})
	if !strings.Contains(stderr, "nonesuch") {
		t.Errorf("an unknown flag after a valid invocation was ignored; the command "+
			"proceeded as though the operator had typed nothing wrong:\n%s", stderr)
	}
}

func TestSetSecret_StillRejectsAnUnknownFlagAfterTheTenant(t *testing.T) {
	t.Setenv("CLEAT_SECRET_MASTER_KEY", "")
	stderr := withExitPanic(t, func() {
		runSetSecret(context.Background(), nil, dialect{},
			[]string{testTenantUUID, "--name", "openai", "--nonesuch"})
	})
	// An unknown flag written after the operand must be reported, not swallowed
	// as though it were a second positional argument.
	if !strings.Contains(stderr, "nonesuch") {
		t.Errorf("an unknown flag after the tenant was accepted silently:\n%s", stderr)
	}
}
