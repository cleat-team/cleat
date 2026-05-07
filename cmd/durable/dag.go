package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	dagplugin "github.com/rcownie/durable/plugins/dag"
)

// runDag dispatches to the appropriate dag subcommand.
func runDag(args []string) {
	if len(args) < 1 {
		dagUsage()
	}

	subCmd := args[0]
	remainder := args[1:]

	switch subCmd {
	case "validate":
		runDagValidate(remainder)
	case "run":
		runDagRun(remainder)
	case "generate":
		runDagGenerate(remainder)
	default:
		fmt.Fprintf(os.Stderr, "Unknown dag subcommand: %s\n\n", subCmd)
		dagUsage()
	}
}

func dagUsage() {
	fmt.Fprintf(os.Stderr, "Usage: durable dag <validate|run|generate> [options] <spec.json>\n\n")
	fmt.Fprintf(os.Stderr, "Subcommands:\n")
	fmt.Fprintf(os.Stderr, "  validate <spec.json>                         Parse spec, check for errors\n")
	fmt.Fprintf(os.Stderr, "  run [--input <json>] [--output <file>] <spec.json>\n")
	fmt.Fprintf(os.Stderr, "                                               Generate a dev workflow\n")
	fmt.Fprintf(os.Stderr, "  generate [--output <file>] <spec.json>       Generate a compilable workflow file\n")
	os.Exit(1)
}

// ---------------------------------------------------------------------------
// validate
// ---------------------------------------------------------------------------

func runDagValidate(args []string) {
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "Usage: durable dag validate <spec.json>\n")
		os.Exit(1)
	}

	specPath := args[0]
	f, err := os.Open(specPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening spec %q: %v\n", specPath, err)
		os.Exit(1)
	}
	defer f.Close()

	// Pass nil registry — we're only validating structure.
	_, err = dagplugin.LoadFromJSON(f, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Validation failed:\n  %v\n", err)
		os.Exit(1)
	}

	fmt.Println("DAG spec is valid.")
}

// ---------------------------------------------------------------------------
// run — manual flag parsing (flags can be mixed with positional args)
// ---------------------------------------------------------------------------

func runDagRun(args []string) {
	inputJSON := "{}"
	outputPath := "dag_workflow.go"

	// Manually extract --input and --output from args, like runDev does.
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--input" || args[i] == "-i":
			if i+1 < len(args) {
				inputJSON = args[i+1]
				args = append(args[:i], args[i+2:]...)
				i--
			}
		case strings.HasPrefix(args[i], "--input="):
			inputJSON = strings.TrimPrefix(args[i], "--input=")
			args = append(args[:i], args[i+1:]...)
			i--
		case args[i] == "--output" || args[i] == "-o":
			if i+1 < len(args) {
				outputPath = args[i+1]
				args = append(args[:i], args[i+2:]...)
				i--
			}
		case strings.HasPrefix(args[i], "--output="):
			outputPath = strings.TrimPrefix(args[i], "--output=")
			args = append(args[:i], args[i+1:]...)
			i--
		}
	}

	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "Usage: durable dag run [--input <json>] [--output <file>] <spec.json>\n")
		os.Exit(1)
	}

	specPath := args[0]

	// Read and validate the spec.
	spec, err := readSpec(specPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error reading spec: %v\n", err)
		os.Exit(1)
	}

	if _, err := validateSpec(spec); err != nil {
		fmt.Fprintf(os.Stderr, "Invalid DAG spec:\n  %v\n", err)
		os.Exit(1)
	}

	// Generate a dev Go program.
	src := generateDevProgram(spec, inputJSON)

	if err := os.WriteFile(outputPath, src, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "Error writing %q: %v\n", outputPath, err)
		os.Exit(1)
	}

	fmt.Printf("Generated DAG dev workflow: %s\n", outputPath)
	fmt.Println()
	fmt.Println("Fill in the task function stubs, then run with:")
	fmt.Printf("  go run ./%s '%s'\n", filepath.Base(outputPath), inputJSON)
	fmt.Println()
	fmt.Println("Or build and deploy with:")
	fmt.Printf("  durable build -o ./out ./%s\n", filepath.Dir(outputPath))
}

// ---------------------------------------------------------------------------
// generate — manual flag parsing
// ---------------------------------------------------------------------------

func runDagGenerate(args []string) {
	outputPath := "dag_workflow.go"

	// Manually extract --output.
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--output" || args[i] == "-o":
			if i+1 < len(args) {
				outputPath = args[i+1]
				args = append(args[:i], args[i+2:]...)
				i--
			}
		case strings.HasPrefix(args[i], "--output="):
			outputPath = strings.TrimPrefix(args[i], "--output=")
			args = append(args[:i], args[i+1:]...)
			i--
		}
	}

	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "Usage: durable dag generate [--output <file>] <spec.json>\n")
		os.Exit(1)
	}

	specPath := args[0]

	spec, err := readSpec(specPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error reading spec: %v\n", err)
		os.Exit(1)
	}

	if _, err := validateSpec(spec); err != nil {
		fmt.Fprintf(os.Stderr, "Invalid DAG spec:\n  %v\n", err)
		os.Exit(1)
	}

	// Generate a production workflow file.
	src := generateWorkflowFile(spec)

	if err := os.WriteFile(outputPath, src, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "Error writing output: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Generated DAG workflow: %s\n", outputPath)
	fmt.Println("Fill in the task function stubs and build with:")
	fmt.Printf("  durable build -o ./out ./%s\n", filepath.Dir(outputPath))
}

// ---------------------------------------------------------------------------
// shared helpers
// ---------------------------------------------------------------------------

// readSpec opens and decodes a JSON DAG spec file.
func readSpec(path string) (*dagplugin.DAGSpec, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var spec dagplugin.DAGSpec
	if err := json.NewDecoder(f).Decode(&spec); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return &spec, nil
}

// validateSpec checks the spec for structural errors without needing a registry.
func validateSpec(spec *dagplugin.DAGSpec) (*dagplugin.DAG, error) {
	return dagplugin.LoadFromJSON(
		strings.NewReader(mustMarshalJSON(spec)),
		nil,
	)
}

// mustMarshalJSON marshals v to JSON, panicking on error (safe for known types).
func mustMarshalJSON(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// ---------------------------------------------------------------------------
// code generation
// ---------------------------------------------------------------------------

// generateDevProgram generates a dev-mode Go program (with //go:build ignore)
// that defines stub task functions and a main() exercising the DAG.
func generateDevProgram(spec *dagplugin.DAGSpec, inputJSON string) []byte {
	var b strings.Builder

	b.WriteString("//go:build ignore\n")
	b.WriteString("// Code generated by \"durable dag run\"; DO NOT EDIT.\n\n")
	b.WriteString("package main\n\n")
	b.WriteString("import (\n")
	b.WriteString("\t\"encoding/json\"\n")
	b.WriteString("\t\"fmt\"\n")
	b.WriteString("\t\"log\"\n")
	b.WriteString("\t\"os\"\n")
	b.WriteString("\n")
	b.WriteString("\tdagplugin \"github.com/rcownie/durable/plugins/dag\"\n")
	b.WriteString(")\n\n")

	// Generate stub task functions.
	for _, task := range spec.Tasks {
		fmt.Fprintf(&b, "func %s(ctx *dagplugin.TaskContext) (string, error) {\n", task.Fn)
		fmt.Fprintf(&b, "\treturn \"\", fmt.Errorf(\"TODO: implement %s\")\n", task.Fn)
		b.WriteString("}\n\n")
	}

	// main function.
	b.WriteString("func main() {\n")
	b.WriteString("\tinputJSON := \"\"\n")
	b.WriteString("\tif len(os.Args) > 1 {\n")
	b.WriteString("\t\tinputJSON = os.Args[1]\n")
	b.WriteString("\t}\n\n")
	b.WriteString("\tvar input interface{}\n")
	b.WriteString("\tif inputJSON != \"\" {\n")
	b.WriteString("\t\tif err := json.Unmarshal([]byte(inputJSON), &input); err != nil {\n")
	b.WriteString("\t\t\tlog.Fatalf(\"error parsing input: %%v\", err)\n")
	b.WriteString("\t\t}\n")
	b.WriteString("\t}\n\n")

	// Build the DAG.
	b.WriteString("\td := dagplugin.NewDAG()\n")
	for _, task := range spec.Tasks {
		parents := formatParents(task.Parents)
		fmt.Fprintf(&b, "\td.AddTask(%q, %s, %s)\n", task.Name, parents, task.Fn)
	}

	// Execute.
	lastTask := spec.Tasks[len(spec.Tasks)-1]
	b.WriteString("\n")
	b.WriteString("\tfmt.Println(\"Executing DAG...\")\n")
	b.WriteString("\t// NOTE: task stubs return errors. Replace them with real implementations.\n")
	b.WriteString("\tif err := d.Execute(nil, input); err != nil {\n")
	b.WriteString("\t\tlog.Fatalf(\"DAG execution failed: %%v\", err)\n")
	b.WriteString("\t}\n")
	fmt.Fprintf(&b, "\tresult, ok := d.Output(%q)\n", lastTask.Name)
	b.WriteString("\tif !ok {\n")
	b.WriteString("\t\tlog.Fatalf(\"no output for final task\")\n")
	b.WriteString("\t}\n")
	b.WriteString("\tfmt.Println(\"Result:\", result)\n")
	b.WriteString("}\n")

	return []byte(b.String())
}

// generateWorkflowFile generates a production-ready Go workflow file with
// //go:wasmexport annotation and stub task functions.
func generateWorkflowFile(spec *dagplugin.DAGSpec) []byte {
	var b strings.Builder

	b.WriteString("// Code generated by \"durable dag generate\"; DO NOT EDIT.\n\n")
	b.WriteString("package main\n\n")
	b.WriteString("import (\n")
	b.WriteString("\t\"encoding/json\"\n")
	b.WriteString("\t\"fmt\"\n")
	b.WriteString("\n")
	b.WriteString("\t\"github.com/rcownie/durable/durable\"\n")
	b.WriteString("\tdagplugin \"github.com/rcownie/durable/plugins/dag\"\n")
	b.WriteString(")\n\n")

	// Generate an input type.
	b.WriteString("// Input is the input to the DAG workflow.\n")
	b.WriteString("type Input struct {\n")
	b.WriteString("\tData json.RawMessage `json:\"data\"`\n")
	b.WriteString("}\n\n")

	// Generate task function stubs.
	for _, task := range spec.Tasks {
		fmt.Fprintf(&b, "// %s is a DAG task function — fill in the implementation.\n", task.Fn)
		fmt.Fprintf(&b, "func %s(ctx *dagplugin.TaskContext) (string, error) {\n", task.Fn)
		fmt.Fprintf(&b, "\treturn \"\", fmt.Errorf(\"TODO: implement %s\")\n", task.Fn)
		b.WriteString("}\n\n")
	}

	// Generate the pipeline entry point.
	pipelineName := "Pipeline"
	if spec.Name != "" {
		pipelineName = exportedName(spec.Name)
	}
	fmt.Fprintf(&b, "//go:wasmexport %s\n", pipelineName)
	fmt.Fprintf(&b, "func %s(h durable.HostCalls, input Input) (string, error) {\n", pipelineName)
	b.WriteString("\td := dagplugin.NewDAG()\n")
	for _, task := range spec.Tasks {
		parents := formatParents(task.Parents)
		fmt.Fprintf(&b, "\td.AddTask(%q, %s, %s)\n", task.Name, parents, task.Fn)
	}

	// Execute and return the last task's output.
	lastTask := spec.Tasks[len(spec.Tasks)-1]
	b.WriteString("\n")
	b.WriteString("\tinputData, _ := json.Marshal(input)\n")
	b.WriteString("\tif err := d.Execute(h, inputData); err != nil {\n")
	b.WriteString("\t\treturn \"\", err\n")
	b.WriteString("\t}\n\n")
	fmt.Fprintf(&b, "\tresult, ok := d.Output(%q)\n", lastTask.Name)
	b.WriteString("\tif !ok {\n")
	fmt.Fprintf(&b, "\t\treturn \"\", fmt.Errorf(\"dag: no output for %%s\", %q)\n", lastTask.Name)
	b.WriteString("\t}\n")
	b.WriteString("\treturn result, nil\n")
	b.WriteString("}\n")

	return []byte(b.String())
}

// formatParents formats a parent slice as a Go literal, or "nil" for empty.
func formatParents(parents []string) string {
	if len(parents) == 0 {
		return "nil"
	}
	elems := make([]string, len(parents))
	for i, p := range parents {
		elems[i] = fmt.Sprintf("%q", p)
	}
	return "[]string{" + strings.Join(elems, ", ") + "}"
}

// exportedName converts a name to an exported Go identifier.
func exportedName(name string) string {
	if name == "" {
		return "Pipeline"
	}
	// Split on non-alphanumeric characters.
	parts := strings.FieldsFunc(name, func(r rune) bool {
		return !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'))
	})
	for i, p := range parts {
		if len(p) > 0 {
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	result := strings.Join(parts, "")
	if result == "" {
		return "Pipeline"
	}
	return result
}
