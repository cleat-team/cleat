package main

import (
	"embed"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"text/template"
)

//go:embed templates/agent/*
var agentTemplates embed.FS

func runInit(args []string) {
	flags := flag.NewFlagSet("init", flag.ExitOnError)
	templateName := flags.String("template", "basic", "project template (basic, agent)")
	flags.Parse(args)

	if flags.NArg() < 1 {
		fmt.Fprintf(os.Stderr, "Usage: cleat init [--template agent|basic] <project-name>\n")
		os.Exit(1)
	}
	projectName := flags.Arg(0)

	switch *templateName {
	case "agent":
		scaffoldAgent(projectName)
	case "basic":
		scaffoldBasic(projectName)
	default:
		fmt.Fprintf(os.Stderr, "Error: unknown template %q. Valid: basic, agent\n", *templateName)
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

import "github.com/rcownie/cleat/durable"

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

func writeYAML(dir, projectName string) {
	yamlContent := fmt.Sprintf("project: %q\nlanguage: go\nentry_points:\n  - name: agent\n    function: AgentLoop\n", projectName)
	os.WriteFile(filepath.Join(dir, "cleat.yaml"), []byte(yamlContent), 0644)
}
