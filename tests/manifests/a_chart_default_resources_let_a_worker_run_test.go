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
