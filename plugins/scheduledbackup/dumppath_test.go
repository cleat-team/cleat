package scheduledbackup

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestATraversingConfigNameIsRefused is cleat#1305.
//
// A backup config's name was checked only for non-emptiness, then interpolated
// into `manual_%s_%s.dump` and joined to DumpDir on its way to `pg_dump -f`. An
// authenticated tenant could direct a full CROSS-TENANT database dump into any
// directory the worker's user can write.
//
// The payloads below are run through the real expression rather than asserted
// against in the abstract, because the exploit is narrower than it first looks
// and the fix has to be aimed at what actually happens:
//
//   - the `manual_` prefix eats one traversal level, since `manual_..` is a
//     literal directory name rather than a parent reference;
//   - the `_<timestamp>.dump` suffix is always appended, so an exact filename
//     like /var/spool/cron/crontabs/root cannot be landed on;
//   - a leading `/` does not escape, because filepath.Join treats it as a
//     relative segment.
//
// What remains is writing a complete database dump into an attacker-chosen
// directory under an attacker-chosen prefix -- a data-exfiltration primitive
// and an unbounded disk fill.
func TestATraversingConfigNameIsRefused(t *testing.T) {
	const dumpDir = "/var/lib/cleat/dumps"

	for _, name := range []string{
		"../../../../var/spool/cron/crontabs/root",
		"../../../../../../srv/www/html/leak",
		"/etc/passwd",
		"..",
		".",
		"../sibling",
		"a/b",
		"a\x00b",
		"",
		strings.Repeat("x", maxConfigNameLen+1),
	} {
		t.Run(fmt.Sprintf("%q", name), func(t *testing.T) {
			if ValidConfigName(name) {
				t.Errorf("ValidConfigName(%q) = true, want false", name)
			}
		})
	}

	// And the join refuses the ones that escape, independently of the charset
	// check. BOTH are asserted because they cover different populations: the
	// allowlist guards the two HTTP doors, and the join is the only guard the
	// cron sweep and the CLI command reach -- neither of which passes through a
	// handler, so a row ALREADY STORED with a traversing name is executed from
	// there with nobody watching.
	for _, name := range []string{
		"../../../../var/spool/cron/crontabs/root",
		"../../../../../../srv/www/html/leak",
	} {
		filename := fmt.Sprintf("manual_%s_%s.dump", name, time.Now().Format("20060102150405"))
		got, err := SafeDumpPath(dumpDir, filename)
		if err == nil {
			t.Errorf("SafeDumpPath(%q, ...) returned %q, want a refusal.\n\n"+
				"This is the guard that covers config rows written before the name "+
				"was validated; without it they keep working.", name, got)
		}
	}
}

// TestSafeDumpPathAcceptsOrdinaryNames is the control, and it is load-bearing:
// every assertion above is satisfied by a SafeDumpPath that refuses everything,
// which would break backups entirely rather than secure them.
func TestSafeDumpPathAcceptsOrdinaryNames(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"nightly", "prod-db", "tenant_42", "v1.2.3-weekly"} {
		if !ValidConfigName(name) {
			t.Errorf("ValidConfigName(%q) = false: a legitimate backup name was rejected, "+
				"which is a worse outcome for an existing operator than the bug", name)
		}
		filename := fmt.Sprintf("manual_%s_20260912010203.dump", name)
		got, err := SafeDumpPath(dir, filename)
		if err != nil {
			t.Fatalf("SafeDumpPath(%q, %q): %v", dir, filename, err)
		}
		if want := filepath.Join(dir, filename); got != want {
			t.Errorf("SafeDumpPath = %q, want %q", got, want)
		}
	}
}

// TestSafeDumpPathRefusesASiblingSharingAPrefix pins the check against the
// mistake a prefix comparison invites.
//
// /var/lib/cleat/dumps-evil starts with /var/lib/cleat/dumps, so a plain
// strings.HasPrefix accepts it. The separator is what makes the comparison mean
// "inside" rather than "starts with".
func TestSafeDumpPathRefusesASiblingSharingAPrefix(t *testing.T) {
	base := t.TempDir()
	dumps := filepath.Join(base, "dumps")
	if err := os.MkdirAll(dumps, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := SafeDumpPath(dumps, "../dumps-evil/x.dump"); err == nil {
		t.Error("SafeDumpPath accepted a sibling directory sharing the dump directory's " +
			"prefix; the comparison is HasPrefix without a separator")
	}
	// Control: the same shape, inside, is accepted.
	if _, err := SafeDumpPath(dumps, "x.dump"); err != nil {
		t.Errorf("SafeDumpPath refused an ordinary filename: %v", err)
	}
}

// TestEveryDumpPathGoesThroughTheChokePoint is the completeness half.
//
// The tests above cover the three call sites that exist today. A fourth added
// later -- another command, an export endpoint -- would join a filename to the
// dump directory and reach pg_dump with no check, and every test here would
// still pass. This reads the source so that omission fails instead.
func TestEveryDumpPathGoesThroughTheChokePoint(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || f == "dumppath.go" {
			continue
		}
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("reading %s: %v", f, err)
		}
		for i, line := range strings.Split(string(raw), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			if !strings.Contains(trimmed, "filepath.Join(") {
				continue
			}
			if !strings.Contains(trimmed, "DumpDir") && !strings.Contains(trimmed, "dumpDir") {
				continue
			}
			found++
			t.Errorf("%s:%d joins a filename to the dump directory directly:\n\t%s\n\n"+
				"Use SafeDumpPath. cleat#1305: the config name reaching pg_dump -f "+
				"unvalidated let an authenticated tenant write a cross-tenant dump "+
				"outside DumpDir.", f, i+1, trimmed)
		}
	}
	_ = found
}
