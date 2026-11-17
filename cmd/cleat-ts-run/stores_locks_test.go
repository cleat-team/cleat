package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestLocksFixtureRefusesWhatWouldBlameTheWorkflow(t *testing.T) {
	cases := []struct{ name, body, want string }{
		{"not an object", `[1]`, "is not {"},
		{"a misspelt key", `{"held":[{"key":"k","holders":"x"}]}`, "unknown field"},
		{"a key with no holder", `{"held":[{"key":"k"}]}`, "needs both a key and a holder"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := newMemoryLocksFromFile(writeFixture(t, t.TempDir(), "l.json", c.body))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %v, want one mentioning %q", err, c.want)
			}
		})
	}
}

// The three properties a workflow can observe, as the shipped stores give them: never re-entrant, only the holder releases, and
// an expired key is taken by the next acquire.
func TestLocksStoreIsNeverReentrantReleasesOnlyForTheHolderAndLetsAnExpiredKeyGo(t *testing.T) {
	ctx := context.Background()
	m, err := newMemoryLocksFromFile(writeFixture(t, t.TempDir(), "l.json", `{"held":[{"key":"theirs","holder":"other"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Unix(1_000, 0)
	m.now = func() time.Time { return clock }

	if got, _ := m.AcquireConcurrencyKey(ctx, "k", "me", 5*time.Second); !got {
		t.Fatal("a free key was refused")
	}
	if got, _ := m.AcquireConcurrencyKey(ctx, "k", "me", 5*time.Second); got {
		t.Fatal("a run re-acquired a key it already holds: acquire(k); acquire(k); release(k) would leave it free while the run thinks it holds it")
	}
	if got, _ := m.AcquireConcurrencyKey(ctx, "theirs", "me", time.Minute); got {
		t.Fatal("a key another run holds was handed over")
	}
	if rel, _ := m.ReleaseConcurrencyKey(ctx, "theirs", "me"); rel {
		t.Fatal("a run released a key another run holds (cleat#1188)")
	}
	if got, _ := m.AcquireConcurrencyKey(ctx, "theirs", "me", time.Minute); got {
		t.Fatal("the key was freed by a release that was not the holder's")
	}
	clock = clock.Add(6 * time.Second) // k's TTL has passed
	if got, _ := m.AcquireConcurrencyKey(ctx, "k", "someone-else", time.Minute); !got {
		t.Fatal("an expired key was not taken by the next acquire")
	}
	if rel, _ := m.ReleaseConcurrencyKey(ctx, "k", "me"); rel {
		t.Fatal("the first run released a key it no longer holds, after it expired and was taken")
	}
}

func TestLocksStoreIsRegisteredUnderItsNameAndReportsWhatItSaw(t *testing.T) {
	f, ok := storeRegistry["locks"]
	if !ok {
		t.Fatalf("no store registered as locks; known: %s", strings.Join(registeredNames(), ", "))
	}
	spec, err := f(writeFixture(t, t.TempDir(), "l.json", `{}`))
	if err != nil {
		t.Fatalf("an empty script is a valid fixture: %v", err)
	}
	key, v := spec.Extra()
	r := v.(map[string]any)
	if key != "locks" || len(r["acquires"].([]lockAcquire)) != 0 || len(r["held"].([]heldNow)) != 0 {
		t.Fatalf("an untouched store reported %q %v", key, r)
	}
}
