package clewservice

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// setupTestServer creates an httptest.Server with the plugin's routes registered.
func setupTestServer(t *testing.T) (*httptest.Server, *Plugin) {
	t.Helper()
	p, _ := setupTestPlugin(t)
	mux := http.NewServeMux()
	if err := p.RegisterRoutes(mux); err != nil {
		t.Fatal(err)
	}
	return httptest.NewServer(mux), p
}

func TestSmokeHealthz(t *testing.T) {
	srv, _ := setupTestServer(t)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status %d, want 200", resp.StatusCode)
	}
	var body map[string]string
	json.NewDecoder(resp.Body).Decode(&body)
	if body["status"] != "ok" {
		t.Errorf("body %v, want status=ok", body)
	}
}

func TestSmokeListProjects(t *testing.T) {
	srv, _ := setupTestServer(t)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/projects")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status %d, want 200", resp.StatusCode)
	}
	var body map[string]any
	json.NewDecoder(resp.Body).Decode(&body)
	projects, ok := body["projects"].([]any)
	if !ok {
		t.Fatal("missing projects array")
	}
	if len(projects) < 1 {
		t.Errorf("got %d projects, want at least 1 (clew)", len(projects))
	}
}

func TestSmokeCreateProject(t *testing.T) {
	srv, _ := setupTestServer(t)
	defer srv.Close()

	body := strings.NewReader(`{"name":"smoke-proj"}`)
	resp, err := http.Post(srv.URL+"/api/projects", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		t.Errorf("status %d, want 201", resp.StatusCode)
	}
	var proj ProjectInfo
	json.NewDecoder(resp.Body).Decode(&proj)
	if proj.Name != "smoke-proj" {
		t.Errorf("name=%s, want smoke-proj", proj.Name)
	}
}

func TestSmokeCreateProjectDuplicate(t *testing.T) {
	srv, p := setupTestServer(t)
	defer srv.Close()

	// Pre-create a project directory.
	os.MkdirAll(p.projectRoot+"/projects/existing", 0755)

	body := strings.NewReader(`{"name":"existing"}`)
	resp, err := http.Post(srv.URL+"/api/projects", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusConflict {
		t.Errorf("status %d, want 409", resp.StatusCode)
	}
}

func TestSmokeCreateTask(t *testing.T) {
	srv, _ := setupTestServer(t)
	defer srv.Close()

	body := strings.NewReader(`{"id":"smoke-001","subject":"Smoke test task","priority":1}`)
	resp, err := http.Post(srv.URL+"/api/tasks", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status %d, want 201", resp.StatusCode)
	}
	var entry TaskEntry
	json.NewDecoder(resp.Body).Decode(&entry)
	if entry.ID != "smoke-001" {
		t.Errorf("id=%s, want smoke-001", entry.ID)
	}
	if entry.Status != "queued" {
		t.Errorf("status=%s, want queued", entry.Status)
	}
}

func TestSmokeCreateTaskInvalidID(t *testing.T) {
	srv, _ := setupTestServer(t)
	defer srv.Close()

	body := strings.NewReader(`{"id":"bad","subject":"Invalid","priority":1}`)
	resp, err := http.Post(srv.URL+"/api/tasks", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status %d, want 400", resp.StatusCode)
	}
}

func TestSmokeGetTask(t *testing.T) {
	srv, _ := setupTestServer(t)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/tasks/clew-001")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	var taskResp TaskDetailResponse
	json.NewDecoder(resp.Body).Decode(&taskResp)
	if taskResp.Task.ID != "clew-001" {
		t.Errorf("Task.ID=%s, want clew-001", taskResp.Task.ID)
	}
	if taskResp.Status.Phase != "queued" {
		t.Errorf("Status.Phase=%s, want queued", taskResp.Status.Phase)
	}
}

func TestSmokeGetTaskNotFound(t *testing.T) {
	srv, _ := setupTestServer(t)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/tasks/nope-001")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status %d, want 404", resp.StatusCode)
	}
	var errResp ErrorResponse
	json.NewDecoder(resp.Body).Decode(&errResp)
	if !strings.Contains(errResp.Error, "task not found") {
		t.Errorf("error=%q, want contains 'task not found'", errResp.Error)
	}
}

func TestSmokeSubmitResult(t *testing.T) {
	srv, _ := setupTestServer(t)
	defer srv.Close()

	body := strings.NewReader(`{"phase":"exploring","outcome":"pass","token_usage":{"input":1000,"output":500}}`)
	resp, err := http.Post(srv.URL+"/api/tasks/clew-001/result", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	var result map[string]any
	json.NewDecoder(resp.Body).Decode(&result)
	if result["phase"] != "exploring" {
		t.Errorf("phase=%v, want exploring", result["phase"])
	}
}

func TestSmokeSubmitResultInvalidTransition(t *testing.T) {
	srv, _ := setupTestServer(t)
	defer srv.Close()

	body := strings.NewReader(`{"phase":"planning","outcome":"pass"}`)
	resp, err := http.Post(srv.URL+"/api/tasks/clew-001/result", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status %d, want 400", resp.StatusCode)
	}
	var errResp ErrorResponse
	json.NewDecoder(resp.Body).Decode(&errResp)
	if !strings.Contains(errResp.Error, "invalid transition") {
		t.Errorf("error=%q, want contains 'invalid transition'", errResp.Error)
	}
}

func TestSmokeAgentHeartbeat(t *testing.T) {
	srv, p := setupTestServer(t)
	defer srv.Close()

	// Write a session.json so heartbeat has something to update.
	session := SessionJSON{TaskID: "clew-001", Status: "running", Role: "worker"}
	sessionData, _ := json.Marshal(session)
	os.WriteFile(p.projectRoot+"/task_state/clew-001/session.json", sessionData, 0644)

	body := strings.NewReader(`{"task_id":"clew-001","agent_id":"smoke-agent"}`)
	resp, err := http.Post(srv.URL+"/api/agent/heartbeat", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	var hb map[string]bool
	json.NewDecoder(resp.Body).Decode(&hb)
	if !hb["ok"] {
		t.Error("expected ok=true")
	}
}

func TestSmokeAgentPoll(t *testing.T) {
	srv, p := setupTestServer(t)
	defer srv.Close()

	// Remove session.json so clew-001 is eligible for polling.
	os.Remove(p.projectRoot + "/task_state/clew-001/session.json")

	resp, err := http.Get(srv.URL + "/api/agent/poll")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		t.Errorf("status %d, want 200 or 204", resp.StatusCode)
	}
	if resp.StatusCode == http.StatusOK {
		var poll PollResponse
		json.NewDecoder(resp.Body).Decode(&poll)
		if poll.TaskID == "" {
			t.Error("poll task_id is empty")
		}
	}
}

func TestSmokeDashboardSummary(t *testing.T) {
	srv, _ := setupTestServer(t)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/dashboard/summary")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status %d, want 200", resp.StatusCode)
	}
	var summary DashboardSummary
	json.NewDecoder(resp.Body).Decode(&summary)
	if summary.TotalTasks != 2 {
		t.Errorf("total_tasks=%d, want 2", summary.TotalTasks)
	}
}
