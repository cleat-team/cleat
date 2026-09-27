package main

import (
	"testing"
	"time"
)

// cleat#2311. A run no live worker can serve is released again on every
// backoff -- every 5s by default -- and each release used to log a WARN line.
// One stuck run wrote about 17,000 of them a day. releaseForAnotherWorker now
// warns once per (workflow, check) per unservableWarnEvery; the counter still
// counts every release.
func TestARepeatedReleaseWarnsOncePerWindowPerRunAndCheck(t *testing.T) {
	w := &Worker{}
	t0 := time.Now()

	if !w.shouldWarnUnservable("wf-1", "history_decrypt", t0) {
		t.Fatal("the first release of a run must warn")
	}
	for i := 1; i <= 50; i++ {
		if w.shouldWarnUnservable("wf-1", "history_decrypt", t0.Add(time.Duration(i)*5*time.Second)) &&
			time.Duration(i)*5*time.Second < unservableWarnEvery {
			t.Fatalf("release %d, %v after the first, warned again inside the window", i, time.Duration(i)*5*time.Second)
		}
	}
	// A different run, and a different check on the same run, each warn on their own.
	if !w.shouldWarnUnservable("wf-2", "history_decrypt", t0.Add(time.Second)) {
		t.Error("a different run must warn")
	}
	if !w.shouldWarnUnservable("wf-1", "version_check", t0.Add(time.Second)) {
		t.Error("a different check on the same run must warn")
	}
	// And after the window the first pair warns again.
	if !w.shouldWarnUnservable("wf-1", "history_decrypt", t0.Add(unservableWarnEvery+time.Minute)) {
		t.Error("a release after the window must warn again")
	}
}
