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
		// The migration steps (cleat#2117) are worker invocations too, and a
		// --migrate-only that the binary did not define would fail the deploy step
		// itself, before any worker started.
		"k8s/migrate-job.yaml",
		"charts/cleat/templates/migrate-job.yaml",
	}

	for _, path := range manifests {
		t.Run(path, func(t *testing.T) {
			used := manifestFlags(t, path)

			// Non-vacuity: if the extractor stops finding arguments -- because a
			// manifest is restructured, or the regex rots -- this test would
			// pass while checking nothing. Every one of these manifests starts a
			// worker, so every one must pass --db.
			// The floor is per manifest: a migration Job legitimately passes only
			// --migrate-only and --db.
			floor := 3
			if strings.HasSuffix(path, "migrate-job.yaml") {
				floor = 2
				if !contains(used, "migrate-only") {
					t.Fatalf("extracted %v from %s, which does not include --migrate-only -- "+
						"the arg block being read is not the migration step's", used, path)
				}
			}
			if len(used) < floor {
				t.Fatalf("extracted only %d flags from %s, want at least %d -- "+
					"the manifest's arg block is no longer being read", len(used), path, floor)
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

// TestEveryWorkerLaunchSiteSaysHowItsSchemaGetsMigrated (cleat#2117). A worker no
// longer migrates the database when it starts, so every artifact that starts one has
// to do one of two things, and this names which for each:
//
//	"--migrate-only"     it is (or has) the deploy step that migrates first
//	"--migrate-on-start" it is a single node that migrates itself
//
// A launch site that does neither works on a database that happens to be migrated
// already and fails, with a message that explains itself, on a fresh one -- which is
// the case nobody re-tests. The list is asserted COMPLETE against the repo, not just
// correct for the entries in it: a new artifact that starts a worker and is in neither
// list fails here rather than shipping without an answer.
func TestEveryWorkerLaunchSiteSaysHowItsSchemaGetsMigrated(t *testing.T) {
	want := map[string]string{
		// A deploy step exists for these.
		"docker-compose.cluster.yml":              "--migrate-only",
		"charts/cleat/templates/migrate-job.yaml": "--migrate-only",
		"k8s/migrate-job.yaml":                    "--migrate-only",
		"packaging/systemd/cleat-worker.service":  "--migrate-only",
		// Single-node development and scaffolds: the worker migrates itself.
		"cmd/cleat/templates/agent/docker-compose.yml":     "--migrate-on-start",
		"cmd/cleat/templates/fullstack/docker-compose.yml": "--migrate-on-start",
		"cmd/cleat/templates/workflow/docker-compose.yml":  "--migrate-on-start",
		"Makefile": "--migrate-on-start",
	}
	for path, flag := range want {
		data, err := os.ReadFile(filepath.Join(repoRoot, path))
		if err != nil {
			t.Errorf("%s: %v", path, err)
			continue
		}
		// Ignore comment-only mentions: a comment that says "--migrate-only" is not a
		// launch site that passes it.
		found := false
		for _, line := range strings.Split(string(data), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "#") {
				continue
			}
			if strings.Contains(line, flag) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s starts a worker but does not pass %s outside a comment: on a fresh database "+
				"the worker verifies the schema and refuses to start", path, flag)
		}
	}

	// COMPLETENESS. Every tracked file that runs the worker image or binary with a
	// database must be in `want` or be named here with a reason. The patterns are the
	// two ways a launch site refers to the worker: the published image, and the
	// installed binary's unit. (A new kind of launch site -- a Nomad job, a Procfile --
	// is not found by these; adding its pattern is how that gets covered.)
	out, err := exec.Command("git", "-C", repoRoot, "ls-files").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	exempt := map[string]string{
		"charts/cleat/templates/deployment.yaml": "the runtime Deployment: verifies; the migrate-job.yaml hook migrates first",
		"k8s/deployment.yaml":                    "the runtime Deployment: verifies; k8s/migrate-job.yaml migrates first",
		"charts/cleat/values.yaml":               "names the image only",
		"Dockerfile":                             "the image: ENTRYPOINT with no database",
		"docker-compose.cluster.yml":             "asserted above",
	}
	launch := regexp.MustCompile(`ghcr\.io/cleat-team/cleat-worker|image:\s*cleat-worker:latest|ExecStart=.*cleat-worker`)
	seen := 0
	for _, path := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if !(strings.HasSuffix(path, ".yml") || strings.HasSuffix(path, ".yaml") || strings.HasSuffix(path, ".service") ||
			strings.HasSuffix(path, ".tpl")) || strings.HasPrefix(path, ".github/") || strings.HasPrefix(path, "docs/") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(repoRoot, path))
		if err != nil || !launch.Match(data) {
			continue
		}
		seen++
		if _, ok := want[path]; ok {
			continue
		}
		if _, ok := exempt[path]; ok {
			continue
		}
		t.Errorf("%s starts the cleat-worker image or unit but is in neither the migrated-how list nor the "+
			"exemptions of this test: say how its schema gets migrated (cleat#2117)", path)
	}
	if seen < 6 {
		t.Fatalf("found only %d launch sites in git ls-files; the scan is not looking where they are", seen)
	}
}
