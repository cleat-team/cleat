package testutil

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
)

// Foreign sessions: who ELSE is connected to this test database.
//
// cleat#982 is four engine-suite failures, all of the shape "an operation that
// needed a workflow row found none", none reproducible in isolation. Five
// mechanisms were proposed and eliminated; two eliminations were later
// withdrawn. The one candidate that has been measured as SUFFICIENT to produce
// the whole family is another process sharing the database -- every
// registeredBackends test calls Setup, which calls a cleanup helper, which is
// an unqualified DELETE FROM across every table. Two processes on one database
// delete each other's fixtures mid-test, and the result reads as an unrelated
// flake in whichever test was unlucky.
//
// So the question that separates "sufficient" from "actually what happened" is
// simply: WAS ANYTHING ELSE CONNECTED? Nobody could answer it, because by the
// time a failure is read the connections are gone. Two sessions on the issue
// independently named capturing it as the next step.
//
// This answers it at the only moment it is both cheap and decisive: when a test
// has already failed. See ReportForeignSessionsOnFailure.
//
// WHY NOT pgrep, which is what the issue proposed. pgrep sees processes on this
// machine, not connections to this database, and the difference already cost
// this investigation a candidate: a concurrent `go test` was observed, assumed
// relevant, and then eliminated because it was pointed at different ports. A
// process list cannot make that distinction and this can -- it asks the server
// who is attached to the database in front of it.

// atStart remembers who was attached the FIRST time this process asked, once
// per dialect, and is reported alongside the at-failure sample.
//
// Two samples rather than one because the probe is a SAMPLER and the damage is
// not: the destructive DELETE runs in another process's setup and teardown, so
// that process can attach, wipe this one's fixtures, and disconnect before the
// failure it caused is ever observed. A single sample taken at failure time
// reports "nobody" for that, which is a false all-clear.
//
// Two samples do not close the gap -- a process that arrives and leaves between
// them is still invisible -- at a cost of one query per dialect per process.
//
// THE PARAGRAPH ABOVE USED TO CLAIM they "cover the common shape, a peer's
// suite running throughout." That was wrong, and it was wrong in the direction
// that makes the probe useless. A peer reproduced cleat#982's signature with a
// known cause -- two short `go test -run` invocations against one database --
// and THIS PROBE FIRED ELEVEN TIMES AND REPORTED "none", at both sample points.
//
// It could not have said anything else. The two sample instants are exactly the
// ones a short-lived interloper falls between: at the first it has not started,
// because a human starts it later reacting to something; by the second it has
// exited. So the sampler is most confident precisely for the SHORTEST-lived
// offenders -- and short-lived is the likely kind, because nobody starts a
// second full suite by accident, but a fifteen-second sanity check does not feel
// like "running the suite."
//
// Hence the gate below. The fix is asymmetry: the incumbent cannot see a process
// that has not started, but a NEWCOMER can always see the incumbent, because a
// suite holds its connections for its whole run. The sampling call already runs
// at the newcomer's first TestDB, so the observation was being taken at the
// right moment and merely not acted on. The two-sample report stays as the
// fallback for the one case the gate cannot cover -- an interloper that attaches
// AFTER this process starts.
var (
	atStartOnce    sync.Map // Dialect -> *sync.Once
	atStartCount   sync.Map // Dialect -> string
	atStartForeign sync.Map // Dialect -> []string
)

// AllowForeignSessionsEnv names the hazard being accepted, not the check being
// skipped: someone reading a CI file or a shell history should see WHAT was
// waived. A name like SKIP_CHECK records only that somebody got past it.
const AllowForeignSessionsEnv = "CLEAT_TEST_ALLOW_FOREIGN_SESSIONS"

// SampleForeignSessionsAtStart records, once per dialect per process, who else
// was attached before this process did any work.
func SampleForeignSessionsAtStart(dialect Dialect) {
	o, _ := atStartOnce.LoadOrStore(dialect, &sync.Once{})
	o.(*sync.Once).Do(func() {
		foreign, basis, ok := ForeignSessions(dialect)
		switch {
		case !ok:
			atStartCount.Store(dialect, "could not tell ("+basis+")")
		case len(foreign) == 0:
			atStartCount.Store(dialect, "none")
		default:
			atStartCount.Store(dialect, fmt.Sprintf("%d session(s): %s",
				len(foreign), strings.Join(foreign, "; ")))
			atStartForeign.Store(dialect, foreign)
		}
	})
}

// ForeignSessionsAtStart reports what SampleForeignSessionsAtStart saw.
func ForeignSessionsAtStart(dialect Dialect) string {
	if v, ok := atStartCount.Load(dialect); ok {
		return v.(string)
	}
	return "not sampled"
}

// selfPID reports this process's id. It is a variable so the tests can claim to
// be a DIFFERENT process and check that this process's own connections are then
// reported as foreign -- the positive control. Without one, "no foreign
// sessions" is what a broken query returns too.
var selfPID = os.Getpid

// selfPPID reports this process's parent. A variable for the same reason
// selfPID is: a test has to be able to claim a different lineage.
var selfPPID = os.Getppid

// isSiblingTestProcess reports whether pid belongs to another package binary of
// the SAME `go test` invocation.
//
// WHY THIS IS NOT AN EXEMPTION HOLE. `go test ./plugins/...` runs each package's
// test binary as a SEPARATE OS PROCESS, up to -p of them at once (default
// NumCPU). Every sibling therefore looks exactly like a foreign client to the
// gate, and whichever starts second is refused -- so `go test ./plugins/...`
// became flaky the moment cleat#1469 landed, with 2 failures in 3 runs on a
// clean develop. `-p 1` passes every time, which is the discriminator:
// package parallelism, not the tests. Found by WS-3, who ran develop three
// times rather than assume it was their branch.
//
// THE ASYMMETRY THAT JUSTIFIED THE NEWCOMER CHECK DOES NOT REACH HERE. "A suite
// holds its connections for the whole run, so the newcomer can always see the
// incumbent" is true of two people sharing a database. It is not true of one
// invocation spanning several packages, where the incumbent is a sibling doing
// exactly what it should.
//
// Siblings share a parent -- the `go` driver -- and have distinct pids;
// measured directly rather than assumed. A second `go test` gets its own driver
// and therefore a different parent, so the case the gate exists for still
// fires.
//
// Fails CLOSED: if the pid cannot be resolved -- a client on another host, a
// process that has already exited -- this returns false and the session counts
// as foreign. An unresolvable pid is the one we know least about, and this gate
// refuses rather than warns.
func isSiblingTestProcess(pid int) bool {
	if pid <= 0 {
		return false
	}
	if pid == selfPID() {
		return true
	}
	//nolint:gosec // G204: fixed binary ("ps"), arguments as an array, no shell. The
	// only variable is strconv.Itoa of an int, so no string reaches the command
	// line and there is nothing to inject. Same shape as credentials.go's vault
	// and aws providers, with a narrower input than either.
	out, err := exec.Command("ps", "-o", "ppid=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return false
	}
	ppid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return false
	}
	// Guard against a reparented orphan: init (1) is every orphan's parent, so
	// matching on it would exempt anything whose real parent has died.
	if ppid <= 1 {
		return false
	}
	return ppid == selfPPID()
}

// pidFromTag recovers the pid from a cleat-test-<pid> application_name, or 0.
func pidFromTag(tag string) int {
	const prefix = "cleat-test-"
	if !strings.HasPrefix(tag, prefix) {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimPrefix(tag, prefix))
	if err != nil {
		return 0
	}
	return n
}

// testProcessTag identifies THIS process to the database server. Two `go test`
// invocations on one machine have different OS pids, which is exactly the
// distinction cleat#982 needs.
func testProcessTag() string {
	return fmt.Sprintf("cleat-test-%d", selfPID())
}

// tagPostgresDSN adds application_name to a PostgreSQL DSN.
//
// PostgreSQL is the one dialect of the three that does NOT hand the client's
// process id to the server. MySQL sends _pid in session_connect_attrs and SQL
// Server exposes host_process_id, both without being asked; pg_stat_activity
// has client_addr and client_port, which identify a CONNECTION rather than a
// process, so a pooled caller looks like several strangers. application_name is
// the one field we can make carry the answer, and it has to go in the DSN
// because database/sql pools connections -- a SET on one of them says nothing
// about the next.
//
// Returns the DSN unchanged if it is not a URL. That is not a silent failure:
// ForeignSessions re-reads the tag from the server and says so when it is
// missing, rather than reporting our own connections as strangers.
func tagPostgresDSN(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		return dsn
	}
	q := u.Query()
	if q.Get("application_name") != "" {
		return dsn
	}
	q.Set("application_name", testProcessTag())
	u.RawQuery = q.Encode()
	return u.String()
}

// ForeignSessions returns one line per session on this database that belongs to
// some other client process.
//
// It OPENS ITS OWN CONNECTION rather than borrowing the caller's, and that is
// not tidiness. 53 of the 76 TestDB call sites in engine/ write
// `defer db.Close()`, and a test function's defers run BEFORE the t.Cleanup
// callbacks -- so a probe holding the caller's pool is dead in roughly seventy
// percent of the failures it exists to explain. Measured 2026-09-13: the query
// returned "sql: database is closed" and the report printed "no other client
// process was attached", which is the reassuring answer and was not a
// measurement of anything.
//
// ok is the fix for the other half of that. It is false when the question could
// not be ANSWERED, which is a different thing from answering "nobody" -- and
// they are indistinguishable in an empty slice. Callers must not render a
// not-ok result as an all-clear.
func ForeignSessions(dialect Dialect) (foreign []string, basis string, ok bool) {
	db, err := openProbeConnection(dialect)
	if err != nil {
		return nil, fmt.Sprintf("could not open a connection to ask (%v)", err), false
	}
	defer db.Close()

	tag := testProcessTag()

	switch dialect {
	case DialectPostgres:
		// Confirm the tag actually reached the server before trusting it to
		// tell our sessions from anyone else's.
		var ourApp string
		if err := db.QueryRow(
			`SELECT application_name FROM pg_stat_activity WHERE pid = pg_backend_pid()`,
		).Scan(&ourApp); err != nil {
			return nil, fmt.Sprintf("could not read pg_stat_activity (%v)", err), false
		}
		if ourApp != tag {
			var total int
			_ = db.QueryRow(
				`SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND pid <> pg_backend_pid()`,
			).Scan(&total)
			return nil, fmt.Sprintf(
				"cannot tell our own sessions apart: application_name is %q, expected %q, "+
					"so the DSN was not tagged (not a URL?). %d other session(s) exist on this "+
					"database and this cannot say whose they are",
				ourApp, tag, total), false
		}
		rows, err := db.Query(
			`SELECT coalesce(application_name,''), coalesce(host(client_addr),''), coalesce(state,''), coalesce(left(query, 120),'')
			   FROM pg_stat_activity
			  WHERE datname = current_database()
			    AND pid <> pg_backend_pid()
			    AND coalesce(application_name,'') <> $1`, tag)
		if err != nil {
			return nil, fmt.Sprintf("could not list pg_stat_activity (%v)", err), false
		}
		defer rows.Close()
		for rows.Next() {
			var app, addr, state, query string
			if err := rows.Scan(&app, &addr, &state, &query); err != nil {
				continue
			}
			// A sibling package binary of this same `go test` is not a
			// stranger. Its tag carries its pid; see isSiblingTestProcess.
			if isSiblingTestProcess(pidFromTag(app)) {
				continue
			}
			foreign = append(foreign, fmt.Sprintf("application_name=%q from %s [%s] %s",
				app, orUnknown(addr), state, oneLine(query)))
		}
		return foreign, fmt.Sprintf("pg_stat_activity, ours identified by application_name=%q", tag), true

	case DialectMySQL:
		// _pid is sent by go-sql-driver/mysql without being asked, and is the
		// client's real OS pid -- verified against os.Getpid() 2026-09-13.
		rows, err := db.Query(
			`SELECT DISTINCT p.ID, coalesce(p.HOST,''), coalesce(p.COMMAND,''), coalesce(a.ATTR_VALUE,'')
			   FROM information_schema.PROCESSLIST p
			   LEFT JOIN performance_schema.session_connect_attrs a
			     ON a.PROCESSLIST_ID = p.ID AND a.ATTR_NAME = '_pid'
			  WHERE p.DB = DATABASE()
			    AND p.ID <> CONNECTION_ID()
			    AND coalesce(a.ATTR_VALUE,'') <> ?`, fmt.Sprint(selfPID()))
		if err != nil {
			return nil, fmt.Sprintf("could not join PROCESSLIST to session_connect_attrs (%v); "+
				"performance_schema may be off, in which case this dialect cannot answer", err), false
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			var host, cmd, cpid string
			if err := rows.Scan(&id, &host, &cmd, &cpid); err != nil {
				continue
			}
			if n, err := strconv.Atoi(strings.TrimSpace(cpid)); err == nil && isSiblingTestProcess(n) {
				continue
			}
			foreign = append(foreign, fmt.Sprintf("connection %d from %s, client pid %s [%s]",
				id, orUnknown(host), orUnknown(cpid), cmd))
		}
		return foreign, fmt.Sprintf("information_schema.PROCESSLIST joined to session_connect_attrs._pid, ours is pid %d", selfPID()), true

	case DialectMSSQL:
		// host_process_id is the client's OS pid, reported by the server with
		// no cooperation from the client at all. This is the strongest of the
		// three, and SQL Server is where three of cleat#982's four failures
		// were seen.
		// is_user_process = 1 is NOT enough on its own. SQL Server's own
		// telemetry session reports as a user process -- measured 2026-09-13 on
		// mssql/server:2022-latest:
		//
		//   session 52  program_name=SQLServerCEIP  host_process_id=408
		//               login_name=NT AUTHORITY\SYSTEM
		//
		// It is present on every instance, permanently, so without this filter
		// the probe reports one stranger on every SQL Server failure forever --
		// a signal that is always on is not a signal.
		//
		// The filter is on the LOGIN, not on program_name: 'SQLServerCEIP' is a
		// feature name that can change and says nothing about what the session
		// is, whereas an NT AUTHORITY principal is the server's own service
		// account and never a test client, which authenticates as sa or a SQL
		// login. The cost is that a client genuinely running under a Windows
		// service account would be missed; that direction loses information,
		// and the other direction makes the probe useless.
		rows, err := db.Query(
			`SELECT session_id, coalesce(host_name,''), coalesce(host_process_id,0), coalesce(program_name,''), coalesce(status,'')
			   FROM sys.dm_exec_sessions
			  WHERE database_id = DB_ID()
			    AND session_id <> @@SPID
			    AND is_user_process = 1
			    AND coalesce(login_name,'') NOT LIKE 'NT AUTHORITY\%'
			    AND coalesce(host_process_id,0) <> @p1`, selfPID())
		if err != nil {
			return nil, fmt.Sprintf("could not read sys.dm_exec_sessions (%v)", err), false
		}
		defer rows.Close()
		for rows.Next() {
			var sid, hpid int64
			var host, prog, status string
			if err := rows.Scan(&sid, &host, &hpid, &prog, &status); err != nil {
				continue
			}
			if isSiblingTestProcess(int(hpid)) {
				continue
			}
			foreign = append(foreign, fmt.Sprintf("session %d from %s, client pid %d, program %q [%s]",
				sid, orUnknown(host), hpid, prog, status))
		}
		return foreign, fmt.Sprintf("sys.dm_exec_sessions, ours identified by host_process_id=%d", selfPID()), true
	}
	return nil, fmt.Sprintf("dialect %s has no foreign-session query", dialect), false
}

// openProbeConnection opens a connection for the probe's own use, resolved the
// same way TestDB resolves one. The PostgreSQL DSN carries the same
// application_name tag, so the probe still recognises this process's sessions
// as its own.
func openProbeConnection(dialect Dialect) (*sql.DB, error) {
	var driver, dsn string
	switch dialect {
	case DialectPostgres:
		driver, dsn = "postgres", tagPostgresDSN(PostgresTestDSN())
	case DialectMySQL:
		driver, dsn = "mysql", os.Getenv("CLEAT_TEST_MYSQL")
	case DialectMSSQL:
		driver, dsn = "sqlserver", os.Getenv("CLEAT_TEST_MSSQL")
	default:
		return nil, fmt.Errorf("unknown dialect %s", dialect)
	}
	if dsn == "" {
		return nil, fmt.Errorf("no DSN configured for %s", dialect)
	}
	db, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func orUnknown(s string) string {
	if s == "" {
		return "<unknown>"
	}
	return s
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// RefuseIfForeignSessionsAtStart stops this process when another client was
// already attached to the test database at its first TestDB call.
//
// THE ASYMMETRY IS THE WHOLE POINT. An incumbent suite cannot see a process that
// has not started yet, so asking it to detect interference is asking for the one
// observation it cannot make. A newcomer can always see an incumbent, because a
// suite holds its connections for its entire run. Checking here converts
// "confusing failures minutes later in the wrong process" into a refusal at the
// moment of the mistake.
//
// WHY REFUSE RATHER THAN WARN. A warning printed into the scrollback of a run
// that then passes is read by nobody; this repo already settled that argument
// when it deleted its "Warn on skipped tests" steps, on the grounds that a
// warning on a green job is not a signal. And the case a warning would be
// gentler on -- "I only ran a quick sanity check while the suite was going" --
// is precisely the case that corrupts the incumbent. There is no safe version of
// it, so there is nothing for a warning to permit.
//
// The interloper almost never needs THIS database specifically; any database
// would do. That is why redirecting it is nearly free and why refusing costs
// less than it looks.
//
// The case the override exists for is the reverse: a CRASHED run's leaked pool
// rather than a live suite. Then the newcomer is the legitimate process and this
// refusal is in its way -- so the message names the sessions, which is what lets
// a reader tell a live peer from a corpse.
//
// Takes an interface rather than *testing.T so the refusal itself can be
// tested. A gate that cannot fire is the same class of defect as a probe that
// cannot report, which is what this replaces -- so "it refuses when it should"
// has to be an assertion, not an assumption.
type foreignSessionReporter interface {
	Helper()
	Logf(format string, args ...any)
	Fatalf(format string, args ...any)
}

func RefuseIfForeignSessionsAtStart(t foreignSessionReporter, dialect Dialect) {
	t.Helper()

	v, ok := atStartForeign.Load(dialect)
	if !ok {
		return // nobody else was attached, or the probe could not tell
	}
	foreign, _ := v.([]string)
	if len(foreign) == 0 {
		return
	}
	if os.Getenv(AllowForeignSessionsEnv) != "" {
		t.Logf("cleat#982: %d foreign session(s) on the %s test database, allowed by %s:\n  %s",
			len(foreign), dialect, AllowForeignSessionsEnv, strings.Join(foreign, "\n  "))
		return
	}

	// Fatal rather than Skip. A skip is a green run, and a green run that
	// silently tested nothing is the outcome this whole file exists to prevent.
	t.Fatalf(`cleat#982: another client is already attached to the %s test database.

%d foreign session(s):
  %s

This process is the NEWCOMER, so it can see the incumbent; the incumbent could
not have seen it. Two processes sharing one test database delete each other's
fixtures -- every registeredBackends Setup runs an unqualified DELETE FROM across
every table -- and the damage surfaces as unreproducible failures in whichever
test was unlucky, usually in the OTHER process.

Point this run at its own database. It almost certainly does not need this one
specifically.

If the sessions above are a crashed run's leaked connections rather than a live
suite, this refusal is in the way of the legitimate process: set %s=1 to proceed.`,
		dialect, len(foreign), strings.Join(foreign, "\n  "), AllowForeignSessionsEnv)
}
