package manifests

// The chart passes --enable-admin-api only when adminApi.enabled is set, and that defaults to false
// (cleat#2267). With the flag on, any authenticated key of any tenant can drain a worker and trigger an
// all-tenant retention sweep, so a chart that turns it on as a side effect of some other value (a drain key
// being configured was the first draft) puts every Helm deployment back in the state the issue closed.
//
// A second reading of the template, not a rendering: it models `if`/`else`/`end` lines and nothing else, and
// the change was also checked against `helm template` in the four configurations that matter. Its known
// positive removes the condition and must be reported.

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

var (
	tplIf   = regexp.MustCompile(`^\s*\{\{-?\s*(if|with|range)\b(.*?)-?\}\}`)
	tplElse = regexp.MustCompile(`^\s*\{\{-?\s*else\b`)
	tplEnd  = regexp.MustCompile(`^\s*\{\{-?\s*end\s*-?\}\}`)
)

// adminFlagLines returns the line numbers where --enable-admin-api is emitted that are NOT inside an
// `if` whose condition mentions .Values.adminApi.enabled (in its `if` arm, not its `else`).
func adminFlagLines(src string) []int {
	type frame struct {
		gated  bool // the current arm is the one guarded by adminApi.enabled
		cond   bool // the if-condition mentions it
		inElse bool
	}
	var stack []frame
	var bad []int
	for i, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case tplIf.MatchString(line):
			m := tplIf.FindStringSubmatch(line)
			c := strings.Contains(m[2], ".Values.adminApi.enabled")
			stack = append(stack, frame{gated: c, cond: c})
		case tplElse.MatchString(line):
			if n := len(stack); n > 0 {
				stack[n-1].inElse = true
				stack[n-1].gated = false
			}
		case tplEnd.MatchString(line):
			if n := len(stack); n > 0 {
				stack = stack[:n-1]
			}
		case strings.HasPrefix(trimmed, "#"):
			// a comment that mentions the flag is not the flag
		case strings.Contains(line, "--enable-admin-api"):
			gated := false
			for _, f := range stack {
				gated = gated || f.gated
			}
			if !gated {
				bad = append(bad, i+1)
			}
		}
	}
	return bad
}

func TestTheChartPassesTheAdminAPIFlagOnlyWhenAdminAPIIsEnabledAndItDefaultsOff(t *testing.T) {
	src, err := os.ReadFile("../../charts/cleat/templates/deployment.yaml")
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)
	if !strings.Contains(text, `"--enable-admin-api"`) {
		t.Fatal("the deployment template no longer mentions --enable-admin-api at all: this scanned nothing")
	}
	if bad := adminFlagLines(text); len(bad) > 0 {
		t.Errorf("deployment.yaml passes --enable-admin-api outside an `if .Values.adminApi.enabled` block at line(s) %v: "+
			"that turns on routes any tenant's key can call for a reason other than the operator asking (cleat#2267)", bad)
	}

	// Known positive: the same file with the condition swapped for another value must be reported.
	mutated := strings.Replace(text, ".Values.adminApi.enabled }}\n            # Off unless asked for", ".Values.auth.adminApiKey }}\n            # Off unless asked for", 1)
	if mutated == text {
		t.Fatal("could not build the known-positive: the guarded block is not where the test expects")
	}
	if len(adminFlagLines(mutated)) == 0 {
		t.Error("the scan did not report --enable-admin-api under an unrelated condition: it cannot fail")
	}

	values, err := os.ReadFile("../../charts/cleat/values.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`(?m)^adminApi:\n  enabled: false\s*$`).Match(values) {
		t.Error("values.yaml must define `adminApi:` with `enabled: false` as the default")
	}
}
