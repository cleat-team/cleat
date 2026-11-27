//go:build chaos

package chaos

// The pure parts of the residency benchmark (residency_bench_test.go, build tags `chaos bench`): the knob parser, the nearest-rank percentile and median, and the rank-separation rule,
// each with the test that gives it a known answer. They need no worker, no database and no benchmark run, so they sit behind `chaos` ALONE: every Chaos job that compiles ./tests/chaos
// with that tag compiles them, and the full sweep runs them. They were in the benchmark's file behind `bench` as well, which no CI job builds, so they compiled in no job and a change that
// broke `separation` or `parseBenchConfig` would have been found by whoever next read a wrong "separated" line (cleat#4493).

import (
	"fmt"
	"math"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

type benchConfig struct {
	Trials, Reps int
	Rows         map[string]bool
	Ks           []int
	Out          string
	NoisyLoad    float64
	Poll         string
}

var benchKnownRows = map[string]bool{"start": true, "signal": true, "timer": true, "history": true, "queries": true}

// parseBenchConfig reads the knobs through get (os.Getenv in a run). A knob that is SET and does not parse is an error and not a silent fall back to the default: a mistyped poll or row
// name would otherwise run a different benchmark from the one asked for and print its numbers as if it were that one (an unparseable CLEAT_BENCH_POLL used to run at 500ms).
func parseBenchConfig(get func(string) string) (benchConfig, error) {
	c := benchConfig{Trials: 30, Reps: 3, Rows: map[string]bool{}, Out: get("CLEAT_BENCH_OUT"), NoisyLoad: float64(runtime.NumCPU()), Poll: "500ms"}
	for _, k := range []struct {
		name string
		dst  *int
	}{{"CLEAT_BENCH_TRIALS", &c.Trials}, {"CLEAT_BENCH_REPS", &c.Reps}} {
		if v := get(k.name); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 {
				return c, fmt.Errorf("%s=%q is not a positive integer", k.name, v)
			}
			*k.dst = n
		}
	}
	if v := get("CLEAT_BENCH_POLL"); v != "" {
		if d, err := time.ParseDuration(v); err != nil || d <= 0 {
			return c, fmt.Errorf("CLEAT_BENCH_POLL=%q is not a positive duration (for example 500ms)", v)
		}
		c.Poll = v
	}
	rows := get("CLEAT_BENCH_ROWS")
	if rows == "" {
		rows = "start,signal,timer,history,queries"
	}
	for _, r := range strings.Split(rows, ",") {
		r = strings.TrimSpace(r)
		if !benchKnownRows[r] {
			return c, fmt.Errorf("CLEAT_BENCH_ROWS names %q, which is not a row (start, signal, timer, history, queries)", r)
		}
		c.Rows[r] = true
	}
	ks := get("CLEAT_BENCH_K")
	if ks == "" {
		ks = "0,10,100,500"
	}
	for _, k := range strings.Split(ks, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(k))
		if err != nil || n < 0 {
			return c, fmt.Errorf("CLEAT_BENCH_K names %q, which is not a non-negative integer", k)
		}
		c.Ks = append(c.Ks, n)
	}
	if v := get("CLEAT_BENCH_NOISY_LOAD"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		// ParseFloat accepts "NaN" and "Inf", and NaN <= 0 is false, so the sign test alone lets a NaN threshold through: a threshold nothing exceeds, and no cell is ever marked NOISY.
		if err != nil || !(f > 0) || math.IsInf(f, 0) {
			return c, fmt.Errorf("CLEAT_BENCH_NOISY_LOAD=%q is not a positive finite number", v)
		}
		c.NoisyLoad = f
	}
	return c, nil
}

func TestBenchConfigRefusesWhatItCannotParse(t *testing.T) {
	env := func(kv map[string]string) func(string) string { return func(k string) string { return kv[k] } }
	if c, err := parseBenchConfig(env(nil)); err != nil || c.Poll != "500ms" || c.Trials != 30 || c.Reps != 3 || len(c.Ks) != 4 || !c.Rows["queries"] {
		t.Errorf("defaults: %+v, %v", c, err)
	}
	if c, err := parseBenchConfig(env(map[string]string{"CLEAT_BENCH_POLL": "100ms", "CLEAT_BENCH_ROWS": "timer, start", "CLEAT_BENCH_K": "0,7"})); err != nil || c.Poll != "100ms" || !c.Rows["timer"] || !c.Rows["start"] || c.Rows["signal"] || len(c.Ks) != 2 || c.Ks[1] != 7 {
		t.Errorf("valid knobs: %+v, %v", c, err)
	}
	for name, kv := range map[string]map[string]string{
		"an unparseable poll":    {"CLEAT_BENCH_POLL": "fast"},
		"a poll with no unit":    {"CLEAT_BENCH_POLL": "100"},
		"a zero poll":            {"CLEAT_BENCH_POLL": "0s"},
		"a mistyped row":         {"CLEAT_BENCH_ROWS": "start,signall"},
		"a non-numeric trials":   {"CLEAT_BENCH_TRIALS": "many"},
		"zero reps":              {"CLEAT_BENCH_REPS": "0"},
		"a non-numeric K":        {"CLEAT_BENCH_K": "0,ten"},
		"a negative noisy load":  {"CLEAT_BENCH_NOISY_LOAD": "-1"},
		"a NaN noisy load":       {"CLEAT_BENCH_NOISY_LOAD": "NaN"},
		"an infinite noisy load": {"CLEAT_BENCH_NOISY_LOAD": "Inf"},
		"a -Inf noisy load":      {"CLEAT_BENCH_NOISY_LOAD": "-Inf"},
		"a zero noisy load":      {"CLEAT_BENCH_NOISY_LOAD": "0"},
	} {
		if _, err := parseBenchConfig(env(kv)); err == nil {
			t.Errorf("%s was accepted: %v", name, kv)
		}
	}
}

// benchPercentile is the nearest-rank percentile of an ascending slice: the smallest value with at least p of the samples at or below it.
func benchPercentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(p*float64(len(sorted)) + 0.999999999)
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}

func benchSorted(d []time.Duration) []time.Duration {
	s := append([]time.Duration(nil), d...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s
}

func benchMedian(d []time.Duration) time.Duration {
	s := benchSorted(d)
	if len(s) == 0 {
		return 0
	}
	if len(s)%2 == 1 {
		return s[len(s)/2]
	}
	return (s[len(s)/2-1] + s[len(s)/2]) / 2
}

// The percentile and median code is the one thing in this file whose answer is known without running a worker, so it has a known positive of its own.
func TestBenchPercentilesAreRight(t *testing.T) {
	var d []time.Duration
	for i := 1; i <= 100; i++ {
		d = append(d, time.Duration(i)*time.Millisecond)
	}
	s := benchSorted(d)
	for _, tc := range []struct {
		p    float64
		want time.Duration
	}{{0.50, 50 * time.Millisecond}, {0.90, 90 * time.Millisecond}, {0.99, 99 * time.Millisecond}, {1.0, 100 * time.Millisecond}} {
		if got := benchPercentile(s, tc.p); got != tc.want {
			t.Errorf("p%v of 1..100 ms = %v, want %v", tc.p*100, got, tc.want)
		}
	}
	// Five samples: p99 is the maximum, which is why the report says so for N under 100.
	five := benchSorted([]time.Duration{5 * time.Millisecond, 1 * time.Millisecond, 4 * time.Millisecond, 2 * time.Millisecond, 3 * time.Millisecond})
	if got := benchPercentile(five, 0.99); got != 5*time.Millisecond {
		t.Errorf("p99 of five samples = %v, want the maximum, 5ms", got)
	}
	if got := benchMedian(five); got != 3*time.Millisecond {
		t.Errorf("median of 1..5 ms = %v, want 3ms", got)
	}
	if got := benchMedian([]time.Duration{1 * time.Millisecond, 2 * time.Millisecond, 3 * time.Millisecond, 10 * time.Millisecond}); got != 2500*time.Microsecond {
		t.Errorf("median of 1, 2, 3, 10 ms = %v, want 2.5ms", got)
	}
}

// separation asks the question the rep medians can answer without a noise threshold: do ALL the off reps lie on one side of ALL the on and on2 reps? With R reps per arm, the R off medians
// against the 2R on and on2 medians, and no effect at all, the chance that the off ones are the R lowest or the R highest is 2 / C(3R, R): 1/42 at R = 3. It is a rank statement, so it does not
// care how loud the machine was, only whether the load happened to fall in the same order as the arms. It needs R >= 3 to be able to say anything.
func separation(off, rest []time.Duration) (string, bool) {
	r := len(off)
	if r < 3 || len(rest) != 2*r {
		return "", false
	}
	minOff, maxOff := benchSorted(off)[0], benchSorted(off)[r-1]
	sr := benchSorted(rest)
	below := maxOff < sr[0]
	above := minOff > sr[2*r-1]
	p := 2 / binomial(3*r, r)
	switch {
	case below:
		return fmt.Sprintf("separation: all %d off rep medians are BELOW all %d on/on2 rep medians (p = %.4f under no effect, 2/C(%d,%d))", r, 2*r, p, 3*r, r), true
	case above:
		return fmt.Sprintf("separation: all %d off rep medians are ABOVE all %d on/on2 rep medians (p = %.4f under no effect, 2/C(%d,%d))", r, 2*r, p, 3*r, r), true
	}
	return fmt.Sprintf("separation: NOT separated: the %d off rep medians interleave the %d on/on2 rep medians; no claim of a difference", r, 2*r), true
}

func binomial(n, k int) float64 {
	v := 1.0
	for i := 1; i <= k; i++ {
		v = v * float64(n-k+i) / float64(i)
	}
	return v
}

// The separation arithmetic has a known answer too: 1/42 at three reps, and the interleaved case must refuse.
func TestBenchSeparationIsRight(t *testing.T) {
	ms := func(v ...int) []time.Duration {
		var d []time.Duration
		for _, x := range v {
			d = append(d, time.Duration(x)*time.Millisecond)
		}
		return d
	}
	if got := binomial(9, 3); got != 84 {
		t.Errorf("C(9,3) = %v, want 84", got)
	}
	line, _ := separation(ms(80, 90, 100), ms(10, 20, 30, 15, 25, 35))
	if !strings.Contains(line, "ABOVE") || !strings.Contains(line, "p = 0.0238") {
		t.Errorf("off all above: %q, want ABOVE and p = 0.0238 (1/42)", line)
	}
	line, _ = separation(ms(1, 2, 3), ms(10, 20, 30, 15, 25, 35))
	if !strings.Contains(line, "BELOW") {
		t.Errorf("off all below: %q, want BELOW", line)
	}
	// Above the lowest on/on2 median but not above the highest: still interleaved. A check of only one end passes the case above and fails this one.
	line, _ = separation(ms(45, 90, 100), ms(20, 30, 40, 50, 60, 70))
	if !strings.Contains(line, "NOT separated") {
		t.Errorf("off above the lowest but not the highest: %q, want NOT separated", line)
	}
	line, _ = separation(ms(10, 90, 100), ms(20, 30, 40, 50, 60, 70))
	if !strings.Contains(line, "NOT separated") {
		t.Errorf("interleaved: %q, want NOT separated", line)
	}
	// A tie is not separation: an off median EQUAL to the highest on/on2 median is not above it. Counting ties as separated (<=) would pass the case above and fail this one.
	line, _ = separation(ms(30, 90, 100), ms(10, 20, 30, 15, 25, 5))
	if !strings.Contains(line, "NOT separated") {
		t.Errorf("off tied with the highest on/on2: %q, want NOT separated", line)
	}
	line, _ = separation(ms(1, 2, 5), ms(5, 20, 30, 15, 25, 35))
	if !strings.Contains(line, "NOT separated") {
		t.Errorf("off tied with the lowest on/on2: %q, want NOT separated", line)
	}
	if _, ok := separation(ms(1, 2), ms(3, 4, 5, 6)); ok {
		t.Errorf("two reps cannot support a rank statement, but separation answered")
	}
}
