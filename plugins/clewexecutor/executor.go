package clewexecutor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// runPhaseInput is the JSON input for the run_phase host function.
type runPhaseInput struct {
	TaskID       string `json:"task_id"`
	Project      string `json:"project"`
	ProjectRoot  string `json:"project_root"`
	Workdir      string `json:"workdir"`
	Phase        string `json:"phase,omitempty"`
	Model        string `json:"model,omitempty"`
	Tool         string `json:"tool,omitempty"`
	RoleOverride string `json:"role_override,omitempty"`
}

// runPhaseOutput is the JSON output from the run_phase host function.
type runPhaseOutput struct {
	ExitCode         int      `json:"exit_code"`
	PhaseChanged     bool     `json:"phase_changed"`
	NewPhase         string   `json:"new_phase"`
	ReviewOutcome    string   `json:"review_outcome"`
	ArtifactsWritten []string `json:"artifacts_written"`
	FindingsCount    int      `json:"findings_count"`
	Started          string   `json:"started"`
	Ended            string   `json:"ended"`
	Status           string   `json:"status"`
	Cached           bool     `json:"cached"`
	Error            string   `json:"error,omitempty"`
}

// runPhase implements the run_phase host function.
func (p *Plugin) runPhase(ctx context.Context, inputJSON string) (string, error) {
	var in runPhaseInput
	if err := json.Unmarshal([]byte(inputJSON), &in); err != nil {
		return "", fmt.Errorf("clew-executor: invalid input: %w", err)
	}

	if in.TaskID == "" {
		out := runPhaseOutput{Status: "failed", Error: "task_id is required"}
		b, _ := json.Marshal(out)
		return string(b), nil
	}
	if in.Project == "" {
		out := runPhaseOutput{Status: "failed", Error: "project is required"}
		b, _ := json.Marshal(out)
		return string(b), nil
	}
	if in.ProjectRoot == "" {
		out := runPhaseOutput{Status: "failed", Error: "project_root is required"}
		b, _ := json.Marshal(out)
		return string(b), nil
	}
	if in.Workdir == "" {
		out := runPhaseOutput{Status: "failed", Error: "workdir is required"}
		b, _ := json.Marshal(out)
		return string(b), nil
	}

	td := taskDir(in.ProjectRoot, in.Project, in.TaskID)
	statusPath := filepath.Join(td, "STATUS.md")
	sessionPath := filepath.Join(td, "session.json")

	phase, err := extractPhase(statusPath)
	if err != nil {
		out := runPhaseOutput{Status: "failed", Error: err.Error()}
		b, _ := json.Marshal(out)
		return string(b), nil
	}

	// Idempotency: check session.json for matching phase AND workflow phase.
	// Without the workflow phase check, review_plan and implement phases
	// (which both see STATUS.md "plan_review") would collide in the cache.
	if rec, _ := readSession(sessionPath); rec != nil &&
		rec.Phase == phase && rec.Status == "completed" && rec.WorkflowPhase == in.Phase {
		reviewOutcome := rec.ReviewOutcome
		if reviewOutcome == "" {
			role, _ := roleForPhase(phase)
			if role == "reviewer" {
				reviewOutcome = determineReviewOutcome(filepath.Join(td, "artifacts"), nil)
			}
		}
		out := runPhaseOutput{
			ExitCode:         rec.ExitCode,
			PhaseChanged:     rec.PhaseChanged,
			NewPhase:         rec.NewPhase,
			ReviewOutcome:    reviewOutcome,
			ArtifactsWritten: rec.ArtifactsWritten,
			FindingsCount:    rec.FindingsCount,
			Started:          rec.Started,
			Ended:            rec.Ended,
			Status:           rec.Status,
			Cached:           true,
		}
		b, _ := json.Marshal(out)
		return string(b), nil
	}

	// Determine role. Workflow phase (in.Phase) takes priority over STATUS.md phase
	// so that the implement phase launches a developer, not a reviewer.
	role := in.RoleOverride
	if role == "" {
		lookupPhase := phase // from STATUS.md
		if in.Phase != "" {
			lookupPhase = workflowPhaseToStatusPhase(in.Phase)
		}
		r, err := roleForPhase(lookupPhase)
		if err != nil {
			out := runPhaseOutput{Status: "failed", Error: err.Error()}
			b, _ := json.Marshal(out)
			return string(b), nil
		}
		role = r
	}

	protocolPath := filepath.Join(in.ProjectRoot, "prompts", role+"-agent.md")
	prompt, err := buildPrompt(in, role, td, protocolPath)
	if err != nil {
		out := runPhaseOutput{Status: "failed", Error: err.Error()}
		b, _ := json.Marshal(out)
		return string(b), nil
	}

	// Resolve agent binary.
	bin := p.agentBin
	if in.Tool == "aider" {
		bin = "aider"
	}
	if _, err := exec.LookPath(bin); err != nil {
		out := runPhaseOutput{Status: "failed", Error: fmt.Sprintf("agent not found: %s (resolve from $PATH)", bin)}
		b, _ := json.Marshal(out)
		return string(b), nil
	}

	// Snapshot artifacts/ before the run.
	artifactsDir := filepath.Join(td, "artifacts")
	snapshot := listArtifactFiles(artifactsDir)

	// Build CLI args.
	args := []string{"--dangerously-skip-permissions"}
	if in.Model != "" {
		args = append(args, "--model", in.Model)
	}

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = in.Workdir
	cmd.Stdin = strings.NewReader(prompt)

	var stdout, stderr cappedBuffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	started := time.Now()
	runErr := cmd.Run()
	ended := time.Now()

	exitCode := 0
	if runErr != nil {
		if exitErr, ok := runErr.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = -1
		}
	}

	status := "completed"
	if exitCode != 0 {
		status = "failed"
	}

	// Determine new phase by re-reading STATUS.md after the agent ran.
	newPhase := phase
	if np, err := extractPhase(statusPath); err == nil {
		newPhase = np
	}
	phaseWasChanged := newPhase != phase
	if !phaseWasChanged {
		newPhase = nextPhase(phase)
		phaseWasChanged = newPhase != phase
	}

	newSnapshot := listArtifactFiles(artifactsDir)
	artifactsWritten := diffArtifacts(snapshot, newSnapshot)
	findingsCount := countFindings(role, artifactsDir)

	reviewOutcome := ""
	if role == "reviewer" {
		reviewOutcome = determineReviewOutcome(artifactsDir, artifactsWritten)
	}

	var errStr string
	if runErr != nil && exitCode != 0 {
		errStr = buildErrorDetail(exitCode, stdout.String(), stderr.String())
	}
	if ctx.Err() != nil {
		if exitCode == 0 && status == "completed" {
			errStr = fmt.Sprintf("warning: WASM context expired but subprocess completed: %v", ctx.Err())
		} else if status != "completed" {
			errStr = fmt.Sprintf("phase timed out: %v", ctx.Err())
			status = "failed"
		}
	}

	startedStr := started.Format(time.RFC3339)
	endedStr := ended.Format(time.RFC3339)

	rec := &sessionRecord{
		Phase:            phase,
		WorkflowPhase:    in.Phase,
		ExitCode:         exitCode,
		PhaseChanged:     phaseWasChanged,
		NewPhase:         newPhase,
		ReviewOutcome:    reviewOutcome,
		ArtifactsWritten: artifactsWritten,
		FindingsCount:    findingsCount,
		Started:          startedStr,
		Ended:            endedStr,
		Status:           status,
	}
	_ = writeSession(sessionPath, rec)

	out := runPhaseOutput{
		ExitCode:         exitCode,
		PhaseChanged:     phaseWasChanged,
		NewPhase:         newPhase,
		ReviewOutcome:    reviewOutcome,
		ArtifactsWritten: artifactsWritten,
		FindingsCount:    findingsCount,
		Started:          startedStr,
		Ended:            endedStr,
		Status:           status,
		Error:            errStr,
	}
	b, _ := json.Marshal(out)
	return string(b), nil
}

// buildPrompt constructs the agent prompt matching clew-run.sh patterns.
func buildPrompt(in runPhaseInput, role, td, protocolPath string) (string, error) {
	if _, err := os.Stat(protocolPath); err != nil {
		return "", fmt.Errorf("protocol file not found: %s", protocolPath)
	}

	taskPath := filepath.Join("projects", in.Project, in.TaskID)

	switch role {
	case "explorer":
		return fmt.Sprintf(
			"You are an explorer agent in the Clew system. Project: %s. Read %s for your full protocol. Then read %s/TASK.md and %s/STATUS.md. Your task ID is %s. The code is in this repository (your working directory). Follow the protocol.",
			in.Project, protocolPath, taskPath, taskPath, in.TaskID,
		), nil
	case "planner":
		return fmt.Sprintf(
			"You are a planner agent in the Clew system. Project: %s. Read %s for your full protocol. Then read %s/TASK.md, %s/STATUS.md, and any artifacts in %s/artifacts/. Your task ID is %s. Follow the protocol.",
			in.Project, protocolPath, taskPath, taskPath, taskPath, in.TaskID,
		), nil
	case "developer":
		return fmt.Sprintf(
			"You are a developer agent in the Clew system. Project: %s. Read %s for your full protocol. Then read %s/TASK.md, %s/STATUS.md, and %s/CONTRACT.md (if it exists). Your task ID is %s. The code to modify is in this repository (your working directory). Follow the protocol exactly, including iteration to convergence at each review phase. Commit and push your changes when done.",
			in.Project, protocolPath, taskPath, taskPath, taskPath, in.TaskID,
		), nil
	case "reviewer":
		reviewType := "implementation"
		statusPath := filepath.Join(td, "STATUS.md")
		if data, err := os.ReadFile(statusPath); err == nil && bytes.Contains(data, []byte("plan_review")) {
			reviewType = "plan"
		}
		return fmt.Sprintf(
			"You are a reviewer agent in the Clew system. Project: %s. Read %s for your full protocol. You are reviewing task %s (%s review). Read %s/TASK.md, %s/STATUS.md, and all artifacts in %s/artifacts/. Your task ID is %s. Follow the protocol, iterating to convergence.",
			in.Project, protocolPath, in.TaskID, reviewType, taskPath, taskPath, taskPath, in.TaskID,
		), nil
	case "cto":
		return fmt.Sprintf(
			"You are the CTO agent in the Clew system. Project: %s. Read %s for your full protocol. Read %s/INDEX.md and %s/tasks.json. Follow the CTO lap protocol exactly. Write your CEO brief to the current task's log.",
			in.Project, protocolPath, taskPath, taskPath,
		), nil
	default:
		return "", fmt.Errorf("unknown role: %s", role)
	}
}

// workflowPhaseToStatusPhase converts workflow-internal phase names to clew status phases.
func workflowPhaseToStatusPhase(wfPhase string) string {
	switch wfPhase {
	case "explore":
		return "exploring"
	case "plan":
		return "planning"
	case "review_plan":
		return "plan_review"
	case "implement":
		return "implementing"
	case "review_impl":
		return "impl_review"
	default:
		return wfPhase
	}
}

// determineReviewOutcome scans review artifacts for outcome markers.
// When newArtifacts is nil/empty, scans all files in artifactsDir (used for cache fallback).
// Returns "" when no review artifacts exist (fresh review phase, first pass).
func determineReviewOutcome(artifactsDir string, newArtifacts []string) string {
	artifacts := newArtifacts
	if len(artifacts) == 0 {
		entries, err := os.ReadDir(artifactsDir)
		if err != nil {
			return ""
		}
		for _, e := range entries {
			if !e.IsDir() {
				artifacts = append(artifacts, e.Name())
			}
		}
	}
	hasBlocker := false
	hasShouldFix := false
	for _, name := range artifacts {
		if !strings.HasPrefix(name, "review-") || !strings.HasSuffix(name, ".md") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(artifactsDir, name))
		if err != nil {
			continue
		}
		if bytes.Contains(data, []byte("[OUTCOME:BLOCKER]")) || bytes.Contains(data, []byte("[BLOCKER]")) {
			hasBlocker = true
		}
		if bytes.Contains(data, []byte("[OUTCOME:SHOULD_FIX]")) || bytes.Contains(data, []byte("[SHOULD_FIX]")) {
			hasShouldFix = true
		}
		if bytes.Contains(data, []byte("[OUTCOME:PASS]")) {
			return "PASS"
		}
	}
	if hasBlocker {
		return "BLOCKER"
	}
	if hasShouldFix {
		return "SHOULD_FIX"
	}
	if len(artifacts) > 0 {
		return "PASS"
	}
	return ""
}

// nextPhase returns the next logical phase after a given phase, used when
// STATUS.md phase hasn't changed after a run (the agent ran but didn't
// update STATUS.md — the caller's workflow can still advance).
func nextPhase(phase string) string {
	switch phase {
	case "queued", "exploring":
		return "planning"
	case "planning":
		return "plan_review"
	case "plan_review":
		return "implementing"
	case "implementing":
		return "impl_review"
	case "impl_review":
		return "done"
	default:
		return phase
	}
}

// listArtifactFiles returns a map of artifact filename → modtime.
// Returns empty map if directory doesn't exist.
func listArtifactFiles(dir string) map[string]time.Time {
	out := make(map[string]time.Time)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out[e.Name()] = info.ModTime()
	}
	return out
}

// diffArtifacts returns filenames that are new or modified in 'after' vs 'before'.
func diffArtifacts(before, after map[string]time.Time) []string {
	var diff []string
	for name, afterTime := range after {
		beforeTime, existed := before[name]
		if !existed || afterTime.After(beforeTime) {
			diff = append(diff, name)
		}
	}
	return diff
}

// countFindings estimates finding count based on role and artifact content.
// For explorer: counts lines in exploration.md.
// For review roles: counts [BLOCKER] and [SHOULD_FIX] markers in review-*.md.
// For all other roles, returns 0.
func countFindings(role, artifactsDir string) int {
	switch role {
	case "explorer":
		data, err := os.ReadFile(filepath.Join(artifactsDir, "exploration.md"))
		if err != nil {
			return 0
		}
		lines := bytes.Count(data, []byte("\n"))
		if len(data) > 0 && data[len(data)-1] != '\n' {
			lines++ // count the last line even without trailing newline
		}
		return lines
	case "reviewer":
		count := 0
		entries, err := os.ReadDir(artifactsDir)
		if err != nil {
			return 0
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasPrefix(e.Name(), "review-") || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			data, err := os.ReadFile(filepath.Join(artifactsDir, e.Name()))
			if err != nil {
				continue
			}
			count += bytes.Count(data, []byte("[BLOCKER]"))
			count += bytes.Count(data, []byte("[SHOULD_FIX]"))
		}
		return count
	default:
		return 0
	}
}

// buildErrorDetail constructs an error message from subprocess output.
func buildErrorDetail(exitCode int, stdout, stderr string) string {
	var parts []string
	parts = append(parts, fmt.Sprintf("exit code %d", exitCode))
	if stderr != "" {
		parts = append(parts, "stderr: "+stderr)
	}
	if stdout != "" {
		parts = append(parts, "stdout: "+stdout)
	}
	return strings.Join(parts, "; ")
}

// cappedBuffer is a bytes.Buffer that caps at 1MB to prevent memory exhaustion.
type cappedBuffer struct {
	buf   bytes.Buffer
	total int
}

const maxCap = 1 << 20 // 1 MB

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if c.total >= maxCap {
		return len(p), nil // silently discard
	}
	remain := maxCap - c.total
	if len(p) > remain {
		p = p[:remain]
	}
	n, err := c.buf.Write(p)
	c.total += n
	return n, err
}

func (c *cappedBuffer) String() string {
	return c.buf.String()
}

func (c *cappedBuffer) Bytes() []byte {
	return c.buf.Bytes()
}

