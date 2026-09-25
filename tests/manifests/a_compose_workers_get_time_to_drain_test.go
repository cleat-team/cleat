package manifests

// Every compose service that serves as a worker must be given time to drain (cleat#2285).
//
// A worker handles SIGTERM by draining for --shutdown-grace (20s by default) before it cancels its
// runs. Docker's default is to SIGKILL after 10s, so a service with no `stop_grace_period` cuts the
// drain off half way and every run still in flight is recovered by the reaper as after a crash,
// which is exactly what the drain exists to avoid. Nothing fails when it is missing: `docker compose
// stop` just takes less time than the worker asked for.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

type composeFile struct {
	Services map[string]struct {
		Image           string   `yaml:"image"`
		Command         []string `yaml:"command"`
		StopGracePeriod string   `yaml:"stop_grace_period"`
	} `yaml:"services"`
}

func TestComposeWorkersAreGivenTimeToDrain(t *testing.T) {
	files := []string{
		"docker-compose.cluster.yml",
		"cmd/cleat/templates/agent/docker-compose.yml",
		"cmd/cleat/templates/workflow/docker-compose.yml",
		"cmd/cleat/templates/fullstack/docker-compose.yml",
	}
	const floor = 30 * time.Second // above the 20s default --shutdown-grace, with room to finish

	workers := 0
	for _, f := range files {
		data, err := os.ReadFile(filepath.Join(repoRoot, f))
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		var c composeFile
		if err := yaml.Unmarshal(data, &c); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		for name, svc := range c.Services {
			if !strings.Contains(svc.Image, "cleat-worker") {
				continue
			}
			if strings.Contains(strings.Join(svc.Command, " "), "--migrate-only") {
				continue // a one-shot deploy step, not a worker
			}
			// The dashboard container runs the worker image with `--` flags of its own but does not execute runs.
			if strings.Contains(name, "dashboard") {
				continue
			}
			workers++
			d, err := time.ParseDuration(svc.StopGracePeriod)
			if err != nil || d < floor {
				t.Errorf("%s: service %q runs a worker with stop_grace_period %q: Docker will SIGKILL it before a 20s drain can finish (want at least %s)",
					f, name, svc.StopGracePeriod, floor)
			}
		}
	}
	// Non-vacuity: 3 cluster workers and one in each of the three scaffolds.
	if workers < 6 {
		t.Errorf("found %d worker services across %d compose files, want at least 6: the scan has stopped seeing them", workers, len(files))
	}
}
