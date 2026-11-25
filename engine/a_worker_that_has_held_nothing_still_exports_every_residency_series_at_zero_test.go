package engine

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// cleat#4474. The OTel exporter writes an instrument out only after its first measurement, so on a worker that had held nothing the held-run gauges, the eviction counter and
// most of the other residency series were simply absent from /metrics, and an operator could not tell "this worker holds nothing" from "the exporter is broken". The set
// records a 0 for each series it can emit when it is built, the way it records the budget.
//
// residentCounter, which every other residency test reads through, returns 0 for a series that is NOT THERE, so none of them could have seen this: a gauge that was absent and
// one that read 0 look the same to it. These tests read the scrape itself.

// residentSeriesWanted is every series the residency instruments can emit, written out here and not read from the engine's own list, so a series dropped from that list shows up
// as a difference between two derivations and not as a list agreeing with itself. The label values are the ones resident.go records: park.go's kind names, and the outcome,
// reason and result constants.
func residentSeriesWanted() []string {
	var want []string
	for _, kind := range []string{"sleep", "signals"} {
		for _, outcome := range []string{"deadline", "cancelled", "evicted"} {
			want = append(want, `cleat_resident_parks_total{kind="`+kind+`",outcome="`+outcome+`"`)
		}
	}
	for _, outcome := range []string{"woke", "recheck_failed"} { // only a signal wait is rechecked, so only it can end these ways
		want = append(want, `cleat_resident_parks_total{kind="signals",outcome="`+outcome+`"`)
	}
	timerReasons := []string{"language", "window", "count", "memory", "pressure", "draining", "code", "deadline"}
	for _, reason := range timerReasons {
		want = append(want, `cleat_resident_refusals_total{kind="sleep",reason="`+reason+`"`)
	}
	for _, reason := range append([]string{"kind"}, timerReasons...) { // a signal wait with no recheck is refused for its kind
		want = append(want, `cleat_resident_refusals_total{kind="signals",reason="`+reason+`"`)
	}
	for _, kind := range []string{"promise", "child", "update"} { // a kind with no wake source is refused for that and nothing else
		want = append(want, `cleat_resident_refusals_total{kind="`+kind+`",reason="kind"`)
	}
	for _, result := range []string{"found", "none", "error"} {
		want = append(want, `cleat_resident_rechecks_total{kind="signals",result="`+result+`"`)
	}
	for _, reason := range []string{"shutdown", "pressure", "code"} {
		want = append(want, `cleat_resident_evictions_total{reason="`+reason+`"`)
	}
	return want
}

// seriesLine returns the scrape's sample lines that start with prefix, in the form `name{labels` (a prefix with no closing brace, since worker_id follows the other labels).
func seriesLines(t *testing.T, body, prefix string) []string {
	t.Helper()
	var out []string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, prefix) {
			out = append(out, line)
		}
	}
	return out
}

func scrapeResident(t *testing.T, m interface{ ServeHTTP() http.Handler }) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.ServeHTTP().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics answered %d: %s", rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

func sampleValue(t *testing.T, line string) float64 {
	t.Helper()
	f := strings.Fields(line)
	v, err := strconv.ParseFloat(f[len(f)-1], 64)
	if err != nil {
		t.Fatalf("cannot read the value of %q: %v", line, err)
	}
	return v
}

func TestAWorkerThatHasHeldNothingStillExportsEveryResidencySeriesAtZero(t *testing.T) {
	m := newReplayMetrics(t)
	clock := newFakeResidentClock(residentT)
	newTestResidentSet(t, residentTestConfig(clock, func(c *ResidencyConfig) { c.Metrics = m }))
	body := scrapeResident(t, m)

	// The two gauges: present, and 0.
	for _, name := range []string{"cleat_resident_runs", "cleat_resident_memory_charged_bytes"} {
		lines := seriesLines(t, body, name+"{")
		if len(lines) != 1 {
			t.Errorf("%s: %d series on a worker that has held nothing, want 1 reading 0", name, len(lines))
			continue
		}
		if v := sampleValue(t, lines[0]); v != 0 {
			t.Errorf("%s reads %v, want 0", name, v)
		}
	}
	// The control for the above: the budget gauge was already exported at start-up, and it is not 0. A scrape that showed every residency gauge at 0 would be showing a
	// helper that writes zeros, and this is the one that distinguishes a number from a placeholder.
	if lines := seriesLines(t, body, "cleat_resident_memory_budget_bytes{"); len(lines) != 1 || sampleValue(t, lines[0]) <= 0 {
		t.Errorf("the budget gauge is not a positive figure on a fresh set: %v", lines)
	}

	// Every counter series the set can emit, at 0.
	for _, prefix := range residentSeriesWanted() {
		lines := seriesLines(t, body, prefix)
		if len(lines) != 1 {
			t.Errorf("%s}: %d series on a worker that has held nothing, want 1 reading 0", prefix, len(lines))
			continue
		}
		if v := sampleValue(t, lines[0]); v != 0 {
			t.Errorf("%s} reads %v, want 0", prefix, v)
		}
	}
}

// A zero recorded at start-up is the SAME series the first real event increments, not a neighbour of it: the held series reads 1, there is still one line for it, and the others
// stay at 0.
func TestAResidencySeriesStartedAtZeroIsTheSeriesTheFirstHoldIncrements(t *testing.T) {
	m := newReplayMetrics(t)
	clock := newFakeResidentClock(residentT)
	set := newTestResidentSet(t, residentTestConfig(clock, func(c *ResidencyConfig) { c.Metrics = m }))
	run := set.ForRun(ResidentRunInfo{Language: "go"})

	held := park(run, context.Background(), timerReq(residentT+5000))
	awaitArmed(t, clock)
	body := scrapeResident(t, m)
	if lines := seriesLines(t, body, "cleat_resident_runs{"); len(lines) != 1 || sampleValue(t, lines[0]) != 1 {
		t.Errorf("cleat_resident_runs while one run is held: %v, want one series reading 1", lines)
	}
	clock.fireAll()
	mustOutcome(t, held, parkDeadline, "the held run")

	body = scrapeResident(t, m)
	lines := seriesLines(t, body, `cleat_resident_parks_total{kind="sleep",outcome="deadline"`)
	if len(lines) != 1 || sampleValue(t, lines[0]) != 1 {
		t.Errorf("a timer hold that reached its deadline: %v, want one series reading 1 (the start-up zero and the hold are two series, or the hold was not counted)", lines)
	}
	if lines := seriesLines(t, body, `cleat_resident_parks_total{kind="sleep",outcome="cancelled"`); len(lines) != 1 || sampleValue(t, lines[0]) != 0 {
		t.Errorf("the cancelled series after a hold that was not cancelled: %v, want one reading 0", lines)
	}
	if lines := seriesLines(t, body, "cleat_resident_runs{"); len(lines) != 1 || sampleValue(t, lines[0]) != 0 {
		t.Errorf("cleat_resident_runs after the run left: %v, want one series reading 0", lines)
	}
}

// Residency off records nothing and exports nothing: the budget gauge is what marks a worker as one that holds runs, and the zeros follow it. A worker with residency off has no
// held runs to report, and a 0 there would read as "residency is on and idle".
func TestAWorkerWithResidencyOffExportsNoResidencySeries(t *testing.T) {
	m := newReplayMetrics(t)
	clock := newFakeResidentClock(residentT)
	newTestResidentSet(t, residentTestConfig(clock, func(c *ResidencyConfig) { c.Metrics = m; c.MaxRuns = 0 }))
	body := scrapeResident(t, m)
	if lines := seriesLines(t, body, "cleat_resident_"); len(lines) != 0 {
		t.Errorf("residency is off and the scrape carries %d residency series: %v", len(lines), lines)
	}
}

// The label values the engine records are constants in resident.go, and a value added to one of them without a start-up zero would reappear as the gap this closes. This reads the
// constants from the source and checks each one is among the values the start-up list names, so the list cannot fall behind.
func TestEveryResidencyLabelValueTheEngineRecordsHasAStartUpZero(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "resident.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing resident.go: %v", err)
	}
	consts := map[string]string{}
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, n := range vs.Names {
				if i >= len(vs.Values) {
					continue
				}
				if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
					v, _ := strconv.Unquote(lit.Value)
					consts[n.Name] = v
				}
			}
		}
	}
	byPrefix := func(prefix string) []string {
		var out []string
		for name, v := range consts {
			if strings.HasPrefix(name, prefix) {
				out = append(out, v)
			}
		}
		sort.Strings(out)
		return out
	}
	refusals, ended, rechecks, evictions := byPrefix("residentRefused"), byPrefix("residentEnded"), byPrefix("residentRecheck"), byPrefix("ResidentEvicted")
	for what, got := range map[string][]string{"refusal reasons": refusals, "park outcomes": ended, "recheck results": rechecks, "eviction reasons": evictions} {
		if len(got) == 0 {
			t.Fatalf("read no %s from resident.go: the constants have been renamed, and this check would pass on nothing", what)
		}
	}

	started := residentStartUpSeries()
	second := func(ss [][2]string) map[string]bool {
		out := map[string]bool{}
		for _, s := range ss {
			out[s[1]] = true
		}
		return out
	}
	evictionReasons := map[string]bool{}
	for _, r := range started.Evictions {
		evictionReasons[r] = true
	}
	for _, c := range []struct {
		what   string
		values []string
		in     map[string]bool
	}{
		{"refusal reason", refusals, second(started.Refusals)},
		{"park outcome", ended, second(started.Parks)},
		{"recheck result", rechecks, second(started.Rechecks)},
		{"eviction reason", evictions, evictionReasons},
	} {
		for _, v := range c.values {
			if !c.in[v] {
				t.Errorf("%s %q is recorded by the engine and has no series at start-up", c.what, v)
			}
		}
	}
}
