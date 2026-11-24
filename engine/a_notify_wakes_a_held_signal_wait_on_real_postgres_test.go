package engine

import (
	"context"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/lib/pq"
)

// THE LISTENER HALF, END TO END ON REAL POSTGRES (cleat#4162, cleat-review3 on #4305). The hold's recheck interval is set to 3 s so that a wake that comes from the interval is unmistakable:
// a real DeliverSignal sends a real pg_notify on the channel, a real listener receives it and calls ResidentSet.Nudge, and the PRODUCTION recheck finds the delivery. The wake must come well
// inside the interval. The control is the same run with the listener not calling Nudge, which must take the interval.
//
// This proves the mechanism on real Postgres. It does NOT compare end-to-end time against residency off: that is cleat#4306's row (held vs off, p50 and p99), and this one asserts a bound, not a
// comparison.

func TestARealNotifyWakesAHeldSignalWaitWellInsideItsRecheckInterval(t *testing.T) {
	var backend StoreBackend
	for _, b := range registeredBackends {
		if b.Name() == "postgres" {
			backend = b
		}
	}
	if backend == nil {
		t.Fatal("no postgres backend is registered")
	}
	for _, tc := range []struct {
		name  string
		nudge bool
	}{
		{"the listener nudges the set", true},
		{"CONTROL: the listener does not nudge, so the interval is what wakes it", false},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)
			truncateAll(t, store)
			ctx := context.Background()
			pg := store.(*PostgresStore)
			channel := "cleat_nudge_row"
			pg.notifyChannel = channel
			wf := startRealRun(t, store, "notify-wake-"+boolWord(tc.nudge))

			const interval = 3 * time.Second
			m := newReplayMetrics(t)
			cfg := DefaultResidencyConfig()
			cfg.MaxRuns = 2
			cfg.RecheckInterval = interval
			cfg.NudgeMinInterval = 0
			cfg.Metrics = m
			set, err := NewResidentSet(cfg)
			if err != nil {
				t.Fatalf("NewResidentSet: %v", err)
			}

			listener := pq.NewListener(testutil.PostgresTestDSN(), 100*time.Millisecond, 5*time.Second, func(pq.ListenerEventType, error) {})
			defer listener.Close()
			if err := listener.Listen(channel); err != nil {
				t.Fatalf("LISTEN: %v", err)
			}
			go func() {
				for n := range listener.Notify {
					if n == nil {
						return
					}
					if tc.nudge {
						set.Nudge()
					}
				}
			}()

			production := signalRecheckFor(realSignalSession(t, store, wf), `["go"]`)
			out := make(chan parkOutcome, 1)
			go func() { out <- set.ForRun(ResidentRunInfo{Language: "go"}).Wait(ctx, signalWaitReq(production)) }()
			deadline := time.Now().Add(10 * time.Second)
			for set.Len() == 0 && time.Now().Before(deadline) {
				time.Sleep(2 * time.Millisecond)
			}
			if set.Len() == 0 {
				t.Fatal("PRECONDITION FAILED: the run was never held, so the latency below measures nothing")
			}
			time.Sleep(200 * time.Millisecond) // the hold has made its admission recheck and is waiting on its tick

			delivered := time.Now()
			if err := store.DeliverSignal(ctx, wf, "go", `{"approved":true}`); err != nil {
				t.Fatalf("DeliverSignal: %v", err)
			}
			select {
			case got := <-out:
				if got != parkWoke {
					t.Fatalf("the hold ended with %d, want parkWoke", got)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the hold never woke")
			}
			lat := time.Since(delivered)
			t.Logf("%s: woke %s after DeliverSignal (recheck interval %s)", tc.name, lat.Round(time.Millisecond), interval)
			if tc.nudge && lat > interval/3 {
				t.Errorf("a real NOTIFY woke the hold after %s, against a %s interval: it waited for the tick and not for the nudge", lat, interval)
			}
			if !tc.nudge && lat < interval/2 {
				t.Errorf("CONTROL FAILED: with no nudge the hold woke after %s, well before its %s interval, so the row above cannot tell a nudge from something else", lat, interval)
			}
		})
	}
}
