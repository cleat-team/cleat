package engine

import (
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestEveryDialectRefusesAnIdempotencyKeyReusedForAnotherDefinition is
// cleat#1047, asserted across the three stores at once.
//
// The defect was that idempotency_keys is keyed (key_hash, tenant_id) and
// carries no definition, so a request naming a DIFFERENT workflow matched the
// first row and was handed that workflow's id with alreadyExisted = true --
// while its own workflow never started. A caller awaiting that id reads the
// other workflow's result as its own.
//
// WHY THIS IS A SOURCE ASSERTION RATHER THAN THREE EXECUTED CASES. The
// behaviour is one comparison inside StartNewRun, written out once per dialect
// because each store has its own SQL. Executing it needs all three databases;
// what actually breaks is one store being edited and the others not, which is
// the failure this file exists to catch and which a single-dialect run would
// miss entirely. cleat#1012 was exactly that -- tenant-scoped on one dialect of
// three -- and so was #1034.
//
// The SELECT check is the half that matters most: a store that refuses on
// mismatch but never reads def_name cannot see a mismatch, so it would pass an
// assertion about the refusal alone while behaving exactly as before.
func TestEveryDialectRefusesAnIdempotencyKeyReusedForAnotherDefinition(t *testing.T) {
	for _, f := range []string{
		"store_lifecycle.go", // PostgreSQL
		"mysql_lifecycle.go", // MySQL
		"mssql_lifecycle.go", // SQL Server
	} {
		t.Run(f, func(t *testing.T) {
			src := stripGoComments(readSourceForTest(t, f))

			if !strings.Contains(src, "ErrIdempotencyKeyDefMismatch") {
				t.Errorf("%s does not refuse a definition mismatch.\n\n"+
					"A key reused for another workflow returns that workflow's id with "+
					"alreadyExisted = true, and the caller awaits the wrong run. Every "+
					"store needs the check; one left out is cleat#1012's shape.", f)
			}

			// BOTH paths, counted rather than matched once. There are two
			// lookups per store -- the ordinary one, and the re-SELECT after
			// ON CONFLICT DO NOTHING when another request won the race. CI
			// caught the first version of cleat#1047 covering only the first:
			// the race path returned the other workflow's id even though the
			// ordinary lookup refused it, which is the same defect reachable
			// only under contention, where it is hardest to see.
			// The column list is matched LOOSELY on purpose. It grew once
			// already -- cleat#1170 added input_digest beside def_name -- and a
			// pattern naming the exact list turns every future column into a
			// red guard that says "def_name is missing" when it is not. What
			// must hold is that each lookup reads BOTH discriminators and that
			// no lookup reads neither.
			lookups := regexp.MustCompile(`SELECT workflow_id([^`+"`"+`]*?)FROM idempotency_keys`).FindAllStringSubmatch(src, -1)
			withDef, withDigest := 0, 0
			for _, m := range lookups {
				if strings.Contains(m[1], "def_name") {
					withDef++
				}
				if strings.Contains(m[1], "input_digest") {
					withDigest++
				}
			}
			if len(lookups) < 2 || withDef < len(lookups) || withDigest < len(lookups) {
				t.Errorf("%s has %d idempotency lookup(s); %d read def_name and %d read "+
					"input_digest.\n\nBoth the ordinary lookup and the post-collision "+
					"re-SELECT must read both. A path that reads neither cannot see a "+
					"mismatch, so it passes the refusal assertion while behaving exactly "+
					"as before (cleat#1047, cleat#1170).",
					f, len(lookups), withDef, withDigest)
			}
			if n := strings.Count(src, "ErrIdempotencyKeyDefMismatch"); n < 2 {
				t.Errorf("%s refuses a DEFINITION mismatch on %d path(s), want both the "+
					"ordinary lookup and the post-collision re-SELECT.", f, n)
			}
			if n := strings.Count(src, "checkIdempotencyInput"); n < 2 {
				t.Errorf("%s refuses an INPUT mismatch on %d path(s), want both the ordinary "+
					"lookup and the post-collision re-SELECT.\n\ncleat#1047 shipped with "+
					"only the first and CI caught it: the race path returned the other "+
					"request's id even though the ordinary lookup refused it. cleat#1170 is "+
					"the same shape one column over.", f, n)
			}

			ins := regexp.MustCompile(`INTO idempotency_keys \(([^)]*)\)`).FindStringSubmatch(src)
			if ins == nil {
				t.Fatalf("%s has no INSERT INTO idempotency_keys at all", f)
			}
			for _, col := range []string{"def_name", "input_digest"} {
				if !strings.Contains(ins[1], col) {
					t.Errorf("%s does not WRITE %s.\n\n"+
						"Rows written without it read back NULL, which both checks treat as "+
						"\"unknown, allow\" -- correct for rows predating the column, and "+
						"silently disabling the check for every new row.", f, col)
				}
			}
		})
	}
}

// TestTheMismatchErrorIsDistinguishable pins that a caller can tell this
// refusal from any other failure, which is the whole point of refusing rather
// than returning the wrong id.
func TestTheMismatchErrorIsDistinguishable(t *testing.T) {
	if !errors.Is(ErrIdempotencyKeyDefMismatch, ErrIdempotencyKeyDefMismatch) {
		t.Fatal("the sentinel does not match itself")
	}
	if ErrIdempotencyKeyDefMismatch.Error() == "" {
		t.Error("the sentinel has no message; a caller seeing it in a log learns nothing")
	}
}

// readSourceForTest reads one of this package's own files.
//
// Fatal rather than Skip if it is missing: these files are in this repository,
// so an unreadable one means the test has lost its subject, and passing then
// would assert nothing.
func readSourceForTest(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("reading %s: %v -- this file is in the repository, so its absence "+
			"means this test is no longer checking what it names", name, err)
	}
	return string(b)
}

// stripGoComments removes // and /* */ comments so the assertions above match
// code rather than prose.
//
// Demonstrated rather than assumed to be needed. Take mssql_lifecycle.go,
// remove the check the way a refactor would, and leave the note this codebase
// would naturally write:
//
//	// Historical note (cleat#1047): this store used to run
//	//   SELECT workflow_id, def_name FROM idempotency_keys
//	// and return ErrIdempotencyKeyDefMismatch on a mismatch.
//
// Both assertions then pass on a store that has stopped doing the thing --
// reached by an ordinary act, and the surviving comment is exactly the one
// someone writes WHILE removing the code.
//
// This is the trap CLAUDE.md records twice: a grep that a retraction satisfies,
// and a scan that read "there is no import_cleat_register_query_handler here"
// as evidence the binding existed. A source assertion is still the right tool
// here -- the alternative needs three databases -- which is precisely why it
// has to read code and not commentary.
//
// String literals are left alone: the SQL this file looks for lives in raw
// string literals, and stripping those would remove the subject.
func stripGoComments(src string) string {
	var out strings.Builder
	inBlock, inLine, inRaw, inStr := false, false, false, false
	for i := 0; i < len(src); i++ {
		c := src[i]
		switch {
		case inLine:
			if c == '\n' {
				inLine = false
				out.WriteByte(c)
			}
		case inBlock:
			if c == '*' && i+1 < len(src) && src[i+1] == '/' {
				inBlock = false
				i++
			}
		case inRaw:
			out.WriteByte(c)
			if c == '`' {
				inRaw = false
			}
		case inStr:
			out.WriteByte(c)
			if c == '\\' && i+1 < len(src) {
				i++
				out.WriteByte(src[i])
			} else if c == '"' {
				inStr = false
			}
		case c == '`':
			inRaw = true
			out.WriteByte(c)
		case c == '"':
			inStr = true
			out.WriteByte(c)
		case c == '/' && i+1 < len(src) && src[i+1] == '/':
			inLine = true
			i++
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			inBlock = true
			i++
		default:
			out.WriteByte(c)
		}
	}
	return out.String()
}
