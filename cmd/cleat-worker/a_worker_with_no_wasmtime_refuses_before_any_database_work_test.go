package main

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// cleat#3787: a worker built without cgo -- which is what `go install` gives a
// machine with no C compiler, exit status 0 and no warning -- has to refuse
// BEFORE it touches a database.
//
// The backend used to be constructed after --migrate-only and
// --generate-api-key had both run, so quick-start steps 3 and 8 succeeded and
// the failure arrived at step 7, naming a variable the reader never set. This
// builds the binary the way that machine does (CGO_ENABLED=0 is forced, so the
// result is the same whatever toolchain runs the test) and drives it as the
// quick-start does.
//
// The database address is one nothing listens on. A worker that reached the
// database would say "refusing to start (database)"; the assertion is that the
// refusal is about the backend, which is only true if it came first.
func TestAWorkerWithNoWasmtimeRefusesBeforeAnyDatabaseWork(t *testing.T) {
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "cleat-worker-nocgo")
	build := exec.Command("go", "build", "-o", bin, "./cmd/cleat-worker")
	build.Dir = repoRoot
	build.Env = append(build.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("CGO_ENABLED=0 go build ./cmd/cleat-worker: %v\n%s", err, out)
	}

	run := func(args ...string) (int, string) {
		cmd := exec.Command(bin, args...)
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		err := cmd.Run()
		if err == nil {
			return 0, out.String()
		}
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode(), out.String()
		}
		t.Fatalf("running %v: %v", args, err)
		return -1, ""
	}
	const nothingListens = "postgres://nobody@127.0.0.1:1/none?sslmode=disable"

	for _, args := range [][]string{
		{"--migrate-only", "--db", nothingListens},
		{"--generate-api-key", "00000000-0000-0000-0000-000000000000", "--db", nothingListens},
		{"--db", nothingListens, "--api-addr", ":0"},
	} {
		code, out := run(args...)
		if code != 1 {
			t.Errorf("%v: exit %d, want 1.\n%s", args, code, out)
		}
		if !strings.Contains(out, "no wasmtime backend") || !strings.Contains(out, "build-essential") {
			t.Errorf("%v: the refusal does not say there is no wasmtime backend and what to install:\n%s", args, out)
		}
		if strings.Contains(out, "refusing to start (database)") {
			t.Errorf("%v: the worker reached the database before it noticed it has no backend:\n%s", args, out)
		}
	}

	// The two modes that answer a question about the binary must still answer.
	if code, out := run("--list-plugins"); code != 0 {
		t.Errorf("--list-plugins exited %d on a binary with no wasmtime; it needs no backend.\n%s", code, out)
	}
	if code, out := run("--verify-backend"); code != 1 || !strings.Contains(out, "verify-backend: FAIL") {
		t.Errorf("--verify-backend = %d, want 1 and a FAIL line.\n%s", code, out)
	}
}
