package manifests

// cleat#2196's reaper-to-worker veto channel needs each worker pod to be
// reachable by a stable DNS name. The owner's decision was a headless
// Service (clusterIP: None) plus the pod spec's own spec.subdomain naming
// it -- not a captured pod IP. That only works if three things agree:
// the headless Service's name, the pod spec's subdomain, and the pod spec's
// --worker-service-name flag (which the worker process uses to build its
// own address, see cmd/cleat-worker/connection_share.go's podAddress). A
// typo in any one of the three leaves the mechanism silently inert: nothing
// errors, the pod just never gets the DNS record cleat#2196 depends on.
//
// k8s/*.yaml is checked by direct YAML parse -- no templating, so no stub
// to get wrong. The Helm chart is textual: its own test file
// (a_chart_enables_the_admin_api_only_when_asked_test.go) already explains
// why a stubbed include() cannot be used to prove two expressions resolve
// to the SAME string (the stub renders any include() call identically
// regardless of its argument), so this asserts the raw template
// expressions are character-for-character identical instead, which is the
// property that actually matters: whatever cleat.fullname resolves to on a
// given release, both files resolve it the same way.

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// k8sService is the subset of a Service manifest this test reads.
type k8sService struct {
	Kind string `yaml:"kind"`
	Meta struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		ClusterIP string            `yaml:"clusterIP"`
		Selector  map[string]string `yaml:"selector"`
	} `yaml:"spec"`
}

// decodeYAMLDocs splits a multi-document YAML file (separated by `---`) into
// one map per document, skipping documents that parse as nil (a leading or
// trailing separator with nothing between).
func decodeYAMLDocs(t *testing.T, path string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	var docs []map[string]any
	for {
		var doc map[string]any
		err := dec.Decode(&doc)
		if err != nil {
			break
		}
		if doc != nil {
			docs = append(docs, doc)
		}
	}
	return docs
}

// headlessServiceName finds the one Service in path whose spec.clusterIP is
// "None", and fails if there is not EXACTLY one -- zero means the headless
// Service was removed or renamed to something this cannot recognise, and
// more than one means a second headless Service was added that this test's
// assumptions (and cleat#2196's single-channel design) do not expect.
func headlessServiceName(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	var found []k8sService
	for {
		var svc k8sService
		if err := dec.Decode(&svc); err != nil {
			break
		}
		if svc.Kind == "Service" && svc.Spec.ClusterIP == "None" {
			found = append(found, svc)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%s: found %d headless (clusterIP: None) Service(s), want exactly 1: %+v", path, len(found), found)
	}
	return found[0].Meta.Name
}

// podSubdomain extracts spec.template.spec.subdomain from a Deployment
// manifest. k8s/deployment.yaml is plain YAML (no Helm templating), so this
// can decode it directly rather than text-scanning.
func podSubdomain(t *testing.T, path string) string {
	t.Helper()
	docs := decodeYAMLDocs(t, path)
	for _, doc := range docs {
		if doc["kind"] != "Deployment" {
			continue
		}
		spec, _ := doc["spec"].(map[string]any)
		tmpl, _ := spec["template"].(map[string]any)
		podSpec, _ := tmpl["spec"].(map[string]any)
		sub, _ := podSpec["subdomain"].(string)
		return sub
	}
	t.Fatalf("%s: no Deployment document found", path)
	return ""
}

func TestK8sManifestsAgreeOnTheHeadlessServiceName(t *testing.T) {
	svcName := headlessServiceName(t, "../../k8s/service.yaml")
	sub := podSubdomain(t, "../../k8s/deployment.yaml")
	if sub != svcName {
		t.Errorf("k8s/deployment.yaml's pod subdomain = %q, but k8s/service.yaml's headless Service is named %q -- "+
			"a pod only gets a per-pod DNS record under a headless Service whose name matches its subdomain exactly",
			sub, svcName)
	}
	if sub == "" {
		t.Fatal("k8s/deployment.yaml's pod subdomain is empty -- no per-pod DNS record is possible")
	}

	raw, err := os.ReadFile("../../k8s/deployment.yaml")
	if err != nil {
		t.Fatal(err)
	}
	wantArg := fmt.Sprintf("--worker-service-name=%s", svcName)
	if !strings.Contains(string(raw), wantArg) {
		t.Errorf("k8s/deployment.yaml does not pass %q -- the worker process has no way to learn the "+
			"headless Service's name, so it cannot publish its own address (see cmd/cleat-worker's podAddress)", wantArg)
	}
}

// chartHeadlessServiceNameExpr and chartSubdomainExpr pull the raw Helm
// template EXPRESSION (not a rendered value -- there is no renderer here
// that can be trusted to resolve include() faithfully, see this file's own
// header) for the headless Service's name and the pod's subdomain,
// respectively. Both must be the literal same text for the invariant to
// hold on any release name.
var headlessServiceNameLine = regexp.MustCompile(`(?m)^\s*name:\s*(\{\{.*\}\}-headless)\s*$`)
var subdomainLine = regexp.MustCompile(`(?m)^\s*subdomain:\s*(\{\{.*\}\}-headless)\s*$`)

func TestHelmChartAgreesOnTheHeadlessServiceName(t *testing.T) {
	svcRaw, err := os.ReadFile("../../charts/cleat/templates/service.yaml")
	if err != nil {
		t.Fatal(err)
	}
	// There are two `name:` lines carrying cleat.fullname in this file (the
	// ordinary Service and the headless one) -- anchor on the one followed
	// by "-headless" in the same expression, rather than the first match,
	// so this does not silently bind to the wrong Service if their order
	// in the file ever changes.
	svcMatches := headlessServiceNameLine.FindAllStringSubmatch(string(svcRaw), -1)
	if len(svcMatches) != 1 {
		t.Fatalf("charts/cleat/templates/service.yaml: found %d name: line(s) matching a headless-service "+
			"pattern, want exactly 1 (got %v)", len(svcMatches), svcMatches)
	}
	svcExpr := svcMatches[0][1]

	depRaw, err := os.ReadFile("../../charts/cleat/templates/deployment.yaml")
	if err != nil {
		t.Fatal(err)
	}
	depMatches := subdomainLine.FindAllStringSubmatch(string(depRaw), -1)
	if len(depMatches) != 1 {
		t.Fatalf("charts/cleat/templates/deployment.yaml: found %d subdomain: line(s), want exactly 1 (got %v)",
			len(depMatches), depMatches)
	}
	subExpr := depMatches[0][1]

	if svcExpr != subExpr {
		t.Errorf("the headless Service's name expression (%q, in service.yaml) and the pod's subdomain "+
			"expression (%q, in deployment.yaml) are not the same text -- they must resolve identically "+
			"on every release for the per-pod DNS record to exist", svcExpr, subExpr)
	}

	wantArg := "--worker-service-name=" + subExpr
	if !strings.Contains(string(depRaw), wantArg) {
		t.Errorf("charts/cleat/templates/deployment.yaml does not pass %q -- the worker process has no way "+
			"to learn the headless Service's name, so it cannot publish its own address", wantArg)
	}
}
