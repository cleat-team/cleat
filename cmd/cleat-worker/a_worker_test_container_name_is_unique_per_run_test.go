package main

import (
	"strings"
	"testing"
)

// cleat#4416. The migrations-outside-the-repo test names a Docker container for its database. It used a name derived from the test's name alone and `docker rm -f`'d that name before
// starting, so a second concurrent run on the same machine deleted the first run's database (measured: the first run failed with no "HTTP API listening" line, 3 s apart; simultaneous
// runs failed on `docker run`, exit 125). This pins the property without Docker: two names for the same test are different, and each is a name Docker accepts.
func TestAWorkerTestContainerNameIsUniquePerRunAndAcceptedByDocker(t *testing.T) {
	old := "cleat-test-1968-pg-" + t.Name() // what the name used to be; the known positive for "the same on every run"
	if old != "cleat-test-1968-pg-"+t.Name() {
		t.Fatalf("CONTROL: the old derivation is not deterministic, so this test measures nothing")
	}

	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		n := workerTestContainerName(t)
		if seen[n] {
			t.Fatalf("workerTestContainerName gave %q twice in %d calls: two runs would share a container", n, i+1)
		}
		seen[n] = true
		if n == old {
			t.Fatalf("the name %q is the old fixed one", n)
		}
		// Docker's rule: [a-zA-Z0-9][a-zA-Z0-9_.-]*
		for j, r := range n {
			ok := r == '_' || r == '.' || r == '-' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
			if !ok || (j == 0 && (r == '_' || r == '.' || r == '-')) {
				t.Fatalf("%q has %q at %d, which Docker refuses in a container name", n, r, j)
			}
		}
	}

	t.Run("a subtest name carries a slash, which Docker refuses", func(t *testing.T) {
		n := workerTestContainerName(t)
		if strings.Contains(n, "/") {
			t.Errorf("%q contains a slash", n)
		}
	})
}
