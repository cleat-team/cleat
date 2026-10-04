package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestAMySQLSchemeDSNIsRefused covers both shapes measured on cleat#2962.
//
// The two refusal shapes are not the same failure and are listed separately on
// purpose. A guard written around the SYMPTOM the issue names -- "the scheme is
// read as the username, so the server refuses user 'mysql'" -- fires only for
// the second one. The first is the commoner shape (a PostgreSQL URL with the
// scheme edited), and it never reaches a server at all: the driver rejects it
// inside sql.Open with a message about a NETWORK.
func TestAMySQLSchemeDSNIsRefused(t *testing.T) {
	refused := []struct {
		name string
		dsn  string
	}{
		// Shape 1: rejected by the driver's parser, with a message naming a
		// network and never the scheme.
		{"plain URL form", "mysql://root:cleat@127.0.0.1:3306/cleat"},
		// Shape 2: parses CLEANLY as User="mysql" and reaches the server, where
		// the refusal names a user nobody configured.
		{"URL form wrapping the driver's own @tcp()", "mysql://cleat:pw@tcp(127.0.0.1:3306)/cleat"},
		{"no userinfo", "mysql://localhost/cleat"},
		{"a schema-less host:port", "mysql://localhost:3306/cleat"},
		// detectDialect trims and lowercases; this guard matches its leniency,
		// or `MYSQL://` would slip past the refusal and reach the driver.
		{"case and surrounding space are not a different scheme", "  MYSQL://cleat:pw@localhost:3306/cleat  "},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			err := rejectMySQLScheme(tc.dsn)
			if err == nil {
				t.Fatalf("rejectMySQLScheme(%q) = nil, want a refusal", tc.dsn)
			}
			// The message must carry the FIX, not only the complaint. A
			// refusal that names the problem without the correct form sends
			// the reader back to the driver's docs, which is where the
			// confusing error already sent them.
			for _, want := range []string{"mysql://", "tcp("} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("rejectMySQLScheme(%q) does not mention %q:\n%s", tc.dsn, want, err)
				}
			}
		})
	}

	// The controls are the load-bearing half. A guard that refused every DSN
	// would satisfy every case above, so the population below is the one it
	// must NOT touch -- including the two schemes that DO work, which is why
	// the three examples in docs/reference/worker-config.md look parallel and
	// are not.
	accepted := []struct {
		name string
		dsn  string
	}{
		{"the driver's own form", "root:cleat@tcp(127.0.0.1:3306)/cleat?parseTime=true"},
		{"postgres URL scheme", "postgres://cleat:pw@localhost:5432/cleat?sslmode=disable"},
		{"postgresql URL scheme", "postgresql://localhost/cleat"},
		{"sqlserver URL scheme", "sqlserver://sa:pw@localhost:1433?database=cleat"},
		{"mssql URL scheme", "mssql://sa:pw@localhost:1433?database=cleat"},
		{"jdbc sqlserver URL", "jdbc:sqlserver://localhost:1433;databaseName=cleat"},
		{"postgres key=value DSN", "host=localhost user=cleat dbname=cleat"},
		{"empty", ""},
	}
	for _, tc := range accepted {
		t.Run("accepts "+tc.name, func(t *testing.T) {
			if err := rejectMySQLScheme(tc.dsn); err != nil {
				t.Errorf("rejectMySQLScheme(%q) = %v, want nil -- this is not a mysql:// DSN", tc.dsn, err)
			}
		})
	}
}

// TestTheRefusalIsReachedFromMain runs the real binary rather than the function,
// because cleat#2962's fix is a function AND a call site, and a unit test above
// proves only the first. A guard wired to nothing would leave that test green.
//
// The second arm is a control. The first asserts a refusal, and a guard that
// refused everything would satisfy it, so the control DSN must reach the driver
// and fail THERE instead -- a different message, from a different layer.
func TestTheRefusalIsReachedFromMain(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if resolved, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = resolved
	}
	root := filepath.Dir(filepath.Dir(cwd)) // cmd/cleatctl -> repo root

	// Built once and run twice: `go run` would compile the package per
	// invocation, and both arms run the same binary anyway.
	bin := filepath.Join(t.TempDir(), "cleatctl")
	build := exec.Command("go", "build", "-o", bin, filepath.Join(root, "cmd", "cleatctl"))
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building cleatctl: %v\n%s", err, out)
	}

	run := func(t *testing.T, dsn string) (int, string) {
		t.Helper()
		cmd := exec.Command(bin, "--db", dsn, "list")
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		if err != nil && cmd.ProcessState == nil {
			t.Fatalf("cleatctl never ran: %v\n%s", err, out)
		}
		return cmd.ProcessState.ExitCode(), string(out)
	}

	t.Run("a mysql:// DSN is refused, and the message names the scheme", func(t *testing.T) {
		code, out := run(t, "mysql://root:cleat@127.0.0.1:3306/cleat")
		if code == 0 {
			t.Fatalf("cleatctl ACCEPTED a mysql:// DSN and exited 0:\n%s", out)
		}
		for _, want := range []string{"mysql://", "tcp("} {
			if !strings.Contains(out, want) {
				t.Errorf("the refusal does not mention %q, so the operator cannot act on it:\n%s", want, out)
			}
		}
		// The pre-fix symptoms, asserted ABSENT. Without this the arm passes on
		// a binary that still handed the DSN to the driver and produced one of
		// these by accident -- which is exactly the state the fix replaces.
		for _, symptom := range []string{"default addr for network", "failed to ping database"} {
			if strings.Contains(out, symptom) {
				t.Errorf("the DSN still reached the driver (%q in the output) -- this is the pre-fix path:\n%s", symptom, out)
			}
		}
	})

	t.Run("control: a non-mysql:// DSN still reaches the driver", func(t *testing.T) {
		// 127.0.0.1:1 is refused instantly, so this needs no server and no
		// wait for a timeout.
		code, out := run(t, "postgres://cleat:pw@127.0.0.1:1/cleat?sslmode=disable")
		if code == 0 {
			t.Fatalf("the control DSN unexpectedly exited 0:\n%s", out)
		}
		if strings.Contains(out, "mysql://") {
			t.Errorf("the guard fired on a PostgreSQL DSN -- it refuses too much:\n%s", out)
		}
		if !strings.Contains(out, "failed to ping database") {
			t.Errorf("the control DSN did not reach the driver, so this arm does not separate "+
				"the guard from a general refusal:\n%s", out)
		}
	})
}
