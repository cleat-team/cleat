package pgvector

import "testing"

// cleat#2487. New had no test calling it directly -- TestInfo (above)
// constructs a *Plugin by literal instead.
func TestNewReturnsAUsablePlugin(t *testing.T) {
	p := New()
	if p == nil {
		t.Fatal("New() returned nil")
	}
	if _, ok := p.(*Plugin); !ok {
		t.Fatalf("New() returned %T, want *Plugin", p)
	}
}
