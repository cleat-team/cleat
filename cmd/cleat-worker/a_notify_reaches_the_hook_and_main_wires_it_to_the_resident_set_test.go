package main

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

// cleat#4162, 2b-i. A held run is `running`, so the NOTIFY that redispatches a suspended waiter finds nothing to dispatch (cleat-review3, #4305). The listener therefore calls a hook for EVERY
// notification, and main points it at ResidentSet.Nudge. Two things can each be wrong while the other is right, so there are two rows: the listener really calls the hook on a real NOTIFY
// (and not when nothing was sent), and main really passes a hook that reaches Nudge.

func TestTheNotifyListenerCallsItsHookForEveryRealNotificationAndNotWithoutOne(t *testing.T) {
	db := testutil.TestDB(t, testutil.DialectPostgres)
	t.Cleanup(func() { db.Close() })
	channel := fmt.Sprintf("cleat_nudge_probe_%d", time.Now().UnixNano())
	var hooks atomic.Int64
	notifyCh := make(chan struct{}, 1)
	closeFn := startNotifyListener(testutil.PostgresTestDSN(), channel, notifyCh, func() { hooks.Add(1) }, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	t.Cleanup(closeFn)

	// CONTROL: nothing sent, nothing called.
	time.Sleep(300 * time.Millisecond)
	if n := hooks.Load(); n != 0 {
		t.Fatalf("the hook was called %d time(s) with no notification sent", n)
	}
	// Three notifications in a row, with the dispatch channel (capacity 1) left FULL after the first: the hook must still run for every one, because the dispatch channel coalesces and a
	// hold that depended on it would miss all but the first.
	for i := 0; i < 3; i++ {
		if _, err := db.Exec("SELECT pg_notify($1, '')", channel); err != nil {
			t.Fatalf("pg_notify: %v", err)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for hooks.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := hooks.Load(); n != 3 {
		t.Errorf("the hook was called %d time(s) for 3 notifications, want 3: it is being coalesced with the dispatch wake-up", n)
	}
	if len(notifyCh) != 1 {
		t.Errorf("the dispatch channel holds %d wake-ups, want exactly 1 (coalesced, as before)", len(notifyCh))
	}
}

func TestMainPassesTheListenerAHookThatNudgesTheResidentSet(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	text := string(src)
	// Known positive: the real main.go. Each of these is a statement a revert would remove.
	for _, want := range []string{
		"startNotifyListener(*dbURL, *notifyChannel, notifyCh, func() { residentForNudge.Load().Nudge() }, logger)",
		"residentForNudge.Store(resident)",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("main.go no longer contains %q: a NOTIFY would not reach the held waits and a held run on Postgres would wait for its interval tick", want)
		}
	}
	// And the hook is not nil at the call: a nil hook compiles and does nothing.
	if strings.Contains(text, "notifyCh, nil, logger)") {
		t.Error("main passes a nil hook to the notify listener")
	}
}
