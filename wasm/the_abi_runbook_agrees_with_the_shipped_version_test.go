package wasm

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
)

// docs/operations/abi-migration.md must agree with CurrentABIVersion.
// cleat#1322 item 1.
//
// WHAT THIS IS FOR. That file used to be a 377-line runbook for migrating
// workflows "from ABI v4 to ABI v5", with a five-version history table and a
// compatibility matrix over engine releases 0.1.x through 0.5.x. One ABI
// version has ever shipped. The runbook also stated that v5 changed the event
// history checksum to BLAKE3 -- a false claim about PRESENT behaviour, since
// computeEventChecksum uses xxhash -- and its steps invoked a `cleat`
// subcommand that does not exist.
//
// WHY IT SURVIVED, which is the part this test is shaped around. A document
// describing a procedure nobody can run produces NO SIGNAL when it is wrong.
// Nothing fails; no test goes red; and a reader who tries to verify it
// concludes they are looking in the wrong place rather than that the document
// is fiction. It was found by a review, not by use, and it had been wrong for
// its whole life.
//
// So the replacement states the current version as a sentence, and this test
// makes that sentence checkable. Prose cannot go red on its own; a number in
// prose compared against the constant can.
//
// WHAT THIS DELIBERATELY DOES NOT CHECK. The document's retrospective section
// names ABI v4 and v5 while describing what the old runbook claimed. Asserting
// "this file mentions no version above CurrentABIVersion" would forbid the
// history from being recorded at all, and the history is the most useful part:
// it is the evidence for why the file is short now. The check is anchored on
// the one sentence that makes a CLAIM about the present.
func TestTheABIRunbookAgreesWithTheShippedVersion(t *testing.T) {
	path := filepath.Join("..", "docs", "operations", "abi-migration.md")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v\n\n"+
			"If the file moved, point this test at it rather than deleting the "+
			"test: the claim it guards moved with it.", path, err)
	}

	re := regexp.MustCompile(`(?m)^\s*The current ABI version is (\d+)\.`)
	m := re.FindSubmatch(raw)
	if m == nil {
		t.Fatalf("%s no longer contains a line of the form "+
			"\"The current ABI version is N.\"\n\n"+
			"That sentence is the thing this test checks. If it was reworded, "+
			"reword the pattern too -- do not drop the assertion, because an "+
			"unasserted statement about the ABI version is exactly what "+
			"cleat#1322 found in this file.", path)
	}

	stated, err := strconv.Atoi(string(m[1]))
	if err != nil {
		t.Fatalf("unparseable version %q in %s: %v", m[1], path, err)
	}
	if stated != CurrentABIVersion {
		t.Errorf("%s says the current ABI version is %d; wasm.CurrentABIVersion is %d.\n\n"+
			"The constant is the authority. If the ABI version was just bumped, this "+
			"document needs more than a number changed -- it currently says no "+
			"migration procedure exists, and at version 2 that stops being true.",
			path, stated, CurrentABIVersion)
	}
}
