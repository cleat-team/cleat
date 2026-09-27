package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A start request naming a priority outside the operator's bound is refused,
// and one inside it starts the workflow.
//
// The accepted cases assert 201 rather than "not 400". The weaker form was
// tempting -- it survives the handler growing some other 400 -- but it also
// passes if the request is refused for a different reason entirely, and a test
// of a bound that passes when the request never reaches the bound is the shape
// this codebase keeps finding.
//
// The bound is checked before any database work, which is what makes this
// testable without one -- and is deliberate for its own sake: an out-of-range
// priority is wrong whether or not the workflow exists, and validating it after
// the version lookup would answer such a request with 404 when the name is also
// unknown.
func TestAStartRequestCannotSetAnUnboundedPriority(t *testing.T) {
	const bound = 1000

	start := func(t *testing.T, priority int) (int, string) {
		t.Helper()
		ms := &mockStore{}
		api := &apiServer{
			store:                ms,
			worker:               newTestWorker(ms),
			maxBodySize:          1 << 20,
			maxPriorityMagnitude: bound,
		}
		body := fmt.Sprintf(`{"input":{},"priority":%d}`, priority)
		req := httptest.NewRequest(http.MethodPost, "/api/workflows/d/start", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp := httptest.NewRecorder()
		api.handleStartWorkflow(resp, req, "d")
		var decoded map[string]string
		_ = json.Unmarshal(resp.Body.Bytes(), &decoded)
		return resp.Code, decoded["error"]
	}

	// refusedForPriority distinguishes "refused because of the number" from any
	// other 400 this handler may grow later.
	refusedForPriority := func(code int, msg string) bool {
		return code == http.StatusBadRequest && strings.Contains(msg, "priority")
	}

	t.Run("outside the bound is refused", func(t *testing.T) {
		// The int32 floor is the value that motivated the bound: ordered
		// `priority ASC` across tenants, it takes the front of every queue.
		for _, p := range []int{-(1 << 31), 1 << 30, bound + 1, -bound - 1} {
			code, msg := start(t, p)
			if !refusedForPriority(code, msg) {
				t.Errorf("priority %d: got %d %q; want 400 naming priority", p, code, msg)
			}
		}
	})

	t.Run("a negative priority inside the bound starts the workflow", func(t *testing.T) {
		// cleat#1051: negative is how work goes ahead of the default 0 without
		// renumbering. A bound implemented as priority >= 0 would pass the
		// subtest above and fail here, which is the regression worth catching.
		for _, p := range []int{-1, -500, -bound, 0, bound} {
			code, msg := start(t, p)
			if refusedForPriority(code, msg) {
				t.Errorf("priority %d: refused as out of range (%d %q); want accepted", p, code, msg)
				continue
			}
			if code != http.StatusCreated {
				t.Errorf("priority %d: got %d %q; want 201", p, code, msg)
			}
		}
	})
}

// With --max-priority-magnitude 0 the operator has set no bound, and the values
// the bound exists to stop are accepted again.
//
// Worth its own test because the disabling case is the one a deployment reaches
// by NOT setting the flag to something, and an implementation that treated 0 as
// "bound of zero" would refuse every non-zero priority on exactly those
// deployments.
func TestAPriorityBoundOfZeroLetsAnyValueThrough(t *testing.T) {
	ms := &mockStore{}
	api := &apiServer{
		store:                ms,
		worker:               newTestWorker(ms),
		maxBodySize:          1 << 20,
		maxPriorityMagnitude: 0,
	}
	body := fmt.Sprintf(`{"input":{},"priority":%d}`, -(1 << 31))
	req := httptest.NewRequest(http.MethodPost, "/api/workflows/d/start", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	api.handleStartWorkflow(resp, req, "d")
	var decoded map[string]string
	_ = json.Unmarshal(resp.Body.Bytes(), &decoded)
	if resp.Code == http.StatusBadRequest && strings.Contains(decoded["error"], "priority") {
		t.Fatalf("priority refused with no bound configured: %d %q", resp.Code, decoded["error"])
	}
	if resp.Code != http.StatusCreated {
		t.Fatalf("got %d %q; want 201 with no bound configured", resp.Code, decoded["error"])
	}
}
