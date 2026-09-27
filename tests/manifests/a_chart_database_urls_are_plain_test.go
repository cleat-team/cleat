package manifests

// The chart's database URL helpers render a plain DSN (cleat#2153).
//
// `cleat.databaseURL` rendered "\npostgres://...": the variable lines above the URL
// ended in `}}` without the trim marker, so the newline after the last of them was
// emitted, and secret.yaml base64-encodes the result into the `database-url`
// secret. `net/url` rejects a leading newline ("invalid control character in
// URL"), so the value is not a DSN a URL-form driver can parse.
//
// Helm's templates are text/template, so the helpers can be executed here without
// helm: the `define` blocks are cut out of _helpers.tpl and run with fake values.
// That is a second reading of the file, not a rendering of the chart. It models
// only the two helpers, and refuses (below) to read one that has grown control
// flow, because a non-greedy cut across an `if` would silently take half of it.
// What it does NOT cover is secret.yaml's use of the result, or the chart as helm
// renders it; the fix was also checked against `helm template`.

import (
	"bytes"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"text/template"
)

const helpersPath = "charts/cleat/templates/_helpers.tpl"

// helperDefine cuts one `define` block out of _helpers.tpl.
func helperDefine(t *testing.T, src, name string) string {
	t.Helper()
	rx := regexp.MustCompile(`(?s)\{\{-?\s*define "` + regexp.QuoteMeta(name) + `"\s*-?\}\}.*?\{\{-?\s*end\s*-?\}\}`)
	all := rx.FindAllString(src, -1)
	if len(all) != 1 {
		t.Fatalf("found %d define blocks for %q in %s, want exactly 1", len(all), name, helpersPath)
	}
	if ctl := regexp.MustCompile(`\{\{-?\s*(if|range|with|else|define)\b`).FindAllString(all[0][2:], -1); len(ctl) > 0 {
		t.Fatalf("helper %q has control flow (%v); this test cuts to the first `end` and would take half of it -- "+
			"read the helper with something that knows the syntax before extending it", name, ctl)
	}
	return all[0]
}

// renderHelper executes a helper with the given .Values.
func renderHelper(t *testing.T, src, name string, values map[string]any) string {
	t.Helper()
	tpl, err := template.New("root").
		Option("missingkey=error").
		Funcs(template.FuncMap{
			// sprig's default: the second argument unless it is empty.
			"default": func(d, v any) any {
				if v == nil || v == "" {
					return d
				}
				return v
			},
		}).
		Parse(helperDefine(t, src, name))
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	var out bytes.Buffer
	if err := tpl.ExecuteTemplate(&out, name, map[string]any{"Values": values}); err != nil {
		t.Fatalf("execute %s: %v", name, err)
	}
	return out.String()
}

// dsnProblems is what "a plain DSN" means here: no whitespace anywhere in it, and
// something net/url accepts.
func dsnProblems(s string) []string {
	var p []string
	if strings.TrimSpace(s) != s {
		p = append(p, "leading or trailing whitespace")
	}
	if strings.ContainsAny(s, " \t\r\n") {
		p = append(p, "contains whitespace")
	}
	if _, err := url.Parse(s); err != nil {
		p = append(p, "net/url rejects it: "+err.Error())
	}
	return p
}

func chartValues(migUser, migPass string) map[string]any {
	return map[string]any{
		"postgres": map[string]any{
			"host": "db.example", "port": 5432, "database": "cleat",
			"username": "cleat", "password": "pw", "sslmode": "require",
		},
		"migration": map[string]any{"username": migUser, "password": migPass},
	}
}

// The predicate has to be able to fail before it is trusted: the bug this test
// exists for, written out, must be reported.
func TestTheDSNPredicateReportsTheLeadingNewline(t *testing.T) {
	if p := dsnProblems("\npostgres://cleat:pw@db.example:5432/cleat?sslmode=require"); len(p) == 0 {
		t.Fatal("dsnProblems accepted a DSN with a leading newline: the checks below cannot fail")
	}
	if p := dsnProblems("postgres://cleat:pw@db.example:5432/cleat?sslmode=require"); len(p) != 0 {
		t.Fatalf("dsnProblems rejected a plain DSN: %v", p)
	}
}

func TestTheChartsDatabaseURLHelpersRenderAPlainDSN(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRoot, helpersPath))
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	const want = "postgres://cleat:pw@db.example:5432/cleat?sslmode=require"

	for _, name := range []string{"cleat.databaseURL", "cleat.migrationDatabaseURL"} {
		got := renderHelper(t, src, name, chartValues("", ""))
		if p := dsnProblems(got); len(p) != 0 {
			t.Errorf("%s renders %q: %v", name, got, p)
		}
		if got != want {
			t.Errorf("%s renders %q, want %q", name, got, want)
		}
	}

	// The migration helper's own job: a DDL-capable role, when one is named.
	got := renderHelper(t, src, "cleat.migrationDatabaseURL", chartValues("ddl", "ddlpw"))
	if want := "postgres://ddl:ddlpw@db.example:5432/cleat?sslmode=require"; got != want {
		t.Errorf("cleat.migrationDatabaseURL with migration.username set renders %q, want %q", got, want)
	}
}
