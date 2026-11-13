package manifests

// cleat#3761. Under Helm 4's default server-side apply, a Deployment whose spec.replicas is written by BOTH the
// chart and the HorizontalPodAutoscaler makes the next `helm upgrade` fail with a field-ownership conflict once
// the autoscaler has changed the count (measured on k3s v1.31.5, Helm 4.3.0: "conflict with "k3s" with
// subresource "scale" using apps/v1: .spec.replicas", three times out of three). The chart therefore renders
// `replicas` only when the autoscaler is OFF, so exactly one of the two owns the count.
//
// WHAT THIS PINS is that rule as an invariant over the two templates' OUTPUT rather than as a text match on one
// of them: for each value of autoscaling.enabled, either the Deployment sets replicas or the chart renders an
// HPA, never both and never neither. A check that deployment.yaml mentions autoscaling.enabled would pass for a
// condition that is inverted or that does not match hpa.yaml's. The render is Go's own text/template over the
// real files (see renderDeployment); it is not helm, and helm was run on the same configurations on a real
// cluster for the PR that added this.

import (
	"bytes"
	"os"
	"regexp"
	"strings"
	"testing"
	"text/template"
)

const hpaPath = "../../charts/cleat/templates/hpa.yaml"

var specReplicas = regexp.MustCompile(`(?m)^  replicas:\s*(\S+)\s*$`)

func renderHPA(t *testing.T, overrides map[string]any) string {
	t.Helper()
	raw, err := os.ReadFile(hpaPath)
	if err != nil {
		t.Fatal(err)
	}
	values := loadChartValues(t)
	for k, v := range overrides {
		setPath(values, k, v)
	}
	tpl, err := template.New("hpa").Funcs(template.FuncMap{
		"include": func(name string, _ any) string { return "stub-" + name },
		"nindent": func(n int, s string) string { return s },
	}).Parse(string(raw))
	if err != nil {
		t.Fatalf("parse hpa.yaml: %v", err)
	}
	var out bytes.Buffer
	data := map[string]any{"Values": values, "Release": map[string]any{"Name": "t"}, "Chart": map[string]any{"Name": "cleat"}}
	if err := tpl.Execute(&out, data); err != nil {
		t.Fatalf("execute hpa.yaml: %v", err)
	}
	return out.String()
}

func TestExactlyOneOfTheChartAndTheAutoscalerOwnsTheReplicaCount(t *testing.T) {
	raw, err := os.ReadFile(deploymentPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name         string
		overrides    map[string]any
		wantReplicas string // "" = the Deployment must not set it
		wantHPA      bool
	}{
		{"autoscaling on (the default)", nil, "", true},
		{"autoscaling on, replicaCount changed", map[string]any{"replicaCount": 7}, "", true},
		{"autoscaling off", map[string]any{"autoscaling.enabled": false}, "3", false},
		{"autoscaling off, replicaCount changed", map[string]any{"autoscaling.enabled": false, "replicaCount": 5}, "5", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dep := renderDeployment(t, string(raw), tc.overrides)
			hpa := renderHPA(t, tc.overrides)
			m := specReplicas.FindStringSubmatch(dep)
			got := ""
			if m != nil {
				got = m[1]
			}
			if got != tc.wantReplicas {
				t.Errorf("Deployment spec.replicas = %q, want %q", got, tc.wantReplicas)
			}
			hasHPA := strings.Contains(hpa, "kind: HorizontalPodAutoscaler")
			if hasHPA != tc.wantHPA {
				t.Errorf("an HPA is rendered = %v, want %v", hasHPA, tc.wantHPA)
			}
			// The invariant itself, so it fails for ANY combination that breaks it, not only the four above.
			if (got != "") == hasHPA {
				t.Errorf("the count has %d owners (Deployment replicas %q, HPA rendered %v): want exactly one",
					map[bool]int{true: 2, false: 0}[got != ""], got, hasHPA)
			}
		})
	}
}

// The scale-up that ends a stop-first upgrade is written in three places -- the chart's refusal message,
// docs/operations/upgrading.md and the CHANGELOG entry for this change -- and they must say the same thing.
// The coordinator's decision (cleat#3761): with autoscaling on it is UNCONDITIONAL. Under Helm 4's default
// server-side apply the count is always 0 after the upgrade, so wording like "check the Deployment, and if it
// reads 0" turns the default path into a step people skip, and the workers stay down with no error.
//
// What this checks is that each of the three carries the command with the placeholder for the floor, and that
// none of them carries the conditional wording. It cannot check that the three agree in every other respect;
// that is what review of the prose is for.
func TestTheStopFirstScaleUpIsUnconditionalInEveryPlaceItIsWritten(t *testing.T) {
	const command = "--replicas=<autoscaling.minReplicas>"
	conditional := []string{"if it reads 0", "if it reads 0/0", "check it after the upgrade", "if the Deployment reads"}

	read := func(path string) string {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	section := func(text, startMarker, endMarker string) string {
		i := strings.Index(text, startMarker)
		if i < 0 {
			t.Fatalf("marker %q not found", startMarker)
		}
		rest := text[i:]
		if j := strings.Index(rest[len(startMarker):], endMarker); j >= 0 {
			rest = rest[:len(startMarker)+j]
		}
		return rest
	}

	for name, text := range map[string]string{
		"the refusal message":                 upgradeRefusal(t, guardInput(worker05, 3, worker06, false)),
		"docs/operations/upgrading.md (Helm)": section(read("../../docs/operations/upgrading.md"), "- **Helm.**", "- **Raw Kubernetes"),
		"the CHANGELOG entry for cleat#3761":  section(read("../../CHANGELOG.md"), "- **Behaviour change (Helm chart): the Deployment no longer sets", "\n- "),
	} {
		t.Run(name, func(t *testing.T) {
			if !strings.Contains(text, command) {
				t.Errorf("does not carry the scale-up command %q", command)
			}
			for _, bad := range conditional {
				if strings.Contains(text, bad) {
					t.Errorf("carries the conditional wording %q; the scale-up is unconditional when autoscaling is on", bad)
				}
			}
		})
	}
}
