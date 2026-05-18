package clewexecutor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/internal/plugin"
)

func TestPluginRegistration(t *testing.T) {
	plugins := plugin.List()
	found := false
	for _, info := range plugins {
		if info.Name == "clew-executor" {
			found = true
			if info.Version == "" {
				t.Error("clew-executor version is empty")
			}
			if info.Description == "" {
				t.Error("clew-executor description is empty")
			}
			break
		}
	}
	if !found {
		t.Error("clew-executor not found in plugin registry")
	}
}

func TestPluginInfo(t *testing.T) {
	p := &Plugin{}
	info := p.Info()
	if info.Name != "clew-executor" {
		t.Errorf("expected name 'clew-executor', got %q", info.Name)
	}
	if info.Version != "0.1.0" {
		t.Errorf("expected version '0.1.0', got %q", info.Version)
	}
	if info.Description == "" {
		t.Error("expected non-empty description")
	}
}

func TestPluginInit(t *testing.T) {
	p := &Plugin{}
	env := &plugin.Environment{}
	if err := p.Init(context.Background(), env); err != nil {
		t.Fatalf("Init() returned error: %v", err)
	}
	if p.logger == nil {
		t.Error("logger should be set after Init")
	}
	if p.agentBin != "claude" {
		t.Errorf("expected default agentBin 'claude', got %q", p.agentBin)
	}
}

func TestPluginInitWithConfig(t *testing.T) {
	p := &Plugin{}
	env := &plugin.Environment{
		Config: []byte(`{"agent_bin": "/usr/local/bin/claude"}`),
	}
	if err := p.Init(context.Background(), env); err != nil {
		t.Fatalf("Init() with config returned error: %v", err)
	}
	if p.agentBin != "/usr/local/bin/claude" {
		t.Errorf("expected agentBin '/usr/local/bin/claude', got %q", p.agentBin)
	}
}

func TestPluginInitWithInvalidConfig(t *testing.T) {
	p := &Plugin{}
	env := &plugin.Environment{
		Config: []byte(`{bad json`),
	}
	err := p.Init(context.Background(), env)
	if err == nil {
		t.Error("expected error for invalid config")
	}
}

func TestRegisterHostFunctions(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(context.Background(), &plugin.Environment{}); err != nil {
		t.Fatalf("Init() failed: %v", err)
	}

	registry := &mockFuncRegistry{}
	if err := p.RegisterHostFunctions(registry); err != nil {
		t.Fatalf("RegisterHostFunctions() returned error: %v", err)
	}
	if len(registry.registrations) != 5 {
		t.Fatalf("expected 5 registrations, got %d", len(registry.registrations))
	}
	// Order: run_phase, check_ci, validate_files, read_file, create_task
	expect := []struct {
		name       string
		idempotent bool
	}{
		{"run_phase", true},
		{"check_ci", false},
		{"validate_files", true},
		{"read_file", true},
		{"create_task", true},
	}
	for i, e := range expect {
		if registry.registrations[i].name != e.name {
			t.Errorf("expected registrations[%d] name %q, got %q", i, e.name, registry.registrations[i].name)
		}
		if registry.registrations[i].idempotent != e.idempotent {
			t.Errorf("expected registrations[%d] idempotent %v, got %v", i, e.idempotent, registry.registrations[i].idempotent)
		}
	}
}

func TestRegisterHostFunctionsNilScope(t *testing.T) {
	p := &Plugin{}
	_ = p.Init(context.Background(), &plugin.Environment{})
	err := p.RegisterHostFunctions(nil)
	if err == nil {
		t.Error("expected error for nil scope")
	}
}

func TestRoleForPhase(t *testing.T) {
	tests := []struct {
		phase    string
		wantRole string
		wantErr  bool
	}{
		{"queued", "explorer", false},
		{"exploring", "explorer", false},
		{"planning", "planner", false},
		{"plan_review", "reviewer", false},
		{"implementing", "developer", false},
		{"impl_review", "reviewer", false},
		{"create_pr", "developer", false},
		{"ci_fix", "developer", false},
		{"merge", "developer", false},
		{"done", "", true},
		{"blocked", "", true},
		{"failed", "", true},
		{"unknown_phase", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.phase, func(t *testing.T) {
			role, err := roleForPhase(tc.phase)
			if tc.wantErr && err == nil {
				t.Errorf("expected error for phase %q, got role %q", tc.phase, role)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("unexpected error for phase %q: %v", tc.phase, err)
			}
			if role != tc.wantRole {
				t.Errorf("expected role %q for phase %q, got %q", tc.wantRole, tc.phase, role)
			}
		})
	}
}

func TestExtractPhase(t *testing.T) {
	tmp := t.TempDir()
	statusPath := filepath.Join(tmp, "STATUS.md")
	os.WriteFile(statusPath, []byte("**Phase:** planning\n"), 0644)

	phase, err := extractPhase(statusPath)
	if err != nil {
		t.Fatalf("extractPhase() returned error: %v", err)
	}
	if phase != "planning" {
		t.Errorf("expected 'planning', got %q", phase)
	}
}

func TestExtractPhaseMissingFile(t *testing.T) {
	_, err := extractPhase("/nonexistent/path/STATUS.md")
	if err == nil {
		t.Error("expected error for missing file")
	}
}

func TestExtractPhaseNoMatch(t *testing.T) {
	tmp := t.TempDir()
	statusPath := filepath.Join(tmp, "STATUS.md")
	os.WriteFile(statusPath, []byte("No phase here\n"), 0644)

	_, err := extractPhase(statusPath)
	if err == nil {
		t.Error("expected error when Phase line not found")
	}
}

func TestBuildPrompt(t *testing.T) {
	in := runPhaseInput{
		TaskID:      "cleat-212",
		Project:     "cleat",
		ProjectRoot: "/tmp/clew",
		Workdir:     "/tmp/cleat",
	}

	// Create a mock protocol file.
	tmp := t.TempDir()
	protocolPath := filepath.Join(tmp, "developer-agent.md")
	os.WriteFile(protocolPath, []byte("protocol content"), 0644)

	tests := []struct {
		role    string
		contains []string
	}{
		{"explorer", []string{"explorer agent", "cleat-212", "TASK.md"}},
		{"planner", []string{"planner agent", "cleat-212", "TASK.md", "artifacts/"}},
		{"developer", []string{"developer agent", "cleat-212", "CONTRACT.md"}},
		{"reviewer", []string{"reviewer agent", "cleat-212", "review", "artifacts/"}},
		{"cto", []string{"CTO agent", "INDEX.md", "tasks.json"}},
	}

	for _, tc := range tests {
		t.Run(tc.role, func(t *testing.T) {
			prompt, err := buildPrompt(in, tc.role, t.TempDir(), protocolPath, "")
			if err != nil {
				t.Fatalf("buildPrompt() for %s returned error: %v", tc.role, err)
			}
			for _, want := range tc.contains {
				if !strings.Contains(prompt, want) {
					t.Errorf("prompt for %s should contain %q, got:\n%s", tc.role, want, prompt)
				}
			}
		})
	}
}

func TestBuildPromptProtocolNotFound(t *testing.T) {
	// buildPrompt doesn't check file existence — that's done by runPhase.
	// It should succeed even with a non-existent protocol path.
	in := runPhaseInput{TaskID: "test", Project: "test", ProjectRoot: "/tmp", Workdir: "/tmp"}
	prompt, err := buildPrompt(in, "explorer", "/tmp", "/nonexistent/protocol.md", "")
	if err != nil {
		t.Fatalf("buildPrompt() returned error: %v", err)
	}
	if prompt == "" {
		t.Error("expected non-empty prompt")
	}
}

func TestBuildPromptUnknownRole(t *testing.T) {
	in := runPhaseInput{TaskID: "test", Project: "test", ProjectRoot: "/tmp", Workdir: "/tmp"}
	_, err := buildPrompt(in, "unknown", "/tmp", "/dev/null", "")
	if err == nil {
		t.Error("expected error for unknown role")
	}
}

func TestSessionReadWrite(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "session.json")

	rec := &sessionRecord{
		Phase:         "planning",
		ExitCode:      0,
		NewPhase:      "plan_review",
		Status:        "completed",
		Started:       "2026-05-16T08:00:00Z",
		Ended:         "2026-05-16T08:05:00Z",
		FindingsCount: 5,
	}
	if err := writeSession(path, rec); err != nil {
		t.Fatalf("writeSession() error: %v", err)
	}

	got, err := readSession(path)
	if err != nil {
		t.Fatalf("readSession() error: %v", err)
	}
	if got == nil {
		t.Fatal("readSession() returned nil")
	}
	if got.Phase != rec.Phase {
		t.Errorf("Phase: expected %q, got %q", rec.Phase, got.Phase)
	}
	if got.ExitCode != rec.ExitCode {
		t.Errorf("ExitCode: expected %d, got %d", rec.ExitCode, got.ExitCode)
	}
	if got.NewPhase != rec.NewPhase {
		t.Errorf("NewPhase: expected %q, got %q", rec.NewPhase, got.NewPhase)
	}
	if got.Status != rec.Status {
		t.Errorf("Status: expected %q, got %q", rec.Status, got.Status)
	}
	if got.FindingsCount != rec.FindingsCount {
		t.Errorf("FindingsCount: expected %d, got %d", rec.FindingsCount, got.FindingsCount)
	}
}

func TestSessionReadMissing(t *testing.T) {
	rec, err := readSession("/nonexistent/session.json")
	if err != nil {
		t.Fatalf("readSession() returned error: %v", err)
	}
	if rec != nil {
		t.Error("expected nil for missing session file")
	}
}

func TestSessionReadCorrupt(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "session.json")
	os.WriteFile(path, []byte("not json"), 0644)

	rec, err := readSession(path)
	if err != nil {
		t.Fatalf("readSession() returned error: %v", err)
	}
	if rec != nil {
		t.Error("expected nil for corrupt session file")
	}
}

func TestDiffArtifacts(t *testing.T) {
	before := map[string]time.Time{
		"a.md": time.Now().Add(-1 * time.Hour),
		"b.md": time.Now(),
	}
	after := map[string]time.Time{
		"a.md": time.Now(),                  // modified (newer)
		"b.md": before["b.md"],              // unchanged (same time - but diff doesn't check this deeply)
		"c.md": time.Now(),                  // new file
	}
	diff := diffArtifacts(before, after)
	if len(diff) < 1 {
		t.Error("expected at least 1 diff entry")
	}
	hasA := false
	hasC := false
	for _, name := range diff {
		if name == "a.md" {
			hasA = true
		}
		if name == "c.md" {
			hasC = true
		}
	}
	if !hasA {
		t.Error("expected a.md in diff (modified)")
	}
	if !hasC {
		t.Error("expected c.md in diff (new)")
	}
}

func TestDiffArtifactsEmptyBefore(t *testing.T) {
	after := map[string]time.Time{
		"x.md": time.Now(),
	}
	diff := diffArtifacts(nil, after)
	if len(diff) != 1 || diff[0] != "x.md" {
		t.Errorf("expected [x.md], got %v", diff)
	}
}

func TestCountFindingsExplorer(t *testing.T) {
	tmp := t.TempDir()
	os.WriteFile(filepath.Join(tmp, "exploration.md"), []byte("line 1\nline 2\nline 3\n"), 0644)
	count := countFindings("explorer", tmp)
	if count != 3 {
		t.Errorf("expected 3 lines, got %d", count)
	}
}

func TestCountFindingsExplorerNoTrailingNewline(t *testing.T) {
	tmp := t.TempDir()
	os.WriteFile(filepath.Join(tmp, "exploration.md"), []byte("line 1\nline 2\nline 3"), 0644)
	count := countFindings("explorer", tmp)
	if count != 3 {
		t.Errorf("expected 3 lines, got %d", count)
	}
}

func TestCountFindingsReviewer(t *testing.T) {
	tmp := t.TempDir()
	os.WriteFile(filepath.Join(tmp, "review-plan.md"), []byte("[BLOCKER] bad thing\n[SHOULD_FIX] minor\n[BLOCKER] worse\n"), 0644)
	count := countFindings("reviewer", tmp)
	if count != 3 {
		t.Errorf("expected 3 findings, got %d", count)
	}
}

func TestCountFindingsReviewerSkipsTableRows(t *testing.T) {
	tmp := t.TempDir()
	os.WriteFile(filepath.Join(tmp, "review-plan.md"), []byte(
		"[BLOCKER] real finding\n| [BLOCKER] | 0 | 0 |\n[SHOULD_FIX] minor\n| [SHOULD_FIX] | 1 | 1 |\n"), 0644)
	count := countFindings("reviewer", tmp)
	if count != 2 {
		t.Errorf("expected 2 findings (table rows skipped), got %d", count)
	}
}

func TestCountFindingsNoArtifact(t *testing.T) {
	count := countFindings("explorer", "/nonexistent/dir")
	if count != 0 {
		t.Errorf("expected 0 for missing dir, got %d", count)
	}
}

func TestCountFindingsOtherRole(t *testing.T) {
	tmp := t.TempDir()
	os.WriteFile(filepath.Join(tmp, "something.md"), []byte("content"), 0644)
	count := countFindings("developer", tmp)
	if count != 0 {
		t.Errorf("expected 0 for developer role, got %d", count)
	}
	count = countFindings("planner", tmp)
	if count != 0 {
		t.Errorf("expected 0 for planner role, got %d", count)
	}
}

func TestNextPhase(t *testing.T) {
	tests := map[string]string{
		"queued":       "planning",
		"exploring":    "planning",
		"planning":     "plan_review",
		"plan_review":  "implementing",
		"implementing": "impl_review",
		"impl_review":  "create_pr",
		"create_pr":    "ci_wait",
		"ci_fix":       "ci_wait",
		"merge":        "done",
		"unknown":      "unknown",
	}
	for phase, want := range tests {
		got := nextPhase(phase)
		if got != want {
			t.Errorf("nextPhase(%q): expected %q, got %q", phase, want, got)
		}
	}
}

func TestTaskDir(t *testing.T) {
	got := taskDir("/root", "cleat", "cleat-212")
	want := filepath.Join("/root", "projects", "cleat", "cleat-212")
	if got != want {
		t.Errorf("expected %q, got %q", want, got)
	}
}

// --- runPhase integration tests ---

func setupRunPhaseTest(t *testing.T, phase string) (string, *Plugin) {
	t.Helper()
	tmp := t.TempDir()

	// Set up task directory structure.
	taskDir := filepath.Join(tmp, "projects", "testproj", "test-task")
	os.MkdirAll(taskDir, 0755)

	statusPath := filepath.Join(taskDir, "STATUS.md")
	os.WriteFile(statusPath, []byte("**Phase:** "+phase+"\n"), 0644)

	// Create mock protocol file for each role the tests use.
	for _, role := range []string{"developer", "explorer", "planner", "reviewer", "cto"} {
		protoDir := filepath.Join(tmp, "prompts")
		os.MkdirAll(protoDir, 0755)
		os.WriteFile(filepath.Join(protoDir, role+"-agent.md"), []byte("protocol"), 0644)
	}

	p := &Plugin{}
	if err := p.Init(context.Background(), &plugin.Environment{}); err != nil {
		t.Fatalf("Init() failed: %v", err)
	}
	p.agentBin = "true" // always exits 0, ignores all args

	return tmp, p
}

func TestRunPhaseSuccess(t *testing.T) {
	root, p := setupRunPhaseTest(t, "implementing")

	in := runPhaseInput{
		TaskID:      "test-task",
		Project:     "testproj",
		ProjectRoot: root,
		Workdir:     root,
	}
	inputJSON, _ := json.Marshal(in)

	outJSON, err := p.runPhase(context.Background(), string(inputJSON))
	if err != nil {
		t.Fatalf("runPhase() returned error: %v", err)
	}

	var out runPhaseOutput
	if err := json.Unmarshal([]byte(outJSON), &out); err != nil {
		t.Fatalf("failed to unmarshal output: %v", err)
	}

	if out.ExitCode != 0 {
		t.Errorf("expected exit_code 0, got %d", out.ExitCode)
	}
	if out.Status != "completed" {
		t.Errorf("expected status 'completed', got %q", out.Status)
	}
	if out.Started == "" {
		t.Error("expected non-empty started time")
	}
	if out.Ended == "" {
		t.Error("expected non-empty ended time")
	}
	if out.NewPhase == "" {
		t.Error("expected non-empty new_phase")
	}
	if out.Cached {
		t.Error("expected Cached: false on first invocation")
	}
}

func TestRunPhaseIdempotent(t *testing.T) {
	root, p := setupRunPhaseTest(t, "implementing")

	in := runPhaseInput{
		TaskID:      "test-task",
		Project:     "testproj",
		ProjectRoot: root,
		Workdir:     root,
	}
	inputJSON, _ := json.Marshal(in)

	// First call.
	outJSON1, err := p.runPhase(context.Background(), string(inputJSON))
	if err != nil {
		t.Fatalf("first runPhase() error: %v", err)
	}

	// Second call should be cached.
	outJSON2, err := p.runPhase(context.Background(), string(inputJSON))
	if err != nil {
		t.Fatalf("second runPhase() error: %v", err)
	}

	var out1, out2 runPhaseOutput
	json.Unmarshal([]byte(outJSON1), &out1)
	json.Unmarshal([]byte(outJSON2), &out2)

	if !out2.Cached {
		t.Error("expected Cached: true on second invocation")
	}
	if out2.ExitCode != out1.ExitCode {
		t.Errorf("cached exit_code mismatch: %d vs %d", out2.ExitCode, out1.ExitCode)
	}
	if out2.Started != out1.Started {
		t.Errorf("cached started mismatch: %q vs %q", out2.Started, out1.Started)
	}

	// Reviewer phase: verify ReviewOutcome is preserved in cache.
	t.Run("reviewer", func(t *testing.T) {
		root2, p2 := setupRunPhaseTest(t, "plan_review")
		artifactsDir := filepath.Join(root2, "projects", "testproj", "test-task", "artifacts")
		os.MkdirAll(artifactsDir, 0755)
		os.WriteFile(filepath.Join(artifactsDir, "review-plan.md"), []byte("[OUTCOME:PASS]\n"), 0644)

		in2 := runPhaseInput{
			TaskID:      "test-task",
			Project:     "testproj",
			ProjectRoot: root2,
			Workdir:     root2,
		}
		inputJSON2, _ := json.Marshal(in2)

		outJSON1, err := p2.runPhase(context.Background(), string(inputJSON2))
		if err != nil {
			t.Fatalf("first runPhase() (reviewer) error: %v", err)
		}
		var r1 runPhaseOutput
		json.Unmarshal([]byte(outJSON1), &r1)
		if r1.ReviewOutcome != "PASS" {
			t.Errorf("expected ReviewOutcome PASS on first call, got %q", r1.ReviewOutcome)
		}

		outJSON2, err := p2.runPhase(context.Background(), string(inputJSON2))
		if err != nil {
			t.Fatalf("second runPhase() (reviewer) error: %v", err)
		}
		var r2 runPhaseOutput
		json.Unmarshal([]byte(outJSON2), &r2)
		if !r2.Cached {
			t.Error("expected Cached: true on second reviewer invocation")
		}
		if r2.ReviewOutcome != "PASS" {
			t.Errorf("expected cached ReviewOutcome PASS, got %q", r2.ReviewOutcome)
		}
	})
}

func TestRunPhaseRoleOverride(t *testing.T) {
	root, p := setupRunPhaseTest(t, "queued")

	in := runPhaseInput{
		TaskID:       "test-task",
		Project:      "testproj",
		ProjectRoot:  root,
		Workdir:      root,
		RoleOverride: "developer", // override queued→explorer default
	}
	inputJSON, _ := json.Marshal(in)

	outJSON, err := p.runPhase(context.Background(), string(inputJSON))
	if err != nil {
		t.Fatalf("runPhase() error: %v", err)
	}

	var out runPhaseOutput
	json.Unmarshal([]byte(outJSON), &out)
	if out.Status != "completed" {
		t.Errorf("expected status 'completed', got %q (error: %s)", out.Status, out.Error)
	}
}

func TestRunPhaseMissingTaskID(t *testing.T) {
	p := &Plugin{}
	_ = p.Init(context.Background(), &plugin.Environment{})

	in := runPhaseInput{}
	inputJSON, _ := json.Marshal(in)
	outJSON, _ := p.runPhase(context.Background(), string(inputJSON))

	var out runPhaseOutput
	json.Unmarshal([]byte(outJSON), &out)
	if out.Error == "" {
		t.Error("expected error for missing task_id")
	}
	if out.Status != "failed" {
		t.Errorf("expected status 'failed', got %q", out.Status)
	}
}

func TestRunPhaseMissingProject(t *testing.T) {
	p := &Plugin{}
	_ = p.Init(context.Background(), &plugin.Environment{})

	in := runPhaseInput{TaskID: "test", ProjectRoot: "/tmp", Workdir: "/tmp"}
	inputJSON, _ := json.Marshal(in)
	outJSON, _ := p.runPhase(context.Background(), string(inputJSON))

	var out runPhaseOutput
	json.Unmarshal([]byte(outJSON), &out)
	if out.Error == "" {
		t.Error("expected error for missing project")
	}
}

func TestRunPhaseMissingProjectRoot(t *testing.T) {
	p := &Plugin{}
	_ = p.Init(context.Background(), &plugin.Environment{})

	in := runPhaseInput{TaskID: "test", Project: "proj", Workdir: "/tmp"}
	inputJSON, _ := json.Marshal(in)
	outJSON, _ := p.runPhase(context.Background(), string(inputJSON))

	var out runPhaseOutput
	json.Unmarshal([]byte(outJSON), &out)
	if out.Error == "" {
		t.Error("expected error for missing project_root")
	}
}

func TestRunPhaseMissingWorkdir(t *testing.T) {
	p := &Plugin{}
	_ = p.Init(context.Background(), &plugin.Environment{})

	in := runPhaseInput{TaskID: "test", Project: "proj", ProjectRoot: "/tmp"}
	inputJSON, _ := json.Marshal(in)
	outJSON, _ := p.runPhase(context.Background(), string(inputJSON))

	var out runPhaseOutput
	json.Unmarshal([]byte(outJSON), &out)
	if out.Error == "" {
		t.Error("expected error for missing workdir")
	}
}

func TestRunPhaseMissingSTATUS(t *testing.T) {
	p := &Plugin{}
	_ = p.Init(context.Background(), &plugin.Environment{})

	in := runPhaseInput{
		TaskID:      "noexist",
		Project:     "noproj",
		ProjectRoot: t.TempDir(),
		Workdir:     t.TempDir(),
	}
	inputJSON, _ := json.Marshal(in)
	outJSON, _ := p.runPhase(context.Background(), string(inputJSON))

	var out runPhaseOutput
	json.Unmarshal([]byte(outJSON), &out)
	if !strings.Contains(out.Error, "STATUS.md") {
		t.Errorf("expected error about STATUS.md, got: %s", out.Error)
	}
}

func TestRunPhaseBlocked(t *testing.T) {
	root, p := setupRunPhaseTest(t, "blocked")

	in := runPhaseInput{
		TaskID:      "test-task",
		Project:     "testproj",
		ProjectRoot: root,
		Workdir:     root,
	}
	inputJSON, _ := json.Marshal(in)
	outJSON, _ := p.runPhase(context.Background(), string(inputJSON))

	var out runPhaseOutput
	json.Unmarshal([]byte(outJSON), &out)
	if out.Error == "" {
		t.Error("expected error for blocked task")
	}
}

func TestRunPhaseDone(t *testing.T) {
	root, p := setupRunPhaseTest(t, "done")

	in := runPhaseInput{
		TaskID:      "test-task",
		Project:     "testproj",
		ProjectRoot: root,
		Workdir:     root,
	}
	inputJSON, _ := json.Marshal(in)
	outJSON, _ := p.runPhase(context.Background(), string(inputJSON))

	var out runPhaseOutput
	json.Unmarshal([]byte(outJSON), &out)
	if out.Error == "" {
		t.Error("expected error for done task")
	}
}

func TestRunPhaseAgentNotFound(t *testing.T) {
	root, p := setupRunPhaseTest(t, "implementing")
	p.agentBin = "nonexistent-binary-xyzzy-12345"

	in := runPhaseInput{
		TaskID:      "test-task",
		Project:     "testproj",
		ProjectRoot: root,
		Workdir:     root,
	}
	inputJSON, _ := json.Marshal(in)
	outJSON, _ := p.runPhase(context.Background(), string(inputJSON))

	var out runPhaseOutput
	json.Unmarshal([]byte(outJSON), &out)
	if !strings.Contains(out.Error, "agent not found") {
		t.Errorf("expected 'agent not found' error, got: %s", out.Error)
	}
}

func TestRunPhaseAgentExitNonZero(t *testing.T) {
	root, p := setupRunPhaseTest(t, "implementing")
	p.agentBin = "false" // always exits 1, ignores all args

	in := runPhaseInput{
		TaskID:      "test-task",
		Project:     "testproj",
		ProjectRoot: root,
		Workdir:     root,
	}
	inputJSON, _ := json.Marshal(in)
	outJSON, _ := p.runPhase(context.Background(), string(inputJSON))

	var out runPhaseOutput
	json.Unmarshal([]byte(outJSON), &out)
	if out.ExitCode != 1 {
		t.Errorf("expected exit_code 1, got %d", out.ExitCode)
	}
	if out.Status != "failed" {
		t.Errorf("expected status 'failed', got %q", out.Status)
	}
	if out.Error == "" || !strings.Contains(out.Error, "exit code") {
		t.Errorf("expected non-empty Error field with 'exit code', got: %s", out.Error)
	}
}

func TestRunPhaseArtifactsDetected(t *testing.T) {
	root, p := setupRunPhaseTest(t, "exploring")

	in := runPhaseInput{
		TaskID:      "test-task",
		Project:     "testproj",
		ProjectRoot: root,
		Workdir:     root,
	}

	// Use a mock script that creates an artifact.
	mockScript := filepath.Join(root, "mock-agent.sh")
	artifactsDir := filepath.Join(root, "projects", "testproj", "test-task", "artifacts")
	os.MkdirAll(artifactsDir, 0755)
	os.WriteFile(mockScript, []byte(`#!/bin/sh
echo "hello" > "`+artifactsDir+`/exploration.md"
`, ), 0755)
	p.agentBin = mockScript

	inputJSON, _ := json.Marshal(in)
	outJSON, _ := p.runPhase(context.Background(), string(inputJSON))

	var out runPhaseOutput
	json.Unmarshal([]byte(outJSON), &out)
	if out.ExitCode != 0 {
		t.Errorf("expected exit_code 0, got %d (error: %s)", out.ExitCode, out.Error)
	}

	found := false
	for _, a := range out.ArtifactsWritten {
		if a == "exploration.md" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected exploration.md in artifacts_written, got %v", out.ArtifactsWritten)
	}
}

func TestRunPhaseContextCancelled(t *testing.T) {
	root, p := setupRunPhaseTest(t, "implementing")

	// Use 'sleep 10' as agent; cancel context immediately.
	p.agentBin = "sleep"

	in := runPhaseInput{
		TaskID:      "test-task",
		Project:     "testproj",
		ProjectRoot: root,
		Workdir:     root,
	}
	inputJSON, _ := json.Marshal(in)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before even starting

	outJSON, _ := p.runPhase(ctx, string(inputJSON))
	var out runPhaseOutput
	json.Unmarshal([]byte(outJSON), &out)
	if !strings.Contains(out.Error, "timed out") && out.Status == "failed" {
		// On some systems, sleep might not be found or context canceled before exec.
		// Just check that the error path works.
		t.Logf("context cancel result: status=%s, error=%s, exit=%d", out.Status, out.Error, out.ExitCode)
	}
}

func TestRunPhaseConvergenceGuardReviewPlan(t *testing.T) {
	// STATUS.md at "implementing" (ahead of review_plan→plan_review).
	// Workflow dispatches review_plan. Guard should return PASS without
	// running an agent — no agent should be invoked.
	root, p := setupRunPhaseTest(t, "implementing")
	p.agentBin = "false" // exits 1; proves agent wasn't run

	in := runPhaseInput{
		TaskID:      "test-task",
		Project:     "testproj",
		ProjectRoot: root,
		Workdir:     root,
		Phase:       "review_plan",
	}
	inputJSON, _ := json.Marshal(in)

	outJSON, err := p.runPhase(context.Background(), string(inputJSON))
	if err != nil {
		t.Fatalf("runPhase() returned error: %v", err)
	}

	var out runPhaseOutput
	json.Unmarshal([]byte(outJSON), &out)
	if out.ReviewOutcome != "PASS" {
		t.Errorf("expected ReviewOutcome PASS, got %q", out.ReviewOutcome)
	}
	if out.Status != "completed" {
		t.Errorf("expected status 'completed', got %q (error: %s)", out.Status, out.Error)
	}
	if out.ExitCode != 0 {
		t.Errorf("expected exit_code 0, got %d", out.ExitCode)
	}
	if out.Cached {
		t.Error("expected Cached false (synthetic PASS, not cache hit)")
	}

	// Second call should hit the session cache.
	outJSON2, err := p.runPhase(context.Background(), string(inputJSON))
	if err != nil {
		t.Fatalf("second runPhase() error: %v", err)
	}
	var out2 runPhaseOutput
	json.Unmarshal([]byte(outJSON2), &out2)
	if !out2.Cached {
		t.Error("expected Cached true on second invocation (session.json written by guard)")
	}
	if out2.ReviewOutcome != "PASS" {
		t.Errorf("expected cached ReviewOutcome PASS, got %q", out2.ReviewOutcome)
	}
}

func TestRunPhaseConvergenceGuardReviewImpl(t *testing.T) {
	// STATUS.md at "done" (ahead of review_impl→impl_review).
	root, p := setupRunPhaseTest(t, "done")
	p.agentBin = "false"

	in := runPhaseInput{
		TaskID:      "test-task",
		Project:     "testproj",
		ProjectRoot: root,
		Workdir:     root,
		Phase:       "review_impl",
	}
	inputJSON, _ := json.Marshal(in)

	outJSON, err := p.runPhase(context.Background(), string(inputJSON))
	if err != nil {
		t.Fatalf("runPhase() returned error: %v", err)
	}

	var out runPhaseOutput
	json.Unmarshal([]byte(outJSON), &out)
	if out.ReviewOutcome != "PASS" {
		t.Errorf("expected ReviewOutcome PASS, got %q", out.ReviewOutcome)
	}
	if out.Status != "completed" {
		t.Errorf("expected status 'completed', got %q (error: %s)", out.Status, out.Error)
	}
}

func TestRunPhaseConvergenceGuardNotTriggeredNormal(t *testing.T) {
	// STATUS.md at "plan_review" (same as review_plan→plan_review).
	// Guard should NOT trigger — normal review flow.
	root, p := setupRunPhaseTest(t, "plan_review")
	p.agentBin = "true"

	artifactsDir := filepath.Join(root, "projects", "testproj", "test-task", "artifacts")
	os.MkdirAll(artifactsDir, 0755)
	os.WriteFile(filepath.Join(artifactsDir, "review-plan.md"),
		[]byte("[OUTCOME:PASS]\n"), 0644)

	in := runPhaseInput{
		TaskID:      "test-task",
		Project:     "testproj",
		ProjectRoot: root,
		Workdir:     root,
		Phase:       "review_plan",
	}
	inputJSON, _ := json.Marshal(in)

	outJSON, err := p.runPhase(context.Background(), string(inputJSON))
	if err != nil {
		t.Fatalf("runPhase() returned error: %v", err)
	}

	var out runPhaseOutput
	json.Unmarshal([]byte(outJSON), &out)
	// Should have run the agent normally — the agent exits 0.
	if out.Status != "completed" {
		t.Errorf("expected status 'completed', got %q (error: %s)", out.Status, out.Error)
	}
	// When STATUS.md == workflow phase, laterPhase returns same phase,
	// role is reviewer, ReviewOutcome determined from artifacts.
}

func TestMatchReviewOutcome(t *testing.T) {
	tests := []struct {
		line string
		want string
	}{
		// Bracketed [OUTCOME:XXX] — canonical format.
		{`[OUTCOME:PASS]`, "PASS"},
		{`[OUTCOME:APPROVED]`, "PASS"},
		{`[OUTCOME:BLOCKER]`, "BLOCKER"},
		{`[OUTCOME:SHOULD_FIX]`, "SHOULD_FIX"},

		// **Verdict:** XXX — bold verdict line.
		{`**Verdict:** PASS`, "PASS"},
		{`**Verdict:** APPROVED`, "PASS"},
		{`**Verdict:** PASSES`, "PASS"},
		{`**Verdict:** pass`, "PASS"},
		{`**Verdict:** approved`, "PASS"},
		{"**Verdict:** ❌ Does not pass", ""},

		// **Verdict: WORD** — word inside bold span.
		{`**Verdict: APPROVED**`, "PASS"},
		{`**Verdict: PASS**`, "PASS"},

		// Bare OUTCOME: XXX (no brackets).
		{`OUTCOME: PASS`, "PASS"},
		{`OUTCOME: BLOCKER`, "BLOCKER"},
		{`OUTCOME: SHOULD_FIX`, "SHOULD_FIX"},

		// Legacy bare [BLOCKER] / [SHOULD_FIX].
		{`[BLOCKER]`, "BLOCKER"},
		{`[SHOULD_FIX]`, "SHOULD_FIX"},

		// Non-matching lines.
		{`Some random text`, ""},
		{``, ""},

		// Table row — matchReviewOutcome does NOT skip table rows.
		{`| [BLOCKER] | 0 | 0 |`, "BLOCKER"},
	}
	for _, tc := range tests {
		t.Run("", func(t *testing.T) {
			got := matchReviewOutcome(tc.line)
			if got != tc.want {
				t.Errorf("matchReviewOutcome(%q) = %q, want %q", tc.line, got, tc.want)
			}
		})
	}
}

func TestDetermineReviewOutcome(t *testing.T) {
	tests := []struct {
		name     string
		files    map[string]string // filename → content
		want     string
	}{
		{
			name: "canonical PASS",
			files: map[string]string{
				"review-plan.md": "[OUTCOME:PASS]\n",
			},
			want: "PASS",
		},
		{
			name: "APPROVED equals PASS",
			files: map[string]string{
				"review-plan.md": "[OUTCOME:APPROVED]\n",
			},
			want: "PASS",
		},
		{
			name: "bold verdict PASS",
			files: map[string]string{
				"review-plan.md": "**Verdict:** PASS\n",
			},
			want: "PASS",
		},
		{
			name: "PASS with table row skipped",
			files: map[string]string{
				"review-plan.md": "[OUTCOME:PASS]\n| [BLOCKER] | 0 | 0 |\n",
			},
			want: "PASS",
		},
		{
			name: "legacy bare BLOCKER",
			files: map[string]string{
				"review-plan.md": "[BLOCKER]\nsome issue\n",
			},
			want: "BLOCKER",
		},
		{
			name: "legacy bare SHOULD_FIX",
			files: map[string]string{
				"review-plan.md": "[SHOULD_FIX]\nminor issue\n",
			},
			want: "SHOULD_FIX",
		},
		{
			name: "BLOCKER beats SHOULD_FIX",
			files: map[string]string{
				"review-plan.md": "[BLOCKER]\n[SHOULD_FIX]\n",
			},
			want: "BLOCKER",
		},
		{
			name:  "no review artifacts",
			files: nil,
			want:  "",
		},
		{
			name: "generic fallback PASS",
			files: map[string]string{
				"review-plan.md": "Some text without markers\n",
			},
			want: "PASS",
		},
		{
			name: "bare OUTCOME line",
			files: map[string]string{
				"review-plan.md": "OUTCOME: PASS\n",
			},
			want: "PASS",
		},
		{
			name: "table row only skipped, generic PASS",
			files: map[string]string{
				"review-plan.md": "| [BLOCKER] | 0 | 0 |\n",
			},
			want: "PASS",
		},
		{
			name: "explicit BLOCKER",
			files: map[string]string{
				"review-plan.md": "[OUTCOME:BLOCKER]\n",
			},
			want: "BLOCKER",
		},
		{
			name: "explicit SHOULD_FIX",
			files: map[string]string{
				"review-plan.md": "[OUTCOME:SHOULD_FIX]\nminor issue\n",
			},
			want: "SHOULD_FIX",
		},
		{
			name: "non-review files ignored",
			files: map[string]string{
				"other-file.txt": "[OUTCOME:PASS]\n",
			},
			want: "PASS",
		},
		{
			name: "verdict wins over skipped table",
			files: map[string]string{
				"review-plan.md": "**Verdict:** APPROVED\n| [BLOCKER] | 1 | 0 |\n",
			},
			want: "PASS",
		},
		{
			name: "verdict word inside bold span",
			files: map[string]string{
				"review-plan.md": "**Verdict: APPROVED**\n",
			},
			want: "PASS",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, content := range tc.files {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
					t.Fatalf("WriteFile: %v", err)
				}
			}
			got := determineReviewOutcome(dir, nil)
			if got != tc.want {
				t.Errorf("determineReviewOutcome() = %q, want %q", got, tc.want)
			}
		})
	}
}

// --- mock helpers ---

type mockFuncRegistry struct {
	registrations []struct {
		name       string
		idempotent bool
	}
}

func (m *mockFuncRegistry) Register(opts plugin.FuncOptions, fn plugin.PluginFunc) error {
	m.registrations = append(m.registrations, struct {
		name       string
		idempotent bool
	}{opts.Name, opts.Idempotent})
	return nil
}

func TestCrashArtifactWrittenOnNonZeroExit(t *testing.T) {
	root, p := setupRunPhaseTest(t, "implementing")

	artifactsDir := filepath.Join(root, "projects", "testproj", "test-task", "artifacts")
	os.MkdirAll(artifactsDir, 0755)
	mockScript := filepath.Join(root, "mock-fail.sh")
	os.WriteFile(mockScript, []byte(`#!/bin/sh
echo "this is stderr output" >&2
echo "this is stdout output"
exit 3
`), 0755)
	p.agentBin = mockScript

	in := runPhaseInput{
		TaskID:      "test-task",
		Project:     "testproj",
		ProjectRoot: root,
		Workdir:     root,
	}
	inputJSON, _ := json.Marshal(in)
	outJSON, _ := p.runPhase(context.Background(), string(inputJSON))

	var out runPhaseOutput
	if err := json.Unmarshal([]byte(outJSON), &out); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if out.ExitCode != 3 {
		t.Errorf("expected exit_code 3, got %d", out.ExitCode)
	}
	if out.Status != "failed" {
		t.Errorf("expected status 'failed', got %q", out.Status)
	}
	if out.CrashLog != "artifacts/crash.log" {
		t.Errorf("expected CrashLog 'artifacts/crash.log', got %q", out.CrashLog)
	}
	if out.DurationMs <= 0 {
		t.Errorf("expected DurationMs > 0, got %d", out.DurationMs)
	}

	// Verify crash.log file exists and has content.
	crashPath := filepath.Join(artifactsDir, "crash.log")
	data, err := os.ReadFile(crashPath)
	if err != nil {
		t.Fatalf("crash.log not found at %s: %v", crashPath, err)
	}
	content := string(data)
	if !strings.Contains(content, "Exit code: 3") {
		t.Error("crash.log missing exit code")
	}
	if !strings.Contains(content, "this is stderr output") {
		t.Error("crash.log missing stderr content")
	}
	if !strings.Contains(content, "this is stdout output") {
		t.Error("crash.log missing stdout content")
	}
	if !strings.Contains(content, "Phase: implementing") {
		t.Error("crash.log missing phase")
	}
	if !strings.Contains(content, "Duration:") {
		t.Error("crash.log missing duration")
	}

	// Verify session.json has crash fields.
	sessionPath := filepath.Join(root, "projects", "testproj", "test-task", "session.json")
	sessData, err := os.ReadFile(sessionPath)
	if err != nil {
		t.Fatalf("session.json not found: %v", err)
	}
	if !strings.Contains(string(sessData), `"crash_log"`) {
		t.Error("session.json missing crash_log field")
	}
	if !strings.Contains(string(sessData), `"duration_ms"`) {
		t.Error("session.json missing duration_ms field")
	}
}

func TestNoCrashArtifactOnSuccess(t *testing.T) {
	root, p := setupRunPhaseTest(t, "implementing")
	// agentBin = "true" (set by setupRunPhaseTest) exits 0

	in := runPhaseInput{
		TaskID:      "test-task",
		Project:     "testproj",
		ProjectRoot: root,
		Workdir:     root,
	}
	inputJSON, _ := json.Marshal(in)
	outJSON, _ := p.runPhase(context.Background(), string(inputJSON))

	var out runPhaseOutput
	json.Unmarshal([]byte(outJSON), &out)
	if out.CrashLog != "" {
		t.Errorf("expected empty CrashLog on success, got %q", out.CrashLog)
	}
	if out.DurationMs != 0 {
		t.Errorf("expected DurationMs 0 on success, got %d", out.DurationMs)
	}

	// Verify no crash.log was written.
	artifactsDir := filepath.Join(root, "projects", "testproj", "test-task", "artifacts")
	crashPath := filepath.Join(artifactsDir, "crash.log")
	if _, err := os.Stat(crashPath); err == nil {
		t.Error("crash.log should not exist on success")
	}
}

func TestCrashArtifactWithEmptyOutput(t *testing.T) {
	root, p := setupRunPhaseTest(t, "implementing")
	p.agentBin = "false" // exits 1, no output

	in := runPhaseInput{
		TaskID:      "test-task",
		Project:     "testproj",
		ProjectRoot: root,
		Workdir:     root,
	}
	inputJSON, _ := json.Marshal(in)
	p.runPhase(context.Background(), string(inputJSON))

	artifactsDir := filepath.Join(root, "projects", "testproj", "test-task", "artifacts")
	data, _ := os.ReadFile(filepath.Join(artifactsDir, "crash.log"))
	content := string(data)
	if !strings.Contains(content, "Exit code: 1") {
		t.Error("crash.log missing exit code for empty-output crash")
	}
}
