package plugins_test

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The transport every plugin's client must be built with. A selector, so
// `env.HTTPTransport` and `p.env.HTTPTransport` both satisfy it and a bare
// `&http.Transport{}` does not.
const guardedTransport = "HTTPTransport"

// egressFinding is one http.Client literal that does not route through the
// guard, with enough position to go and look at it.
type egressFinding struct {
	Pos     string
	Problem string
}

// surveyEgress reports every http.Client composite literal in one file whose
// Transport is not the guarded one, and how many it examined.
//
// AST, NOT TEXT, and that is the whole point of this file. The assertion this
// replaced was
//
//	strings.Contains(src[loc[1]:end], "Transport:")
//
// over a 400-byte window after `&http.Client{`. Any key named Transport
// satisfies it, so a plugin written
//
//	&http.Client{Transport: &http.Transport{}, Timeout: 10 * time.Second}
//
// -- open to the loopback, link-local and RFC1918 destinations the guard exists
// to refuse -- PASSED, while the failure message the test would have printed
// names env.HTTPTransport, an identifier the assertion never looked for
// (cleat#1768, measured on a staged plugin before this change).
//
// The window is gone with it. It existed only because a text scan cannot tell
// which literal it is standing in, so it guessed with a byte count; a parsed
// literal knows its own elements and a Transport on the next client cannot
// vouch for this one.
//
// WHAT IT STILL CANNOT SEE, stated rather than left for the next reader to
// discover. It proves a client is CONSTRUCTED with the guarded transport, not
// that every request uses that client:
//
//   - a client assembled field by field (`var c http.Client; c.Transport = ...`)
//     is not a composite literal and is not examined;
//   - a transport reached through a local variable (`tr := env.HTTPTransport`)
//     is reported, because the selector is not at the literal. No plugin does
//     either today -- all eight guarded clients name env.HTTPTransport inline --
//     so the strictness costs nothing now and would have to be revisited
//     deliberately rather than by loosening this back to a substring.
func surveyEgress(fset *token.FileSet, path string, src []byte) (findings []egressFinding, clients int, err error) {
	file, err := parser.ParseFile(fset, path, src, 0)
	if err != nil {
		return nil, 0, err
	}
	ast.Inspect(file, func(nd ast.Node) bool {
		lit, ok := nd.(*ast.CompositeLit)
		if !ok {
			return true
		}
		// Matches `&http.Client{...}` and `http.Client{...}` alike: the
		// address-of is a UnaryExpr wrapped AROUND this node, so it is not
		// ours to look at. The regex this replaced required the `&` and
		// would have missed the other form.
		sel, ok := lit.Type.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Client" {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "http" {
			return true
		}
		clients++

		var transport ast.Expr
		for _, el := range lit.Elts {
			kv, ok := el.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "Transport" {
				transport = kv.Value
			}
		}
		pos := fset.Position(lit.Pos()).String()
		switch t := transport.(type) {
		case nil:
			findings = append(findings, egressFinding{pos, "builds an http.Client with no Transport at all"})
		case *ast.SelectorExpr:
			if t.Sel.Name != guardedTransport {
				findings = append(findings, egressFinding{pos,
					fmt.Sprintf("sets Transport to %s", render(fset, transport))})
			}
		case *ast.UnaryExpr:
			// THE SECOND SPELLING OF GUARDED, and it is not a loosening.
			//
			// A plugin receives a ready-made env.HTTPTransport. The worker
			// builds its own, because its guard is per tenant and constructed
			// per call: `&http.Transport{DialContext: c.egressGuard(ctx).DialContext}`.
			// Both end at engine.EgressGuard.DialContext; only one names a field.
			//
			// So this accepts a composite &http.Transport{...} ONLY when its
			// DialContext is a call to something named egressGuard. An
			// &http.Transport{} with no DialContext, or with any other dialer,
			// still falls through to a finding -- which is the case that
			// mattered here, since the bench-svc forwarder held exactly that.
			if !dialsThroughEgressGuard(t) {
				findings = append(findings, egressFinding{pos,
					fmt.Sprintf("sets Transport to %s", render(fset, transport))})
			}
		default:
			findings = append(findings, egressFinding{pos,
				fmt.Sprintf("sets Transport to %s", render(fset, transport))})
		}
		return true
	})
	return findings, clients, nil
}

// dialsThroughEgressGuard reports whether an `&http.Transport{...}` literal sets
// DialContext to a selector on a call to something named egressGuard -- the
// worker's spelling of "guarded", as opposed to a plugin's env.HTTPTransport.
//
// Deliberately shallow. It matches the shape `X.egressGuard(...).DialContext`
// and nothing else: a transport with no DialContext, or one dialing anything
// else, is a finding. A helper that wrapped the guard under another name would
// be reported, and should be -- the alternative is a matcher that accepts any
// DialContext at all, which is the substring check this file replaced.
func dialsThroughEgressGuard(t *ast.UnaryExpr) bool {
	lit, ok := t.X.(*ast.CompositeLit)
	if !ok {
		return false
	}
	if sel, ok := lit.Type.(*ast.SelectorExpr); !ok || sel.Sel.Name != "Transport" {
		return false
	}
	for _, el := range lit.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok || key.Name != "DialContext" {
			continue
		}
		outer, ok := kv.Value.(*ast.SelectorExpr)
		if !ok || outer.Sel.Name != "DialContext" {
			return false
		}
		call, ok := outer.X.(*ast.CallExpr)
		if !ok {
			return false
		}
		fn, ok := call.Fun.(*ast.SelectorExpr)
		// TWO NAMES, because the worker has two policies and they are not
		// interchangeable: egressGuard is for a destination a GUEST named and
		// consults the per-tenant allowlist; serviceEgressGuard is for one the
		// OPERATOR named and does not. Both end at EgressGuard.DialContext.
		//
		// An allowlist of exact names rather than a suffix match, so a helper
		// called anythingEgressGuard does not qualify by being spelled well.
		return ok && (fn.Sel.Name == "egressGuard" || fn.Sel.Name == "serviceEgressGuard")
	}
	return false
}

func render(fset *token.FileSet, e ast.Expr) string {
	var b bytes.Buffer
	if err := printer.Fprint(&b, fset, e); err != nil {
		return "<unprintable>"
	}
	return b.String()
}

// cleat#1565 open question 4. Every plugin's outbound HTTP goes through the
// egress-guarded transport the worker supplies.
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

	repo, files := pluginFiles(t)
	fset := token.NewFileSet()
	examined, clients := 0, 0

	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		// FROM DISK, not from `git show HEAD:`. git ls-files supplies the
		// LIST -- which is what keeps scratch worktrees out of scope -- and
		// the bytes come from the working tree. Reading HEAD is exactly
		// backwards: it passes a change that ADDS an unguarded client and
		// fails the change that fixes one. cmd/cleat/documented_cli_surface_test.go
		// records the same mistake.
		body, err := os.ReadFile(filepath.Join(repo, f))
		if err != nil {
			t.Errorf("%s: listed by git ls-files and unreadable: %v", f, err)
			continue
		}
		findings, n, err := surveyEgress(fset, f, body)
		if err != nil {
			// NOT a skip. A file that does not parse is UNMEASURED, and an
			// unmeasured plugin reads exactly like a guarded one -- which is
			// the defect this whole test exists to refuse.
			t.Errorf("%s: does not parse, so it was not surveyed: %v", f, err)
			continue
		}
		if n == 0 {
			continue
		}
		examined++
		clients += n

		if why, ok := exempt[f]; ok {
			if !strings.Contains(string(body), "cleat#1565") {
				t.Errorf("%s is exempt (%s) but says nothing about why at the site; "+
					"an unexplained exemption is indistinguishable from an oversight", f, why)
			}
			continue
		}
		for _, fd := range findings {
			t.Errorf("%s: %s.\nPlugin egress must go through env.HTTPTransport, or the "+
				"loopback/link-local/RFC1918 floor does not apply to it (cleat#1565). "+
				"If it genuinely must be unguarded, exempt it by name in this test with "+
				"the reason.", fd.Pos, fd.Problem)
		}
	}

	if examined == 0 || clients == 0 {
		t.Fatalf("examined %d files and found %d clients; the scan measured nothing, "+
			"which reads identically to a clean tree", examined, clients)
	}
	// Every exemption must still match something, or it is a grant covering
	// code that no longer exists.
	listed := " " + strings.Join(files, " ") + " "
	for f := range exempt {
		if !strings.Contains(listed, " "+f+" ") {
			t.Errorf("exemption for %q matches no tracked file; delete it", f)
		}
	}
}

// pluginFiles is the tracked plugin sources, and the repo root they are
// relative to.
//
// -C root: git ls-files run from plugins/ lists only plugins/ and prints paths
// relative to it, so every later read fails and the scan measures nothing. The
// floor below caught that, which is the only reason this comment exists rather
// than a silent pass.
func pluginFiles(t *testing.T) (string, []string) {
	t.Helper()
	root, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("git rev-parse: %v", err)
	}
	repo := strings.TrimSpace(string(root))
	// cmd/cleat-worker IS IN SCOPE, and leaving it out is what this scan missed.
	//
	// The survey was named for plugins and scanned only plugins/, so the worker's
	// own outbound clients were never examined -- and one of them,
	// forwardToBenchSvc's package-level http.Client, held a bare
	// &http.Transport{} and dialed wherever it was pointed. The single outbound
	// path that reaches an operator-named service was the single path outside the
	// egress floor, and the guard written to catch exactly that was looking one
	// directory away.
	//
	// The boundary that matters is "code in this repo that dials out on a
	// workflow's behalf", not "code under plugins/".
	out, err := exec.Command("git", "-C", repo, "ls-files",
		"plugins/*.go", "plugins/**/*.go",
		"cmd/cleat-worker/*.go").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	var files []string
	for _, f := range strings.Fields(string(out)) {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		files = append(files, f)
	}
	if len(files) < 50 {
		t.Fatalf("git ls-files matched %d non-test files under plugins/ and cmd/cleat-worker/; "+
			"the scan did not see the tree", len(files))
	}
	return repo, files
}

// TestTheEgressScannerReportsAnUnguardedTransport is the known-positive, and it
// is the half this test did not have.
//
// The survey above cannot be exercised by the tree: every plugin is guarded, so
// a run over the repo is green whether the assertion is right, wrong, or
// deleted. That is precisely how a substring check for a key named "Transport"
// survived beside a failure message naming env.HTTPTransport -- nothing in the
// suite ever handed it something that should fail.
//
// Fixtures rather than a staged plugin: the survey takes a path and bytes, so
// the cases run without touching the index. Worth knowing if you reach for a
// staged file instead -- the real test lists through git ls-files, so an
// UNSTAGED fixture is invisible to it and the run is green for the wrong
// reason.
func TestTheEgressScannerReportsAnUnguardedTransport(t *testing.T) {
	const preamble = "package p\n\nimport \"net/http\"\n\nvar env struct{ HTTPTransport http.RoundTripper }\n\n"

	cases := []struct {
		name    string
		body    string
		clients int
		want    string // substring of the reported problem; "" means no finding
	}{{
		name:    "the guarded form",
		body:    "var c = &http.Client{Transport: env.HTTPTransport}",
		clients: 1,
		want:    "",
	}, {
		name:    "a bare transport, which the substring check passed",
		body:    "var c = &http.Client{Transport: &http.Transport{}}",
		clients: 1,
		want:    "sets Transport to &http.Transport{}",
	}, {
		name:    "no Transport at all",
		body:    "var c = &http.Client{}",
		clients: 1,
		want:    "no Transport at all",
	}, {
		name:    "the default transport, named",
		body:    "var c = &http.Client{Transport: http.DefaultTransport}",
		clients: 1,
		want:    "sets Transport to http.DefaultTransport",
	}, {
		name:    "without the address-of, which the regex required",
		body:    "var c = http.Client{Transport: &http.Transport{}}",
		clients: 1,
		want:    "sets Transport to &http.Transport{}",
	}, {
		name: "a guarded client beside an unguarded one -- the neighbour must not vouch",
		body: "var a = &http.Client{Transport: env.HTTPTransport}\n" +
			"var b = &http.Client{Transport: &http.Transport{}}",
		clients: 2,
		want:    "sets Transport to &http.Transport{}",
	}, {
		name:    "not an http.Client at all",
		body:    "type Client struct{ Transport int }\n\nvar c = &Client{Transport: 1}",
		clients: 0,
		want:    "",
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fset := token.NewFileSet()
			findings, clients, err := surveyEgress(fset, "fixture.go", []byte(preamble+tc.body+"\n"))
			if err != nil {
				t.Fatalf("the fixture does not parse, so this case asserts nothing: %v", err)
			}
			if clients != tc.clients {
				t.Errorf("examined %d http.Client literals, want %d", clients, tc.clients)
			}
			got := problems(findings)
			if tc.want == "" {
				if len(findings) != 0 {
					t.Errorf("reported %v; a guarded client must not be reported", got)
				}
				return
			}
			if len(findings) == 0 {
				t.Fatalf("reported nothing. This case is the defect itself: the check " +
					"that shipped looked for a key named Transport, which every one of " +
					"these cases supplies.")
			}
			if !strings.Contains(strings.Join(got, " | "), tc.want) {
				t.Errorf("reported %v, which does not mention %q. Asserting on the TEXT "+
					"rather than on the count: a finding of the wrong kind satisfies "+
					"\"exactly one finding\" and says nothing.", got, tc.want)
			}
		})
	}
}

func problems(fs []egressFinding) []string {
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, f.Problem)
	}
	sort.Strings(out)
	return out
}
