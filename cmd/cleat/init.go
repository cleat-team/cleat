package main

import (
	"embed"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"
)

//go:embed templates/agent/*
var agentTemplates embed.FS

//go:embed templates/agent-python/*
var agentPythonTemplates embed.FS

//go:embed templates/workflow/*
var workflowTemplates embed.FS

func runInit(args []string) {
	flags := flag.NewFlagSet("init", flag.ExitOnError)
	templateName := flags.String("template", "basic", "project template (basic, agent, agent-python, workflow)")
	_ = flags.Parse(args)

	if flags.NArg() < 1 {
		fmt.Fprintf(os.Stderr, "Usage: cleat init [--template agent|basic|agent-python|workflow] <project-name>\n")
		os.Exit(1)
	}
	projectName := flags.Arg(0)

	switch *templateName {
	case "agent":
		scaffoldAgent(projectName)
	case "basic":
		scaffoldBasic(projectName)
	case "agent-python":
		scaffoldAgentPython(projectName)
	case "workflow":
		scaffoldWorkflow(projectName)
	default:
		fmt.Fprintf(os.Stderr, "Error: unknown template %q. Valid: basic, agent, agent-python, workflow\n", *templateName)
		os.Exit(1)
	}
}

func scaffoldBasic(projectName string) {
	dir := projectName
	if err := os.MkdirAll(dir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	mainGo := filepath.Join(dir, "main.go")
	if err := os.WriteFile(mainGo, []byte(`package main

import "github.com/cleat-team/cleat/cleat"

// @cleatEntry(name="hello")
func Hello(h cleat.HostCalls, input string) (string, error) {
	h.DurableLog("hello", "greeting")
	return `+"`"+`{"greeting":"hello, world"}`+"`"+`, nil
}
`), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	writeYAML(dir, projectName)
	fmt.Printf("Created basic project in %s/\n", dir)
}

func scaffoldAgent(projectName string) {
	dir := projectName
	if err := os.MkdirAll(dir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	copyTemplate := func(name, dest string) {
		data, err := agentTemplates.ReadFile("templates/agent/" + name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error reading template %s: %v\n", name, err)
			os.Exit(1)
		}
		if strings.HasSuffix(dest, ".go") {
			data = stripScaffoldBuildTag(data)
		}
		if err := os.WriteFile(filepath.Join(dir, dest), data, 0644); err != nil {
			fmt.Fprintf(os.Stderr, "Error writing %s: %v\n", dest, err)
			os.Exit(1)
		}
	}

	copyTemplate("workflow.go", "workflow.go")
	copyTemplate("tools.go", "tools.go")
	copyTemplate("docker-compose.yml", "docker-compose.yml")

	// README uses template substitution for project name.
	data, err := agentTemplates.ReadFile("templates/agent/README.md")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	tmpl, err := template.New("readme").Parse(string(data))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	f, err := os.Create(filepath.Join(dir, "README.md"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	defer f.Close()
	if err := tmpl.Execute(f, map[string]string{"ProjectName": projectName}); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	writeYAML(dir, projectName)
	fmt.Printf("Created AI agent project in %s/\n", dir)
}

func scaffoldAgentPython(projectName string) {
	dir := projectName
	if err := os.MkdirAll(dir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	copyTemplate := func(name, dest string) {
		data, err := agentPythonTemplates.ReadFile("templates/agent-python/" + name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error reading template %s: %v\n", name, err)
			os.Exit(1)
		}
		if strings.HasSuffix(dest, ".go") {
			data = stripScaffoldBuildTag(data)
		}
		if err := os.WriteFile(filepath.Join(dir, dest), data, 0644); err != nil {
			fmt.Fprintf(os.Stderr, "Error writing %s: %v\n", dest, err)
			os.Exit(1)
		}
	}

	copyTemplate("agent.py", "agent.py")
	copyTemplate("cleat.toml", "cleat.toml")
	copyTemplate(".gitignore", ".gitignore")
	copyTemplate("README.md", "README.md")
	copyTemplate("requirements.txt", "requirements.txt")

	fmt.Printf("Created Python agent project in %s/\n", dir)
}

func scaffoldWorkflow(projectName string) {
	dir := projectName
	if err := os.MkdirAll(dir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	copyTemplate := func(name, dest string) {
		data, err := workflowTemplates.ReadFile("templates/workflow/" + name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error reading template %s: %v\n", name, err)
			os.Exit(1)
		}
		if strings.HasSuffix(dest, ".go") {
			data = stripScaffoldBuildTag(data)
		}
		if err := os.WriteFile(filepath.Join(dir, dest), data, 0644); err != nil {
			fmt.Fprintf(os.Stderr, "Error writing %s: %v\n", dest, err)
			os.Exit(1)
		}
	}

	copyTemplate("main.go", "main.go")
	copyTemplate("main_test.go", "main_test.go")
	copyTemplate("cleat.yaml", "cleat.yaml")
	copyTemplate("go.mod.txt", "go.mod")
	copyTemplate("Makefile", "Makefile")
	copyTemplate("README.md", "README.md")
	copyTemplate("docker-compose.yml", "docker-compose.yml")

	fmt.Printf("Created workflow project in %s/\n", dir)
}

func writeYAML(dir, projectName string) {
	yamlContent := fmt.Sprintf("project: %q\nlanguage: go\nentry_points:\n  - name: agent\n    function: AgentLoop\n", projectName)
	_ = os.WriteFile(filepath.Join(dir, "cleat.yaml"), []byte(yamlContent), 0644)
}

// stripScaffoldBuildTag removes a leading `//go:build ignore` constraint (and
// the blank line after it) from an embedded Go template before it is written
// into a user's new project.
//
// Template .go files carry that constraint so they do NOT compile as packages
// of this repository. That is not tidiness: cmd/cleat/templates/workflow was
// a real package, it imported github.com/cleat-team/cleat/cleat, and that
// single import made the root module depend on the cleat/ module while cleat/
// already depended on the root -- a module cycle whose only fix was a
// `replace` directive in go.mod, which in turn makes
// `go install <pkg>@<version>` refuse the module outright:
//
//	"The go.mod file for the module providing named packages contains one or
//	 more replace directives."
//
// So README.md's `go install github.com/cleat-team/cleat/cmd/cleat@latest`
// could not work while these files compiled. Excluding them breaks the cycle.
//
// The constraint must not reach the user, though: before this function
// existed, `cleat init --template agent` copied templates/agent/workflow.go
// verbatim, `//go:build ignore` and all, so the generated project contained
// zero buildable Go files -- `go build ./...` in a fresh scaffold reported
// "matched no packages". Verified before the fix, and covered by
// TestScaffoldedGoFilesHaveNoBuildConstraint.
func stripScaffoldBuildTag(data []byte) []byte {
	s := string(data)
	for _, tag := range []string{"//go:build ignore\n", "// +build ignore\n"} {
		if strings.HasPrefix(s, tag) {
			s = strings.TrimPrefix(s, tag)
			s = strings.TrimPrefix(s, "\n")
		}
	}
	return []byte(s)
}
