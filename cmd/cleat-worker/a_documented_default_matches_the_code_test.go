package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// docs/reference/worker-config.md states a default for every flag it documents,
// and until now nothing compared those statements against the code. cleat#1829
// changed --max-quota-events from 0 to 50000 and the reference went on saying 0
// for two releases; the sentence beside it told an operator that a runaway
// workflow would be KILLED at the bound when in fact the run rolls over, which
// is an argument for never setting the flag. The stale reference worked against
// the safety bound the change had just added. Nothing noticed, because nothing
// looked.
//
// # Why this reads the flag rather than its declaration
//
// The obvious guard greps `flag.Int(...)` out of config.go and compares the
// literal against the doc cell. Measured on a clean tree that reports fourteen
// disagreements of which THIRTEEN are the instrument: eight are duration
// notation (code `5*time.Minute`, doc `5m`), three are a parenthetical gloss in
// the cell (`1048576` (1 MiB)), and two are symbolic defaults declared in
// another package (engine.DefaultMaxVersionAge). A guard that cries thirteen
// times on a clean tree gets deleted, or baselined -- and a baseline of
// thirteen formatting differences then hides the fourteenth.
//
// So this does not model the declaration. It asks the flag package what the
// default IS, after the compiler has resolved every const, every cross-package
// reference and every arithmetic expression, and compares that against the doc
// cell PARSED AS THE TYPE THE FLAG DECLARES. 5m and 5m0s are the same
// time.Duration; 0.80 and 0.8 are the same float64; 720h needs no arithmetic
// parser because nobody is parsing arithmetic. Ten of the thirteen false
// positives are not suppressed here, they are absent -- there is no expression
// to misread.
//
// The remaining three are the gloss, and the fix for those is to parse the CELL
// rather than the line: the Default column legitimately carries a reader-facing
// gloss, and that gloss is worth keeping. documentedValue takes the first
// backticked span and treats the rest as prose.

const workerConfigDoc = "../../docs/reference/worker-config.md"

// documentedFloor is a scanner control. Every failure mode of the doc parser --
// a heading style that changes, a table that grows a column, a file that moves
// -- produces the same symptom: zero flags scanned and a comparison that agrees
// with everything. An empty parse is what a clean run looks like, so the parse
// has to assert its own size. 60 flags are documented today.
const documentedFloor = 55

type docDefault struct {
	flag string // without the leading --
	cell string // the raw Default cell, gloss included
	line int
}

// parseDocumentedDefaults reads the '### --flag' sections of the reference and
// returns the Default cell of each one's attribute table. The column is located
// by its header name, never by position: the tables carry either 'Env var' or
// 'Description' as a third column and one of them could grow a fourth.
func parseDocumentedDefaults(src string) []docDefault {
	var out []docDefault
	var pending string
	var pendingLine int
	var header []string

	for i, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)

		if strings.HasPrefix(trimmed, "### ") {
			name := strings.TrimSpace(strings.TrimPrefix(trimmed, "### "))
			name = strings.Trim(name, "`")
			if strings.HasPrefix(name, "--") {
				pending = strings.TrimPrefix(name, "--")
				pendingLine = i + 1
				header = nil
			} else {
				pending = ""
			}
			continue
		}

		if pending == "" || !strings.HasPrefix(trimmed, "|") {
			continue
		}

		cells := splitRow(trimmed)
		if header == nil {
			header = cells
			continue
		}
		if isRuleRow(cells) {
			continue
		}

		col := indexOf(header, "Default")
		if col >= 0 && col < len(cells) {
			out = append(out, docDefault{flag: pending, cell: cells[col], line: pendingLine})
		}
		pending = ""
	}
	return out
}

func splitRow(row string) []string {
	row = strings.TrimPrefix(row, "|")
	row = strings.TrimSuffix(row, "|")
	parts := strings.Split(row, "|")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

func isRuleRow(cells []string) bool {
	for _, c := range cells {
		if c == "" || strings.Trim(c, "-: ") != "" {
			return false
		}
	}
	return true
}

func indexOf(cells []string, want string) int {
	for i, c := range cells {
		if strings.EqualFold(c, want) {
			return i
		}
	}
	return -1
}

// documentedValue takes the value out of a Default cell and leaves the prose
// behind. `1048576` (1 MiB) is one value and one gloss, not a disagreement.
func documentedValue(cell string) string {
	if i := strings.Index(cell, "`"); i >= 0 {
		if j := strings.Index(cell[i+1:], "`"); j >= 0 {
			return cell[i+1 : i+1+j]
		}
	}
	// No backticks: drop a trailing parenthetical and take what is left.
	if i := strings.Index(cell, "("); i > 0 {
		cell = cell[:i]
	}
	return strings.TrimSpace(cell)
}

type mismatch struct {
	flag   string
	doc    string
	actual string
	reason string
}

func (m mismatch) String() string {
	return fmt.Sprintf("--%s: doc says %q, code says %q (%s)", m.flag, m.doc, m.actual, m.reason)
}

// compareDefaults parses each documented value as the type its flag declares.
// A flag that is documented but does not exist is a finding of its own -- that
// is the other direction of the same drift, and it is the direction a reader
// hits hardest, because the flag they were told to set is refused.
func compareDefaults(docs []docDefault, actual map[string]flag.Getter) []mismatch {
	var out []mismatch
	for _, d := range docs {
		g, ok := actual[d.flag]
		if !ok {
			out = append(out, mismatch{d.flag, documentedValue(d.cell), "", "documented flag does not exist"})
			continue
		}
		if reason := disagrees(documentedValue(d.cell), g.Get()); reason != "" {
			out = append(out, mismatch{d.flag, documentedValue(d.cell), fmt.Sprint(g.Get()), reason})
		}
	}
	return out
}

// disagrees returns "" when the documented text denotes the same value as the
// flag's actual default, and otherwise says why not.
func disagrees(doc string, got any) string {
	switch want := got.(type) {
	case time.Duration:
		d, err := time.ParseDuration(doc)
		if err != nil {
			return "not a duration"
		}
		if d != want {
			return "different duration"
		}
	case float64:
		f, err := strconv.ParseFloat(doc, 64)
		if err != nil {
			return "not a number"
		}
		if f != want {
			return "different number"
		}
	case bool:
		b, err := strconv.ParseBool(doc)
		if err != nil {
			return "not a bool"
		}
		if b != want {
			return "different bool"
		}
	case int:
		return disagreesInt(doc, int64(want))
	case int64:
		return disagreesInt(doc, want)
	case uint:
		return disagreesInt(doc, int64(want))
	case string:
		// The reference quotes string defaults; "" and `""` are the same empty.
		if strings.Trim(doc, `"`) != want {
			return "different string"
		}
	default:
		return fmt.Sprintf("unsupported flag type %T", got)
	}
	return ""
}

func disagreesInt(doc string, want int64) string {
	n, err := strconv.ParseInt(strings.ReplaceAll(doc, "_", ""), 10, 64)
	if err != nil {
		return "not an integer"
	}
	if n != want {
		return "different integer"
	}
	return ""
}

// workerFlags is the code side: every flag cleat-worker registers, and nothing
// else. The test binary's own flags live on the same FlagSet, so they are
// excluded by name -- and the exclusion is asserted below rather than trusted,
// because a filter that drops too much makes this check agree with anything.
func workerFlags() map[string]flag.Getter {
	out := map[string]flag.Getter{}
	flag.VisitAll(func(f *flag.Flag) {
		if strings.HasPrefix(f.Name, "test.") {
			return
		}
		if g, ok := f.Value.(flag.Getter); ok {
			out[f.Name] = g
		}
	})
	return out
}

func TestEveryDocumentedFlagDefaultMatchesTheCode(t *testing.T) {
	src, err := os.ReadFile(workerConfigDoc)
	if err != nil {
		t.Fatalf("read %s: %v", workerConfigDoc, err)
	}

	docs := parseDocumentedDefaults(string(src))
	if len(docs) < documentedFloor {
		t.Fatalf("scanned %d documented flags in %s, floor is %d -- the parser has stopped seeing the tables, "+
			"and a parser that sees nothing agrees with everything", len(docs), workerConfigDoc, documentedFloor)
	}

	actual := workerFlags()
	if _, ok := actual["max-quota-events"]; !ok {
		t.Fatal("cleat-worker's flags are not registered on flag.CommandLine, so the code side of this check is empty")
	}
	if _, ok := actual["test.v"]; ok {
		t.Fatal("the test binary's own flags leaked into the worker flag set")
	}

	bad := compareDefaults(docs, actual)
	if len(bad) > 0 {
		lines := make([]string, len(bad))
		for i, m := range bad {
			lines[i] = "  " + m.String()
		}
		t.Fatalf("%d of %d documented defaults disagree with the code:\n%s\n\n"+
			"Update %s, or the flag, so an operator reading the reference is told what the worker will do.",
			len(bad), len(docs), strings.Join(lines, "\n"), workerConfigDoc)
	}

	t.Logf("%d documented flags checked against %d registered flags", len(docs), len(actual))
}

// TestTheComparisonReportsAMisstatedDefault is the known-positive. A guard that
// has only ever been observed to pass is indistinguishable from one that cannot
// fail, and this one is built to tolerate four shapes of difference -- so the
// question it has to answer is not "does it run" but "with that much tolerance,
// can it still say no".
func TestTheComparisonReportsAMisstatedDefault(t *testing.T) {
	fs := flag.NewFlagSet("known-positive", flag.ContinueOnError)
	fs.Int("max-quota-events", 50000, "")
	fs.Duration("compaction-interval", 5*time.Minute, "")
	actual := getters(fs)

	cases := []struct {
		name string
		doc  string
		flag string
		want string
	}{
		{"the stale integer this check was written for", "`0`", "max-quota-events", "different integer"},
		{"an integer off by a digit", "`5000`", "max-quota-events", "different integer"},
		{"a duration stated in the wrong unit", "`5s`", "compaction-interval", "different duration"},
		{"a gloss that disagrees with its own value is judged on the value", "`0` (50000)", "max-quota-events", "different integer"},
		{"a cell that states no value at all", "(host default)", "max-quota-events", "not an integer"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bad := compareDefaults([]docDefault{{flag: tc.flag, cell: tc.doc}}, actual)
			if len(bad) != 1 {
				t.Fatalf("documented %s as %s: got %d findings, want 1 -- this guard cannot report a wrong default",
					tc.flag, tc.doc, len(bad))
			}
			if bad[0].reason != tc.want {
				t.Errorf("reason = %q, want %q", bad[0].reason, tc.want)
			}
		})
	}

	t.Run("a documented flag the binary does not have", func(t *testing.T) {
		bad := compareDefaults([]docDefault{{flag: "max-quota-evnts", cell: "`50000`"}}, actual)
		if len(bad) != 1 || bad[0].reason != "documented flag does not exist" {
			t.Fatalf("got %v, want one 'documented flag does not exist'", bad)
		}
	})
}

// TestTheComparisonAcceptsTheThirteenFalsePositives pins the tolerance, using
// the exact cells the naive version reported on a clean tree. Without this, the
// natural repair to a future failure is to narrow the comparison until the tree
// goes green, which is how thirteen of these came back.
func TestTheComparisonAcceptsTheThirteenFalsePositives(t *testing.T) {
	fs := flag.NewFlagSet("negative-control", flag.ContinueOnError)
	fs.Duration("compaction-interval", 5*time.Minute, "")
	fs.Duration("poll", 500*time.Millisecond, "")
	fs.Duration("max-workflow-duration", 0, "")
	fs.Duration("version-gc-max-age", 720*time.Hour, "")
	fs.Int64("max-body-size", 1048576, "")
	fs.Int("version-gc-min-versions", 3, "")
	fs.Float64("memory-soft-limit", 0.80, "")
	fs.String("driver", "postgres", "")
	fs.String("api-addr", "", "")
	fs.Bool("require-auth", true, "")
	actual := getters(fs)

	cells := []docDefault{
		{flag: "compaction-interval", cell: "`5m`"},            // code 5*time.Minute
		{flag: "poll", cell: "`500ms`"},                        // code 500*time.Millisecond
		{flag: "max-workflow-duration", cell: "`0`"},           // 0 is a duration
		{flag: "version-gc-max-age", cell: "`720h` (30 days)"}, // cross-package const + gloss
		{flag: "max-body-size", cell: "`1048576` (1 MiB)"},     // gloss
		{flag: "version-gc-min-versions", cell: "`3`"},         // cross-package const
		{flag: "memory-soft-limit", cell: "`0.80`"},            // prints as 0.8
		{flag: "driver", cell: "`\"postgres\"`"},               // quoted string
		{flag: "api-addr", cell: "`\"\"`"},                     // quoted empty
		{flag: "require-auth", cell: "`true`"},                 // bool
	}

	if bad := compareDefaults(cells, actual); len(bad) > 0 {
		t.Fatalf("the comparison reported %d of %d correct cells:\n%v", len(bad), len(cells), bad)
	}
}

// TestTheComparisonReportsTheDriftItWasWrittenFor runs the check, unchanged,
// against the reference as it actually stood at 9b29a54b -- the last commit
// before #1839 -- and requires it to name the two flags that were really wrong
// while staying silent about the four beside them.
//
// The synthetic known-positives above prove the comparison can say no. They do
// not prove it says no to THIS, because I chose their inputs after writing the
// comparison, so they cannot disagree with me. These bytes were written before
// the check existed. The excerpt is verbatim; reproduce the whole-file run with
//
//	git show 9b29a54b:docs/reference/worker-config.md
//
// which reports the same two and nothing else across all sixty sections.
func TestTheComparisonReportsTheDriftItWasWrittenFor(t *testing.T) {
	src, err := os.ReadFile("testdata/stale-reference-excerpt.txt")
	if err != nil {
		t.Fatalf("read the stale-reference fixture: %v", err)
	}

	docs := parseDocumentedDefaults(string(src))
	if len(docs) != 6 {
		t.Fatalf("parsed %d sections from the fixture, want 6 -- two that were wrong and four that only looked it", len(docs))
	}

	// The code side is today's, because that is the comparison that was missing:
	// the code moved in #1829 and the reference did not follow.
	got := map[string]string{}
	for _, m := range compareDefaults(docs, workerFlags()) {
		got[m.flag] = m.reason
	}

	want := map[string]string{
		"max-quota-events":     "different integer", // #1829 moved it to 50000; the doc still said 0
		"compaction-threshold": "not an integer",    // the cell stated "(host default)" against an actual 100
	}
	for f, reason := range want {
		if got[f] != reason {
			t.Errorf("--%s: reported %q, want %q -- this guard would not have caught the drift it exists for", f, got[f], reason)
		}
		delete(got, f)
	}
	for f, reason := range got {
		t.Errorf("--%s reported as %q, but its cell was correct -- this is the false-positive class that gets a guard deleted", f, reason)
	}
}

// TestTheValueIsSeparatedFromItsGloss pins both branches of documentedValue.
// The backticked branch is exercised by every cell in the reference; the bare
// branch is exercised by none of them today, which is exactly why it needs a
// test -- an unexercised branch can be deleted without anything going red, and
// then the first cell that drops its backticks is read as empty.
func TestTheValueIsSeparatedFromItsGloss(t *testing.T) {
	for _, tc := range []struct{ cell, want string }{
		{"`50000`", "50000"},
		{"`1048576` (1 MiB)", "1048576"},
		{"`720h` (30 days)", "720h"},
		{"`\"\"` (required)", `""`},
		{"`0` (disabled)", "0"},
		{"100 (the host default)", "100"}, // no backticks: gloss still goes
		{"100", "100"},
		{"(host default)", "(host default)"}, // no value at all, and it must not read as empty
	} {
		if got := documentedValue(tc.cell); got != tc.want {
			t.Errorf("documentedValue(%q) = %q, want %q", tc.cell, got, tc.want)
		}
	}
}

func TestTheDocParserFindsTheDefaultColumnByName(t *testing.T) {
	const src = "### --alpha\n" +
		"\n" +
		"| Type | Default | Env var |\n" +
		"|------|---------|---------|\n" +
		"| string | `\"a\"` | `A` |\n" +
		"\n" +
		"### --beta\n" +
		"\n" +
		"| Type | Env var | Default |\n" + // same table, columns swapped
		"|------|---------|---------|\n" +
		"| int | `B` | `7` |\n" +
		"\n" +
		"### not a flag\n" +
		"\n" +
		"| Type | Default |\n" +
		"|------|---------|\n" +
		"| x | `9` |\n"

	got := parseDocumentedDefaults(src)
	if len(got) != 2 {
		t.Fatalf("parsed %d sections, want 2 (%v)", len(got), got)
	}
	if got[0].flag != "alpha" || documentedValue(got[0].cell) != `"a"` {
		t.Errorf("alpha = %+v", got[0])
	}
	if got[1].flag != "beta" || documentedValue(got[1].cell) != "7" {
		t.Errorf("beta read the wrong column: %+v", got[1])
	}
}

func getters(fs *flag.FlagSet) map[string]flag.Getter {
	out := map[string]flag.Getter{}
	fs.VisitAll(func(f *flag.Flag) {
		out[f.Name] = f.Value.(flag.Getter)
	})
	return out
}
