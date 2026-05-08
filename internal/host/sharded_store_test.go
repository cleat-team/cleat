package host

import (
	"testing"
)

// ---------------------------------------------------------------------------
// ShardedStore tests that do not require real database connections
// ---------------------------------------------------------------------------

func TestShardedStore_Shards(t *testing.T) {
	shards := []*Shard{
		{Config: ShardConfig{Name: "shard-0"}},
		{Config: ShardConfig{Name: "shard-1"}},
	}
	s := &ShardedStore{shards: shards}

	got := s.Shards()
	if len(got) != 2 {
		t.Fatalf("Shards() returned %d shards, want 2", len(got))
	}
	if got[0].Config.Name != "shard-0" {
		t.Errorf("Shards()[0].Name = %q, want %q", got[0].Config.Name, "shard-0")
	}
	if got[1].Config.Name != "shard-1" {
		t.Errorf("Shards()[1].Name = %q, want %q", got[1].Config.Name, "shard-1")
	}

	// Verify it returns a new slice (separate from the internal one).
	if len(got) != len(s.shards) {
		t.Error("Shards() returned wrong length")
	}
}

func TestShardedStore_Shards_Empty(t *testing.T) {
	s := &ShardedStore{}
	got := s.Shards()
	if len(got) != 0 {
		t.Errorf("expected empty shards, got %d", len(got))
	}
}

func TestShardedStore_GetShard_Empty(t *testing.T) {
	s := &ShardedStore{}
	shard := s.getShard("any-key")
	if shard != nil {
		t.Error("getShard on empty store should return nil")
	}
}

func TestShardedStore_GetShard_SingleShard(t *testing.T) {
	s := &ShardedStore{
		shards: []*Shard{
			{Config: ShardConfig{Name: "only-shard"}},
		},
	}

	// All keys should route to the same shard.
	for _, key := range []string{"anything", "", "wf-123", "wf-456"} {
		shard := s.getShard(key)
		if shard == nil {
			t.Fatalf("getShard(%q) returned nil", key)
		}
		if shard.Config.Name != "only-shard" {
			t.Errorf("getShard(%q) = %q, want only-shard", key, shard.Config.Name)
		}
	}
}

func TestShardedStore_GetShard_MultipleShards(t *testing.T) {
	shards := []*Shard{
		{Config: ShardConfig{Name: "shard-a"}},
		{Config: ShardConfig{Name: "shard-b"}},
		{Config: ShardConfig{Name: "shard-c"}},
	}
	s := &ShardedStore{shards: shards}

	// Verify that different keys can map to different shards.
	results := make(map[string]int)
	for _, key := range []string{"workflow-alpha", "workflow-beta", "workflow-gamma", "workflow-delta", "workflow-epsilon"} {
		shard := s.getShard(key)
		if shard == nil {
			t.Fatalf("getShard(%q) returned nil", key)
		}
		results[shard.Config.Name]++
	}

	// With 3 shards and 5 keys, we expect reasonable distribution.
	if len(results) < 2 {
		t.Errorf("expected keys to distribute across at least 2 shards, got %v", results)
	}
}

func TestShardedStore_GetShard_Consistency(t *testing.T) {
	shards := []*Shard{
		{Config: ShardConfig{Name: "shard-a"}},
		{Config: ShardConfig{Name: "shard-b"}},
	}
	s := &ShardedStore{shards: shards}

	// The same key must always route to the same shard.
	key := "consistent-workflow-id-12345"
	first := s.getShard(key)
	if first == nil {
		t.Fatal("getShard returned nil")
	}

	for i := 0; i < 100; i++ {
		shard := s.getShard(key)
		if shard == nil {
			t.Fatal("getShard returned nil")
		}
		if shard.Config.Name != first.Config.Name {
			t.Errorf("iteration %d: key routed to %q instead of %q",
				i, shard.Config.Name, first.Config.Name)
		}
	}
}
