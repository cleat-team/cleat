package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// cleat#3072. Nothing compiled the Go snippets published in docs/tutorials/,
// and one of them could not compile: your-first-workflow.md's Saga example
// declared its closure-local results with `:=` inside each closure, so the
// sibling compensation closure could not see them -- a reader who copied it got
// `declared and not used: reservation` / `undefined: reservation`. It sat there
// until an audit ran the walkthrough.
//
// WHY THIS IS A GO TEST IN THIS PACKAGE rather than a script wired into a
// workflow: `./cmd/...` is one of the package groups in ci.yml's test-go
// matrix, so a test here is wired the moment it lands. A guard that needs a new
// CI step is a mechanism someone must remember to connect -- and this repo
// already has a defect class for mechanisms that exist and are wired to
// nothing.
//
// WHY `go vet` AND NOT `go build` -- AND WHY THIS IS NOT THE WEAKENING ITS
// NEIGHBOUR REFUSES. a_scaffold_builds_test.go's header records that its
// predecessor "ran `go vet` rather than `cleat build`" and calls that misstep
// "easy to make again"; the same fact (a `package main` with no `main`) is
// present here, so the resemblance is real and this choice has to be argued
// rather than assumed.
//
// The distinction that saves it is the SUBJECT, not the command's strength: the
// rejected test's subject was a real generated project, on which `cleat build`
// has something to operate. Here the subject is a synthetic assembly that no
// `cleat build` invocation could be pointed at -- the document's own file is not
// what gets compiled. `vet` is therefore the strongest check available for the
// artifact under test, which is what that rule actually asks for. (It also runs
// the compiler's type-checking, so it reports everything a copying reader would
// hit, `declared and not used` included -- measured, not assumed.)
//
// WHAT IT CANNOT SEE, stated rather than implied: it stops short of the link and
// emit stages, so a collision only those reject would pass here. The mitigation
// for that is a separate gap and not this test's business -- quick-start.md's
// documented path IS run for real by TestTutorialQuickStartReachesADoneWorkflow,
// while NOTHING in this repository runs your-first-workflow.md's `cleat build`:
// that file is referenced by no Go, shell or workflow file other than this
// guard. So the weaker-command objection lands hardest exactly where there is no
// other check at all, and that is worth recording rather than papering over.
//
// NOT GUARDED ON NETWORK AVAILABILITY, for the reason a_scaffold_builds_test.go
// gives: `go mod tidy` is a real dependency of the thing under test, and
// skipping when the network is absent would report clean in the one situation
// where this test has measured nothing.
//
// scripts/build-documented-examples.sh covers the NEIGHBOURING case -- it runs
// the `cleat build` each examples/*/README.md documents, deriving its set from
// `git ls-files` -- so it guards example README *commands*, not the fenced code
// blocks in docs/tutorials/. Its own header states the principle this file
// extends: "A green CI run was evidence that the unit tests pass. It was not
// evidence that the product does what the documentation says."

// tutorialFragment wraps a fenced ```go block that is a FRAGMENT rather than a
// self-contained declaration.
//
// THIS IS THE PART THAT CANNOT BE AUTOMATED AWAY, so it is stated rather than
// inferred. A tutorial presents the Saga step as a block to place inside an
// entry point the document declares elsewhere, so the block's enclosing
// signature is in the prose, not in the block. The stub supplies that context.
//
// ★ THE RULE, AND THIS FILE GOT IT WRONG FIRST: a stub's signature must be the
// signature of a function the DOCUMENT declares. Nothing in a stub may be
// invented, because an invented parameter hides a defect of precisely the class
// this guard exists for -- and it hides it in the most convincing way available,
// by making the document compile.
//
// The counter-example is real and was caught in review. Both stubs used to
// declare `totalCents int`, and the document declares `totalCents` NOWHERE: its
// PlaceOrder computes the total as `reservation.TotalCents`, and processPayment's
// parameter is `amountCents`. So the stubs were not reproducing the document's
// context, they were supplying an identifier it did not have -- and a reader
// pasting the block got `undefined: totalCents`. Worse, the same stub footer
// returned the typed block's `err`, hiding a second defect of the same class:
// as published, that block declared an error and never used it.
//
// Each stub is now the signature of the document's own function -- PlaceOrder
// for the Saga block, processPayment (whose body the typed block replaces) for
// the other -- and both snippets were repaired to use what they declare.
//
// Keyed by a MARKER rather than by index, so inserting a block above does not
// silently re-point a stub at the wrong text.
type tutorialFragment struct {
	marker string // a substring that identifies the block
	header string // the function header the document implies for it
	footer string // what closes it, including any use its locals need
	// support is a SIBLING Go file in the same package, written beside the
	// snippet, for a block that is a complete example -- its own package clause,
	// so nothing may be prepended -- yet still calls code the page assumes the
	// reader has written. A header cannot serve that: the block's package clause
	// is already there, and an import the block's own body does not use would make
	// the page's example fail for a READER, whose workflow lives in another file.
	// The sibling is the shape Go itself uses for the same relationship, which is
	// why it is a file rather than a trick: measured on cleat#3112,
	// test-workflows.md's "Complete example" calls ApprovalWorkflow, which the page
	// declares nowhere, and the stub that stands in for it needs `cleat` in scope.
	support string
}

// tutorialSnippet is one file to compile: the snippet, plus an optional sibling
// support file in the same package.
type tutorialSnippet struct {
	src     string
	support string
	// label names the source block in a failure message. A perBlock document has
	// one file per block, and `vet: ./snippet.go:32:12` identifies nothing on a
	// page with twenty-four of them -- the line number is a property of the
	// ASSEMBLED file, not of the document. cleat#3112 grew this table fivefold.
	label string
}

type tutorialDoc struct {
	path      string
	fragments []tutorialFragment
	// perBlock compiles each fenced block as its OWN file instead of joining
	// them into one. A tutorial is one program whose blocks are parts of it; a
	// how-to page is a collection of independent illustrations, so joining them
	// cannot compile -- measured on cleat#3097, 48 of the 50 blocks under
	// docs/how-to/ carry no package clause, and common-patterns.md has two blocks
	// that both open `func CreateOrder(...)`.
	//
	// Every block then needs a fragment or its own package clause, because a bare
	// fragment has nothing to compile as a file. That is enforced rather than
	// skipped -- see snippetFiles. And the fragments carry package clauses and
	// IMPORT SETS, not shared preludes: each block is its own file, so an import
	// a block does not use fails it ("imported and not used").
	perBlock bool
	// knownBroken replaces one block so the guard can be shown to FAIL. A guard
	// that only ever passes is indistinguishable from a guard that stopped
	// checking, which is the same defect this whole file is about.
	knownBroken *tutorialFragment
	brokenText  string
	brokenWant  string // the text the failure must contain
}

// SCOPE, STATED RATHER THAN LEFT AS AN OMISSION. Every file under
// docs/tutorials/ is link-checked (below), and every one is now SNIPPET-compiled
// too. The third, signals-and-human-loop.md, was excluded until cleat#3079
// repaired it: all three of its AwaitSignals calls disagreed with the SDK, which
// declares
//
//	AwaitSignals(signalNames []string, timeout time.Duration) SignalResult
//
// -- one return value, no option functions. The document showed one call
// assigning to a single `err` with `cleat.WithTimeout`/`cleat.WithSignalPayload`
// (neither exists), and another taking two values from three string arguments.
// Repairing that was a change to what a tutorial TEACHES; the exclusion naming
// it was deleted with the repair, which is the expiry excludedDocs asserts.
//
// cleat#3097 WIDENED IT TO THE HOW-TO PAGES, in the mode they need. docs/how-to/
// is a different SHAPE from a tutorial -- measured: 48 of its 50 Go blocks carry
// no package clause, and common-patterns.md has two blocks that both open
// `func CreateOrder(...)` -- so those pages are compiled one file per block
// (perBlock) rather than joined, each block carrying its own package clause and
// import set.
//
// FOUR PAGES ARE COVERED HERE: use-plugins.md and use-secrets.md (9 blocks)
// since cleat#3097, and test-workflows.md (17) and common-patterns.md (24) since
// cleat#3112. The LINK half below still covers docs/tutorials/ only.
//
// The two added in cleat#3112 needed scaffolding the first two did not, and both
// are why a block is not simply "fragments or nothing":
//
//   - test-workflows.md's blocks call the READER's own workflow (ApprovalWorkflow,
//     MyWorkflow), and its "Complete example" carries its own package clause -- a
//     fragment header cannot wrap that, because an import the block's own body
//     does not use would fail the page's example for a reader, whose workflow
//     lives in another file. Those blocks take a SIBLING support file; see
//     tutorialFragment.support.
//   - common-patterns.md's 24 blocks share page-local types and helpers
//     (PipelineInput, ChildInput, extractID, processOrderSmall), so ONE sibling
//     file per page carries them and each fragment supplies only its own package,
//     import set and wrapper; see commonPatternsSupport.
//
// Both pages carry their own known-positive, so their tables cannot stop matching
// unnoticed.

// Import sets shared by common-patterns.md's fragment headers. Each block is its
// own file, so a block may import only what its own body uses -- an import a
// block does not use fails it, which is why these are separate constants rather
// than one prelude.
const (
	cpCleat           = "package main\n\nimport \"github.com/cleat-team/cleat/cleat\""
	cpCleatFmt        = "package main\n\nimport (\n\t\"fmt\"\n\n\t\"github.com/cleat-team/cleat/cleat\"\n)"
	cpCleatTime       = "package main\n\nimport (\n\t\"time\"\n\n\t\"github.com/cleat-team/cleat/cleat\"\n)"
	cpCleatFmtTime    = "package main\n\nimport (\n\t\"fmt\"\n\t\"time\"\n\n\t\"github.com/cleat-team/cleat/cleat\"\n)"
	cpCleatFmtStrings = "package main\n\nimport (\n\t\"fmt\"\n\t\"strings\"\n\n\t\"github.com/cleat-team/cleat/cleat\"\n)"
	cpCleatJSON       = "package main\n\nimport (\n\t\"encoding/json\"\n\n\t\"github.com/cleat-team/cleat/cleat\"\n)"
)

// commonPatternsSupport is the sibling support file shared by every
// common-patterns.md block: the page-local types it names (PipelineInput,
// ChildInput, OrderItem), the page-local helpers it calls (extractID,
// processOrderSmall, checkPickupStatus) and the values the surrounding prose
// assumes (inputJSON, driverID, items). Every one is derived from the page's own
// usage -- `ChildInput{Item: item, JobID: jobID, Index: i}` fixes the field names
// and types, `processOrderSmall(h, userID, items)` fixes the signature -- rather
// than invented. One file for the whole page, because these are shared, and
// unused package-level declarations are legal where unused imports are not.
//
// It deliberately declares neither `h` nor `processOrder`: block 20 declares
// both itself, and a duplicate would fail that block alone.
const commonPatternsSupport = `package main

import "github.com/cleat-team/cleat/cleat"

type (
	PipelineInput  struct{ Items []string }
	PipelineResult struct{ Succeeded, Failed int }
	ChildInput     struct {
		Item  string
		JobID string
		Index int
	}
	ChildResult       struct{}
	SubscriptionInput struct{}
	OrderItem         struct{}
)

var (
	inputJSON   string
	requestJSON string
	flightJSON  string
	hotelJSON   string
	flightRef   string
	hotelRef    string
	driverID    string
	runID       string
	items       []string
	item        string
	jobID       string
	i           int
	driver      struct {
		DriverName string
		ETAMinutes string
	}
)

func extractID(s string) string { return "" }

func processOrderSmall(h cleat.HostCalls, userID string, items []OrderItem) (string, error) {
	return "", nil
}

func checkPickupStatus(driverID string) (string, error) { return "", nil }

func isComplete(s string) bool { return false }

func chargeWithRetry(h cleat.HostCalls, input SubscriptionInput) error { return nil }

func enterGracePeriod(h cleat.HostCalls, input SubscriptionInput) (string, error) {
	return "", nil
}

func toJSON(v interface{}) string { return "" }

func stepJSON(i int) string { return "" }

func placeOrderV2(input string) error { return nil }

func placeOrderV1(input string) error { return nil }
`

var tutorialDocs = []tutorialDoc{
	{
		path: "docs/tutorials/quick-start.md",
		// Its one Go block is the whole scaffold sample: package clause,
		// imports and the entry point together.
	},
	{
		path: "docs/tutorials/your-first-workflow.md",
		fragments: []tutorialFragment{
			{
				marker: "s := cleat.NewSaga()",
				// PlaceOrder's own signature (your-first-workflow.md).
				header: "func _sagaFragment(h cleat.HostCalls, userID string, cart []CartItem) (string, error) {",
				footer: "return \"\", nil\n}",
			},
			{
				marker: "h.DurableCallTyped(",
				// processPayment's own signature -- the block is presented as a
				// replacement for that function's body.
				header: "func _typedCallFragment(h cleat.HostCalls, userID string, amountCents int) (Charge, error) {",
				footer: "return Charge{}, nil\n}",
			},
		},
		knownBroken: &tutorialFragment{marker: "s := cleat.NewSaga()"},
		brokenText: "// The ORIGINAL documented form, kept as the guard's known-positive.\n" +
			"// `reservation` is declared INSIDE the forward closure, so the sibling\n" +
			"// cannot see it -- exactly the two errors the audit quoted.\n" +
			"s := cleat.NewSaga()\n" +
			"s.AddStep(\"reserve_inventory\",\n" +
			"    func(h cleat.HostCalls) (string, error) {\n" +
			"        reservation, err := reserveInventory(h, userID, cart)\n" +
			"        return \"\", err\n    },\n" +
			"    func(h cleat.HostCalls) error {\n" +
			"        return releaseReservation(h, reservation.ReservationID)\n    },\n)\n" +
			"if err := s.Run(h); err != nil {\n    return \"\", err\n}\n",
		brokenWant: "declared and not used: reservation",
	},
	{
		path: "docs/tutorials/signals-and-human-loop.md",
		// Its first block is the whole example -- package clause, imports,
		// types and submitExpense -- so only the two the prose draws out of it
		// need stubs. Each marker is deliberately NOT text that occurs in that
		// complete block: assemble wraps the FIRST fragment whose marker it
		// finds, so a marker shared with block 0 would wrap block 0 instead,
		// and the failure would look like a broken document rather than a
		// broken table entry.
		fragments: []tutorialFragment{
			{
				marker: "escalating to on-call",
				header: "func _timeoutFragment(h cleat.HostCalls, reportID string) (string, error) {",
				footer: "return \"\", nil\n}",
			},
			{
				marker: "switch res.Name",
				header: "func _multiSignalFragment(h cleat.HostCalls) {",
				footer: "}",
			},
		},
	},
	{
		// cleat#3097. A how-to page is a different SHAPE from a tutorial: each
		// block is an independent illustration rather than a part of one program,
		// so it is compiled one file per block (perBlock).
		path:     "docs/how-to/use-plugins.md",
		perBlock: true,
		// Each fragment is that block's package clause AND IMPORT SET. They are per
		// block rather than a shared prelude because a block is its own file, so an
		// import it does not use fails it -- and these blocks use different ones.
		fragments: []tutorialFragment{
			{
				marker: "func SummarizeOrder(",
				header: "package main\n\nimport (\n\t\"encoding/json\"\n\t\"fmt\"\n\n\t\"github.com/cleat-team/cleat/cleat\"\n)",
			},
			{
				marker: "func NotifyOnSlack(",
				header: "package main\n\nimport (\n\t\"strings\"\n\n\t\"github.com/cleat-team/cleat/cleat\"\n)",
			},
			{
				// A bare statement pair: it needs the function around it as well as
				// the package, and a use for the two values the prose never reads.
				marker: "policy := cleat.RetryPolicy{",
				header: "package main\n\nimport (\n\t\"time\"\n\n\t\"github.com/cleat-team/cleat/cleat\"\n)\n\nfunc _retryFragment(h cleat.HostCalls, requestJSON string) error {",
				footer: "_, _ = resp, err\n\treturn nil\n}",
			},
			{
				marker: "func GenerateResponse(",
				header: "package main\n\nimport (\n\t\"encoding/json\"\n\n\t\"github.com/cleat-team/cleat/cleat\"\n)",
			},
			{
				marker: "func SendAlert(",
				header: "package main\n\nimport (\n\t\"encoding/json\"\n\n\t\"github.com/cleat-team/cleat/cleat\"\n)",
			},
			{
				marker: "func EscalateToOnCall(",
				header: "package main\n\nimport (\n\t\"encoding/json\"\n\n\t\"github.com/cleat-team/cleat/cleat\"\n)",
			},
			{
				marker: "func AwaitWebhookEvent(",
				header: "package main\n\nimport (\n\t\"encoding/json\"\n\t\"fmt\"\n\n\t\"github.com/cleat-team/cleat/cleat\"\n)",
			},
		},
		// THE MODE'S OWN KNOWN-POSITIVE. A per-block walk that had stopped yielding
		// snippets would report every block clean, so one block is replaced with
		// text that MUST fail, and for the recorded reason. The replacement uses
		// both of its fragment's imports, so the only error it can produce is the
		// one this guard exists to catch.
		knownBroken: &tutorialFragment{marker: "func SummarizeOrder("},
		brokenText: "func SummarizeOrder(h cleat.HostCalls, input string) error {\n" +
			"\tunusedLocal := 1\n" +
			"\t_ = json.RawMessage{}\n" +
			"\t_ = fmt.Sprint(\"\")\n" +
			"\treturn nil\n}\n",
		brokenWant: "declared and not used: unusedLocal",
	},
	{
		path:     "docs/how-to/use-secrets.md",
		perBlock: true,
		fragments: []tutorialFragment{
			{
				// Both blocks are a bare `h.DurableCall(...)` whose results the prose
				// never reads, so each needs a package, the SDK import and a function
				// to stand in.
				marker: "h.DurableCall(\"llm\", \"chat\"",
				header: "package main\n\nimport \"github.com/cleat-team/cleat/cleat\"\n\nfunc _secretFragment(h cleat.HostCalls) {",
				footer: "}",
			},
			{
				marker: "h.DurableCall(\"http\", \"fetch\"",
				header: "package main\n\nimport \"github.com/cleat-team/cleat/cleat\"\n\nfunc _secretFragment(h cleat.HostCalls) {",
				footer: "}",
			},
		},
	},
	{
		// cleat#3112. A collection of test fragments: every block but the two
		// complete examples needs a package, an import set, a function to live in
		// and stubs for the workflow entry points the page names but does not
		// define.
		//
		// THE STUBS COME FROM THE PAGE, NOT FROM ME. `MyWorkflow` and
		// `ApprovalWorkflow` are declared nowhere on the page, and its own usage
		// fixes their shape -- `err := ApprovalWorkflow(h, `{"amount": 5000}`)` with
		// a single error return, and the prose above the first snippet states
		// `func MyWorkflow(h cleat.HostCalls, input string) error`. A stub shaped
		// `(string, error)` would not compile against the page as written, which is
		// the point: the page is the only thing that says which.
		path:     "docs/how-to/test-workflows.md",
		perBlock: true,
		fragments: []tutorialFragment{
			{
				// A bare call and an assertion: needs a package, a function, and the
				// workflow the page's prose defines.
				marker: "err := MyWorkflow(env.H(), `{\"key\": \"value\"}`)",
				header: "package myworkflow_test\n\nimport (\n\t\"testing\"\n\n\t\"github.com/cleat-team/cleat/cleat\"\n\t\"github.com/cleat-team/cleat/cleat/cleattest\"\n)\n\nfunc MyWorkflow(h cleat.HostCalls, input string) error { return nil }\n\nfunc _snippet(t *testing.T) {\n\tenv := cleattest.NewTestEnv()",
				footer: "}",
			},
			{
				marker: "env.OnCall(\"payments\", \"Charge\", nil).Return(`{\"status\":\"ok\"",
				header: "package myworkflow_test\n\nimport (\n\t\"testing\"\n\n\t\"github.com/cleat-team/cleat/cleat/cleattest\"\n)\n\nfunc _snippet(t *testing.T) {\n\tenv := cleattest.NewTestEnv()",
				footer: "}",
			},
			{
				marker: "// Match by predicate.",
				header: "package myworkflow_test\n\nimport (\n\t\"strings\"\n\t\"testing\"\n\n\t\"github.com/cleat-team/cleat/cleat/cleattest\"\n)\n\nfunc _snippet(t *testing.T) {\n\tenv := cleattest.NewTestEnv()",
				footer: "}",
			},
			{
				marker: "env.OnCall(\"payments\", \"Charge\", nil).ReturnJSON(",
				header: "package myworkflow_test\n\nimport (\n\t\"testing\"\n\n\t\"github.com/cleat-team/cleat/cleat/cleattest\"\n)\n\nfunc _snippet(t *testing.T) {\n\tenv := cleattest.NewTestEnv()",
				footer: "}",
			},
			{
				marker: "env.OnPluginCall(\"llm\", \"chat\").Return(",
				header: "package myworkflow_test\n\nimport (\n\t\"testing\"\n\n\t\"github.com/cleat-team/cleat/cleat/cleattest\"\n)\n\nfunc _snippet(t *testing.T) {\n\tenv := cleattest.NewTestEnv()",
				footer: "}",
			},
			{
				marker: "func TestApprovalWorkflow(t *testing.T) {",
				header: "package myworkflow_test\n\nimport (\n\t\"testing\"\n\n\t\"github.com/cleat-team/cleat/cleat\"\n\t\"github.com/cleat-team/cleat/cleat/cleattest\"\n)\n\nfunc ApprovalWorkflow(h cleat.HostCalls, input string) error { return nil }",
			},
			{
				marker: "func TestApprovalTimeout(t *testing.T) {",
				header: "package myworkflow_test\n\nimport (\n\t\"testing\"\n\t\"time\"\n\n\t\"github.com/cleat-team/cleat/cleat\"\n\t\"github.com/cleat-team/cleat/cleat/cleattest\"\n)\n\nfunc ApprovalWorkflow(h cleat.HostCalls, input string) error { return nil }",
			},
			{
				marker: "func TestPollingWorkflow(t *testing.T) {",
				header: "package myworkflow_test\n\nimport (\n\t\"strings\"\n\t\"testing\"\n\n\t\"github.com/cleat-team/cleat/cleat\"\n\t\"github.com/cleat-team/cleat/cleat/cleattest\"\n)\n\nfunc PollForDelivery(h cleat.HostCalls, input string) error { return nil }",
			},
			{
				marker: "env.SetCancelled(\"manual override\")",
				header: "package myworkflow_test\n\nimport (\n\t\"testing\"\n\n\t\"github.com/cleat-team/cleat/cleat/cleattest\"\n)\n\nfunc _snippet(t *testing.T) {\n\tenv := cleattest.NewTestEnv()",
				footer: "}",
			},
			{
				marker: "env.AssertCalled(t, \"payments\", \"Charge\")",
				header: "package myworkflow_test\n\nimport (\n\t\"testing\"\n\n\t\"github.com/cleat-team/cleat/cleat/cleattest\"\n)\n\nfunc _snippet(t *testing.T) {\n\tenv := cleattest.NewTestEnv()",
				footer: "}",
			},
			{
				marker: "history := env.CallHistory()",
				header: "package myworkflow_test\n\nimport (\n\t\"testing\"\n\n\t\"github.com/cleat-team/cleat/cleat/cleattest\"\n)\n\nfunc _snippet(t *testing.T) {\n\tenv := cleattest.NewTestEnv()",
				footer: "}",
			},
			{
				// `err := MyWorkflow(env.H(), \`{"order_id":"ord_1"}\`)` opens this
				// block, the SetVersion block AND the replay block, so the marker is
				// the QueryState line -- the only text all three do not share.
				marker: "status, ok := env.QueryState(",
				header: "package myworkflow_test\n\nimport (\n\t\"testing\"\n\n\t\"github.com/cleat-team/cleat/cleat\"\n\t\"github.com/cleat-team/cleat/cleat/cleattest\"\n)\n\nfunc MyWorkflow(h cleat.HostCalls, input string) error { return nil }\n\nfunc _snippet(t *testing.T) {\n\tenv := cleattest.NewTestEnv()",
				footer: "}",
			},
			{
				marker: "env.SetVersion(2)",
				header: "package myworkflow_test\n\nimport (\n\t\"testing\"\n\n\t\"github.com/cleat-team/cleat/cleat\"\n\t\"github.com/cleat-team/cleat/cleat/cleattest\"\n)\n\nfunc MyWorkflow(h cleat.HostCalls, input string) error { return nil }\n\nfunc _snippet(t *testing.T) {\n\tenv := cleattest.NewTestEnv()",
				footer: "}",
			},
			{
				marker: "env.AssertContinued(t,",
				header: "package myworkflow_test\n\nimport (\n\t\"testing\"\n\n\t\"github.com/cleat-team/cleat/cleat/cleattest\"\n)\n\nfunc _snippet(t *testing.T) {\n\tenv := cleattest.NewTestEnv()",
				footer: "}",
			},
			{
				marker: "func TestReplayWorkflow(t *testing.T) {",
				header: "package myworkflow_test\n\nimport (\n\t\"testing\"\n\n\t\"github.com/cleat-team/cleat/cleat\"\n\t\"github.com/cleat-team/cleat/cleat/cleattest\"\n)\n\nfunc MyWorkflow(h cleat.HostCalls, input string) error { return nil }",
			},
			{
				// The page's "Complete example: approval workflow test". It carries
				// its OWN package clause, so nothing may be prepended -- and it calls
				// ApprovalWorkflow, which the page declares nowhere and which fixes
				// its own shape (`err := ApprovalWorkflow(h, ...)`, single error
				// return). So the workflow goes in a SIBLING file: the block keeps the
				// page's exact text and import set, and the stub is a second file in
				// the package, which is what a reader has anyway -- their workflow
				// lives beside their test. An empty header is the whole point: the
				// block supplies its own package and imports, and adding `cleat` to
				// THEM would make the page's example fail for a reader, whose own
				// workflow file is where that import belongs.
				marker:  "func TestApprovalWorkflow_Rejected(t *testing.T) {",
				support: "package myworkflow_test\n\nimport \"github.com/cleat-team/cleat/cleat\"\n\nfunc ApprovalWorkflow(h cleat.HostCalls, input string) error { return nil }",
			},
		},
		// This page's own known-positive. Without it, a fragment table that had
		// stopped matching -- or an assembly that vetted as an empty file -- would
		// still report seventeen blocks clean. The replacement keeps its fragment's
		// marker and uses each of that fragment's imports, so the only error it can
		// produce is the one this guard exists to catch.
		knownBroken: &tutorialFragment{marker: "func TestReplayWorkflow(t *testing.T) {"},
		brokenText: "func TestReplayWorkflow(t *testing.T) {\n" +
			"\tenv := cleattest.NewTestEnv()\n" +
			"\t_ = env\n" +
			"\tunusedLocal := 1\n}\n",
		brokenWant: "declared and not used: unusedLocal",
	},
	{
		// cleat#3112. common-patterns.md is a CATALOGUE, not a program: 24
		// independent blocks, none carrying a package clause, several of them
		// excerpts from the middle of a function (they `return` with no signature)
		// and several naming types and helpers the page never defines. Each block
		// gets a fragment for its own package, import set and wrapper; the types and
		// helpers are SHARED through one sibling support file, because they are
		// shared -- see commonPatternsSupport.
		//
		// The wrappers close over the same convention the tutorial fragments do: a
		// block that returns `"", fmt.Errorf(...)` mid-function is wrapped in a
		// function with those results, and a footer supplies the trailing return and
		// consumes any local the excerpt declares but does not use. That second job
		// is scaffolding rather than a page defect: `status, err := cleat.PollUntil(
		// ...)` followed by `if err != nil` is exactly what a reader writes, and the
		// `status` they would go on to use is simply outside the excerpt.
		path:     "docs/how-to/common-patterns.md",
		perBlock: true,
		fragments: []tutorialFragment{
			{marker: "var chargeID, driverID string", header: cpCleat, support: commonPatternsSupport},
			{marker: "h.DurableDeferFunc(func() {", header: cpCleat, support: commonPatternsSupport},
			{marker: "func RunPipeline(", header: cpCleatFmt, support: commonPatternsSupport},
			{
				// The block's failure path is `return nil, fmt.Errorf(...)`, so the
				// result type must be nilable: `(string, error)` would not compile
				// against the page as written. interface{} is the least the page's own
				// code admits.
				marker:  "s.AddParallel(",
				header:  cpCleatFmt + "\n\nfunc _saga(h cleat.HostCalls) (interface{}, error) {",
				footer:  "\n\treturn \"\", nil\n}",
				support: commonPatternsSupport,
			},
			{marker: "func WaitForPayment(", header: cpCleatFmtTime, support: commonPatternsSupport},
			{
				marker:  "// Wait for one of several signals.",
				header:  cpCleatTime + "\n\nfunc _signalSet(h cleat.HostCalls) {",
				footer:  "}",
				support: commonPatternsSupport,
			},
			{
				marker:  "sig := h.AwaitSignals(",
				header:  cpCleatTime + "\n\nfunc _reply(h cleat.HostCalls) {",
				footer:  "}",
				support: commonPatternsSupport,
			},
			{
				marker:  "// Start a child workflow -- does not block.",
				header:  cpCleatFmt + "\n\nfunc _child(h cleat.HostCalls) (string, error) {",
				footer:  "\n\t_ = result\n\treturn \"\", nil\n}",
				support: commonPatternsSupport,
			},
			{
				marker:  "[TERMINATED]",
				header:  cpCleatFmtStrings + "\n\nfunc _childErr(h cleat.HostCalls) (string, error) {",
				footer:  "\n\t_ = result\n\treturn \"\", nil\n}",
				support: commonPatternsSupport,
			},
			{
				marker:  "ChildInput{Item: item, JobID: jobID, Index: i}",
				header:  cpCleat + "\n\nfunc _typedChild(h cleat.HostCalls) {",
				footer:  "\n\t_, _ = runID, err\n}",
				support: commonPatternsSupport,
			},
			{
				marker:  "runIDs := make([]string, len(items))",
				header:  cpCleat + "\n\nfunc _fanIn(h cleat.HostCalls) {",
				footer:  "\n\t_, _ = results, err\n}",
				support: commonPatternsSupport,
			},
			{marker: "func ProcessItem(", header: cpCleatFmtTime, support: commonPatternsSupport},
			{marker: "func ManageSubscription(", header: cpCleatFmtTime, support: commonPatternsSupport},
			{marker: "// PlaceLargeOrder uses ContinueAsNew", header: cpCleatJSON, support: commonPatternsSupport},
			{
				marker:  "status, err := cleat.PollUntil(",
				header:  cpCleatFmtTime + "\n\nfunc _pollUntil(h cleat.HostCalls) (string, error) {",
				footer:  "\n\t_ = status\n\treturn \"\", nil\n}",
				support: commonPatternsSupport,
			},
			{marker: "func PollUntilCustom(", header: cpCleatFmtTime, support: commonPatternsSupport},
			{
				marker:  "result, err := h.DurableCallWithOptions(",
				header:  cpCleatTime + "\n\nfunc _retryOptions(h cleat.HostCalls) {",
				footer:  "\n\t_, _ = result, err\n}",
				support: commonPatternsSupport,
			},
			{
				marker:  "NonRetryableErrors:",
				header:  cpCleatTime + "\n\nfunc _nonRetryable(h cleat.HostCalls) {",
				footer:  "\n\t_ = policy\n}",
				support: commonPatternsSupport,
			},
			{
				marker:  "policy := cleat.DefaultRetryPolicy()",
				header:  cpCleat + "\n\nfunc _defaultPolicy(h cleat.HostCalls) {",
				footer:  "\n\t_ = policy\n}",
				support: commonPatternsSupport,
			},
			{
				marker:  "deadline := h.Now().Add(30 * time.Second)",
				header:  cpCleatFmtTime + "\n\nfunc _deadlineLoop(h cleat.HostCalls) (string, error) {",
				footer:  "}",
				support: commonPatternsSupport,
			},
			{marker: "// Package-level declaration -- the transformer detects this.", header: cpCleat, support: commonPatternsSupport},
			{marker: "func LongRunningProcess(", header: cpCleatFmt, support: commonPatternsSupport},
			{marker: "// Declare minimum version this code supports.", header: cpCleat, support: commonPatternsSupport},
			{
				marker:  "order_status",
				header:  cpCleat + "\n\nfunc _queryState(h cleat.HostCalls) {",
				footer:  "}",
				support: commonPatternsSupport,
			},
		},
		// This page's known-positive, and it exercises the wrapper AND the shared
		// support file: the replacement keeps its fragment's marker, and the failure
		// it must produce is a local the wrapper's footer does not consume. A table
		// that had stopped matching would leave this compiling, which the test
		// reports as "the known-positive compiled" rather than as a silent pass.
		knownBroken: &tutorialFragment{marker: "policy := cleat.DefaultRetryPolicy()"},
		brokenText: "policy := cleat.DefaultRetryPolicy()\n" +
			"unusedLocal := 1\n",
		brokenWant: "declared and not used: unusedLocal",
	},
}

// excludedDocs are tutorials that do NOT compile today, each with the error that
// must STILL be present and the issue tracking the repair.
//
// THE ENTRY RETIRES ITSELF, which is why it exists rather than a comment saying
// the same thing. When the document is repaired the expected error disappears
// and this assertion fails -- the prompt to move the document into tutorialDocs.
// An exclusion recorded only as an explanation is a SKIP WITH NO EXPIRY: nothing
// fails if the document is fixed and never re-added, so the guard quietly covers
// less than its name claims. Same shape as the testdata fixture table's recorded
// expected failure, which is likewise the entry that proves its own guard can
// report at all.
//
// `whole` names the block that stands alone as a file, so the expiry needs no
// per-fragment stubs: a tutorial's complete example is self-contained, and the
// blocks the prose draws out of it are fragments of that one.
type excludedDoc struct {
	path      string
	whole     int    // index into the document's fenced ```go blocks
	wantError string // must still appear, or the exclusion has gone stale
	issue     string
}

// EMPTY, and the entry that was here is why it can be: signals-and-human-loop.md
// was the last exclusion, and cleat#3079 repaired it, so the document moved up
// into tutorialDocs. The machinery stays for the next document that needs it
// rather than being removed with its only user -- an empty list is what the
// expiry test reports as "nothing to expire", not as a gap in coverage.
var excludedDocs = []excludedDoc{}

var goFenceRe = regexp.MustCompile("(?ms)^```go\\s*$\\n(.*?)^```\\s*$")

// goBlocks returns every fenced ```go block in a markdown file, in order.
func goBlocks(t *testing.T, markdown string) []string {
	t.Helper()
	var out []string
	for _, m := range goFenceRe.FindAllStringSubmatch(markdown, -1) {
		out = append(out, m[1])
	}
	return out
}

// assemble turns a tutorial's own Go blocks into one compilable file, wrapping
// the blocks the document presents as fragments. `override` replaces the marker
// block with different text, which is how the known-positive is built.
func assemble(t *testing.T, doc tutorialDoc, markdown, override string) string {
	t.Helper()
	blocks := goBlocks(t, markdown)
	if len(blocks) == 0 {
		t.Fatalf("%s has no fenced ```go block -- the extractor or the file has drifted", doc.path)
	}
	var parts []string
	for _, b := range blocks {
		text := b
		if override != "" && doc.knownBroken != nil && strings.Contains(b, doc.knownBroken.marker) {
			text = override
		}
		if frag := matchingFragment(doc, text); frag != nil {
			text = frag.header + "\n" + text + frag.footer
		}
		parts = append(parts, text)
	}
	return strings.Join(parts, "\n\n")
}

// matchingFragment returns the fragment whose marker the text contains, or nil.
// A marker matched by two blocks wraps both, which is why the tables below pick
// markers that occur in exactly one.
func matchingFragment(doc tutorialDoc, text string) *tutorialFragment {
	for i := range doc.fragments {
		if strings.Contains(text, doc.fragments[i].marker) {
			return &doc.fragments[i]
		}
	}
	return nil
}

var packageClauseRe = regexp.MustCompile(`(?m)^package `)

// snippetFiles returns the files to compile for one document, with `override`
// substituted for the block its marker identifies when `override` is non-empty.
func snippetFiles(t *testing.T, doc tutorialDoc, markdown, override string) []tutorialSnippet {
	t.Helper()
	if !doc.perBlock {
		return []tutorialSnippet{{src: assemble(t, doc, markdown, override), label: "the joined program"}}
	}
	firstLine := func(s string) string {
		for _, l := range strings.Split(s, "\n") {
			if t := strings.TrimSpace(l); t != "" {
				return t
			}
		}
		return "(empty block)"
	}
	blocks := goBlocks(t, markdown)
	// EVERY FRAGMENT MUST MATCH EXACTLY ONE BLOCK, asserted here rather than left to
	// the tables' habit of picking markers that occur once. A marker matching two
	// blocks wraps both in the same scaffolding, which refuses a CORRECT document:
	// the block's own text is still compiled verbatim, so a collision is a false RED
	// rather than a false green -- cleat-review built one to check the direction and
	// found no hole -- but it costs a debugging session on a page that was fine. A
	// marker matching none is wrong the same way. Worth a loop rather than a comment
	// because cleat#3112 is about to grow this table fivefold.
	// The known-positive's marker is checked too, not only the fragments': the
	// substitution it drives is what the negative control rests on, and a marker
	// there that matched no block is exactly the "unexercised control reads as a
	// clean run" failure.
	markers := make([]string, 0, len(doc.fragments)+1)
	for _, f := range doc.fragments {
		markers = append(markers, f.marker)
	}
	if doc.knownBroken != nil {
		markers = append(markers, doc.knownBroken.marker)
	}
	for _, marker := range markers {
		n := 0
		for _, b := range blocks {
			if strings.Contains(b, marker) {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("%s: the marker %q matches %d block(s), want exactly one. A marker matching "+
				"two wraps both blocks in the same scaffolding and refuses a correct document; "+
				"one matching none leaves a block with no package clause.",
				doc.path, marker, n)
		}
	}
	var out []tutorialSnippet
	for i, b := range blocks {
		text := b
		if override != "" && doc.knownBroken != nil && strings.Contains(b, doc.knownBroken.marker) {
			text = override
		}
		label := fmt.Sprintf("block %d: %s", i, firstLine(text))
		switch frag := matchingFragment(doc, text); {
		case frag != nil:
			out = append(out, tutorialSnippet{src: frag.header + "\n" + text + frag.footer, support: frag.support, label: label})
		case packageClauseRe.MatchString(text):
			out = append(out, tutorialSnippet{src: text, label: label}) // a complete example: nothing to add
		default:
			t.Fatalf("%s block %d has neither a package clause nor a fragment stub, so it "+
				"could never compile -- add a tutorialFragment for it rather than letting the "+
				"walk skip it.\nblock:\n%s", doc.path, i, text)
		}
	}
	if len(out) == 0 {
		t.Fatalf("%s declares perBlock and yielded no snippet at all -- the extractor or the "+
			"document has drifted, so this guard would pass over nothing", doc.path)
	}
	return out
}

// vetSnippet writes an assembled file into a scratch module that resolves the
// SDK from the PUBLISHED release -- what a reader following the tutorial gets --
// and returns `go vet`'s combined output.
// snippetModule resolves the published SDK ONCE per test binary and returns the
// go.mod and go.sum every snippet is vetted against.
//
// Resolve the SDK the way a reader following the tutorial does: the PUBLISHED
// release, from the module proxy -- not this checkout.
//
// `@latest` rather than a pinned version, so this tracks the release instead of
// rotting at the next tag. Before cleat#3083 this module had no dependency at all
// and relied on resolveScaffoldAgainstThisCheckout to supply one, so the guard was
// compiling snippets against the working tree rather than against what a reader
// resolves.
//
// ⚠ A PAGE NOW LEANS ON THIS CHOICE. docs/how-to/test-workflows.md's note on the
// timeout pair (cleat#3098) explains that its two blocks cannot be joined yet
// because the primitive that orders them is not in the published SDK -- true only
// while this resolves `@latest`. If the resolution ever changes to a pin, or to
// this checkout, that sentence goes stale with it, and the page would be citing
// this guard for a policy it no longer holds.
//
// ONCE, NOT PER SNIPPET. Resolution is a module-proxy round trip, and cleat#3112
// took this guard from 9 snippets to 50. Fifty round trips a run is slower and,
// worse, fifty chances to lose the run to a transient proxy error -- which is
// exactly what a `go get` failure here does, since it `Fatalf`s. Resolving once
// keeps the property this guard deliberately chose (a network failure FAILS,
// rather than skipping and reporting clean over nothing -- see the file header's
// "not guarded on network availability") while shrinking the exposure to a single
// call. The failure is still loud; it is just no longer unpredictable which
// snippet it lands on, or how often.
var (
	snippetModuleOnce sync.Once
	snippetModFile    []byte
	snippetSumFile    []byte
	snippetModuleErr  error
)

func snippetModule() ([]byte, []byte, error) {
	snippetModuleOnce.Do(func() {
		dir, err := os.MkdirTemp("", "cleat-snippet-module")
		if err != nil {
			snippetModuleErr = err
			return
		}
		defer os.RemoveAll(dir)
		if err := os.WriteFile(filepath.Join(dir, "go.mod"),
			[]byte("module tutorial_snippet\n\ngo 1.27.0\n"), 0o644); err != nil {
			snippetModuleErr = err
			return
		}
		// A seed file that imports the SDK, so `go mod tidy` KEEPS the require
		// instead of pruning a module nothing in the directory imports.
		if err := os.WriteFile(filepath.Join(dir, "seed.go"),
			[]byte("package tutorial_snippet\n\nimport _ \"github.com/cleat-team/cleat/cleat\"\n"), 0o644); err != nil {
			snippetModuleErr = err
			return
		}
		for _, args := range [][]string{
			{"get", "github.com/cleat-team/cleat/cleat@latest"},
			{"mod", "tidy"},
		} {
			cmd := exec.Command("go", args...)
			cmd.Dir = dir
			if out, err := cmd.CombinedOutput(); err != nil {
				snippetModuleErr = fmt.Errorf("go %s: %w\n%s", strings.Join(args, " "), err, out)
				return
			}
		}
		if snippetModFile, err = os.ReadFile(filepath.Join(dir, "go.mod")); err != nil {
			snippetModuleErr = err
			return
		}
		snippetSumFile, _ = os.ReadFile(filepath.Join(dir, "go.sum"))
	})
	return snippetModFile, snippetSumFile, snippetModuleErr
}

func vetSnippet(t *testing.T, src, support string) string {
	t.Helper()
	mod, sum, err := snippetModule()
	if err != nil {
		t.Fatalf("resolve the published SDK: %v", err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), mod, 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	if len(sum) > 0 {
		if err := os.WriteFile(filepath.Join(dir, "go.sum"), sum, 0o644); err != nil {
			t.Fatalf("write go.sum: %v", err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "snippet.go"), []byte(src), 0o644); err != nil {
		t.Fatalf("write snippet.go: %v", err)
	}
	// A sibling file for a block that is a complete example yet consumes code the
	// page assumes the reader has written -- see tutorialFragment.support. It is a
	// separate file rather than more text on the snippet because the snippet has
	// its own package clause and its own import set, and neither may be edited
	// without changing what the page shows.
	if support != "" {
		if err := os.WriteFile(filepath.Join(dir, "support.go"), []byte(support), 0o644); err != nil {
			t.Fatalf("write support.go: %v", err)
		}
	}
	cmd := exec.Command("go", "vet", "./...")
	cmd.Dir = dir
	// -mod=mod so a snippet whose import set is a SUBSET of the seed's does not
	// fail on a go.mod it would otherwise want to rewrite. It cannot reach the
	// network for a version: the module is already required and summed.
	cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod")
	out, err := cmd.CombinedOutput()
	if err == nil {
		return ""
	}
	return string(out)
}

func TestTutorialGoSnippetsCompile(t *testing.T) {
	root := repoRoot(t)
	for _, doc := range tutorialDocs {
		t.Run(doc.path, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(root, doc.path))
			if err != nil {
				t.Fatalf("read %s: %v", doc.path, err)
			}
			markdown := string(raw)

			for _, snip := range snippetFiles(t, doc, markdown, "") {
				if out := vetSnippet(t, snip.src, snip.support); out != "" {
					t.Errorf("a Go snippet in %s does not type-check, so a reader copying it gets a build failure.\n%s\n%s", doc.path, snip.label, out)
				}
			}

			// The known-positive. Without it, a guard that had stopped
			// extracting anything would also report nothing here.
			if doc.knownBroken == nil {
				return
			}
			// Find the snippet the replacement actually landed in. Its ABSENCE is
			// its own failure: a marker that matched no block means the negative
			// control was never exercised, and an unexercised control reads
			// exactly like a clean run.
			var broken, brokenSupport string
			for _, snip := range snippetFiles(t, doc, markdown, doc.brokenText) {
				if strings.Contains(snip.src, doc.brokenText) {
					broken, brokenSupport = snip.src, snip.support
				}
			}
			if broken == "" {
				t.Fatalf("the known-positive for %s did not land in any snippet: its marker %q "+
					"matched no block, so this guard's negative control measured nothing.",
					doc.path, doc.knownBroken.marker)
			}
			out := vetSnippet(t, broken, brokenSupport)
			if out == "" {
				t.Errorf("the known-positive in %s compiled. This guard cannot report a failure at all, "+
					"so its pass above is not evidence.", doc.path)
				return
			}
			// Printed on every run, not only on failure: the negative control
			// for a guard is the thing that proves it can still report, and one
			// that is only visible when something already went wrong is not
			// evidence that it ran.
			t.Logf("known-positive rejected the broken snippet, as it must:\n%s", strings.TrimSpace(out))

			// Asserted on the TEXT, not only on the status: a second, correct
			// mechanism could otherwise supply the failure and certify a guard
			// that has lost the check it is named after.
			if !strings.Contains(out, doc.brokenWant) {
				t.Errorf("the known-positive in %s failed, but not for the reason this guard exists to catch.\n"+
					"want an error containing %q\ngot:\n%s", doc.path, doc.brokenWant, out)
			}
		})
	}
}

var mdLinkRe = regexp.MustCompile(`\]\(([^)\s]+)\)`)

// TestTutorialRelativeLinksResolve guards the other half of the same gap: the
// same file that carried a snippet which could not compile carried two links
// pointing at paths that do not exist (`docs/guide/` was reorganised away, and
// a sibling link was written one directory too shallow). Nothing checked them.
// deadLinks returns the relative link targets in markdown that do not resolve
// against the file's own directory.
func deadLinks(docDir, markdown string) []string {
	var dead []string
	for _, m := range mdLinkRe.FindAllStringSubmatch(markdown, -1) {
		target := m[1]
		if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") ||
			strings.HasPrefix(target, "mailto:") || strings.HasPrefix(target, "#") {
			continue
		}
		rel := strings.SplitN(target, "#", 2)[0]
		if rel == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(docDir, rel)); err != nil {
			dead = append(dead, target)
		}
	}
	return dead
}

func TestTutorialRelativeLinksResolve(t *testing.T) {
	root := repoRoot(t)

	// The negative control, FIRST, because it is the thing that makes every
	// clean result below mean something: this checker must report a link to a
	// path that is definitely not there. Without it, a pattern that silently
	// matched nothing would report every tutorial as clean.
	synthetic := "[gone](definitely-not-a-real-path-3072.md)"
	if got := deadLinks(t.TempDir(), synthetic); len(got) != 1 {
		t.Fatalf("the link checker reported %v for a link that certainly does not resolve; it cannot "+
			"report a dead link, so its silence below is not evidence", got)
	}

	// EVERY tutorial, not only the snippet-compiled ones: link rot is not
	// confined to the files that happen to hold Go blocks, and this half needs
	// no per-document stubs.
	docs, err := filepath.Glob(filepath.Join(root, "docs", "tutorials", "*.md"))
	if err != nil || len(docs) == 0 {
		t.Fatalf("no tutorials matched docs/tutorials/*.md (err=%v) -- the check would be vacuous", err)
	}
	for _, abs := range docs {
		rel, _ := filepath.Rel(root, abs)
		t.Run(rel, func(t *testing.T) {
			raw, err := os.ReadFile(abs)
			if err != nil {
				t.Fatalf("read %s: %v", rel, err)
			}
			if dead := deadLinks(filepath.Dir(abs), string(raw)); len(dead) > 0 {
				t.Errorf("%s links to %v, which do not exist", rel, dead)
			}
		})
	}
}

// TestExcludedTutorialsStillFailForTheRecordedReason is what stops an exclusion
// from being a silent, permanent hole in the coverage above.
func TestExcludedTutorialsStillFailForTheRecordedReason(t *testing.T) {
	root := repoRoot(t)
	if len(excludedDocs) == 0 {
		t.Log("no tutorials are excluded; nothing to expire")
		return
	}
	for _, doc := range excludedDocs {
		t.Run(doc.path, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(root, doc.path))
			if err != nil {
				t.Fatalf("read %s: %v", doc.path, err)
			}
			blocks := goBlocks(t, string(raw))
			if doc.whole >= len(blocks) {
				t.Fatalf("%s has %d fenced ```go block(s) and the exclusion names block %d -- the file has "+
					"moved on, so this check is looking in the wrong place", doc.path, len(blocks), doc.whole)
			}
			out := vetSnippet(t, blocks[doc.whole], "")
			if out == "" {
				t.Errorf("%s compiles now. Move it into tutorialDocs and delete it from excludedDocs: its "+
					"exclusion (%s) has lapsed, and leaving the entry here means this guard silently covers "+
					"less than it claims.", doc.path, doc.issue)
				return
			}
			if !strings.Contains(out, doc.wantError) {
				t.Errorf("%s no longer fails for the recorded reason.\nwant an error containing %q\ngot:\n%s\n"+
					"Either the repair is partial or the file moved on for another reason; check %s.",
					doc.path, doc.wantError, out, doc.issue)
			}
		})
	}
}
