package main

import (
	"embed"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"text/template"

	"golang.org/x/mod/module"
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
		if dest == "go.mod" {
			data = substituteScaffoldModuleVersion(data)
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
		if dest == "go.mod" {
			data = substituteScaffoldModuleVersion(data)
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
			data = substituteScaffoldModuleVersion(data)
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

// scaffoldModuleVersionPlaceholder is the token a go.mod.txt template writes
// instead of a literal version, substituted at generation time by
// substituteScaffoldModuleVersion.
const scaffoldModuleVersionPlaceholder = "{{CLEAT_MODULE_VERSION}}"

// scaffoldLastKnownGoodVersion is the fallback used when the running binary's
// own build info does not name a real release -- update it when cutting a new
// tag. It is a FALLBACK, not the primary mechanism: a binary built from a
// tagged release (which is what a `go install`'d or released `cleat` is)
// always takes the branch below that reads its own version, so this constant
// going stale between releases costs nothing in the case that matters and
// only degrades the local-build case, which already has the full source tree.
const scaffoldLastKnownGoodVersion = "v0.2.0"

// cleatModuleVersion returns the module version a freshly scaffolded
// project's go.mod should require.
//
// cleat#1888: every go.mod.txt template hardcoded "v0.0.0", a version that
// was never tagged, so `go mod tidy` in a fresh scaffold failed outright with
// "unknown revision v0.0.0" for every user, on the first command any
// template's own README tells them to run.
//
// PREFER THE RUNNING BINARY'S OWN VERSION, so a generated project pins
// exactly the cleat that generated it -- consistent by construction, with
// nothing here to update at release time. debug.ReadBuildInfo reports this
// the same way `cleat version` already does (runVersion, this package).
//
// BUT NOT EVERY VALUE IT CAN REPORT IS USABLE. A plain local `go build`
// inside this repo's own checkout does NOT report "(devel)" the way older Go
// toolchains did -- verified empirically, 2026-09-18, go1.27: it reports a
// PSEUDO-version derived from the checkout's VCS state, e.g.
// "v0.1.1-0.20260918073609-1c504ef5bf65". module.IsPseudoVersion is the
// correct discriminator (this project already depends on golang.org/x/mod);
// a hand-rolled "does it look like vX.Y.Z" regex would work today but is the
// kind of check this repo's own guards have repeatedly shown drifts quietly
// as the version format's edge cases change.
//
// A pseudo-version is not necessarily UNRESOLVABLE -- if the exact commit it
// names is reachable from a public remote, go mod tidy can often fetch it --
// but it is not the stable, cache-friendly answer a fresh project should pin
// to, and a contributor's local build is exactly the case where falling back
// to a known-good tag costs nothing (they have the full source tree; they are
// not relying on the generated project's pin for anything).
func cleatModuleVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok || info.Main.Version == "" || info.Main.Version == "(devel)" {
		return scaffoldLastKnownGoodVersion
	}
	if module.IsPseudoVersion(info.Main.Version) {
		return scaffoldLastKnownGoodVersion
	}
	return info.Main.Version
}

// substituteScaffoldModuleVersion replaces scaffoldModuleVersionPlaceholder
// with the version cleatModuleVersion resolves to. A literal ReplaceAll
// rather than text/template: go.mod.txt is not template syntax anywhere else
// in it, and every other placeholder-bearing template in this package
// (README.md) already uses text/template for a DIFFERENT reason -- project
// NAME substitution, which every template needs and go.mod does not.
// Introducing text/template here for one token would mean two substitution
// mechanisms doing the same job.
func substituteScaffoldModuleVersion(data []byte) []byte {
	return []byte(strings.ReplaceAll(string(data), scaffoldModuleVersionPlaceholder, cleatModuleVersion()))
}

// scaffoldBasicGoMod is basic's go.mod, as a literal rather than an embedded
// template: scaffoldBasic has no templates/basic/ directory (its one source
// file is written inline, a few lines above), and adding a whole embed.FS for
// one file would be a heavier change than the defect warrants.
func scaffoldBasicGoMod(projectName string) string {
	return fmt.Sprintf("module %s\n\ngo 1.24\n\nrequire github.com/cleat-team/cleat %s\n",
		projectName, cleatModuleVersion())
}
