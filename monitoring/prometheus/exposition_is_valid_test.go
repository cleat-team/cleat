package prometheus

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A scrape is accepted or rejected AS A WHOLE: one malformed line and Prometheus drops the target, so every
// series on it is lost and `up` reads 0. That is how histogram output with doubled label braces and
// non-cumulative buckets made CleatWorkerDown fire on healthy workers and kept cleat_db_reachable from ever
// being stored (cleat#2266). Nothing in this package's tests read the /metrics text back.
//
// The validator below is written from the exposition format and shares nothing with the writer it checks.

var (
	// name, optional {label="value",...}, a value. Label values may contain escaped characters.
	sampleLine = regexp.MustCompile(`^([a-zA-Z_:][a-zA-Z0-9_:]*)(\{(?:[a-zA-Z_][a-zA-Z0-9_]*="(?:[^"\\]|\\.)*"(?:,[a-zA-Z_][a-zA-Z0-9_]*="(?:[^"\\]|\\.)*")*)?\})? (\S+)$`)
	labelPair  = regexp.MustCompile(`([a-zA-Z_][a-zA-Z0-9_]*)="((?:[^"\\]|\\.)*)"`)
)

type sample struct {
	name   string
	labels map[string]string
	value  float64
}

// parseExposition rejects anything the text format does not allow, and returns the samples.
func parseExposition(t *testing.T, body string) []sample {
	t.Helper()
	var out []sample
	for n, line := range strings.Split(strings.TrimRight(body, "\n"), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		m := sampleLine.FindStringSubmatch(line)
		if m == nil {
			t.Fatalf("line %d is not valid exposition text (Prometheus would drop the whole scrape): %q", n+1, line)
		}
		v, err := strconv.ParseFloat(m[3], 64)
		if err != nil {
			t.Fatalf("line %d: value %q is not a number: %q", n+1, m[3], line)
		}
		labels := map[string]string{}
		for _, p := range labelPair.FindAllStringSubmatch(m[2], -1) {
			labels[p[1]] = p[2]
		}
		out = append(out, sample{m[1], labels, v})
	}
	return out
}

func scrape(t *testing.T, m *Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.ServeHTTP().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics = %d: %s", rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

func labelKey(l map[string]string) string {
	keys := make([]string, 0, len(l))
	for k := range l {
		if k != "le" {
			keys = append(keys, k+"="+l[k])
		}
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

// TestEveryHistogramOnTheScrapeIsWellFormedAndCumulative records known observations into two histograms,
// one with labels beyond the defaults and one without any, and checks the text a scraper would read.
func TestEveryHistogramOnTheScrapeIsWellFormedAndCumulative(t *testing.T) {
	m, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// Seven observations spread across the boundaries, including some above the top one.
	obs := []time.Duration{500 * time.Microsecond, 2 * time.Millisecond, 2 * time.Millisecond, 40 * time.Millisecond,
		3 * time.Second, 2 * time.Hour, 48 * time.Hour}
	for _, d := range obs {
		m.RecordClaimLatency(ctx, d, "wf")
		m.RecordPollWaitDuration(ctx, d)
	}

	samples := parseExposition(t, scrape(t, m))

	type series struct {
		buckets map[float64]float64 // le -> value
		count   float64
		hasCnt  bool
	}
	all := map[string]*series{}
	get := func(name string, l map[string]string) *series {
		k := name + "|" + labelKey(l)
		if all[k] == nil {
			all[k] = &series{buckets: map[float64]float64{}}
		}
		return all[k]
	}
	for _, s := range samples {
		switch {
		case strings.HasSuffix(s.name, "_bucket"):
			le, ok := s.labels["le"]
			if !ok {
				t.Fatalf("%s has no le label", s.name)
			}
			bound := math.Inf(1)
			if le != "+Inf" {
				if bound, err = strconv.ParseFloat(le, 64); err != nil {
					t.Fatalf("%s le=%q is not a number", s.name, le)
				}
			}
			get(strings.TrimSuffix(s.name, "_bucket"), s.labels).buckets[bound] = s.value
		case strings.HasSuffix(s.name, "_count"):
			x := get(strings.TrimSuffix(s.name, "_count"), s.labels)
			x.count, x.hasCnt = s.value, true
		}
	}

	// Vacuity: a validator that saw no histogram passes for the same reason a correct one does.
	want := map[string]bool{"cleat_claim_latency_seconds": false, "cleat_poll_wait_seconds": false}
	for k, s := range all {
		if !s.hasCnt {
			continue
		}
		name := strings.SplitN(k, "|", 2)[0]
		if _, ok := want[name]; ok {
			want[name] = true
		}
		bounds := make([]float64, 0, len(s.buckets))
		for b := range s.buckets {
			bounds = append(bounds, b)
		}
		sort.Float64s(bounds)
		if len(bounds) < 2 || !math.IsInf(bounds[len(bounds)-1], 1) {
			t.Errorf("%s: buckets %v do not end in +Inf", k, bounds)
			continue
		}
		prev := -1.0
		for _, b := range bounds {
			if s.buckets[b] < prev {
				t.Errorf("%s: bucket le=%g is %g, below the one before it (%g): buckets must be cumulative", k, b, s.buckets[b], prev)
			}
			prev = s.buckets[b]
		}
		if inf := s.buckets[math.Inf(1)]; inf != s.count || inf != float64(len(obs)) {
			t.Errorf("%s: le=+Inf is %g and _count is %g, both want %d (every observation)", k, inf, s.count, len(obs))
		}
	}
	for name, seen := range want {
		if !seen {
			t.Fatalf("no %s histogram on the scrape; this checked nothing", name)
		}
	}

	// A known distribution: the number of observations <= 5ms is exactly three (0.5ms, 2ms, 2ms). This
	// pins the cumulative arithmetic itself rather than only its shape.
	for _, s := range samples {
		if s.name == "cleat_poll_wait_seconds_bucket" && s.labels["le"] == "0.005" && s.value != 3 {
			t.Errorf("cleat_poll_wait_seconds le=0.005 = %g, want 3 (0.5ms, 2ms, 2ms)", s.value)
		}
	}
}

// The validator has to be able to fail. These are the shapes cleat#2266 measured, and promtool rejects
// them too.
func TestTheExpositionValidatorRejectsWhatPrometheusRejects(t *testing.T) {
	for _, bad := range []string{
		`cleat_x_bucket{{dialect="postgres"},le="0.001"} 5`,
		`cleat_x_bucket{dialect="postgres"},le="0.001"} 5`,
		`cleat_x_bucket{dialect="postgres",le=0.001} 5`,
		`cleat_x_bucket{dialect="postgres",le="0.001"} five`,
	} {
		if sampleLine.MatchString(bad) && numeric(bad) {
			t.Errorf("the validator accepted %q", bad)
		}
	}
	for _, good := range []string{
		`cleat_x_bucket{dialect="postgres",le="0.001"} 5`,
		`cleat_x_bucket{le="+Inf"} 5`,
		`cleat_x_count 7`,
		`cleat_x_sum{a="b\"c"} 1.5e-05`,
	} {
		if !sampleLine.MatchString(good) || !numeric(good) {
			t.Errorf("the validator rejected %q", good)
		}
	}
}

func numeric(line string) bool {
	_, err := strconv.ParseFloat(line[strings.LastIndex(line, " ")+1:], 64)
	return err == nil
}
