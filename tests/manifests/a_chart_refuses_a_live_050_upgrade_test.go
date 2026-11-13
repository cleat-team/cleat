package manifests

// The chart refuses a helm upgrade from 0.5.x while 0.5.x workers are running (cleat#3758, D79).
//
// WHY. An in-place 0.5.0 to 0.6.0 upgrade is supported only if every 0.5.0 worker is stopped first. The chart's
// migration is a pre-upgrade hook, which Helm runs before it touches the release's other resources, so a plain
// `helm upgrade` migrated the schema under three Ready 0.5.0 pods and then rolled the workers, old beside new,
// and printed "Upgrade complete" (measured on a real cluster in cleat#3758). templates/upgrade-guard.yaml
// refuses that, before any hook, using `lookup` on the live Deployment.
//
// WHAT THIS PINS. The DECISION, which lives in templates/_upgrade-guard.tpl as a pure function of its inputs:
// when it refuses, when it stays silent, and what the refusal tells the user to do. The render is Go's own
// text/template over the real file, so every `and`, `not` and `regexMatch` is evaluated for real; only
// regexMatch, the one Helm (sprig) function the file uses, is supplied, and it is regexp.MatchString with
// sprig's argument order. It is not helm.
//
// WHAT THIS DOES NOT PIN, and nothing in this repo's CI does: that upgrade-guard.yaml feeds the decision the
// right inputs, and that Helm's `lookup` returns what the guard expects. Both were run on a real cluster for
// the PR that added this (k3s, Helm 4.3.0): refused with three 0.5.0 workers running, with no Job created and
// the migration count unchanged; allowed after a scale to 0. CI's `helm lint` and `helm template` (the chart
// dry-run job) show that the file renders, and `lookup` returns nothing there, so they say nothing about the
// refusal. The checks on upgrade-guard.yaml below are text checks and are weak on purpose-built grounds: they
// keep the removal pointer (cleat#3759) from being deleted and the `fail` from being dropped.

import (
	"bytes"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"text/template"
)

const (
	guardDefinePath = "../../charts/cleat/templates/_upgrade-guard.tpl"
	guardCallerPath = "../../charts/cleat/templates/upgrade-guard.yaml"
)

const (
	worker05  = "ghcr.io/cleat-team/cleat-worker:v0.5.0"
	worker06  = "ghcr.io/cleat-team/cleat-worker:v0.6.0"
	digest    = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	repoImage = "ghcr.io/cleat-team/cleat-worker"
)

// upgradeRefusal executes the real decision template. regexMatch has sprig's signature: (pattern, subject).
func upgradeRefusal(t *testing.T, in map[string]any) string {
	t.Helper()
	raw, err := os.ReadFile(guardDefinePath)
	if err != nil {
		t.Fatal(err)
	}
	tpl, err := template.New("guard").Funcs(template.FuncMap{
		"regexMatch": func(pattern, subject string) (bool, error) { return regexp.MatchString(pattern, subject) },
		// sprig's toString: nil is the empty string, anything else is its printed form.
		"toString": func(v any) string {
			if v == nil {
				return ""
			}
			return fmt.Sprint(v)
		},
	}).Parse(string(raw))
	if err != nil {
		t.Fatalf("parse %s: %v", guardDefinePath, err)
	}
	var buf bytes.Buffer
	if err := tpl.ExecuteTemplate(&buf, "cleat.upgradeRefusal", in); err != nil {
		t.Fatalf("execute cleat.upgradeRefusal: %v", err)
	}
	return strings.TrimSpace(buf.String())
}

func guardInput(live string, replicas int, newImage string, allow any) map[string]any {
	return map[string]any{
		"liveImage": live, "liveReplicas": replicas, "newImage": newImage, "allow": allow,
		"fullname": "cleat", "namespace": "exp", "name": "cleat", "release": "rel",
	}
}

func TestTheUpgradeGuardRefusesALive050Upgrade(t *testing.T) {
	refuse := []struct {
		name, live string
		replicas   int
		newImage   string
	}{
		{"the published tag", worker05, 3, worker06},
		{"a tag without the v", repoImage + ":0.5.0", 3, worker06},
		{"a later 0.5 patch", repoImage + ":0.5.3", 3, worker06},
		{"a tag of just 0.5", repoImage + ":0.5", 3, worker06},
		{"a pre-release", repoImage + ":v0.5.0-rc1", 3, worker06},
		{"a build suffix", repoImage + ":v0.5.0+build7", 3, worker06},
		{"a tag and a digest", repoImage + ":v0.5.0@" + digest, 3, worker06},
		{"an architecture suffix", repoImage + ":0.5.0_amd64", 3, worker06},
		{"a registry with a port", "registry.example:5000/cleat/worker:v0.5.0", 3, worker06},
		{"one worker is enough", worker05, 1, worker06},
		{"upgrading to latest", worker05, 3, repoImage + ":latest"},
		{"upgrading to a digest-only image", worker05, 3, repoImage + "@" + digest},
	}
	for _, c := range refuse {
		t.Run(c.name, func(t *testing.T) {
			got := upgradeRefusal(t, guardInput(c.live, c.replicas, c.newImage, false))
			if got == "" {
				t.Fatalf("live %q with %d replicas, upgrading to %q, was not refused: a plain helm upgrade would "+
					"migrate the schema under running 0.5.x workers", c.live, c.replicas, c.newImage)
			}
		})
	}
}

func TestTheUpgradeGuardStaysSilentWhenItCannotKnowOrNeedNotCare(t *testing.T) {
	silent := []struct {
		name, live string
		replicas   int
		newImage   string
		allow      bool
	}{
		// The workers are stopped. Replicas, not pods: a Deployment at 0 with pods still draining passes, which
		// the refusal message says out loud.
		{"no workers running", worker05, 0, worker06, false},
		// The operator's explicit override.
		{"the override is set", worker05, 3, worker06, true},
		// The live version cannot be known, so the guard does not guess (the chart's own default tag is latest).
		{"live tag is latest", repoImage + ":latest", 3, worker06, false},
		{"live image is a digest with no tag", repoImage + "@" + digest, 3, worker06, false},
		{"live image is empty (no worker container found)", "", 3, worker06, false},
		// Not 0.5.x.
		{"live is 0.6.0", worker06, 3, worker06, false},
		{"live is 0.4.0", repoImage + ":v0.4.0", 3, worker06, false},
		{"live is 0.15.0, not 0.5", repoImage + ":v0.15.0", 3, worker06, false},
		{"live is 0.50.0, not 0.5", repoImage + ":v0.50.0", 3, worker06, false},
		{"live is 0.5.0x, which is not a version", repoImage + ":v0.5.0x", 3, worker06, false},
		// 0.5.x to 0.5.y is not the step D79 is about.
		{"a patch upgrade inside 0.5", worker05, 3, repoImage + ":v0.5.1", false},
		{"a downgrade to 0.5", worker06, 3, worker05, false},
	}
	for _, c := range silent {
		t.Run(c.name, func(t *testing.T) {
			if got := upgradeRefusal(t, guardInput(c.live, c.replicas, c.newImage, c.allow)); got != "" {
				t.Fatalf("live %q with %d replicas, upgrading to %q (allow=%v), was refused, but the guard "+
					"must stay silent here:\n%s", c.live, c.replicas, c.newImage, c.allow, got)
			}
		})
	}
}

// The override fails CLOSED. `not .allow` is true for the string "false" (a non-empty string is truthy), so a
// quoted value in a values file or --set-string used to switch the guard OFF, which is the wrong way round for a
// safety check (cleat-review, on cleat#3763). Only the boolean true or the string "true" skips it; everything else,
// including a mistyped value, leaves the guard on.
func TestTheUpgradeGuardOverrideFailsClosed(t *testing.T) {
	skips := []struct {
		name  string
		allow any
	}{
		{"the boolean true", true},
		{"the string true", "true"},
	}
	for _, c := range skips {
		t.Run("skips: "+c.name, func(t *testing.T) {
			if got := upgradeRefusal(t, guardInput(worker05, 3, worker06, c.allow)); got != "" {
				t.Fatalf("allow=%#v was refused, but it is the override:\n%s", c.allow, got)
			}
		})
	}
	keeps := []struct {
		name  string
		allow any
	}{
		{"the boolean false", false},
		{"the string false", "false"},
		{"the string False", "False"},
		{"the empty string", ""},
		{"nil", nil},
		{"the string yes", "yes"},
		{"the string 1", "1"},
		{"the number 1", 1},
	}
	for _, c := range keeps {
		t.Run("keeps the guard on: "+c.name, func(t *testing.T) {
			if got := upgradeRefusal(t, guardInput(worker05, 3, worker06, c.allow)); got == "" {
				t.Fatalf("allow=%#v switched the guard off; only true may", c.allow)
			}
		})
	}
}

// The refusal is only useful if the user can act on it without reading anything else.
func TestTheUpgradeGuardRefusalSaysExactlyWhatToDo(t *testing.T) {
	got := upgradeRefusal(t, guardInput(worker05, 3, worker06, false))
	for _, want := range []string{
		"kubectl scale deployment/cleat --replicas=0 -n exp",
		"kubectl wait --for=delete pod -l app.kubernetes.io/name=cleat,app.kubernetes.io/instance=rel -n exp",
		"upgrade.allowLiveWorkers=true",
		"docs/operations/upgrading.md",
		// The limits, stated where the user reads the refusal.
		`"latest"`, "digest", "terminating",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the refusal does not contain %q:\n%s", want, got)
		}
	}
}

func TestTheUpgradeGuardIsDeclaredAndCarriesItsRemovalPointer(t *testing.T) {
	v := loadChartValues(t)
	up, ok := v["upgrade"].(map[string]any)
	if !ok {
		t.Fatal("values.yaml declares no `upgrade:` map, so --set upgrade.allowLiveWorkers=true is an undeclared value")
	}
	if allow, ok := up["allowLiveWorkers"].(bool); !ok || allow {
		t.Errorf("upgrade.allowLiveWorkers must default to false, got %#v", up["allowLiveWorkers"])
	}

	raw, err := os.ReadFile(guardCallerPath)
	if err != nil {
		t.Fatal(err)
	}
	caller := string(raw)
	for _, want := range []string{
		"TEMPORARY",
		"cleat#3759", // the issue that removes it: a temporary mechanism with no pointer is permanent
		"cleat.upgradeRefusal",
		"fail $msg",
		"allowLiveWorkers",
		`lookup "apps/v1" "Deployment"`,
	} {
		if !strings.Contains(caller, want) {
			t.Errorf("templates/upgrade-guard.yaml does not contain %q", want)
		}
	}
}
