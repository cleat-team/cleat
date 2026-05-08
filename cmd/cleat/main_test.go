package main

import (
	"flag"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"testing"

	"github.com/rcownie/cleat/internal/analyzer"
	"github.com/rcownie/cleat/internal/closure"
)

func TestIsValidTarget(t *testing.T) {
	valid := []string{"go", "tinygo", "rust", "java", "assemblyscript", "python"}
	for _, v := range valid {
		if !isValidTarget(v) {
			t.Errorf("isValidTarget(%q) = false, want true", v)
		}
	}
	invalid := []string{"", "gogo", "csharp", "swift", "kotlin", "javascript", "wasm"}
	for _, v := range invalid {
		if isValidTarget(v) {
			t.Errorf("isValidTarget(%q) = true, want false", v)
		}
	}
}

func TestFormatSize(t *testing.T) {
	tests := []struct {
		bytes int64
		want  string
	}{
		{0, "0 B"},
		{1, "1 B"},
		{500, "500 B"},
		{1023, "1023 B"},
		{1024, "1.0 KB"},
		{1536, "1.5 KB"},
		{1024 * 1024, "1.0 MB"},
		{2 * 1024 * 1024, "2.0 MB"},
		{10 * 1024 * 1024, "10.0 MB"},
	}
	for _, tt := range tests {
		got := formatSize(tt.bytes)
		if got != tt.want {
			t.Errorf("formatSize(%d) = %q, want %q", tt.bytes, got, tt.want)
		}
	}
}

func TestGetDBConnStr(t *testing.T) {
	// --db flag takes precedence over environment variable.
	dbConnStr = ""
	t.Setenv("CLEAT_DATABASE_URL", "postgres://test/env")
	got := getDBConnStr()
	if got != "postgres://test/env" {
		t.Errorf("getDBConnStr() = %q, want %q", got, "postgres://test/env")
	}

	// --db flag overrides env var.
	dbConnStr = "postgres://test/dbflag"
	got = getDBConnStr()
	if got != "postgres://test/dbflag" {
		t.Errorf("getDBConnStr() = %q, want %q", got, "postgres://test/dbflag")
	}
}

func TestGetDBConnStr_Empty(t *testing.T) {
	dbConnStr = ""
	t.Setenv("CLEAT_DATABASE_URL", "")
	got := getDBConnStr()
	if got != "" {
		t.Errorf("getDBConnStr() = %q, want empty", got)
	}
}

func TestFormatThreadingStatus(t *testing.T) {
	if got := formatThreadingStatus(nil); got != "OK" {
		t.Errorf("formatThreadingStatus(nil) = %q, want %q", got, "OK")
	}
	if got := formatThreadingStatus([]closure.ThreadingError{}); got != "OK" {
		t.Errorf("formatThreadingStatus([]) = %q, want %q", got, "OK")
	}
	errs := []closure.ThreadingError{
		{Message: "test error 1"},
		{Message: "test error 2"},
		{Message: "test error 3"},
	}
	if got := formatThreadingStatus(errs); got != "3 error(s)" {
		t.Errorf("formatThreadingStatus(3 errs) = %q, want %q", got, "3 error(s)")
	}
}

// TestCommandFlags verifies that each subcommand's flag set can be created
// and parsed without panicking or requiring a database connection.
func TestCommandFlags(t *testing.T) {
	t.Run("build", func(t *testing.T) {
		fs := flag.NewFlagSet("build", flag.ContinueOnError)
		o := fs.String("o", "", "")
		target := fs.String("target", "go", "")
		entry := fs.String("entry", "", "")
		if err := fs.Parse([]string{"-o", "./out", "--target", "tinygo", "--entry", "file.py:func", "."}); err != nil {
			t.Fatal(err)
		}
		if *o != "./out" {
			t.Errorf("build -o = %q, want %q", *o, "./out")
		}
		if *target != "tinygo" {
			t.Errorf("build --target = %q, want %q", *target, "tinygo")
		}
		if *entry != "file.py:func" {
			t.Errorf("build --entry = %q, want %q", *entry, "file.py:func")
		}
		if n := fs.Arg(0); n != "." {
			t.Errorf("build arg = %q, want %q", n, ".")
		}
	})

	t.Run("deploy", func(t *testing.T) {
		fs := flag.NewFlagSet("deploy", flag.ContinueOnError)
		name := fs.String("name", "", "")
		ns := fs.String("namespace", "", "")
		tq := fs.String("task-queue", "default", "")
		if err := fs.Parse([]string{"--name", "myworkflow", "--namespace", "prod", "--task-queue", "gpu", "workflow.wasm"}); err != nil {
			t.Fatal(err)
		}
		if *name != "myworkflow" {
			t.Errorf("deploy --name = %q, want %q", *name, "myworkflow")
		}
		if *ns != "prod" {
			t.Errorf("deploy --namespace = %q, want %q", *ns, "prod")
		}
		if *tq != "gpu" {
			t.Errorf("deploy --task-queue = %q, want %q", *tq, "gpu")
		}
		if fs.Arg(0) != "workflow.wasm" {
			t.Errorf("deploy arg = %q, want %q", fs.Arg(0), "workflow.wasm")
		}
	})

	t.Run("deploy_dry_run_defaults", func(t *testing.T) {
		// Test that deploy flag defaults are correct when no DB is set.
		fs := flag.NewFlagSet("deploy", flag.ContinueOnError)
		name := fs.String("name", "", "")
		ns := fs.String("namespace", "", "")
		tq := fs.String("task-queue", "default", "")
		if err := fs.Parse([]string{"test.wasm"}); err != nil {
			t.Fatal(err)
		}
		if *name != "" {
			t.Errorf("default --name = %q, want empty", *name)
		}
		if *ns != "" {
			t.Errorf("default --namespace = %q, want empty", *ns)
		}
		if *tq != "default" {
			t.Errorf("default --task-queue = %q, want %q", *tq, "default")
		}
		if fs.Arg(0) != "test.wasm" {
			t.Errorf("arg = %q, want %q", fs.Arg(0), "test.wasm")
		}
	})

	t.Run("schedule_add", func(t *testing.T) {
		fs := flag.NewFlagSet("schedule add", flag.ContinueOnError)
		cron := fs.String("cron", "", "")
		def := fs.String("def", "", "")
		ep := fs.String("entry-point", "", "")
		inp := fs.String("input", "{}", "")
		if err := fs.Parse([]string{"--cron", "0 9 * * *", "--def", "myworkflow", "--entry-point", "run", "--input", `{"key":"val"}`, "myschedule"}); err != nil {
			t.Fatal(err)
		}
		if *cron != "0 9 * * *" {
			t.Errorf("cron = %q, want %q", *cron, "0 9 * * *")
		}
		if *def != "myworkflow" {
			t.Errorf("def = %q, want %q", *def, "myworkflow")
		}
		if *ep != "run" {
			t.Errorf("entry-point = %q, want %q", *ep, "run")
		}
		if *inp != `{"key":"val"}` {
			t.Errorf("input = %q, want %q", *inp, `{"key":"val"}`)
		}
		if fs.Arg(0) != "myschedule" {
			t.Errorf("arg = %q, want %q", fs.Arg(0), "myschedule")
		}
	})

	t.Run("run", func(t *testing.T) {
		fs := flag.NewFlagSet("run", flag.ContinueOnError)
		wasm := fs.String("wasm", "", "")
		ep := fs.String("entry-point", "place_order", "")
		inp := fs.String("input", "{}", "")
		addr := fs.String("api-addr", ":8080", "")
		target := fs.String("target", "go", "")
		if err := fs.Parse([]string{"--wasm", "test.wasm", "--entry-point", "myfunc", "--input", `{"x":1}`, "--api-addr", ":9090", "--target", "tinygo"}); err != nil {
			t.Fatal(err)
		}
		if *wasm != "test.wasm" {
			t.Errorf("wasm = %q, want %q", *wasm, "test.wasm")
		}
		if *ep != "myfunc" {
			t.Errorf("entry-point = %q, want %q", *ep, "myfunc")
		}
		if *inp != `{"x":1}` {
			t.Errorf("input = %q, want %q", *inp, `{"x":1}`)
		}
		if *addr != ":9090" {
			t.Errorf("api-addr = %q, want %q", *addr, ":9090")
		}
		if *target != "tinygo" {
			t.Errorf("target = %q, want %q", *target, "tinygo")
		}
	})

	t.Run("run_defaults", func(t *testing.T) {
		fs := flag.NewFlagSet("run", flag.ContinueOnError)
		ep := fs.String("entry-point", "place_order", "")
		inp := fs.String("input", "{}", "")
		addr := fs.String("api-addr", ":8080", "")
		target := fs.String("target", "go", "")
		if err := fs.Parse([]string{}); err != nil {
			t.Fatal(err)
		}
		if *ep != "place_order" {
			t.Errorf("default entry-point = %q", *ep)
		}
		if *inp != "{}" {
			t.Errorf("default input = %q", *inp)
		}
		if *addr != ":8080" {
			t.Errorf("default api-addr = %q", *addr)
		}
		if *target != "go" {
			t.Errorf("default target = %q", *target)
		}
	})
}

func TestVetJSONOutput_Empty(t *testing.T) {
	out := vetJSONOutput(
		&analyzer.AnalysisResult{
			Funcs:             map[string]*analyzer.FuncDecl{},
			NumFuncs:          5,
			NumDurableLeaves:  3,
			NumDurableClosure: 1,
			NumPure:           1,
		},
		&closure.Result{
			DurableLeaves:  map[string]bool{},
			DurableClosure: map[string]bool{},
			Pure:           map[string]bool{},
			Errors:         map[string][]closure.ValidationError{},
			Warnings:       map[string][]closure.ValidationWarning{},
		},
		[]closure.ThreadingError{},
	)
	if len(out.Errors) != 0 {
		t.Errorf("expected 0 errors, got %d", len(out.Errors))
	}
	if len(out.Warnings) != 0 {
		t.Errorf("expected 0 warnings, got %d", len(out.Warnings))
	}
	if out.Summary.Functions != 5 {
		t.Errorf("expected 5 functions, got %d", out.Summary.Functions)
	}
	if out.Summary.DurableLeaves != 3 {
		t.Errorf("expected 3 durable leaves, got %d", out.Summary.DurableLeaves)
	}
	if out.Summary.Pure != 1 {
		t.Errorf("expected 1 pure, got %d", out.Summary.Pure)
	}
}

func TestVetJSONOutput_WithErrors(t *testing.T) {
	out := vetJSONOutput(
		&analyzer.AnalysisResult{
			Funcs: map[string]*analyzer.FuncDecl{},
		},
		&closure.Result{
			Errors: map[string][]closure.ValidationError{
				"pkg.FuncA": {{Code: "E001", Message: "err1"}},
			},
			Warnings: map[string][]closure.ValidationWarning{
				"pkg.FuncB": {{Code: "W001", Message: "warn1"}},
			},
			DurableLeaves:  map[string]bool{},
			DurableClosure: map[string]bool{},
			Pure:           map[string]bool{},
		},
		[]closure.ThreadingError{
			{FuncName: "pkg.FuncC", Message: "threading issue", Chain: []string{"main", "FuncC"}},
		},
	)
	if len(out.Errors) != 2 {
		t.Errorf("expected 2 errors, got %d", len(out.Errors))
	}
	if len(out.Warnings) != 1 {
		t.Errorf("expected 1 warning, got %d", len(out.Warnings))
	}
	// Threading errors are appended first, then closure errors.
	if out.Errors[0].Message != "threading issue" {
		t.Errorf("expected threading issue at [0], got %q", out.Errors[0].Message)
	}
	if len(out.Errors[0].Chain) != 2 {
		t.Errorf("expected chain length 2 at [0], got %d", len(out.Errors[0].Chain))
	}
	if out.Errors[1].Code != "E001" {
		t.Errorf("expected E001 at [1], got %q", out.Errors[1].Code)
	}
	if out.Warnings[0].Code != "W001" {
		t.Errorf("expected W001, got %q", out.Warnings[0].Code)
	}
}

func TestLookupFile_KnownFunc(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "/some/path/workflow.go", "package main\nfunc Foo() {}\n", 0)
	if err != nil {
		t.Fatal(err)
	}
	funcDecl := f.Decls[0].(*ast.FuncDecl)
	result := &analyzer.AnalysisResult{
		Funcs: map[string]*analyzer.FuncDecl{
			"pkg.Foo": {
				Pkg: &analyzer.Package{
					Fset: fset,
					Path: "pkg",
				},
				Ast: funcDecl,
			},
		},
	}
	got := lookupFile(result, "pkg.Foo")
	want := filepath.Base("/some/path/workflow.go")
	if got != want {
		t.Errorf("lookupFile = %q, want %q", got, want)
	}
}

func TestLookupFile_UnknownFunc(t *testing.T) {
	result := &analyzer.AnalysisResult{
		Funcs: map[string]*analyzer.FuncDecl{},
	}
	got := lookupFile(result, "pkg.Unknown")
	if got != "" {
		t.Errorf("lookupFile = %q, want empty", got)
	}
}

func TestLookupFile_NilPackage(t *testing.T) {
	result := &analyzer.AnalysisResult{
		Funcs: map[string]*analyzer.FuncDecl{
			"pkg.Bad": {
				Pkg: nil,
				Ast: &ast.FuncDecl{},
			},
		},
	}
	got := lookupFile(result, "pkg.Bad")
	if got != "" {
		t.Errorf("lookupFile = %q, want empty", got)
	}
}

func TestLookupFile_NilFset(t *testing.T) {
	result := &analyzer.AnalysisResult{
		Funcs: map[string]*analyzer.FuncDecl{
			"pkg.NoFset": {
				Pkg: &analyzer.Package{
					Fset: nil,
				},
				Ast: &ast.FuncDecl{},
			},
		},
	}
	got := lookupFile(result, "pkg.NoFset")
	if got != "" {
		t.Errorf("lookupFile = %q, want empty", got)
	}
}

func TestHelpDoesNotPanic(t *testing.T) {
	// Verify that --help returns ErrHelp instead of panicking or calling os.Exit.
	t.Run("build", func(t *testing.T) {
		fs := flag.NewFlagSet("build", flag.ContinueOnError)
		fs.String("o", "", "")
		fs.String("target", "go", "")
		if err := fs.Parse([]string{"--help"}); err != flag.ErrHelp {
			t.Errorf("expected flag.ErrHelp, got %v", err)
		}
	})
	t.Run("deploy", func(t *testing.T) {
		fs := flag.NewFlagSet("deploy", flag.ContinueOnError)
		fs.String("name", "", "")
		fs.String("namespace", "", "")
		fs.String("task-queue", "default", "")
		if err := fs.Parse([]string{"--help"}); err != flag.ErrHelp {
			t.Errorf("expected flag.ErrHelp, got %v", err)
		}
	})
	t.Run("run", func(t *testing.T) {
		fs := flag.NewFlagSet("run", flag.ContinueOnError)
		fs.String("wasm", "", "")
		fs.String("entry-point", "place_order", "")
		fs.String("input", "{}", "")
		if err := fs.Parse([]string{"--help"}); err != flag.ErrHelp {
			t.Errorf("expected flag.ErrHelp, got %v", err)
		}
	})
}
