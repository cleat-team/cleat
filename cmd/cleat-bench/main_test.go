package main

import (
	"context"
	"flag"
	"testing"
	"time"
)

func TestBenchCallerCall(t *testing.T) {
	c := &benchCaller{}
	result, err := c.Call(context.Background(), "http", "fetch", `{"url":"http://example.com","method":"GET"}`)
	if err != nil {
		t.Fatalf("benchCaller.Call() error: %v", err)
	}
	if result != `{"status":"ok"}` {
		t.Errorf("benchCaller.Call() = %q, want %q", result, `{"status":"ok"}`)
	}
}

func TestBenchState(t *testing.T) {
	s := &benchState{version: 7, minVersion: 3}
	if v := s.Version(); v != 7 {
		t.Errorf("Version() = %d, want 7", v)
	}
	if v := s.MinVersion(); v != 3 {
		t.Errorf("MinVersion() = %d, want 3", v)
	}
}

func TestBenchState_Defaults(t *testing.T) {
	s := &benchState{}
	if v := s.Version(); v != 0 {
		t.Errorf("Version() = %d, want 0", v)
	}
	if v := s.MinVersion(); v != 0 {
		t.Errorf("MinVersion() = %d, want 0", v)
	}
}

func TestReportStats_NoData(t *testing.T) {
	// Should not panic when given an empty slice.
	reportStats("test", nil)
	reportStats("test", []time.Duration{})
}

func TestReportStats_WithData(t *testing.T) {
	// Should not panic when given data.
	reportStats("test", []time.Duration{
		100 * time.Millisecond,
		200 * time.Millisecond,
		300 * time.Millisecond,
	})
}

// TestBenchFlags verifies the cleat-bench flag set can be created and parsed
// without requiring a database connection.
func TestBenchFlags(t *testing.T) {
	fs := flag.NewFlagSet("cleat-bench", flag.ContinueOnError)
	db := fs.String("db", "", "")
	wf := fs.String("workflow", "", "")
	ep := fs.String("entry-point", "place_order", "")
	count := fs.Int("count", 100, "")
	conc := fs.Int("concurrency", 10, "")
	tq := fs.String("task-queue", "default", "")
	if err := fs.Parse([]string{
		"--db", "postgres://localhost/bench",
		"--workflow", "myworkflow",
		"--entry-point", "run",
		"--count", "50",
		"--concurrency", "5",
		"--task-queue", "gpu",
	}); err != nil {
		t.Fatal(err)
	}
	if *db != "postgres://localhost/bench" {
		t.Errorf("db = %q, want %q", *db, "postgres://localhost/bench")
	}
	if *wf != "myworkflow" {
		t.Errorf("workflow = %q, want %q", *wf, "myworkflow")
	}
	if *ep != "run" {
		t.Errorf("entry-point = %q, want %q", *ep, "run")
	}
	if *count != 50 {
		t.Errorf("count = %d, want 50", *count)
	}
	if *conc != 5 {
		t.Errorf("concurrency = %d, want 5", *conc)
	}
	if *tq != "gpu" {
		t.Errorf("task-queue = %q, want %q", *tq, "gpu")
	}
}

func TestBenchFlag_Defaults(t *testing.T) {
	fs := flag.NewFlagSet("cleat-bench", flag.ContinueOnError)
	db := fs.String("db", "", "")
	wf := fs.String("workflow", "", "")
	ep := fs.String("entry-point", "place_order", "")
	count := fs.Int("count", 100, "")
	conc := fs.Int("concurrency", 10, "")
	tq := fs.String("task-queue", "default", "")
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	if *db != "" {
		t.Errorf("default db = %q, want empty", *db)
	}
	if *wf != "" {
		t.Errorf("default workflow = %q, want empty", *wf)
	}
	if *ep != "place_order" {
		t.Errorf("default entry-point = %q, want %q", *ep, "place_order")
	}
	if *count != 100 {
		t.Errorf("default count = %d, want 100", *count)
	}
	if *conc != 10 {
		t.Errorf("default concurrency = %d, want 10", *conc)
	}
	if *tq != "default" {
		t.Errorf("default task-queue = %q, want %q", *tq, "default")
	}
}

// TestBenchHelpDoesNotPanic verifies that --help returns flag.ErrHelp
// instead of panicking or calling os.Exit.
func TestBenchHelpDoesNotPanic(t *testing.T) {
	fs := flag.NewFlagSet("cleat-bench", flag.ContinueOnError)
	fs.String("db", "", "")
	fs.String("workflow", "", "")
	fs.Int("count", 100, "")
	fs.Int("concurrency", 10, "")
	if err := fs.Parse([]string{"--help"}); err != flag.ErrHelp {
		t.Errorf("expected flag.ErrHelp, got %v", err)
	}
}

// TestBenchFunctionsLinkage verifies that benchmark functions referenced
// in main() are linked into the test binary (compile-time check).
func TestBenchFunctionsLinkage(t *testing.T) {
	_ = runBenchmark
	_ = runReplayBenchmark
	_ = reportStats
}
