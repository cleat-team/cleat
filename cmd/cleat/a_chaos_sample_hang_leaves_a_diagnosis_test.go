package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// cleat#4432. The required `Chaos sample` job hung once: its `Seeded sample` step sat for 35 minutes, was cancelled at the job's 30-minute limit plus the 5-minute cancellation
// grace, and left NO log at all, so nothing could say why. `go test -timeout 20m` is meant to fire before that and dump goroutines, and it had fired before on this job (two failures
// at 20m02s on 2026-11-22), so whatever hung this time was somewhere it could not reach, or the runner stopped responding.
//
// Whichever it was, a hang must leave something to read. That needs three properties, and each is silent when lost: a hung step is stopped by the RUNNER while the runner is alive (a
// step timeout BELOW the job's: a job-level cancel never reaches an `if: always()` step), a step that says what the machine looked like when the sample did not succeed, and an
// artifact that carries the resource samples as well as the test log. This pins the shape of all three and its ordering (go test < step < job), which no one reads in a diff.
//
// IT DOES NOT RUN THE WORKFLOW and cannot say a hang will be diagnosed; it says the pieces that make one diagnosable are still there. The scripts were run for real in a container (the
// sampler wrote its samples, the step exited promptly, the diagnose step handled a present and a missing log, a stray `sleep` was shown to need the pkill); that is a measurement, and
// this is the guard that keeps it true.

type chaosStep struct {
	Name           string         `yaml:"name"`
	Uses           string         `yaml:"uses"`
	If             string         `yaml:"if"`
	Run            string         `yaml:"run"`
	TimeoutMinutes int            `yaml:"timeout-minutes"`
	With           map[string]any `yaml:"with"`
}

type chaosJob struct {
	TimeoutMinutes int         `yaml:"timeout-minutes"`
	Steps          []chaosStep `yaml:"steps"`
}

type chaosWorkflow struct {
	Jobs map[string]chaosJob `yaml:"jobs"`
}

var goTestTimeoutMinutes = regexp.MustCompile(`go test[^\n]*-timeout (\d+)m`)

// chaosSampleProblems says what is missing from the sample job's hang diagnosis; an empty result is a job that will leave something to read.
func chaosSampleProblems(j chaosJob) []string {
	var problems []string
	sample := -1
	for i, s := range j.Steps {
		if s.Name == "Seeded sample" {
			sample = i
		}
	}
	if sample < 0 {
		return []string{"there is no `Seeded sample` step: this guard is reading the wrong job"}
	}
	s := j.Steps[sample]
	m := goTestTimeoutMinutes.FindStringSubmatch(s.Run)
	goTest := 0
	if m == nil {
		problems = append(problems, "the Seeded sample step's `go test` has no `-timeout <n>m`, so a hang inside a test has no goroutine dump to wait for")
	} else {
		for _, c := range m[1] {
			goTest = goTest*10 + int(c-'0')
		}
	}
	switch {
	case j.TimeoutMinutes == 0:
		problems = append(problems, "the job has no timeout-minutes")
	case s.TimeoutMinutes == 0:
		problems = append(problems, "the Seeded sample step has no timeout-minutes of its own: a hang is stopped only by the JOB's limit, and a job-level cancel never reaches the `if: always()` steps, so the log is never uploaded (cleat#4432)")
	case s.TimeoutMinutes >= j.TimeoutMinutes:
		problems = append(problems, "the Seeded sample step's timeout is not below the job's, so the job's cancel arrives first and the diagnosis steps never run")
	case goTest != 0 && s.TimeoutMinutes <= goTest:
		problems = append(problems, "the Seeded sample step's timeout is not above go test's own -timeout, so the runner kills the step before go test can dump its goroutines")
	}
	if !strings.Contains(s.Run, "chaos-sample-resources.log") {
		problems = append(problems, "the Seeded sample step no longer writes the resource samples (chaos-sample-resources.log)")
	}
	if !strings.Contains(s.Run, "pkill -P") {
		problems = append(problems, "the sampler is not stopped with its child `sleep` (measured: without `pkill -P` one stray sleep outlives the step)")
	}
	diagnose, upload := -1, -1
	for i, st := range j.Steps {
		if i <= sample {
			continue
		}
		if strings.Contains(st.If, "failure()") && strings.Contains(st.If, "cancelled()") && diagnose < 0 {
			diagnose = i
		}
		if strings.HasPrefix(st.Uses, "actions/upload-artifact") {
			upload = i
		}
	}
	if diagnose < 0 {
		problems = append(problems, "no step after the sample runs on `failure() || cancelled()` to say what the machine looked like")
	} else {
		d := j.Steps[diagnose]
		if d.TimeoutMinutes == 0 {
			problems = append(problems, "the diagnose step has no timeout of its own: a diagnosis that hangs would hold the job as the sample did")
		}
		// scripts/chaos-flake-log.py reads go test -json events out of the failed job log; a copy of the test log in this step would be counted a second time.
		if strings.Contains(d.Run, "chaos-sample.log") {
			problems = append(problems, "the diagnose step reads chaos-sample.log: that file is go test -json, and a copy of its events in the job log would be counted twice by scripts/chaos-flake-log.py")
		}
		if !strings.Contains(d.Run, "chaos-sample-resources.log") {
			problems = append(problems, "the diagnose step does not print the resource samples")
		}
	}
	switch {
	case upload < 0:
		problems = append(problems, "there is no artifact upload after the sample")
	case j.Steps[upload].If != "always()":
		problems = append(problems, "the artifact upload is not `if: always()`, so a failed or cancelled sample uploads nothing")
	default:
		path, _ := j.Steps[upload].With["path"].(string)
		for _, want := range []string{"chaos-sample.log", "chaos-sample-resources.log"} {
			if !strings.Contains(path, want) {
				problems = append(problems, "the artifact upload does not carry "+want)
			}
		}
	}
	return problems
}

func loadChaosJob(t *testing.T, mutate func(*chaosJob)) chaosJob {
	t.Helper()
	root := repoRoot(t)
	data, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "chaos.yml"))
	if err != nil {
		t.Fatalf("UNMEASURED: cannot read chaos.yml: %v. This is a failure of the check, not a finding about the workflow.", err)
	}
	var wf chaosWorkflow
	if err := yaml.Unmarshal(data, &wf); err != nil {
		t.Fatalf("UNMEASURED: chaos.yml does not parse: %v", err)
	}
	j, ok := wf.Jobs["chaos-sample"]
	if !ok || len(j.Steps) < 4 {
		t.Fatalf("UNMEASURED: no `chaos-sample` job with its steps in chaos.yml (jobs: %d); the check read nothing", len(wf.Jobs))
	}
	if mutate != nil {
		mutate(&j)
	}
	return j
}

func stepNamed(j *chaosJob, name string) *chaosStep {
	for i := range j.Steps {
		if j.Steps[i].Name == name {
			return &j.Steps[i]
		}
	}
	return nil
}

func TestAHungChaosSampleLeavesSomethingToRead(t *testing.T) {
	if p := chaosSampleProblems(loadChaosJob(t, nil)); len(p) != 0 {
		t.Fatalf("the Chaos sample job would not leave a diagnosis of a hang (cleat#4432):\n  - %s", strings.Join(p, "\n  - "))
	}
}

// The guard itself, with a KNOWN POSITIVE per property: each way of losing the diagnosis, applied to the real job, must be named. A guard that passes the real job and every broken one
// is a green that measured nothing.
func TestTheChaosSampleDiagnosisGuardNamesEachWayOfLosingIt(t *testing.T) {
	seeded := func(j *chaosJob) *chaosStep { return stepNamed(j, "Seeded sample") }
	diagnose := func(j *chaosJob) *chaosStep { return stepNamed(j, "Diagnose a sample that did not succeed") }
	upload := func(j *chaosJob) *chaosStep {
		for i := range j.Steps {
			if strings.HasPrefix(j.Steps[i].Uses, "actions/upload-artifact") {
				return &j.Steps[i]
			}
		}
		return nil
	}
	cases := []struct {
		name   string
		mutate func(*chaosJob)
		want   string
	}{
		{"the step timeout removed", func(j *chaosJob) { seeded(j).TimeoutMinutes = 0 }, "no timeout-minutes of its own"},
		{"the step timeout equal to the job's", func(j *chaosJob) { seeded(j).TimeoutMinutes = j.TimeoutMinutes }, "not below the job's"},
		{"the step timeout above the job's", func(j *chaosJob) { seeded(j).TimeoutMinutes = j.TimeoutMinutes + 5 }, "not below the job's"},
		{"the step timeout equal to go test's", func(j *chaosJob) { seeded(j).TimeoutMinutes = 20 }, "not above go test's own"},
		{"go test without a -timeout", func(j *chaosJob) {
			seeded(j).Run = regexp.MustCompile(` -timeout \d+m`).ReplaceAllString(seeded(j).Run, "")
		}, "no `-timeout"},
		{"the sampler removed", func(j *chaosJob) {
			seeded(j).Run = strings.ReplaceAll(seeded(j).Run, "chaos-sample-resources.log", "x.log")
		}, "no longer writes the resource samples"},
		{"the sampler's child not stopped", func(j *chaosJob) { seeded(j).Run = strings.ReplaceAll(seeded(j).Run, "pkill -P", "echo") }, "child `sleep`"},
		{"the diagnose step removed", func(j *chaosJob) { diagnose(j).If = "" }, "no step after the sample runs on"},
		{"the diagnose step ignoring a cancel", func(j *chaosJob) { diagnose(j).If = "${{ failure() }}" }, "no step after the sample runs on"},
		{"the diagnose step without a timeout", func(j *chaosJob) { diagnose(j).TimeoutMinutes = 0 }, "diagnose step has no timeout"},
		{"the diagnose step printing the test log", func(j *chaosJob) { diagnose(j).Run += "\ntail -n 50 chaos-sample.log\n" }, "counted twice"},
		{"the diagnose step not printing the samples", func(j *chaosJob) {
			diagnose(j).Run = strings.ReplaceAll(diagnose(j).Run, "chaos-sample-resources.log", "x.log")
		}, "does not print the resource samples"},
		{"the upload no longer always()", func(j *chaosJob) { upload(j).If = "success()" }, "not `if: always()`"},
		{"the upload without the resource log", func(j *chaosJob) { upload(j).With["path"] = "chaos-sample.log" }, "does not carry chaos-sample-resources.log"},
		{"the upload without the test log", func(j *chaosJob) { upload(j).With["path"] = "chaos-sample-resources.log" }, "does not carry chaos-sample.log"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			problems := chaosSampleProblems(loadChaosJob(t, c.mutate))
			if len(problems) == 0 {
				t.Fatalf("the guard passed a job that has lost its diagnosis (%s)", c.name)
			}
			if !strings.Contains(strings.Join(problems, "\n"), c.want) {
				t.Errorf("the guard failed for another reason than the one broken (%s): want a problem containing %q, got:\n  - %s", c.name, c.want, strings.Join(problems, "\n  - "))
			}
		})
	}
}
