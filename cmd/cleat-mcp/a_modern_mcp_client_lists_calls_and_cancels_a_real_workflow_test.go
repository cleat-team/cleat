package main

// cleat#1982 acceptance, closed for real. #3011 shipped the whole surface
// against an in-repo fake and said so honestly: "the proxy against a live
// API with a genuinely deployed workflow" was "the next step, and neither is
// claimed here." This is that step.
//
// It is the only test in this package that builds and runs real binaries
// (cleat, cleatctl, cleat-worker, cleat-mcp) against a real database and
// drives the real one over HTTP with a hand-rolled MODERN (2026-07-28)
// JSON-RPC client -- no fake on either side.
//
// Clause 1 was restated 2026-10-05 (owner decision, option b, recorded on the
// issue) to be satisfiable with exactly this client, rather than with "an MCP
// client (e.g. the MCP Inspector)": every currently-released MCP client
// (@modelcontextprotocol/sdk 1.32.0, the Inspector 2.9.0 built on it) speaks
// only the legacy initialize-handshake protocol (<=2025-11-25), cleat-mcp
// correctly implements only the modern per-request protocol, and the spec's
// own compatibility matrix says that combination fails. Confirmed by actually
// running the Inspector CLI against a live cleat-mcp: it sends initialize
// first and never reaches tools/list. Owner, verbatim: "There are no legacy
// clients, you don't need to keep compatibility." No dual-era support is
// being added here; this test is what the restated clause asks for.
//
// The fixture is testdata/hello/ (root module, already used by README.md's
// own "try it" command) deployed through `cleatctl deploy workflow`, not
// `cleat deploy` -- cleat#3150, filed from this same session, found that
// `cleat deploy` never populates entry_point_schemas, so a workflow deployed
// that way is permanently absent from GET /api/openapi.json and therefore
// from tools/list, which is built from nothing else. Using `cleat deploy`
// here would make this test assert an empty tool list forever, which is
// exactly the kind of green-measuring-nothing this repo's CLAUDE.md warns
// about.
//
// Postgres only, matching this package's and cmd/cleat-worker's other
// real-binary boot tests: nothing under test here varies by dialect.

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/lib/pq"
)

func TestAModernMCPClientListsCallsAndCancelsARealWorkflow(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs real binaries against a real database")
	}
	admin := e2eAdminDSN()
	if admin == "" {
		t.Skip("CLEAT_TEST_POSTGRES/CLEAT_TEST_DB not set, skipping")
	}

	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}

	bin := t.TempDir()
	e2eBuild(t, repoRoot, filepath.Join(bin, "cleat"), "./cmd/cleat")
	e2eBuild(t, repoRoot, filepath.Join(bin, "cleatctl"), "./cmd/cleatctl")
	e2eBuild(t, repoRoot, filepath.Join(bin, "cleat-worker"), "./cmd/cleat-worker")
	e2eBuild(t, repoRoot, filepath.Join(bin, "cleat-mcp"), "./cmd/cleat-mcp")

	ownerDSN := e2eScratchDB(t, admin)

	if code, out := e2eRun(t, filepath.Join(bin, "cleat-worker"), nil,
		"--driver=postgres", "--db="+ownerDSN, "--migrate-only"); code != 0 {
		t.Fatalf("--migrate-only exited %d:\n%s", code, out)
	}

	appDSN := e2eAppRoleDSN(t, ownerDSN)

	wasmDir := t.TempDir()
	e2eCleatBuild(t, filepath.Join(bin, "cleat"), repoRoot, wasmDir, "./testdata/hello/")
	wasmPath := filepath.Join(wasmDir, "hello.wasm")

	const authName = "e2e-hello-auth"
	const internalName = "e2e-hello-internal"
	e2eDeployWorkflow(t, filepath.Join(bin, "cleatctl"), ownerDSN, authName, wasmPath, "")
	e2eDeployWorkflow(t, filepath.Join(bin, "cleatctl"), ownerDSN, internalName, wasmPath, "internal")

	workerAddr, stopWorker := e2eStartWorker(t, filepath.Join(bin, "cleat-worker"), appDSN, ownerDSN)
	defer stopWorker()

	apiKey := e2eGenerateAPIKey(t, filepath.Join(bin, "cleat-worker"), appDSN)

	mcpAddr := fmt.Sprintf("127.0.0.1:%d", e2eFreePort(t))
	stopMCP := e2eStartMCP(t, filepath.Join(bin, "cleat-mcp"), "http://"+workerAddr, mcpAddr)
	defer stopMCP()
	e2eWaitForHTTP(t, mcpAddr)

	client := &e2eMCPClient{addr: mcpAddr, apiKey: apiKey}

	// tools/list: the auth-exposed workflow is present; the internal one
	// never appears (acceptance clause 2, half 1).
	list := client.call(t, "tools/list", map[string]any{})
	tools, _ := list["result"].(map[string]any)["tools"].([]any)
	var names []string
	for _, raw := range tools {
		tool, _ := raw.(map[string]any)
		names = append(names, fmt.Sprintf("%v", tool["name"]))
	}
	if !contains(names, authName) {
		t.Fatalf("tools/list = %v, want %q present", names, authName)
	}
	if contains(names, internalName) {
		t.Fatalf("tools/list = %v: an internal-exposure workflow must never appear", names)
	}

	// tools/call, then an IDENTICAL retry: acceptance clause 3, unconditionally.
	call1 := client.call(t, "tools/call", map[string]any{
		"name": authName, "arguments": map[string]any{"input": "Ada"},
	})
	call2 := client.call(t, "tools/call", map[string]any{
		"name": authName, "arguments": map[string]any{"input": "Ada"},
	})
	id1 := e2eRunID(t, call1)
	id2 := e2eRunID(t, call2)
	if id1 != id2 {
		t.Fatalf("an identical-argument retry started a second run: %s != %s", id1, id2)
	}

	// tasks/get on the completed run: real execution, not a canned value --
	// the result reflects the argument this test actually sent.
	e2eWaitForStatus(t, client, id1, "completed")
	get := client.call(t, "tasks/get", map[string]any{"taskId": id1})
	result, _ := get["result"].(map[string]any)
	if result["status"] != "completed" {
		t.Fatalf("tasks/get status = %v, want completed", result["status"])
	}
	resultText := e2eResultText(t, result)
	if !strings.Contains(resultText, "Ada") {
		t.Fatalf("tasks/get result = %q, want it to contain the argument this test sent (%q)", resultText, "Ada")
	}

	// tasks/cancel on a FRESH run: cleat#1982's own fix (PR #3149) -- before
	// it, this failed for every call, on every run, because cancelRun sent no
	// request body to a route that requires one.
	call3 := client.call(t, "tools/call", map[string]any{
		"name":      authName,
		"arguments": map[string]any{"input": "ToCancel", "idempotency_key": "e2e-cancel-1"},
	})
	id3 := e2eRunID(t, call3)
	cancel := client.call(t, "tasks/cancel", map[string]any{"taskId": id3})
	if cancel["error"] != nil {
		t.Fatalf("tasks/cancel = %v, want a result, not an error", cancel["error"])
	}

	// Calling the internal workflow BY NAME fails (acceptance clause 2, half
	// 2) -- a tool-execution error (isError), carrying the route's own 404,
	// not a crash and not a silent success.
	callInternal := client.call(t, "tools/call", map[string]any{
		"name": internalName, "arguments": map[string]any{"input": "x"},
	})
	internalResult, _ := callInternal["result"].(map[string]any)
	if internalResult == nil || internalResult["isError"] != true {
		t.Fatalf("tools/call on an internal-exposure workflow = %v, want a tool-execution error (isError)", callInternal)
	}
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

// ---- real-binary plumbing -------------------------------------------------

// e2eAdminDSN mirrors cmd/cleat-worker's deployDialect.admin() for
// PostgreSQL: CLEAT_TEST_POSTGRES, falling back to CLEAT_TEST_DB because the
// test-go/commands job sets only the latter.
func e2eAdminDSN() string {
	if v := os.Getenv("CLEAT_TEST_POSTGRES"); v != "" {
		return v
	}
	return os.Getenv("CLEAT_TEST_DB")
}

func e2eBuild(t *testing.T, repoRoot, out, pkg string) {
	t.Helper()
	cmd := exec.Command("go", "build", "-o", out, pkg)
	cmd.Dir = repoRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build %s: %v\n%s", pkg, err, out)
	}
}

// e2eScratchDB creates an empty database on the admin server and returns the
// owner-role DSN for it, dropping it (and terminating its backends first --
// Postgres refuses to drop a database with open connections) in Cleanup.
func e2eScratchDB(t *testing.T, admin string) string {
	t.Helper()
	adb, err := sql.Open("postgres", admin)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { adb.Close() })
	if err := adb.Ping(); err != nil {
		t.Fatalf("CLEAT_TEST_POSTGRES/CLEAT_TEST_DB is set but unreachable: %v", err)
	}
	name := fmt.Sprintf("cleat_1982_e2e_%d", time.Now().UnixNano()%1_000_000_000)
	if _, err := adb.Exec(`CREATE DATABASE ` + name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		a, err := sql.Open("postgres", admin)
		if err != nil {
			return
		}
		defer a.Close()
		_, _ = a.Exec(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()`, name)
		_, _ = a.Exec(`DROP DATABASE IF EXISTS ` + name)
	})
	u, err := parseDSNReplaceDB(admin, name)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func parseDSNReplaceDB(dsn, dbName string) (string, error) {
	// admin DSNs in this tree are postgres://user:pass@host:port/db?params.
	i := strings.LastIndex(dsn, "/")
	j := strings.Index(dsn, "?")
	if i < 0 {
		return "", fmt.Errorf("DSN has no path: %q", dsn)
	}
	if j < 0 || j < i {
		return dsn[:i+1] + dbName, nil
	}
	return dsn[:i+1] + dbName + dsn[j:], nil
}

// e2eAppRoleDSN gives cleat_app a login password (idempotent: ALTER ROLE, not
// CREATE) and returns its DSN against the same database as ownerDSN.
func e2eAppRoleDSN(t *testing.T, ownerDSN string) string {
	t.Helper()
	owner, err := sql.Open("postgres", ownerDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if _, err := owner.Exec(`ALTER ROLE cleat_app LOGIN PASSWORD 'cleat_1982_e2e_pw'`); err != nil {
		t.Fatalf("giving cleat_app a login (is the schema baseline applied?): %v", err)
	}
	u, err := parseDSNReplaceUser(ownerDSN, "cleat_app", "cleat_1982_e2e_pw")
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func parseDSNReplaceUser(dsn, user, pass string) (string, error) {
	const scheme = "postgres://"
	if !strings.HasPrefix(dsn, scheme) {
		return "", fmt.Errorf("DSN does not start with %q: %q", scheme, dsn)
	}
	rest := strings.TrimPrefix(dsn, scheme)
	at := strings.Index(rest, "@")
	if at < 0 {
		return "", fmt.Errorf("DSN has no user@host: %q", dsn)
	}
	return scheme + user + ":" + pass + "@" + rest[at+1:], nil
}

// e2eCleatBuild runs the real `cleat build` on srcPath (relative to repoRoot)
// into outDir.
func e2eCleatBuild(t *testing.T, cleatBin, repoRoot, outDir, srcPath string) {
	t.Helper()
	cmd := exec.Command(cleatBin, "build", "-o", outDir, srcPath)
	cmd.Dir = repoRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cleat build %s: %v\n%s", srcPath, err, out)
	}
}

// e2eDeployWorkflow runs the real `cleatctl deploy workflow`, which -- unlike
// `cleat deploy` -- reads the build's .schema.json sidecar and populates
// entry_point_schemas (cleat#3150). exposure is passed as -exposure when
// non-empty.
func e2eDeployWorkflow(t *testing.T, cleatctlBin, ownerDSN, name, wasmPath, exposure string) {
	t.Helper()
	args := []string{"-db", ownerDSN, "deploy", "workflow"}
	if exposure != "" {
		args = append(args, "-exposure", exposure)
	}
	args = append(args, name, wasmPath)
	cmd := exec.Command(cleatctlBin, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cleatctl deploy workflow %s: %v\n%s", name, err, out)
	}
}

// e2eRun runs bin to completion and returns its exit code and output, for the
// modes that exit (e.g. --migrate-only).
func e2eRun(t *testing.T, bin string, env []string, args ...string) (int, string) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return 0, string(out)
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), string(out)
	}
	t.Fatalf("running %s: %v\n%s", bin, err, out)
	return -1, string(out)
}

type e2eSyncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *e2eSyncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *e2eSyncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// e2eFreePort mirrors cmd/cleat-worker's freePort: release a kernel-chosen
// port and hand it to a different process. There is a race window between
// the two (cleat#3138's own caveat); cleat-mcp has no bound-address log line
// the way cleat-worker does post-cleat#3136, so this is the pragmatic choice
// rather than the preferred one -- see the follow-up noted in this test's PR.
func e2eFreePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// e2eBoundAPIAddr mirrors cmd/cleat-worker's boundAPIAddr: the worker logs
// JSON on stderr, and the line this test waits for names the address the
// socket actually bound (cleat#3136), not the configured one.
func e2eBoundAPIAddr(s string) string {
	for _, line := range strings.Split(s, "\n") {
		var rec struct {
			Msg  string `json:"msg"`
			Addr string `json:"addr"`
		}
		if json.Unmarshal([]byte(line), &rec) == nil && rec.Msg == "HTTP API listening" {
			return rec.Addr
		}
	}
	return ""
}

func e2eStartWorker(t *testing.T, bin, appDSN, ownerDSN string) (string, func()) {
	t.Helper()
	cmd := exec.Command(bin,
		"--driver=postgres", "--db="+appDSN, "--migrate-db="+ownerDSN, "--api-addr=127.0.0.1:0")
	var out e2eSyncBuffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	stop := func() {
		_ = cmd.Process.Kill()
		<-exited
	}

	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-exited:
			t.Fatalf("cleat-worker exited before reporting a bound API address:\n%s", out.String())
		default:
		}
		if addr := e2eBoundAPIAddr(out.String()); addr != "" {
			return addr, stop
		}
		time.Sleep(100 * time.Millisecond)
	}
	stop()
	t.Fatalf("no \"HTTP API listening\" line within 90s:\n%s", out.String())
	return "", stop
}

func e2eStartMCP(t *testing.T, bin, api, addr string) func() {
	t.Helper()
	cmd := exec.Command(bin, "--api="+api, "--addr="+addr)
	var out e2eSyncBuffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-exited
	})
	return func() {
		_ = cmd.Process.Kill()
		<-exited
	}
}

// e2eWaitForHTTP polls addr until something accepts a TCP connection.
func e2eWaitForHTTP(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("nothing accepted a connection on %s within 30s", addr)
}

// e2eGenerateAPIKey runs the real --generate-api-key mode and parses its
// stdout.
func e2eGenerateAPIKey(t *testing.T, bin, appDSN string) string {
	t.Helper()
	tenant := "00000000-0000-0000-0000-000000000000"
	cmd := exec.Command(bin, "--driver=postgres", "--db="+appDSN, "--generate-api-key="+tenant)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("--generate-api-key: %v\n%s", err, out)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "Key:") {
			fields := strings.Fields(line)
			if len(fields) == 2 {
				return fields[1]
			}
		}
	}
	t.Fatalf("no Key: line in --generate-api-key output:\n%s", out)
	return ""
}

// ---- a hand-rolled MODERN (2026-07-28) MCP client --------------------------
//
// Not @modelcontextprotocol/sdk, and not the Inspector: neither can speak
// this revision (owner decision, option b; see the top-of-file comment and
// cleat#1982). This client sends exactly what the spec's modern form
// requires -- per-request _meta, no handshake -- and nothing more.

type e2eMCPClient struct {
	addr   string
	apiKey string
}

func (c *e2eMCPClient) call(t *testing.T, method string, params map[string]any) map[string]any {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  method,
		"params":  params,
		"_meta": map[string]any{
			"io.modelcontextprotocol/protocolVersion": "2026-07-28",
			"clientInfo":         map[string]any{"name": "cleat-1982-e2e-test", "version": "0.1.0"},
			"clientCapabilities": map[string]any{},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, "http://"+c.addr+"/", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("MCP-Protocol-Version", "2026-07-28")
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	defer resp.Body.Close()
	var decoded map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Fatalf("%s: decoding response: %v", method, err)
	}
	return decoded
}

func e2eRunID(t *testing.T, resp map[string]any) string {
	t.Helper()
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("tools/call: no result: %v", resp)
	}
	sc, ok := result["structuredContent"].(map[string]any)
	if !ok {
		t.Fatalf("tools/call: no structuredContent: %v", resp)
	}
	id, _ := sc["run_id"].(string)
	if id == "" {
		t.Fatalf("tools/call: no run_id: %v", resp)
	}
	return id
}

func e2eResultText(t *testing.T, taskResult map[string]any) string {
	t.Helper()
	result, ok := taskResult["result"].(map[string]any)
	if !ok {
		t.Fatalf("tasks/get: completed task has no result: %v", taskResult)
	}
	content, ok := result["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("tasks/get: result has no content: %v", taskResult)
	}
	item, _ := content[0].(map[string]any)
	text, _ := item["text"].(string)
	return text
}

func e2eWaitForStatus(t *testing.T, client *e2eMCPClient, taskID, want string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		resp := client.call(t, "tasks/get", map[string]any{"taskId": taskID})
		result, _ := resp["result"].(map[string]any)
		if status, _ := result["status"].(string); status == want {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("task %s did not reach status %q within 30s", taskID, want)
}
