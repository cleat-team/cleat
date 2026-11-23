package manifests

// cleat#4439. The owner's requirement (2026-11-23): "The 2 workers should be on separate hardware, not together as containers on the same physical machine."
// The autoscaler's floor is two workers, the chart set no affinity, and nothing else keeps them apart: only the scheduler's own, unguaranteed spreading. A
// node drain could also take both, because there was no PodDisruptionBudget anywhere in charts/ or k8s/.
//
// WHAT THIS PINS, for the chart and for the raw manifests alike:
//   - the Deployment carries a topologySpreadConstraint on kubernetes.io/hostname with maxSkew 1, and it is a PREFERENCE by default (ScheduleAnyway), so a one-node
//     cluster still starts every worker. The strict value renders DoNotSchedule, a zone key adds a second constraint, and `enabled: false` renders none;
//   - a PodDisruptionBudget exists with maxUnavailable 1, and `enabled: false` renders none;
//   - every selector here is the STABLE selector (cleat.selectorLabels in the chart, spec.selector in the raw manifests) and not the full label set. The full set
//     carries the chart version, so a constraint or budget built on it stops matching the old pods after an upgrade;
//   - the chart's defaults and the raw manifests' agree, so the two cannot drift apart the way the memory defaults did.
//
// The chart is rendered by Go's own text/template with sprig stubbed (see renderDeployment), which can see a template's structure and not its labels' text. What
// the scheduler and the API server DO with it was measured on a kind cluster for the PR that added this, not here.

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

const pdbTemplatePath = "../../charts/cleat/templates/pdb.yaml"

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// constraintBlock returns the rendered text after `topologySpreadConstraints:` ("" when there is none). It is the last key the template renders.
func constraintBlock(rendered string) string {
	i := strings.Index(rendered, "topologySpreadConstraints:")
	if i < 0 {
		return ""
	}
	return rendered[i:]
}

var (
	reSkew      = regexp.MustCompile(`maxSkew:\s*(\S+)`)
	reTopoKey   = regexp.MustCompile(`topologyKey:\s*(\S+)`)
	reWhenUnsa  = regexp.MustCompile(`whenUnsatisfiable:\s*(\S+)`)
	reStableSel = regexp.MustCompile(`matchLabels:\s*stub-cleat\.selectorLabels`)
)

func TestTheChartSpreadsWorkersAcrossNodesAsAPreferenceByDefault(t *testing.T) {
	dep := readFile(t, deploymentPath)
	block := constraintBlock(renderDeployment(t, dep, nil))
	if block == "" {
		t.Fatal("the default render carries no topologySpreadConstraints: nothing keeps the two workers apart")
	}
	if got := reSkew.FindAllStringSubmatch(block, -1); len(got) != 1 || got[0][1] != "1" {
		t.Errorf("default maxSkew = %v, want exactly one constraint with maxSkew 1", got)
	}
	if got := reTopoKey.FindAllStringSubmatch(block, -1); len(got) != 1 || got[0][1] != "kubernetes.io/hostname" {
		t.Errorf("default topologyKey = %v, want exactly one, kubernetes.io/hostname (one domain per node)", got)
	}
	// The default is a PREFERENCE: a one-node cluster, or one whose other nodes are full or cordoned, must still start every worker.
	if got := reWhenUnsa.FindAllStringSubmatch(block, -1); len(got) != 1 || got[0][1] != "ScheduleAnyway" {
		t.Errorf("default whenUnsatisfiable = %v, want ScheduleAnyway: the default must not refuse to schedule", got)
	}
}

func TestTheChartSpreadIsStrictOnRequestHasAZoneOptionAndCanBeTurnedOff(t *testing.T) {
	dep := readFile(t, deploymentPath)

	strict := constraintBlock(renderDeployment(t, dep, map[string]any{"topologySpread.whenUnsatisfiable": "DoNotSchedule"}))
	if got := reWhenUnsa.FindAllStringSubmatch(strict, -1); len(got) != 1 || got[0][1] != "DoNotSchedule" {
		t.Errorf("strict mode renders whenUnsatisfiable = %v, want one DoNotSchedule", got)
	}

	zone := constraintBlock(renderDeployment(t, dep, map[string]any{"topologySpread.zoneKey": "topology.kubernetes.io/zone"}))
	keys := reTopoKey.FindAllStringSubmatch(zone, -1)
	if len(keys) != 2 || keys[0][1] != "kubernetes.io/hostname" || keys[1][1] != "topology.kubernetes.io/zone" {
		t.Errorf("with zoneKey set the topology keys are %v, want hostname then the zone key", keys)
	}
	// A zone constraint must not turn the node constraint strict, nor the reverse: they are independent.
	if got := reWhenUnsa.FindAllStringSubmatch(zone, -1); len(got) != 2 || got[0][1] != "ScheduleAnyway" || got[1][1] != "ScheduleAnyway" {
		t.Errorf("with zoneKey set whenUnsatisfiable = %v, want ScheduleAnyway for both", got)
	}

	if off := constraintBlock(renderDeployment(t, dep, map[string]any{"topologySpread.enabled": false})); off != "" {
		t.Errorf("topologySpread.enabled=false still renders a constraint:\n%s", off)
	}
}

func TestTheChartsSpreadAndBudgetSelectTheWorkersByTheStableSelector(t *testing.T) {
	dep := renderDeployment(t, readFile(t, deploymentPath), map[string]any{"topologySpread.zoneKey": "topology.kubernetes.io/zone"})
	block := constraintBlock(dep)
	// One labelSelector per constraint, each the stable selector. A constraint selecting by cleat.labels would carry the chart version.
	if n, want := len(reStableSel.FindAllString(block, -1)), 2; n != want {
		t.Errorf("%d of the %d constraints select by cleat.selectorLabels", n, want)
	}
	if strings.Contains(block, "stub-cleat.labels") {
		t.Error("a constraint selects by cleat.labels, which carries the chart version and stops matching the old pods after an upgrade")
	}
	pdb := renderDeployment(t, readFile(t, pdbTemplatePath), nil)
	if !reStableSel.MatchString(pdb) {
		t.Errorf("the PodDisruptionBudget does not select by cleat.selectorLabels:\n%s", pdb)
	}
}

func TestTheChartRendersAPodDisruptionBudgetThatAllowsOneVoluntaryEviction(t *testing.T) {
	tpl := readFile(t, pdbTemplatePath)
	pdb := renderDeployment(t, tpl, nil)
	if !strings.Contains(pdb, "kind: PodDisruptionBudget") {
		t.Fatalf("the default render has no PodDisruptionBudget:\n%s", pdb)
	}
	if m := regexp.MustCompile(`maxUnavailable:\s*(\S+)`).FindStringSubmatch(pdb); m == nil || m[1] != "1" {
		t.Errorf("default maxUnavailable = %v, want 1: a drain must take one worker at a time", m)
	}
	if !strings.Contains(pdb, "apiVersion: policy/v1") {
		t.Errorf("the budget is not policy/v1 (policy/v1beta1 was removed in Kubernetes 1.25):\n%s", pdb)
	}
	if off := renderDeployment(t, tpl, map[string]any{"podDisruptionBudget.enabled": false}); strings.Contains(off, "PodDisruptionBudget") {
		t.Errorf("podDisruptionBudget.enabled=false still renders one:\n%s", off)
	}
	if pct := renderDeployment(t, tpl, map[string]any{"podDisruptionBudget.maxUnavailable": "25%"}); !strings.Contains(pct, "maxUnavailable: 25%") {
		t.Errorf("a percentage maxUnavailable does not render as given:\n%s", pct)
	}
}

// rawDeployment returns the worker Deployment's spec from k8s/deployment.yaml.
func rawDeployment(t *testing.T) map[string]any {
	t.Helper()
	for _, doc := range decodeYAMLDocs(t, "../../k8s/deployment.yaml") {
		if doc["kind"] == "Deployment" {
			spec, _ := doc["spec"].(map[string]any)
			return spec
		}
	}
	t.Fatal("k8s/deployment.yaml has no Deployment")
	return nil
}

func rawConstraint(t *testing.T) map[string]any {
	t.Helper()
	spec := rawDeployment(t)
	tmpl, _ := spec["template"].(map[string]any)
	podSpec, _ := tmpl["spec"].(map[string]any)
	cs, _ := podSpec["topologySpreadConstraints"].([]any)
	if len(cs) != 1 {
		t.Fatalf("k8s/deployment.yaml has %d topologySpreadConstraints, want 1", len(cs))
	}
	c, _ := cs[0].(map[string]any)
	return c
}

func rawBudget(t *testing.T) map[string]any {
	t.Helper()
	for _, doc := range decodeYAMLDocs(t, "../../k8s/pdb.yaml") {
		if doc["kind"] == "PodDisruptionBudget" {
			spec, _ := doc["spec"].(map[string]any)
			return spec
		}
	}
	t.Fatal("k8s/pdb.yaml has no PodDisruptionBudget")
	return nil
}

func matchLabels(m map[string]any) map[string]any {
	sel, _ := m["labelSelector"].(map[string]any)
	if sel == nil {
		sel, _ = m["selector"].(map[string]any)
	}
	ml, _ := sel["matchLabels"].(map[string]any)
	return ml
}

func TestTheRawManifestsSpreadWorkersAndBudgetThemTheSameWay(t *testing.T) {
	values := loadChartValues(t)
	spread, _ := values["topologySpread"].(map[string]any)
	budget, _ := values["podDisruptionBudget"].(map[string]any)

	c := rawConstraint(t)
	// The raw manifests and the chart must agree on the default, or one of them silently keeps two workers together.
	for _, f := range []struct{ raw, chart string }{{"maxSkew", "maxSkew"}, {"topologyKey", "topologyKey"}, {"whenUnsatisfiable", "whenUnsatisfiable"}} {
		if toString(c[f.raw]) != toString(spread[f.chart]) {
			t.Errorf("k8s/deployment.yaml %s = %v, but charts/cleat/values.yaml topologySpread.%s = %v", f.raw, c[f.raw], f.chart, spread[f.chart])
		}
	}
	if c["whenUnsatisfiable"] != "ScheduleAnyway" {
		t.Errorf("the raw manifests' default whenUnsatisfiable = %v, want ScheduleAnyway", c["whenUnsatisfiable"])
	}

	spec := rawDeployment(t)
	selector := matchLabels(map[string]any{"selector": spec["selector"]})
	if len(selector) == 0 {
		t.Fatal("the raw Deployment has no spec.selector.matchLabels")
	}
	same := func(what string, got map[string]any) {
		if len(got) != len(selector) {
			t.Errorf("%s selects %v, but the Deployment's spec.selector is %v", what, got, selector)
			return
		}
		for k, v := range selector {
			if got[k] != v {
				t.Errorf("%s selects %v, but the Deployment's spec.selector is %v", what, got, selector)
			}
		}
	}
	same("the topologySpreadConstraint", matchLabels(c))

	b := rawBudget(t)
	same("the PodDisruptionBudget", matchLabels(b))
	if toString(b["maxUnavailable"]) != toString(budget["maxUnavailable"]) {
		t.Errorf("k8s/pdb.yaml maxUnavailable = %v, but charts/cleat/values.yaml podDisruptionBudget.maxUnavailable = %v", b["maxUnavailable"], budget["maxUnavailable"])
	}
}
