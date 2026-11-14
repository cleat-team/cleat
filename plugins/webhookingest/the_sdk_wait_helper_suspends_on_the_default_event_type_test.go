package webhookingest

import (
	"os"
	"regexp"
	"testing"
)

// The SDK's typed wait suspends on "__evt:"+type, and the claim is keyed on an
// EXACT type, so when a caller names none it has to use the type this plugin
// assigns an event whose sender set neither X-Github-Event nor X-Event-Type. A
// mismatch is a wait that never wakes and times out looking like a quiet source
// (cleat#3794).
//
// THE SDK CONSTANT IS READ FROM SOURCE, NOT IMPORTED. The SDK is its own module
// and the root module deliberately does not require it: a test that imports
// cleat/pluginclients/webhookingest makes `go mod tidy` add the published
// github.com/cleat-team/cleat/cleat to the root go.mod, and outside the
// workspace that resolves to a release that has no DefaultEventType, so the test
// could not compile there. The cross-module Lint guard caught the import on the
// first push; reading the file is the shape the other cross-module checks use.
var sdkDefaultEventTypeRe = regexp.MustCompile(`(?m)^const DefaultEventType = "([^"]*)"\s*$`)

func TestTheSDKWaitHelpersDefaultEventTypeIsThePluginsDefault(t *testing.T) {
	const path = "../../cleat/pluginclients/webhookingest/wait.go"
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the SDK's wait helper: %v", err)
	}
	m := sdkDefaultEventTypeRe.FindSubmatch(src)
	if m == nil {
		t.Fatalf("%s has no `const DefaultEventType = \"...\"` line: the SDK helper or this pattern moved, "+
			"and a test that matched nothing would pass for the wrong reason", path)
	}
	if got := string(m[1]); got != defaultWebhookEventType {
		t.Fatalf("SDK Wait waits on %q when no event type is given, but the plugin stores such an event as %q",
			got, defaultWebhookEventType)
	}
}

// The control: the pattern reads a value out of a line shaped like the SDK's and
// refuses lines that merely mention the name, so "matched" is a statement about
// the constant and not about a comment that describes it.
func TestTheDefaultEventTypePatternReadsOnlyTheConstant(t *testing.T) {
	if m := sdkDefaultEventTypeRe.FindSubmatch([]byte("const DefaultEventType = \"hook\"\n")); m == nil || string(m[1]) != "hook" {
		t.Errorf("the pattern did not read the value of a declaration: %v", m)
	}
	for _, not := range []string{
		"// const DefaultEventType = \"webhook\"\n",
		"\tconst DefaultEventType = \"webhook\"\n",
		"var DefaultEventType = \"webhook\"\n",
	} {
		if sdkDefaultEventTypeRe.Match([]byte(not)) {
			t.Errorf("the pattern matched a line that is not the declaration: %q", not)
		}
	}
}
