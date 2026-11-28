package engine

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// cleat#3630. ShardedStore.LoadWASM asks the designated shard first and then the others, keeping the LAST
// error. A shard that refuses a library member whose bytes do not match their digest therefore reported
// "wasm not found" whenever a later shard had no copy, and a later shard that did have one was used instead:
// the refusal was lost either way. A mismatch is a finding about the data, not a miss, so it is returned
// as it is, from the shard that found it, and no other shard is asked.
//
// The shards here are fakes: only LoadWASM is called, and the routing decides the order, which the test
// pins by choosing a name that routes to the shard it wants asked first.

type loadWASMShard struct {
	WorkflowStore
	wasm []byte
	err  error
	asks int
}

func (f *loadWASMShard) LoadWASM(context.Context, string, int) ([]byte, error) {
	f.asks++
	return f.wasm, f.err
}

func TestAShardedLoadDoesNotHideADigestMismatch(t *testing.T) {
	mismatch := &LibraryMemberDigestMismatchError{Library: "lib", Name: "lib.def", Version: 1, Recorded: "aa", Computed: "bb"}
	notFound := errors.New("wasm not found: lib.def v1")

	cases := []struct {
		name          string
		first, rest   *loadWASMShard // first is the designated shard
		wantMismatch  bool
		wantRestAsked bool
	}{
		{"the designated shard finds a mismatch and the others have no copy", &loadWASMShard{err: mismatch}, &loadWASMShard{err: notFound}, true, false},
		{"the designated shard finds a mismatch and another shard holds intact bytes", &loadWASMShard{err: mismatch}, &loadWASMShard{wasm: []byte("intact")}, true, false},
		{"the designated shard has no copy and another finds a mismatch", &loadWASMShard{err: notFound}, &loadWASMShard{err: mismatch}, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stores := []WorkflowStore{c.first, c.rest}
			st, err := NewShardedStore([]ShardConfig{{Name: "a"}, {Name: "b"}}, stores, []func() error{func() error { return nil }, func() error { return nil }})
			if err != nil {
				t.Fatalf("UNMEASURED: NewShardedStore: %v", err)
			}
			// A name that routes to the shard holding c.first, so the order is the one the case describes.
			var name string
			for i := 0; i < 10000; i++ {
				n := fmt.Sprintf("lib.def%d", i)
				if st.getShard(n).Config.Name == "a" {
					name = n
					break
				}
			}
			if name == "" {
				t.Fatal("UNMEASURED: no name routes to shard a")
			}
			wasm, err := st.LoadWASM(context.Background(), name, 1)
			if c.wantMismatch && !errors.Is(err, ErrLibraryMemberDigestMismatch) {
				t.Errorf("LoadWASM = %d bytes, %v; want the digest mismatch, as the shard found it", len(wasm), err)
			}
			if len(wasm) != 0 {
				t.Errorf("LoadWASM returned %d bytes beside a mismatch; it must return none", len(wasm))
			}
			if got := c.rest.asks > 0; got != c.wantRestAsked {
				t.Errorf("the other shard was asked: %v, want %v", got, c.wantRestAsked)
			}
		})
	}
}
