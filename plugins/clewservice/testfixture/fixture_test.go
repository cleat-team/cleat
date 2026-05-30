package testfixture

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestSmokeAllEndpoints(t *testing.T) {
	f := New(t)
	defer f.Close()

	type testCase struct {
		method  string
		path    string
		body    string
		wantAny []int // acceptable status codes
	}

	cases := []testCase{
		// --- System (2 endpoints) ---
		{"GET", "/healthz", "", []int{200}},
		{"GET", "/api/auth/check", "", []int{200}},

		// --- Projects (2 endpoints, 4 sub-tests) ---
		{"GET", "/api/projects", "", []int{200}},
		{"POST", "/api/projects", `{"name":"smoke"}`, []int{201}},
		{"POST", "/api/projects", `{"name":"bad name!"}`, []int{400}},
		{"POST", "/api/projects", `{"name":"smoke"}`, []int{409}},

		// --- Tasks (1 endpoint, 3 sub-tests) ---
		{"POST", "/api/tasks", `{"id":"smoke-001","subject":"Smoke","priority":1}`, []int{201}},
		{"POST", "/api/tasks", `{"id":"smoke-001","subject":"Dup","priority":1}`, []int{409}},
		{"POST", "/api/tasks", `{"id":"bad","subject":"Bad ID","priority":1}`, []int{400}},

		// --- Task detail (GET /api/tasks/{id}) ---
		{"GET", "/api/tasks/smoke-001", "", []int{200}},
		// --- Result submission (POST /api/tasks/{id}/result) ---
		// Valid transition queued->exploring; STATUS.md exists from task creation.
		{"POST", "/api/tasks/smoke-001/result", `{"phase":"exploring","outcome":"pass"}`, []int{200}},
		// --- Agent heartbeat (POST /api/agent/heartbeat) ---
		// No session.json seeded -> readSessionJSON fails -> 404.
		{"POST", "/api/agent/heartbeat", `{"task_id":"smoke-001","agent_id":"agent-1"}`, []int{404}},
		// --- Agent poll (GET /api/agent/poll) ---
		// smoke-001 transitioned to "exploring" by the result POST above -> no queued tasks remain.
		{"GET", "/api/agent/poll", "", []int{204}},

		// --- Dashboard (1 endpoint) ---
		{"GET", "/api/dashboard/summary", "", []int{200}},
	}

	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			var body io.Reader
			if tc.body != "" {
				body = strings.NewReader(tc.body)
			}
			req, err := http.NewRequest(tc.method, f.URL+tc.path, body)
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			if tc.body != "" {
				req.Header.Set("Content-Type", "application/json")
			}

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("do request: %v", err)
			}
			defer resp.Body.Close()

			// Status code must be in the acceptable set.
			ok := false
			for _, code := range tc.wantAny {
				if resp.StatusCode == code {
					ok = true
					break
				}
			}
			if !ok {
				t.Errorf("status %d not in acceptable range %v", resp.StatusCode, tc.wantAny)
			}

			// Content-Type must be application/json (except 204 no-body responses).
			if resp.StatusCode != 204 {
				ct := resp.Header.Get("Content-Type")
				if ct != "" && !strings.Contains(ct, "application/json") {
					t.Errorf("Content-Type=%q, want application/json", ct)
				}
				// Body must be parseable JSON.
				var v any
				if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
					t.Errorf("body is not valid JSON: %v", err)
				}
			}
		})
	}
}
