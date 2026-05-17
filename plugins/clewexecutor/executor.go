package clewexecutor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
	CrashLog         string   `json:"crash_log,omitempty"`
	DurationMs       int64    `json:"duration_ms,omitempty"`
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
			CrashLog:         rec.CrashLog,
			DurationMs:       rec.DurationMs,
		}
		b, _ := json.Marshal(out)
		return string(b), nil
	}

	// Use the later of STATUS.md phase and workflow phase for role lookup.
	// If STATUS.md is ahead (e.g. "implementing" vs wf "explore"), use it —
	// an explorer adds no value when a plan is already approved.
	// If the workflow phase is ahead (e.g. "implement" vs "plan_review"),
	// use it — the reviewer passed and we need a developer now.
	role := in.RoleOverride
	if role == "" {
		wfStatusPhase := workflowPhaseToStatusPhase(in.Phase)

		// Convergence guard: when STATUS.md has advanced past a review
		// phase (e.g. CTO/human manually advanced it), return PASS to
		// prevent the workflow from entering a retry loop dispatching
		// non-reviewer roles with empty ReviewOutcome.
		if (in.Phase == "review_plan" || in.Phase == "review_impl") &&
			phaseOrder[phase] > phaseOrder[wfStatusPhase] {
			now := time.Now().Format(time.RFC3339)
			rec := &sessionRecord{
				Phase:         phase,
				WorkflowPhase: in.Phase,
				ReviewOutcome: "PASS",
				Started:       now,
				Ended:         now,
				Status:        "completed",
			}
			_ = writeSession(sessionPath, rec)
			out := runPhaseOutput{
				ReviewOutcome: "PASS",
				Started:       now,
				Ended:         now,
				Status:        "completed",
			}
			b, _ := json.Marshal(out)
			return string(b), nil
		}

		lookupPhase := laterPhase(phase, wfStatusPhase)
		r, err := roleForPhase(lookupPhase)
		if err != nil {
			out := runPhaseOutput{Status: "failed", Error: err.Error()}
			b, _ := json.Marshal(out)
			return string(b), nil
		}
		role = r
	}

	protocolPath := filepath.Join(in.ProjectRoot, "prompts", role+"-agent.md")
	if _, err := os.Stat(protocolPath); err != nil {
		// For non-clew projects, prompts live in the clew repo, not project_root.
		// Fall back: look for ../clew/prompts relative to workdir.
		fallback := filepath.Join(in.Workdir, "..", "clew", "prompts", role+"-agent.md")
		if _, err2 := os.Stat(fallback); err2 == nil {
			protocolPath = fallback
		}
	}
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

	var crashLogPath string
	var durationMs int64
	if exitCode != 0 {
		durationMs = ended.Sub(started).Milliseconds()
		crashLogPath = "artifacts/crash.log"

		crashContent := fmt.Sprintf(`# Crash log — %s
Task: %s
Phase: %s
Exit code: %d
Duration: %dms
Started: %s
Ended: %s
Error: exit code %d

## stdout (last 200 lines)
%s

## stderr (last 200 lines)
%s
`, in.TaskID, in.TaskID, phase, exitCode, durationMs,
			started.Format(time.RFC3339), ended.Format(time.RFC3339),
			exitCode,
			tailLines(stdout.String(), 200), tailLines(stderr.String(), 200))

		artifactPath := filepath.Join(artifactsDir, "crash.log")
		if err := os.MkdirAll(artifactsDir, 0755); err != nil {
			crashLogPath = ""
		} else {
			_ = os.WriteFile(artifactPath, []byte(crashContent), 0644)
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
		CrashLog:         crashLogPath,
		DurationMs:       durationMs,
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
		CrashLog:         crashLogPath,
		DurationMs:       durationMs,
	}
	b, _ := json.Marshal(out)
	return string(b), nil
}

// checkCIInput is the JSON input for the check_ci host function.
type checkCIInput struct {
	Workdir string `json:"workdir"`
}

// checkCIOutput is the JSON output from the check_ci host function.
type checkCIOutput struct {
	CIStatus string `json:"ci_status"`
	PRURL    string `json:"pr_url"`
	Details  string `json:"details"`
}

// checkCI implements the check_ci host function — queries GitHub CI status
// for the current branch without launching a Claude session.
func (p *Plugin) checkCI(ctx context.Context, inputJSON string) (string, error) {
	var in checkCIInput
	if err := json.Unmarshal([]byte(inputJSON), &in); err != nil {
		return "", fmt.Errorf("check_ci: invalid input: %w", err)
	}
	if in.Workdir == "" {
		out := checkCIOutput{CIStatus: "error", Details: "workdir is required"}
		b, _ := json.Marshal(out)
		return string(b), nil
	}

	branchCmd := exec.CommandContext(ctx, "git", "-C", in.Workdir, "branch", "--show-current")
	branchOut, err := branchCmd.Output()
	if err != nil {
		var stderr string
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			stderr = string(exitErr.Stderr)
		}
		out := checkCIOutput{
			CIStatus: "error",
			Details:  fmt.Sprintf("git branch --show-current: %v: %s", err, stderr),
		}
		b, _ := json.Marshal(out)
		return string(b), nil
	}
	branch := strings.TrimSpace(string(branchOut))
	if branch == "" {
		out := checkCIOutput{CIStatus: "error", Details: "detached HEAD"}
		b, _ := json.Marshal(out)
		return string(b), nil
	}

	ghCmd := exec.CommandContext(ctx, "gh", "pr", "list",
		"--head", branch,
		"--json", "number,state,url,statusCheckRollup",
		"--limit", "1",
	)
	ghCmd.Dir = in.Workdir
	ghOut, ghErr := ghCmd.Output()
	if ghErr != nil {
		var exitErr *exec.ExitError
		if errors.As(ghErr, &exitErr) {
			out := checkCIOutput{
				CIStatus: "error",
				Details:  fmt.Sprintf("gh pr list: %s", string(exitErr.Stderr)),
			}
			b, _ := json.Marshal(out)
			return string(b), nil
		}
		out := checkCIOutput{
			CIStatus: "error",
			Details:  fmt.Sprintf("gh pr list: %v", ghErr),
		}
		b, _ := json.Marshal(out)
		return string(b), nil
	}

	var prs []struct {
		Number            int    `json:"number"`
		State             string `json:"state"`
		URL               string `json:"url"`
		StatusCheckRollup []struct {
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
		} `json:"statusCheckRollup"`
	}
	if err := json.Unmarshal(ghOut, &prs); err != nil {
		out := checkCIOutput{
			CIStatus: "error",
			Details:  fmt.Sprintf("parse gh output: %v", err),
		}
		b, _ := json.Marshal(out)
		return string(b), nil
	}
	if len(prs) == 0 {
		out := checkCIOutput{
			CIStatus: "no_pr",
			Details:  fmt.Sprintf("no PR found for branch %q", branch),
		}
		b, _ := json.Marshal(out)
		return string(b), nil
	}

	pr := prs[0]
	prURL := pr.URL

	if len(pr.StatusCheckRollup) == 0 {
		out := checkCIOutput{CIStatus: "passing", PRURL: prURL, Details: "no checks configured"}
		b, _ := json.Marshal(out)
		return string(b), nil
	}

	hasFailure := false
	hasPending := false
	for _, check := range pr.StatusCheckRollup {
		if check.Status != "COMPLETED" {
			hasPending = true
			continue
		}
		switch check.Conclusion {
		case "FAILURE", "CANCELLED", "ACTION_REQUIRED", "TIMED_OUT", "STARTUP_FAILURE":
			hasFailure = true
		}
	}

	if hasFailure {
		out := checkCIOutput{CIStatus: "failing", PRURL: prURL, Details: "checks failed"}
		b, _ := json.Marshal(out)
		return string(b), nil
	}
	if hasPending {
		out := checkCIOutput{CIStatus: "pending", PRURL: prURL, Details: "checks in progress"}
		b, _ := json.Marshal(out)
		return string(b), nil
	}
	out := checkCIOutput{CIStatus: "passing", PRURL: prURL, Details: "all checks passed"}
	b, _ := json.Marshal(out)
	return string(b), nil
}

// buildPrompt constructs the agent prompt matching clew-run.sh patterns.
func buildPrompt(in runPhaseInput, role, td, protocolPath string) (string, error) {
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
		// For tasks with an approved plan, skip explore/plan/review and go straight to implementation.
		planPath := filepath.Join(td, "artifacts", "plan.md")
		if planData, err := os.ReadFile(planPath); err == nil {
			return fmt.Sprintf(
				"You are a developer agent implementing an APPROVED plan. Do NOT re-explore, re-plan, or re-review the plan — those phases are done. Go directly to implementation.\n\nProject: %s. Task ID: %s.\n\nFirst read these files to understand what to build:\n- %s/TASK.md\n- %s/STATUS.md\n\nThen read the APPROVED PLAN below and IMPLEMENT it:\n\n--- APPROVED PLAN (%s/artifacts/plan.md) ---\n%s\n--- END PLAN ---\n\nYour job: implement every item in this plan. Modify the code files, write tests, run the test suite, and fix any failures. When done, write implementation notes to %s/artifacts/implementation.md, commit your changes with a descriptive message, and push to the feature branch. Do NOT stop at exploration or planning — produce actual code changes.",
				in.Project, in.TaskID, taskPath, taskPath, taskPath, string(planData), taskPath,
			), nil
		}
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
	case "create_pr":
		return "create_pr"
	case "ci_wait":
		return "ci_wait"
	case "ci_fix":
		return "ci_fix"
	case "merge":
		return "merge"
	default:
		return wfPhase
	}
}


	// phaseOrder maps clew status phases to their ordinal position.
	var phaseOrder = map[string]int{
		"queued": 0, "exploring": 1, "planning": 2, "plan_review": 3,
		"implementing": 4, "impl_review": 5,
		"create_pr": 6, "ci_wait": 7, "ci_fix": 8, "merge": 9,
		"done": 10,
	}

	// laterPhase returns whichever phase is further along in the lifecycle.
	func laterPhase(a, b string) string {
		if phaseOrder[a] >= phaseOrder[b] {
			return a
		}
		return b
	}
// matchReviewOutcome checks a single line for a review outcome marker.
// Returns "PASS", "BLOCKER", "SHOULD_FIX", or "".
func matchReviewOutcome(line string) string {
	// 1. Bracketed [OUTCOME:XXX] — canonical format (case-sensitive, HasPrefix).
	if strings.HasPrefix(line, "[OUTCOME:") {
		switch {
		case strings.HasPrefix(line, "[OUTCOME:PASS]"):
			return "PASS"
		case strings.HasPrefix(line, "[OUTCOME:APPROVED]"):
			return "PASS"
		case strings.HasPrefix(line, "[OUTCOME:BLOCKER]"):
			return "BLOCKER"
		case strings.HasPrefix(line, "[OUTCOME:SHOULD_FIX]"):
			return "SHOULD_FIX"
		}
	}

	// 2. **Verdict:...** — bold verdict line (case-insensitive word match).
	// Handle both **Verdict:** WORD and **Verdict: WORD** formats.
	if idx := strings.Index(line, "**Verdict:"); idx >= 0 {
		rest := strings.TrimSpace(line[idx+len("**Verdict:"):])
		rest = strings.TrimLeft(rest, "*")
		rest = strings.TrimSpace(rest)
		if end := strings.Index(rest, "**"); end >= 0 {
			rest = strings.TrimSpace(rest[:end])
		}
		fields := strings.Fields(rest)
		if len(fields) > 0 {
			w := fields[0]
			if strings.EqualFold(w, "PASS") || strings.EqualFold(w, "APPROVED") || strings.EqualFold(w, "PASSES") {
				return "PASS"
			}
		}
		return ""
	}

	// 3. Bare OUTCOME: XXX (no brackets, case-sensitive, HasPrefix).
	if strings.HasPrefix(line, "OUTCOME: ") {
		switch {
		case strings.HasPrefix(line, "OUTCOME: PASS"):
			return "PASS"
		case strings.HasPrefix(line, "OUTCOME: APPROVED"):
			return "PASS"
		case strings.HasPrefix(line, "OUTCOME: BLOCKER"):
			return "BLOCKER"
		case strings.HasPrefix(line, "OUTCOME: SHOULD_FIX"):
			return "SHOULD_FIX"
		}
	}

	// 4. Legacy bare [BLOCKER] / [SHOULD_FIX] (Contains not HasPrefix — the
	// caller skips table rows; matchReviewOutcome sees them as BLOCKER/SHOULD_FIX
	// because [BLOCKER] appears anywhere on the line. Rule 1 catches
	// [OUTCOME:BLOCKER] first so there's no double-detection).
	if strings.Contains(line, "[BLOCKER]") {
		return "BLOCKER"
	}
	if strings.Contains(line, "[SHOULD_FIX]") {
		return "SHOULD_FIX"
	}

	return ""
}

// determineReviewOutcome scans the latest review round artifact for outcome markers.
// Only the most recent round counts — stale BLOCKERs from earlier rounds are ignored.
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

	// Find the latest review round files by prefix + round number.
	// Matches "review-plan-round3-claude.md" or "review-plan-round1.md".
	// Returns all files for the highest round number.
	findLatestRoundFiles := func(prefix string) []string {
		var latestN int
		var all []string
		marker := prefix + "-round"
		for _, name := range artifacts {
			if !strings.HasPrefix(name, marker) || !strings.HasSuffix(name, ".md") {
				continue
			}
			// Extract round number: "review-plan-round3-claude.md" -> "3-claude"
			rest := strings.TrimSuffix(strings.TrimPrefix(name, marker), ".md")
			if rest == "" {
				continue
			}
			// rest could be "3" or "3-claude" — take the leading digits
			numStr := rest
			if dash := strings.Index(rest, "-"); dash >= 0 {
				numStr = rest[:dash]
			}
			n, err := strconv.Atoi(numStr)
			if err != nil {
				continue
			}
			if n > latestN {
				latestN = n
				all = nil
			}
			if n == latestN {
				all = append(all, name)
			}
		}
		return all
	}

	hasBlocker := false
	hasShouldFix := false
	scannedAny := false

	for _, prefix := range []string{"review-plan", "review-impl"} {
		latestFiles := findLatestRoundFiles(prefix)
		if len(latestFiles) == 0 {
			// Fall back to legacy unversioned file name
			legacy := prefix + ".md"
			for _, name := range artifacts {
				if name == legacy {
					latestFiles = []string{legacy}
					break
				}
			}
		}
		for _, latest := range latestFiles {
			data, err := os.ReadFile(filepath.Join(artifactsDir, latest))
			if err != nil {
				continue
			}
			scannedAny = true
			for _, line := range strings.Split(string(data), "\n") {
				line = strings.TrimSpace(line)
				if line == "" || strings.HasPrefix(line, "|") {
					continue
				}
				switch matchReviewOutcome(line) {
				case "PASS":
					// PASS from one model is good but keep scanning other models for BLOCKERs
				case "BLOCKER":
					hasBlocker = true
				case "SHOULD_FIX":
					hasShouldFix = true
				}
			}
		}
		// After scanning all models in the latest round, check aggregated result
		if scannedAny && !hasBlocker {
			return "PASS"
		}
	}
	if hasBlocker {
		return "BLOCKER"
	}
	if hasShouldFix {
		return "SHOULD_FIX"
	}
	if scannedAny || len(artifacts) > 0 {
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
		return "create_pr"
	case "create_pr":
		return "ci_wait"
	case "ci_fix":
		return "ci_wait"
	case "merge":
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
			for _, line := range strings.Split(string(data), "\n") {
				line = strings.TrimSpace(line)
				if line == "" || strings.HasPrefix(line, "|") {
					continue
				}
				outcome := matchReviewOutcome(line)
				if outcome == "BLOCKER" || outcome == "SHOULD_FIX" {
					count++
				}
			}
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

// tailLines returns the last n lines of s. If n <= 0, returns empty string.
func tailLines(s string, n int) string {
	if n <= 0 {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
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

