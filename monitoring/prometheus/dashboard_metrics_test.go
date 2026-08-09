package prometheus

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestDashboardMetricsExist guards against exactly the defect that shipped
// twice: a Grafana panel querying a metric name that does not exist.
// grafana-dashboard.json queried cleat_durable_calls_total (the real name is
// cleat_calls_total) and grafana/dashboard.json queried
// cleat_db_query_duration_seconds (the real name is
// cleat_db_query_latency_seconds). Both panels rendered "No data" forever,
// silently, in exactly the place an incident responder would look for
// DurableCall rate and DB latency percentiles. Nothing failed, because
// nothing connects a dashboard JSON file to the Go source it queries -- this
// test is that connection.
//
// It works entirely off the source text, not a running exporter: every
// `cleat_...` metric name this package registers is extracted from
// metrics.go with a regexp, and every `cleat_...` identifier referenced
// anywhere in monitoring/**/*.json is checked against that set. A histogram
// registered as e.g. cleat_db_query_latency_seconds also permits
// cleat_db_query_latency_seconds_bucket/_sum/_count, the suffixes Prometheus
// derives from a histogram and the only way a dashboard legitimately
// queries one (histogram_quantile needs _bucket).
func TestDashboardMetricsExist(t *testing.T) {
	registered, histograms := registeredMetricNames(t)
	if len(registered) == 0 {
		t.Fatal("found no cleat_* metric names in metrics.go -- the extraction regexp is broken, " +
			"which would make this whole test pass vacuously")
	}

	dashboardDir := ".." // monitoring/
	var jsonFiles []string
	err := filepath.WalkDir(dashboardDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(path, ".json") {
			jsonFiles = append(jsonFiles, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dashboardDir, err)
	}
	if len(jsonFiles) == 0 {
		t.Fatal("found no monitoring/**/*.json files -- this test would pass vacuously")
	}

	metricRefRE := regexp.MustCompile(`cleat_[a-zA-Z0-9_]+`)

	var findings []string
	for _, path := range jsonFiles {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		seen := map[string]bool{}
		for _, name := range metricRefRE.FindAllString(string(data), -1) {
			if seen[name] {
				continue
			}
			seen[name] = true
			if metricNameIsValid(name, registered, histograms) {
				continue
			}
			findings = append(findings, fmt.Sprintf("%s: %q is not a registered metric", path, name))
		}
	}

	if len(findings) > 0 {
		sort.Strings(findings)
		t.Errorf("%d dashboard metric reference(s) do not exist in monitoring/prometheus/metrics.go "+
			"(the panel renders \"No data\" forever, silently):\n  %s",
			len(findings), strings.Join(findings, "\n  "))
	}
}

// registeredMetricNames extracts every cleat_* metric name string literal
// from metrics.go (the superset approach: a name mentioned anywhere in that
// file, including in its own error-wrapping strings, is a real metric name,
// and being permissive here costs nothing -- the test's job is to catch
// dashboard references to names that don't exist, not to police
// metrics.go's own style) and separately the subset that are histograms,
// which is what allows a dashboard to legitimately add _bucket/_sum/_count.
func registeredMetricNames(t *testing.T) (all map[string]bool, histograms map[string]bool) {
	t.Helper()
	data, err := os.ReadFile("metrics.go")
	if err != nil {
		t.Fatalf("read metrics.go: %v", err)
	}
	src := string(data)

	all = map[string]bool{}
	for _, m := range regexp.MustCompile(`"(cleat_[a-zA-Z0-9_]+)"`).FindAllStringSubmatch(src, -1) {
		all[m[1]] = true
	}

	histograms = map[string]bool{}
	for _, m := range regexp.MustCompile(`Float64Histogram\(\s*\n?\s*"(cleat_[a-zA-Z0-9_]+)"`).FindAllStringSubmatch(src, -1) {
		histograms[m[1]] = true
	}
	return all, histograms
}

// metricNameIsValid reports whether name is either a registered metric
// verbatim, or a registered histogram's name with a Prometheus-derived
// _bucket/_sum/_count suffix.
func metricNameIsValid(name string, all, histograms map[string]bool) bool {
	if all[name] {
		return true
	}
	for _, suffix := range []string{"_bucket", "_sum", "_count"} {
		if base, ok := strings.CutSuffix(name, suffix); ok && histograms[base] {
			return true
		}
	}
	return false
}
