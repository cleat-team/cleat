package auditlog

// The chain against real databases (cleat#2047): appending, verifying, and each way of
// tampering with it, on all three dialects.
//
// Each dialect gets a SCRATCH DATABASE with the plugin's own migrations applied through
// plugin.RunMigrations, not hand-written DDL: the columns, the unique index and the head
// table under test are the ones that ship. A long-lived shared database would keep the
// shape an earlier run left (CREATE TABLE IF NOT EXISTS never adds a column).

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/lib/pq"
	_ "github.com/microsoft/go-mssqldb"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
	"github.com/google/uuid"
)

type chainDialect struct {
	name    string
	env     string
	driver  string
	dialect plugin.Dialect
	td      testutil.Dialect
}

var chainDialects = []chainDialect{
	{"postgres", "CLEAT_TEST_POSTGRES", "postgres", plugin.DialectPostgres, testutil.DialectPostgres},
	{"mysql", "CLEAT_TEST_MYSQL", "mysql", plugin.DialectMySQL, testutil.DialectMySQL},
	{"mssql", "CLEAT_TEST_MSSQL", "sqlserver", plugin.DialectMSSQL, testutil.DialectMSSQL},
}

type chainEnv struct {
	t       *testing.T
	d       chainDialect
	owner   *sql.DB
	dsn     string // the scratch database, for a second pool
	scratch string
}

// scratchDatabase creates an empty database on the server the dialect's test DSN names
// and returns a DSN for it.
func scratchDatabase(t *testing.T, d chainDialect, admin *sql.DB, adminDSN string) string {
	t.Helper()
	name := "cleat_audit2047_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	var dsn, drop string
	switch d.dialect {
	case plugin.DialectMySQL:
		if _, err := admin.Exec("CREATE DATABASE " + name); err != nil {
			t.Fatalf("create scratch database: %v", err)
		}
		dsn, drop = scratchDSN(adminDSN, name), "DROP DATABASE IF EXISTS "+name
	case plugin.DialectMSSQL:
		if _, err := admin.Exec("CREATE DATABASE [" + name + "]"); err != nil {
			t.Fatalf("create scratch database: %v", err)
		}
		u, err := url.Parse(adminDSN)
		if err != nil {
			t.Fatal(err)
		}
		q := u.Query()
		q.Set("database", name)
		u.RawQuery = q.Encode()
		dsn = u.String()
		drop = fmt.Sprintf("IF DB_ID('%[1]s') IS NOT NULL BEGIN ALTER DATABASE [%[1]s] SET SINGLE_USER WITH ROLLBACK IMMEDIATE; DROP DATABASE [%[1]s] END", name)
	default:
		if _, err := admin.Exec("CREATE DATABASE " + name); err != nil {
			t.Fatalf("create scratch database: %v", err)
		}
		u, err := url.Parse(adminDSN)
		if err != nil {
			t.Fatal(err)
		}
		u.Path = "/" + name
		dsn, drop = u.String(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"
	}
	t.Cleanup(func() {
		if a, err := sql.Open(d.driver, adminDSN); err == nil {
			defer a.Close()
			_, _ = a.Exec(drop)
		}
	})
	return dsn
}

func newChainEnv(t *testing.T, d chainDialect) *chainEnv {
	t.Helper()
	adminDSN := os.Getenv(d.env)
	if adminDSN == "" && d.name == "postgres" {
		// The CI job for ./plugins/... sets only CLEAT_TEST_DB, and the rest of this
		// repository's tests treat it as PostgreSQL's DSN (testutil.TestDB does).
		// Reading only CLEAT_TEST_POSTGRES made every PostgreSQL case here skip in
		// that job, so the suite reported green having measured nothing.
		adminDSN = os.Getenv("CLEAT_TEST_DB")
	}
	if adminDSN == "" {
		t.Skipf("%s not set, skipping %s", d.env, d.name)
	}
	admin, err := sql.Open(d.driver, adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close() })
	if err := admin.Ping(); err != nil {
		t.Fatalf("%s is set but unreachable: %v", d.env, err)
	}
	dsn := scratchDatabase(t, d, admin, adminDSN)
	owner, err := sql.Open(d.driver, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { owner.Close() })
	// The core schema first: the tenant policy the plugin's v2 migration emits calls
	// cleat.assert_tenant_set(), and AllTenantIDs reads admin.tenants.
	testutil.SetupFullSchema(t, owner, d.td)

	p := &Plugin{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := plugin.RunMigrations(context.Background(), owner, d.dialect, nil,
		[]*plugin.LoadedPlugin{{Plugin: p, Healthy: true}}, plugin.WithSchema("public")); err != nil {
		t.Fatalf("the plugin's own migrations: %v", err)
	}
	return &chainEnv{t: t, d: d, owner: owner, dsn: dsn}
}

func forEachChainDialect(t *testing.T, fn func(t *testing.T, e *chainEnv)) {
	t.Helper()
	for _, d := range chainDialects {
		t.Run(d.name, func(t *testing.T) { fn(t, newChainEnv(t, d)) })
	}
}

// plugin returns a Plugin over its own connection pool. A second one is a second worker.
func (e *chainEnv) plugin() *Plugin {
	e.t.Helper()
	db, err := sql.Open(e.d.driver, e.dsn)
	if err != nil {
		e.t.Fatal(err)
	}
	db.SetMaxOpenConns(8)
	e.t.Cleanup(func() { db.Close() })
	return &Plugin{
		db:      &engine.SQLDBAdapter{DB: db, Dialect: e.d.dialect},
		dialect: e.d.dialect,
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		config:  Config{RetentionDays: 90},
	}
}

// admin is a handle that goes through the same tenant-scoped session the plugin uses.
// It has to: SQL Server's security policy filters EVERY connection's reads and writes on
// audit_events by the session's tenant, the owner's included, so a raw query on the pool
// sees no rows and a raw UPDATE changes none -- silently, which is how a tamper test
// passes without tampering.
func (e *chainEnv) admin() plugin.PluginDB {
	return &engine.SQLDBAdapter{DB: e.owner, Dialect: e.d.dialect}
}

// exec runs a statement as tenant and returns the rows it changed. A tamper that changes
// nothing is a failure: it would leave the chain untouched and the test green.
func (e *chainEnv) exec(tenant uuid.UUID, query string, args ...any) int64 {
	e.t.Helper()
	n, err := e.admin().Exec(plugin.ForTenant(context.Background(), tenant), plugin.Rebind(query, e.d.dialect), args...)
	if err != nil {
		e.t.Fatalf("%v\n  %s", err, query)
	}
	return n
}

func (e *chainEnv) mustChange(tenant uuid.UUID, query string, args ...any) {
	e.t.Helper()
	if n := e.exec(tenant, query, args...); n < 1 {
		e.t.Fatalf("the statement changed no rows, so it tampered with nothing:\n  %s", query)
	}
}

func (e *chainEnv) scan(tenant uuid.UUID, query string, args []any, dest ...any) {
	e.t.Helper()
	if err := plugin.ScanRow(e.admin().QueryRow(plugin.ForTenant(context.Background(), tenant),
		plugin.Rebind(query, e.d.dialect), args...), dest...); err != nil {
		e.t.Fatalf("%v\n  %s", err, query)
	}
}

func (e *chainEnv) verify(tenant uuid.UUID) ChainReport {
	e.t.Helper()
	p := e.plugin()
	rep, err := VerifyChain(context.Background(), p.db, e.d.dialect, tenant, VerifyOptions{})
	if err != nil {
		e.t.Fatalf("VerifyChain: %v", err)
	}
	return rep
}

// record appends n events for tenant through the plugin's own entry point, with values
// chosen to be awkward: non-ASCII and non-BMP text, an empty user id, trailing spaces.
func (e *chainEnv) record(p *Plugin, tenant uuid.UUID, n int) {
	e.t.Helper()
	for i := 0; i < n; i++ {
		user := "user-" + fmt.Sprint(i%3)
		if i%4 == 0 {
			user = ""
		}
		p.recordAudit(context.Background(), tenant, user, []string{"GET", "POST", "DELETE"}[i%3],
			fmt.Sprintf("/api/café/%d/\U0001F600/trail ", i), 200+i%5,
			"10.0.0.1", "agent ü中 "+strings.Repeat("x", i%7)+" ", time.Duration(i)*time.Millisecond)
	}
}

// failOnLoggedError makes a swallowed recordAudit error a test failure. recordAudit logs
// and drops its error, as it always did, so a chain that never got written would leave
// "verified, 0 rows" and pass.
func (p *Plugin) captureErrors() *[]string {
	var mu sync.Mutex
	var errs []string
	p.logger = slog.New(&captureHandler{mu: &mu, errs: &errs})
	return &errs
}

type captureHandler struct {
	mu   *sync.Mutex
	errs *[]string
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Level >= slog.LevelError {
		var b strings.Builder
		b.WriteString(r.Message)
		r.Attrs(func(a slog.Attr) bool { b.WriteString(fmt.Sprintf(" %s=%v", a.Key, a.Value)); return true })
		h.mu.Lock()
		*h.errs = append(*h.errs, b.String())
		h.mu.Unlock()
	}
	return nil
}
func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

// ---- the tests ----

func TestAChainWrittenByTheRecorderVerifies(t *testing.T) {
	forEachChainDialect(t, func(t *testing.T, e *chainEnv) {
		p := e.plugin()
		errs := p.captureErrors()
		tenant := uuid.New()
		const n = 25
		e.record(p, tenant, n)
		if len(*errs) > 0 {
			t.Fatalf("recordAudit logged %d errors, the first: %s", len(*errs), (*errs)[0])
		}
		rep := e.verify(tenant)
		if !rep.OK() || rep.Checked != n || rep.HeadSeq != n || rep.FloorSeq != 0 || rep.Unchained != 0 {
			t.Fatalf("a chain nobody touched: %+v (break %+v), want ok, %d checked", rep, rep.Break, n)
		}

		// The rows themselves: contiguous seq from 1, the first linked to the zero hash.
		var first, last, head string
		var seqs int
		e.scan(tenant, `SELECT COUNT(*) FROM audit_events WHERE tenant_id = $1 AND seq BETWEEN 1 AND $2`, []any{tenant.String(), n}, &seqs)
		if seqs != n {
			t.Errorf("%d rows have seq in 1..%d, want %d", seqs, n, n)
		}
		e.scan(tenant, `SELECT prev_hash FROM audit_events WHERE tenant_id = $1 AND seq = 1`, []any{tenant.String()}, &first)
		if strings.TrimSpace(first) != zeroHashHex {
			t.Errorf("the first row's prev_hash is %q, want the zero hash", first)
		}
		e.scan(tenant, `SELECT row_hash FROM audit_events WHERE tenant_id = $1 AND seq = $2`, []any{tenant.String(), n}, &last)
		e.scan(tenant, `SELECT hash FROM audit_chain_heads WHERE tenant_id = $1`, []any{tenant.String()}, &head)
		if strings.TrimSpace(last) == "" || strings.TrimSpace(last) != strings.TrimSpace(head) {
			t.Errorf("the head records %q, the newest row's hash is %q", head, last)
		}

		// Another tenant's chain is separate, and untouched by everything above.
		other := uuid.New()
		e.record(p, other, 3)
		if r := e.verify(other); !r.OK() || r.Checked != 3 || r.HeadSeq != 3 {
			t.Errorf("a second tenant's chain: %+v", r)
		}
	})
}

// Each tamper is applied to a fresh chain and must be reported at the right place with
// the right kind. Every one is falsified by removing the check that reports it.
func TestEveryTamperWithAChainIsReported(t *testing.T) {
	forEachChainDialect(t, func(t *testing.T, e *chainEnv) {
		p := e.plugin()
		errs := p.captureErrors()
		const n = 10

		ts := map[plugin.Dialect]string{
			plugin.DialectPostgres: `timestamp = timestamp + interval '1 microsecond'`,
			plugin.DialectMySQL:    "`timestamp` = `timestamp` + INTERVAL 1 MICROSECOND",
			plugin.DialectMSSQL:    `[timestamp] = DATEADD(MICROSECOND, 1, [timestamp])`,
		}[e.d.dialect]

		cases := []struct {
			name string
			kind string
			seq  int64
		}{
			{"method", BreakEdited, 5},
			{"path", BreakEdited, 5},
			{"status_code", BreakEdited, 5},
			{"status_code to NULL", BreakEdited, 5},
			{"user_id", BreakEdited, 5},
			{"user_id to NULL", BreakEdited, 5},
			{"ip_address", BreakEdited, 5},
			{"user_agent", BreakEdited, 5},
			{"duration_ms", BreakEdited, 5},
			{"metadata", BreakEdited, 5},
			{"timestamp by 1us", BreakEdited, 5},
			{"row_hash", BreakEdited, 5},
			{"prev_hash", BreakRelinked, 6},
			{"delete a middle row", BreakMissing, 5},
			{"delete the first row", BreakMissing, 1},
			{"delete the last row", BreakTruncatedTail, 10},
			{"delete the last three rows", BreakTruncatedTail, 8},
			{"delete the head row", BreakHeadMissing, 0},
			{"move the head back (rows beyond it)", BreakExtraRows, 10},
			{"edit only the head's hash", BreakHeadMismatch, 10},
			{"move the head forward (rows it records are absent)", BreakTruncatedTail, 11},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				tenant := uuid.New()
				T := tenant.String()
				e.record(p, tenant, n)
				if len(*errs) > 0 {
					t.Fatalf("recordAudit logged: %s", (*errs)[0])
				}
				if r := e.verify(tenant); !r.OK() || r.Checked != n {
					t.Fatalf("PRECONDITION: the untouched chain does not verify: %+v %+v", r, r.Break)
				}
				// Placeholders are numbered in the order they APPEAR, because MySQL binds ?
				// by appearance: the SET clause's values come first, then the tenant and seq.
				upd := func(seq int64, set string, args ...any) {
					k := len(args)
					e.mustChange(tenant, fmt.Sprintf(`UPDATE audit_events SET %s WHERE tenant_id = $%d AND seq = $%d`, set, k+1, k+2),
						append(args, T, seq)...)
				}
				del := func(where string) {
					e.mustChange(tenant, `DELETE FROM audit_events WHERE tenant_id = $1 AND `+where, T)
				}
				switch c.name {
				case "method":
					upd(5, `method = $1`, "PUT")
				case "path":
					upd(5, `path = $1`, "/other")
				case "status_code":
					upd(5, `status_code = 500`)
				case "status_code to NULL":
					upd(5, `status_code = NULL`)
				case "user_id":
					upd(5, `user_id = $1`, "mallory")
				case "user_id to NULL":
					upd(5, `user_id = NULL`)
				case "ip_address":
					upd(5, `ip_address = $1`, "6.6.6.6")
				case "user_agent":
					upd(5, `user_agent = $1`, "evil")
				case "duration_ms":
					upd(5, `duration_ms = 999999`)
				case "metadata":
					upd(5, `metadata = $1`, `{"role":"admin"}`)
				case "timestamp by 1us":
					upd(5, ts)
				case "row_hash":
					upd(5, `row_hash = $1`, strings.Repeat("ab", 32))
				case "prev_hash":
					upd(6, `prev_hash = $1`, strings.Repeat("cd", 32))
				case "delete a middle row":
					del(`seq = 5`)
				case "delete the first row":
					del(`seq = 1`)
				case "delete the last row":
					del(`seq = 10`)
				case "delete the last three rows":
					del(`seq >= 8`)
				case "delete the head row":
					e.mustChange(tenant, `DELETE FROM audit_chain_heads WHERE tenant_id = $1`, T)
				case "edit only the head's hash":
					e.mustChange(tenant, `UPDATE audit_chain_heads SET hash = $1 WHERE tenant_id = $2`, strings.Repeat("ef", 32), T)
				case "move the head forward (rows it records are absent)":
					e.mustChange(tenant, `UPDATE audit_chain_heads SET seq = 11 WHERE tenant_id = $1`, T)
				case "move the head back (rows beyond it)":
					var h string
					e.scan(tenant, `SELECT row_hash FROM audit_events WHERE tenant_id = $1 AND seq = 9`, []any{T}, &h)
					e.mustChange(tenant, `UPDATE audit_chain_heads SET seq = 9, hash = $1 WHERE tenant_id = $2`, strings.TrimSpace(h), T)
				}
				rep := e.verify(tenant)
				if rep.OK() {
					t.Fatalf("the tamper %q verified clean: %+v", c.name, rep)
				}
				if rep.Break.Kind != c.kind || (c.seq != 0 && rep.Break.Seq != c.seq) {
					t.Fatalf("%q reported as %s at seq %d (%s), want %s at seq %d",
						c.name, rep.Break.Kind, rep.Break.Seq, rep.Break.Detail, c.kind, c.seq)
				}
				// Every other chain is unaffected.
				other := uuid.New()
				e.record(p, other, 2)
				if r := e.verify(other); !r.OK() {
					t.Errorf("an untouched tenant's chain reports %+v", r.Break)
				}
			})
		}
	})
}

// Two workers appending for one tenant at once must never fork its chain. The head row's
// lock is what serialises them; this goes red if that lock is removed (the falsification
// is recorded in the PR, with the detection rate that chose these numbers).
func TestConcurrentAppendersNeverForkAChain(t *testing.T) {
	const goroutines, perGoroutine, rounds = 8, 6, 3
	forEachChainDialect(t, func(t *testing.T, e *chainEnv) {
		workers := []*Plugin{e.plugin(), e.plugin()} // two workers, two pools
		var errs []*[]string
		for _, p := range workers {
			errs = append(errs, p.captureErrors())
		}
		tenant := uuid.New()
		total := 0
		for round := 0; round < rounds; round++ {
			start := make(chan struct{})
			var wg sync.WaitGroup
			for g := 0; g < goroutines; g++ {
				wg.Add(1)
				go func(g int) {
					defer wg.Done()
					<-start
					e.record(workers[g%len(workers)], tenant, perGoroutine)
				}(g)
			}
			close(start)
			wg.Wait()
			total += goroutines * perGoroutine
		}
		for _, es := range errs {
			if len(*es) > 0 {
				t.Fatalf("an append failed under concurrency (%d in all), the first: %s", len(*es), (*es)[0])
			}
		}
		rep := e.verify(tenant)
		if !rep.OK() || rep.Checked != int64(total) || rep.HeadSeq != int64(total) {
			t.Fatalf("after %d concurrent appends: %+v, break %+v", total, rep, rep.Break)
		}
	})
}

// MySQL reads and writes a TIMESTAMP through the SESSION's time zone. The chain must not:
// a hash that depended on the session zone would call an untouched row edited the day
// someone changed the server's zone. Writers and the verifier here all use different zones.
func TestTheChainDoesNotDependOnTheMySQLSessionTimeZone(t *testing.T) {
	var d chainDialect
	for _, c := range chainDialects {
		if c.dialect == plugin.DialectMySQL {
			d = c
		}
	}
	e := newChainEnv(t, d)
	withZone := func(zone string) *Plugin {
		p := e.plugin()
		db, err := sql.Open("mysql", e.dsn+"&time_zone="+url.QueryEscape("'"+zone+"'"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		p.db = &engine.SQLDBAdapter{DB: db, Dialect: plugin.DialectMySQL}
		return p
	}
	// The column is DATETIME(6): TIMESTAMP(6) overflows in 2038, and DATETIME does no zone
	// conversion, which is what makes the rest of this test hold.
	var colType string
	if err := e.owner.QueryRow(`SELECT DATA_TYPE FROM information_schema.columns
		WHERE table_schema = DATABASE() AND table_name = 'audit_events' AND column_name = 'timestamp'`).Scan(&colType); err != nil || colType != "datetime" {
		t.Fatalf("audit_events.timestamp is %q (err %v) on MySQL, want datetime", colType, err)
	}
	tenant := uuid.New()
	for _, zone := range []string{"-04:00", "+05:30", "America/New_York", "+00:00"} {
		p := withZone(zone)
		errs := p.captureErrors()
		e.record(p, tenant, 3)
		if len(*errs) > 0 {
			t.Fatalf("append in a %s session: %s", zone, (*errs)[0])
		}
	}
	for _, zone := range []string{"-04:00", "+05:30", "America/New_York", "+00:00", "SYSTEM"} {
		rep, err := VerifyChain(context.Background(), withZone(zone).db, plugin.DialectMySQL, tenant, VerifyOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if !rep.OK() || rep.Checked != 12 {
			t.Fatalf("verified from a %s session: %+v, break %+v", zone, rep, rep.Break)
		}
	}
}

// Text that is not valid UTF-8, and NUL bytes, are replaced before the row is hashed and
// stored. PostgreSQL refuses both, so such an event used to be dropped whole; and a hash
// over bytes that are not text cannot be reproduced by a verifier in another language.
func TestTextThatCannotBeStoredIsReplacedNotDropped(t *testing.T) {
	forEachChainDialect(t, func(t *testing.T, e *chainEnv) {
		p := e.plugin()
		errs := p.captureErrors()
		tenant := uuid.New()
		p.recordAudit(context.Background(), tenant, "user\xff", "GET", "/p\x00ath\xc3(", 200,
			"1.2.3.4", "agent\xf0\x28\x8c\xbc and \x00 nul", time.Millisecond)
		if len(*errs) > 0 {
			t.Fatalf("recordAudit logged: %s", (*errs)[0])
		}
		var path, ua string
		e.scan(tenant, `SELECT path, user_agent FROM audit_events WHERE tenant_id = $1 AND seq = 1`, []any{tenant.String()}, &path, &ua)
		if !strings.Contains(path, "�") || strings.ContainsRune(path, 0) || !strings.Contains(ua, "�") {
			t.Fatalf("stored path %q, user agent %q: expected U+FFFD replacements and no NUL", path, ua)
		}
		if rep := e.verify(tenant); !rep.OK() || rep.Checked != 1 {
			t.Fatalf("%+v %+v", rep, rep.Break)
		}
	})
}

// ChainedTenants is what `cleatctl audit verify --all-tenants` walks. A tenant it does
// not list is a tenant nobody verifies, so the list has to include the one whose head
// row is gone -- the very break a verifier exists to report -- and must not include a
// tenant that only has rows from before the chain existed.
func TestChainedTenantsListsEveryTenantThatHasAChain(t *testing.T) {
	forEachChainDialect(t, func(t *testing.T, e *chainEnv) {
		p := e.plugin()
		withChain, headLost, preChain := uuid.New(), uuid.New(), uuid.New()
		e.record(p, withChain, 3)
		e.record(p, headLost, 2)
		e.mustChange(headLost, `DELETE FROM audit_chain_heads WHERE tenant_id = $1`, headLost.String())
		e.mustChange(preChain, `INSERT INTO audit_events (id, tenant_id, method, path, status_code, user_id, ip_address, user_agent, duration_ms)
			VALUES ($1, $2, 'GET', '/old', 200, '', '', '', 1)`, uuid.NewString(), preChain.String())

		got, err := ChainedTenants(context.Background(), p.db, e.d.dialect)
		if err != nil {
			t.Fatalf("ChainedTenants: %v", err)
		}
		want := []uuid.UUID{withChain, headLost}
		sort.Slice(want, func(i, j int) bool { return want[i].String() < want[j].String() })
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("ChainedTenants = %v, want exactly %v (a tenant with a chain, and one whose head is gone; not the tenant with only pre-chain rows %v)", got, want, preChain)
		}
		// The known positive behind the list: the tenant whose head is gone really is
		// reported as broken, so leaving it out of the list would have hidden something.
		if rep := e.verify(headLost); rep.OK() || rep.Break.Kind != BreakHeadMissing {
			t.Errorf("the tenant whose head was deleted verifies as %+v, want %s", rep, BreakHeadMissing)
		}
	})
}
