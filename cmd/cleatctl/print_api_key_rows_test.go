package main

import (
	"bytes"
	"database/sql"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestPrintAPIKeyRowsShowsExpiryAndTriState is cleat#2352's CLI half, and the
// reason it exists is that printAPIKeyRows is a pure function of its rows: the
// fake-driver suite elsewhere in this package cannot reach it, and a revert to
// the pre-expiry shape -- always "active", no EXPIRES column -- still compiles
// and still prints a plausible table, so nothing would catch it. A DB-less
// test over captured stdout is the only thing that fails when the tri-state
// regresses, in the same style as TestResolveAPIKeyStmt_ExcludesExpired.
//
// The first three fixtures are the three states design v2 §(8) asked the CLI
// to distinguish: a live key (neither disabled nor expired), a revoked key
// (disabled_at set -- an operator or the OAuth sweep acted), and an expired
// key (expires_at passed with no disabled_at -- nobody acted, the key's own
// lifetime ran out). The fourth is a key whose expiry is still in the future:
// it must read "active", and it is the one fixture that separates the correct
// predicate expires_at < now from a buggy "any non-NULL expires_at is expired"
// -- the first three cannot, because their expires_at is either NULL or
// already in the past.
func TestPrintAPIKeyRowsShowsExpiryAndTriState(t *testing.T) {
	now := time.Now()
	rows := []apiKeyRow{
		{keyID: uuid.MustParse("10000000-0000-0000-0000-000000000001"), tenantID: uuid.Nil, description: "always on"},
		{keyID: uuid.MustParse("10000000-0000-0000-0000-000000000002"), tenantID: uuid.Nil, description: "killed",
			disabledAt: sql.NullTime{Time: now.Add(-24 * time.Hour), Valid: true}},
		{keyID: uuid.MustParse("10000000-0000-0000-0000-000000000003"), tenantID: uuid.Nil, description: "ran out",
			expiresAt: sql.NullTime{Time: now.Add(-time.Hour), Valid: true}},
		{keyID: uuid.MustParse("10000000-0000-0000-0000-000000000004"), tenantID: uuid.Nil, description: "not yet",
			expiresAt: sql.NullTime{Time: now.Add(24 * time.Hour), Valid: true}},
	}

	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	printAPIKeyRows(rows)
	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatal(err)
	}
	r.Close()
	out := buf.String()

	if !strings.Contains(out, "EXPIRES") {
		t.Errorf("header missing the EXPIRES column:\n%s", out)
	}
	// The tri-state is the point: a regression to always-"active" must fail
	// here, on both non-active arms.
	if !strings.Contains(out, "revoked ") {
		t.Errorf("disabled key not reported revoked:\n%s", out)
	}
	if !strings.Contains(out, "expired ") {
		t.Errorf("past-expiry key not reported expired:\n%s", out)
	}
	// "active" must appear exactly twice: the always-on key AND the not-yet
	// key. One occurrence is the regression the fourth fixture exists to catch
	// -- a buggy "any non-NULL expires_at is expired" reads the future-expiry
	// key as expired and leaves only the always-on key active.
	if got := strings.Count(out, "active"); got != 2 {
		t.Errorf("active count = %d, want 2 (always-on and not-yet-expired):\n%s", got, out)
	}
}
