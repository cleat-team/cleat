package prometheus

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// unfedMetrics names every exported *Metrics method with no production caller,
// and says what each one is waiting for.
//
// WHY AN ABSENT GAUGE IS WORSE THAN A ZERO ONE. An OTel gauge that is never
// Set emits no series at all, so an alert on it never fires and a dashboard
// panel reads "No data" rather than a stale value. Absence in Prometheus is
// not falsy -- silence is indistinguishable from healthy, which is the
// property this repo keeps paying for elsewhere. A counter that is never Added
// at least reads 0 (cleat#1317).
//
// THIS IS AN INVENTORY OF MISSING CALL SITES, NOT A LIST OF THINGS TO DELETE.
// Several of these are the right metric for a real condition and what is
// missing is the feeder: SetWorkflowsStuck needs a sweep to compute and publish
// it. Deleting them would remove the design intent along with the defect. Each
// entry therefore says which it is.
var unfedMetrics = map[string]string{
	// The gauges. The four fed by metricsSweepLoop since cleat#1317 are gone
	// from this list, and so is SetMemoryPressureRatio, which was a duplicate
	// of SetMemoryPressure rather than an unfed metric. What remains is the
	// WASM cache pair, waiting on an accessor rather than on a caller --
	// WasmDiskCache exposes neither its length nor its size.

	// The counters and histograms. Less severe -- a counter never Added reads
	// 0 rather than vanishing -- but still an instrument nothing writes.
	"RecordWasmCompileDuration": "NEEDS A CALL SITE, and did not used to -- it " +
		"was fed from the LOAD path, which compiles nothing, so it carried " +
		"storage latency under a compile name (cleat#1317). Real compilation is " +
		"Runtime.CompileModule in engine/. NOT blocked on a Metrics handle -- " +
		"engine already imports monitoring/prometheus and PostgresStore already " +
		"holds one; RecordEncryptionError was wired that way. What is missing is " +
		"a compile SITE with a receiver that has one. Deliberately left unfed " +
		"rather than fed from the wrong measurement: an absent histogram is " +
		"honest, a confident wrong one invites action",
}

// TestEveryMetricHasAFeeder.
//
// cleat#1021's shape one layer out: there it was store methods with no
// production caller, where tests calling them heavily made the surface look
// covered. Same here -- each of these has a wrapper, a registration, a
// description string, and in several cases a test. Everything except the call.
func TestEveryMetricHasAFeeder(t *testing.T) {
	methods := exportedMetricsMethods(t)
	// A FLOOR ON THE INPUT, not on the answer. If the parse breaks or the file
	// moves, the method set is empty, every allowlist entry reads as stale and
	// the guard reports a tidy list of things to delete -- blaming the
	// allowlist rather than the reader. 50, against 68 measured 2026-09-13.
	if len(methods) < 50 {
		t.Fatalf("found only %d exported *Metrics methods: the extraction is "+
			"broken, not the allowlist (68 on 2026-09-13)", len(methods))
	}

	called := callersOutsideOwnBody(t, methods)

	var newlyUnfed, nowFed []string
	for name := range methods {
		if !called[name] {
			if _, known := unfedMetrics[name]; !known {
				newlyUnfed = append(newlyUnfed, name)
			}
		}
	}
	for name := range unfedMetrics {
		if _, ok := methods[name]; !ok {
			nowFed = append(nowFed, name+" (no longer a method)")
			continue
		}
		if called[name] {
			nowFed = append(nowFed, name)
		}
	}
	sort.Strings(newlyUnfed)
	sort.Strings(nowFed)

	if len(newlyUnfed) > 0 {
		t.Errorf("these exported *Metrics methods have no production caller and are "+
			"not declared unfed:\n  %s\n\n"+
			"An OTel gauge that is never Set emits NO SERIES, so an alert on it can "+
			"never fire and a panel reads \"No data\" rather than a stale value. Wire a "+
			"call site, or add an entry saying what it is waiting for. cleat#1317.",
			strings.Join(newlyUnfed, "\n  "))
	}
	if len(nowFed) > 0 {
		t.Errorf("these are declared unfed but now HAVE a production caller (or are "+
			"gone):\n  %s\n\nDelete the entries -- this is the direction the list is "+
			"meant to move, and a stale one covers whatever arrives at that name next.",
			strings.Join(nowFed, "\n  "))
	}
}

// exportedMetricsMethods maps each exported *Metrics method to the byte range
// of its own declaration, parsed with go/ast rather than matched with a regex
// so that a brace inside a string or comment cannot end a function early.
func exportedMetricsMethods(t *testing.T) map[string][2]int {
	t.Helper()
	const path = "metrics.go"
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	out := map[string][2]int{}
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || len(fn.Recv.List) != 1 || !fn.Name.IsExported() {
			continue
		}
		star, ok := fn.Recv.List[0].Type.(*ast.StarExpr)
		if !ok {
			continue
		}
		id, ok := star.X.(*ast.Ident)
		if !ok || id.Name != "Metrics" {
			continue
		}
		out[fn.Name.Name] = [2]int{fset.Position(fn.Pos()).Offset, fset.Position(fn.End()).Offset}
	}
	return out
}

// callersOutsideOwnBody reports which methods are called from tracked non-test
// Go outside their own declaration.
//
// THE EXCLUSION IS THE METHOD'S OWN BODY, NOT ITS FILE, and the difference is
// not pedantic: excluding metrics.go wholesale discards SIBLING DELEGATION --
// one exported method calling another -- which is a real path to production. A
// file-level exclusion would report a delegated method as unfed. cleat#1317
// records that exact error being made and corrected.
func callersOutsideOwnBody(t *testing.T, methods map[string][2]int) map[string]bool {
	t.Helper()
	out, err := exec.Command("git", "ls-files", "../..").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	var files []string
	for _, f := range strings.Fields(string(out)) {
		if strings.HasSuffix(f, ".go") && !strings.HasSuffix(f, "_test.go") {
			files = append(files, f)
		}
	}
	if len(files) == 0 {
		t.Fatal("no Go sources found: the scan would pass vacuously")
	}

	self := "metrics.go"
	selfSrc, err := os.ReadFile(self)
	if err != nil {
		t.Fatalf("read %s: %v", self, err)
	}

	// Read and blank each file ONCE, before the per-method loop. It used to
	// re-read every source for every method -- 46 methods x 520 files -- which
	// was tolerable only because the read was the whole cost. Parsing is not.
	sources := make(map[string][]byte, len(files))
	for _, f := range files {
		if strings.HasSuffix(f, "/monitoring/prometheus/metrics.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Errorf("%s: listed by git ls-files and unreadable: %v", f, err)
			continue
		}
		blanked, err := codeWithoutComments(f, b)
		if err != nil {
			// NOT a skip, which is what this was. A file that does not parse
			// is UNMEASURED, and an unmeasured file reads exactly like one
			// with no call in it -- which for this guard means "this metric
			// has no feeder", the finding it exists to report. All 520 tracked
			// non-test sources parse today.
			t.Errorf("%s: does not parse, so its calls were not counted: %v", f, err)
			continue
		}
		sources[f] = blanked
	}

	selfBlanked, err := codeWithoutComments(self, selfSrc)
	if err != nil {
		t.Fatalf("parse %s: %v", self, err)
	}

	called := map[string]bool{}
	for name, span := range methods {
		pat := regexp.MustCompile(`\.` + regexp.QuoteMeta(name) + `\s*\(`)
		// metrics.go with only THIS method's body removed. Offsets come from
		// the ORIGINAL source, which is why codeWithoutComments preserves them
		// byte for byte rather than deleting anything.
		trimmed := append(append([]byte{}, selfBlanked[:span[0]]...), selfBlanked[span[1]:]...)
		if pat.Match(trimmed) {
			called[name] = true
			continue
		}
		for _, b := range sources {
			if pat.Match(b) {
				called[name] = true
				break
			}
		}
	}
	return called
}

// codeWithoutComments returns src with every comment's text replaced by spaces,
// byte offsets and line numbers untouched.
//
// WHY. The scan above is `\.<Name>\s*\(` over raw source, and a COMMENTED-OUT
// call matches it. Measured on develop by commenting out the only production
// call site of AddCompactionEventsDeleted:
//
//	// engine/compaction.go:431
//	-		metrics.AddCompactionEventsDeleted(ctx, int64(compactedStep))
//	+		// sabotage: metrics.AddCompactionEventsDeleted(ctx, int64(compactedStep))
//
//	ok  github.com/cleat-team/cleat/monitoring/prometheus  0.283s
//
// A gauge with no feeder, reported as fed, by the guard whose own failure text
// says an unfed OTel gauge emits no series so an alert can never fire. The
// negative control is what makes that a blind spot rather than a broken guard:
// DELETING the same line fails it correctly. It is blind to exactly the form a
// developer produces -- a call disabled in place, or a name left in a doc
// comment after a refactor. (cleat#1768; the same shape as #1771 and #1772.)
//
// BLANKING, NOT DELETING, because exportedMetricsMethods hands back byte
// offsets into the original metrics.go and the sibling-delegation check slices
// with them. A stripper that shortens anything silently re-points every span.
//
// cmd/cleat-worker/route_table_test.go's stripGoComments does the same job with
// a hand-written state machine, and was already immune to this. Using the
// parser here rather than copying it: it is in another package, and a parser
// cannot disagree with the compiler about where a comment ends.
func codeWithoutComments(path string, src []byte) ([]byte, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.ParseComments)
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(src))
	copy(out, src)
	base := fset.File(f.Pos()).Base()
	for _, group := range f.Comments {
		lo, hi := int(group.Pos())-base, int(group.End())-base
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

// TestTheFeederScanIsNotSatisfiedByAComment is the known-positive this guard
// did not have.
//
// Every metric in the tree is either fed or declared unfed, so a run over the
// repo is green whether the call scan is right, wrong, or deleted. That is how
// a raw-source regex survived here. These fixtures fail without it.
func TestTheFeederScanIsNotSatisfiedByAComment(t *testing.T) {
	pat := regexp.MustCompile(`\.` + regexp.QuoteMeta("AddThing") + `\s*\(`)

	cases := []struct {
		name string
		src  string
		want bool // does the file contain a real call?
	}{{
		name: "a real call",
		src:  "package p\n\nfunc f() {\n\tm.AddThing(ctx, 1)\n}\n",
		want: true,
	}, {
		name: "the call commented out, its name left behind -- the defect",
		src:  "package p\n\nfunc f() {\n\t// m.AddThing(ctx, 1)\n}\n",
		want: false,
	}, {
		name: "a doc comment naming it",
		src:  "package p\n\n// f used to call m.AddThing(ctx, 1) before the sweep moved.\nfunc f() {}\n",
		want: false,
	}, {
		name: "a block comment",
		src:  "package p\n\nfunc f() {\n\t/* m.AddThing(ctx, 1) */\n}\n",
		want: false,
	}, {
		name: "a trailing comment beside an unrelated statement",
		src:  "package p\n\nfunc f() {\n\tg() // TODO: m.AddThing(ctx, 1)\n}\n",
		want: false,
	}, {
		name: "the name inside a STRING is still a match, and that is correct",
		src:  "package p\n\nfunc f() {\n\ts := \"m.AddThing(\"\n\t_ = s\n}\n",
		want: true,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			blanked, err := codeWithoutComments("fixture.go", []byte(tc.src))
			if err != nil {
				t.Fatalf("the fixture does not parse, so this case asserts nothing: %v", err)
			}
			// Offsets must survive, or the sibling-delegation slice above cuts
			// metrics.go in the wrong place.
			if len(blanked) != len(tc.src) {
				t.Fatalf("blanking changed the length: %d, want %d", len(blanked), len(tc.src))
			}
			if got := pat.Match(blanked); got != tc.want {
				t.Errorf("call found = %v, want %v, in:\n%s", got, tc.want, tc.src)
			}
		})
	}
}

// TestTheFeederScanSeesADeletedCall is the other half, and it is what makes the
// case above a BLIND SPOT rather than a broken guard.
//
// Without it, "commenting the call out leaves it green" is equally consistent
// with "this scan never worked". The pair says: the scan does detect a call
// going away, and the comment is what hid it.
func TestTheFeederScanSeesADeletedCall(t *testing.T) {
	pat := regexp.MustCompile(`\.` + regexp.QuoteMeta("AddThing") + `\s*\(`)
	const with = "package p\n\nfunc f() {\n\tm.AddThing(ctx, 1)\n}\n"
	const without = "package p\n\nfunc f() {\n}\n"

	for _, tc := range []struct {
		name string
		src  string
		want bool
	}{{"the call present", with, true}, {"the call deleted outright", without, false}} {
		blanked, err := codeWithoutComments("fixture.go", []byte(tc.src))
		if err != nil {
			t.Fatalf("%s: fixture does not parse: %v", tc.name, err)
		}
		if got := pat.Match(blanked); got != tc.want {
			t.Errorf("%s: call found = %v, want %v", tc.name, got, tc.want)
		}
	}
}
