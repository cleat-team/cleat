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
	// The gauges, which are the serious half: each is an operator's only view
	// of a growth or stall condition, and none of them emits a series.
	"SetWorkflowsStuck": "NEEDS A FEEDER. The stuck-workflow gauge -- a " +
		"stalled run is precisely the condition you page on, and the series " +
		"does not exist. Wants a sweep that counts runs past their deadline",
	"SetEventHistorySize": "NEEDS A FEEDER. Event-history growth, the " +
		"observable for the retention behaviour cleat#1294 documents. The " +
		"thing that would let an operator SEE unbounded growth is not fed",
	"SetEventHistoryRowCount": "NEEDS A FEEDER. The row-count half of the " +
		"same observable",
	"SetMemoryPressureRatio": "NEEDS A FEEDER. The observable for the " +
		"--memory-soft-limit/--memory-hard-limit circuit breaker. The breaker " +
		"works; its indicator does not. Note SetMemoryPressure (no Ratio) IS " +
		"fed, so a reader checking 'is memory observable' finds a yes",
	"SetConcurrencyKeysTotal": "NEEDS A FEEDER. Concurrency-key inventory",
	"SetConcurrencyKeysExpiringSoon": "NEEDS A FEEDER. The leading indicator " +
		"for a key sweep falling behind",
	"SetWasmCacheEntries": "NEEDS A FEEDER. Compiled-module cache occupancy",
	"SetWasmCacheBytes":   "NEEDS A FEEDER. The byte half of the same",

	// The counters and histograms. Less severe -- a counter never Added reads
	// 0 rather than vanishing -- but still an instrument nothing writes.
	"RecordClaimLatency":          "NEEDS A CALL SITE. Claim latency is recorded nowhere",
	"RecordWasmLoadLatency":       "NEEDS A CALL SITE. Module load latency",
	"RecordEncryptionError":       "NEEDS A CALL SITE. Payload-encryption failures",
	"RecordReaperInstanceClaimed": "NEEDS A CALL SITE. Reaper reclaims",
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

	called := map[string]bool{}
	for name, span := range methods {
		pat := regexp.MustCompile(`\.` + regexp.QuoteMeta(name) + `\s*\(`)
		// metrics.go with only THIS method's body removed.
		trimmed := string(selfSrc[:span[0]]) + string(selfSrc[span[1]:])
		if pat.MatchString(trimmed) {
			called[name] = true
			continue
		}
		for _, f := range files {
			if strings.HasSuffix(f, "/monitoring/prometheus/metrics.go") {
				continue
			}
			b, err := os.ReadFile(f)
			if err != nil {
				continue
			}
			if pat.Match(b) {
				called[name] = true
				break
			}
		}
	}
	return called
}
