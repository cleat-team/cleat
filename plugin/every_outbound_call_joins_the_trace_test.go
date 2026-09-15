package plugin

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// notYetPropagating declares each outbound-request site that does NOT yet join
// the caller's trace, keyed "<path>:<enclosing func>".
//
// AN ENTRY IS A DEBT, NOT A GRANT. cleat#1596 is staged: the injector and the
// guest's own fetch land first, the plugin sweep second. This list is the work
// remaining, written down so it is countable instead of rediscovered, and so a
// NEW outbound call cannot join it silently.
//
// The reason field says why the site is not done rather than why it is exempt.
// Two genuinely never propagate and say so; the rest are queued.
// stageTwo marks a site queued for the plugin sweep. Deliberately one shared
// string: these are not distinct reasons, and inventing twenty different
// sentences would read as twenty considered decisions.
const stageTwo = "stage 2 of cleat#1596: queued for the plugin sweep. A live caller trace " +
	"exists here -- the trace-id reaches a plugin on plugin.CallContextFromContext(ctx).TraceID."

var notYetPropagating = map[string]string{
	// --- out of scope: not a hop in any run's causal chain ---
	"cleat/backendkit/client.go:CallPlugin":               "a CLIENT library for callers OF cleat, not cleat making an outbound call on a run's behalf. Any trace it carries belongs to its own caller's process; injecting cleat's here would attach a foreign trace to somebody else's request.",
	"cleat/backendkit/client.go:DeleteWorkflow":           "a CLIENT library for callers OF cleat, not cleat making an outbound call on a run's behalf. Any trace it carries belongs to its own caller's process; injecting cleat's here would attach a foreign trace to somebody else's request.",
	"cleat/backendkit/client.go:GetHistory":               "a CLIENT library for callers OF cleat, not cleat making an outbound call on a run's behalf. Any trace it carries belongs to its own caller's process; injecting cleat's here would attach a foreign trace to somebody else's request.",
	"cleat/backendkit/client.go:GetWorkflow":              "a CLIENT library for callers OF cleat, not cleat making an outbound call on a run's behalf. Any trace it carries belongs to its own caller's process; injecting cleat's here would attach a foreign trace to somebody else's request.",
	"cleat/backendkit/client.go:GetWorkflowState":         "a CLIENT library for callers OF cleat, not cleat making an outbound call on a run's behalf. Any trace it carries belongs to its own caller's process; injecting cleat's here would attach a foreign trace to somebody else's request.",
	"cleat/backendkit/client.go:Health":                   "a CLIENT library for callers OF cleat, not cleat making an outbound call on a run's behalf. Any trace it carries belongs to its own caller's process; injecting cleat's here would attach a foreign trace to somebody else's request.",
	"cleat/backendkit/client.go:ListWorkflows":            "a CLIENT library for callers OF cleat, not cleat making an outbound call on a run's behalf. Any trace it carries belongs to its own caller's process; injecting cleat's here would attach a foreign trace to somebody else's request.",
	"cleat/backendkit/client.go:QueryState":               "a CLIENT library for callers OF cleat, not cleat making an outbound call on a run's behalf. Any trace it carries belongs to its own caller's process; injecting cleat's here would attach a foreign trace to somebody else's request.",
	"cleat/backendkit/client.go:SignalWorkflow":           "a CLIENT library for callers OF cleat, not cleat making an outbound call on a run's behalf. Any trace it carries belongs to its own caller's process; injecting cleat's here would attach a foreign trace to somebody else's request.",
	"cleat/backendkit/client.go:StartWorkflow":            "a CLIENT library for callers OF cleat, not cleat making an outbound call on a run's behalf. Any trace it carries belongs to its own caller's process; injecting cleat's here would attach a foreign trace to somebody else's request.",
	"cleat/backendkit/client.go:StartWorkflowRaw":         "a CLIENT library for callers OF cleat, not cleat making an outbound call on a run's behalf. Any trace it carries belongs to its own caller's process; injecting cleat's here would attach a foreign trace to somebody else's request.",
	"cleat/backendkit/client.go:StartWorkflowWithOptions": "a CLIENT library for callers OF cleat, not cleat making an outbound call on a run's behalf. Any trace it carries belongs to its own caller's process; injecting cleat's here would attach a foreign trace to somebody else's request.",
	"plugin/index.go:DownloadWASM":                        "fetches from the plugin REGISTRY during resolution -- before and outside any run, so there is no caller trace to join. Would need a trace ORIGINATED rather than continued, which is the scheduled-run question in stage 3.",
	"plugin/index.go:fetchURL":                            "fetches from the plugin REGISTRY during resolution -- before and outside any run, so there is no caller trace to join. Would need a trace ORIGINATED rather than continued, which is the scheduled-run question in stage 3.",

	// --- stage 2: the plugin sweep. Each has a live caller trace to join. ---
	"cmd/cleat-worker/setup.go:forwardToBenchSvc":       stageTwo,
	"plugins/notifications/background.go:deliver":       stageTwo,
	"plugins/slacknotify/host_functions.go:sendMessage": stageTwo,
	"cleat/embedded/runner.go:handleHTTPFetch": "stage 2 of cleat#1596, and it needs one " +
		"thing the plugin sites do not: the EMBEDDED runner has no inbound request, so it has " +
		"no trace to continue. Sibling of the worker's handleHTTPFetch, which this PR fixes, " +
		"but the fix there is origination rather than propagation -- the scheduled-run case.",
	"plugins/datadogexport/background.go:exportForConfig":        stageTwo,
	"plugins/email/host_functions.go:checkStatus":                stageTwo,
	"plugins/kafkaconnect/background.go:consumeViaRestProxy":     stageTwo,
	"plugins/kafkaconnect/background.go:createConsumer":          stageTwo,
	"plugins/kafkaconnect/background.go:pollRecords":             stageTwo,
	"plugins/kafkaconnect/background.go:subscribeConsumer":       stageTwo,
	"plugins/kafkaconnect/host_functions.go:produceViaRestProxy": stageTwo,
	"plugins/llm/providers/anthropic.go:AnthropicChat":           stageTwo,
	"plugins/llm/providers/anthropic.go:AnthropicChatStream":     stageTwo,
	"plugins/llm/providers/gemini.go:GeminiChat":                 stageTwo,
	"plugins/llm/providers/ollama.go:OllamaChat":                 stageTwo,
	"plugins/llm/providers/ollama.go:OllamaChatStream":           stageTwo,
	"plugins/llm/providers/openai.go:OpenAIChat":                 stageTwo,
	"plugins/llm/providers/openai.go:OpenAIChatStream":           stageTwo,
	"plugins/llm/providers/openai.go:OpenAIEmbed":                stageTwo,
	"plugins/oauthprovider/routes.go:handleCallback":             stageTwo,
	"plugins/pagerdutyalert/host_functions.go:postToPagerDuty":   stageTwo,
}

// TestEveryOutboundCallJoinsTheTrace fails when an outbound HTTP request is
// built at a site that neither propagates the caller's trace nor is declared as
// not yet doing so. cleat#1596.
//
// WHY A GUARD AND NOT A CHECKLIST. There are ~37 request-construction sites
// across ~16 files. A sweep that large is never finished by remembering -- the
// next plugin added is the next broken hop, and the symptom (a trace that ends
// at cleat) is invisible from inside the repo. This makes the remaining work a
// number and makes a NEW un-traced site a red test.
//
// IT SCANS TRACKED FILES, WHICH HAS A LOCAL BLIND SPOT WORTH KNOWING. git
// ls-files is used rather than a filesystem walk so a scratch worktree under
// .claude/ is not scanned as if it were this repo -- but an UNTRACKED new file
// is invisible too. Verified: adding a new outbound site in an untracked file
// reports nothing; the same file after `git add -N` is reported immediately. CI
// is unaffected, since it checks out committed code. Locally, `git add` before
// trusting a green run on a file you have just created.
//
// SITE GRANULARITY, NOT FILE. Keying by file would let one propagating call in
// a file silently cover every other call in it -- the "a key coarser than the
// thing it exempts" failure. The key is <path>:<enclosing func>.
func TestEveryOutboundCallJoinsTheTrace(t *testing.T) {
	out, err := exec.Command("git", "-C", "..", "ls-files", "*.go").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v -- this guard reasons about THE REPO, so it walks tracked "+
			"files rather than the filesystem", err)
	}
	newReq := regexp.MustCompile(`http\.NewRequest(WithContext)?\(`)
	funcDecl := regexp.MustCompile(`^func (?:\([^)]*\) )?([A-Za-z_][A-Za-z0-9_]*)`)

	var undeclared []string
	var stale []string
	seen := map[string]bool{}
	sites := 0

	for _, f := range strings.Fields(string(out)) {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Join("..", f))
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		lines := strings.Split(string(src), "\n")
		// Precompute the enclosing function for each line.
		enclosing := make([]string, len(lines))
		cur := "(package level)"
		for i, ln := range lines {
			if m := funcDecl.FindStringSubmatch(ln); m != nil {
				cur = m[1]
			}
			enclosing[i] = cur
		}
		for i, ln := range lines {
			if strings.HasPrefix(strings.TrimSpace(ln), "//") || !newReq.MatchString(ln) {
				continue
			}
			sites++
			key := f + ":" + enclosing[i]
			// Does the enclosing function propagate? Scan its body.
			if functionPropagates(lines, enclosing, i) {
				continue
			}
			// MARKED ONLY WHEN IT DOES NOT PROPAGATE, and that placement is the
			// difference between a debt list that can shrink and one that can
			// only grow. Marking every site -- which this did first -- means an
			// entry whose site gets FIXED stays valid forever, silently
			// covering the next un-traced call added to the same function.
			// Marking only unfixed sites makes a fix show up as a stale entry
			// that must be deleted.
			seen[key] = true
			if _, ok := notYetPropagating[key]; !ok {
				undeclared = append(undeclared,
					fmt.Sprintf("%s:%d  (%s)\n      key: %q", f, i+1, enclosing[i], key))
			}
		}
	}
	for key := range notYetPropagating {
		if !seen[key] {
			stale = append(stale, key)
		}
	}

	// UNMEASURED rather than a silent pass: a regex that stopped matching would
	// otherwise report a clean tree, which is this guard's own failure mode.
	if sites == 0 {
		t.Fatal("UNMEASURED: no outbound request construction found anywhere in the tree. " +
			"That is not plausible -- it is the pattern no longer matching.")
	}
	t.Logf("outbound request sites: %d checked, %d declared as not yet propagating",
		sites, len(notYetPropagating))

	sort.Strings(undeclared)
	if len(undeclared) > 0 {
		t.Errorf("%d outbound call site(s) neither propagate the caller's trace nor are declared:"+
			"\n\n    %s\n\n"+
			"Every one of these is a hop where a customer's trace ends and cleat looks like a leaf "+
			"that swallowed everything downstream (cleat#1596).\n\n"+
			"Either call plugin.SetTraceparent(req, traceID) -- the trace-id reaches a plugin on "+
			"plugin.CallContextFromContext(ctx).TraceID -- or add an entry to notYetPropagating "+
			"saying WHY it does not, which is a debt rather than a grant.",
			len(undeclared), strings.Join(undeclared, "\n    "))
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Errorf("%d declared site(s) no longer need declaring:\n\n    %s\n\n"+
			"Either it now PROPAGATES -- delete the entry, the debt is paid -- or the call moved "+
			"or was removed, in which case re-key or delete it. A stale entry silently covers "+
			"whatever arrives at that name next.",
			len(stale), strings.Join(stale, "\n    "))
	}
}

// functionPropagates reports whether the function enclosing line idx mentions
// SetTraceparent anywhere in its body.
func functionPropagates(lines, enclosing []string, idx int) bool {
	fn := enclosing[idx]
	for i := range lines {
		if enclosing[i] == fn && strings.Contains(lines[i], "SetTraceparent") {
			return true
		}
	}
	return false
}
