package typescript

// The lock bindings against each dialect's REAL concurrency-key store, on a real worker (cleat#3827, group E).
//
// WHY THIS EXISTS BESIDE THE IN-PROCESS TESTS. The in-process store is one this package wrote, so it answers the way this package
// believes the databases answer. What a guest actually gets back is whatever PostgreSQL, MySQL and SQL Server say, and they have
// disagreed before (re-acquiring a held key was re-entrant on MySQL alone until IMPROVEMENT-PLAN 3.39; a TTL below a second was
// truncated to zero on one path, 3.34). So the four answers below are read from the worker's own result, once per dialect
// (CLEAT_TS_DIALECT), and are listed in both dialect jobs in typescript-tier2.yml.
//
// ONE DIALECT PER RUN, like every worker test here. The three answers a guest can observe: never re-entrant, release frees only
// the holder's own key, and the TTL is honoured to the millisecond-scale the guest asked for.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"
)

func TestTypeScriptLocksOnARealWorker(t *testing.T) {
	const src = "tests/typescript/testdata/workflows/locks.ts"
	e := newWorkerEnv(t, map[string]string{
		"locks_round_trip": src,
		"locks_holder":     src,
		"locks_contender":  src,
		"locks_expiry":     src,
	})
	w := startWorker(t, e.workerBin, e.appDSN, e.ownerDSN)

	// Keys carry a per-run suffix. MySQL and SQL Server keep the default tenant's data in a database that OUTLIVES the scratch one
	// (the worker logs "creating tenant database cleat_00000000_..."), so a key a previous run left held, with a ten-minute TTL, is
	// still held when the next run starts; a fixed key would then be refused to this run's holder and read as a database defect.
	sfx := fmt.Sprintf("-%d", time.Now().UnixNano())
	solo, shared, short := "solo"+sfx, "shared"+sfx, "short"+sfx

	// 1. One run, alone: the first acquire is taken, a second is refused (never re-entrant), a release frees it, and it can be
	//    taken again. A dialect whose self-reacquire answered true would fail `again`.
	rt := waitTerminal(t, w, e.key, startRun(t, w, e.key, "locks_round_trip", map[string]any{"key": solo}), 90*time.Second)
	if s, _ := rt["status"].(string); s != "done" {
		t.Fatalf("round trip ended %q: %v\nworker log tail:\n%s", s, rt, tail(w.out.String(), 3000))
	}
	got := resultObject(t, rt)
	if got["first"] != true || got["again"] != false || got["afterRelease"] != true {
		t.Errorf("round trip answered %v, want first=true again=false afterRelease=true", got)
	}

	// 2. Two runs: a holder parks on a long sleep WHILE HOLDING the key; a second run is refused, and its release of a key it does
	//    not hold frees nothing for it, so its next acquire is refused too.
	//
	//    THE PRECONDITION IS THE HOLDER'S OWN HISTORY, not its status. A first version waited for status `ready` or `suspended`, and
	//    a run that has only just been created reads `ready` too, so on two dialects the contender ran before the holder had
	//    acquired anything and was (correctly) let in -- a failure that read as a database defect. The holder's acquire_lock event
	//    with lock_acquired=1 is recorded only after the store answered, so seeing it means the key is held.
	holder := startRun(t, w, e.key, "locks_holder", map[string]any{"key": shared})
	// Cancelled when the test ends, pass or fail: the holder is parked on a five-minute sleep, and on MySQL and SQL Server its row
	// survives in the tenant database, where an open run on this workflow's version makes the NEXT deploy of different bytes
	// ("refusing to replace the WASM ... 1 run(s) are still open") fail before any test code runs.
	t.Cleanup(func() {
		api(t, w, e.key, "POST", "/api/workflows/"+holder+"/cancel", map[string]any{"preemptive": true, "reason": "cleat#3827 locks test cleanup"})
	})
	deadline := time.Now().Add(60 * time.Second)
	for !historyHasAcquired(t, w, e.key, holder) {
		if time.Now().After(deadline) {
			t.Fatalf("UNMEASURED: the holder's history never showed an acquired lock, so whether it holds the key is unknown; worker log tail:\n%s", tail(w.out.String(), 3000))
		}
		time.Sleep(200 * time.Millisecond)
	}
	ct := waitTerminal(t, w, e.key, startRun(t, w, e.key, "locks_contender", map[string]any{"key": shared}), 90*time.Second)
	if s, _ := ct["status"].(string); s != "done" {
		t.Fatalf("contender ended %q: %v\nworker log tail:\n%s", s, ct, tail(w.out.String(), 3000))
	}
	if c := resultObject(t, ct); c["first"] != false || c["afterRelease"] != false {
		t.Errorf("the contender answered %v, want first=false afterRelease=false: it was let into a key another run holds, or its release of a key it never held freed it", c)
	}

	// 3. The TTL is honoured: taken, refused, and after the TTL passes taken again by the same run.
	ex := waitTerminal(t, w, e.key, startRun(t, w, e.key, "locks_expiry", map[string]any{"key": short, "ttlMs": 1500}), 90*time.Second)
	if s, _ := ex["status"].(string); s != "done" {
		t.Fatalf("expiry run ended %q: %v\nworker log tail:\n%s", s, ex, tail(w.out.String(), 3000))
	}
	if x := resultObject(t, ex); x["first"] != true || x["again"] != false || x["afterExpiry"] != true {
		t.Errorf("expiry run answered %v, want first=true again=false afterExpiry=true: the key was not held for its TTL, or was not released when it passed", x)
	}
}

// historyHasAcquired reports whether the run's recorded history holds an acquire_lock event the store answered "acquired". The
// reply is DECODED and walked, and its shape (array or object) is not relied on: the first version searched the raw body for
// `"lock_acquired":1`, which MySQL's JSON column rewrites as `"lock_acquired": 1`, so on that dialect alone the holder never
// seemed to have acquired anything.
func historyHasAcquired(t *testing.T, w *worker, key, id string) bool {
	t.Helper()
	req, err := http.NewRequest("GET", "http://"+w.addr+"/api/workflows/"+id+"/history", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("GET history: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("GET history: HTTP %d %s", resp.StatusCode, raw)
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("history is not JSON: %v\n%s", err, raw)
	}
	var walk func(v any) bool
	walk = func(v any) bool {
		switch x := v.(type) {
		case map[string]any:
			if x["type"] == "acquire_lock" && x["lock_acquired"] == float64(1) {
				return true
			}
			for _, c := range x {
				if walk(c) {
					return true
				}
			}
		case []any:
			for _, c := range x {
				if walk(c) {
					return true
				}
			}
		}
		return false
	}
	return walk(doc)
}
