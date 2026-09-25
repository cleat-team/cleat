package manifests

// The chart passes --enable-admin-api only when adminApi.enabled is set, and that defaults to false
// (cleat#2267). With the flag on, any authenticated key of any tenant can drain a worker and trigger an
// all-tenant retention sweep, so a chart that turns it on as a side effect of some other value (a drain key
// being configured was the first draft) puts every Helm deployment back in the state the issue closed.
//
// THE FIRST VERSION OF THIS TEST COULD NOT FAIL FOR THE CASE IT EXISTS FOR. It checked that the `if` around the
// flag mentioned .Values.adminApi.enabled, so `if or .Values.auth.adminApiKey .Values.adminApi.enabled`, which
// re-introduces the auto-enable, passed (cleat-review). This one renders the template and asserts on the
// OUTPUT, for the combinations of values that matter.
//
// The render is Go's own text/template over the real deployment.yaml and the real values.yaml, so every `if`,
// `and` and `or` in the template is evaluated for real. Only the three Helm-only functions the template uses
// (include, toYaml, nindent) are stubbed: they shape the output and take no part in deciding whether the flag
// appears. It is not helm, and helm was also run on the same four configurations when this was written; no CI
// job here runs helm.

import (
	"bytes"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"text/template"
	"time"

	"gopkg.in/yaml.v3"
)

const deploymentPath = "../../charts/cleat/templates/deployment.yaml"
const valuesPath = "../../charts/cleat/values.yaml"

func loadChartValues(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(valuesPath)
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := yaml.Unmarshal(raw, &v); err != nil {
		t.Fatalf("parse values.yaml: %v", err)
	}
	return v
}

// setPath sets a dotted path in a nested map, like `helm --set a.b=c`.
func setPath(m map[string]any, path string, val any) {
	parts := strings.Split(path, ".")
	for _, p := range parts[:len(parts)-1] {
		next, ok := m[p].(map[string]any)
		if !ok {
			next = map[string]any{}
			m[p] = next
		}
		m = next
	}
	m[parts[len(parts)-1]] = val
}

// renderDeployment executes the deployment template with the chart's default values plus overrides.
func renderDeployment(t *testing.T, tplText string, overrides map[string]any) string {
	t.Helper()
	values := loadChartValues(t)
	for k, v := range overrides {
		setPath(values, k, v)
	}
	tpl, err := template.New("deployment").Funcs(template.FuncMap{
		"include": func(name string, _ any) string { return "stub-" + name },
		"toYaml":  func(v any) string { return "" },
		"nindent": func(n int, s string) string { return s },
	}).Parse(tplText) // a missing key is nil, as in helm: `with .Values.image.pullSecrets` is simply false
	if err != nil {
		t.Fatalf("parse deployment.yaml: %v", err)
	}
	var out bytes.Buffer
	data := map[string]any{"Values": values, "Release": map[string]any{"Name": "t"}, "Chart": map[string]any{"Name": "cleat"}}
	if err := tpl.Execute(&out, data); err != nil {
		t.Fatalf("render deployment.yaml: %v", err)
	}
	return out.String()
}

// active drops YAML comment lines: a comment that mentions the flag is not the flag.
func active(rendered string) string {
	var keep []string
	for _, l := range strings.Split(rendered, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(l), "#") {
			keep = append(keep, l)
		}
	}
	return strings.Join(keep, "\n")
}

type adminCase struct {
	name                string
	set                 map[string]any
	wantFlag, wantDrain bool
}

var adminCases = []adminCase{
	{"defaults", nil, false, false},
	{"a drain key alone", map[string]any{"auth.adminApiKey": "k"}, false, false},
	{"an existing secret alone", map[string]any{"auth.existingSecret": "s"}, false, false},
	{"admin API on, no key", map[string]any{"adminApi.enabled": true}, true, false},
	{"admin API on, with a key", map[string]any{"adminApi.enabled": true, "auth.adminApiKey": "k"}, true, true},
	{"admin API on, with an existing secret", map[string]any{"adminApi.enabled": true, "auth.existingSecret": "s"}, true, true},
}

func TestTheChartRendersTheAdminAPIFlagOnlyWhenAdminAPIIsEnabled(t *testing.T) {
	src, err := os.ReadFile(deploymentPath)
	if err != nil {
		t.Fatal(err)
	}
	tplText := string(src)

	// Vacuity: the render has to be able to produce the flag at all, or every "absent" below is trivial.
	on := active(renderDeployment(t, tplText, map[string]any{"adminApi.enabled": true}))
	if !strings.Contains(on, `"--enable-admin-api"`) {
		t.Fatalf("rendering with adminApi.enabled=true did not produce the flag: this render measures nothing\n%s", on)
	}
	if got := loadChartValues(t)["adminApi"]; got == nil {
		t.Fatal("values.yaml has no adminApi block")
	}

	for _, c := range adminCases {
		t.Run(c.name, func(t *testing.T) {
			out := active(renderDeployment(t, tplText, c.set))
			if has := strings.Contains(out, "--enable-admin-api"); has != c.wantFlag {
				t.Errorf("[%v] --enable-admin-api present = %v, want %v. Turning the admin API on must be an explicit "+
					"choice (adminApi.enabled), not a side effect of another value (cleat#2267).", c.set, has, c.wantFlag)
			}
			if has := strings.Contains(out, "/api/admin/drain"); has != c.wantDrain {
				t.Errorf("[%v] the preStop drain call present = %v, want %v", c.set, has, c.wantDrain)
			}
			// Whatever the hook is, it is one of the two: it drains through the route or it sleeps.
			if sleeps := regexp.MustCompile(`"sleep \d+"`).MatchString(out); sleeps == c.wantDrain {
				t.Errorf("[%v] preStop sleeps = %v with drain = %v: exactly one of them must be the hook", c.set, sleeps, c.wantDrain)
			}
		})
	}
}

// The known-positive: the two mistakes the first version of the test could not see. Each is applied to the
// template and rendered; the assertion above must then FAIL for at least one case, or it cannot fail.
func TestTheChartAdminAPICheckReportsTheMistakesItExistsFor(t *testing.T) {
	src, err := os.ReadFile(deploymentPath)
	if err != nil {
		t.Fatal(err)
	}
	orig := string(src)
	guard := "{{- if .Values.adminApi.enabled }}\n            # Off unless asked for"
	if strings.Count(orig, guard) != 1 {
		t.Fatal("cannot build the known-positives: the guarded block is not where the test expects it")
	}
	for name, cond := range map[string]string{
		"an OR with the drain key (the first draft's behaviour)": "{{- if or .Values.auth.adminApiKey .Values.adminApi.enabled }}",
		"the condition inverted":                                 "{{- if not .Values.adminApi.enabled }}",
	} {
		mutated := strings.Replace(orig, guard, cond+"\n            # Off unless asked for", 1)
		reported := false
		for _, c := range adminCases {
			out := active(renderDeployment(t, mutated, c.set))
			if strings.Contains(out, "--enable-admin-api") != c.wantFlag {
				reported = true
			}
		}
		if !reported {
			t.Errorf("%s: no case reported it, so the check cannot fail for it", name)
		}
	}
}

// The pod must live long enough to drain (cleat#2285). SIGTERM makes the worker drain for --shutdown-grace
// before it cancels its runs, the preStop hook comes first and comes out of the same clock, and the kubelet
// SIGKILLs the pod when terminationGracePeriodSeconds runs out. Kubernetes' default of 30s is shorter than a
// 20s drain plus a 5s preStop plus the release writes, so a chart that sets no value, or one that raises the
// grace without the deadline, kills the worker mid-drain and every run in flight is recovered by the reaper
// as after a crash. Nothing fails when it is wrong: the pod just terminates.

// drainBudgetProblem reports why the rendered deployment cannot drain, or "" if it can.
func drainBudgetProblem(out string) string {
	tg := regexp.MustCompile(`terminationGracePeriodSeconds:\s*(\d+)`).FindStringSubmatch(out)
	if tg == nil {
		return "no terminationGracePeriodSeconds is rendered, so the kubelet applies its 30s default"
	}
	gr := regexp.MustCompile(`"--shutdown-grace=([0-9a-z.]+)"`).FindStringSubmatch(out)
	if gr == nil {
		return "no --shutdown-grace is rendered"
	}
	grace, err := time.ParseDuration(gr[1])
	if err != nil {
		return "--shutdown-grace is not a duration: " + gr[1]
	}
	sleep := 0
	if m := regexp.MustCompile(`"sleep (\d+)"`).FindStringSubmatch(out); m != nil {
		sleep, _ = strconv.Atoi(m[1])
	}
	deadline, _ := strconv.Atoi(tg[1])
	const margin = 10 // release writes and process exit
	if need := int(grace.Seconds()) + sleep + margin; deadline < need {
		return fmt.Sprintf("terminationGracePeriodSeconds is %d but the drain needs %d (shutdown-grace %s + preStop sleep %ds + %ds margin)",
			deadline, need, grace, sleep, margin)
	}
	return ""
}

func TestTheChartGivesThePodTimeToDrain(t *testing.T) {
	src, err := os.ReadFile(deploymentPath)
	if err != nil {
		t.Fatal(err)
	}
	orig := string(src)

	for _, set := range []map[string]any{
		nil,
		{"adminApi.enabled": true, "auth.adminApiKey": "k"},
		{"worker.shutdownGrace": "40s", "worker.terminationGracePeriodSeconds": 90},
	} {
		if p := drainBudgetProblem(active(renderDeployment(t, orig, set))); p != "" {
			t.Errorf("[%v] %s", set, p)
		}
	}

	// Known-positives: each is a way to ship a chart that cannot drain, and the check must say so.
	for name, mutated := range map[string]struct {
		tpl string
		set map[string]any
	}{
		"the deadline left at the Kubernetes default": {strings.Replace(orig, "      terminationGracePeriodSeconds: {{ .Values.worker.terminationGracePeriodSeconds }}\n", "", 1), nil},
		"the grace raised without the deadline":       {orig, map[string]any{"worker.shutdownGrace": "55s"}},
		"the flag dropped":                            {strings.Replace(orig, `            - "--shutdown-grace={{ .Values.worker.shutdownGrace }}"`+"\n", "", 1), nil},
	} {
		if mutated.tpl == orig && mutated.set == nil {
			t.Fatalf("%s: the mutation did not change the template", name)
		}
		if drainBudgetProblem(active(renderDeployment(t, mutated.tpl, mutated.set))) == "" {
			t.Errorf("%s: the check reported nothing, so it cannot fail for it", name)
		}
	}
}
