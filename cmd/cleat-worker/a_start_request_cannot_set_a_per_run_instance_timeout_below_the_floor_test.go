package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A start request whose per-run wasm_instance_timeout_ms is above 0 and under 250 gets a 400 that says why (cleat#3922, owner decision
// 2026-11-18): the same floor, through the same function, as --wasm-instance-timeout and a tenant's setting. 250 and up, 0 and an
// absent field start the workflow, and a NEGATIVE value is still refused as before.
//
// Accepted cases assert 201, not "not 400": a test of a bound that passes when the request never reaches the bound is the shape this
// codebase keeps finding. Like TestAStartRequestCannotSetAnUnboundedPriority, the check precedes any database work, so no database.
func TestAStartRequestCannotSetAPerRunInstanceTimeoutBelowTheFloor(t *testing.T) {
	start := func(t *testing.T, field string) (int, string) {
		t.Helper()
		ms := &mockStore{}
		api := &apiServer{store: ms, worker: newTestWorker(ms), maxBodySize: 1 << 20}
		body := `{"input":{}` + field + `}`
		req := httptest.NewRequest(http.MethodPost, "/api/workflows/d/start", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp := httptest.NewRecorder()
		api.handleStartWorkflow(resp, req, "d")
		var decoded map[string]string
		_ = json.Unmarshal(resp.Body.Bytes(), &decoded)
		return resp.Code, decoded["error"]
	}

	for _, ms := range []int{1, 100, 249} {
		code, msg := start(t, fmt.Sprintf(`,"wasm_instance_timeout_ms":%d`, ms))
		if code != http.StatusBadRequest {
			t.Errorf("wasm_instance_timeout_ms=%d: got %d %q, want 400", ms, code, msg)
			continue
		}
		for _, want := range []string{"per-run wasm_instance_timeout_ms", fmt.Sprintf("%dms", ms), "250ms", "defers", "ABI.md"} {
			if !strings.Contains(msg, want) {
				t.Errorf("wasm_instance_timeout_ms=%d: the 400 does not contain %q: %q", ms, want, msg)
			}
		}
	}

	for name, field := range map[string]string{
		"250, the floor itself":   `,"wasm_instance_timeout_ms":250`,
		"30000":                   `,"wasm_instance_timeout_ms":30000`,
		"0, which is no override": `,"wasm_instance_timeout_ms":0`,
		"absent":                  ``,
		"a low OTHER bound":       `,"wasm_wall_clock_ceiling_ms":10,"host_retry_budget_ms":10`,
	} {
		if code, msg := start(t, field); code != http.StatusCreated {
			t.Errorf("%s: got %d %q, want 201", name, code, msg)
		}
	}

	// A negative value stays refused, and for its own reason, not the floor's.
	code, msg := start(t, `,"wasm_instance_timeout_ms":-5`)
	if code != http.StatusBadRequest || !strings.Contains(msg, "must be positive milliseconds") {
		t.Errorf("a negative value: got %d %q, want the existing 400 about positive milliseconds", code, msg)
	}
}
