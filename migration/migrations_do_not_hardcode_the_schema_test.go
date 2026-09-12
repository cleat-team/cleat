package migration_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Every place a migration file names the schema cleat builds into, it has to
// ask rather than assert. cleat#1287.
//
// WHY A GUARD AND NOT A CONVENTION. Before #1287 the answer was the literal
// `public`, written into nineteen files as `SET search_path = public;`, into
// four SECURITY DEFINER function attributes, into a dozen GRANTs and into
// three catalogue lookups. Twenty-five files did not write it, so the core
// schema was built across two schemas at once whenever --schema was set and
// the run died at 020_event_intent.sql. Nothing failed for the twenty-five,
// nothing failed for the nineteen, and the defect was only visible in the
// interaction.
//
// That is the shape a guard is for: no single file is wrong, and the next file
// someone adds will copy whichever neighbour they happened to open.
//
// THE RULE, which is not "never write public" but "never write it where it
// means the configured schema":
//
//   - a statement that takes a schema NAME -- GRANT ... ON SCHEMA, ALTER
//     ROLE, ALTER DEFAULT PRIVILEGES -- asks with current_schema(), inside a
//     DO block and format(%I).
//   - a SECURITY DEFINER function's own search_path attribute uses
//     `SET search_path FROM CURRENT`, which freezes the session value onto
//     the function at creation time.
//   - everything else is unqualified and resolves through search_path.
//   - do not set search_path in a file at all. The runner owns it.
//
// WHY NOT A PLACEHOLDER, since that is the obvious answer and was the first
// one here. A `:"CLEAT_SCHEMA"` that psql expands natively looks free, and it
// is not: it becomes a contract every applier of these files has to honour.
// This repository has eleven of them. The one that found it was
// engine/schema_bootstrap_test.go, which reads the shipped files and Execs
// them raw precisely to prove a fresh deployment works, and which failed with
//
//	applying shipped migration 005_app_role.sql failed: pq: syntax error at
//	or near ":" (42601)
//
// current_schema() and FROM CURRENT are native SQL, so every applier gets the
// same answer with no substitution step to forget.
func TestMigrationsDoNotHardcodeTheSchema(t *testing.T) {
	dir := filepath.Join(migrationsRoot(t), "postgres")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}

	// Lines that name public deliberately, each with the reason. Matched on
	// the exact trimmed text so that a NEW use of public in the same file is
	// still a failure -- an allowlist keyed on the filename would exempt the
	// file rather than the line.
	allowed := map[string]string{
		`WHERE schemaname = 'public'`:                                "063: rewrites the pre-#1278 kv_store policy, and the databases that have one ran under the plugin search_path pin, so their kv_store is in public by construction",
		`DROP POLICY kv_store_tenant_isolation ON public.kv_store;`:  "063: same",
		`CREATE POLICY kv_store_tenant_isolation ON public.kv_store`: "063: same",
	}

	var findings []string
	examined := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		examined++
		for i, line := range strings.Split(stripSQLComments(string(src)), "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" {
				continue
			}
			if _, ok := allowed[trimmed]; ok {
				continue
			}
			if bareWordPublic.MatchString(line) {
				findings = append(findings, e.Name()+":"+strconv.Itoa(i+1)+": "+trimmed)
			}
			if setsSearchPath.MatchString(line) {
				findings = append(findings, e.Name()+":"+strconv.Itoa(i+1)+
					": sets search_path; the runner owns it: "+trimmed)
			}
		}
	}

	// A scan that examined nothing reports the same zero as a clean tree. The
	// count is not a fixed number on purpose -- the population grows -- but it
	// is never small.
	if examined < 20 {
		t.Fatalf("examined only %d migration files; the scan is not reaching them", examined)
	}
	if len(findings) > 0 {
		t.Errorf("%d line(s) name the schema instead of asking for it "+
			"(examined %d files):\n  %s\n\n"+
			"Ask for the schema with current_schema() (in a DO block, via "+
			"format with the identifier verb) or with `SET search_path FROM "+
			"CURRENT` on a function "+
			"attribute. See this test's doc comment for why, and add a line to "+
			"`allowed` with a reason if public is genuinely meant.",
			len(findings), examined, strings.Join(findings, "\n  "))
	}
}

var (
	// \b would also match the "public" in pg_catalog-ish compounds; the
	// surrounding-character class is what keeps public_foo from counting.
	bareWordPublic = regexp.MustCompile(`(^|[^A-Za-z0-9_])public([^A-Za-z0-9_]|$)`)
	// A SET that supplies a VALUE -- `= something` or `TO something` -- which
	// is either a statement (the runner owns search_path, so a file must not
	// set it) or a function attribute naming a fixed schema (which would not
	// follow --schema). `SET search_path FROM CURRENT` is neither: it captures
	// whatever the runner already established, which is the supported way for
	// a SECURITY DEFINER function to carry one.
	//
	// The first version of this rule discriminated on a trailing semicolon --
	// attributes written before `AS $$...$$` have none. That is true of four
	// of the five in this tree and false of the fifth, because
	// create_tenant_role carries its attribute AFTER the body, where it ends
	// the CREATE statement and takes the semicolon with it. A discriminator
	// that happens to hold for the examples you looked at is the subject of
	// this whole file.
	setsSearchPath = regexp.MustCompile(`(?i)^\s*SET\s+(LOCAL\s+)?search_path\s*(=|\bTO\b)`)
)

// stripSQLComments removes -- comments and /* */ blocks so that prose about a
// rule is not read as an instance of it. This file's own subject is a text
// search that could not tell those apart.
func stripSQLComments(s string) string {
	s = regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(s, "")
	out := make([]string, 0, 256)
	for _, line := range strings.Split(s, "\n") {
		inSingle := false
		cut := -1
		for i := 0; i+1 < len(line); i++ {
			switch {
			case line[i] == '\'':
				inSingle = !inSingle
			case !inSingle && line[i] == '-' && line[i+1] == '-':
				cut = i
			}
			if cut >= 0 {
				break
			}
		}
		if cut >= 0 {
			line = line[:cut]
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}
