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
