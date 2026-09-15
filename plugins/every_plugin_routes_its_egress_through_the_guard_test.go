package plugins_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// cleat#1565 open question 4. Every plugin's outbound HTTP goes through the
// egress-guarded transport the worker supplies.
//
// A SOURCE SCAN, and the limit is stated rather than hidden: it proves a client
// is CONSTRUCTED with a Transport, not that every request uses that client. The
// thing it can catch is the failure that actually happens -- a new plugin, or a
// new client in an existing one, built the way all nine were before this
// change.
//
// git ls-files rather than a filesystem walk, so .claude/worktrees -- a whole
// second copy of the repo -- cannot contribute a file and make this pass or
// fail on a scratch checkout.
func TestEveryPluginRoutesItsEgressThroughTheGuard(t *testing.T) {
	// The one legitimate unguarded client in the tree, exempted BY NAME with
	// the reason, so it stays a decision rather than an omission.
	exempt := map[string]string{
		"plugins/blobstore/backend.go": "the AWS IAM credential chain: reaching " +
			"169.254.169.254 IS its job, and its destination comes from the SDK rather " +
			"than from a tenant, a config or a workflow",
	}

	// -C root: git ls-files run from plugins/ lists only plugins/ and prints
	// paths relative to it, so every later read fails and the scan measures
	// nothing. The floor below caught that, which is the only reason this
	// comment exists rather than a silent pass.
	root, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("git rev-parse: %v", err)
	}
	repo := strings.TrimSpace(string(root))
	out, err := exec.Command("git", "-C", repo, "ls-files", "plugins/*.go", "plugins/**/*.go").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	files := strings.Fields(string(out))
	if len(files) < 50 {
		t.Fatalf("git ls-files matched %d files under plugins/; the scan did not see the tree", len(files))
	}

	clientRE := regexp.MustCompile(`&http\.Client\{`)
	examined, found := 0, 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		// FROM DISK, not from `git show HEAD:`. git ls-files supplies the
		// LIST -- which is what keeps scratch worktrees out of scope -- and
		// the bytes come from the working tree. Reading HEAD is exactly
		// backwards: it passes a change that ADDS an unguarded client and
		// fails the change that fixes one. cmd/cleat/documented_cli_surface_test.go
		// records the same mistake, and I made it here anyway before the
		// output made it obvious.
		body, err := os.ReadFile(filepath.Join(repo, f))
		if err != nil {
			continue
		}
		src := string(body)
		locs := clientRE.FindAllStringIndex(src, -1)
		if locs == nil {
			continue
		}
		examined++
		if why, ok := exempt[f]; ok {
			found++
			if !strings.Contains(src, "cleat#1565") {
				t.Errorf("%s is exempt (%s) but says nothing about why at the site; "+
					"an unexplained exemption is indistinguishable from an oversight", f, why)
			}
			continue
		}
		for _, loc := range locs {
			// The Transport must be set in THIS composite literal, so look
			// only as far as the next 400 bytes rather than anywhere in the
			// file -- a Transport on some other client would otherwise vouch
			// for this one.
			end := loc[1] + 400
			if end > len(src) {
				end = len(src)
			}
			if !strings.Contains(src[loc[1]:end], "Transport:") {
				t.Errorf("%s builds an http.Client with no Transport. Plugin egress must go "+
					"through env.HTTPTransport, or the loopback/link-local/RFC1918 floor does "+
					"not apply to it (cleat#1565). If it genuinely must be unguarded, exempt "+
					"it by name in this test with the reason.", f)
			}
			found++
		}
	}
	if examined == 0 || found == 0 {
		t.Fatalf("examined %d files and found %d clients; the scan measured nothing, "+
			"which reads identically to a clean tree", examined, found)
	}
	// Every exemption must still match something, or it is a grant covering
	// code that no longer exists.
	for f := range exempt {
		if !strings.Contains(strings.Join(files, " "), f) {
			t.Errorf("exemption for %q matches no tracked file; delete it", f)
		}
	}
}
