package workflow

import (
	"encoding/json"
	"fmt"

	"github.com/cleat-team/cleat/cleat"
)

var pluginPhases = []string{
	"explore",
	"plan",
	"review_plan",
	"implement",
	"review_impl",
}

const maxReviewRounds = 3

// TaskInput is the workflow input, JSON-marshaled by the cleat runtime.
type TaskInput struct {
	TaskID      string `json:"task_id"`
	ProjectRoot string `json:"project_root"`
	Project     string `json:"project"`
	Workdir     string `json:"workdir"`
	ParentRunID string `json:"parent_run_id,omitempty"`
}

// TaskOutput is the workflow return value.
type TaskOutput struct {
	TaskID           string   `json:"task_id"`
	FinalPhase       string   `json:"final_phase"`
	PhasesCompleted  []string `json:"phases_completed"`
	TotalPluginCalls int      `json:"total_plugin_calls"`
}

// PluginCallInput is the JSON payload sent to clew-executor:run_phase.
// Must match clew-executor's runPhaseInput struct exactly.
type PluginCallInput struct {
	TaskID       string `json:"task_id"`
	Project      string `json:"project"`
	ProjectRoot  string `json:"project_root"`
	Workdir      string `json:"workdir"`
	Phase        string `json:"phase,omitempty"`
	Model        string `json:"model,omitempty"`
	Tool         string `json:"tool,omitempty"`
	RoleOverride string `json:"role_override,omitempty"`
}

// PluginCallOutput is the structured result from clew-executor:run_phase.
type PluginCallOutput struct {
	ExitCode         int      `json:"exit_code"`
	PhaseChanged     bool     `json:"phase_changed"`
	NewPhase         string   `json:"new_phase"`
	ReviewOutcome    string   `json:"review_outcome"`
	ArtifactsWritten []string `json:"artifacts_written"`
	FindingsCount    int      `json:"findings_count"`
	Status           string   `json:"status"`
	Error            string   `json:"error,omitempty"`
	Cached           bool     `json:"cached"`
}

// HandleIncident is the cleat workflow entry point.
func HandleIncident(h cleat.HostCalls, task_id, project_root, project, workdir, tool, model, parent_run_id string) (string, error) {
	input := TaskInput{
		TaskID:      task_id,
		ProjectRoot: project_root,
		Project:     project,
		Workdir:     workdir,
		ParentRunID: parent_run_id,
	}

	output := runStateMachine(h, input)

	resultJSON, err := json.Marshal(output)
	if err != nil {
		return "", fmt.Errorf("marshaling output: %w", err)
	}
	return string(resultJSON), nil
}

func runStateMachine(h cleat.HostCalls, input TaskInput) TaskOutput {
	var completed []string
	callCount := 0

	for _, phase := range pluginPhases {
		statusPhase := phaseToStatusPhase(phase)
		setQueryState(h, "phase", statusPhase)

		result, err := callPlugin(h, input, phase)
		callCount++
		if err != nil || result.Status == "failed" {
			setQueryState(h, "phase", "blocked")
			return TaskOutput{
				TaskID:           input.TaskID,
				FinalPhase:       "blocked",
				PhasesCompleted:  completed,
				TotalPluginCalls: callCount,
			}
		}

		// Handle exploration phase outcomes (can diverge from linear flow).
		if phase == "explore" {
			if result.ExitCode != 0 {
				setQueryState(h, "phase", "failed")
				return TaskOutput{
					TaskID:           input.TaskID,
					FinalPhase:       "failed",
					PhasesCompleted:  completed,
					TotalPluginCalls: callCount,
				}
			}
			if result.PhaseChanged && result.NewPhase != "" {
				switch result.NewPhase {
				case "waiting_on_children":
					setQueryState(h, "phase", "waiting_on_children")
					h.DurableLog(fmt.Sprintf(
						"task %s needs decomposition, transitioning to waiting_on_children",
						input.TaskID))
					return TaskOutput{
						TaskID:           input.TaskID,
						FinalPhase:       "waiting_on_children",
						PhasesCompleted:  completed,
						TotalPluginCalls: callCount,
					}
				case "done":
					setQueryState(h, "phase", "done")
					return TaskOutput{
						TaskID:           input.TaskID,
						FinalPhase:       "done",
						PhasesCompleted:  completed,
						TotalPluginCalls: callCount,
					}
				case "failed":
					setQueryState(h, "phase", "failed")
					return TaskOutput{
						TaskID:           input.TaskID,
						FinalPhase:       "failed",
						PhasesCompleted:  completed,
						TotalPluginCalls: callCount,
					}
				}
			}
		}

		// Work phases (plan, implement): non-zero exit → failed.
		if phase == "plan" || phase == "implement" {
			if result.ExitCode != 0 {
				setQueryState(h, "phase", "failed")
				return TaskOutput{
					TaskID:           input.TaskID,
					FinalPhase:       "failed",
					PhasesCompleted:  completed,
					TotalPluginCalls: callCount,
				}
			}
		}

		// Non-review phases pass → continue to next phase.
		if phase != "review_plan" && phase != "review_impl" {
			completed = append(completed, phase)
			continue
		}

		// Review phase: handle review outcomes.
		if result.ReviewOutcome == "PASS" {
			completed = append(completed, phase)
			continue
		}

		// BLOCKER or SHOULD_FIX: loop back to the preceding work phase.
		// Both mean "author fixes and re-submits" per reviewer protocol.
		// The distinction only matters for whether the review PASSES.
		prevPhase := reviewToPrevPhase(phase)
		loopPassed := false
		for round := 1; round <= maxReviewRounds; round++ {
			setQueryState(h, "phase", phaseToStatusPhase(prevPhase))
			workResult, workErr := callPlugin(h, input, prevPhase)
			callCount++
			if workErr != nil {
				setQueryState(h, "phase", "blocked")
				return TaskOutput{
					TaskID:           input.TaskID,
					FinalPhase:       "blocked",
					PhasesCompleted:  completed,
					TotalPluginCalls: callCount,
				}
			}
			if workResult.ExitCode != 0 {
				setQueryState(h, "phase", "failed")
				return TaskOutput{
					TaskID:           input.TaskID,
					FinalPhase:       "failed",
					PhasesCompleted:  completed,
					TotalPluginCalls: callCount,
				}
			}

			setQueryState(h, "phase", phaseToStatusPhase(phase))
			result, err = callPlugin(h, input, phase)
			callCount++
			if err != nil {
				setQueryState(h, "phase", "blocked")
				return TaskOutput{
					TaskID:           input.TaskID,
					FinalPhase:       "blocked",
					PhasesCompleted:  completed,
					TotalPluginCalls: callCount,
				}
			}

			if result.ReviewOutcome == "PASS" {
				completed = append(completed, phase)
				loopPassed = true
				break
			}
			// BLOCKER or SHOULD_FIX: continue looping.
		}

		if !loopPassed {
			setQueryState(h, "phase", "failed")
			return TaskOutput{
				TaskID:           input.TaskID,
				FinalPhase:       "failed",
				PhasesCompleted:  completed,
				TotalPluginCalls: callCount,
			}
		}
	}

	// All phases completed.
	setQueryState(h, "phase", "done")

	if input.ParentRunID != "" {
		if err := h.SignalWorkflow(input.ParentRunID, "child_done", input.TaskID); err != nil {
			h.DurableLog(fmt.Sprintf("failed to signal parent: %v", err))
		}
	}

	return TaskOutput{
		TaskID:           input.TaskID,
		FinalPhase:       "done",
		PhasesCompleted:  completed,
		TotalPluginCalls: callCount,
	}
}

func phaseToStatusPhase(phase string) string {
	switch phase {
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
		return phase
	}
}

func reviewToPrevPhase(reviewPhase string) string {
	switch reviewPhase {
	case "review_plan":
		return "plan"
	case "review_impl":
		return "implement"
	default:
		return ""
	}
}

func setQueryState(h cleat.HostCalls, key, value string) {
	h.SetQueryState(key, value)
	h.DurableLog(fmt.Sprintf("phase=%s", value))
}

func callPlugin(h cleat.HostCalls, input TaskInput, phase string) (PluginCallOutput, error) {
	req := PluginCallInput{
		TaskID:      input.TaskID,
		Project:     input.Project,
		ProjectRoot: input.ProjectRoot,
		Workdir:     input.Workdir,
		Phase:       phase,
	}
	reqJSON, err := json.Marshal(req)
	if err != nil {
		return PluginCallOutput{}, fmt.Errorf("marshaling plugin input: %w", err)
	}

	resultJSON, err := h.PluginCall("clew-executor", "run_phase", string(reqJSON))
	if err != nil {
		return PluginCallOutput{}, fmt.Errorf("plugin call failed: %w", err)
	}

	var result PluginCallOutput
	if err := json.Unmarshal([]byte(resultJSON), &result); err != nil {
		return PluginCallOutput{}, fmt.Errorf("unmarshaling plugin result: %w", err)
	}
	return result, nil
}
