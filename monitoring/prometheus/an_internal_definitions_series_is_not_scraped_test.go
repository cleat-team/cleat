package prometheus

// cleat#3001: an `internal` definition's NAME must not reach the /metrics
// scrape. The series are process-global while the class is per-tenant, so the
// worker notes the names it has learned (via NoteInternalDefs) and ServeHTTP
// drops their series before rendering -- the one place that decides what this
// endpoint may say.

import (
	"context"
	"strings"
	"testing"
)

func TestAnInternalDefinitionsSeriesIsNotScraped(t *testing.T) {
	m, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// Two definitions, one internal. Record both, then tell the filter which is
	// which -- the order the worker uses.
	m.RecordWorkflowStarted(ctx, "secret-def")
	m.RecordWorkflowStarted(ctx, "public-def")
	m.NoteInternalDefs("secret-def")

	body := scrape(t, m)

	if strings.Contains(body, "secret-def") {
		t.Errorf("an `internal` definition's name reached the scrape:\n%s", body)
	}
	// The control, so the assertion above cannot pass by dropping everything:
	// a non-internal series must survive.
	if !strings.Contains(body, "public-def") {
		t.Errorf("a non-internal definition was dropped, so the filter is not keyed on the class:\n%s", body)
	}
}

// NoteInternalDefs is additive and idempotent: marking the same name twice, or
// marking when no series exists for it, must not panic or resurrect a dropped
// series -- the worker marks on every refresh.
func TestNotingAnInternalDefinitionTwiceIsHarmless(t *testing.T) {
	m, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	m.RecordWorkflowStarted(context.Background(), "secret-def")
	m.NoteInternalDefs("secret-def")
	m.NoteInternalDefs("secret-def", "")

	if body := scrape(t, m); strings.Contains(body, "secret-def") {
		t.Errorf("re-marking let an internal name back into the scrape:\n%s", body)
	}
}
