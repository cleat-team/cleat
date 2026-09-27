package main

// A stalled worker is never invisible to a secret writer AND unaware of it
// (cleat#2167, found by review of cleat#2160).
//
// The gate counts a worker as live for engine.SecretKeyLiveWindow after its last
// heartbeat. The worker re-registers and re-checks its secrets only after a gap
// longer than membershipStaleAfter. If a stall can outlast the first and end before
// the second, a writer stores a key version the worker cannot open and the worker
// resumes, sees no lapse, and serves. These tests drive the REAL writer gate and the
// REAL membership tick through that timeline, and each half has a known-positive: a
// configuration or a state in which the hazard is real, so a pass cannot be an
// artefact of a timeline that never produced it.

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/internal/tenantctx"
	"github.com/google/uuid"
)

type stallOutcome struct {
	wrote   bool // a writer stored a key version this worker cannot open
	stopped bool // ...and the worker's next membership tick stopped it
}

// stall plays the timeline. The worker holds only key version 1 and its last membership
// tick (or, with zeroLastBeat, no tick at all) was `age` ago, which is also how old its
// registry row is. A writer then stores a version-2 secret, and the worker's next tick
// runs with the threshold that heartbeat implies.
func stall(t *testing.T, heartbeat, age time.Duration, zeroLastBeat bool) stallOutcome {
	t.Helper()
	e := newLapseEnv(t, ringV1(t), nil)
	ctx := context.Background()
	if err := e.reg.Register(ctx, engine.WorkerRegistration{
		WorkerID: e.id, Hostname: "stalled", PID: 1, SecretKeyVersions: []int{1}}); err != nil {
		t.Fatal(err)
	}
	// A worker the tick stops leaves its row behind, freshly heartbeated and holding only
	// version 1, until its shutdown path deregisters it. Two calls in one test share a t,
	// so the second would find the first's row live and be refused for the wrong reason.
	defer func() { _ = e.reg.Deregister(context.Background(), e.id) }()
	if _, err := e.db.Exec(`UPDATE admin.workers SET last_heartbeat_at = now() - make_interval(secs => $1)
		WHERE worker_id = $2`, age.Seconds(), e.id); err != nil {
		t.Fatalf("age the registry row: %v", err)
	}
	if zeroLastBeat {
		e.w.membershipLastBeat = time.Time{}
	} else {
		e.w.membershipLastBeat = time.Now().Add(-age)
	}

	// The writer holds versions 2 and 1, so it writes at 2.
	v1, v2 := ringV1(t), ringV2(t)
	writer, err := engine.NewKeyRing(v2.Current(), v1.Current())
	if err != nil {
		t.Fatal(err)
	}
	tenant := uuid.MustParse(engine.DefaultTenantUUID)
	name := "cleat-2167-" + uuid.NewString()[:8]
	t.Cleanup(func() { _, _ = e.db.Exec(`DELETE FROM tenant_secrets WHERE name = $1`, name) })
	werr := engine.NewSecretStoreWithRing(e.db, "postgres", writer).
		PutSecret(tenantctx.With(ctx, tenant), tenant.String(), name, "value-under-v2")
	out := stallOutcome{wrote: werr == nil}
	if werr != nil {
		t.Logf("the write was refused: %v", werr)
	}

	e.w.membershipTick(membershipStaleAfter(heartbeat))
	out.stopped = e.w.ctx.Err() != nil
	return out
}

func ringV2(t *testing.T) *engine.KeyRing {
	t.Helper()
	r, err := engine.NewKeyRing(engine.VersionedKey{Version: 2, Key: []byte("fedcba9876543210fedcba9876543210")})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// The bound, in both directions and everywhere in between.
func TestAHeartbeatIsRefusedExactlyWhereTheLapseThresholdReachesTheLiveWindow(t *testing.T) {
	for _, tc := range []struct {
		hb   time.Duration
		want bool // accepted
	}{
		{0, true}, // the flag default is applied downstream
		{time.Second, true},
		{5 * time.Second, true},
		{30 * time.Second, true},
		{149 * time.Second, true},
		{149*time.Second + 900*time.Millisecond, true},
		{150 * time.Second, false},
		{4 * time.Minute, false},
		{10 * time.Minute, false},
	} {
		err := validateHeartbeat(tc.hb)
		if (err == nil) != tc.want {
			t.Errorf("validateHeartbeat(%v) = %v, accepted=%v, want accepted=%v", tc.hb, err, err == nil, tc.want)
		}
		if err != nil {
			for _, must := range []string{"--heartbeat", "150s"} {
				if !strings.Contains(err.Error(), must) {
					t.Errorf("the refusal for %v does not say %q:\n%s", tc.hb, must, err)
				}
			}
		}
	}

	// The relation, not a list of examples: accepted <=> the threshold the loop
	// uses is below the writers' window. Sampled every second across ten minutes.
	for hb := time.Second; hb <= 10*time.Minute; hb += time.Second {
		accepted := validateHeartbeat(hb) == nil
		below := membershipStaleAfter(hb) < engine.SecretKeyLiveWindow
		if accepted != below {
			t.Fatalf("at --heartbeat %v: accepted=%v but threshold %v below the %v window = %v",
				hb, accepted, membershipStaleAfter(hb), engine.SecretKeyLiveWindow, below)
		}
	}
}

// The timeline. Every heartbeat the worker accepts closes the hole; a heartbeat it
// refuses does not, and that last row is the known-positive: without it "stopped"
// could be true for any reason at all.
func TestAStallPastTheWritersWindowIsAlwaysALapseForTheWorker(t *testing.T) {
	age := engine.SecretKeyLiveWindow + time.Minute // the writer no longer counts this worker

	for _, hb := range []time.Duration{5 * time.Second, 30 * time.Second, 149 * time.Second} {
		t.Run(fmt.Sprintf("heartbeat %v", hb), func(t *testing.T) {
			if err := validateHeartbeat(hb); err != nil {
				t.Fatalf("this test is about accepted heartbeats, and %v is refused: %v", hb, err)
			}
			got := stall(t, hb, age, false)
			if !got.wrote {
				t.Fatal("the writer was refused, so the writer DID count the stalled worker: " +
					"the timeline did not reach the state it is about")
			}
			if !got.stopped {
				t.Fatalf("a write landed at a version the worker cannot open while it was stalled past the "+
					"writers' window, and its next tick at --heartbeat %v did not stop it (cleat#2167)", hb)
			}
		})
	}

	// The margin. At the largest heartbeat the worker accepts the threshold is 2s below
	// the writers' window, and a stall that has just crossed the window (1s past it) must
	// already be a lapse. The 6 minute stall above cannot tell a 2s margin from none.
	t.Run("at the boundary, 1s past the writers' window", func(t *testing.T) {
		hb := 149 * time.Second
		got := stall(t, hb, engine.SecretKeyLiveWindow+time.Second, false)
		if !got.wrote || !got.stopped {
			t.Fatalf("a stall 1s past the window at --heartbeat %v: wrote=%v stopped=%v, want both true", hb, got.wrote, got.stopped)
		}
	})

	t.Run("KNOWN-POSITIVE: a heartbeat the worker refuses", func(t *testing.T) {
		hb := 4 * time.Minute
		if validateHeartbeat(hb) == nil {
			t.Fatalf("%v is accepted, so this is not the configuration cleat#2167 measured", hb)
		}
		got := stall(t, hb, age, false)
		if !got.wrote || got.stopped {
			t.Fatalf("at --heartbeat %v the timeline gave wrote=%v stopped=%v, want wrote=true stopped=false: "+
				"the hazard the bound exists for is no longer reproducible, so the passes above prove nothing",
				hb, got.wrote, got.stopped)
		}
	})
}

// GAP 2: the first tick after a slow boot. The lapse clock has to have started at
// registration; a zero value means "never ticked, so cannot have lapsed".
func TestASlowBootIsALapseOnTheFirstTick(t *testing.T) {
	age := engine.SecretKeyLiveWindow + time.Minute

	// Registration was `age` ago and nothing has ticked since: main sets
	// membershipLastBeat to the registration time.
	got := stall(t, 5*time.Second, age, false)
	if !got.wrote || !got.stopped {
		t.Fatalf("a worker whose boot took longer than the writers' window after it registered: "+
			"wrote=%v stopped=%v, want both true", got.wrote, got.stopped)
	}

	t.Run("KNOWN-POSITIVE: the clock never started", func(t *testing.T) {
		got := stall(t, 5*time.Second, age, true)
		if !got.wrote || got.stopped {
			t.Fatalf("with membershipLastBeat unset the timeline gave wrote=%v stopped=%v, want wrote=true "+
				"stopped=false: the state cleat#2167 measured is no longer reproducible", got.wrote, got.stopped)
		}
	})
}

// main is the only place that constructs the production Worker, so a test of the tick
// cannot tell whether main starts the lapse clock. The literal has to name the field.
func workerLiteralSetsMembershipLastBeat(src []byte) (bool, error) {
	f, err := parser.ParseFile(token.NewFileSet(), "main.go", src, 0)
	if err != nil {
		return false, err
	}
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		if id, ok := lit.Type.(*ast.Ident); !ok || id.Name != "Worker" {
			return true
		}
		for _, el := range lit.Elts {
			if kv, ok := el.(*ast.KeyValueExpr); ok {
				if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "membershipLastBeat" {
					// A literal time.Time{} is the zero value written out.
					if c, ok := kv.Value.(*ast.CompositeLit); ok && len(c.Elts) == 0 {
						continue
					}
					found = true
				}
			}
		}
		return true
	})
	return found, nil
}

func TestMainStartsTheLapseClockAtRegistration(t *testing.T) {
	// The finder has to be able to say no.
	for name, src := range map[string]string{
		"a literal that omits the field": `package main; var w = &Worker{id: "x"}`,
		"a literal that writes the zero": `package main; var w = &Worker{membershipLastBeat: time.Time{}}`,
	} {
		if ok, err := workerLiteralSetsMembershipLastBeat([]byte(src)); err != nil || ok {
			t.Fatalf("the finder accepted %s: ok=%v err=%v", name, ok, err)
		}
	}
	if ok, err := workerLiteralSetsMembershipLastBeat([]byte(
		`package main; var w = &Worker{membershipLastBeat: registeredAt}`)); err != nil || !ok {
		t.Fatalf("the finder rejected a literal that sets the field: ok=%v err=%v", ok, err)
	}

	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	ok, err := workerLiteralSetsMembershipLastBeat(src)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("main.go constructs the Worker without setting membershipLastBeat: a boot slower than the " +
			"writers' window is then never a lapse (cleat#2167)")
	}
}

// THE BEAT IS RECORDED BEFORE THE ROUND TRIP (cleat#2167, found reviewing cleat#2171).
//
// The database stamps last_heartbeat_at while the statement runs; the reply then travels
// back. A beat recorded when the call RETURNS makes this worker's gap shorter than the
// writer's by the reply's latency, and at the largest accepted heartbeat the margin
// under the writers' window is only 2s. The property is that the worker's own gap is
// never SHORTER than the gap a writer measures, and it is checked against a real reply
// delay: a trigger holds the UPDATE's reply for 4s AFTER the row is stamped. Both gaps
// are durations (the database's is now() minus its own stamp, ours is monotonic since the
// beat), so no comparison crosses two clocks.
func TestTheWorkersGapIsNeverShorterThanTheWritersGap(t *testing.T) {
	e := newLapseEnv(t, ringV1(t), nil)
	ctx := context.Background()
	if err := e.reg.Register(ctx, engine.WorkerRegistration{
		WorkerID: e.id, Hostname: "slow-reply", PID: 1, SecretKeyVersions: []int{1}}); err != nil {
		t.Fatal(err)
	}
	fn := "zz_2167_slow_reply_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	for _, stmt := range []string{
		`CREATE FUNCTION public.` + fn + `() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_sleep(4); RETURN NULL; END $$`,
		`CREATE TRIGGER ` + fn + ` AFTER UPDATE ON admin.workers FOR EACH ROW WHEN (NEW.worker_id = '` + e.id + `') EXECUTE FUNCTION public.` + fn + `()`,
	} {
		if _, err := e.db.Exec(stmt); err != nil {
			t.Fatalf("install the slow-reply trigger: %v\n  %s", err, stmt)
		}
	}
	t.Cleanup(func() {
		_, _ = e.db.Exec(`DROP TRIGGER IF EXISTS ` + fn + ` ON admin.workers`)
		_, _ = e.db.Exec(`DROP FUNCTION IF EXISTS public.` + fn + `()`)
	})

	e.w.membershipLastBeat = time.Now()
	began := time.Now()
	e.w.membershipTick(membershipStaleAfter(149 * time.Second))
	if took := time.Since(began); took < 3*time.Second {
		t.Fatalf("the tick took %v, so the trigger never delayed the heartbeat's reply and this test measured nothing", took)
	}

	// The database's gap first, ours after: measured in that order, any difference from the
	// instants being apart can only make OUR gap the larger of the two.
	var dbGap float64
	if err := e.db.QueryRow(`SELECT extract(epoch FROM now() - last_heartbeat_at) FROM admin.workers WHERE worker_id = $1`, e.id).Scan(&dbGap); err != nil {
		t.Fatal(err)
	}
	ourGap := time.Since(e.w.membershipLastBeat).Seconds()
	t.Logf("the writer's gap %.2fs, this worker's %.2fs", dbGap, ourGap)
	if dbGap < 3 {
		t.Fatalf("the database's gap is %.2fs: the row was not stamped before the delayed reply, so the scenario did not happen", dbGap)
	}
	// A tolerance, because the two gaps come from different clocks: the database's is its wall clock
	// (now() minus a stamp it took), ours is this host's monotonic clock. Recorded before the round
	// trip, ours is the larger by the two queries' latency, a few milliseconds, and a clock-rate
	// difference over a 4s window (an NTP slew, a container's virtual clock under load) is the same
	// size: with no tolerance this failed on an idle machine's 4.01s against 4.01s (cleat#2225). The
	// defect this test guards is the reply's whole latency, 4s here, so 250ms cannot hide it.
	const clockNoise = 0.25
	if ourGap < dbGap-clockNoise {
		t.Fatalf("this worker's gap (%.2fs) is SHORTER than the writer's (%.2fs) by %.2fs: the beat was recorded after the round trip, "+
			"so a stall can be past the writers' window while the worker still calls it fresh (cleat#2167)", ourGap, dbGap, dbGap-ourGap)
	}
}

// mainCalls reports whether function fn in src contains a call to callee whose arguments
// mention argIdent, and where (a byte offset, for ordering against other statements).
func mainCalls(src []byte, fn, callee, argIdent string) (bool, token.Pos, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", src, 0)
	if err != nil {
		return false, 0, err
	}
	var found bool
	var at token.Pos
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Name.Name != fn || fd.Recv != nil {
			continue
		}
		ast.Inspect(fd, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if id, ok := call.Fun.(*ast.Ident); !ok || id.Name != callee {
				return true
			}
			mentions := false
			for _, a := range call.Args {
				ast.Inspect(a, func(m ast.Node) bool {
					if id, ok := m.(*ast.Ident); ok && id.Name == argIdent {
						mentions = true
					}
					return true
				})
			}
			if mentions && !found {
				found, at = true, call.Pos()
			}
			return true
		})
	}
	return found, at, nil
}

// main is the only place validateHeartbeat can run, and deleting the call leaves every
// test of the function itself green. So the call is checked in the source, by a finder
// that is shown to say no.
func TestMainRefusesAnUnsafeHeartbeatBeforeItDoesAnythingElse(t *testing.T) {
	for name, src := range map[string]string{
		"main never calls it":                 `package main; func main() { flag.Parse() }`,
		"a different function calls it":       `package main; func other() { _ = validateHeartbeat(*heartbeatInterval) }; func main() {}`,
		"main calls it with another value":    `package main; func main() { _ = validateHeartbeat(time.Second) }`,
		"main mentions it without calling it": `package main; func main() { _ = validateHeartbeat }`,
	} {
		if ok, _, err := mainCalls([]byte(src), "main", "validateHeartbeat", "heartbeatInterval"); err != nil || ok {
			t.Fatalf("the finder accepted %q: ok=%v err=%v", name, ok, err)
		}
	}
	if ok, _, err := mainCalls([]byte(`package main; func main() { if err := validateHeartbeat(*heartbeatInterval); err != nil {} }`),
		"main", "validateHeartbeat", "heartbeatInterval"); err != nil || !ok {
		t.Fatalf("the finder rejected a real call: ok=%v err=%v", ok, err)
	}

	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	ok, hbPos, err := mainCalls(src, "main", "validateHeartbeat", "heartbeatInterval")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("main() never calls validateHeartbeat(*heartbeatInterval): a --heartbeat at which a stalled worker can hide " +
			"from a secret writer is then accepted, and every test of validateHeartbeat still passes (cleat#2167)")
	}
	// Before the worker registers: a refusal that came after it would have published the row.
	_, regPos, err := mainCalls(src, "main", "registerWithKeyCheck", "workerRegistry")
	if err != nil {
		t.Fatal(err)
	}
	if regPos != 0 && hbPos > regPos {
		t.Fatal("main() checks --heartbeat only after it has registered the worker")
	}
}

// registeredAt has to be taken BEFORE the registration: the registry stamps the row while
// RegisterUnderKeyGate runs (after a wait for the gate lock that can last 45s), so a clock
// started when the call returns is short by all of it.
func TestTheLapseClockIsStartedBeforeTheRegistration(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	f, err := parser.ParseFile(token.NewFileSet(), "main.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	var assignAt, callAt token.Pos
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.AssignStmt:
			if len(x.Lhs) == 1 {
				if id, ok := x.Lhs[0].(*ast.Ident); ok && id.Name == "registeredAt" && assignAt == 0 {
					assignAt = x.Pos()
				}
			}
		case *ast.CallExpr:
			if id, ok := x.Fun.(*ast.Ident); ok && id.Name == "registerWithKeyCheck" && callAt == 0 {
				callAt = x.Pos()
			}
		}
		return true
	})
	if assignAt == 0 || callAt == 0 {
		t.Fatalf("did not find both `registeredAt := ...` (%v) and the registerWithKeyCheck call (%v) in main.go", assignAt != 0, callAt != 0)
	}
	if assignAt > callAt {
		t.Fatal("registeredAt is assigned AFTER registerWithKeyCheck returns: the lapse clock is then short by the gate wait " +
			"and the registration round trip (cleat#2167)")
	}
}
