// Diamond DAG pipeline -- extract -> classify+translate -> summarize.
//
// Demonstrates:
//   - dagplugin.NewDAG / AddTask for building a diamond dependency graph
//   - dagplugin.(*DAG).Execute for level-by-level execution via ChildWorkflow/AwaitAllChildren
//   - Parent outputs flowing from upstream to downstream tasks via ParentOutput
//   - dagplugin.(*DAG).Output for retrieving individual task results after execution
//
// Build:
//
//	cleat build -o /tmp/out ./examples/dag/
package dagexample

import (
	"encoding/json"
	"fmt"

	"github.com/rcownie/cleat/durable"
	dagplugin "github.com/rcownie/cleat/plugins/dag"
)

// ---- Domain types ----

// DocumentInput is the input to the DAG pipeline.
type DocumentInput struct {
	Text string `json:"text"`
	Lang string `json:"lang,omitempty"`
}

// ---- Task functions ----

// extractText is the root task. It processes the raw document.
func extractText(ctx *dagplugin.TaskContext) (string, error) {
	data, ok := ctx.Input.(json.RawMessage)
	if !ok {
		return "", fmt.Errorf("extract: expected json.RawMessage input, got %T", ctx.Input)
	}

	var input DocumentInput
	if err := json.Unmarshal(data, &input); err != nil {
		return "", fmt.Errorf("extract: unmarshal input: %w", err)
	}

	// Call a document-processing service.
	result, err := ctx.H.DurableCall("docproc", "Extract", string(data))
	if err != nil {
		return "", fmt.Errorf("extract: %w", err)
	}
	return result, nil
}

// classifyDocument runs after extract and classifies the extracted content.
func classifyDocument(ctx *dagplugin.TaskContext) (string, error) {
	parentOut, err := ctx.ParentOutput("extract")
	if err != nil {
		return "", err
	}

	result, err := ctx.H.DurableCall("classify", "Classify", parentOut)
	if err != nil {
		return "", fmt.Errorf("classify: %w", err)
	}
	return result, nil
}

// translateDocument runs after extract and translates the extracted content.
func translateDocument(ctx *dagplugin.TaskContext) (string, error) {
	parentOut, err := ctx.ParentOutput("extract")
	if err != nil {
		return "", err
	}

	result, err := ctx.H.DurableCall("translate", "Translate", parentOut)
	if err != nil {
		return "", fmt.Errorf("translate: %w", err)
	}
	return result, nil
}

// summarizeDocument runs after classify and translate. It produces a summary
// using both parent outputs.
func summarizeDocument(ctx *dagplugin.TaskContext) (string, error) {
	classOut, err := ctx.ParentOutput("classify")
	if err != nil {
		return "", err
	}
	transOut, err := ctx.ParentOutput("translate")
	if err != nil {
		return "", err
	}

	input := map[string]string{
		"classification": classOut,
		"translation":    transOut,
	}
	inputJSON, _ := json.Marshal(input)

	result, err := ctx.H.DurableCall("summarize", "Summarize", string(inputJSON))
	if err != nil {
		return "", fmt.Errorf("summarize: %w", err)
	}
	return result, nil
}

// Pipeline is the durable workflow entry point. It builds a diamond DAG and
// executes it level by level:
//
//	extract -> classify + translate -> summarize
func Pipeline(h cleat.HostCalls, input DocumentInput) (string, error) {
	d := dagplugin.NewDAG()
	d.AddTask("extract", nil, extractText)
	d.AddTask("classify", []string{"extract"}, classifyDocument)
	d.AddTask("translate", []string{"extract"}, translateDocument)
	d.AddTask("summarize", []string{"classify", "translate"}, summarizeDocument)

	if err := d.Execute(h, input); err != nil {
		return "", err
	}

	result, ok := d.Output("summarize")
	if !ok {
		return "", fmt.Errorf("dag: no output for summarize task")
	}
	return result, nil
}
