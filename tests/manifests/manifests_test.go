// Package manifests checks that the deployment manifests the project ships can
// actually start the binary they invoke.
//
// This is IMPROVEMENT-PLAN §2.7, in the form that does not need a cluster.
// Go's flag package treats an unrecognised flag as fatal: it prints
// "flag provided but not defined", dumps usage, and exits 2. In Kubernetes that
// is a CrashLoopBackOff on every pod, forever, and no test in the repo noticed
// because nothing ever ran the manifests' arguments past flag parsing.
//
// What this does NOT cover: whether a worker started this way reaches ready.
// That needs a real cluster (kind/helm) in CI. This covers the failure mode
// that was actually shipped -- arguments the binary rejects outright.
package manifests

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// repoRoot is where this package sits relative to the module root.
const repoRoot = "../.."

// workerFlagSet returns the flags cleat-worker actually accepts.
//
// It builds the binary and reads --help rather than scanning the source for
// flag.String(...) calls. That is deliberate: a source scan is a second,
// hand-maintained model of the flag set, and it is wrong in both directions --
// flags can be registered from init() in an imported package, and a scan that
// looks right can silently miss real flags. --help is what the container sees.
func workerFlagSet(t *testing.T) map[string]bool {
	t.Helper()

	bin := filepath.Join(t.TempDir(), "cleat-worker")
	build := exec.Command("go", "build", "-o", bin, "./cmd/cleat-worker")
	build.Dir = repoRoot
	// CGO is not needed to enumerate flags, and the wasmtime headers are not
	// always present. See CLAUDE.md.
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building cleat-worker: %v\n%s", err, out)
	}

	// --help exits non-zero by design; the usage text is what we want.
	out, _ := exec.Command(bin, "--help").CombinedOutput()

	flagLine := regexp.MustCompile(`(?m)^\s+-([a-zA-Z0-9][a-zA-Z0-9._-]*)`)
	flags := map[string]bool{}
	for _, m := range flagLine.FindAllStringSubmatch(string(out), -1) {
		flags[m[1]] = true
	}

	// Guard against a parse that silently matches nothing: an empty flag set
	// would make every manifest assertion below pass vacuously.
	if len(flags) < 40 {
		t.Fatalf("parsed only %d flags from cleat-worker --help, want at least 40 -- "+
			"the usage format changed and this parser no longer reads it:\n%s", len(flags), out)
	}
	return flags
}

// argListItem matches a YAML sequence entry that begins a command-line flag,
// e.g. `- "--task-queue=queue-1"` or `- --concurrency=10`.
var argListItem = regexp.MustCompile(`^\s*-\s+"?--([a-zA-Z0-9][a-zA-Z0-9._-]*)`)

// blockStart matches the keys whose sequence entries are argv for the container.
var blockStart = regexp.MustCompile(`^\s*(args|command):\s*$`)

// manifestFlags returns the flag names passed to a container in the given file.
//
// It scans text rather than parsing YAML because charts/cleat is a Go template
// and does not parse as YAML until it is rendered. Flag *names* are always
// literal -- only the values carry {{ }} -- so the contract this test checks
// survives the templating.
//
// Only entries directly under an `args:` or `command:` key are collected, and
// only those starting with `--`. That skips probe commands like
// `- "wget -q -O- ... --post-data=..."`, whose first token is not a flag.
func manifestFlags(t *testing.T, path string) []string {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(repoRoot, path))
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}

	var flags []string
	inBlock := false
	for _, line := range strings.Split(string(data), "\n") {
		if blockStart.MatchString(line) {
			inBlock = true
			continue
		}
		if !inBlock {
			continue
		}
		if m := argListItem.FindStringSubmatch(line); m != nil {
			flags = append(flags, m[1])
			continue
		}
		// A non-empty line that is not a sequence entry ends the block.
		if strings.TrimSpace(line) != "" && !strings.HasPrefix(strings.TrimSpace(line), "-") {
			inBlock = false
		}
	}
	return flags
}

// TestDeploymentManifestsUseFlagsTheWorkerAccepts is the regression test for the
// crash-loop: k8s/deployment.yaml passed --namespace and charts/cleat passed
// --tenant-id and --namespace, none of which cleat-worker has. --namespace was
// removed in dfa8702 when the namespace concept was deleted from the store
// interface; --tenant-id was never a cleat-worker flag at all.
func TestDeploymentManifestsUseFlagsTheWorkerAccepts(t *testing.T) {
	accepted := workerFlagSet(t)

	manifests := []string{
		"k8s/deployment.yaml",
		"charts/cleat/templates/deployment.yaml",
		"docker-compose.cluster.yml",
	}

	for _, path := range manifests {
		t.Run(path, func(t *testing.T) {
			used := manifestFlags(t, path)

			// Non-vacuity: if the extractor stops finding arguments -- because a
			// manifest is restructured, or the regex rots -- this test would
			// pass while checking nothing. Every one of these manifests starts a
			// worker, so every one must pass --db.
			if len(used) < 3 {
				t.Fatalf("extracted only %d flags from %s, want at least 3 -- "+
					"the manifest's arg block is no longer being read", len(used), path)
			}
			if !contains(used, "db") {
				t.Fatalf("extracted %v from %s, which does not include --db -- "+
					"the arg block being read is not the worker's", used, path)
			}

			for _, f := range used {
				if !accepted[f] {
					t.Errorf("%s passes --%s, which cleat-worker does not define; "+
						"Go's flag package exits 2 on an unknown flag, so every "+
						"container started from this manifest crash-loops", path, f)
				}
			}
		})
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
