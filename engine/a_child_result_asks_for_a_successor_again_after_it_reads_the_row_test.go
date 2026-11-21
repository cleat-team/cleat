package engine

import (
	"context"
	"errors"
	"fmt"
	goscanner "go/scanner"
	"go/token"
	"strconv"
	"sync"
	"testing"
)

// cleat#4069, the LOGIC, with no database and no timing. The race row (a_child_result_never_settles_on_a_run_the_chain_has_left_test.go) shows the hazard fires on the real stores; these show what the
// shared helper does about it, and that the ShardedStore site, which the race row does not construct, does the same across shards.

// chainWorld is a scripted continue-as-new chain: rows by id, successors by id, and a hook that runs when a row is READ, which is how a test moves the chain "between the walk and the read".
type chainWorld struct {
	mu         sync.Mutex
	rows       map[string]ChildOutcome
	successors map[string]string
	afterRead  map[string]func(w *chainWorld)
	reads      []string
}

func (w *chainWorld) successorOf(_ context.Context, id string) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.successors[id], nil
}

func (w *chainWorld) read(_ context.Context, id string) (ChildOutcome, error) {
	w.mu.Lock()
	out, ok := w.rows[id]
	w.reads = append(w.reads, id)
	hook := w.afterRead[id]
	w.mu.Unlock()
	if hook != nil {
		hook(w) // the chain moves AFTER this row was read, which is the window
	}
	if !ok {
		return ChildOutcome{}, nil
	}
	return out, nil
}

func (w *chainWorld) link(from, to string) {
	w.mu.Lock()
	w.successors[from] = to
	w.mu.Unlock()
}

// superseded is what a run that continued as new reads as: done, with an empty result, "done because it was superseded, not because it finished".
var superseded = ChildOutcome{Completed: true, Result: `{}`}

func newChainWorld() *chainWorld {
	return &chainWorld{rows: map[string]ChildOutcome{}, successors: map[string]string{}, afterRead: map[string]func(*chainWorld){}}
}

func TestAChildResultReadFromARunThatContinuedWhileItWasReadFollowsTheChain(t *testing.T) {
	final := ChildOutcome{Completed: true, Result: `{"final":true}`}
	for _, tc := range []struct {
		name      string
		build     func() *chainWorld
		wantReads []string
		want      ChildOutcome
	}{
		{
			// CONTROL: nothing moves. The helper must not invent a hop.
			name: "CONTROL: the chain does not move",
			build: func() *chainWorld {
				w := newChainWorld()
				w.rows["a"] = final
				return w
			},
			wantReads: []string{"a"},
			want:      final,
		},
		{
			// The hazard: a's row is read as the superseded {} and a's successor appears right after. Reading once returned {}.
			name: "the chain moves once between the walk and the read",
			build: func() *chainWorld {
				w := newChainWorld()
				w.rows["a"], w.rows["b"] = superseded, final
				w.afterRead["a"] = func(w *chainWorld) { w.link("a", "b") }
				return w
			},
			wantReads: []string{"a", "b"},
			want:      final,
		},
		{
			name: "the chain keeps moving after each read",
			build: func() *chainWorld {
				w := newChainWorld()
				w.rows["a"], w.rows["b"], w.rows["c"], w.rows["d"] = superseded, superseded, superseded, final
				w.afterRead["a"] = func(w *chainWorld) { w.link("a", "b") }
				w.afterRead["b"] = func(w *chainWorld) { w.link("b", "c") }
				w.afterRead["c"] = func(w *chainWorld) { w.link("c", "d") }
				return w
			},
			wantReads: []string{"a", "b", "c", "d"},
			want:      final,
		},
		{
			// The chain was already ahead when the call started: the walk finds the end and reads it once.
			name: "the chain is already ahead at the start",
			build: func() *chainWorld {
				w := newChainWorld()
				w.rows["a"], w.rows["b"] = superseded, final
				w.link("a", "b")
				return w
			},
			wantReads: []string{"b"},
			want:      final,
		},
		{
			// A run the store cannot find reads as the zero outcome, as every GetChildResult does, and is returned as it is.
			name:      "a run that does not exist",
			build:     func() *chainWorld { return newChainWorld() },
			wantReads: []string{"a"},
			want:      ChildOutcome{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := tc.build()
			got, err := readChildOutcomeFollowingTheChain(context.Background(), "a", w.successorOf, w.read)
			if err != nil {
				t.Fatalf("readChildOutcomeFollowingTheChain: %v", err)
			}
			if got != tc.want {
				t.Errorf("returned %+v, want %+v", got, tc.want)
			}
			if fmt.Sprint(w.reads) != fmt.Sprint(tc.wantReads) {
				t.Errorf("read %v, want %v", w.reads, tc.wantReads)
			}
		})
	}
}

func TestAChildResultReadErrorsAreReportedAndNotSwallowed(t *testing.T) {
	boom := errors.New("boom")
	if _, err := readChildOutcomeFollowingTheChain(context.Background(), "a",
		func(context.Context, string) (string, error) { return "", nil },
		func(context.Context, string) (ChildOutcome, error) { return ChildOutcome{}, boom }); !errors.Is(err, boom) {
		t.Errorf("a read error came back as %v, want it passed through", err)
	}
	// The re-check is a lookup like any other: if it fails, the outcome read just before it must NOT be returned as though the chain had been confirmed to end there.
	reads := 0
	if out, err := readChildOutcomeFollowingTheChain(context.Background(), "a",
		func(context.Context, string) (string, error) {
			if reads == 0 {
				return "", nil // the walk
			}
			return "", boom // the re-check, after the read
		},
		func(context.Context, string) (ChildOutcome, error) { reads++; return superseded, nil }); !errors.Is(err, boom) || out != (ChildOutcome{}) {
		t.Errorf("a failing successor re-check returned (%+v, %v), want the error and the zero outcome, not the outcome read before it", out, err)
	}
}

// A chain that moves after EVERY read, forever, must stop with the same bound the walk has, and not loop. (It cannot happen on data the engine wrote; see maxContinueAsNewChainWalk.)
func TestAChainThatMovesAfterEveryReadForeverIsBoundedNotLoopedOn(t *testing.T) {
	readIDs := map[string]bool{}
	var mu sync.Mutex
	next := 0
	successorOf := func(_ context.Context, id string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if readIDs[id] {
			next++
			return "r" + strconv.Itoa(next), nil // as soon as a run has been read, it has a successor (short ids: the chain is a million long)
		}
		return "", nil
	}
	read := func(_ context.Context, id string) (ChildOutcome, error) {
		mu.Lock()
		defer mu.Unlock()
		readIDs[id] = true
		return superseded, nil
	}
	_, err := readChildOutcomeFollowingTheChain(context.Background(), "a", successorOf, read)
	if !errors.Is(err, ErrContinueAsNewChainTooLong) {
		t.Errorf("a chain that moves after every read returned %v, want ErrContinueAsNewChainTooLong", err)
	}
}

// scriptedShard is a shard that owns some rows and answers GetChildResult for them only. It implements runSuccessorFinder over the shared world.
type scriptedShard struct {
	WorkflowStore
	world *chainWorld
	owns  map[string]bool
}

func (s *scriptedShard) GetChildResult(ctx context.Context, id string) (ChildOutcome, error) {
	if !s.owns[id] {
		return ChildOutcome{}, fmt.Errorf("MISROUTED: this shard does not own %q", id)
	}
	return s.world.read(ctx, id)
}

func (s *scriptedShard) successorOfRun(ctx context.Context, id string) (string, error) {
	return s.world.successorOf(ctx, id)
}

// The ShardedStore site. A chain crosses shards routinely (every continuation gets a fresh id and getShard hashes the id), so the shard that holds run a cannot see run b, and its own
// GetChildResult cannot follow the chain to it. The ids are chosen so that they route to DIFFERENT shards, and each shard answers only for the rows it owns.
func TestAShardedChildResultFollowsAChainThatMovesToAnotherShardAfterTheRead(t *testing.T) {
	world := newChainWorld()
	shards := []*scriptedShard{{world: world, owns: map[string]bool{}}, {world: world, owns: map[string]bool{}}}
	sharded, err := NewShardedStore(
		[]ShardConfig{{Name: "s0"}, {Name: "s1"}},
		[]WorkflowStore{shards[0], shards[1]},
		[]func() error{nil, nil})
	if err != nil {
		t.Fatalf("NewShardedStore: %v", err)
	}
	// Find two ids that live on different shards.
	var a, b string
	for i := 0; i < 1000 && b == ""; i++ {
		id := fmt.Sprintf("run-%d", i)
		if a == "" {
			a = id
			continue
		}
		if sharded.getShard(id) != sharded.getShard(a) {
			b = id
		}
	}
	if b == "" {
		t.Fatal("could not find two ids on different shards")
	}
	for _, sh := range shards {
		if sharded.getShard(a).Store == sh {
			sh.owns[a] = true
		}
		if sharded.getShard(b).Store == sh {
			sh.owns[b] = true
		}
	}
	final := ChildOutcome{Completed: true, Result: `{"final":true}`}
	world.rows[a], world.rows[b] = superseded, final
	world.afterRead[a] = func(w *chainWorld) { w.link(a, b) } // the chain moves to the OTHER shard after a's row is read

	got, err := sharded.GetChildResult(context.Background(), a)
	if err != nil {
		t.Fatalf("GetChildResult: %v", err)
	}
	if got != final {
		t.Errorf("the parent asked for its child's result and got %+v, want %+v: the ShardedStore read a run the chain had already left, on another shard", got, final)
	}
	if fmt.Sprint(world.reads) != fmt.Sprint([]string{a, b}) {
		t.Errorf("read %v, want [%s %s]", world.reads, a, b)
	}

	// CONTROL: with nothing moving, the answer is the first read and nothing extra is read.
	world2 := newChainWorld()
	world2.rows[a] = final
	shards[0].world, shards[1].world = world2, world2
	got, err = sharded.GetChildResult(context.Background(), a)
	if err != nil || got != final || fmt.Sprint(world2.reads) != fmt.Sprint([]string{a}) {
		t.Errorf("CONTROL: with a chain that does not move got (%+v, %v) after reading %v, want %+v after reading [%s]", got, err, world2.reads, final, a)
	}
}

// callsWithArgsInCode reports whether src CALLS name in code: the identifier followed by an opening parenthesis, as the Go scanner reads it, so a comment or a string that names it does not count.
// stop_correspondence_guard_test.go's callsInCode is the same idea but matches only `name()` with an EMPTY argument list, so it cannot see `terminalRunID(ctx, id, f)`: the first version of this
// guard used it and failed its own known-positive, which is why the known-positive is there.
func callsWithArgsInCode(src, name string) bool {
	fset := token.NewFileSet()
	file := fset.AddFile("", fset.Base(), len(src))
	var sc goscanner.Scanner
	sc.Init(file, []byte(src), func(token.Position, string) {}, 0)
	ident := false
	for {
		_, tok, lit := sc.Scan()
		if tok == token.EOF {
			return false
		}
		if ident && tok == token.LPAREN {
			return true
		}
		ident = tok == token.IDENT && lit == name
	}
}

// THE SWEEP, as a test (cleat#4069). The hazard is "walk to the last run, then read it in a separate statement", and terminalRunID is the walk. Every reader of a chain goes through
// walkToTerminalRun (reads a run's instance) or readChildOutcomeFollowingTheChain (reads a child's outcome), and both ask for the successor again after the read. A bare call to terminalRunID from
// anywhere else is a walk with nothing after it, which is the defect. Today there is none outside terminal_run.go; this fails when one appears, naming it.
func TestNoCodeWalksAChainWithTerminalRunIDAndThenReadsWithoutAskingAgain(t *testing.T) {
	sources := engineSources(t)
	// Known positive: the scan sees the real calls inside terminal_run.go, so "no caller elsewhere" is not what a scan that matches nothing would also say.
	if !callsWithArgsInCode(sources["terminal_run.go"], "terminalRunID") {
		t.Fatal("the scan does not see the terminalRunID calls inside terminal_run.go, so it would pass whatever the tree said")
	}
	// And it is comment-proof, because a comment that names the function is not a call: the retraction trap.
	if callsWithArgsInCode("// terminalRunID(ctx, id, f) was the old shape\nfunc x() {}\n", "terminalRunID") {
		t.Fatal("scan control: a comment naming terminalRunID was reported as a call")
	}
	if !callsWithArgsInCode("func x() { _, _ = terminalRunID(ctx, id, f) }\n", "terminalRunID") {
		t.Fatal("scan control: a bare call was not reported")
	}
	var offenders []string
	for name, src := range sources {
		if name == "terminal_run.go" {
			continue
		}
		if callsWithArgsInCode(src, "terminalRunID") {
			offenders = append(offenders, name)
		}
	}
	if len(offenders) > 0 {
		t.Errorf("terminalRunID is called from %v. A walk to the last run followed by a separate read lets the chain move in between and hands the caller a run the chain has left (cleat#4069, cleat#4061): "+
			"use walkToTerminalRun (for an instance) or readChildOutcomeFollowingTheChain (for a child's outcome), which ask for the successor again after the read", offenders)
	}
}
