package engine

// cleat#3540, PR 3a: installing a library bundle.
//
// The property that matters most is the one the readers cannot enforce: an installed
// definition's row must carry EVERY non-bytes column of its member. PR 2's readers take
// only the bytes, the length and the digest from the member; the schemas input
// validation reads, the exposure class, the plugin dependencies and the task queue are
// read from the tenant's row. A column the install left out is NULL (or a default) where
// a value was published, and nothing fails, which is how input validation was once
// skipped without an error.
//
// So the test derives the columns it checks from the database (the ones workflow_defs
// and library.members share), not from a list in the test: a column added to both tables
// fails it by name. And it seeds a value in the member that is NOT the column's default
// and not the stale row's, so "the install copied it" and "the column happens to hold
// that value anyway" cannot be confused.
//
// The rest are the rules the design gives an install: one transaction for the whole
// bundle (a collision on one member writes nothing for any), an upgrade rewrites which
// bundle the tenant holds AND when, reinstalling is idempotent and does not re-enable a
// version the tenant disabled, a tenant's own definition is never shadowed, a member
// that declares `public` is refused for a tenant that has not opted in, and one
// tenant's install does not touch another's.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/cleat-team/cleat/engine/testutil"
)

// installableStore is what the rig hands back: a tenant's store, which must be able to
// install and be read through.
type installableStore interface {
	WorkflowStore
	LibraryInstaller
}

type libInstallRig struct {
	name string
	// store is the tenant's store over a connection the tenant policy applies to.
	store func(tenant string) installableStore
	// exec runs one statement as the operator. tenant scopes it where the dialect needs a
	// tenant in the session (SQL Server); "" is unscoped. ? is the placeholder.
	exec func(t *testing.T, tenant, query string, args ...any)
	// query returns the rows of one statement, every column as text.
	query func(t *testing.T, tenant, query string, args ...any) []map[string]string
	// text casts a column expression to text on this dialect.
	text func(col string) string
	// schema is the schema workflow_defs lives in, for information_schema.
	schema string
	// seedMember publishes one member as the operator would.
	seedMember func(t *testing.T, library string, m libMember)
	// seedStale writes a tenant's row from an EARLIER install of the member's key: same
	// library, every metadata column holding something other than the member's value.
	seedStale func(t *testing.T, tenant, library string, m libMember)
	// cleanup removes everything the test wrote.
	cleanup func(tenants []string, library string)
	// registerPlugins records plugin names in the registry the way a worker's registration
	// does (recordPluginsOn), and forgetPlugin removes one, which only a test may do:
	// nothing in the product deletes from the registry (cleat#3600).
	registerPlugins func(t *testing.T, names ...string)
	forgetPlugin    func(t *testing.T, name string)
}

// libMember is a member with a value in every column that is not the column's default.
type libMember struct {
	name     string
	version  int
	exposure string
	// variant perturbs every value, so two members of one library differ.
	variant int
	// deps, when set, is the member's plugin_deps instead of the default one.
	deps string
}

func (m libMember) wasm() []byte {
	b := make([]byte, 120+m.variant)
	for i := range b {
		b[i] = byte(i*5 + m.version + m.variant)
	}
	return b
}

func (m libMember) digest() string {
	sum := sha256.Sum256(m.wasm())
	return hex.EncodeToString(sum[:])
}

func (m libMember) taskQueue() string { return fmt.Sprintf("lib-queue-%d", 10+m.variant) }
func (m libMember) pluginDeps() string {
	if m.deps != "" {
		return m.deps
	}
	return fmt.Sprintf(`{"slack-notify":"1.%d.0"}`, m.variant)
}
func (m libMember) dagSpec() string {
	return fmt.Sprintf(`{"nodes":["a%d","b%d"]}`, m.variant, m.variant)
}
func (m libMember) entrySchemas() string {
	return fmt.Sprintf(`{"alpha%d":{"type":"object"}}`, m.variant)
}
func (m libMember) abi() int                 { return 2 + m.variant }
func (m libMember) minVersion() int          { return 7 + m.variant }
func (m libMember) maxHistory() int          { return 500 + m.variant }
func (m libMember) validationDisabled() bool { return true }

// staleOf is the same key with a value in every metadata column that differs from m's:
// a different variant for each numeric and text column, the other class for exposure.
// (input_validation_disabled is false, where m's is true.)
func staleOf(m libMember) libMember {
	other := "internal"
	if m.exposure == "internal" {
		other = "auth"
	}
	return libMember{name: m.name, version: m.version, exposure: other, variant: m.variant + 40}
}

func TestAnInstallCopiesEveryColumnOfItsMembers(t *testing.T) {
	t.Run("postgres as a role the policy applies to", func(t *testing.T) {
		adminDB := testutil.TestDB(t, testutil.DialectPostgres)
		t.Cleanup(func() { adminDB.Close() })
		testutil.SetupFullSchema(t, adminDB, testutil.DialectPostgres)
		testutil.CleanupPostgresTestData(t, adminDB)
		appDB := testutil.OpenPostgresRLSTestDB(t, adminDB)
		t.Cleanup(func() { appDB.Close() })
		assertNotSuperuserBypass(t, appDB)
		ph := func(q string) string { // ? -> $1, $2, ...
			var b strings.Builder
			n := 0
			for _, r := range q {
				if r == '?' {
					n++
					fmt.Fprintf(&b, "$%d", n)
				} else {
					b.WriteRune(r)
				}
			}
			return b.String()
		}
		runAnInstallCopiesEveryColumn(t, &libInstallRig{
			name:   "postgres",
			schema: "public",
			text:   func(col string) string { return col + "::text" },
			store: func(tenant string) installableStore {
				return NewPostgresStore(appDB).WithTenant(tenant)
			},
			exec: func(t *testing.T, _ string, q string, args ...any) {
				t.Helper()
				if _, err := adminDB.Exec(ph(q), args...); err != nil {
					t.Fatalf("exec %q: %v", q, err)
				}
			},
			query: func(t *testing.T, _ string, q string, args ...any) []map[string]string {
				t.Helper()
				return scanStringRows(t, adminDB, ph(q), args...)
			},
			seedMember: func(t *testing.T, library string, m libMember) {
				t.Helper()
				if _, err := adminDB.Exec(`
					INSERT INTO library.members (library, name, version, wasm_bytes, wasm_sha256, wasm_length, abi_version, min_version,
						plugin_deps, entry_points, task_queue, max_history_length, dag_spec, entry_point_schemas, exposure, input_validation_disabled)
					VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb, $10::text[], $11, $12, $13::jsonb, $14::jsonb, $15, $16)`,
					library, m.name, m.version, m.wasm(), m.digest(), len(m.wasm()), m.abi(), m.minVersion(),
					m.pluginDeps(), fmt.Sprintf("{alpha%d,beta%d}", m.variant, m.variant), m.taskQueue(), m.maxHistory(),
					m.dagSpec(), m.entrySchemas(), m.exposure, m.validationDisabled()); err != nil {
					t.Fatalf("publish member %s: %v", m.name, err)
				}
			},
			seedStale: func(t *testing.T, tenant, library string, m libMember) {
				t.Helper()
				stale := staleOf(m)
				if _, err := adminDB.Exec(`
					INSERT INTO workflow_defs (tenant_id, name, version, library, abi_version, min_version, plugin_deps, entry_points,
						task_queue, max_history_length, dag_spec, entry_point_schemas, exposure, input_validation_disabled)
					VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb, $8::text[], $9, $10, $11::jsonb, $12::jsonb, $13, $14)`,
					tenant, m.name, m.version, library, stale.abi(), stale.minVersion(), stale.pluginDeps(),
					fmt.Sprintf("{alpha%d,beta%d}", stale.variant, stale.variant), stale.taskQueue(), stale.maxHistory(),
					stale.dagSpec(), stale.entrySchemas(), stale.exposure, false); err != nil {
					t.Fatalf("seed a stale row for %s: %v", tenant, err)
				}
			},
			registerPlugins: func(t *testing.T, names ...string) {
				t.Helper()
				if err := recordPluginsOn(context.Background(), adminDB, DialectPostgres, names); err != nil {
					t.Fatalf("register plugins %v: %v", names, err)
				}
			},
			forgetPlugin: func(t *testing.T, name string) {
				t.Helper()
				if _, err := adminDB.Exec(`DELETE FROM admin.registered_plugins WHERE plugin_name = $1`, name); err != nil {
					t.Fatalf("forget plugin %q: %v", name, err)
				}
			},
			cleanup: func(tenants []string, library string) {
				for _, tenant := range tenants {
					_, _ = adminDB.Exec(`DELETE FROM tenant_library_installs WHERE tenant_id = $1`, tenant)
					_, _ = adminDB.Exec(`DELETE FROM workflow_defs WHERE tenant_id = $1`, tenant)
				}
				_, _ = adminDB.Exec(`DELETE FROM library.bundle_members WHERE library = $1`, library)
				_, _ = adminDB.Exec(`DELETE FROM library.bundles WHERE library = $1`, library)
				_, _ = adminDB.Exec(`DELETE FROM library.members WHERE library = $1`, library)
			},
		})
	})

	t.Run("sqlserver", func(t *testing.T) {
		db := testutil.TestDB(t, testutil.DialectMSSQL)
		t.Cleanup(func() { db.Close() })
		testutil.SetupFullSchema(t, db, testutil.DialectMSSQL)
		ctx := context.Background()
		asTenant := func(t *testing.T, tenant string, fn func(c *sql.Conn)) {
			t.Helper()
			c, err := db.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			if tenant != "" {
				if _, err := c.ExecContext(ctx, `EXEC sp_set_session_context @key = N'tenant_id', @value = @p1`, tenant); err != nil {
					t.Fatalf("scoping the connection to %s: %v", tenant, err)
				}
			}
			fn(c)
		}
		ph := func(q string) string {
			var b strings.Builder
			n := 0
			for _, r := range q {
				if r == '?' {
					n++
					fmt.Fprintf(&b, "@p%d", n)
				} else {
					b.WriteRune(r)
				}
			}
			return b.String()
		}
		runAnInstallCopiesEveryColumn(t, &libInstallRig{
			name:   "sqlserver",
			schema: "dbo",
			text:   func(col string) string { return "CAST(" + col + " AS nvarchar(max))" },
			store: func(tenant string) installableStore {
				return NewMSSQLStore(db).WithTenant(tenant)
			},
			exec: func(t *testing.T, tenant, q string, args ...any) {
				t.Helper()
				asTenant(t, tenant, func(c *sql.Conn) {
					if _, err := c.ExecContext(ctx, ph(q), args...); err != nil {
						t.Fatalf("exec %q: %v", q, err)
					}
				})
			},
			query: func(t *testing.T, tenant, q string, args ...any) []map[string]string {
				t.Helper()
				var out []map[string]string
				asTenant(t, tenant, func(c *sql.Conn) {
					rows, err := c.QueryContext(ctx, ph(q), args...)
					if err != nil {
						t.Fatalf("query %q: %v", q, err)
					}
					defer rows.Close()
					out = collectStringRows(t, rows)
				})
				return out
			},
			seedMember: func(t *testing.T, library string, m libMember) {
				t.Helper()
				validation := 0
				if m.validationDisabled() {
					validation = 1
				}
				if _, err := db.ExecContext(ctx, `
					INSERT INTO library.members (library, name, version, wasm_bytes, wasm_sha256, wasm_length, abi_version, min_version,
						plugin_deps, entry_points, task_queue, max_history_length, dag_spec, entry_point_schemas, exposure, input_validation_disabled)
					VALUES (@p1, @p2, @p3, @p4, @p5, @p6, @p7, @p8, @p9, @p10, @p11, @p12, @p13, @p14, @p15, @p16)`,
					library, m.name, m.version, m.wasm(), m.digest(), len(m.wasm()), m.abi(), m.minVersion(),
					m.pluginDeps(), fmt.Sprintf(`["alpha%d","beta%d"]`, m.variant, m.variant), m.taskQueue(), m.maxHistory(),
					m.dagSpec(), m.entrySchemas(), m.exposure, validation); err != nil {
					t.Fatalf("publish member %s: %v", m.name, err)
				}
			},
			seedStale: func(t *testing.T, tenant, library string, m libMember) {
				t.Helper()
				stale := staleOf(m)
				asTenant(t, tenant, func(c *sql.Conn) {
					if _, err := c.ExecContext(ctx, `
						INSERT INTO workflow_defs (tenant_id, name, version, library, abi_version, min_version, plugin_deps, entry_points,
							task_queue, max_history_length, dag_spec, entry_point_schemas, exposure, input_validation_disabled)
						VALUES (@p1, @p2, @p3, @p4, @p5, @p6, @p7, @p8, @p9, @p10, @p11, @p12, @p13, 0)`,
						tenant, m.name, m.version, library, stale.abi(), stale.minVersion(), stale.pluginDeps(),
						fmt.Sprintf(`["alpha%d","beta%d"]`, stale.variant, stale.variant), stale.taskQueue(), stale.maxHistory(),
						stale.dagSpec(), stale.entrySchemas(), stale.exposure); err != nil {
						t.Fatalf("seed a stale row for %s: %v", tenant, err)
					}
				})
			},
			registerPlugins: func(t *testing.T, names ...string) {
				t.Helper()
				if err := recordPluginsOn(ctx, db, DialectMSSQL, names); err != nil {
					t.Fatalf("register plugins %v: %v", names, err)
				}
			},
			forgetPlugin: func(t *testing.T, name string) {
				t.Helper()
				if _, err := db.ExecContext(ctx, `DELETE FROM admin.registered_plugins WHERE plugin_name = @p1`, name); err != nil {
					t.Fatalf("forget plugin %q: %v", name, err)
				}
			},
			cleanup: func(tenants []string, library string) {
				for _, tenant := range tenants {
					asTenant(t, tenant, func(c *sql.Conn) {
						_, _ = c.ExecContext(ctx, `DELETE FROM tenant_library_installs WHERE tenant_id = @p1`, tenant)
						_, _ = c.ExecContext(ctx, `DELETE FROM workflow_defs WHERE tenant_id = @p1`, tenant)
					})
				}
				_, _ = db.ExecContext(ctx, `DELETE FROM library.bundle_members WHERE library = @p1`, library)
				_, _ = db.ExecContext(ctx, `DELETE FROM library.bundles WHERE library = @p1`, library)
				_, _ = db.ExecContext(ctx, `DELETE FROM library.members WHERE library = @p1`, library)
			},
		})
	})
}

func scanStringRows(t *testing.T, db *sql.DB, q string, args ...any) []map[string]string {
	t.Helper()
	rows, err := db.Query(q, args...)
	if err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	defer rows.Close()
	return collectStringRows(t, rows)
}

func collectStringRows(t *testing.T, rows *sql.Rows) []map[string]string {
	t.Helper()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]string
	for rows.Next() {
		vals := make([]sql.NullString, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		row := map[string]string{}
		for i, c := range cols {
			if vals[i].Valid {
				row[c] = vals[i].String
			} else {
				row[c] = "<NULL>"
			}
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func runAnInstallCopiesEveryColumn(t *testing.T, rig *libInstallRig) {
	t.Helper()
	ctx := context.Background()
	const (
		tenantA = "f2000000-0000-4000-8000-0000000035a0"
		tenantB = "f2000000-0000-4000-8000-0000000035b0"
		tenantC = "f2000000-0000-4000-8000-0000000035c0"
	)
	tenants := []string{tenantA, tenantB, tenantC}

	// Every member this test publishes declares the plugin below by default, and an install
	// is refused when a declared plugin was never registered (cleat#3600), so the way a
	// worker would have registered it is the setup. The refusal itself has its own cases.
	rig.registerPlugins(t, "slack-notify")

	// newLibrary gives each case its own library and clears what a previous run left.
	newLibrary := func(t *testing.T) string {
		t.Helper()
		library := fmt.Sprintf("lib-3540-%d", time.Now().UnixNano())
		rig.cleanup(tenants, library)
		t.Cleanup(func() { rig.cleanup(tenants, library) })
		return library
	}
	bundle := func(t *testing.T, library, bundleVersion string, members ...libMember) {
		t.Helper()
		rig.exec(t, "", `INSERT INTO library.bundles (library, bundle_version, published_by) VALUES (?, ?, 'test')`, library, bundleVersion)
		for _, m := range members {
			rig.exec(t, "", `INSERT INTO library.bundle_members (library, bundle_version, name, version) VALUES (?, ?, ?, ?)`,
				library, bundleVersion, m.name, m.version)
		}
	}
	member := func(name string, version int, exposure string, variant int) libMember {
		return libMember{name: name, version: version, exposure: exposure, variant: variant}
	}
	defRows := func(t *testing.T, tenant string) map[string]map[string]string { // key: "name@version"
		t.Helper()
		rows := rig.query(t, tenant, `SELECT name, version, library, `+rig.text("wasm_bytes")+` AS wasm_text, disabled_at, gc_eligible FROM workflow_defs WHERE tenant_id = ?`, tenant)
		out := map[string]map[string]string{}
		for _, r := range rows {
			out[r["name"]+"@"+r["version"]] = r
		}
		return out
	}
	installRow := func(t *testing.T, tenant, library string) (map[string]string, bool) {
		t.Helper()
		rows := rig.query(t, tenant, `SELECT bundle_version, `+rig.text("installed_at")+` AS installed_at, installed_by FROM tenant_library_installs WHERE tenant_id = ? AND library = ?`, tenant, library)
		if len(rows) == 0 {
			return nil, false
		}
		return rows[0], true
	}
	assertNothingWritten := func(t *testing.T, tenant, library string) {
		t.Helper()
		if _, ok := installRow(t, tenant, library); ok {
			t.Errorf("%s: a refused install left a record of the install", rig.name)
		}
		for key, r := range defRows(t, tenant) {
			if r["library"] != "<NULL>" {
				t.Errorf("%s: a refused install left a library definition %s (library %s)", rig.name, key, r["library"])
			}
		}
	}

	// sharedColumns is the non-key, non-bytes columns workflow_defs and library.members
	// both have, read from the database.
	sharedColumns := func(t *testing.T) []string {
		t.Helper()
		shared := rig.query(t, "", `
			SELECT c.column_name AS column_name FROM information_schema.columns c
			WHERE c.table_schema = '`+rig.schema+`' AND c.table_name = 'workflow_defs'
			  AND c.column_name IN (SELECT column_name FROM information_schema.columns WHERE table_schema = 'library' AND table_name = 'members')`)
		var out []string
		for _, r := range shared {
			switch r["column_name"] {
			case "library", "name", "version", "wasm_bytes":
				continue // the key, the reference and the bytes themselves
			}
			out = append(out, r["column_name"])
		}
		sort.Strings(out)
		return out
	}

	t.Run("it copies every non-bytes column the two tables share", func(t *testing.T) {
		library := newLibrary(t)
		m := member("copy-def", 4, "internal", 3)
		rig.seedMember(t, library, m)
		bundle(t, library, "1.0.0", m)

		// The columns come from the database. What the install copies is written in
		// libraryInstallColumns; the two must agree, and a column on both tables that is
		// not in it is named.
		sharedNonBytes := sharedColumns(t)
		var copied []string
		for _, c := range strings.Split(libraryInstallColumns, ",") {
			copied = append(copied, strings.TrimSpace(c))
		}
		sort.Strings(copied)
		if strings.Join(sharedNonBytes, ",") != strings.Join(copied, ",") {
			t.Fatalf("%s: workflow_defs and library.members share %v, but the install copies %v. A column both tables have is a value the "+
				"member published and the tenant's row would not carry: add it to libraryInstallColumns and to both installs", rig.name, sharedNonBytes, copied)
		}

		// A control: a row with every column at its default shows what "not copied" looks like.
		rig.exec(t, tenantC, `INSERT INTO workflow_defs (name, version, wasm_bytes, tenant_id) VALUES ('copy-control', 1, ?, ?)`, []byte("x"), tenantC)
		control := rig.query(t, tenantC, `SELECT `+selectTexts(rig, sharedNonBytes)+` FROM workflow_defs WHERE tenant_id = ? AND name = 'copy-control'`, tenantC)
		if len(control) != 1 {
			t.Fatalf("%s: control row missing", rig.name)
		}
		memberRow := rig.query(t, "", `SELECT `+selectTexts(rig, sharedNonBytes)+` FROM library.members WHERE library = ? AND name = ? AND version = ?`, library, m.name, m.version)
		if len(memberRow) != 1 {
			t.Fatalf("%s: member row missing", rig.name)
		}
		for _, col := range sharedNonBytes {
			if memberRow[0][col] == control[0][col] {
				t.Fatalf("%s: the member's %s is %q, which is also the column's default. A test that seeds the default cannot tell a copy from "+
					"an omission: give libMember a different value", rig.name, col, memberRow[0][col])
			}
		}

		res, err := rig.store(tenantA).InstallLibraryBundle(ctx, InstallLibraryRequest{Library: library, BundleVersion: "1.0.0", InstalledBy: "tester"})
		if err != nil {
			t.Fatalf("%s: InstallLibraryBundle: %v", rig.name, err)
		}
		if len(res.Members) != 1 || res.Members[0].Name != m.name || res.Members[0].Version != m.version || res.Members[0].Exposure != ExposureInternal {
			t.Errorf("%s: result members = %+v", rig.name, res.Members)
		}

		got := rig.query(t, tenantA, `SELECT `+selectTexts(rig, sharedNonBytes)+`, library, `+rig.text("wasm_bytes")+` AS wasm_text FROM workflow_defs WHERE tenant_id = ? AND name = ? AND version = ?`,
			tenantA, m.name, m.version)
		if len(got) != 1 {
			t.Fatalf("%s: the tenant has %d rows for the installed member, want 1", rig.name, len(got))
		}
		for _, col := range sharedNonBytes {
			if got[0][col] != memberRow[0][col] {
				t.Errorf("%s: column %s is %q on the tenant's row, want the member's %q", rig.name, col, got[0][col], memberRow[0][col])
			}
		}
		if got[0]["library"] != library {
			t.Errorf("%s: the tenant's row names library %q, want %q", rig.name, got[0]["library"], library)
		}
		if got[0]["wasm_text"] != "<NULL>" {
			t.Errorf("%s: an installed definition holds bytes of its own; it must hold none", rig.name)
		}

		// And the reader that PR 2 made follow the reference now has something to follow.
		loaded, err := rig.store(tenantA).LoadWASM(ctx, m.name, m.version)
		if err != nil || string(loaded) != string(m.wasm()) {
			t.Errorf("%s: LoadWASM after the install = %d bytes, %v; want the member's %d", rig.name, len(loaded), err, len(m.wasm()))
		}
	})

	t.Run("it refreshes a stale row of the same library from the member, column for column", func(t *testing.T) {
		library := newLibrary(t)
		m := member("stale-def", 2, "auth", 5)
		rig.seedMember(t, library, m)
		bundle(t, library, "1.0.0", m)
		rig.seedStale(t, tenantA, library, m)

		cols := sharedColumns(t)
		before := rig.query(t, tenantA, `SELECT `+selectTexts(rig, cols)+` FROM workflow_defs WHERE tenant_id = ? AND name = ?`, tenantA, m.name)
		memberRow := rig.query(t, "", `SELECT `+selectTexts(rig, cols)+` FROM library.members WHERE library = ? AND name = ? AND version = ?`, library, m.name, m.version)
		if len(before) != 1 || len(memberRow) != 1 {
			t.Fatalf("%s: stale row or member row missing", rig.name)
		}
		// The stale row must differ from the member in EVERY column, or a refresh that skips
		// one cannot be told from one that does not.
		for _, col := range cols {
			if before[0][col] == memberRow[0][col] {
				t.Fatalf("%s: the stale row already holds the member's %s (%q): seed a different value", rig.name, col, before[0][col])
			}
		}

		if _, err := rig.store(tenantA).InstallLibraryBundle(ctx, InstallLibraryRequest{Library: library, BundleVersion: "1.0.0"}); err != nil {
			t.Fatalf("%s: InstallLibraryBundle over a stale row: %v", rig.name, err)
		}
		after := rig.query(t, tenantA, `SELECT `+selectTexts(rig, cols)+` FROM workflow_defs WHERE tenant_id = ? AND name = ?`, tenantA, m.name)
		if len(after) != 1 {
			t.Fatalf("%s: the tenant has %d rows after the refresh", rig.name, len(after))
		}
		for _, col := range cols {
			if after[0][col] != memberRow[0][col] {
				t.Errorf("%s: column %s is still %q after the refresh, want the member's %q", rig.name, col, after[0][col], memberRow[0][col])
			}
		}
	})

	t.Run("an upgrade rewrites the bundle AND when, and reinstalling changes nothing", func(t *testing.T) {
		library := newLibrary(t)
		m1 := member("up-one", 1, "auth", 1)
		m2v1 := member("up-two", 1, "auth", 2)
		m2v2 := member("up-two", 2, "auth", 4)
		for _, m := range []libMember{m1, m2v1, m2v2} {
			rig.seedMember(t, library, m)
		}
		bundle(t, library, "1.0.0", m1, m2v1)
		bundle(t, library, "2.0.0", m1, m2v2)
		st := rig.store(tenantA)

		first, err := st.InstallLibraryBundle(ctx, InstallLibraryRequest{Library: library, BundleVersion: "1.0.0", InstalledBy: "alice"})
		if err != nil {
			t.Fatalf("%s: first install: %v", rig.name, err)
		}
		if first.PreviousBundleVersion != "" {
			t.Errorf("%s: a first install reports a previous bundle %q", rig.name, first.PreviousBundleVersion)
		}
		// A clock-independent way to see installed_at move: put it far in the past.
		rig.exec(t, tenantA, `UPDATE tenant_library_installs SET installed_at = '2001-01-01T00:00:00Z' WHERE tenant_id = ? AND library = ?`, tenantA, library)
		// A version the tenant disabled stays disabled across a reinstall.
		rig.exec(t, tenantA, `UPDATE workflow_defs SET disabled_at = '2002-02-02T00:00:00Z' WHERE tenant_id = ? AND name = 'up-one'`, tenantA)

		second, err := st.InstallLibraryBundle(ctx, InstallLibraryRequest{Library: library, BundleVersion: "2.0.0", InstalledBy: "bob"})
		if err != nil {
			t.Fatalf("%s: upgrade: %v", rig.name, err)
		}
		if second.PreviousBundleVersion != "1.0.0" {
			t.Errorf("%s: the upgrade reports previous bundle %q, want 1.0.0", rig.name, second.PreviousBundleVersion)
		}
		rec, ok := installRow(t, tenantA, library)
		if !ok {
			t.Fatalf("%s: no install record after the upgrade", rig.name)
		}
		if rec["bundle_version"] != "2.0.0" {
			t.Errorf("%s: bundle_version = %q after an upgrade, want 2.0.0", rig.name, rec["bundle_version"])
		}
		if strings.HasPrefix(rec["installed_at"], "2001-") {
			t.Errorf("%s: installed_at is still %s after an upgrade: it must be rewritten", rig.name, rec["installed_at"])
		}
		if rec["installed_by"] != "bob" {
			t.Errorf("%s: installed_by = %q, want the upgrader", rig.name, rec["installed_by"])
		}
		defs := defRows(t, tenantA)
		for _, key := range []string{"up-one@1", "up-two@1", "up-two@2"} {
			if _, ok := defs[key]; !ok {
				t.Errorf("%s: the tenant has no definition %s after upgrading (a version an earlier bundle installed must stay, since its runs replay it)", rig.name, key)
			}
		}
		if defs["up-one@1"]["disabled_at"] == "<NULL>" {
			t.Errorf("%s: reinstalling re-enabled a version the tenant had disabled", rig.name)
		}
		if defs["up-one@1"]["gc_eligible"] != "false" && defs["up-one@1"]["gc_eligible"] != "0" {
			t.Errorf("%s: an install armed gc_eligible: %q", rig.name, defs["up-one@1"]["gc_eligible"])
		}

		// Installing the bundle the tenant already holds is idempotent.
		again, err := st.InstallLibraryBundle(ctx, InstallLibraryRequest{Library: library, BundleVersion: "2.0.0"})
		if err != nil {
			t.Fatalf("%s: reinstalling the same bundle: %v", rig.name, err)
		}
		if again.PreviousBundleVersion != "2.0.0" || len(again.Members) != 2 {
			t.Errorf("%s: reinstall = %+v", rig.name, again)
		}
		if len(defRows(t, tenantA)) != 3 {
			t.Errorf("%s: reinstalling changed the number of definitions: %d, want 3", rig.name, len(defRows(t, tenantA)))
		}
	})

	t.Run("one transaction: a colliding member writes nothing for any member", func(t *testing.T) {
		library := newLibrary(t)
		good := member("tx-good", 1, "auth", 1)
		clash := member("tx-clash", 1, "auth", 2)
		rig.seedMember(t, library, good)
		rig.seedMember(t, library, clash)
		bundle(t, library, "1.0.0", good, clash)
		// The tenant deployed tx-clash itself. An install does not replace it.
		rig.exec(t, tenantA, `INSERT INTO workflow_defs (name, version, wasm_bytes, abi_version, tenant_id) VALUES (?, ?, ?, 1, ?)`,
			clash.name, clash.version, []byte("the tenant's own code"), tenantA)

		_, err := rig.store(tenantA).InstallLibraryBundle(ctx, InstallLibraryRequest{Library: library, BundleVersion: "1.0.0"})
		var collision *LibraryDefinitionCollisionError
		if !errors.Is(err, ErrLibraryDefinitionCollision) || !errors.As(err, &collision) {
			t.Fatalf("%s: err = %v, want a LibraryDefinitionCollisionError", rig.name, err)
		}
		if collision.Name != clash.name || collision.HeldFromLibrary != "" {
			t.Errorf("%s: collision = %+v, want the tenant's own %s", rig.name, collision, clash.name)
		}
		defs := defRows(t, tenantA)
		if _, ok := defs[good.name+"@1"]; ok {
			t.Errorf("%s: a member was installed although another member of the bundle collided: the install is not one transaction", rig.name)
		}
		if defs[clash.name+"@1"]["library"] != "<NULL>" || defs[clash.name+"@1"]["wasm_text"] == "<NULL>" {
			t.Errorf("%s: the tenant's own definition was changed by a refused install: %v", rig.name, defs[clash.name+"@1"])
		}
		if _, ok := installRow(t, tenantA, library); ok {
			t.Errorf("%s: a refused install recorded itself", rig.name)
		}
	})

	t.Run("a definition held from a different library is not taken over", func(t *testing.T) {
		library := newLibrary(t)
		other := newLibrary(t)
		m := member("other-def", 1, "auth", 1)
		rig.seedMember(t, library, m)
		rig.seedMember(t, other, m)
		bundle(t, library, "1.0.0", m)
		rig.exec(t, tenantA, `INSERT INTO workflow_defs (name, version, library, abi_version, tenant_id) VALUES (?, ?, ?, 1, ?)`, m.name, m.version, other, tenantA)

		_, err := rig.store(tenantA).InstallLibraryBundle(ctx, InstallLibraryRequest{Library: library, BundleVersion: "1.0.0"})
		var collision *LibraryDefinitionCollisionError
		if !errors.As(err, &collision) || collision.HeldFromLibrary != other {
			t.Fatalf("%s: err = %v, want a collision naming library %q", rig.name, err, other)
		}
		if got := defRows(t, tenantA)[m.name+"@1"]["library"]; got != other {
			t.Errorf("%s: the definition now names library %q, want it left with %q", rig.name, got, other)
		}
	})

	t.Run("a bundle that does not exist is a not-found and writes nothing", func(t *testing.T) {
		library := newLibrary(t)
		_, err := rig.store(tenantA).InstallLibraryBundle(ctx, InstallLibraryRequest{Library: library, BundleVersion: "9.9.9"})
		var nf *LibraryBundleNotFoundError
		if !errors.Is(err, ErrLibraryBundleNotFound) || !errors.As(err, &nf) {
			t.Fatalf("%s: err = %v, want a LibraryBundleNotFoundError", rig.name, err)
		}
		assertNothingWritten(t, tenantA, library)
	})

	t.Run("a member that declares public is refused for a tenant that has not opted in", func(t *testing.T) {
		library := newLibrary(t)
		pub := member("pub-def", 1, "public", 1)
		plain := member("pub-plain", 1, "auth", 2)
		rig.seedMember(t, library, pub)
		rig.seedMember(t, library, plain)
		bundle(t, library, "1.0.0", pub, plain)

		_, err := rig.store(tenantA).InstallLibraryBundle(ctx, InstallLibraryRequest{Library: library, BundleVersion: "1.0.0", TenantAllowsPublic: false})
		var notOptedIn *ErrExposurePublicNotOptedIn
		if !errors.As(err, &notOptedIn) {
			t.Fatalf("%s: err = %v, want ErrExposurePublicNotOptedIn", rig.name, err)
		}
		assertNothingWritten(t, tenantA, library)

		if _, err := rig.store(tenantA).InstallLibraryBundle(ctx, InstallLibraryRequest{Library: library, BundleVersion: "1.0.0", TenantAllowsPublic: true}); err != nil {
			t.Fatalf("%s: install for an opted-in tenant: %v", rig.name, err)
		}
		got := rig.query(t, tenantA, `SELECT exposure FROM workflow_defs WHERE tenant_id = ? AND name = ?`, tenantA, pub.name)
		if len(got) != 1 || got[0]["exposure"] != "public" {
			t.Errorf("%s: the installed exposure = %v, want public", rig.name, got)
		}
	})

	t.Run("one tenant's install does not touch another's, and does not collide with its same-named definition", func(t *testing.T) {
		library := newLibrary(t)
		m := member("iso-def", 1, "auth", 1)
		rig.seedMember(t, library, m)
		bundle(t, library, "1.0.0", m)
		// B has its own ordinary definition under the SAME name and version. Under
		// (tenant_id, name, version) that is a different row, so A's install must not see it.
		rig.exec(t, tenantB, `INSERT INTO workflow_defs (name, version, wasm_bytes, abi_version, tenant_id) VALUES (?, ?, ?, 1, ?)`,
			m.name, m.version, []byte("B's own code"), tenantB)

		if _, err := rig.store(tenantA).InstallLibraryBundle(ctx, InstallLibraryRequest{Library: library, BundleVersion: "1.0.0"}); err != nil {
			t.Fatalf("%s: install for A: %v", rig.name, err)
		}
		b := defRows(t, tenantB)
		if len(b) != 1 || b[m.name+"@1"]["library"] != "<NULL>" {
			t.Errorf("%s: tenant B's definitions changed after A's install: %v", rig.name, b)
		}
		if _, ok := installRow(t, tenantB, library); ok {
			t.Errorf("%s: A's install recorded itself for B", rig.name)
		}
		// And B can install the same bundle only when it has no ordinary definition in the way.
		_, err := rig.store(tenantB).InstallLibraryBundle(ctx, InstallLibraryRequest{Library: library, BundleVersion: "1.0.0"})
		if !errors.Is(err, ErrLibraryDefinitionCollision) {
			t.Errorf("%s: B's install over its own same-named definition: err = %v, want a collision", rig.name, err)
		}
	})

	t.Run("a very long subject does not fail the install, and is cut where the column has a width", func(t *testing.T) {
		library := newLibrary(t)
		m := member("by-def", 1, "auth", 1)
		rig.seedMember(t, library, m)
		bundle(t, library, "1.0.0", m)
		// 200 characters outside the Basic Multilingual Plane: 400 UTF-16 units, over
		// SQL Server's nvarchar(255). A cut by rune count leaves 255 runes, which is 510
		// units, and the insert fails with Msg 2628.
		by := strings.Repeat("\U0001F600", 200)
		if _, err := rig.store(tenantA).InstallLibraryBundle(ctx, InstallLibraryRequest{Library: library, BundleVersion: "1.0.0", InstalledBy: by}); err != nil {
			t.Fatalf("%s: a long InstalledBy failed the install: %v", rig.name, err)
		}
		rec, ok := installRow(t, tenantA, library)
		if !ok {
			t.Fatalf("%s: no install record", rig.name)
		}
		if rec["installed_by"] == "" || rec["installed_by"] == "<NULL>" || !strings.HasPrefix(by, rec["installed_by"]) {
			t.Errorf("%s: installed_by is not a non-empty prefix of the subject", rig.name)
		}
		if rig.name == "sqlserver" && len(utf16.Encode([]rune(rec["installed_by"]))) > 255 {
			t.Errorf("%s: installed_by is %d UTF-16 units, over the column's 255", rig.name, len(utf16.Encode([]rune(rec["installed_by"]))))
		}
	})

	t.Run("an install naming plugins no worker has registered is refused, names every one, and writes nothing (cleat#3600)", func(t *testing.T) {
		library := newLibrary(t)
		const pluginA, pluginB = "unregistered-a-3600", "unregistered-b-3600"
		rig.forgetPlugin(t, pluginA)
		rig.forgetPlugin(t, pluginB)
		// Two members, each declaring a plugin nobody registered, and one that is registered.
		m1 := libMember{name: "deps-def-a", version: 1, exposure: "auth", variant: 1, deps: `{"` + pluginA + `":">=1.0.0","slack-notify":"1.0.0"}`}
		m2 := libMember{name: "deps-def-b", version: 1, exposure: "auth", variant: 2, deps: `{"` + pluginB + `":"2.0.0"}`}
		rig.seedMember(t, library, m1)
		rig.seedMember(t, library, m2)
		bundle(t, library, "1.0.0", m1, m2)

		_, err := rig.store(tenantA).InstallLibraryBundle(ctx, InstallLibraryRequest{Library: library, BundleVersion: "1.0.0", InstalledBy: "t"})
		if !errors.Is(err, ErrUnmetPluginDeps) {
			t.Fatalf("%s: install = %v, want an error wrapping ErrUnmetPluginDeps", rig.name, err)
		}
		var unmet *UnmetPluginDepsError
		if !errors.As(err, &unmet) {
			t.Fatalf("%s: %v is not an *UnmetPluginDepsError", rig.name, err)
		}
		var got []string
		for _, m := range unmet.Missing {
			got = append(got, m.Name+"<-"+m.DeclaredBy)
		}
		sort.Strings(got)
		want := []string{
			pluginA + `<-member "deps-def-a" version 1 of library "` + library + `"`,
			pluginB + `<-member "deps-def-b" version 1 of library "` + library + `"`,
		}
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("%s: missing = %v, want exactly %v (the registered plugin must not be listed, and each missing one names its member)", rig.name, got, want)
		}
		assertNothingWritten(t, tenantA, library)

		// The refusal was the cause: once a worker has registered them, the same install lands.
		rig.registerPlugins(t, pluginA, pluginB)
		t.Cleanup(func() { rig.forgetPlugin(t, pluginA); rig.forgetPlugin(t, pluginB) })
		if _, err := rig.store(tenantA).InstallLibraryBundle(ctx, InstallLibraryRequest{Library: library, BundleVersion: "1.0.0", InstalledBy: "t"}); err != nil {
			t.Fatalf("%s: the same install after the plugins were registered failed: %v", rig.name, err)
		}
		if _, ok := installRow(t, tenantA, library); !ok {
			t.Errorf("%s: no install record after the plugins were registered", rig.name)
		}
	})

	t.Run("a member that declares no plugin_deps installs with no registry entry (cleat#3600)", func(t *testing.T) {
		library := newLibrary(t)
		m := libMember{name: "no-deps-def", version: 1, exposure: "auth", variant: 5, deps: `{}`}
		rig.seedMember(t, library, m)
		bundle(t, library, "1.0.0", m)
		if _, err := rig.store(tenantA).InstallLibraryBundle(ctx, InstallLibraryRequest{Library: library, BundleVersion: "1.0.0", InstalledBy: "t"}); err != nil {
			t.Fatalf("%s: an install with nothing to check failed: %v", rig.name, err)
		}
	})

	t.Run("a bad name or bundle version is refused before any statement", func(t *testing.T) {
		for _, bad := range [][2]string{{"", "1.0.0"}, {"a/b", "1.0.0"}, {"lib", ""}, {"lib", "1 0"}, {strings.Repeat("a", 129), "1.0.0"}} {
			if _, err := rig.store(tenantA).InstallLibraryBundle(ctx, InstallLibraryRequest{Library: bad[0], BundleVersion: bad[1]}); err == nil {
				t.Errorf("%s: install of library %q bundle %q succeeded", rig.name, bad[0], bad[1])
			}
		}
	})
}

// selectTexts is `col AS col` for each column, cast to text, joined for a SELECT list.
func selectTexts(rig *libInstallRig, cols []string) string {
	parts := make([]string, len(cols))
	for i, c := range cols {
		parts[i] = rig.text(c) + " AS " + c
	}
	return strings.Join(parts, ", ")
}
