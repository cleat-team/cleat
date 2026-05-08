package main

import (
	"flag"
	"testing"

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
