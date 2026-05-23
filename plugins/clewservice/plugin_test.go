package clewservice

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

func TestConcurrentWrites(t *testing.T) {
	p, _ := setupTestPlugin(t)
	mux := setupTestMux(t, p)

	var wg sync.WaitGroup
	errs := make(chan error, 5)

	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			body := strings.NewReader(
				fmt.Sprintf(`{"id":"clew-conc-%03d","subject":"Concurrent %d","priority":2}`, n, n))
			req := httptest.NewRequest("POST", "/api/tasks", body)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)
			if w.Code != http.StatusCreated {
				errs <- fmt.Errorf("task %d: status %d", n, w.Code)
			}
		}(i)
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Error(err)
	}
}

// setupTestPluginWithDeps creates a Plugin with tasks that have dependencies.
func setupTestPluginWithDeps(t *testing.T) (*Plugin, string) {
	t.Helper()
	root := t.TempDir()

	taskState := filepath.Join(root, "task_state")
	os.MkdirAll(filepath.Join(taskState, "lessons_learned"), 0755)
	os.MkdirAll(filepath.Join(root, "projects"), 0755)

	tasks := TasksJSON{
		Version: "1",
		Updated: Timestamp(),
		Tasks: map[string]TaskEntry{
			"clew-001": {
				ID: "clew-001", Subject: "Task 1", Status: "queued",
				Priority: 5, Cost: TaskCost{BudgetUSD: 10},
				DependsOn: []string{}, Children: []string{},
				Created: Timestamp(), Updated: Timestamp(),
			},
			"clew-002": {
				ID: "clew-002", Subject: "Task 2", Status: "done",
				Priority: 2, Cost: TaskCost{BudgetUSD: 20},
				DependsOn: []string{}, Children: []string{},
				Created: Timestamp(), Updated: Timestamp(),
			},
			"clew-003": {
				ID: "clew-003", Subject: "Task 3", Status: "queued",
				Priority: 3, DependsOn: []string{"clew-001"},
				Cost: TaskCost{BudgetUSD: 10},
				Children: []string{},
				Created: Timestamp(), Updated: Timestamp(),
			},
			"clew-004": {
				ID: "clew-004", Subject: "Task 4", Status: "queued",
				Priority: 4, DependsOn: []string{"clew-002"},
				Cost: TaskCost{BudgetUSD: 10},
				Children: []string{},
				Created: Timestamp(), Updated: Timestamp(),
			},
		},
	}
	writeTestTasksJSON(t, filepath.Join(taskState, "tasks.json"), &tasks)

	for _, id := range []string{"clew-001", "clew-003", "clew-004"} {
		dir := filepath.Join(taskState, id)
		os.MkdirAll(dir, 0755)
		os.WriteFile(filepath.Join(dir, "TASK.md"), []byte("# "+id+" — Test\n\n"), 0644)
		os.WriteFile(filepath.Join(dir, "STATUS.md"), []byte("# Status\n\n**Phase:** queued\n"), 0644)
	}

	p := &Plugin{
		projectRoot:   root,
		newTaskScript: "/bin/true",
		logger:        slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}
	return p, root
}

func TestDispatchDependencyCheck(t *testing.T) {
	p, _ := setupTestPluginWithDeps(t)
	mux := setupTestMux(t, p)

	// Dispatch task with unsatisfied dep (clew-003 depends on clew-001 which is queued).
	body := strings.NewReader(`{"role":"cto","tool":"claude"}`)
	req := httptest.NewRequest("POST", "/api/tasks/clew-003/dispatch", body)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("dispatch with unsatisfied dep: status %d, want 400", w.Code)
	}

	// Dispatch task with satisfied dep (clew-004 depends on clew-002 which is done).
	body2 := strings.NewReader(`{"role":"worker","tool":"claude"}`)
	req2 := httptest.NewRequest("POST", "/api/tasks/clew-004/dispatch", body2)
	w2 := httptest.NewRecorder()
	mux.ServeHTTP(w2, req2)
	if w2.Code != http.StatusCreated {
		t.Errorf("dispatch with satisfied dep: status %d, want 201 (body: %s)", w2.Code, w2.Body.String())
	}
}

func TestPollOrderingDepSkip(t *testing.T) {
	p, _ := setupTestPluginWithDeps(t)
	mux := setupTestMux(t, p)

	// clew-001 (priority 5) and clew-004 (priority 4) are eligible queued tasks.
	// clew-003 has unsatisfied dep on clew-001 and should be skipped.
	// clew-004 has lower priority number so should be returned.
	req := httptest.NewRequest("GET", "/api/agent/poll", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("poll with mixed deps: status %d, want 200", w.Code)
	}
	var resp PollResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.TaskID != "clew-004" {
		t.Errorf("poll returned %s, want clew-004 (lowest priority among eligible)", resp.TaskID)
	}
	if resp.Priority != 4 {
		t.Errorf("poll priority=%d, want 4", resp.Priority)
	}
}

func TestPollNoEligibleTasks(t *testing.T) {
	p, root := setupTestPluginWithDeps(t)

	// Change clew-001 and clew-004 to non-queued status, leaving only
	// clew-003 which has unsatisfied depends_on.
	tasksPath := filepath.Join(root, "task_state", "tasks.json")
	data, _ := os.ReadFile(tasksPath)
	var tasks TasksJSON
	json.Unmarshal(data, &tasks)
	for _, id := range []string{"clew-001", "clew-004"} {
		e := tasks.Tasks[id]
		e.Status = "exploring"
		tasks.Tasks[id] = e
	}
	writeTestTasksJSON(t, tasksPath, &tasks)

	mux := setupTestMux(t, p)
	req := httptest.NewRequest("GET", "/api/agent/poll", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Errorf("poll no eligible tasks: status %d, want 204", w.Code)
	}
}

func TestContentType(t *testing.T) {
	p, _ := setupTestPlugin(t)
	mux := setupTestMux(t, p)

	tests := []struct {
		file, wantCT string
	}{
		{"TASK.md", "text/markdown; charset=utf-8"},
		{"STATUS.md", "text/markdown; charset=utf-8"},
		{"session.json", "application/json"},
	}

	for _, tc := range tests {
		req := httptest.NewRequest("GET", "/api/tasks/clew-001/content?file="+tc.file, nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("content %s: status %d, want 200", tc.file, w.Code)
			continue
		}
		got := w.Header().Get("Content-Type")
		if got != tc.wantCT {
			t.Errorf("content %s: Content-Type=%q, want %q", tc.file, got, tc.wantCT)
		}
	}
}

func TestStatusMDPreservation(t *testing.T) {
	p, root := setupTestPlugin(t)

	// Write rich STATUS.md with review content that should survive a phase update.
	richStatus := []byte("# Status — clew-001\n\n**Phase:** queued\n\n**Review Round 1:** PASS\n**Review Round 2:** SHOULD_FIX\n\nSome notes here.\n")
	os.WriteFile(filepath.Join(root, "task_state", "clew-001", "STATUS.md"), richStatus, 0644)

	mux := setupTestMux(t, p)

	// Submit result to transition queued → exploring.
	body := strings.NewReader(`{"phase":"exploring","outcome":"starting"}`)
	req := httptest.NewRequest("POST", "/api/tasks/clew-001/result", body)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("result post: status %d, want 200 (body: %s)", w.Code, w.Body.String())
	}

	// Read back STATUS.md and verify rich content was preserved.
	statusData, err := os.ReadFile(filepath.Join(root, "task_state", "clew-001", "STATUS.md"))
	if err != nil {
		t.Fatal(err)
	}
	statusStr := string(statusData)
	if !strings.Contains(statusStr, "Review Round 1: PASS") {
		t.Errorf("STATUS.md did not preserve Review Round 1: %s", statusStr)
	}
	if !strings.Contains(statusStr, "Review Round 2: SHOULD_FIX") {
		t.Errorf("STATUS.md did not preserve Review Round 2: %s", statusStr)
	}
	if !strings.Contains(statusStr, "Some notes here") {
		t.Errorf("STATUS.md did not preserve notes: %s", statusStr)
	}
	if !strings.Contains(statusStr, "**Phase:** exploring") {
		t.Errorf("STATUS.md did not update phase to exploring: %s", statusStr)
	}
}

func TestResultPhaseFromStatusMD(t *testing.T) {
	p, root := setupTestPlugin(t)

	// Modify tasks.json: set clew-001 status to "planning"
	// (which would allow plan_review if it were the source of truth).
	// But STATUS.md still says "queued", which should NOT allow plan_review.
	tasksPath := filepath.Join(root, "task_state", "tasks.json")
	data, _ := os.ReadFile(tasksPath)
	var tasks TasksJSON
	json.Unmarshal(data, &tasks)
	entry := tasks.Tasks["clew-001"]
	entry.Status = "planning"
	tasks.Tasks["clew-001"] = entry
	writeTestTasksJSON(t, tasksPath, &tasks)

	// STATUS.md says "queued", so queued→plan_review should be rejected (skip).
	os.WriteFile(filepath.Join(root, "task_state", "clew-001", "STATUS.md"),
		[]byte("# Status\n\n**Phase:** queued\n"), 0644)

	mux := setupTestMux(t, p)

	// Try invalid transition: STATUS.md queued → plan_review (skip).
	body := strings.NewReader(`{"phase":"plan_review","outcome":"skip"}`)
	req := httptest.NewRequest("POST", "/api/tasks/clew-001/result", body)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("invalid transition (STATUS.md queued→plan_review): status %d, want 400 (body: %s)",
			w.Code, w.Body.String())
	}

	// Try valid transition: STATUS.md queued → exploring.
	body2 := strings.NewReader(`{"phase":"exploring","outcome":"starting"}`)
	req2 := httptest.NewRequest("POST", "/api/tasks/clew-001/result", body2)
	w2 := httptest.NewRecorder()
	mux.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Errorf("valid transition (STATUS.md queued→exploring): status %d, want 200 (body: %s)",
			w2.Code, w2.Body.String())
	}
}
