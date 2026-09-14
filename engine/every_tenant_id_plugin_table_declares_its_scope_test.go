package engine_test

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/plugin"
)

// A plugin table with a tenant_id column and no plugin.Migration.TenantScoped
// entry is scoped by the plugin's own WHERE clauses and by nothing else. That
// is the state cleat#1277 found for every plugin table and cleat#1512 finished
// clearing, and the only thing keeping it cleared is this test: TenantScoped is
// a field somebody has to remember to write, on a tree that gains plugins.
//
// Scope, stated because the field's own doc comment is easy to read as wider
// than it is: TenantScoped installs a policy on PostgreSQL and does nothing on
// MySQL or SQL Server (plugin.applyTenantScoping returns early for both). This
// test is about the DECLARATION, which is dialect-independent -- a table whose
// MySQL arm carries tenant_id must be declared too, so that the PostgreSQL arm
// of the same table gets its policy and so that a future SQL Server
// implementation has the set already written down.
//
// Cf. plugin/no_two_plugins_declare_the_same_table_test.go, which guards the
// other half of the same field: that the names are unique across plugins.
func TestEveryPluginTableWithATenantIDDeclaresItsTenantScope(t *testing.T) {
	plugins, err := plugin.Discover()
	if err != nil {
		t.Fatalf("plugin.Discover: %v", err)
	}

	// Registered-vs-present, so that a plugin nobody imported into this test
	// binary cannot pass by being invisible. The blank imports live in
	// plugin_migrations_test.go; a new plugin added without one would
	// otherwise never be examined, and an unexamined plugin reads exactly
	// like a compliant one.
	assertEveryPluginPackageIsRegistered(t, plugins)

	for _, lp := range plugins {
		hm, ok := lp.Plugin.(plugin.HasMigrations)
		if !ok {
			continue
		}
		name := lp.Plugin.Info().Name

		declared := map[string]bool{}
		for _, m := range hm.Migrations() {
			for _, table := range m.TenantScoped {
				declared[strings.ToLower(table)] = true
			}
		}

		for _, table := range tablesDeclaringATenantID(hm.Migrations()) {
			if declared[table] {
				continue
			}
			t.Errorf("plugin %q: table %q has a tenant_id column but no migration "+
				"declares it in TenantScoped, so nothing installs a row-level "+
				"security policy on it and its isolation rests entirely on the "+
				"plugin's own WHERE clauses (cleat#1277, cleat#1512).\n"+
				"  Add it to the TenantScoped list of the migration that creates it:\n"+
				"      TenantScoped: []string{%q},\n"+
				"  If the table genuinely must not carry a policy -- every reader has\n"+
				"  a tenant is the requirement, so a cross-tenant background sweep\n"+
				"  disqualifies it -- say so at the declaration site and here, rather\n"+
				"  than leaving the omission to read as an oversight.",
				name, table, table)
		}
	}
}

// tablesDeclaringATenantID returns, lowercased and deduplicated, the name of
// every table any dialect arm of these migrations creates with a tenant_id
// COLUMN.
//
// "Column", not "mention": the check is over the parenthesised column list of
// each CREATE TABLE, matched by counting parens rather than by scanning to the
// next statement. A scan to the next CREATE TABLE attributes a following
// `CREATE INDEX ... ON other_table(tenant_id)` to the wrong table, which is how
// the first version of this read plugins/notifications: webhook_delivery has no
// tenant_id column and cannot have one (see the comment at its declaration),
// and the index on webhook_config that follows it made it look like it did.
func tablesDeclaringATenantID(ms []plugin.Migration) []string {
	seen := map[string]bool{}
	for _, m := range ms {
		for _, arm := range []string{m.Up, m.UpMySQL, m.UpMSSQL} {
			for _, ct := range createTableStatements(arm) {
				if tenantIDColumn.MatchString(ct.columns) {
					seen[ct.table] = true
				}
			}
		}
	}
	out := make([]string, 0, len(seen))
	for t := range seen {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

var (
	createTableHead = regexp.MustCompile(`(?is)CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?["\x60\[]?([A-Za-z_][\w.]*)["\x60\]]?\s*\(`)

	// Anchored to a column definition: start of the list or just after a
	// comma, so `UNIQUE (tenant_id, ...)` and `PRIMARY KEY (tenant_id)` --
	// which are constraints on a column declared elsewhere, possibly in
	// another table -- do not on their own make a table look scoped.
	tenantIDColumn = regexp.MustCompile(`(?is)(?:^\(|,)\s*["\x60\[]?tenant_id["\x60\]]?\s+\w`)
)

type createTable struct{ table, columns string }

// createTableStatements returns each CREATE TABLE in sql paired with its
// parenthesised column list, the parens counted so the list ends where the
// statement's own closing paren is rather than at the first one.
func createTableStatements(sql string) []createTable {
	var out []createTable
	for _, loc := range createTableHead.FindAllStringSubmatchIndex(sql, -1) {
		name := sql[loc[2]:loc[3]]
		if i := strings.LastIndex(name, "."); i >= 0 {
			name = name[i+1:]
		}
		open := loc[1] - 1 // the "(" the head pattern ends on
		depth, end := 0, -1
		for i := open; i < len(sql); i++ {
			switch sql[i] {
			case '(':
				depth++
			case ')':
				depth--
				if depth == 0 {
					end = i + 1
				}
			}
			if end >= 0 {
				break
			}
		}
		if end < 0 {
			end = len(sql) // unbalanced; take the rest and let the column check decide
		}
		out = append(out, createTable{
			table:   strings.ToLower(name),
			columns: sql[open:end],
		})
	}
	return out
}

// assertEveryPluginPackageIsRegistered fails if a package under plugins/ both
// creates a table and registers a plugin that did not come back from Discover
// -- that is, somebody added a plugin with tables and did not add its blank
// import to this test binary.
//
// "And creates a table" is the narrowing, and it is deliberate rather than
// convenient. Requiring EVERY plugins/ package to be visible here is the
// stronger invariant and was the first version; it costs a blank import of
// plugins/email, which pulls sendgrid into this package's import graph and out
// through the replace directives into tests/plugin-harness/go.sum. Paying a
// cross-module dependency for three plugins that own no tables buys nothing --
// a plugin with no CREATE TABLE has nothing for this test to examine, and the
// day one gains a table is the day this check starts demanding its import.
//
// The name is read out of the source rather than inferred from the directory,
// because the two differ and not rarely: plugins/email registers "email-notify".
// A first version of this mapped directory to name by stripping hyphens, which
// reported plugins/email as unregistered when it was imported and working.
//
// A package whose Register call is found but whose Name cannot be read fails
// too, rather than passing quietly -- that combination means this check could
// not answer the question, which is not the same as the answer being yes.
//
// It reads a directory rather than a compiled-in list on purpose: a list is a
// census of a growing population and would have to be edited by exactly the
// person who forgot the import. Note the cache consequence, since it is not
// obvious: `go test` keys its cache on files opened inside the package
// directory, so this listing does not invalidate a cached engine_test result
// locally. CI passes -count=1 to every package, so it is checked there on
// every run.
func assertEveryPluginPackageIsRegistered(t *testing.T, loaded []*plugin.LoadedPlugin) {
	t.Helper()

	entries, err := os.ReadDir(filepath.Join("..", "plugins"))
	if err != nil {
		t.Fatalf("read plugins dir: %v", err)
	}

	registered := map[string]bool{}
	for _, lp := range loaded {
		registered[lp.Plugin.Info().Name] = true
	}

	var missing, unreadable []string
	examined := 0
	for _, e := range entries {
		if !e.IsDir() || e.Name() == "plugintest" {
			continue
		}
		dir := filepath.Join("..", "plugins", e.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		// Two passes over the package, because the CREATE TABLE and the
		// plugin.Register call need not be in the same file.
		var sources []string
		createsATable := false
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".go") || strings.HasSuffix(f.Name(), "_test.go") {
				continue
			}
			b, err := os.ReadFile(filepath.Join(dir, f.Name()))
			if err != nil {
				t.Fatalf("read %s: %v", f.Name(), err)
			}
			src := string(b)
			sources = append(sources, src)
			if createTableHead.MatchString(src) {
				createsATable = true
			}
		}
		if !createsATable {
			continue
		}
		for _, src := range sources {
			i := strings.Index(src, "plugin.Register(")
			if i < 0 {
				continue
			}
			examined++
			m := registerName.FindStringSubmatch(src[i:])
			if m == nil {
				unreadable = append(unreadable, e.Name())
				break
			}
			if !registered[m[1]] {
				missing = append(missing, e.Name()+" (registers "+m[1]+")")
			}
			break
		}
	}

	// A floor, not a count: zero examined means the directory walk found no
	// Register call at all, which is what a broken path or a changed call
	// spelling looks like -- and which would otherwise report success.
	if examined == 0 {
		t.Errorf("found no plugins/ package that both creates a table and calls " +
			"plugin.Register, so this check examined nothing. The walk or one of " +
			"the two patterns is wrong, not the tree.")
	}
	if len(unreadable) > 0 {
		sort.Strings(unreadable)
		t.Errorf("these plugins/ packages call plugin.Register but no Name literal "+
			"could be read from the call, so this check could not tell whether they "+
			"are registered: %s",
			strings.Join(unreadable, ", "))
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("these plugins/ packages register a plugin that was not discovered, "+
			"so this test never examined them: %s\n"+
			"  Add a blank import to engine/plugin_migrations_test.go.",
			strings.Join(missing, ", "))
	}
}

// registerName pulls the Name field out of a plugin.Register(plugin.PluginInfo{...})
// call. Anchored on the field rather than on the first string in the call, so
// that a Description or Author appearing first cannot be read as the name.
var registerName = regexp.MustCompile(`(?s)\bName:\s*"([^"]+)"`)

// TestTablesDeclaringATenantIDReadsColumnsNotMentions pins the two readings
// that separate this parser from a grep, both of which it got wrong first.
//
// It is a unit test of the pure function rather than another pass over the real
// plugins, deliberately: the real tree is one sample and happens to contain the
// shapes below, but nothing keeps it containing them, and a parser property
// that holds only while some plugin happens to exercise it is not pinned.
func TestTablesDeclaringATenantIDReadsColumnsNotMentions(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want []string
	}{
		{
			name: "an index on another table does not scope this one",
			// plugins/notifications: webhook_delivery has no tenant_id and
			// cannot have one, and the index on webhook_config follows it.
			// A scan from CREATE TABLE to the next CREATE TABLE reads the
			// index into webhook_delivery and reports it as scoped.
			sql: `
				CREATE TABLE IF NOT EXISTS webhook_config (
					tenant_id UUID NOT NULL,
					url       TEXT NOT NULL
				);
				CREATE TABLE IF NOT EXISTS webhook_delivery (
					id      UUID PRIMARY KEY,
					attempt INTEGER NOT NULL
				);
				CREATE INDEX idx_webhook_config_tenant ON webhook_config(tenant_id);
			`,
			want: []string{"webhook_config"},
		},
		{
			name: "a constraint naming tenant_id is not a column declaring it",
			sql: `
				CREATE TABLE child (
					id            UUID PRIMARY KEY,
					collection_id UUID NOT NULL,
					UNIQUE (tenant_id, collection_id)
				);
			`,
			want: nil,
		},
		{
			name: "a nested paren in a column type does not end the column list",
			sql: `
				CREATE TABLE a (
					id        UUID PRIMARY KEY,
					embedding vector(1536),
					tenant_id UUID NOT NULL
				);
				CREATE TABLE b (
					id UUID PRIMARY KEY
				);
			`,
			want: []string{"a"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tablesDeclaringATenantID([]plugin.Migration{{Up: tc.sql}})
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}
