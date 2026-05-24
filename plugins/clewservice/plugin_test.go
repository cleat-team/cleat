package clewservice

import (
	"context"
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

	"github.com/cleat-team/cleat/pluginapi"
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
				Children: []string{},
				DependsOn: []string{},
				Created:  TimestampDate(),
				Updated:  TimestampDate(),
			},
			"clew-002": {
				ID:       "clew-002",
				Subject:  "Done task",
				Status:   "done",
				Priority: 2,
				Cost:     TaskCost{BudgetUSD: 20, SpentUSD: 15},
				Children: []string{},
				DependsOn: []string{},
				Created:  TimestampDate(),
				Updated:  TimestampDate(),
			},
		},
	}
	writeTestTasksJSON(t, filepath.Join(taskState, "tasks.json"), &tasks)

	// Create a task directory for clew-001.
	os.MkdirAll(filepath.Join(taskState, "clew-001"), 0755)
	os.WriteFile(filepath.Join(taskState, "clew-001", "TASK.md"), []byte("# clew-001 — Test\n\n"), 0644)
	os.WriteFile(filepath.Join(taskState, "clew-001", "STATUS.md"), []byte("# Status\n\n**Phase:** queued\n"), 0644)

	p := &Plugin{
		projectRoot:   root,
		newTaskScript: "/bin/true",
		tenantID:      "test-tenant",
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
	p.tenantID = ""
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
				Created: TimestampDate(), Updated: TimestampDate(),
			},
			"clew-002": {
				ID: "clew-002", Subject: "Task 2", Status: "done",
				Priority: 2, Cost: TaskCost{BudgetUSD: 20},
				DependsOn: []string{}, Children: []string{},
				Created: TimestampDate(), Updated: TimestampDate(),
			},
			"clew-003": {
				ID: "clew-003", Subject: "Task 3", Status: "queued",
				Priority: 3, DependsOn: []string{"clew-001"},
				Cost: TaskCost{BudgetUSD: 10},
				Children: []string{},
				Created: TimestampDate(), Updated: TimestampDate(),
			},
			"clew-004": {
				ID: "clew-004", Subject: "Task 4", Status: "queued",
				Priority: 4, DependsOn: []string{"clew-002"},
				Cost: TaskCost{BudgetUSD: 10},
				Children: []string{},
				Created: TimestampDate(), Updated: TimestampDate(),
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
		tenantID:      "test-tenant",
		logger:        slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}
	return p, root
}

func TestDispatchDependencyCheck(t *testing.T) {
	p, root := setupTestPluginWithDeps(t)

	// Create a mock dispatch script that writes session.json.
	mockScript := filepath.Join(root, "mock-dispatch.sh")
	scriptContent := "#!/bin/sh\n" +
		"TASKID=$1\n" +
		"DIR=\"" + root + "/task_state/$TASKID\"\n" +
		"mkdir -p \"$DIR\"\n" +
		"echo '{\"task_id\":\"'$TASKID'\",\"status\":\"dispatched\"}' > \"$DIR/session.json\"\n" +
		"exit 0\n"
	os.WriteFile(mockScript, []byte(scriptContent), 0755)
	p.newTaskScript = mockScript

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
	if w2.Code != http.StatusOK {
		t.Errorf("dispatch with satisfied dep: status %d, want 200 (body: %s)", w2.Code, w2.Body.String())
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
	p, root := setupTestPlugin(t)

	// Create session.json so it can be served.
	os.WriteFile(filepath.Join(root, "task_state", "clew-001", "session.json"),
		[]byte(`{"task_id":"clew-001","status":"dispatched"}`), 0644)

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
	if !strings.Contains(statusStr, "**Review Round 1:** PASS") {
		t.Errorf("STATUS.md did not preserve Review Round 1: %s", statusStr)
	}
	if !strings.Contains(statusStr, "**Review Round 2:** SHOULD_FIX") {
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

// ── clew-133c tests ──

func TestTaskGet(t *testing.T) {
	p, root := setupTestPlugin(t)

	// Write session.json inline before request.
	session := SessionJSON{
		TaskID:  "clew-001",
		Status:  "running",
		Phase:   "exploring",
		AgentID: "agent-1",
	}
	sessionData, _ := json.MarshalIndent(session, "", "  ")
	sessionData = append(sessionData, '\n')
	os.WriteFile(filepath.Join(root, "task_state", "clew-001", "session.json"), sessionData, 0644)

	mux := setupTestMux(t, p)

	req := httptest.NewRequest("GET", "/api/tasks/clew-001", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/tasks/clew-001: status %d, want 200", w.Code)
	}
	var resp TaskDetailResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Task.ID != "clew-001" {
		t.Errorf("Task.ID=%s, want clew-001", resp.Task.ID)
	}
	if resp.Status.Phase != "queued" {
		t.Errorf("Status.Phase=%s, want queued", resp.Status.Phase)
	}
	if resp.Session == nil {
		t.Error("Session is nil, want non-nil (session.json exists in setup)")
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
	var errResp ErrorResponse
	json.NewDecoder(w.Body).Decode(&errResp)
	if !strings.Contains(errResp.Error, "task not found") {
		t.Errorf("error message: %q, want containing 'task not found'", errResp.Error)
	}
}

func TestTaskGetNoSession(t *testing.T) {
	p, root := setupTestPlugin(t)
	os.MkdirAll(filepath.Join(root, "task_state", "clew-002"), 0755)
	os.WriteFile(filepath.Join(root, "task_state", "clew-002", "STATUS.md"),
		[]byte("# Status\n\n**Phase:** done\n"), 0644)

	mux := setupTestMux(t, p)

	req := httptest.NewRequest("GET", "/api/tasks/clew-002", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/tasks/clew-002: status %d, want 200", w.Code)
	}
	var resp TaskDetailResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Task.ID != "clew-002" {
		t.Errorf("Task.ID=%s, want clew-002", resp.Task.ID)
	}
	if resp.Session != nil {
		t.Error("Session should be nil when session.json does not exist")
	}
}

func TestTaskResultPostInvalidPhase(t *testing.T) {
	p, _ := setupTestPlugin(t)
	mux := setupTestMux(t, p)

	body := strings.NewReader(`{"phase":"bogus","outcome":"fail"}`)
	req := httptest.NewRequest("POST", "/api/tasks/clew-001/result", body)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("POST result invalid phase: status %d, want 400", w.Code)
	}
	var errResp ErrorResponse
	json.NewDecoder(w.Body).Decode(&errResp)
	if !strings.Contains(errResp.Error, "invalid phase") {
		t.Errorf("error message: %q, want containing 'invalid phase'", errResp.Error)
	}
}

func TestTaskResultPostTokenCost(t *testing.T) {
	p, root := setupTestPlugin(t)
	mux := setupTestMux(t, p)

	tasksPath := filepath.Join(root, "task_state", "tasks.json")
	data, _ := os.ReadFile(tasksPath)
	var tasksBefore TasksJSON
	json.Unmarshal(data, &tasksBefore)
	initialCost := tasksBefore.Tasks["clew-001"].Cost.SpentUSD

	body := strings.NewReader(`{"phase":"exploring","outcome":"pass","token_usage":{"input":1000,"output":500}}`)
	req := httptest.NewRequest("POST", "/api/tasks/clew-001/result", body)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("POST result token cost: status %d, want 200 (body: %s)", w.Code, w.Body.String())
	}

	data, _ = os.ReadFile(tasksPath)
	var tasksAfter TasksJSON
	json.Unmarshal(data, &tasksAfter)
	newCost := tasksAfter.Tasks["clew-001"].Cost.SpentUSD
	if newCost <= initialCost {
		t.Errorf("cost.spent_usd not incremented: before=%f, after=%f", initialCost, newCost)
	}
	expectedDelta := 0.0105
	delta := newCost - initialCost
	if delta < expectedDelta-0.001 || delta > expectedDelta+0.001 {
		t.Errorf("cost delta=%f, want ~%f", delta, expectedDelta)
	}
}

func TestTaskResultPostToTerminal(t *testing.T) {
	p, _ := setupTestPlugin(t)
	mux := setupTestMux(t, p)

	body := strings.NewReader(`{"phase":"failed","outcome":"fail"}`)
	req := httptest.NewRequest("POST", "/api/tasks/clew-001/result", body)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("POST result to terminal: status %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["phase"] != "failed" {
		t.Errorf("phase=%v, want failed", resp["phase"])
	}
}

func TestTaskResultPostNotFound(t *testing.T) {
	p, _ := setupTestPlugin(t)
	mux := setupTestMux(t, p)

	body := strings.NewReader(`{"phase":"exploring","outcome":"pass"}`)
	req := httptest.NewRequest("POST", "/api/tasks/clew-999/result", body)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("POST result not found: status %d, want 404", w.Code)
	}
}

func TestAgentHeartbeat(t *testing.T) {
	p, root := setupTestPlugin(t)
	session := SessionJSON{
		TaskID: "clew-001",
		Status: "running",
		Role:   "worker",
	}
	sessionData, _ := json.Marshal(session)
	os.WriteFile(filepath.Join(root, "task_state", "clew-001", "session.json"), sessionData, 0644)

	mux := setupTestMux(t, p)

	body := strings.NewReader(`{"task_id":"clew-001","agent_id":"agent-42"}`)
	req := httptest.NewRequest("POST", "/api/agent/heartbeat", body)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("POST heartbeat: status %d, want 200 (body: %s)", w.Code, w.Body.String())
	}

	sessionData, err := os.ReadFile(filepath.Join(root, "task_state", "clew-001", "session.json"))
	if err != nil {
		t.Fatal(err)
	}
	var updated SessionJSON
	json.Unmarshal(sessionData, &updated)
	if updated.HeartbeatAt == "" {
		t.Error("heartbeat_at is empty, want non-empty RFC 3339 timestamp")
	}
	if updated.AgentID != "agent-42" {
		t.Errorf("agent_id=%s, want agent-42", updated.AgentID)
	}
	if updated.Role != "worker" {
		t.Errorf("role=%s, want worker (should be preserved)", updated.Role)
	}
}

func TestAgentHeartbeatNoSession(t *testing.T) {
	p, root := setupTestPlugin(t)
	os.Remove(filepath.Join(root, "task_state", "clew-001", "session.json"))

	mux := setupTestMux(t, p)

	body := strings.NewReader(`{"task_id":"clew-001"}`)
	req := httptest.NewRequest("POST", "/api/agent/heartbeat", body)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("POST heartbeat no session: status %d, want 404 (body: %s)", w.Code, w.Body.String())
	}
}

func TestAgentPollBasic(t *testing.T) {
	p, root := setupTestPlugin(t)
	os.Remove(filepath.Join(root, "task_state", "clew-001", "session.json"))

	mux := setupTestMux(t, p)

	req := httptest.NewRequest("GET", "/api/agent/poll", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/agent/poll: status %d, want 200", w.Code)
	}
	var resp PollResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.TaskID != "clew-001" {
		t.Errorf("poll task_id=%s, want clew-001", resp.TaskID)
	}
	if resp.Priority != 1 {
		t.Errorf("poll priority=%d, want 1", resp.Priority)
	}
	if resp.Subject == "" {
		t.Error("poll subject is empty")
	}
	if resp.TaskPath != "task_state/clew-001" {
		t.Errorf("poll task_path=%s, want task_state/clew-001", resp.TaskPath)
	}
}

func TestAgentPollSkipsActive(t *testing.T) {
	p, root := setupTestPlugin(t)

	// Write session.json for clew-001 so it's considered active.
	session := SessionJSON{TaskID: "clew-001", Status: "running", Phase: "exploring"}
	sessionData, _ := json.MarshalIndent(session, "", "  ")
	sessionData = append(sessionData, '\n')
	os.WriteFile(filepath.Join(root, "task_state", "clew-001", "session.json"), sessionData, 0644)

	mux := setupTestMux(t, p)

	req := httptest.NewRequest("GET", "/api/agent/poll", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Errorf("GET /api/agent/poll with active session: status %d, want 204", w.Code)
	}
}

func TestTaskResultPost(t *testing.T) {
	p, root := setupTestPlugin(t)
	mux := setupTestMux(t, p)

	// Write session.json before result POST.
	session := SessionJSON{TaskID: "clew-001", Status: "running", Phase: "queued"}
	sessionData, _ := json.MarshalIndent(session, "", "  ")
	sessionData = append(sessionData, '\n')
	os.WriteFile(filepath.Join(root, "task_state", "clew-001", "session.json"), sessionData, 0644)

	body := strings.NewReader(`{"phase":"exploring","outcome":"pass","token_usage":{"input":1000,"output":500}}`)
	req := httptest.NewRequest("POST", "/api/tasks/clew-001/result", body)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("POST /api/tasks/clew-001/result: status %d, want 200 (body: %s)", w.Code, w.Body.String())
	}

	// Verify STATUS.md was updated on disk.
	statusData, err := os.ReadFile(filepath.Join(root, "task_state", "clew-001", "STATUS.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(statusData), "**Phase:** exploring") {
		t.Errorf("STATUS.md should contain exploring, got: %s", string(statusData))
	}

	// Verify tasks.json entry status updated.
	tasksData, _ := os.ReadFile(filepath.Join(root, "task_state", "tasks.json"))
	var tasks TasksJSON
	json.Unmarshal(tasksData, &tasks)
	if tasks.Tasks["clew-001"].Status != "exploring" {
		t.Errorf("tasks.json status=%s, want exploring", tasks.Tasks["clew-001"].Status)
	}

	// Verify session.json was updated with result fields.
	sessData, _ := os.ReadFile(filepath.Join(root, "task_state", "clew-001", "session.json"))
	var updatedSession SessionJSON
	json.Unmarshal(sessData, &updatedSession)
	if updatedSession.Phase != "exploring" {
		t.Errorf("session.phase=%s, want exploring", updatedSession.Phase)
	}
	if updatedSession.ExitCode == nil || *updatedSession.ExitCode != 0 {
		t.Errorf("session.exit_code should be 0, got %v", updatedSession.ExitCode)
	}
	if updatedSession.TokenUsage == nil || *updatedSession.TokenUsage != 1500 {
		t.Errorf("session.token_usage should be 1500, got %v", updatedSession.TokenUsage)
	}
}

func TestTaskResultPostInvalidTransition(t *testing.T) {
	p, _ := setupTestPlugin(t)
	mux := setupTestMux(t, p)

	body := strings.NewReader(`{"phase":"planning","outcome":"pass"}`)
	req := httptest.NewRequest("POST", "/api/tasks/clew-001/result", body)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("POST queued->planning: status %d, want 400", w.Code)
	}
	var errResp ErrorResponse
	json.NewDecoder(w.Body).Decode(&errResp)
	if !strings.Contains(errResp.Error, "invalid transition") {
		t.Errorf("error=%q, want contains 'invalid transition'", errResp.Error)
	}
}

func TestCanTransition(t *testing.T) {
	tests := []struct {
		from, to string
		want     bool
	}{
		// Valid +1 forward steps.
		{"queued", "exploring", true},
		{"exploring", "planning", true},
		{"planning", "plan_review", true},
		{"plan_review", "implementing", true},
		{"implementing", "impl_review", true},
		{"impl_review", "done", true},
		// Skip steps (invalid).
		{"queued", "planning", false},
		{"queued", "implementing", false},
		{"exploring", "plan_review", false},
		// From terminal to any valid phase.
		{"failed", "queued", true},
		{"failed", "exploring", true},
		{"blocked", "implementing", true},
		{"waiting_on_children", "done", true},
		// Any phase to terminal.
		{"queued", "failed", true},
		{"exploring", "blocked", true},
		{"implementing", "waiting_on_children", true},
		// Invalid target phase.
		{"queued", "bogus", false},
		{"failed", "nonexistent", false},
		// Backwards (invalid).
		{"exploring", "queued", false},
		{"done", "impl_review", false},
	}

	for _, tc := range tests {
		t.Run(tc.from+"->"+tc.to, func(t *testing.T) {
			got := CanTransition(tc.from, tc.to)
			if got != tc.want {
				t.Errorf("CanTransition(%s, %s) = %v, want %v", tc.from, tc.to, got, tc.want)
			}
		})
	}
}

// ── clew-141a merged tests ──

func TestAuthCheckAuthenticated(t *testing.T) {
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
	if body["authenticated"] != true {
		t.Errorf("GET /api/auth/check with tenant: authenticated=%v, want true", body["authenticated"])
	}
	if body["tenant"] != "test-tenant" {
		t.Errorf("GET /api/auth/check: tenant=%v, want test-tenant", body["tenant"])
	}
}

func TestProjectsGetEmpty(t *testing.T) {
	p, root := setupTestPlugin(t)
	os.RemoveAll(filepath.Join(root, "projects"))
	mux := setupTestMux(t, p)

	req := httptest.NewRequest("GET", "/api/projects", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("GET /api/projects (empty): status %d, want 200", w.Code)
	}
	var body map[string]any
	json.NewDecoder(w.Body).Decode(&body)
	projects, _ := body["projects"].([]any)
	if len(projects) != 1 {
		t.Errorf("GET /api/projects (empty): got %d projects, want 1 (clew only)", len(projects))
	}
}

func TestProjectsPostMissingName(t *testing.T) {
	p, _ := setupTestPlugin(t)
	mux := setupTestMux(t, p)

	body := strings.NewReader(`{"name":""}`)
	req := httptest.NewRequest("POST", "/api/projects", body)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("POST /api/projects missing name: status %d, want 400", w.Code)
	}
}

func TestTasksPostInvalidID(t *testing.T) {
	p, _ := setupTestPlugin(t)
	mux := setupTestMux(t, p)

	tests := []struct {
		id   string
		desc string
	}{
		{"bad", "no hyphen"},
		{"BAD-001", "uppercase"},
		{"clew-01", "not 3 digits"},
		{"clew-001$", "special char"},
		{"CLEW-001", "uppercase prefix"},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			body := strings.NewReader(fmt.Sprintf(`{"id":"%s","subject":"Test","priority":1}`, tc.id))
			req := httptest.NewRequest("POST", "/api/tasks", body)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)

			if w.Code != http.StatusBadRequest {
				t.Errorf("POST /api/tasks invalid ID %q: status %d, want 400", tc.id, w.Code)
			}
		})
	}
}

func TestTasksPostMissingFields(t *testing.T) {
	p, _ := setupTestPlugin(t)
	mux := setupTestMux(t, p)

	tests := []struct {
		body string
		desc string
	}{
		{`{"id":"","subject":"Test","priority":1}`, "empty id"},
		{`{"id":"clew-010","subject":"","priority":1}`, "empty subject"},
		{``, "missing JSON"},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/api/tasks", strings.NewReader(tc.body))
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)

			if w.Code != http.StatusBadRequest {
				t.Errorf("POST /api/tasks %s: status %d, want 400", tc.desc, w.Code)
			}
		})
	}
}

func TestTasksPostWithParent(t *testing.T) {
	p, root := setupTestPlugin(t)
	mux := setupTestMux(t, p)

	body := strings.NewReader(`{"id":"clew-003","subject":"Child task","parent":"clew-001","priority":2}`)
	req := httptest.NewRequest("POST", "/api/tasks", body)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("POST /api/tasks with parent: status %d, want 201 (body: %s)", w.Code, w.Body.String())
	}

	var entry TaskEntry
	json.NewDecoder(w.Body).Decode(&entry)
	if entry.Parent == nil || *entry.Parent != "clew-001" {
		t.Errorf("POST /api/tasks with parent: parent=%v, want clew-001", entry.Parent)
	}

	tasksPath := filepath.Join(root, "task_state", "tasks.json")
	data, _ := os.ReadFile(tasksPath)
	var tasks TasksJSON
	json.Unmarshal(data, &tasks)
	parent := tasks.Tasks["clew-001"]
	found := false
	for _, c := range parent.Children {
		if c == "clew-003" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("POST /api/tasks with parent: parent children=%v, should contain clew-003", parent.Children)
	}
}

func TestTasksPostParentNonexistent(t *testing.T) {
	p, _ := setupTestPlugin(t)
	mux := setupTestMux(t, p)

	body := strings.NewReader(`{"id":"clew-003","subject":"Orphan","parent":"clew-999","priority":2}`)
	req := httptest.NewRequest("POST", "/api/tasks", body)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("POST /api/tasks nonexistent parent: status %d, want 400", w.Code)
	}
	var errResp ErrorResponse
	json.NewDecoder(w.Body).Decode(&errResp)
	if !strings.Contains(errResp.Error, "clew-999") {
		t.Errorf("POST /api/tasks nonexistent parent: error=%q, want contains clew-999", errResp.Error)
	}
}

func TestTasksPostDefaultBudget(t *testing.T) {
	p, _ := setupTestPlugin(t)
	mux := setupTestMux(t, p)

	body := strings.NewReader(`{"id":"clew-003","subject":"No budget","priority":1}`)
	req := httptest.NewRequest("POST", "/api/tasks", body)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("POST /api/tasks default budget: status %d, want 201", w.Code)
	}
	var entry TaskEntry
	json.NewDecoder(w.Body).Decode(&entry)
	if entry.Cost.BudgetUSD != 10 {
		t.Errorf("POST /api/tasks default budget: budget=%f, want 10", entry.Cost.BudgetUSD)
	}
}

func TestTasksPostZeroBudget(t *testing.T) {
	p, _ := setupTestPlugin(t)
	mux := setupTestMux(t, p)

	body := strings.NewReader(`{"id":"clew-003","subject":"Zero budget","priority":1,"budget":0}`)
	req := httptest.NewRequest("POST", "/api/tasks", body)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("POST /api/tasks zero budget: status %d, want 201", w.Code)
	}
	var entry TaskEntry
	json.NewDecoder(w.Body).Decode(&entry)
	if entry.Cost.BudgetUSD != 10 {
		t.Errorf("POST /api/tasks zero budget: budget=%f, want 10", entry.Cost.BudgetUSD)
	}
}

func TestTasksPostStatFormat(t *testing.T) {
	p, root := setupTestPlugin(t)
	mux := setupTestMux(t, p)

	body := strings.NewReader(`{"id":"clew-003","subject":"Format check","priority":1,"budget":25}`)
	req := httptest.NewRequest("POST", "/api/tasks", body)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("POST /api/tasks format: status %d, want 201", w.Code)
	}

	statusData, err := os.ReadFile(filepath.Join(root, "task_state", "clew-003", "STATUS.md"))
	if err != nil {
		t.Fatal(err)
	}
	statusStr := string(statusData)
	if !strings.Contains(statusStr, "**Phase:** queued") {
		t.Error("STATUS.md missing Phase field")
	}
	if !strings.Contains(statusStr, "**Assigned:** none") {
		t.Error("STATUS.md missing Assigned field")
	}
	if !strings.Contains(statusStr, "**Budget:** $25") {
		t.Error("STATUS.md missing or wrong Budget field")
	}
	if !strings.Contains(statusStr, "**Spent:** $0") {
		t.Error("STATUS.md missing Spent field")
	}
	if !strings.Contains(statusStr, "**Created:**") {
		t.Error("STATUS.md missing Created field")
	}
	if !strings.Contains(statusStr, "**Updated:**") {
		t.Error("STATUS.md missing Updated field")
	}
}

func TestTasksPostTaskMDFormat(t *testing.T) {
	p, root := setupTestPlugin(t)
	mux := setupTestMux(t, p)

	body := strings.NewReader(`{"id":"clew-003","subject":"Format check","priority":1}`)
	req := httptest.NewRequest("POST", "/api/tasks", body)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("POST /api/tasks TASK.md format: status %d, want 201", w.Code)
	}

	taskData, err := os.ReadFile(filepath.Join(root, "task_state", "clew-003", "TASK.md"))
	if err != nil {
		t.Fatal(err)
	}
	taskStr := string(taskData)
	if !strings.Contains(taskStr, "## What") {
		t.Error("TASK.md missing ## What")
	}
	if !strings.Contains(taskStr, "## Scope") {
		t.Error("TASK.md missing ## Scope")
	}
	if !strings.Contains(taskStr, "## Acceptance") {
		t.Error("TASK.md missing ## Acceptance")
	}
	if !strings.Contains(taskStr, "- [ ] TODO") {
		t.Error("TASK.md missing - [ ] TODO")
	}
}

func TestDashboardSummaryNotFound(t *testing.T) {
	p, root := setupTestPlugin(t)
	os.Remove(filepath.Join(root, "task_state", "tasks.json"))
	mux := setupTestMux(t, p)

	req := httptest.NewRequest("GET", "/api/dashboard/summary", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("GET /api/dashboard/summary not found: status %d, want 404", w.Code)
	}
}

func TestPluginInit(t *testing.T) {
	root := t.TempDir()
	cfg := json.RawMessage(fmt.Sprintf(`{"project_root":"%s"}`, root))

	p := &Plugin{}
	err := p.Init(context.Background(), &pluginapi.Environment{
		Config: cfg,
		Logger: slog.New(slog.NewTextHandler(os.Stderr, nil)),
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if p.projectRoot != root {
		t.Errorf("Init: projectRoot=%s, want %s", p.projectRoot, root)
	}
}

func TestPluginInitMissingProjectRoot(t *testing.T) {
	p := &Plugin{}
	err := p.Init(context.Background(), &pluginapi.Environment{
		Config: json.RawMessage(`{}`),
		Logger: slog.New(slog.NewTextHandler(os.Stderr, nil)),
	})
	if err == nil {
		t.Error("Init without project_root: expected error, got nil")
	}
}

func TestPluginInfo(t *testing.T) {
	p := &Plugin{}
	info := p.Info()
	if info.Name != "clew-service" {
		t.Errorf("Info: name=%s, want clew-service", info.Name)
	}
	if info.DatabaseAccess != pluginapi.DatabaseAccessNone {
		t.Errorf("Info: database_access=%s, want none", info.DatabaseAccess)
	}
}

func TestTaskGetWithFiles(t *testing.T) {
	p, root := setupTestPlugin(t)
	mux := setupTestMux(t, p)

	logsDir := filepath.Join(root, "task_state", "clew-001", "logs")
	os.MkdirAll(logsDir, 0755)
	os.WriteFile(filepath.Join(logsDir, "a.log"), []byte("a"), 0644)
	os.WriteFile(filepath.Join(logsDir, "b.log"), []byte("b"), 0644)
	os.WriteFile(filepath.Join(logsDir, "c.log"), []byte("c"), 0644)

	artifactsDir := filepath.Join(root, "task_state", "clew-001", "artifacts")
	os.MkdirAll(artifactsDir, 0755)
	os.WriteFile(filepath.Join(artifactsDir, "plan.md"), []byte("plan"), 0644)
	os.WriteFile(filepath.Join(artifactsDir, "exploration.md"), []byte("exploration"), 0644)

	req := httptest.NewRequest("GET", "/api/tasks/clew-001", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/tasks/clew-001: status %d, want 200", w.Code)
	}
	var resp TaskDetailResponse
	json.NewDecoder(w.Body).Decode(&resp)

	if len(resp.Logs) != 3 {
		t.Errorf("logs: got %d entries, want 3", len(resp.Logs))
	}
	if len(resp.Logs) >= 3 {
		if resp.Logs[0] != "c.log" {
			t.Errorf("logs[0]=%s, want c.log (reverse chron)", resp.Logs[0])
		}
		if resp.Logs[1] != "b.log" {
			t.Errorf("logs[1]=%s, want b.log", resp.Logs[1])
		}
		if resp.Logs[2] != "a.log" {
			t.Errorf("logs[2]=%s, want a.log", resp.Logs[2])
		}
	}

	if len(resp.Artifacts) != 2 {
		t.Errorf("artifacts: got %d entries, want 2", len(resp.Artifacts))
	}
	if len(resp.Artifacts) >= 2 {
		if resp.Artifacts[0] != "exploration.md" {
			t.Errorf("artifacts[0]=%s, want exploration.md", resp.Artifacts[0])
		}
		if resp.Artifacts[1] != "plan.md" {
			t.Errorf("artifacts[1]=%s, want plan.md", resp.Artifacts[1])
		}
	}
}

func TestContentGet(t *testing.T) {
	p, root := setupTestPlugin(t)
	mux := setupTestMux(t, p)

	content := []byte("# CEO Guidance\n\nSome markdown content.")
	os.WriteFile(filepath.Join(root, "task_state", "CEO-GUIDANCE.md"), content, 0644)

	req := httptest.NewRequest("GET", "/api/content/CEO-GUIDANCE.md", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/content/CEO-GUIDANCE.md: status %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/markdown; charset=utf-8" {
		t.Errorf("Content-Type: %s, want text/markdown; charset=utf-8", ct)
	}
	if w.Body.String() != string(content) {
		t.Errorf("body: got %q, want %q", w.Body.String(), string(content))
	}

	// 404: nonexistent file.
	req2 := httptest.NewRequest("GET", "/api/content/nonexistent.md", nil)
	w2 := httptest.NewRecorder()
	mux.ServeHTTP(w2, req2)
	if w2.Code != http.StatusNotFound {
		t.Errorf("GET nonexistent: status %d, want 404", w2.Code)
	}

	// 400: non-.md extension.
	req3 := httptest.NewRequest("GET", "/api/content/test.txt", nil)
	w3 := httptest.NewRecorder()
	mux.ServeHTTP(w3, req3)
	if w3.Code != http.StatusBadRequest {
		t.Errorf("GET .txt: status %d, want 400", w3.Code)
	}

	// 200: serve from projects/ subtree.
	projDir := filepath.Join(root, "projects", "testproj")
	os.MkdirAll(projDir, 0755)
	os.WriteFile(filepath.Join(projDir, "README.md"), []byte("# Project README"), 0644)
	req4 := httptest.NewRequest("GET", "/api/content/testproj/README.md", nil)
	w4 := httptest.NewRecorder()
	mux.ServeHTTP(w4, req4)
	if w4.Code != http.StatusOK {
		t.Fatalf("GET project file: status %d, want 200 (body: %s)", w4.Code, w4.Body.String())
	}
	if w4.Body.String() != "# Project README" {
		t.Errorf("project file body: %q", w4.Body.String())
	}
}

func TestSafePathTraversal(t *testing.T) {
	p := &Plugin{projectRoot: "/tmp/clew"}

	_, err := p.safePath("../../etc/passwd")
	if err == nil {
		t.Error("safePath traversal: expected error, got nil")
	}
}

func TestSafePathValid(t *testing.T) {
	p := &Plugin{projectRoot: "/tmp/clew"}

	got, err := p.safePath("task_state/clew-001")
	if err != nil {
		t.Errorf("safePath valid: unexpected error: %v", err)
	}
	if !strings.HasPrefix(got, "/tmp/clew") {
		t.Errorf("safePath valid: %s doesn't have prefix /tmp/clew", got)
	}
	if !strings.HasSuffix(got, "task_state/clew-001") {
		t.Errorf("safePath valid: %s doesn't end with task_state/clew-001", got)
	}
}

func TestAgentDispatchValidation(t *testing.T) {
	p, root := setupTestPlugin(t)
	mux := setupTestMux(t, p)

	// 400: invalid task ID format (path param).
	req1 := httptest.NewRequest("POST", "/api/tasks/bad/dispatch", strings.NewReader(`{}`))
	w1 := httptest.NewRecorder()
	mux.ServeHTTP(w1, req1)
	if w1.Code != http.StatusBadRequest {
		t.Errorf("invalid format: status %d, want 400", w1.Code)
	}

	// 400: nonexistent task.
	req2 := httptest.NewRequest("POST", "/api/tasks/clew-999/dispatch", strings.NewReader(`{}`))
	w2 := httptest.NewRecorder()
	mux.ServeHTTP(w2, req2)
	if w2.Code != http.StatusBadRequest {
		t.Errorf("nonexistent task: status %d, want 400 (got %d)", w2.Code, w2.Code)
	}

	// 409: active session.
	session := SessionJSON{TaskID: "clew-001", Status: "running"}
	sessionData, _ := json.MarshalIndent(session, "", "  ")
	sessionData = append(sessionData, '\n')
	os.WriteFile(filepath.Join(root, "task_state", "clew-001", "session.json"), sessionData, 0644)

	req3 := httptest.NewRequest("POST", "/api/tasks/clew-001/dispatch", strings.NewReader(`{}`))
	w3 := httptest.NewRecorder()
	mux.ServeHTTP(w3, req3)
	if w3.Code != http.StatusConflict {
		t.Errorf("active session: status %d, want 409", w3.Code)
	}
}

func TestAgentCancel(t *testing.T) {
	p, root := setupTestPlugin(t)
	mux := setupTestMux(t, p)

	// 404: no session.json.
	req1 := httptest.NewRequest("POST", "/api/tasks/clew-001/cancel", nil)
	w1 := httptest.NewRecorder()
	mux.ServeHTTP(w1, req1)
	if w1.Code != http.StatusNotFound {
		t.Errorf("no session: status %d, want 404 (got %d)", w1.Code, w1.Code)
	}

	// 200: cancel active session.
	session := SessionJSON{TaskID: "clew-001", Status: "running"}
	sessionData, _ := json.MarshalIndent(session, "", "  ")
	sessionData = append(sessionData, '\n')
	os.WriteFile(filepath.Join(root, "task_state", "clew-001", "session.json"), sessionData, 0644)

	req2 := httptest.NewRequest("POST", "/api/tasks/clew-001/cancel", nil)
	w2 := httptest.NewRecorder()
	mux.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("cancel running: status %d, want 200 (body: %s)", w2.Code, w2.Body.String())
	}
	var okResp map[string]bool
	json.NewDecoder(w2.Body).Decode(&okResp)
	if !okResp["ok"] {
		t.Error("cancel response: ok should be true")
	}

	// Verify session.json updated on disk.
	data, _ := os.ReadFile(filepath.Join(root, "task_state", "clew-001", "session.json"))
	var updated SessionJSON
	json.Unmarshal(data, &updated)
	if updated.Status != "cancelled" {
		t.Errorf("session status=%s, want cancelled", updated.Status)
	}
	if updated.Ended == "" {
		t.Error("session ended should be set after cancel")
	}

	// 404: already-terminal session.
	req3 := httptest.NewRequest("POST", "/api/tasks/clew-001/cancel", nil)
	w3 := httptest.NewRecorder()
	mux.ServeHTTP(w3, req3)
	if w3.Code != http.StatusNotFound {
		t.Errorf("already cancelled: status %d, want 404 (got %d)", w3.Code, w3.Code)
	}
}
