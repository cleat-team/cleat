package main

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
)

// cleat#3597, after owner decision D55: `cleatctl library publish ... --replicate-to <dsn>` writes a
// bundle to every shard of a sharded deployment, --db being the admin shard.

// libraryTestPassword is the STEM of the password testutil gives its library logins, which is
// "Cleat-library-test-1!". Every output of every run below is searched for it: a command that
// handles one DSN per shard is a command that can print one. It is the stem and not the whole
// password because a URL DSN percent-encodes the trailing "!" (as %21), so a search for the literal
// passes over a DSN that was printed whole. The first version searched for the literal and stayed
// green under a mutant that printed the raw DSN.
const libraryTestPassword = "Cleat-library-test-1"

func TestRedactedTargetNeverShowsWhoOrWithWhat(t *testing.T) {
	for name, tc := range map[string]struct{ dsn, want string }{
		"a PostgreSQL URL":                        {"postgres://admin:s3cret@db1.internal:5432/cleat_prod?sslmode=disable", "db1.internal:5432/cleat_prod"},
		"a SQL Server URL, database in the query": {"sqlserver://sa:s3cret@db2.internal:1433?database=cleat_prod", "db2.internal:1433/cleat_prod"},
		"a host with no database":                 {"postgres://admin:s3cret@db3.internal:5432", "db3.internal:5432"},
		"a key=value DSN is not echoed":           {"host=db4 user=admin password=s3cret dbname=cleat", "a database whose DSN is not a URL (not shown)"},
		"an empty string":                         {"", "a database whose DSN is not a URL (not shown)"},
	} {
		t.Run(name, func(t *testing.T) {
			got := redactedTarget(tc.dsn)
			if got != tc.want {
				t.Errorf("redactedTarget(%q) = %q, want %q", tc.dsn, got, tc.want)
			}
			if strings.Contains(got, "s3cret") || strings.Contains(got, "@") {
				t.Errorf("redactedTarget leaked a credential: %q", got)
			}
		})
	}
}

func TestShardedPublishRefusesBeforeWritingWhenAShardCannotBeUsed(t *testing.T) {
	req := engine.PublishLibraryRequest{Library: "acme", BundleVersion: "1", Members: []engine.PublishLibraryMember{
		{Name: "m", Version: 1, WasmBytes: []byte("x"), ABIVersion: 1, MinVersion: 1},
	}}
	t.Run("a shard of another kind of database", func(t *testing.T) {
		stderr := withExitPanic(t, func() {
			libraryPublishSharded(context.Background(), nil, engine.DialectPostgres, req, []string{"sqlserver://u:p@h:1433?database=d"}, false)
		})
		if !strings.Contains(stderr, "is a mssql database") || !strings.Contains(stderr, "Nothing was written to any shard") {
			t.Errorf("stderr = %q, want the mixed kinds named and nothing written", stderr)
		}
	})
	t.Run("a shard that cannot be reached is found before the first write, and its password is not printed", func(t *testing.T) {
		stderr := withExitPanic(t, func() {
			libraryPublishSharded(context.Background(), nil, engine.DialectPostgres, req,
				[]string{"postgres://u:UNREACHABLE-SECRET@127.0.0.1:1/d?sslmode=disable&connect_timeout=2"}, false)
		})
		if !strings.Contains(stderr, "cannot be reached") || !strings.Contains(stderr, "Nothing was written to any shard") || !strings.Contains(stderr, "shard-2") {
			t.Errorf("stderr = %q, want shard-2 named as unreachable and nothing written", stderr)
		}
		if strings.Contains(stderr, "UNREACHABLE-SECRET") {
			t.Errorf("the shard's password was printed: %q", stderr)
		}
	})
}

// The built binary, three real databases of one dialect, each through its own publisher login. The
// partial failure is a real one: shard 2 is reached through a login that is not a publisher.
func TestTheBuiltCleatctlPublishesToEveryShardAndAReRunCompletesOne(t *testing.T) {
	root := cleatctlRepoRoot(t)
	bin := filepath.Join(t.TempDir(), "cleatctl")
	build := exec.Command("go", "build", "-o", bin, filepath.Join(root, "cmd", "cleatctl"))
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building cleatctl: %v\n%s", err, out)
	}
	for _, tc := range []struct {
		name     string
		tdialect testutil.Dialect
		edialect engine.Dialect
	}{
		{"postgres", testutil.DialectPostgres, engine.DialectPostgres},
		{"mssql", testutil.DialectMSSQL, engine.DialectMSSQL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var owner [3]*sql.DB
			var pubDSN, readerDSN [3]string
			for i, prefix := range []string{"clishard_a", "clishard_b", "clishard_c"} {
				owner[i] = testutil.ScratchDatabase(t, tc.tdialect, prefix)
				_, pubDSN[i] = testutil.LibraryLoginDSN(t, owner[i], tc.tdialect, true)
				_, readerDSN[i] = testutil.LibraryLoginDSN(t, owner[i], tc.tdialect, false)
			}
			member := stampedMember(t, t.TempDir(), "solo", 1, nil, "s")
			rows := func(i int, lib string) int {
				n := 0
				for _, table := range []string{"library.members", "library.bundles", "library.bundle_members"} {
					var c int
					if err := owner[i].QueryRow(`SELECT count(*) FROM `+table+` WHERE library = `+cliPlaceholder(tc.edialect), lib).Scan(&c); err != nil {
						t.Fatalf("count %s on shard %d: %v", table, i+1, err)
					}
					n += c
				}
				return n
			}
			run := func(args ...string) (int, string) {
				cmd := exec.Command(bin, args...)
				cmd.Dir = root
				out, err := cmd.CombinedOutput()
				if err != nil && cmd.ProcessState == nil {
					t.Fatalf("cleatctl never ran: %v\n%s", err, out)
				}
				if s := string(out); strings.Contains(s, libraryTestPassword) {
					t.Fatalf("the output carries a shard's password:\n%s", s)
				}
				return cmd.ProcessState.ExitCode(), string(out)
			}
			lib := fmt.Sprintf("cli-shard-%d", time.Now().UnixNano()%1_000_000_000_000)

			// shard 2 is reached through a login that is not a publisher: the database refuses it.
			code, out := run("--db", pubDSN[0], "library", "publish", lib, "1.0.0", member,
				"--replicate-to", readerDSN[1], "--replicate-to", pubDSN[2])
			if code == 0 {
				t.Fatalf("a publish through a non-publisher login on shard 2 exited 0:\n%s", out)
			}
			for _, want := range []string{"did not reach every shard", "failed on [shard-2]", "Run the same publish again", "shard-1 = the --db connection (the admin shard)"} {
				if !strings.Contains(out, want) {
					t.Errorf("the failure output lacks %q:\n%s", want, out)
				}
			}
			for i, want := range []int{3, 0, 3} { // 1 member + 1 bundle + 1 listing row
				if n := rows(i, lib); n != want {
					t.Errorf("after the partial failure shard %d holds %d row(s), want %d", i+1, n, want)
				}
			}

			// the repair: the same command, with shard 2's own publisher login.
			code, out = run("--db", pubDSN[0], "library", "publish", lib, "1.0.0", member,
				"--replicate-to", pubDSN[1], "--replicate-to", pubDSN[2])
			if code != 0 {
				t.Fatalf("the same publish again exited %d:\n%s", code, out)
			}
			if !strings.Contains(out, "shard-1: already published") || !strings.Contains(out, "shard-2: 1 member(s) written") || !strings.Contains(out, "shard-3: already published") {
				t.Errorf("the repair output does not show shard 1 and 3 left alone and shard 2 written:\n%s", out)
			}
			for i := range owner {
				if n := rows(i, lib); n != 3 {
					t.Errorf("after the repair shard %d holds %d row(s), want 3", i+1, n)
				}
			}

			// an unreachable shard is found before the first write, so nothing changed anywhere.
			lib2 := lib + "-pre"
			u, err := url.Parse(pubDSN[2])
			if err != nil {
				t.Fatal(err)
			}
			u.Host = "127.0.0.1:1"
			code, out = run("--db", pubDSN[0], "library", "publish", lib2, "1.0.0", member, "--replicate-to", pubDSN[1], "--replicate-to", u.String())
			if code == 0 || !strings.Contains(out, "cannot be reached") || !strings.Contains(out, "Nothing was written to any shard") {
				t.Errorf("an unreachable shard: exit %d, output:\n%s", code, out)
			}
			for i := range owner {
				if n := rows(i, lib2); n != 0 {
					t.Errorf("an unreachable shard was reported but shard %d holds %d row(s) of that library, want 0", i+1, n)
				}
			}
		})
	}
}
