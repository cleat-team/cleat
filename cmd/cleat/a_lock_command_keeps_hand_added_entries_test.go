package main

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/wasm"
)

// cleat#3521, the sibling of cleat#3508. `cleat lock <package>` wrote the lock
// from the scan alone, so a child started from an imported package (in the lock
// only because someone listed it) was dropped, and `cleat lock --update`
// re-resolved every name, replacing a version chosen by hand, with no report.
//
// These drive lockFileFor, which is runLock without the database connection and
// the file write, through a stub store. That is deliberate: the host this was
// written on had no spare Docker disk, so nothing here needs a database. What
// that leaves untested is runLock's own wiring (reading cleat.lock, choosing
// between the scan and the lock's names, writing the file).

// deployedStore answers the two resolution calls from a fixed map and embeds the
// interface so anything else it is asked for fails loudly.
type deployedStore struct {
	engine.WorkflowStore
	latest map[string]int
}

func (s deployedStore) ResolveLatestVersion(_ context.Context, name string) (int, error) {
	v, ok := s.latest[name]
	if !ok {
		return 0, fmt.Errorf("%s is not deployed", name)
	}
	return v, nil
}

func (s deployedStore) ResolveVersionByTag(_ context.Context, name, _ string) (int, error) {
	return 0, fmt.Errorf("no tag for %s", name)
}

func TestLockCommandKeepsHandAddedEntries(t *testing.T) {
	ctx := context.Background()
	// The lock a user has: the own-package child out of date, and a child an
	// imported package starts, listed by hand at a version they chose.
	existing := &wasm.LockFile{Version: wasm.LockFileVersion, Policy: "frozen", Entries: map[string]wasm.LockEntry{
		"own-child": {Version: 1}, "imported-child": {Version: 5},
	}}
	store := deployedStore{latest: map[string]int{"own-child": 2, "imported-child": 9}}

	t.Run("lock <package> re-resolves what the scan finds and keeps the rest", func(t *testing.T) {
		lf, resolved, kept, changed, err := lockFileFor(ctx, store, existing, map[string]bool{"own-child": true}, "stable")
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]wasm.LockEntry{"own-child": {Version: 2}, "imported-child": {Version: 5}}
		if !reflect.DeepEqual(lf.Entries, want) {
			t.Errorf("entries = %v, want %v: the hand-added entry did not survive", lf.Entries, want)
		}
		if !reflect.DeepEqual(resolved, map[string]int{"own-child": 2}) {
			t.Errorf("resolved = %v, want only the scanned child", resolved)
		}
		if !reflect.DeepEqual(kept, []string{"imported-child"}) {
			t.Errorf("kept = %v, want [imported-child]", kept)
		}
		if !reflect.DeepEqual(changed, []lockChange{{Name: "own-child", From: 1, To: 2}}) {
			t.Errorf("changed = %v, want own-child 1 -> 2", changed)
		}
		if lf.Version != wasm.LockFileVersion || lf.Policy != "stable" {
			t.Errorf("lock header = version %d policy %q", lf.Version, lf.Policy)
		}
	})

	t.Run("lock --update re-resolves every name and reports each version it moves", func(t *testing.T) {
		all := map[string]bool{"own-child": true, "imported-child": true}
		lf, _, kept, changed, err := lockFileFor(ctx, store, existing, all, "stable")
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]wasm.LockEntry{"own-child": {Version: 2}, "imported-child": {Version: 9}}
		if !reflect.DeepEqual(lf.Entries, want) {
			t.Errorf("entries = %v, want %v", lf.Entries, want)
		}
		if len(kept) != 0 {
			t.Errorf("kept = %v, want nothing: --update resolved every name", kept)
		}
		// The point of the report: the hand-chosen version 5 became 9, and the
		// run says so.
		wantChanged := []lockChange{{Name: "imported-child", From: 5, To: 9}, {Name: "own-child", From: 1, To: 2}}
		if !reflect.DeepEqual(changed, wantChanged) {
			t.Errorf("changed = %v, want %v", changed, wantChanged)
		}
	})

	t.Run("with no lock yet the result is the resolved set and nothing is reported", func(t *testing.T) {
		lf, _, kept, changed, err := lockFileFor(ctx, store, nil, map[string]bool{"own-child": true}, "stable")
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(lf.Entries, map[string]wasm.LockEntry{"own-child": {Version: 2}}) || len(kept) != 0 || len(changed) != 0 {
			t.Errorf("entries=%v kept=%v changed=%v", lf.Entries, kept, changed)
		}
	})

	t.Run("an unresolvable child fails the run and writes nothing to merge", func(t *testing.T) {
		if _, _, _, _, err := lockFileFor(ctx, store, existing, map[string]bool{"not-deployed": true}, "stable"); err == nil {
			t.Error("a child with no deployed version must fail the run")
		}
	})
}
