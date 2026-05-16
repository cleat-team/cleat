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

	// Idempotency: check session.json for matching completed phase.
	if rec, _ := readSession(sessionPath); rec != nil &&
		rec.Phase == phase && rec.Status == "completed" {
		out := runPhaseOutput{
			ExitCode:         rec.ExitCode,
			PhaseChanged:     true,
			NewPhase:         rec.NewPhase,
			ReviewOutcome:    "",
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

	// Determine role.
	role := in.RoleOverride
	if role == "" {
		r, err := roleForPhase(phase)
		if err != nil {
			out := runPhaseOutput{Status: "failed", Error: err.Error()}
			b, _ := json.Marshal(out)
			return string(b), nil
		}
		role = r
	}

	// Prompts live in the clew repo. Resolve clew root from workdir.
	clewRoot := in.Workdir
	if filepath.Base(clewRoot) != "clew" {
		clewRoot = filepath.Join(filepath.Dir(clewRoot), "clew")
	}
	if _, err := os.Stat(filepath.Join(clewRoot, "prompts")); err != nil {
		clewRoot = filepath.Join(filepath.Dir(in.ProjectRoot), "clew")
	}
	protocolPath := filepath.Join(clewRoot, "prompts", role+"-agent.md")
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

	// Use a long timeout independent of the workflow's WASM execution context.
	execCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(execCtx, bin, args...)
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
		// WASM context expired but subprocess may have completed.
		// Don't override a successful completion.
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
		ExitCode:         exitCode,
		NewPhase:         newPhase,
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

	taskMD := filepath.Join(td, "TASK.md")
	statusMD := filepath.Join(td, "STATUS.md")
	contractMD := filepath.Join(td, "CONTRACT.md")
	artifactsDir := filepath.Join(td, "artifacts")
	indexMD := filepath.Join(filepath.Dir(td), "INDEX.md")
	tasksJSON := filepath.Join(filepath.Dir(td), "tasks.json")

	switch role {
	case "explorer":
		return fmt.Sprintf(
			"You are an explorer agent in the Clew system. Project: %s. Read %s for your full protocol. Then read %s and %s. Your task ID is %s. The code is in this repository (your working directory). Follow the protocol.",
			in.Project, protocolPath, taskMD, statusMD, in.TaskID,
		), nil
	case "planner":
		return fmt.Sprintf(
			"You are a planner agent in the Clew system. Project: %s. Read %s for your full protocol. Then read %s, %s, and any artifacts in %s. Your task ID is %s. Follow the protocol.",
			in.Project, protocolPath, taskMD, statusMD, artifactsDir, in.TaskID,
		), nil
	case "developer":
		return fmt.Sprintf(
			"You are a developer agent in the Clew system. Project: %s. Read %s for your full protocol. Then read %s, %s, and %s (if it exists). Your task ID is %s. The code to modify is in this repository (your working directory). Follow the protocol exactly, including iteration to convergence at each review phase. Commit and push your changes when done.",
			in.Project, protocolPath, taskMD, statusMD, contractMD, in.TaskID,
		), nil
	case "reviewer":
		reviewType := "implementation"
		if data, err := os.ReadFile(statusMD); err == nil && bytes.Contains(data, []byte("plan_review")) {
			reviewType = "plan"
		}
		return fmt.Sprintf(
			"You are a reviewer agent in the Clew system. Project: %s. Read %s for your full protocol. You are reviewing task %s (%s review). Read %s, %s, and all artifacts in %s. Your task ID is %s. Follow the protocol, iterating to convergence. End your review with exactly one of: [OUTCOME:PASS], [OUTCOME:BLOCKER], or [OUTCOME:SHOULD_FIX].",
			in.Project, protocolPath, in.TaskID, reviewType, taskMD, statusMD, artifactsDir, in.TaskID,
		), nil
	case "cto":
		return fmt.Sprintf(
			"You are the CTO agent in the Clew system. Project: %s. Read %s for your full protocol. Read %s and %s. Follow the CTO lap protocol exactly. Write your CEO brief to the current task's log.",
			in.Project, protocolPath, indexMD, tasksJSON,
		), nil
	default:
		return "", fmt.Errorf("unknown role: %s", role)
	}
}

// nextPhase returns the next logical phase after a given phase.
func nextPhase(phase string) string {
	switch phase {
	case "queued", "exploring":
		return "planning"
	case "planning":
		return "plan_review"
	case "plan_review", "plan_approved":
		return "implementing"
	case "implementing":
		return "impl_review"
	case "impl_review", "impl_approved":
		return "done"
	default:
		if strings.Contains(phase, "approved") {
			if strings.Contains(phase, "plan") {
				return "implementing"
			}
			return "done"
		}
		return phase
	}
}

// determineReviewOutcome scans review artifacts for outcome markers.
// Returns "PASS", "BLOCKER", "SHOULD_FIX", or "" if indeterminate.
func determineReviewOutcome(artifactsDir string, newArtifacts []string) string {
	hasBlocker := false
	hasShouldFix := false
	for _, name := range newArtifacts {
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
	if len(newArtifacts) > 0 {
		return "PASS"
	}
	return ""
}

// listArtifactFiles returns a map of artifact filename to modtime.
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
func countFindings(role, artifactsDir string) int {
	switch role {
	case "explorer":
		data, err := os.ReadFile(filepath.Join(artifactsDir, "exploration.md"))
		if err != nil {
			return 0
		}
		lines := bytes.Count(data, []byte("\n"))
		if len(data) > 0 && data[len(data)-1] != '\n' {
			lines++
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

// cappedBuffer is a bytes.Buffer that caps at 1MB.
type cappedBuffer struct {
	buf   bytes.Buffer
	total int
}

const maxCap = 1 << 20 // 1 MB

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if c.total >= maxCap {
		return len(p), nil
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
