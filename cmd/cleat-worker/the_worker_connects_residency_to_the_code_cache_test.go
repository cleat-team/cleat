package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// A held instance pins the compiled code it runs even after the module cache evicts it, so the worker connects its resident set to the backend's code cache
// (engine.ResidentSet.AttachToCodeCache), which sends back every held run of evicted code. The engine's end-to-end row exercises that method; THIS guards that the worker
// calls it at all, because a missing call leaves every other test green and holds pinning code nothing bounds. A source scan is what there is: main() builds both sides and
// is not unit-testable.
func TestTheWorkerConnectsItsResidentSetToTheCodeCacheOfItsBackend(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	text := string(src)
	set := strings.Index(text, "engine.NewResidentSet(")
	if set < 0 {
		t.Fatal("PRECONDITION FAILED: main.go no longer builds a resident set with engine.NewResidentSet, so this guard is looking at the wrong place")
	}
	call := regexp.MustCompile(`\bresident\.AttachToCodeCache\(\s*wt\s*\)`).FindStringIndex(text)
	if call == nil {
		t.Fatal("main.go never connects the resident set to the backend's code cache (resident.AttachToCodeCache(wt)): a held run would keep alive compiled code the cache had decided to evict, which no bound accounts for")
	}
	if call[0] < set {
		t.Errorf("the code-cache connection (offset %d) comes before the resident set is built (offset %d)", call[0], set)
	}
}
