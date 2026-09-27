package main

import (
	"embed"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
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

// The fullstack template has a web/ subdirectory, so this pattern is the
// directory rather than templates/fullstack/* -- `*` does not descend, and a
// scaffold missing its front-end would still build and still be wrong.
//
//go:embed templates/fullstack
var fullstackTemplates embed.FS

func runInit(args []string) {
	flags := flag.NewFlagSet("init", flag.ExitOnError)
	templateName := flags.String("template", "basic", "project template (basic, agent, agent-python, workflow, fullstack)")
	_ = flags.Parse(args)

	if flags.NArg() < 1 {
		fmt.Fprintf(os.Stderr, "Usage: cleat init [--template agent|basic|agent-python|workflow|fullstack] <project-name>\n")
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
	case "fullstack":
		scaffoldFullstack(projectName)
	default:
		fmt.Fprintf(os.Stderr, "Error: unknown template %q. Valid: basic, agent, agent-python, workflow, fullstack\n", *templateName)
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
	h.DurableLog("hello: greeting")
	return `+"`"+`{"greeting":"hello, world"}`+"`"+`, nil
}
`), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	// cleat#1888: main.go imports github.com/cleat-team/cleat/cleat, and
	// nothing here named a go.mod for that import to resolve against -- a
	// user's first command met a raw Go toolchain error with no cleat
	// context ("go.mod file not found").
	goModPath := filepath.Join(dir, "go.mod")
	if err := os.WriteFile(goModPath, []byte(scaffoldBasicGoMod(projectName)), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	writeYAML(dir, projectName)
	tidyScaffold(dir)
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
	copyTemplate("go.mod.txt", "go.mod")
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
	tidyScaffold(dir)
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

	tidyScaffold(dir)
	fmt.Printf("Created workflow project in %s/\n", dir)
}

// scaffoldFullstack writes a project wiring a durable command path, the
// plugins in front of it, and a front-end.
//
// It walks the embedded tree rather than listing filenames, because this
// template has a subdirectory: a hand-maintained list silently stops copying
// whatever someone adds later, and the failure is a scaffold that is merely
// incomplete rather than broken -- which nobody notices.
func scaffoldFullstack(projectName string) {
	dir := projectName
	const root = "templates/fullstack"

	entries, err := fs.Sub(fullstackTemplates, root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	err = fs.WalkDir(entries, ".", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dir, path), 0755)
		}
		data, rerr := fs.ReadFile(entries, path)
		if rerr != nil {
			return rerr
		}
		dest := path
		// go.mod.txt is stored under that name so it is not treated as this
		// repository's own go.mod, exactly as the workflow template does.
		if dest == "go.mod.txt" {
			dest = "go.mod"
		}
		if strings.HasSuffix(dest, ".go") {
			data = stripScaffoldBuildTag(data)
		}
		return os.WriteFile(filepath.Join(dir, dest), data, 0644)
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	tidyScaffold(dir)
	fmt.Printf("Created full-stack project in %s/\n", dir)
	fmt.Printf("  next: cd %s && make up && make logs | grep rate-limiter\n", dir)
	fmt.Printf("  the rate limiter must report mode=db; see README.md\n")
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

// tidyScaffold resolves the scaffold's dependencies and writes its go.sum.
//
// WHY THE SCAFFOLD DOES NOT PIN A VERSION ITSELF. The generated code imports
// github.com/cleat-team/cleat/cleat, which is its own Go module (cleat/go.mod)
// and HAS NEVER BEEN TAGGED -- `go list -m -versions` on it returns nothing,
// while the parent module has v0.1.0 and v0.2.0. cleat#1888 shipped a version
// stamped into a require for the PARENT module, which the scaffold does not
// import, so `go mod tidy` rewrote it anyway and a scaffold that skipped tidy
// failed with "missing go.sum entry".
//
// So the go.mod written above carries `module` and `go` and nothing else, and
// this resolves the rest. That is correct whether or not the SDK is ever
// tagged: with a tag, tidy selects it; without one, tidy selects a
// pseudo-version off the default branch. Either way the scaffold builds, which
// stamping a version of a module it does not import never achieved.
//
// NOT FATAL ON FAILURE. tidy needs the network, and a user behind a proxy or
// offline should get a project plus one instruction, not no project. The
// message names the command rather than the failure, because "run go mod tidy"
// is the whole remedy.
func tidyScaffold(dir string) {
	cmd := exec.Command("go", "mod", "tidy")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "\nWarning: could not resolve dependencies (%v).\n"+
			"Run 'go mod tidy' in %s before building.\n%s\n", err, dir, out)
	}
}

// scaffoldBasicGoMod is basic's go.mod, as a literal rather than an embedded
// template: scaffoldBasic has no templates/basic/ directory (its one source
// file is written inline, a few lines above), and adding a whole embed.FS for
// one file would be a heavier change than the defect warrants.
func scaffoldBasicGoMod(projectName string) string {
	return fmt.Sprintf("module %s\n\ngo 1.24\n", projectName)
}
