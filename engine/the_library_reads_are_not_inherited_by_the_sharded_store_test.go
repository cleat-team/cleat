package engine

// cleat#3627: the routes decide 501 by asking the tenant's store whether it implements a read.
//
// A sharded deployment answers 501 to the installs list and the catalogue (as it does to the install,
// cleat#3606) only because *ShardedStore does not implement them. It has no embedded store today, which is
// the only reason: nothing else pins it, and a future change that embedded a store would turn the 501 into a
// live read of whichever shard it embedded, silently and with a 200. A compile-time `var _` cannot assert
// ABSENCE, so this asks at run time, the way TestTheShardedStoreDoesNotInstallYet does for the install.
//
// The other half is the one that would fail the other way: a store the production factory hands out that is
// wrapped or decorated would fail the same assertion, and every read would answer 501 on a deployment that
// can serve it. So this opens a store the way the worker does and asks the same question.

import (
	"context"
	"testing"
)

func TestTheShardedStoreDoesNotListLibraries(t *testing.T) {
	if _, ok := any(&ShardedStore{}).(LibraryInstallLister); ok {
		t.Fatal("ShardedStore is now a LibraryInstallLister: GET /api/library-installs would stop answering 501 on a sharded deployment and answer for one shard. Decide that on purpose (cleat#3606), then update the route and its test")
	}
	if _, ok := any(&ShardedStore{}).(LibraryCatalogue); ok {
		t.Fatal("ShardedStore is now a LibraryCatalogue: GET /api/libraries would stop answering 501 on a sharded deployment and answer for one shard. Decide that on purpose (cleat#3606), then update the route and its test")
	}
}

func TestAStoreFromTheProductionFactoryCanListLibraries(t *testing.T) {
	st, closer, err := NewPostgresStoreFactory(nil, "public").OpenStore(context.Background(), "f5000000-0000-4000-8000-0000000036a0")
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer closer.Close()
	if _, ok := st.(LibraryInstallLister); !ok {
		t.Fatalf("a PostgresStoreFactory store is a %T and is not a LibraryInstallLister: the installs route would answer 501 on every PostgreSQL deployment", st)
	}
	if _, ok := st.(LibraryCatalogue); !ok {
		t.Fatalf("a PostgresStoreFactory store is a %T and is not a LibraryCatalogue: the catalogue route would answer 501 on every PostgreSQL deployment", st)
	}
}
