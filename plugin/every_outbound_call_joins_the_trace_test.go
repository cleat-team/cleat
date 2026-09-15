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
// needsOrigination marks a site with NO caller trace to join, because nothing
// upstream of it ever had one. cleat#1596 stage 3.
//
// A different problem from propagation, and the distinction is why stage 2
// stopped where it did. A plugin host function runs inside a workflow step, so
// a trace exists and the fix is one line. A BACKGROUND SWEEP runs on a timer
// with no inbound request and no CallContext -- nothing to continue, so a trace
// must be MANUFACTURED, which raises a question nobody has answered: is each
// sweep iteration one trace, each delivery one, or the loop itself a single
// long-lived one? Guessing produces traces worse than none, because they look
// authoritative.
const needsOrigination = "cleat#1596 stage 3: a background sweep with no inbound request and no " +
	"CallContext, so there is no caller trace to join. Needs a trace ORIGINATED, which is a design " +
	"question (one trace per iteration? per delivery? per loop?) rather than a one-line fix."

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
	"plugins/notifications/background.go:deliver": needsOrigination,
	"cleat/embedded/runner.go:handleHTTPFetch": "cleat#1596 stage 3, and it needs one " +
		"thing the plugin sites do not: the EMBEDDED runner has no inbound request, so it has " +
		"no trace to continue. Sibling of the worker's handleHTTPFetch, which this PR fixes, " +
		"but the fix there is origination rather than propagation -- the scheduled-run case.",
	"plugins/datadogexport/background.go:exportForConfig":    needsOrigination,
	"plugins/kafkaconnect/background.go:consumeViaRestProxy": needsOrigination,
	"plugins/kafkaconnect/background.go:createConsumer":      needsOrigination,
	"plugins/kafkaconnect/background.go:pollRecords":         needsOrigination,
	"plugins/kafkaconnect/background.go:subscribeConsumer":   needsOrigination,
	"plugins/oauthprovider/routes.go:handleCallback": "an inbound HTTP HANDLER, not a workflow step. The trace it should join belongs to the " +
		"browser or IdP that called it and arrives on the INBOUND request -- not to any run, and " +
		"there is no CallContext here. Joining it means parsing the incoming traceparent on the " +
		"plugin mux the way cmd/cleat-worker does on its own routes: a third mechanism, not this " +
		"issue's propagation.",
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
