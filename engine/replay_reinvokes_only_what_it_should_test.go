package engine

import (
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// idempotentRegistrations is every plugin function whose recorded output the
// REPLAY path throws away.
//
// WHAT THE FLAG ACTUALLY DOES, because its name does not say so.
// `FuncOptions.Idempotent` is documented as "safe to re-invoke during replay",
// and engine/plugins.go acts on it literally: on replay it discards
// rec.PluginOutput and calls the function live. That licenses a determinism
// claim -- "returns the same value on replay" -- from a word that only promises
// "no new side effects". They are different properties, and eight functions
// were registered against the weaker one (cleat#1318).
//
// Two further properties of that branch make a wrong entry worse than it looks:
// the flag is NOT persisted in event_history, so replay reads the CURRENT
// registry -- flipping a registration changes the replay semantics of runs
// recorded before the change -- and there is no per-call override.
//
// So this list is an allowlist, and every entry states why replaying it live
// cannot change what the workflow already decided. An entry whose reason is
// "it has no side effects" is wrong by construction: that is the question the
// flag's name asks, not the one replay needs.
var idempotentRegistrations = map[string]string{
	"blobstore.get": "a blob is immutable once written -- the key names a " +
		"specific object, and overwriting it is a different operation with a " +
		"different key in every store this plugin targets",
	"llm.embed": "near-deterministic for a fixed model and input, which is " +
		"the property replay needs. Not merely side-effect-free",

	// The four below are NOT defended as replay-deterministic. They are here
	// because removing the flag changes resumption behaviour and that is a
	// decision rather than a fix -- see the test's failure message. Each names
	// what makes it non-deterministic, so nobody has to re-derive it.
	"featureflags.evaluate_flag": "OPEN (cleat#1318): reads mutable state by " +
		"definition. A workflow that branched on enabled=true, suspended, and " +
		"replayed after an operator toggled the flag evaluates false -- the " +
		"recorded history and the live call disagree",
	"pgvector.search": "OPEN (cleat#1318): reads a mutable index. Inserts " +
		"between the original call and the replay change the result set",
	"llm.list_models": "OPEN (cleat#1318): a provider's model list is not " +
		"stable over a workflow's lifetime",
	"eventtriggers.await_event": "OPEN (cleat#1318): selects the latest " +
		"UNPROCESSED event, so a replay can match a different one -- and on " +
		"the not-found path it WRITES, calling registerAwaiter. Removing the " +
		"flag has to answer what re-registers the awaiter after a crash",
	"webhookingest.await_webhook": "OPEN (cleat#1318): an await over mutable " +
		"state, same shape as await_event",
}

// TestReplayReInvokesOnlyWhatIsAllowlisted.
//
// A source scan rather than a registry walk, because the registry is populated
// at runtime by whichever plugins a binary embeds -- so a test that asked the
// registry would report only what its own build registered, and a new
// registration in a plugin this test does not import would be invisible. The
// question is about the REPO, so it is asked of the tracked sources.
func TestReplayReInvokesOnlyWhatIsAllowlisted(t *testing.T) {
	found := scanIdempotentRegistrations(t)

	var added []string
	for k := range found {
		if _, ok := idempotentRegistrations[k]; !ok {
			added = append(added, k)
		}
	}
	var removed []string
	for k := range idempotentRegistrations {
		if _, ok := found[k]; !ok {
			removed = append(removed, k)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)

	if len(added) > 0 {
		t.Errorf("these plugin functions are registered Idempotent and are not in the "+
			"allowlist:\n  %s\n\n"+
			"Idempotent means the REPLAY path discards the recorded output and calls the "+
			"function live, so it is a claim that the function RETURNS THE SAME VALUE on "+
			"replay -- not merely that re-invoking is safe. Add an entry saying why that "+
			"holds, or drop the flag. cleat#1318.",
			strings.Join(added, "\n  "))
	}
	if len(removed) > 0 {
		t.Errorf("the allowlist names functions no longer registered Idempotent:\n  %s\n\n"+
			"Delete the entries. A stale exemption silently covers whatever arrives at "+
			"that name next.", strings.Join(removed, "\n  "))
	}
}

// scanIdempotentRegistrations returns "<plugin>.<function>" for every
// Register call carrying Idempotent: true.
func scanIdempotentRegistrations(t *testing.T) map[string]bool {
	t.Helper()
	out, err := exec.Command("git", "ls-files", "../plugins").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	files := strings.Fields(string(out))
	if len(files) == 0 {
		t.Fatal("no plugin sources found: the scan would pass vacuously")
	}

	// The plugin name is the directory, which is how the registry keys them.
	re := regexp.MustCompile(`Register\(plugin\.FuncOptions\{[^}]*Name:\s*"([^"]+)"[^}]*Idempotent:\s*true[^}]*\}`)
	got := map[string]bool{}
	var scanned int
	for _, f := range files {
		if !strings.HasSuffix(f, ".go") || strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		scanned++
		// git ls-files prints paths relative to the CWD, which for a go test
		// is the package directory -- so these arrive as "../plugins/<name>/…"
		// rather than "plugins/<name>/…". Keying off the segment AFTER
		// "plugins" rather than a fixed index keeps that from mattering.
		parts := strings.Split(f, "/")
		var pluginName string
		for i, seg := range parts {
			if seg == "plugins" && i+1 < len(parts) {
				pluginName = parts[i+1]
				break
			}
		}
		if pluginName == "" {
			continue
		}
		for _, m := range re.FindAllStringSubmatch(string(b), -1) {
			got[pluginName+"."+m[1]] = true
		}
	}
	if scanned == 0 {
		t.Fatal("scanned no plugin .go files: the scan would pass vacuously")
	}
	return got
}
