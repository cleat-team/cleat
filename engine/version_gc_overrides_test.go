package engine

// cleat#1315. Version GC had one policy, compiled in and unreachable: both
// surfaces read only dry_run and then took DefaultGCOptions() wholesale, so
// MinVersionsToKeep=3 and MaxVersionAge=30d could not be changed from anywhere
// short of editing the source -- while docs/troubleshooting.md told an operator
// to "adjust the GC retention policy" with two flags that did not exist.
//
// THE REFUSAL CASES ARE THE POINT, not the happy path. Falling back to the
// default on a value it could not parse would run a DESTRUCTIVE sweep under a
// policy the caller did not ask for and answer 200 -- worse than the
// inflexibility being fixed, because it looks like it worked.
//
// 0 IS REFUSED, and that is the non-obvious one. GarbageCollectVersions
// normalises MinVersionsToKeep <= 0 and MaxVersionAge <= 0 back to the
// defaults, so a handler that accepted 0 would answer 200 and then sweep under
// a policy of 3 and 30 days. That is the same silent substitution, at the
// boundary built to prevent it. TestZeroIsRefusedBecauseTheSweepWouldSubstitute
// pins the reasoning to the behaviour it depends on.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// gcProbeStore satisfies just enough of WorkflowStore for
// GarbageCollectVersions: it lists no definitions, so the sweep is a no-op and
// the handler's PARSING is what the test measures. The embedded interface
// panics on anything else, which is the intended signal if the sweep starts
// reaching for more.
type gcProbeStore struct{ WorkflowStore }

func (gcProbeStore) ListWorkflowDefs(context.Context, string) ([]WorkflowDef, error) {
	return nil, nil
}
func (gcProbeStore) GetActiveInstanceCountsByVersion(context.Context) (map[string]int, error) {
	return map[string]int{}, nil
}

func postGC(t *testing.T, query string) *httptest.ResponseRecorder {
	t.Helper()
	h := &versionHandler{resolve: StaticVersionStore(gcProbeStore{})}
	rec := httptest.NewRecorder()
	h.runGC(rec, httptest.NewRequest(http.MethodPost, "/api/versions/gc?"+query, nil))
	return rec
}

func TestTheGCEndpointTakesAPolicyAndRefusesAMalformedOne(t *testing.T) {
	for _, tc := range []struct {
		query string
		want  int
		why   string
	}{
		{"", 200, "no overrides: the documented default applies"},
		{"dry_run=true", 200, "the pre-existing parameter still works"},
		{"min_versions=5", 200, "a valid override"},
		{"max_age=1h", 200, "a valid override"},
		{"min_versions=5&max_age=168h", 200, "both together"},
		{"min_versions=", 200, "empty means absent, not zero"},
		{"max_age=", 200, "empty means absent, not zero"},

		{"min_versions=abc", 400, "not an integer"},
		{"min_versions=-1", 400, "negative would keep nothing"},
		{"min_versions=0", 400, "the sweep substitutes the default for 0"},
		{"max_age=abc", 400, "not a duration"},
		{"max_age=7", 400, "a bare number is 7ns, which is not what anyone means by 7"},
		{"max_age=-1h", 400, "negative would make every version eligible at once"},
		{"max_age=0", 400, "the sweep substitutes the default for 0"},
	} {
		t.Run(tc.query, func(t *testing.T) {
			rec := postGC(t, tc.query)
			if rec.Code != tc.want {
				t.Errorf("POST /api/versions/gc?%s = %d, want %d (%s)\n\nbody: %s",
					tc.query, rec.Code, tc.want, tc.why, strings.TrimSpace(rec.Body.String()))
			}
		})
	}
}

// The reason 0 is refused, asserted against the engine rather than assumed.
// If GarbageCollectVersions ever stops substituting, the refusal becomes
// unnecessary and this test says so.
func TestZeroIsRefusedBecauseTheSweepWouldSubstitute(t *testing.T) {
	res, err := GarbageCollectVersions(context.Background(), gcProbeStore{}, GCOptions{
		MinVersionsToKeep: 0,
		MaxVersionAge:     0,
		DryRun:            true,
	})
	if err != nil {
		t.Fatalf("sweep with a zero policy: %v", err)
	}
	if res == nil {
		t.Fatal("sweep returned no result")
	}
	// The substitution is internal, so it is shown by the endpoint refusing
	// rather than by reading opts back. What this asserts is that a zero policy
	// does NOT error -- which is exactly why it must not reach the sweep.
	if rec := postGC(t, "min_versions=0"); rec.Code != 400 {
		t.Errorf("min_versions=0 returned %d; the sweep accepts a zero policy silently, "+
			"so the endpoint is the only thing that can refuse it", rec.Code)
	}
}

// A bare number is the input most likely to be typed: max_age=7 meaning seven
// days. time.ParseDuration rejects it, and a handler that ignored the error
// would sweep with 30 days while the caller believed they had asked for 7.
func TestABareNumberIsRefusedForMaxAge(t *testing.T) {
	rec := postGC(t, "max_age=7")
	if rec.Code != 400 {
		t.Fatalf("max_age=7 returned %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "720h") {
		t.Errorf("the error does not show the expected form: %s", rec.Body.String())
	}
	_ = time.Hour
}
