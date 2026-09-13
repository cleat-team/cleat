package engine

import (
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// reInvokedOnReplay is every plugin function whose recorded output the REPLAY
// path throws away.
//
// WHAT LICENSES THAT, and it is two properties rather than one since
// cleat#1318. `FuncOptions.Idempotent` was documented as "safe to re-invoke
// during replay" and engine/plugins.go acted on it literally, so a word that
// only promises "no new side effects" was licensing a determinism claim --
// "returns the same value on replay". Seven functions were registered against
// the weaker reading. Replay now re-invokes only when a registration sets BOTH
// Idempotent and SameValueOnReplay.
//
// Two further properties of that branch make a wrong entry worse than it looks:
// neither flag is persisted in event_history, so replay reads the CURRENT
// registry -- flipping a registration changes the replay semantics of runs
// recorded before the change -- and there is no per-call override.
//
// Every entry states why replaying it live cannot change what the workflow
// already decided. An entry whose reason is "it has no side effects" is wrong
// by construction: that is what Idempotent asks, not what replay needs.
var reInvokedOnReplay = map[string]string{
	"blobstore.get": "a GET has no effect, and a blob key is treated as " +
		"write-once so a replay reads what the original read. The convention " +
		"is NOT enforced by the plugin -- blobGet takes a key and no version " +
		"-- so this is an assertion about how callers use keys",
	"llm.embed": "near-deterministic for a fixed model and input, which is " +
		"the property replay needs rather than mere absence of side effects. " +
		"\"Near\" is doing work: a hosted model can change behind a stable name",
}

// notReInvokedOnReplay is the other half, and it exists so the REASONS survive.
//
// These five were re-invoked on replay before cleat#1318 and are not any more.
// Deleting them from this file would leave the change looking like an absence,
// and an absence is what a broken scan reports too. Asserting them positively
// means a regression -- someone adding SameValueOnReplay back -- fails here
// with the argument against it already written down.
var notReInvokedOnReplay = map[string]string{
	"featureflags.evaluate_flag": "reads mutable state by definition. A " +
		"workflow that branched on enabled=true, suspended, and replayed after " +
		"an operator toggled the flag would evaluate false -- history and the " +
		"live call disagreeing about a decision already taken",
	"pgvector.search": "reads a mutable index. Inserts between the original " +
		"call and the replay change the result set",
	"llm.list_models": "a provider's model list is not stable over a " +
		"workflow's lifetime",
	"eventtriggers.await_event": "selects the latest UNPROCESSED event, so a " +
		"replay can match a different one -- and on the not-found path it " +
		"WRITES, calling registerAwaiter before returning a successful \"no " +
		"event\" output. So it is neither idempotent nor stable",
	"webhookingest.await_webhook": "an await over mutable state, same shape " +
		"as await_event",
}

// TestReplayReInvokesOnlyWhatIsAllowlisted.
//
// A source scan rather than a registry walk, because the registry is populated
// at runtime by whichever plugins a binary embeds -- so a test that asked the
// registry would report only what its own build registered, and a new
// registration in a plugin this test does not import would be invisible. The
// question is about the REPO, so it is asked of the tracked sources.
func TestReplayReInvokesOnlyWhatIsAllowlisted(t *testing.T) {
	found := scanReInvokedOnReplay(t)

	var added []string
	for k := range found {
		if _, ok := reInvokedOnReplay[k]; !ok {
			added = append(added, k)
		}
	}
	var removed []string
	for k := range reInvokedOnReplay {
		if _, ok := found[k]; !ok {
			removed = append(removed, k)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)

	if len(added) > 0 {
		t.Errorf("these plugin functions set BOTH Idempotent and SameValueOnReplay, so the "+
			"replay path discards their recorded output, and they are not in the "+
			"allowlist:\n  %s\n\n"+
			"That pair is a claim that the function RETURNS THE SAME VALUE on replay -- not "+
			"merely that re-invoking is safe. Add an entry saying why that holds, or drop "+
			"SameValueOnReplay. cleat#1318.",
			strings.Join(added, "\n  "))
	}
	if len(removed) > 0 {
		t.Errorf("the allowlist names functions that no longer re-invoke on replay:\n  %s\n\n"+
			"Delete the entries. A stale exemption silently covers whatever arrives at "+
			"that name next.", strings.Join(removed, "\n  "))
	}
}

// TestTheDeliberatelyNotReInvokedStayThatWay asserts the other half POSITIVELY.
//
// The five below were re-invoked on replay until cleat#1318. A test that only
// checked the allowlist would pass if they were silently restored to it and
// someone added matching entries -- and would pass equally if the scan broke
// and returned nothing. Naming them makes the regression loud and gives the
// next author the argument rather than making them re-derive it.
func TestTheDeliberatelyNotReInvokedStayThatWay(t *testing.T) {
	found := scanReInvokedOnReplay(t)
	registered := scanAllRegistrations(t)

	for name, why := range notReInvokedOnReplay {
		// Non-vacuity first: if the function is not registered at all, this
		// entry is checking nothing and the scan cannot tell us otherwise.
		if !registered[name] {
			t.Errorf("%s is in notReInvokedOnReplay but no registration was found for it.\n\n"+
				"Either it was renamed or removed -- in which case delete this entry -- or "+
				"the scan is broken, in which case every other assertion here is vacuous "+
				"too.", name)
			continue
		}
		if found[name] {
			t.Errorf("%s re-invokes on replay again.\n\n  why it must not: %s\n\n"+
				"It sets both Idempotent and SameValueOnReplay, so replay discards its "+
				"recorded output and calls it live. cleat#1318 removed that deliberately.",
				name, why)
		}
	}
}

// scanReInvokedOnReplay returns "<plugin>.<function>" for every Register call
// whose FuncOptions sets BOTH Idempotent and SameValueOnReplay true.
//
// Both, and in either order: the fields are a set, not a sequence, and a scan
// that assumed the order in which they happen to be written today would stop
// matching the first time someone reformatted a literal -- reporting "nothing
// re-invokes", which reads exactly like success.
func scanReInvokedOnReplay(t *testing.T) map[string]bool {
	t.Helper()
	return scanRegistrations(t, func(body string) bool {
		return regexp.MustCompile(`Idempotent:\s*true`).MatchString(body) &&
			regexp.MustCompile(`SameValueOnReplay:\s*true`).MatchString(body)
	})
}

// scanAllRegistrations returns every registered "<plugin>.<function>",
// whatever its policy. Used as the non-vacuity control.
func scanAllRegistrations(t *testing.T) map[string]bool {
	t.Helper()
	return scanRegistrations(t, func(string) bool { return true })
}

func scanRegistrations(t *testing.T, keep func(body string) bool) map[string]bool {
	t.Helper()
	out, err := exec.Command("git", "ls-files", "../plugins").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	files := strings.Fields(string(out))
	if len(files) == 0 {
		t.Fatal("no plugin sources found: the scan would pass vacuously")
	}

	// The literal runs to the closing brace of FuncOptions. [^}]* spans
	// newlines, which multi-line registrations need -- so a COMMENT containing
	// a brace inside one of these literals would cut the match short. None does
	// today; if one is added, this scan quietly stops seeing that registration,
	// which is why the non-vacuity checks below exist.
	re := regexp.MustCompile(`Register\(plugin\.FuncOptions\{([^}]*)\}`)
	nameRe := regexp.MustCompile(`Name:\s*"([^"]+)"`)

	got := map[string]bool{}
	var scanned, literals int
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
			literals++
			nm := nameRe.FindStringSubmatch(m[1])
			if nm == nil {
				continue
			}
			if keep(m[1]) {
				got[pluginName+"."+nm[1]] = true
			}
		}
	}
	if scanned == 0 {
		t.Fatal("scanned no plugin .go files: the scan would pass vacuously")
	}
	if literals == 0 {
		t.Fatal("matched no FuncOptions literals at all: the regex has stopped " +
			"seeing registrations, so every assertion built on this scan is vacuous")
	}
	return got
}
