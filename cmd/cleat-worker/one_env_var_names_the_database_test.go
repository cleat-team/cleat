package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// cmd/cleat read CLEAT_DATABASE_URL while cmd/cleat-worker and cmd/cleat-bench
// read DATABASE_URL -- same flag name, same repo, different variable. Both
// names appeared in the SAME operations documents, so a reader had to work out
// which binary took which.
//
// Worse, engine/credentials.go read BOTH and preferred the generic one, so when
// both were set -- precisely the shared pod this namespace exists to survive --
// an unrelated application's database silently became cleat's. Connecting to
// the wrong database succeeds, so nothing reports it.
//
// A SCAN, because this is a claim about the whole tree and one call site
// reverting is invisible in review. Shaped after
// TestOnlyThePluginTransportCarriesThePrivateHostExemption, which exists for
// the same reason: a future caller could read the generic name in a fifth file
// and every behavioural test would still pass.
func TestNoProductionCodeReadsTheGenericDatabaseURL(t *testing.T) {
	root := repoRootForEnvScan(t)
	out, err := exec.Command("git", "-C", root, "ls-files", "*.go").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	files := strings.Fields(string(out))
	// A scan that found nothing reports a clean tree, which reads identically
	// to success.
	if len(files) < 100 {
		t.Fatalf("git ls-files matched %d Go files; the scan did not see the repo", len(files))
	}

	// The BARE name only: CLEAT_DATABASE_URL contains DATABASE_URL as a
	// substring, and matching that would report every correct call site.
	bare := regexp.MustCompile(`Getenv\("DATABASE_URL"\)`)

	var offenders []string
	checked := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		body, rerr := os.ReadFile(filepath.Join(root, f))
		if rerr != nil {
			continue
		}
		checked++
		if bare.Match(body) {
			offenders = append(offenders, f)
		}
	}
	if checked == 0 {
		t.Fatal("no non-test Go files were read; the scan measured nothing")
	}

	if len(offenders) != 0 {
		t.Errorf("these read the generic DATABASE_URL: %v\n"+
			"Use CLEAT_DATABASE_URL. Every other service in a shared pod or container "+
			"also sets DATABASE_URL, so the generic name lets an unrelated "+
			"application's database silently become cleat's -- and connecting to the "+
			"wrong database succeeds, so nothing reports it.", offenders)
	}
}

// The matcher must see the thing it is looking for, and must not fire on the
// namespaced name it is meant to permit. Without this, a regex that stopped
// matching would report a clean tree forever.
func TestTheEnvScanSeesTheBareNameAndNotTheNamespacedOne(t *testing.T) {
	bare := regexp.MustCompile(`Getenv\("DATABASE_URL"\)`)
	for _, tc := range []struct {
		src  string
		want bool
		why  string
	}{
		{`os.Getenv("DATABASE_URL")`, true, "the exact call this forbids"},
		{`v := os.Getenv("DATABASE_URL"); _ = v`, true, "assigned rather than returned"},
		{`os.Getenv("CLEAT_DATABASE_URL")`, false,
			"the namespaced name CONTAINS the bare one as a substring; matching it " +
				"would report every correct call site and the scan would be useless"},
		{`os.Getenv("OTHER_DATABASE_URL")`, false, "a different variable ending the same way"},
		{`// DATABASE_URL used to be read here`, false, "a sentence about it is not a read"},
	} {
		if got := bare.MatchString(tc.src); got != tc.want {
			t.Errorf("match(%q) = %v, want %v -- %s", tc.src, got, tc.want, tc.why)
		}
	}
}

func repoRootForEnvScan(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("git rev-parse: %v", err)
	}
	return strings.TrimSpace(string(out))
}
