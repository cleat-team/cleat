package testutil

import (
	"fmt"
	"strings"
	"testing"
)

// recordingT captures what RefuseIfForeignSessionsAtStart did instead of
// letting it kill the test doing the checking.
type recordingT struct {
	fatal  string
	logged string
}

func (r *recordingT) Helper() {}
func (r *recordingT) Logf(format string, args ...any) {
	r.logged += fmt.Sprintf(format, args...)
}
func (r *recordingT) Fatalf(format string, args ...any) {
	r.fatal += fmt.Sprintf(format, args...)
}

// TestRefuseIfForeignSessionsAtStart covers the gate added for cleat#982, and
// the first case is the one that matters: a gate that can never fire is exactly
// the defect it replaces. The probe this supersedes reported "no other client
// process was attached" eleven times against a KNOWN interloper, because its two
// sample instants straddled the offender's whole lifetime. A refusal nobody has
// watched fire is the same bug wearing a louder coat.
func TestRefuseIfForeignSessionsAtStart(t *testing.T) {
	const dialect = DialectPostgres

	t.Run("refuses when another client was attached at start", func(t *testing.T) {
		atStartForeign.Store(dialect, []string{"cleat-test-4242 (pid 4242)"})
		t.Cleanup(func() { atStartForeign.Delete(dialect) })
		t.Setenv(AllowForeignSessionsEnv, "")

		var rec recordingT
		RefuseIfForeignSessionsAtStart(&rec, dialect)

		if rec.fatal == "" {
			t.Fatal("the gate did not refuse with a foreign session recorded at start, " +
				"so it cannot protect anything")
		}
		// The message has a job beyond failing: a refusal that does not name
		// the sessions cannot be told from a crashed run's leaked pool, and one
		// that does not name the override gets worked around by deleting it.
		for _, want := range []string{"cleat-test-4242", AllowForeignSessionsEnv} {
			if !strings.Contains(rec.fatal, want) {
				t.Errorf("the refusal message does not mention %q:\n%s", want, rec.fatal)
			}
		}
	})

	t.Run("stays quiet when nobody else was attached", func(t *testing.T) {
		atStartForeign.Delete(dialect)
		var rec recordingT
		RefuseIfForeignSessionsAtStart(&rec, dialect)
		if rec.fatal != "" {
			t.Errorf("the gate refused a run with no foreign sessions: %s", rec.fatal)
		}
	})

	t.Run("an empty slice is not a foreign session", func(t *testing.T) {
		atStartForeign.Store(dialect, []string{})
		t.Cleanup(func() { atStartForeign.Delete(dialect) })
		var rec recordingT
		RefuseIfForeignSessionsAtStart(&rec, dialect)
		if rec.fatal != "" {
			t.Errorf("the gate refused on an empty session list: %s", rec.fatal)
		}
	})

	t.Run("the override proceeds and still says what it accepted", func(t *testing.T) {
		atStartForeign.Store(dialect, []string{"cleat-test-4242 (pid 4242)"})
		t.Cleanup(func() { atStartForeign.Delete(dialect) })
		t.Setenv(AllowForeignSessionsEnv, "1")

		var rec recordingT
		RefuseIfForeignSessionsAtStart(&rec, dialect)

		if rec.fatal != "" {
			t.Errorf("the override did not suppress the refusal: %s", rec.fatal)
		}
		if !strings.Contains(rec.logged, "cleat-test-4242") {
			t.Errorf("the override path did not name the sessions it accepted: %q", rec.logged)
		}
	})
}

// TestRefuseGateFiresAgainstARealForeignConnection is the end-to-end half.
//
// The cases above seed atStartForeign directly, so they prove the gate DECIDES
// correctly and prove nothing about whether the decision is ever reached. That
// distinction is the entire subject of cleat#982: the probe this supersedes
// decided correctly every time and was never in a position to decide anything
// useful. So this drives the real path -- a real second connection, the real
// sampler, the real gate -- and asserts the refusal actually arrives.
func TestRefuseGateFiresAgainstARealForeignConnection(t *testing.T) {
	const dialect = DialectPostgres
	TestDB(t, dialect) // applies the schema and the PostgreSQL DSN tag

	// Stage a connection belonging to "another process". selfPID is a variable
	// precisely so a test can do this without a second `go test`.
	stranger := openSecondConnection(t, dialect, "cleat-test-999999")
	defer stranger.Close()

	foreign, basis, ok := ForeignSessions(dialect)
	if !ok {
		t.Fatalf("the probe could not answer at all: %s -- the gate below is untestable", basis)
	}
	if len(foreign) == 0 {
		t.Fatalf("staged a foreign connection and the probe reported none (basis: %s).\n\n"+
			"Without this the gate cannot fire, and a gate that cannot fire is the "+
			"defect cleat#982 is about.", basis)
	}

	// Feed the real observation to the real gate.
	atStartForeign.Store(dialect, foreign)
	t.Cleanup(func() { atStartForeign.Delete(dialect) })
	t.Setenv(AllowForeignSessionsEnv, "")

	var rec recordingT
	RefuseIfForeignSessionsAtStart(&rec, dialect)
	if rec.fatal == "" {
		t.Fatal("a real foreign connection was attached and the gate did not refuse")
	}
	if !strings.Contains(rec.fatal, "cleat-test-999999") {
		t.Errorf("the refusal did not name the staged session:\n%s", rec.fatal)
	}
	t.Logf("gate fired against a real foreign connection. basis: %s", basis)
}
