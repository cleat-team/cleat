package plugin

import (
	"fmt"
	"go/parser"
	"go/token"
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
// needsOrigination marks a site with NO caller trace to join and NO decision yet
// about whether one should be manufactured. cleat#1611.
//
// Shrinking: cleat#1611 answered it for the two sweeps that had a clear unit of
// work (a due webhook delivery, a tenant statistics export) and answered it
// NEGATIVELY for the kafka-connect consumer, which has its own entry saying why.
// What is left under this constant is work nobody has priced.
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
	"cleat/embedded/runner.go:handleHTTPFetch": "cleat#1596 stage 3, and it needs one " +
		"thing the plugin sites do not: the EMBEDDED runner has no inbound request, so it has " +
		"no trace to continue. Sibling of the worker's handleHTTPFetch, which this PR fixes, " +
		"but the fix there is origination rather than propagation -- the scheduled-run case.",
	"plugins/kafkaconnect/background.go:consumeViaRestProxy": "the kafka-connect consumer polls every FIVE SECONDS per enabled config, whether or not " +
		"records exist, so originating here would produce a trace per empty poll per config per " +
		"worker -- continuously. That is the flood WithNewTrace's contract forbids, and it buries " +
		"the traces that mean something. cleat#1611 decided AGAINST tracing it: the consume's three " +
		"HTTP calls are infrastructure chatter rather than a unit of business work, and the unit " +
		"worth tracing is what happens when records actually arrive -- which is downstream of here. " +
		"Compare plugins/datadogexport, which DOES originate: one real export per config per 60s.",
	"plugins/kafkaconnect/background.go:createConsumer": "the kafka-connect consumer polls every FIVE SECONDS per enabled config, whether or not " +
		"records exist, so originating here would produce a trace per empty poll per config per " +
		"worker -- continuously. That is the flood WithNewTrace's contract forbids, and it buries " +
		"the traces that mean something. cleat#1611 decided AGAINST tracing it: the consume's three " +
		"HTTP calls are infrastructure chatter rather than a unit of business work, and the unit " +
		"worth tracing is what happens when records actually arrive -- which is downstream of here. " +
		"Compare plugins/datadogexport, which DOES originate: one real export per config per 60s.",
	"plugins/kafkaconnect/background.go:pollRecords": "the kafka-connect consumer polls every FIVE SECONDS per enabled config, whether or not " +
		"records exist, so originating here would produce a trace per empty poll per config per " +
		"worker -- continuously. That is the flood WithNewTrace's contract forbids, and it buries " +
		"the traces that mean something. cleat#1611 decided AGAINST tracing it: the consume's three " +
		"HTTP calls are infrastructure chatter rather than a unit of business work, and the unit " +
		"worth tracing is what happens when records actually arrive -- which is downstream of here. " +
		"Compare plugins/datadogexport, which DOES originate: one real export per config per 60s.",
	"plugins/kafkaconnect/background.go:subscribeConsumer": "the kafka-connect consumer polls every FIVE SECONDS per enabled config, whether or not " +
		"records exist, so originating here would produce a trace per empty poll per config per " +
		"worker -- continuously. That is the flood WithNewTrace's contract forbids, and it buries " +
		"the traces that mean something. cleat#1611 decided AGAINST tracing it: the consume's three " +
		"HTTP calls are infrastructure chatter rather than a unit of business work, and the unit " +
		"worth tracing is what happens when records actually arrive -- which is downstream of here. " +
		"Compare plugins/datadogexport, which DOES originate: one real export per config per 60s.",
	"plugins/oauthprovider/routes.go:handleCallback": "an inbound HTTP HANDLER, not a workflow step. The trace it should join belongs to the " +
		"browser or IdP that called it and arrives on the INBOUND request -- not to any run, and " +
		"there is no CallContext here. Joining it means parsing the incoming traceparent on the " +
		"plugin mux the way cmd/cleat-worker does on its own routes: a third mechanism, not this " +
		"issue's propagation.",
	"plugins/oauthprovider/oidc.go:getJSON": "OIDC discovery and JWKS fetches (cleat#1582), reached ONLY from handleLogin and " +
		"handleCallback -- so this is the same debt as the entry above it, for the same reason, " +
		"and it should be paid at the same time by the same mechanism. Checked rather than " +
		"inherited: the only production site that sets a CallContext is execSession." +
		"pluginCallContext (engine/plugin_call_context.go:72), which is a workflow host-call " +
		"path, so plugin.CallContextFromContext returns nil on every HTTP handler and there is " +
		"no TraceID to propagate. Fixing the sibling fixes this without touching oidc.go.",
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
		blanked, err := withoutComments(f, src)
		if err != nil {
			// NOT a skip. A tracked .go file that does not parse is
			// UNMEASURED, and an unmeasured file reads exactly like one with
			// no outbound calls in it. All 520 non-test sources parse today,
			// so this firing means something is genuinely wrong.
			t.Errorf("%s: does not parse, so its outbound calls were not checked: %v", f, err)
			continue
		}
		lines := strings.Split(string(blanked), "\n")
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

// withoutComments returns src with every comment's text replaced by spaces,
// byte offsets and line numbers untouched.
//
// WHY, AND IT IS THE WHOLE OF THIS CHANGE. functionPropagates below asks
// whether the enclosing function MENTIONS SetTraceparent. A mention in a
// comment is a mention. Measured on develop by commenting out the call at
// cmd/cleat-worker/setup.go:228 and leaving its name behind:
//
//   - plugin.SetTraceparent(req, c.traceID)
//
//   - // sabotage: plugin.SetTraceparent(req, c.traceID)
//
//     ok  github.com/cleat-team/cleat/plugin  0.312s
//
// The tree still builds, and an outbound request that no longer carries the
// caller's trace passes a test called "every outbound call joins the trace".
// The negative control is what makes that a blind spot rather than a broken
// guard: DELETING the same line fails it correctly. It is blind to exactly the
// form a developer produces -- a call disabled in place, or a name left in a
// doc comment after a refactor. Two comments in that same file already mention
// SetTraceparent, so the masking text was already there. (cleat#1768.)
//
// Blanking rather than deleting, because every line index in this guard -- the
// enclosing-function table, the reported line number, the debt-list key -- is
// an index into the ORIGINAL file. A comment stripper that shortens anything
// silently re-points all of them.
//
// It also fixes the over-reporting direction, which nobody had hit: the site
// scan skips a line whose FIRST non-space is "//", so a trailing
// "// http.NewRequest(" would have been counted as a real call site.
func withoutComments(path string, src []byte) ([]byte, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, parser.ParseComments)
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(src))
	copy(out, src)
	base := fset.File(file.Pos()).Base()
	for _, group := range file.Comments {
		lo := int(group.Pos()) - base
		hi := int(group.End()) - base
		if lo < 0 || hi > len(out) || lo > hi {
			continue
		}
		for i := lo; i < hi; i++ {
			if out[i] != '\n' {
				out[i] = ' '
			}
		}
	}
	return out, nil
}

// functionPropagates reports whether the function enclosing line idx mentions
// SetTraceparent anywhere in its body.
//
// Still a mention rather than a call, deliberately: propagation reaches a
// request through helpers and wrappers here, and requiring the CallExpr in the
// same function would report sites that are correct. What it is no longer
// satisfied by is a mention in a COMMENT -- withoutComments above blanks those
// before this ever sees them.
func functionPropagates(lines, enclosing []string, idx int) bool {
	fn := enclosing[idx]
	for i := range lines {
		if enclosing[i] == fn && strings.Contains(lines[i], "SetTraceparent") {
			return true
		}
	}
	return false
}

// TestTheTraceScanIsNotSatisfiedByAComment is the known-positive this guard did
// not have.
//
// Every outbound site in the tree either propagates or is declared, so a run
// over the repo is green whether functionPropagates is right, wrong, or
// deleted. That is how a substring match survived: nothing ever handed it a
// case that should fail. These fixtures do.
//
// It drives withoutComments and functionPropagates together, over the same
// enclosing-function table the real scan builds, because asserting on
// withoutComments alone would leave the JOIN between them untested -- and the
// join is where the defect lived.
func TestTheTraceScanIsNotSatisfiedByAComment(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want bool // does the function enclosing the NewRequest site propagate?
	}{{
		name: "a real call propagates",
		src: "package p\n\nfunc send() {\n\treq, _ := http.NewRequest(\"GET\", u, nil)\n" +
			"\tplugin.SetTraceparent(req, id)\n}\n",
		want: true,
	}, {
		name: "the call commented out, its name left behind -- the defect",
		src: "package p\n\nfunc send() {\n\treq, _ := http.NewRequest(\"GET\", u, nil)\n" +
			"\t// plugin.SetTraceparent(req, id)\n}\n",
		want: false,
	}, {
		name: "a doc comment mentioning it, above a function that does not call it",
		src: "package p\n\n// send would SetTraceparent if it had a trace to join.\n" +
			"func send() {\n\treq, _ := http.NewRequest(\"GET\", u, nil)\n}\n",
		want: false,
	}, {
		name: "a trailing comment on the request line itself",
		src:  "package p\n\nfunc send() {\n\treq, _ := http.NewRequest(\"GET\", u, nil) // no SetTraceparent yet\n}\n",
		want: false,
	}, {
		name: "a block comment",
		src: "package p\n\nfunc send() {\n\treq, _ := http.NewRequest(\"GET\", u, nil)\n" +
			"\t/* plugin.SetTraceparent(req, id) */\n}\n",
		want: false,
	}, {
		name: "the neighbour must not vouch: another function calls it",
		src: "package p\n\nfunc other() {\n\tplugin.SetTraceparent(req, id)\n}\n\n" +
			"func send() {\n\treq, _ := http.NewRequest(\"GET\", u, nil)\n}\n",
		want: false,
	}}

	newReq := regexp.MustCompile(`http\.NewRequest(WithContext)?\(`)
	funcDecl := regexp.MustCompile(`^func (?:\([^)]*\) )?([A-Za-z_][A-Za-z0-9_]*)`)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			blanked, err := withoutComments("fixture.go", []byte(tc.src))
			if err != nil {
				t.Fatalf("the fixture does not parse, so this case asserts nothing: %v", err)
			}
			// Line numbers must survive the blanking, or every key this guard
			// reports points at the wrong line.
			if got, want := strings.Count(string(blanked), "\n"), strings.Count(tc.src, "\n"); got != want {
				t.Fatalf("blanking changed the line count: %d, want %d", got, want)
			}

			lines := strings.Split(string(blanked), "\n")
			enclosing := make([]string, len(lines))
			cur := "(package level)"
			for i, ln := range lines {
				if m := funcDecl.FindStringSubmatch(ln); m != nil {
					cur = m[1]
				}
				enclosing[i] = cur
			}
			idx := -1
			for i, ln := range lines {
				if strings.HasPrefix(strings.TrimSpace(ln), "//") || !newReq.MatchString(ln) {
					continue
				}
				idx = i
			}
			if idx < 0 {
				t.Fatalf("UNMEASURED: no outbound request site found in the fixture, so "+
					"nothing was asked about it.\n%s", string(blanked))
			}
			if got := functionPropagates(lines, enclosing, idx); got != tc.want {
				t.Errorf("functionPropagates = %v, want %v, for the site in %q",
					got, tc.want, enclosing[idx])
			}
		})
	}
}
