package prometheus

import (
	"context"
	"regexp"
	"strings"
	"testing"
)

// TestCounterHelpAndTypeLinesNameTheSample is cleat#2272's known-positive.
//
// writeMetric used to strip _total from the # HELP / # TYPE line's name for
// every counter, believing it was following OpenMetrics convention, while
// leaving _total on the actual sample name -- so classic-format Prometheus
// text carried metadata lines that named a DIFFERENT metric family than the
// samples they preceded. promtool's `check metrics` (classic-format mode,
// which is what this exposition actually is: no `# EOF` trailer, no
// OpenMetrics content type) requires the two to match exactly, and reported
// "no help text" for cleat_background_loops_total during cleat#2266's real
// scrape -- the one counter that happened to have nonzero data at that
// moment. "Probably true of every counter" (the issue's own words): the
// defect was in stripTotalSuffix's caller, not in which counter was
// incremented, so this test picks a DIFFERENT counter (RecordWorkflowsPurged)
// than the one the issue happened to observe, to prove the fix is general
// rather than a fix for one name.
func TestCounterHelpAndTypeLinesNameTheSample(t *testing.T) {
	m, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	m.RecordWorkflowsPurged(context.Background(), 3)

	body := scrape(t, m)

	const name = "cleat_workflows_purged_total"
	helpRe := regexp.MustCompile(`(?m)^# HELP (\S+) .*$`)
	typeRe := regexp.MustCompile(`(?m)^# TYPE (\S+) counter$`)

	helpNames := helpRe.FindAllStringSubmatch(body, -1)
	typeNames := typeRe.FindAllStringSubmatch(body, -1)

	foundHelp, foundType := false, false
	for _, h := range helpNames {
		if h[1] == name {
			foundHelp = true
		}
	}
	for _, ty := range typeNames {
		if ty[1] == name {
			foundType = true
		}
	}
	if !foundHelp {
		t.Errorf("no '# HELP %s ...' line naming the actual sample -- got:\n%s", name, grep(body, "workflows_purged"))
	}
	if !foundType {
		t.Errorf("no '# TYPE %s counter' line naming the actual sample -- got:\n%s", name, grep(body, "workflows_purged"))
	}

	// The stripped form must not appear either -- a stray metadata line for a
	// metric family that has no samples is exactly the mismatch this test
	// exists to catch, just from the other side.
	const stripped = "cleat_workflows_purged"
	for _, h := range helpNames {
		if h[1] == stripped {
			t.Errorf("found a '# HELP %s ...' line -- the stripped name, which has no matching sample", stripped)
		}
	}
}

func grep(body, substr string) string {
	var out []string
	for _, l := range strings.Split(body, "\n") {
		if strings.Contains(l, substr) {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}
