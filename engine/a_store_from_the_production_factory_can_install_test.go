package engine

// cleat#3540, PR 3a: the install route asks the tenant's store whether it is a
// LibraryInstaller. A store the production factory hands out that is wrapped or
// decorated would fail that assertion quietly, and every install would answer 501
// "this deployment's store cannot install" on a deployment that can. So this opens a
// store the way the worker does and asks the same question.
//
// MSSQLStoreFactory returns the concrete *MSSQLStore and needs a live pool to open one,
// so it is held by the compile-time assertions in library_install.go instead.

import (
	"context"
	"testing"
)

func TestAStoreFromTheProductionFactoryCanInstall(t *testing.T) {
	st, closer, err := NewPostgresStoreFactory(nil, "public").OpenStore(context.Background(), "f5000000-0000-4000-8000-0000000035a0")
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer closer.Close()
	if _, ok := st.(LibraryInstaller); !ok {
		t.Fatalf("a PostgresStoreFactory store is a %T and is not a LibraryInstaller: the route would answer 501 on every PostgreSQL deployment", st)
	}
}

// The sharded store does not install yet (cleat#3597). The route's 501 names that issue;
// when the sharded store learns to install, this fails, and the message and the 3b tests
// are the next thing to change.
func TestTheShardedStoreDoesNotInstallYet(t *testing.T) {
	if _, ok := any(&ShardedStore{}).(LibraryInstaller); ok {
		t.Fatal("ShardedStore is now a LibraryInstaller: update the install route's 501 message (it names cleat#3597) and its test, and test the sharded install")
	}
}
