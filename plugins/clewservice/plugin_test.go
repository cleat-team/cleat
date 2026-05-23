package clewservice

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setupTestPlugin creates a Plugin pointed at a temp directory with basic structure.
func setupTestPlugin(t *testing.T) (*Plugin, string) {
	t.Helper()
	root := t.TempDir()

	// Create task_state structure.
	taskState := filepath.Join(root, "task_state")
	os.MkdirAll(filepath.Join(taskState, "lessons_learned"), 0755)
	os.MkdirAll(filepath.Join(root, "projects"), 0755)

	// Create a sample tasks.json.
	tasks := TasksJSON{
		Version: "1",
		Updated: Timestamp(),
		Tasks: map[string]TaskEntry{
			"clew-001": {
				ID:       "clew-001",
				Subject:  "Test task",
				Status:   "queued",
				Priority: 1,
				Cost:     TaskCost{BudgetUSD: 10, SpentUSD: 0},
				Created:  Timestamp(),
				Updated:  Timestamp(),
			},
			"clew-002": {
				ID:       "clew-002",
				Subject:  "Done task",
				Status:   "done",
				Priority: 2,
				Cost:     TaskCost{BudgetUSD: 20, SpentUSD: 15},
				Created:  Timestamp(),
				Updated:  Timestamp(),
			},
		},
	}
	writeTestTasksJSON(t, filepath.Join(taskState, "tasks.json"), &tasks)

	// Create a task directory for clew-001.
	os.MkdirAll(filepath.Join(taskState, "clew-001"), 0755)
	os.WriteFile(filepath.Join(taskState, "clew-001", "TASK.md"), []byte("# clew-001 — Test\n\n"), 0644)
	os.WriteFile(filepath.Join(taskState, "clew-001", "STATUS.md"), []byte("# Status\n\n**Phase:** queued\n"), 0644)
	os.WriteFile(filepath.Join(taskState, "clew-001", "session.json"), []byte(`{"task_id":"clew-001","status":"dispatched"}`), 0644)

	p := &Plugin{
		projectRoot:   root,
		newTaskScript: "/bin/true",
		logger:        slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}

	return p, root
}


// writeTestTasksJSON is a helper that writes tasks.json (not atomic, for test setup).
func writeTestTasksJSON(t *testing.T, path string, tasks *TasksJSON) {
	t.Helper()
	data, err := json.MarshalIndent(tasks, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
}

func setupTestMux(t *testing.T, p *Plugin) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	if err := p.RegisterRoutes(mux); err != nil {
		t.Fatal(err)
	}
	return mux
}

func TestHealthEndpoint(t *testing.T) {
	p, _ := setupTestPlugin(t)
	mux := setupTestMux(t, p)

	req := httptest.NewRequest("GET", "/healthz", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("GET /healthz: status %d, want 200", w.Code)
	}
	var body map[string]string
	json.NewDecoder(w.Body).Decode(&body)
	if body["status"] != "ok" {
		t.Errorf("GET /healthz: body %v, want status=ok", body)
	}
}

func TestAuthCheckUnauthenticated(t *testing.T) {
	p, _ := setupTestPlugin(t)
	mux := setupTestMux(t, p)

	req := httptest.NewRequest("GET", "/api/auth/check", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("GET /api/auth/check: status %d, want 200", w.Code)
	}
	var body map[string]any
	json.NewDecoder(w.Body).Decode(&body)
	if body["authenticated"] != false {
		t.Errorf("GET /api/auth/check without auth: authenticated=%v, want false", body["authenticated"])
	}
}

func TestProjectsGet(t *testing.T) {
	p, _ := setupTestPlugin(t)
	mux := setupTestMux(t, p)

	req := httptest.NewRequest("GET", "/api/projects", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("GET /api/projects: status %d, want 200", w.Code)
	}
	var body map[string]any
	json.NewDecoder(w.Body).Decode(&body)
	projects, ok := body["projects"].([]any)
	if !ok {
		t.Fatal("GET /api/projects: missing projects array")
	}
	if len(projects) != 1 {
		t.Errorf("GET /api/projects: got %d projects, want 1 (clew)", len(projects))
	}
}

func TestProjectsPost(t *testing.T) {
	p, _ := setupTestPlugin(t)
	mux := setupTestMux(t, p)

	body := strings.NewReader(`{"name":"test-project"}`)
	req := httptest.NewRequest("POST", "/api/projects", body)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Errorf("POST /api/projects: status %d, want 201", w.Code)
	}

	var proj ProjectInfo
	json.NewDecoder(w.Body).Decode(&proj)
	if proj.Name != "test-project" {
		t.Errorf("POST /api/projects: name=%s, want test-project", proj.Name)
	}
}

func TestProjectsPostDuplicate(t *testing.T) {
	p, root := setupTestPlugin(t)
	// Pre-create a project directory.
	os.MkdirAll(filepath.Join(root, "projects", "existing"), 0755)
	mux := setupTestMux(t, p)

	body := strings.NewReader(`{"name":"existing"}`)
	req := httptest.NewRequest("POST", "/api/projects", body)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Errorf("POST /api/projects duplicate: status %d, want 409", w.Code)
	}
}

func TestProjectsPostInvalidName(t *testing.T) {
	p, _ := setupTestPlugin(t)
	mux := setupTestMux(t, p)

	body := strings.NewReader(`{"name":"bad name!"}`)
	req := httptest.NewRequest("POST", "/api/projects", body)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("POST /api/projects invalid name: status %d, want 400", w.Code)
	}
}

func TestTasksGet(t *testing.T) {
	p, _ := setupTestPlugin(t)
	mux := setupTestMux(t, p)

	req := httptest.NewRequest("GET", "/api/tasks", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("GET /api/tasks: status %d, want 200", w.Code)
	}
	var tasks TasksJSON
	json.NewDecoder(w.Body).Decode(&tasks)
	if len(tasks.Tasks) != 2 {
		t.Errorf("GET /api/tasks: got %d tasks, want 2", len(tasks.Tasks))
	}
}

func TestTasksPost(t *testing.T) {
	p, _ := setupTestPlugin(t)
	mux := setupTestMux(t, p)

	body := strings.NewReader(`{"id":"clew-003","subject":"New task","priority":2}`)
	req := httptest.NewRequest("POST", "/api/tasks", body)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Errorf("POST /api/tasks: status %d, want 201", w.Code)
	}
	var entry TaskEntry
	json.NewDecoder(w.Body).Decode(&entry)
	if entry.ID != "clew-003" {
		t.Errorf("POST /api/tasks: id=%s, want clew-003", entry.ID)
	}
	if entry.Status != "queued" {
		t.Errorf("POST /api/tasks: status=%s, want queued", entry.Status)
	}
}

func TestTasksPostDuplicate(t *testing.T) {
	p, _ := setupTestPlugin(t)
	mux := setupTestMux(t, p)

	body := strings.NewReader(`{"id":"clew-001","subject":"Duplicate","priority":1}`)
	req := httptest.NewRequest("POST", "/api/tasks", body)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Errorf("POST /api/tasks duplicate: status %d, want 409", w.Code)
	}
}

func TestDashboardSummary(t *testing.T) {
	p, _ := setupTestPlugin(t)
	mux := setupTestMux(t, p)

	req := httptest.NewRequest("GET", "/api/dashboard/summary", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("GET /api/dashboard/summary: status %d, want 200", w.Code)
	}
	var summary DashboardSummary
	json.NewDecoder(w.Body).Decode(&summary)
	if summary.TotalTasks != 2 {
		t.Errorf("GET /api/dashboard/summary: total_tasks=%d, want 2", summary.TotalTasks)
	}
	if summary.TasksByStatus["queued"] != 1 {
		t.Errorf("GET /api/dashboard/summary: queued count=%d, want 1", summary.TasksByStatus["queued"])
	}
	if summary.TasksByStatus["done"] != 1 {
		t.Errorf("GET /api/dashboard/summary: done count=%d, want 1", summary.TasksByStatus["done"])
	}
	if summary.TotalBudgetUSD != 30 {
		t.Errorf("GET /api/dashboard/summary: total_budget_usd=%f, want 30", summary.TotalBudgetUSD)
	}
	if summary.TotalSpentUSD != 15 {
		t.Errorf("GET /api/dashboard/summary: total_spent_usd=%f, want 15", summary.TotalSpentUSD)
	}
	if len(summary.RecentActivity) != 2 {
		t.Errorf("GET /api/dashboard/summary: recent_activity count=%d, want 2", len(summary.RecentActivity))
	}
}

func TestLessonsGet(t *testing.T) {
	p, root := setupTestPlugin(t)
	// Create a lesson file.
	os.MkdirAll(filepath.Join(root, "task_state", "lessons_learned"), 0755)
	os.WriteFile(filepath.Join(root, "task_state", "lessons_learned", "test-lesson.md"), []byte("# Lesson"), 0644)
	mux := setupTestMux(t, p)

	req := httptest.NewRequest("GET", "/api/lessons", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("GET /api/lessons: status %d, want 200", w.Code)
	}
	var body map[string]any
	json.NewDecoder(w.Body).Decode(&body)
	lessons, _ := body["lessons"].([]any)
	if len(lessons) != 1 {
		t.Errorf("GET /api/lessons: got %d lessons, want 1", len(lessons))
	}
}

func TestAtomicWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	data := []byte("hello world")

	if err := atomicWrite(path, data, 0644); err != nil {
		t.Fatalf("atomicWrite: %v", err)
	}

	read, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(read) != "hello world" {
		t.Errorf("read back: got %q, want %q", read, data)
	}

	// Verify no temp file was left behind.
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Error("temp file was not cleaned up")
	}
}

func TestPathSafety(t *testing.T) {
	p := &Plugin{projectRoot: "/tmp/clew"}

	// Valid path.
	got, err := p.safePath("task_state/clew-001")
	if err != nil {
		t.Errorf("safePath valid: unexpected error: %v", err)
	}
	if !strings.HasPrefix(got, "/tmp/clew") {
		t.Errorf("safePath valid: %s doesn't have prefix /tmp/clew", got)
	}

	// Path traversal attempt.
	_, err = p.safePath("../../etc/passwd")
	if err == nil {
		t.Error("safePath traversal: expected error, got nil")
	}
}

func TestPhaseTransitions(t *testing.T) {
	tests := []struct {
		from, to string
		want     bool
	}{
		{"queued", "exploring", true},
		{"exploring", "planning", true},
		{"planning", "plan_review", true},
		{"plan_review", "implementing", true},
		{"implementing", "impl_review", true},
		{"impl_review", "done", true},
		// Skipping phases.
		{"queued", "implementing", false},
		{"exploring", "plan_review", false},
		// Terminal states.
		{"done", "failed", true},
		{"failed", "queued", true},
		{"blocked", "exploring", true},
		// From any phase to terminal.
		{"queued", "failed", true},
		{"planning", "blocked", true},
		// Invalid target.
		{"queued", "bogus", false},
	}

	for _, tc := range tests {
		got := CanTransition(tc.from, tc.to)
		if got != tc.want {
			t.Errorf("CanTransition(%s, %s) = %v, want %v", tc.from, tc.to, got, tc.want)
		}
	}
}

func TestTaskGetNotFound(t *testing.T) {
	p, _ := setupTestPlugin(t)
	mux := setupTestMux(t, p)

	req := httptest.NewRequest("GET", "/api/tasks/clew-999", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("GET /api/tasks/clew-999: status %d, want 404", w.Code)
	}
}

func TestTaskGetFound(t *testing.T) {
	p, _ := setupTestPlugin(t)
	mux := setupTestMux(t, p)

	req := httptest.NewRequest("GET", "/api/tasks/clew-001", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("GET /api/tasks/clew-001: status %d, want 200", w.Code)
	}
	var detail TaskDetailResponse
	json.NewDecoder(w.Body).Decode(&detail)
	if detail.Task.ID != "clew-001" {
		t.Errorf("GET /api/tasks/clew-001: task.id=%s, want clew-001", detail.Task.ID)
	}
	if detail.Status.Phase != "queued" {
		t.Errorf("GET /api/tasks/clew-001: status.phase=%s, want queued", detail.Status.Phase)
	}
	if detail.Session == nil {
		t.Error("GET /api/tasks/clew-001: session is nil, expected session data")
	}
}

func TestTaskContentGet(t *testing.T) {
	p, _ := setupTestPlugin(t)
	mux := setupTestMux(t, p)

	req := httptest.NewRequest("GET", "/api/tasks/clew-001/content?file=TASK.md", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("GET /api/tasks/clew-001/content: status %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "clew-001") {
		t.Errorf("GET content: body doesn't contain task ID: %q", w.Body.String())
	}
}

func TestTaskContentGetInvalidFile(t *testing.T) {
	p, _ := setupTestPlugin(t)
	mux := setupTestMux(t, p)

	req := httptest.NewRequest("GET", "/api/tasks/clew-001/content?file=/etc/passwd", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("GET /api/tasks/clew-001/content invalid file: status %d, want 400", w.Code)
	}
}

func TestTaskResultPost_ValidTransition(t *testing.T) {
	p, _ := setupTestPlugin(t)
	mux := setupTestMux(t, p)

	body := strings.NewReader(`{"phase":"exploring","outcome":"starting"}`)
	req := httptest.NewRequest("POST", "/api/tasks/clew-001/result", body)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("POST /api/tasks/clew-001/result: status %d, want 200", w.Code)
	}
}

func TestTaskResultPost_InvalidTransition(t *testing.T) {
	p, _ := setupTestPlugin(t)
	mux := setupTestMux(t, p)

	body := strings.NewReader(`{"phase":"implementing","outcome":"skip"}`)
	req := httptest.NewRequest("POST", "/api/tasks/clew-001/result", body)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("POST /api/tasks/clew-001/result skip: status %d, want 400", w.Code)
	}
}

func TestAgentPoll(t *testing.T) {
	p, _ := setupTestPlugin(t)
	mux := setupTestMux(t, p)

	req := httptest.NewRequest("GET", "/api/agent/poll", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("GET /api/agent/poll: status %d, want 200", w.Code)
	}
	var resp PollResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.TaskID != "clew-001" {
		t.Errorf("GET /api/agent/poll: task_id=%s, want clew-001", resp.TaskID)
	}
	if resp.Priority != 1 {
		t.Errorf("GET /api/agent/poll: priority=%d, want 1", resp.Priority)
	}
}

func TestAgentPollNoTasks(t *testing.T) {
	p, root := setupTestPlugin(t)
	// Remove all queued tasks.
	tasks := TasksJSON{
		Version: "1",
		Updated: Timestamp(),
		Tasks: map[string]TaskEntry{
			"clew-002": {
				ID:      "clew-002",
				Status:  "done",
				Created: Timestamp(),
				Updated: Timestamp(),
			},
		},
	}
	writeTestTasksJSON(t, filepath.Join(root, "task_state", "tasks.json"), &tasks)
	mux := setupTestMux(t, p)

	req := httptest.NewRequest("GET", "/api/agent/poll", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Errorf("GET /api/agent/poll no tasks: status %d, want 204", w.Code)
	}
}

func TestAgentHeartbeat(t *testing.T) {
	p, _ := setupTestPlugin(t)
	mux := setupTestMux(t, p)

	body := strings.NewReader(`{"task_id":"clew-001","agent_id":"test-agent"}`)
	req := httptest.NewRequest("POST", "/api/agent/heartbeat", body)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("POST /api/agent/heartbeat: status %d, want 200", w.Code)
	}

	// Verify heartbeat was written.
	session, _ := p.readSessionJSON("clew-001")
	if session.HeartbeatAt == "" {
		t.Error("heartbeat_at was not written")
	}
	if session.AgentID != "test-agent" {
		t.Errorf("agent_id=%s, want test-agent", session.AgentID)
	}
}

func TestTaskLogsGet(t *testing.T) {
	p, root := setupTestPlugin(t)
	os.MkdirAll(filepath.Join(root, "task_state", "clew-001", "logs"), 0755)
	os.WriteFile(filepath.Join(root, "task_state", "clew-001", "logs", "2026-05-23.md"), []byte("test"), 0644)
	mux := setupTestMux(t, p)

	req := httptest.NewRequest("GET", "/api/tasks/clew-001/logs", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("GET /api/tasks/clew-001/logs: status %d, want 200", w.Code)
	}
	var body map[string]any
	json.NewDecoder(w.Body).Decode(&body)
	logs, _ := body["logs"].([]any)
	if len(logs) != 1 {
		t.Errorf("GET /api/tasks/clew-001/logs: got %d logs, want 1", len(logs))
	}
}

func TestTaskArtifactsGet(t *testing.T) {
	p, root := setupTestPlugin(t)
	os.MkdirAll(filepath.Join(root, "task_state", "clew-001", "artifacts"), 0755)
	os.WriteFile(filepath.Join(root, "task_state", "clew-001", "artifacts", "plan.md"), []byte("test"), 0644)
	mux := setupTestMux(t, p)

	req := httptest.NewRequest("GET", "/api/tasks/clew-001/artifacts", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("GET /api/tasks/clew-001/artifacts: status %d, want 200", w.Code)
	}
	var body map[string]any
	json.NewDecoder(w.Body).Decode(&body)
	artifacts, _ := body["artifacts"].([]any)
	if len(artifacts) != 1 {
		t.Errorf("GET /api/tasks/clew-001/artifacts: got %d artifacts, want 1", len(artifacts))
	}
}
