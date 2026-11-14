package engine

// cleat#3785. ShardedStore.GetWorkflowDefExposure sends the narrow read to the shard that holds the
// definition's name, and answers the same question the wide way where that shard's store cannot answer the
// narrow one. Neither needs a database: the shards are stubs that record what they were asked.

import (
	"context"
	"testing"
)

type narrowShard struct {
	WorkflowStore
	class ExposureClass
	asked []string
}

func (n *narrowShard) GetWorkflowDefExposure(_ context.Context, name string, _ int) (ExposureClass, bool, error) {
	n.asked = append(n.asked, name)
	return n.class, true, nil
}

type wideShard struct {
	WorkflowStore
	def   *WorkflowDef
	asked []string
}

func (w *wideShard) GetWorkflowDef(_ context.Context, name string, _ int) (*WorkflowDef, error) {
	w.asked = append(w.asked, name)
	return w.def, nil
}

func TestAShardedExposureReadIsAnsweredByTheShardThatHoldsTheName(t *testing.T) {
	ctx := context.Background()
	narrow := &narrowShard{class: ExposureInternal}
	sharded, err := NewShardedStore([]ShardConfig{{Name: "only"}}, []WorkflowStore{narrow},
		[]func() error{func() error { return nil }})
	if err != nil {
		t.Fatalf("NewShardedStore: %v", err)
	}
	class, found, err := sharded.GetWorkflowDefExposure(ctx, "wf", 1)
	if err != nil || !found || class != ExposureInternal || len(narrow.asked) != 1 {
		t.Errorf("= (%q, %v, %v) after %d narrow reads; want (internal, true, nil) after exactly 1", class, found, err, len(narrow.asked))
	}

	for _, c := range []struct {
		name  string
		def   *WorkflowDef
		class ExposureClass
		found bool
	}{
		{"a shard that cannot answer the narrow question answers it from the whole definition", &WorkflowDef{Exposure: ExposurePublic}, ExposurePublic, true},
		{"and an absent definition is not found, not an error", nil, "", false},
	} {
		wide := &wideShard{def: c.def}
		sharded, err := NewShardedStore([]ShardConfig{{Name: "only"}}, []WorkflowStore{wide},
			[]func() error{func() error { return nil }})
		if err != nil {
			t.Fatalf("NewShardedStore: %v", err)
		}
		class, found, err := sharded.GetWorkflowDefExposure(ctx, "wf", 1)
		if err != nil || found != c.found || class != c.class || len(wide.asked) != 1 {
			t.Errorf("%s: = (%q, %v, %v) after %d wide reads; want (%q, %v, nil) after 1", c.name, class, found, err, len(wide.asked), c.class, c.found)
		}
	}
}
