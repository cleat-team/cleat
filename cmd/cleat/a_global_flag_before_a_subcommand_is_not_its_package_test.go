package main

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// cleat#3519. `build` and `vet` handed their flag set os.Args[2:] after main had
// already consumed the global flags, so in `cleat --tenant X vet .` the flag set
// saw `X vet .` and took X as the package. Every other subcommand reads
// flag.Args(), which is why `cleat --db <dsn> plugin install` always worked.
//
// The refusal in refuseTrailingFlags made the cases that also carried a flag of
// their own loud. The case it cannot see is a global flag with NO other flag:
// `cleat --tenant X build .` has nothing after the "package", so X was built (or
// vetted) as the package without a word. That case is in the table on purpose.
func TestAGlobalFlagBeforeASubcommandIsNotItsPackage(t *testing.T) {
	needBinary(t)
	dir := buildFixture(t)

	const tenant = "00000000-0000-0000-0000-000000000001"
	// An unreachable DSN is fine: the fixture starts no child, so the build never
	// opens it. What is under test is which argument is the package.
	const dsn = "postgres://nobody@127.0.0.1:1/none?sslmode=disable"

	cases := []struct {
		name   string
		args   []string
		writes bool // the command is expected to leave a .wasm under OUT
	}{
		{"control: no global flag, build", []string{"build", "-o", "OUT", "."}, true},
		{"control: no global flag, vet", []string{"vet", "."}, false},
		{"--tenant before build with a flag of its own", []string{"--tenant", tenant, "build", "-o", "OUT", "."}, true},
		{"--db before build with a flag of its own", []string{"--db", dsn, "build", "--channel", "latest", "-o", "OUT", "."}, true},
		{"--tenant before build, nothing else", []string{"--tenant", tenant, "build", "."}, false},
		{"--tenant before vet, nothing else", []string{"--tenant", tenant, "vet", "."}, false},
		{"--db before vet with a flag of its own", []string{"--db", dsn, "vet", "--json", "."}, false},
		{"both globals before build", []string{"--db", dsn, "--tenant", tenant, "build", "-o", "OUT", "."}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "out")
			args := make([]string, len(c.args))
			for i, a := range c.args {
				args[i] = strings.ReplaceAll(a, "OUT", out)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			cmd := exec.CommandContext(ctx, cleatBinary, args...)
			cmd.Dir = dir
			b, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("cleat %s failed:\n%s", strings.Join(c.args, " "), b)
			}
			// A build that took the wrong argument for its package would not have
			// built this one.
			if c.writes {
				wasms, _ := filepath.Glob(filepath.Join(out, "*.wasm"))
				if len(wasms) != 1 {
					t.Errorf("want one .wasm under %s, got %v:\n%s", out, wasms, b)
				}
			}
		})
	}
}
