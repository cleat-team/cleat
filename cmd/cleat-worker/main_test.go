package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
)

func TestGenerateWorkerID(t *testing.T) {
	id := generateWorkerID()
	if len(id) != 32 {
		t.Errorf("generateWorkerID() = %q (len %d), want 32 hex chars", id, len(id))
	}
	for _, c := range id {
		if !strings.ContainsRune("0123456789abcdef", c) {
			t.Errorf("generateWorkerID() = %q contains non-hex char %c", id, c)
		}
	}
}

func TestGenerateWorkerID_Unique(t *testing.T) {
	ids := make(map[string]bool)
	for i := 0; i < 100; i++ {
		id := generateWorkerID()
		if ids[id] {
			t.Errorf("duplicate worker ID after 100 iterations: %q", id)
		}
		ids[id] = true
	}
}

func TestGenerateTraceID(t *testing.T) {
	id := generateTraceID()
	if len(id) != 32 {
		t.Errorf("generateTraceID() = %q (len %d), want 32 hex chars", id, len(id))
	}
	for _, c := range id {
		if !strings.ContainsRune("0123456789abcdef", c) {
			t.Errorf("generateTraceID() = %q contains non-hex char %c", id, c)
		}
	}
}

func TestExtractTraceIDFromTraceParent(t *testing.T) {
	tests := []struct {
		name   string
		header string
		want   string
	}{
		{"valid", "00-abcdef0123456789abcdef0123456789-0123456789abcdef-01", "abcdef0123456789abcdef0123456789"},
		{"empty header", "", ""},
		{"single part", "00", ""},
		{"short trace ID", "00-abc-123-01", ""},
		{"long trace ID", "00-abcdef0123456789abcdef0123456789ff-123-01", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractTraceIDFromTraceParent(tt.header)
			if got != tt.want {
				t.Errorf("extractTraceIDFromTraceParent(%q) = %q, want %q", tt.header, got, tt.want)
			}
		})
	}
}

func TestIsConnectionError(t *testing.T) {
	connectionErrors := []string{
		"dial tcp: connection refused",
		"connection reset by peer",
		"connection closed",
		"no reachable servers",
		"server closed the connection",
		"connection timed out",
		"broken pipe",
		"unexpected EOF",
		"driver: bad connection",
	}
	for _, msg := range connectionErrors {
		if !isConnectionError(errors.New(msg)) {
			t.Errorf("isConnectionError(%q) = false, want true", msg)
		}
	}
}

func TestIsConnectionError_Negative(t *testing.T) {
	notConnectionErrors := []string{
		"",
		"some other error",
		"connection", // partial match of "connection" alone is not sufficient
		"syntax error at or near",
		"permission denied",
		"relation does not exist",
	}
	for _, msg := range notConnectionErrors {
		if isConnectionError(errors.New(msg)) {
			t.Errorf("isConnectionError(%q) = true, want false", msg)
		}
	}
}

func TestIsConnectionError_Nil(t *testing.T) {
	if isConnectionError(nil) {
		t.Error("isConnectionError(nil) = true, want false")
	}
}

func TestDetermineEntryPoint(t *testing.T) {
	tests := []struct {
		name  string
		input json.RawMessage
		want  string
	}{
		{"nil input", nil, ""},
		{"empty JSON", json.RawMessage(""), ""},
		{"empty object", json.RawMessage("{}"), ""},
		{"with entry point", json.RawMessage(`{"__entry_point":"myfunc"}`), "myfunc"},
		{"with entry point and other fields", json.RawMessage(`{"__entry_point":"handler","order_id":"abc"}`), "handler"},
		{"case sensitivity", json.RawMessage(`{"__entry_point":"handle_incident"}`), "handle_incident"},
		{"empty entry point value", json.RawMessage(`{"__entry_point":""}`), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := determineEntryPoint(tt.input, nil)
			if got != tt.want {
				t.Errorf("determineEntryPoint(%s) = %q, want %q", string(tt.input), got, tt.want)
			}
		})
	}
}

func TestBaseDSNFromURL(t *testing.T) {
	tests := []struct {
		url  string
		want string
	}{
		{
			url:  "postgres://user:pass@localhost:5432/cleat?sslmode=disable",
			want: "host=localhost port=5432 dbname=cleat sslmode=disable",
		},
		{
			url:  "postgres://user@host:5432/db",
			want: "host=host port=5432 dbname=db sslmode=disable",
		},
		{
			url:  "postgres://localhost/mydb",
			want: "host=localhost port=5432 dbname=mydb sslmode=disable",
		},
		{
			url:  "postgres://db.example.com:6432/production?sslmode=require",
			want: "host=db.example.com port=6432 dbname=production sslmode=require",
		},
	}
	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			got := baseDSNFromURL(tt.url)
			if got != tt.want {
				t.Errorf("baseDSNFromURL(%q) = %q, want %q", tt.url, got, tt.want)
			}
		})
	}
}

func TestBaseDSNFromURL_Invalid(t *testing.T) {
	// url.Parse is lenient; most "invalid" inputs still parse. These verify
	// the function returns something (doesn't panic) for edge-case inputs.
	_ = baseDSNFromURL("not-a-url")
	_ = baseDSNFromURL("")
	_ = baseDSNFromURL("postgres://")
}

func TestBaseDSNFromDSN(t *testing.T) {
	tests := []struct {
		dsn  string
		want string
	}{
		{"host=localhost port=5432 dbname=cleat user=admin password=secret sslmode=disable", "host=localhost port=5432 dbname=cleat sslmode=disable"},
		{"host=localhost dbname=test sslmode=require", "host=localhost dbname=test sslmode=require"},
		{"host=host1 port=5432 user=u password=p dbname=d", "host=host1 port=5432 dbname=d"},
	}
	for _, tt := range tests {
		t.Run(tt.dsn, func(t *testing.T) {
			got := baseDSNFromDSN(tt.dsn)
			if got != tt.want {
				t.Errorf("baseDSNFromDSN(%q) = %q, want %q", tt.dsn, got, tt.want)
			}
		})
	}
}

func TestBaseDSNFromDSN_Empty(t *testing.T) {
	if got := baseDSNFromDSN(""); got != "" {
		t.Errorf("baseDSNFromDSN(empty) = %q, want empty", got)
	}
}

func TestGenerateUpdatePromiseID(t *testing.T) {
	id, err := generateUpdatePromiseID()
	if err != nil {
		t.Fatalf("generateUpdatePromiseID() error: %v", err)
	}
	if !strings.HasPrefix(id, "upd-") {
		t.Errorf("generateUpdatePromiseID() = %q, want prefix %q", id, "upd-")
	}
	if len(id) != 4+32 { // "upd-" + 32 hex chars
		t.Errorf("generateUpdatePromiseID() = %q (len %d), want %d", id, len(id), 4+32)
	}
}

func TestGenerateUpdatePromiseID_Unique(t *testing.T) {
	ids := make(map[string]bool)
	for i := 0; i < 100; i++ {
		id, err := generateUpdatePromiseID()
		if err != nil {
			t.Fatal(err)
		}
		if ids[id] {
			t.Errorf("duplicate update promise ID: %q", id)
		}
		ids[id] = true
	}
}

func TestWorkerFlags(t *testing.T) {
	fs := flag.NewFlagSet("cleat-worker", flag.ContinueOnError)
	db := fs.String("db", "", "")
	conc := fs.Int("concurrency", 10, "")
	hb := fs.Duration("heartbeat", 5*time.Second, "")
	poll := fs.Duration("poll", 500*time.Millisecond, "")
	apiAddr := fs.String("api-addr", "", "")
	tq := fs.String("task-queue", "default", "")
	ct := fs.Int("compaction-threshold", engine.DefaultCompactionThreshold, "")
	ci := fs.Duration("compaction-interval", 5*time.Minute, "")
	sf := fs.String("shards-file", "", "")
	pc := fs.String("plugin-config", "", "")
	mwd := fs.Duration("max-workflow-duration", 0, "")
	if err := fs.Parse([]string{
		"--db", "postgres://localhost/cleat",
		"--concurrency", "20",
		"--heartbeat", "10s",
		"--poll", "1s",
		"--api-addr", ":9090",
		"--task-queue", "gpu,high-memory",
		"--compaction-threshold", "500",
		"--compaction-interval", "10m",
		"--max-workflow-duration", "2m",
	}); err != nil {
		t.Fatal(err)
	}
	if *db != "postgres://localhost/cleat" {
		t.Errorf("db = %q", *db)
	}
	if *conc != 20 {
		t.Errorf("concurrency = %d, want 20", *conc)
	}
	if *hb != 10*time.Second {
		t.Errorf("heartbeat = %v, want 10s", *hb)
	}
	if *poll != time.Second {
		t.Errorf("poll = %v, want 1s", *poll)
	}
	if *apiAddr != ":9090" {
		t.Errorf("api-addr = %q", *apiAddr)
	}
	if *tq != "gpu,high-memory" {
		t.Errorf("task-queue = %q", *tq)
	}
	if *ct != 500 {
		t.Errorf("compaction-threshold = %d", *ct)
	}
	if *ci != 10*time.Minute {
		t.Errorf("compaction-interval = %v, want 10m", *ci)
	}
	if *sf != "" {
		t.Errorf("shards-file = %q, want empty", *sf)
	}
	if *pc != "" {
		t.Errorf("plugin-config = %q, want empty", *pc)
	}
	if *mwd != 2*time.Minute {
		t.Errorf("max-workflow-duration = %v, want 2m", *mwd)
	}
}

func TestWorkerFlag_Defaults(t *testing.T) {
	fs := flag.NewFlagSet("cleat-worker", flag.ContinueOnError)
	db := fs.String("db", "", "")
	conc := fs.Int("concurrency", 10, "")
	tq := fs.String("task-queue", "default", "")
	ct := fs.Int("compaction-threshold", engine.DefaultCompactionThreshold, "")
	mwd := fs.Duration("max-workflow-duration", 0, "")
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	if *db != "" {
		t.Errorf("default db = %q, want empty", *db)
	}
	if *conc != 10 {
		t.Errorf("default concurrency = %d, want 10", *conc)
	}
	if *tq != "default" {
		t.Errorf("default task-queue = %q", *tq)
	}
	if *mwd != 0 {
		t.Errorf("default max-workflow-duration = %v, want 0", *mwd)
	}
	if *ct != engine.DefaultCompactionThreshold {
		t.Errorf("default compaction-threshold = %d, want %d", *ct, engine.DefaultCompactionThreshold)
	}
}

// TestWorkerFunctionsLinkage verifies that worker functions referenced in main()
// are linked into the test binary (compile-time check).
func TestWorkerFunctionsLinkage(t *testing.T) {
	var w *Worker
	_ = w.Run
	_ = generateWorkerID
	_ = generateTraceID
	_ = isConnectionError
	_ = determineEntryPoint
	_ = baseDSNFromURL
	_ = baseDSNFromDSN
	_ = generateUpdatePromiseID
	_ = fmt.Sprintf("compile check: %T", w)
	_ = loadShardConfigs
}

// TestWorkerHelpDoesNotPanic verifies that --help returns flag.ErrHelp.
func TestWorkerHelpDoesNotPanic(t *testing.T) {
	fs := flag.NewFlagSet("cleat-worker", flag.ContinueOnError)
	fs.String("db", "", "")
	fs.Int("concurrency", 10, "")
	if err := fs.Parse([]string{"--help"}); err != flag.ErrHelp {
		t.Errorf("expected flag.ErrHelp, got %v", err)
	}
}

func TestJSONLogFormat(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	logger.InfoContext(context.Background(), "test message", "workflow_id", "wf-123", "tenant_id", "t-456")

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("log output is not valid JSON: %v\nGot: %s", err, buf.String())
	}
	if entry["msg"] != "test message" {
		t.Errorf("expected msg 'test message', got %v", entry["msg"])
	}
	if entry["workflow_id"] != "wf-123" {
		t.Errorf("expected workflow_id 'wf-123', got %v", entry["workflow_id"])
	}
	if entry["tenant_id"] != "t-456" {
		t.Errorf("expected tenant_id 't-456', got %v", entry["tenant_id"])
	}
}

func TestOtelFlagsRegistered(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	otelEndpoint := fs.String("otel-endpoint", "", "OTLP HTTP endpoint")
	otelDisabled := fs.Bool("otel-disabled", false, "Disable OTel")

	if *otelEndpoint != "" {
		t.Errorf("expected default --otel-endpoint to be empty, got %q", *otelEndpoint)
	}
	if *otelDisabled != false {
		t.Errorf("expected default --otel-disabled to be false")
	}

	if err := fs.Parse([]string{"--otel-endpoint", "localhost:4318", "--otel-disabled", "true"}); err != nil {
		t.Fatalf("failed to parse flags: %v", err)
	}
	if *otelEndpoint != "localhost:4318" {
		t.Errorf("expected --otel-endpoint=localhost:4318, got %q", *otelEndpoint)
	}
	if *otelDisabled != true {
		t.Errorf("expected --otel-disabled=true")
	}
}
