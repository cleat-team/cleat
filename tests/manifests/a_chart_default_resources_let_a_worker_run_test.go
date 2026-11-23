package manifests

// The chart's default resources and autoscaling let a worker RUN, and do not pin the autoscaler at its maximum (cleat#4393).
//
// The old defaults were requests 100m / 128Mi and limits 500m / 512Mi, with an HPA on CPU 70% AND memory 80%. Measured 2026-11-23 (arm64 Linux): a Python
// component compile was OOM-killed (exit 137) at a 512m cgroup limit, for both Python builds, and completed at 768m, 1g and 2g; a Python worker's baseline
// after one guest has run is about 565 MiB; the same compile took 11.4 s at a 0.5 CPU limit against 1.2 to 2.4 s unconstrained; and a memory target of 80%
// of a 128Mi request is 102 MiB, below the 207 MiB a warmed Go worker uses, so the HPA computes more replicas, forever, until maxReplicas.
//
// WHAT THIS PINS, as invariants over the values and the rendered HPA and not as the particular numbers: the memory limit clears the smallest limit a Python
// compile survived with margin (1 GiB), the CPU limit leaves a compile room (2 CPUs, or none), a request never exceeds its limit, and the default HPA
// scales on CPU only while an explicit targetMemory still renders a memory metric (the control that the absence is the chart's choice and not a template
// that can no longer render one). The render is Go's own text/template over the real files, not helm; helm ran on the same configurations for the PR.

import (
	"strconv"
	"strings"
	"testing"
)

// quantity parses the Kubernetes quantities this chart uses: a plain number, a "m" suffix (milli) or Ki/Mi/Gi. It returns base units (bytes, or cores x 1000 for "m").
func quantity(t *testing.T, v any) float64 {
	t.Helper()
	s := strings.TrimSpace(strings.Trim(strings.TrimSpace(toString(v)), `"`))
	mult := 1.0
	for suf, m := range map[string]float64{"Ki": 1 << 10, "Mi": 1 << 20, "Gi": 1 << 30, "m": 1e-3} {
		if strings.HasSuffix(s, suf) {
			s, mult = strings.TrimSuffix(s, suf), m
			break
		}
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		t.Fatalf("cannot parse the Kubernetes quantity %q: %v", v, err)
	}
	return f * mult
}

func toString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	}
	return ""
}

func resourceValue(t *testing.T, values map[string]any, kind, res string) (float64, bool) {
	t.Helper()
	r, _ := values["resources"].(map[string]any)
	k, _ := r[kind].(map[string]any)
	v, ok := k[res]
	if !ok {
		return 0, false
	}
	return quantity(t, v), true
}

func TestTheChartsDefaultResourcesLetAWorkerRun(t *testing.T) {
	values := loadChartValues(t)
	memLimit, ok := resourceValue(t, values, "limits", "memory")
	if !ok {
		t.Fatal("the chart sets no memory limit, which is a decision this test should be told about")
	}
	if min := float64(1 << 30); memLimit < min {
		t.Errorf("limits.memory is %.0f MiB, under 1 GiB: a Python component compile peaks near 500 MiB above a ~565 MiB baseline and was OOM-killed at 512 MiB", memLimit/(1<<20))
	}
	if cpuLimit, ok := resourceValue(t, values, "limits", "cpu"); ok && cpuLimit < 2 {
		t.Errorf("limits.cpu is %.2f, under 2: a Python compile took 11.4 s at 0.5 CPU against 1.2 to 2.4 s unconstrained", cpuLimit)
	}
	for _, res := range []string{"memory", "cpu"} {
		req, rok := resourceValue(t, values, "requests", res)
		lim, lok := resourceValue(t, values, "limits", res)
		if rok && lok && req > lim {
			t.Errorf("requests.%s (%v) exceeds limits.%s (%v): the pod would be rejected", res, req, res, lim)
		}
	}
}

// The owner's decision, 2026-11-23: a worker RESERVES its memory, so the memory request equals the limit, in the chart and in both raw manifests. Pinned as
// equality and not as 2Gi: the number is values.yaml's, and the rule is the decision.
func TestAWorkersMemoryRequestEqualsItsLimit(t *testing.T) {
	values := loadChartValues(t)
	req, rok := resourceValue(t, values, "requests", "memory")
	lim, lok := resourceValue(t, values, "limits", "memory")
	if !rok || !lok || req != lim {
		t.Errorf("charts/cleat/values.yaml: requests.memory = %v, limits.memory = %v; a worker reserves its memory, so they must be equal (rok=%v lok=%v)", req, lim, rok, lok)
	}
	for _, tc := range []struct{ path, kind string }{{"../../k8s/deployment.yaml", "Deployment"}, {"../../k8s/migrate-job.yaml", "Job"}} {
		r := containerResources(t, tc.path, tc.kind)
		rq, _ := r["requests"].(map[string]any)
		lm, _ := r["limits"].(map[string]any)
		if quantity(t, rq["memory"]) != quantity(t, lm["memory"]) {
			t.Errorf("%s: requests.memory = %v, limits.memory = %v; they must be equal", tc.path, rq["memory"], lm["memory"])
		}
	}
}

func TestTheDefaultAutoscalerScalesOnCPUAndNotOnMemory(t *testing.T) {
	def := renderHPA(t, nil)
	if !strings.Contains(def, "name: cpu") {
		t.Errorf("the default HPA has no CPU metric:\n%s", def)
	}
	if strings.Contains(def, "name: memory") {
		t.Errorf("the default HPA scales on memory, which grows with caches and held runs and pins the autoscaler at maxReplicas:\n%s", def)
	}
	// The control: an explicit targetMemory still renders the metric, so the absence above is the default's doing.
	set := renderHPA(t, map[string]any{"autoscaling.targetMemory": 75})
	if !strings.Contains(set, "name: memory") || !strings.Contains(set, "averageUtilization: 75") {
		t.Errorf("setting autoscaling.targetMemory=75 does not render a memory metric of 75%%:\n%s", set)
	}
	if !strings.Contains(set, "name: cpu") {
		t.Errorf("setting a memory target dropped the CPU metric:\n%s", set)
	}
}

// The raw manifests under k8s/ carry the SAME defaults as the chart, with the same defect fixed (cleat#4393): they said 128Mi / 512Mi, a Python compile is
// killed at 512Mi, and their HPA scaled on memory as well. The owner's rule is that the defaults work smoothly wherever they come from, so this pins the two
// manifests to the chart's values.yaml by VALUE (the chart is the source) and not to numbers written a second time here, and pins the manifest HPA to CPU only.

// containerResources returns the first container's resources of the first document of kind in path.
func containerResources(t *testing.T, path, kind string) map[string]any {
	t.Helper()
	for _, doc := range decodeYAMLDocs(t, path) {
		if doc["kind"] != kind {
			continue
		}
		spec, _ := doc["spec"].(map[string]any)
		tmpl, _ := spec["template"].(map[string]any)
		if kind == "Job" {
			podSpec, _ := tmpl["spec"].(map[string]any)
			cs, _ := podSpec["containers"].([]any)
			c, _ := cs[0].(map[string]any)
			r, _ := c["resources"].(map[string]any)
			return r
		}
		podSpec, _ := tmpl["spec"].(map[string]any)
		cs, _ := podSpec["containers"].([]any)
		c, _ := cs[0].(map[string]any)
		r, _ := c["resources"].(map[string]any)
		return r
	}
	t.Fatalf("%s: no %s document found", path, kind)
	return nil
}

func TestTheRawManifestsCarryTheChartsDefaultResources(t *testing.T) {
	chart, _ := loadChartValues(t)["resources"].(map[string]any)
	for _, tc := range []struct{ path, kind string }{
		{"../../k8s/deployment.yaml", "Deployment"},
		{"../../k8s/migrate-job.yaml", "Job"},
	} {
		got := containerResources(t, tc.path, tc.kind)
		for _, kind := range []string{"requests", "limits"} {
			cm, _ := chart[kind].(map[string]any)
			gm, _ := got[kind].(map[string]any)
			for _, res := range []string{"cpu", "memory"} {
				if want, have := quantity(t, cm[res]), quantity(t, gm[res]); want != have {
					t.Errorf("%s %s.%s = %v, but charts/cleat/values.yaml says %v: the raw manifest has drifted from the chart (cleat#4393)", tc.path, kind, res, gm[res], cm[res])
				}
			}
		}
	}
}

func TestTheRawManifestsAutoscalerScalesOnCPUAndNotOnMemory(t *testing.T) {
	for _, doc := range decodeYAMLDocs(t, "../../k8s/deployment.yaml") {
		if doc["kind"] != "HorizontalPodAutoscaler" {
			continue
		}
		spec, _ := doc["spec"].(map[string]any)
		metrics, _ := spec["metrics"].([]any)
		var names []string
		for _, m := range metrics {
			mm, _ := m.(map[string]any)
			if res, ok := mm["resource"].(map[string]any); ok {
				names = append(names, toString(res["name"]))
			}
		}
		hasCPU, hasMem := false, false
		for _, n := range names {
			hasCPU = hasCPU || n == "cpu"
			hasMem = hasMem || n == "memory"
		}
		if !hasCPU {
			t.Errorf("k8s/deployment.yaml's HPA has no CPU resource metric (has %v)", names)
		}
		if hasMem {
			t.Errorf("k8s/deployment.yaml's HPA scales on memory (%v), which pins it at maxReplicas (cleat#4393)", names)
		}
		return
	}
	t.Fatal("k8s/deployment.yaml has no HorizontalPodAutoscaler")
}
