// Command cleat-mcp exposes deployed cleat workflows as MCP tools, for
// cleat#1982.
//
// # What this is, and what it deliberately is not
//
// It is a PROTOCOL ADAPTER. It holds no state, makes no exposure decision of
// its own, and re-implements no access control. Every question about who may
// see or start what is answered by cleat-worker, on the same routes any other
// client uses:
//
//   - tools/list is built from GET /api/openapi.json, which cleat-worker
//     already filters through withoutInternalDefs (cmd/cleat-worker/openapi.go)
//     and already scopes to the caller's tenant. An `internal` definition is
//     therefore absent from the tool list for the same reason it is absent from
//     that document -- not because this proxy remembered to skip it.
//   - tools/call POSTs /api/workflows/{name}/start, which refuses an internal
//     definition by name (cmd/cleat-worker/exposure_routes.go). A client that
//     guesses an internal name gets the route's own 404, unaltered.
//
// That is a deliberate design choice and the reason this file is short. The
// alternative -- reading workflow_defs here and applying the class locally --
// would be a SECOND implementation of cleat#1986 slice 2b's decision, and the
// two would drift. cleat-review's rule applies directly: the routes a shared
// gate cannot reach are the ones needing individual attention, so the right
// move is to have no second gate.
//
// # Idempotency
//
// A retried tools/call must start exactly one run (cleat#1982 acceptance
// clause 3). cleat's start route already dedupes on an `Idempotency-Key`
// header, so this proxy supplies one rather than inventing a mechanism:
//
//	key = sha256(tenant-scope, tool name, canonical JSON of arguments)
//
// The key is DERIVED FROM THE ARGUMENTS, which is what makes the tool's
// advertised `idempotentHint: true` a true statement rather than a hopeful
// one: the MCP spec defines that hint as "calling the tool repeatedly WITH THE
// SAME ARGUMENTS will have no additional effect", so anything not in the
// arguments cannot be what makes it true.
//
// A caller who genuinely wants a second run passes a distinct `idempotency_key`
// argument. That is a different argument set, so it is not "a repeated call with
// the same arguments" -- which is how the guarantee and the escape hatch stay
// compatible. The obvious alternatives do not work: the JSON-RPC request id
// cannot be used, because the spec requires a retry's id to DIFFER from the
// original.
//
// # Annotations, and what they are not
//
// Every workflow tool is annotated as side-effecting (`readOnlyHint: false`).
// Per the owner's ruling that is a CLIENT-INTERFACE affordance: it is what makes
// a conforming client offer its confirmation prompt. It is NOT the mitigation
// for the prompt-injection surface -- the spec says clients MUST treat
// annotations from an untrusted server as untrusted, and a conforming client may
// ignore them entirely. The boundary is the exposure classes, plus auth, plus a
// human in the loop that the protocol does not guarantee.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/cleat-team/cleat/plugin"
)

// protocolVersion is the MCP revision this proxy speaks. cleat#1982 required
// verifying it against the spec rather than the design notes, which were
// agent-supplied: 2026-07-28 is current, and it is a STATELESS protocol --
// sessions and the initialize handshake are removed, so there is no per-client
// state to keep and no handshake to complete.
const protocolVersion = "2026-07-28"

// credHeaders are forwarded verbatim to cleat-worker. Decision 3 is that the
// MVP passes through the credentials the cleat API already accepts rather than
// inventing its own; OAuth resource-server mode is phase 2.
var credHeaders = []string{"Authorization", "X-Cleat-API-Key"}

func main() {
	api := flag.String("api", envOr("CLEAT_API", "http://localhost:8080"),
		"Base URL of the cleat API to proxy, e.g. http://localhost:8080")
	addr := flag.String("addr", envOr("CLEAT_MCP_ADDR", ":8765"),
		"Address to serve MCP on")
	flag.Parse()

	p := &proxy{api: strings.TrimRight(*api, "/"), http: &http.Client{Timeout: 60 * time.Second}}

	srv := &http.Server{
		Addr:              *addr,
		Handler:           p,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		ln := listenAndLogBoundAddr(*addr, p.api)
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Fatalf("cleat-mcp: %v", err)
		}
	}()

	<-ctx.Done()
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutCtx)
}

// listenAndLogBoundAddr binds addr and logs the address it actually bound --
// not addr itself (cleat#3155). Before this, ListenAndServe bound and logged
// the configured address in one call, the same shape cleat-worker had until
// cleat#3136/#3137: a caller that starts this on an ephemeral `:0` port had
// no log line, or any other instrumentation, telling it which port it got.
//
// Extracted from main so the logged line is assertable without exec'ing a
// built binary or touching the package-level flag.CommandLine main.Parse()
// already consumed.
func listenAndLogBoundAddr(addr, api string) net.Listener {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("cleat-mcp: listen on %s: %v", addr, err)
	}
	log.Printf("cleat-mcp: proxying %s, serving MCP on %s (protocol %s)", api, ln.Addr().String(), protocolVersion)
	return ln
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

type proxy struct {
	api  string
	http *http.Client
}

// ---- JSON-RPC over HTTP -------------------------------------------------

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// JSON-RPC and MCP error codes we use. -32602 is the spec's own answer for an
// unknown or malformed tool reference; we do not invent a private code for a
// missing workflow, because a client that receives -32602 knows what it means.
const (
	codeParseError = -32700
	codeInvalidReq = -32600
	codeNoMethod   = -32601
	codeInvalidPar = -32602
)

func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		// The 2026-07-28 Streamable HTTP transport carries JSON-RPC on POST.
		http.Error(w, "cleat-mcp: use POST", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		writeRPC(w, rpcResponse{JSONRPC: "2.0", Error: &rpcError{codeParseError, "reading request: " + err.Error()}})
		return
	}

	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeRPC(w, rpcResponse{JSONRPC: "2.0", Error: &rpcError{codeParseError, "invalid JSON: " + err.Error()}})
		return
	}

	switch req.Method {
	case "server/discover":
		// Optional in the spec; answering it lets a client learn the version
		// without guessing.
		writeRPC(w, rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
			"protocolVersion": protocolVersion,
			"serverInfo":      map[string]any{"name": "cleat-mcp", "version": "0.1.0"},
			"capabilities":    map[string]any{"tools": map[string]any{}},
		}})
	case "tools/list":
		p.toolsList(w, r, req)
	case "tools/call":
		p.toolsCall(w, r, req)
	case "tasks/get":
		p.tasksGet(w, r, req)
	case "tasks/cancel":
		p.tasksCancel(w, r, req)
	default:
		writeRPC(w, rpcResponse{JSONRPC: "2.0", ID: req.ID,
			Error: &rpcError{codeNoMethod, "unsupported method " + req.Method}})
	}
}

func writeRPC(w http.ResponseWriter, resp rpcResponse) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// ---- tools/list ---------------------------------------------------------

type tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	Annotations map[string]any `json:"annotations"`
}

// workflowAnnotations is the annotation block for every workflow tool.
//
// readOnlyHint:false is the load-bearing one and it is what the owner's ruling
// asked for: starting a workflow changes cleat's state, and a conforming client
// that sees a non-read-only tool engages its confirmation UI. The other three
// are stated because the defaults already imply them and saying so is how a
// client learns what is true; the DANGEROUS direction is the reverse -- a
// readOnlyHint:true would suppress that prompt on the one surface the owner
// called a prompt-injection path, so a test asserts it never appears.
func workflowAnnotations() map[string]any {
	return map[string]any{
		"readOnlyHint":    false, // starts a run
		"destructiveHint": false, // additive: it creates a run, destroys nothing
		"idempotentHint":  true,  // true because the key is derived from the arguments
		"openWorldHint":   false, // the domain is this cleat instance
	}
}

func (p *proxy) toolsList(w http.ResponseWriter, r *http.Request, req rpcRequest) {
	doc, status, err := p.getOpenAPI(r)
	if err != nil {
		writeRPC(w, rpcResponse{JSONRPC: "2.0", ID: req.ID,
			Error: &rpcError{codeInvalidReq, fmt.Sprintf("cleat API openapi document: %v", err)}})
		return
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		// The caller's credentials are the API's to judge, and this is the same
		// refusal the API gave -- not a proxy-local decision.
		writeRPC(w, rpcResponse{JSONRPC: "2.0", ID: req.ID,
			Error: &rpcError{codeInvalidReq, "cleat API refused the request's credentials"}})
		return
	}
	if status != http.StatusOK {
		writeRPC(w, rpcResponse{JSONRPC: "2.0", ID: req.ID,
			Error: &rpcError{codeInvalidReq, fmt.Sprintf("cleat API openapi document: status %d", status)}})
		return
	}

	tools := toolsFromOpenAPI(doc)
	// Deterministic order: the spec SHOULD-requires it so clients can cache the
	// list, and a map-derived order would change between identical requests.
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })

	writeRPC(w, rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{"tools": tools}})
}

// toolsFromOpenAPI turns the document's start paths into tools.
//
// The inputSchema is the document's own request-body schema, passed through
// unaltered. That is the point of going through /api/openapi.json rather than
// listing definitions: for a Go or Python workflow it is the schema the emitter
// produced (cleat#1980), and for Rust, Java and AssemblyScript -- which carry
// NULL schemas BY DESIGN -- it is the passthrough the document already chose.
// This proxy does not upgrade one into the other, because it cannot tell them
// apart and must not claim a shape it does not have.
func toolsFromOpenAPI(doc map[string]any) []tool {
	paths, _ := doc["paths"].(map[string]any)
	out := make([]tool, 0, len(paths))
	for path, raw := range paths {
		name, ok := workflowNameFromStartPath(path)
		if !ok {
			continue
		}
		item, _ := raw.(map[string]any)
		post, _ := item["post"].(map[string]any)
		if post == nil {
			continue // a path with no POST is not a start route
		}
		schema := requestBodySchema(post)
		if schema == nil {
			schema = map[string]any{"type": "object"}
		}
		desc, _ := post["description"].(string)
		if desc == "" {
			desc = "Start the " + name + " workflow on cleat."
		}
		out = append(out, tool{
			Name:        toolName(name),
			Description: desc + " Starting a workflow is a side-effecting call; a retry with the same arguments returns the run already started.",
			InputSchema: schema,
			Annotations: workflowAnnotations(),
		})
	}
	return out
}

// workflowNameFromStartPath recognises /api/workflows/{name}/start and returns
// {name}. Anything else -- /api/workflows/{name}/tags, the run routes -- is not
// a start path and is skipped rather than guessed at.
func workflowNameFromStartPath(path string) (string, bool) {
	const pre = "/api/workflows/"
	if !strings.HasPrefix(path, pre) {
		return "", false
	}
	rest := strings.TrimPrefix(path, pre)
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) != 2 || parts[1] != "start" {
		return "", false
	}
	if parts[0] == "" || strings.ContainsAny(parts[0], "{}") {
		return "", false
	}
	return parts[0], true
}

func requestBodySchema(post map[string]any) map[string]any {
	rb, _ := post["requestBody"].(map[string]any)
	if rb == nil {
		return nil
	}
	content, _ := rb["content"].(map[string]any)
	if content == nil {
		return nil
	}
	aj, _ := content["application/json"].(map[string]any)
	if aj == nil {
		return nil
	}
	schema, _ := aj["schema"].(map[string]any)
	return schema
}

// toolName maps a workflow name to a legal MCP tool name. The spec allows
// [A-Za-z0-9_.-] and forbids spaces and commas; a workflow name with anything
// else is sanitised rather than dropped, because dropping a workflow silently
// would be a workflow that exists and cannot be called.
func toolName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '_', r == '-', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}

// ---- tools/call ---------------------------------------------------------

type callParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

func (p *proxy) toolsCall(w http.ResponseWriter, r *http.Request, req rpcRequest) {
	var params callParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		writeRPC(w, rpcResponse{JSONRPC: "2.0", ID: req.ID,
			Error: &rpcError{codeInvalidPar, "invalid params: " + err.Error()}})
		return
	}
	if params.Name == "" {
		writeRPC(w, rpcResponse{JSONRPC: "2.0", ID: req.ID,
			Error: &rpcError{codeInvalidPar, "params.name is required"}})
		return
	}

	args := map[string]any{}
	if len(params.Arguments) > 0 {
		if err := json.Unmarshal(params.Arguments, &args); err != nil {
			writeRPC(w, rpcResponse{JSONRPC: "2.0", ID: req.ID,
				Error: &rpcError{codeInvalidPar, "params.arguments is not an object"}})
			return
		}
	}

	// idempotency_key is an argument rather than a side channel, which is what
	// keeps the published idempotentHint true. It is removed from the body sent
	// to the start route -- cleat's own dedupe is the header.
	explicitKey, _ := args["idempotency_key"].(string)
	delete(args, "idempotency_key")

	key := explicitKey
	if key == "" {
		key = deriveKey(r, params.Name, args)
	}

	status, body, err := p.start(r, params.Name, args, key)
	if err != nil {
		writeRPC(w, rpcResponse{JSONRPC: "2.0", ID: req.ID,
			Error: &rpcError{codeInvalidReq, "cleat API: " + err.Error()}})
		return
	}
	if status != http.StatusOK && status != http.StatusCreated && status != http.StatusAccepted {
		// A tool EXECUTION error, per the spec: actionable feedback the model can
		// act on (an unknown name, a refusal), reported in the result with
		// isError rather than as a JSON-RPC error.
		writeRPC(w, rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
			"isError": true,
			"content": []map[string]any{{"type": "text", "text": startErrorMessage(status, body)}},
		}})
		return
	}

	var started struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(body, &started)

	writeRPC(w, rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
		"content": []map[string]any{{
			"type": "text",
			"text": fmt.Sprintf("Started %s as run %s.", params.Name, started.ID),
		}},
		"structuredContent": map[string]any{"run_id": started.ID, "workflow": params.Name},
	}})
}

// deriveKey is the default idempotency key.
//
// It is scoped to the CALLER as well as the arguments. Without the caller in the
// hash, two tenants that call the same tool with the same arguments compute the
// same key, and cleat's dedupe would hand the second caller the FIRST caller's
// run id -- a disclosure dressed as a duplicate.
//
// The credentials stand in for the caller's identity because that is what the
// API itself authenticates; it is not a cryptographic identity, and it is not
// used as one. canonical JSON sorts keys so that the same arguments in a
// different field order hash identically.
func deriveKey(r *http.Request, tool string, args map[string]any) string {
	h := sha256.New()
	for _, name := range credHeaders {
		h.Write([]byte(name))
		h.Write([]byte{0})
		h.Write([]byte(r.Header.Get(name)))
		h.Write([]byte{0})
	}
	h.Write([]byte(tool))
	h.Write([]byte{0})
	h.Write([]byte(canonicalJSON(args)))
	return hex.EncodeToString(h.Sum(nil))
}

func canonicalJSON(v any) string {
	b, err := json.Marshal(sortKeys(v))
	if err != nil {
		return fmt.Sprintf("%#v", v)
	}
	return string(b)
}

// sortKeys recursively normalises maps so that json.Marshal emits a stable
// ordering. encoding/json already sorts map keys, so this exists only to make
// that guarantee explicit rather than incidental.
func sortKeys(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = sortKeys(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = sortKeys(val)
		}
		return out
	default:
		return v
	}
}

func startErrorMessage(status int, body []byte) string {
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil && e.Error != "" {
		return fmt.Sprintf("cleat refused the start (HTTP %d): %s", status, e.Error)
	}
	return fmt.Sprintf("cleat refused the start (HTTP %d).", status)
}

// ---- tasks/get and tasks/cancel -----------------------------------------

// taskParams is the shared parameter shape of the Tasks extension's get and
// cancel. Both address a task by id, and the id is the RUN id -- cleat#1982's
// design table: "task id | run id".
type taskParams struct {
	TaskID string `json:"taskId"`
}

func (p *proxy) tasksGet(w http.ResponseWriter, r *http.Request, req rpcRequest) {
	var params taskParams
	if err := json.Unmarshal(req.Params, &params); err != nil || params.TaskID == "" {
		writeRPC(w, rpcResponse{JSONRPC: "2.0", ID: req.ID,
			Error: &rpcError{codeInvalidPar, "params.taskId is required"}})
		return
	}
	status, body, err := p.getRun(r, params.TaskID)
	if err != nil {
		writeRPC(w, rpcResponse{JSONRPC: "2.0", ID: req.ID,
			Error: &rpcError{codeInvalidReq, "cleat API: " + err.Error()}})
		return
	}
	if status != http.StatusOK {
		writeRPC(w, rpcResponse{JSONRPC: "2.0", ID: req.ID,
			Error: &rpcError{codeInvalidPar, fmt.Sprintf("unknown task %q (HTTP %d)", params.TaskID, status)}})
		return
	}
	writeRPC(w, rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: taskFromRun(params.TaskID, body)})
}

func (p *proxy) tasksCancel(w http.ResponseWriter, r *http.Request, req rpcRequest) {
	var params taskParams
	if err := json.Unmarshal(req.Params, &params); err != nil || params.TaskID == "" {
		writeRPC(w, rpcResponse{JSONRPC: "2.0", ID: req.ID,
			Error: &rpcError{codeInvalidPar, "params.taskId is required"}})
		return
	}
	status, body, err := p.cancelRun(r, params.TaskID)
	if err != nil {
		writeRPC(w, rpcResponse{JSONRPC: "2.0", ID: req.ID,
			Error: &rpcError{codeInvalidReq, "cleat API: " + err.Error()}})
		return
	}
	if status != http.StatusOK && status != http.StatusAccepted {
		writeRPC(w, rpcResponse{JSONRPC: "2.0", ID: req.ID,
			Error: &rpcError{codeInvalidPar, fmt.Sprintf("could not cancel task %q (HTTP %d)", params.TaskID, status)}})
		return
	}
	if len(body) == 0 {
		writeRPC(w, rpcResponse{JSONRPC: "2.0", ID: req.ID,
			Result: map[string]any{"taskId": params.TaskID, "status": "cancelled"}})
		return
	}
	writeRPC(w, rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: taskFromRun(params.TaskID, body)})
}

// taskFromRun shapes a run as a task. The status mapping is conservative and
// the payload fields are status-dependent, matching the extension's inlined
// shape: `result` for a completed task, `error` for a failed one.
func taskFromRun(taskID string, body []byte) map[string]any {
	var run struct {
		ID        string `json:"id"`
		Status    string `json:"status"`
		Result    string `json:"result"`
		Error     string `json:"error"`
		ErrorCode string `json:"error_code"`
	}
	_ = json.Unmarshal(body, &run)
	id := run.ID
	if id == "" {
		id = taskID
	}
	status := taskStatus(run.Status)
	task := map[string]any{"taskId": id, "status": status}
	switch status {
	case "completed":
		if run.Result != "" {
			task["result"] = map[string]any{
				"content": []map[string]any{{"type": "text", "text": run.Result}},
			}
		}
	case "failed":
		msg := run.Error
		if msg == "" {
			msg = run.ErrorCode
		}
		task["error"] = map[string]any{"code": -32603, "message": msg}
	}
	return task
}

// taskStatus maps a cleat run status onto the extension's closed set of
// working / input_required / completed / failed / cancelled.
//
// The default is `working`, and that is deliberate: calling an unrecognised
// status "completed" would tell a client a run had finished, which is the one
// direction a client acts on. Cleat's vocabulary is not a closed set this proxy
// may assume, so anything unrecognised is reported as still in progress.
//
// `suspended` -- a run waiting on a signal or promise -- is reported as
// `working` rather than `input_required`. cleat#1982 defers that mapping to
// phase 2, and reporting input_required without the extension's inputRequests
// payload would ask a client for input it cannot see.
func taskStatus(cleatStatus string) string {
	switch cleatStatus {
	case "done", "completed":
		return "completed"
	case "failed":
		return "failed"
	case "cancelled":
		return "cancelled"
	default:
		return "working"
	}
}

// ---- HTTP to cleat ------------------------------------------------------

func (p *proxy) getOpenAPI(r *http.Request) (map[string]any, int, error) {
	status, body, err := p.call(r, outbound{
		method: http.MethodGet,
		path:   "/api/openapi.json",
		limit:  documentReadLimit,
	})
	if err != nil {
		return nil, 0, err
	}
	if status != http.StatusOK {
		return nil, status, nil
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, status, fmt.Errorf("decoding document: %w", err)
	}
	return doc, status, nil
}

func (p *proxy) getRun(r *http.Request, runID string) (int, []byte, error) {
	return p.call(r, outbound{method: http.MethodGet, path: "/api/workflows/" + url.PathEscape(runID)})
}

// cancelBody is the cancel route's own request shape (cleat#1975/D3): both
// fields are optional and the zero value is the non-preemptive cancel every
// existing caller already gets. It is sent explicitly, rather than omitted,
// because handleCancel decodes with decodeJSONBody -- the STRICT variant,
// not decodeOptionalJSONBody -- so an empty body is read as malformed JSON
// (io.EOF) and refused with 400 before the route ever looks at the run.
var cancelBody = []byte("{}")

func (p *proxy) cancelRun(r *http.Request, runID string) (int, []byte, error) {
	return p.call(r, outbound{
		method: http.MethodPost,
		path:   "/api/workflows/" + url.PathEscape(runID) + "/cancel",
		body:   cancelBody,
	})
}

func (p *proxy) start(r *http.Request, workflow string, args map[string]any, key string) (int, []byte, error) {
	payload, err := json.Marshal(map[string]any{"input": args})
	if err != nil {
		return 0, nil, err
	}
	return p.call(r, outbound{
		method:  http.MethodPost,
		path:    "/api/workflows/" + url.PathEscape(workflow) + "/start",
		body:    payload,
		headers: map[string]string{"Idempotency-Key": key},
	})
}

// Read limits, and why they are not one number.
//
// Unifying the three outbound builders into call() also unified their read
// limits, and the smallest won -- dropping the OpenAPI document from 8 MiB to
// 1 MiB. cleat-review caught it on cleat#3011 with a 2.89 MiB document, where
// tools/list failed ENTIRELY while blaming the JSON ("unexpected end of JSON
// input") rather than the ceiling.
//
// They are different quantities and the difference is not tuning:
//
//   - A RUN RESULT is bounded by one workflow's output.
//   - The OPENAPI DOCUMENT is bounded by the CUSTOMER'S DEPLOYMENT -- one entry
//     per exposed workflow, each carrying its full schemas -- so it grows with
//     use and has no shape-independent size.
//
// The truncation is made legible below rather than left to the decoder, because
// a silent cut is what turned "the document is bigger than the cap" into a
// message about malformed JSON.
const (
	defaultReadLimit  = 1 << 20
	documentReadLimit = 8 << 20
)

// outbound is one request to cleat.
type outbound struct {
	method  string
	path    string
	body    []byte
	headers map[string]string
	// limit overrides defaultReadLimit for a response whose size is not
	// bounded by a single workflow's output. Zero means defaultReadLimit.
	limit int64
}

// call is the ONE place an outbound request to cleat is built. It exists so
// that two things a new call site would otherwise have to remember are attached
// in one place, and both fail QUIETLY when forgotten:
//
//   - CREDENTIALS. An unauthenticated call is not refused -- it is answered as
//     the DEFAULT tenant -- so forgetting them reads as a working request
//     returning someone else's data rather than as a failure.
//   - THE CALLER'S TRACE. A proxy is a hop, and a hop that does not pass the
//     trace on is where a customer's chain ends: cleat appears in a collector
//     as a leaf that swallowed everything downstream (cleat#1596).
func (p *proxy) call(r *http.Request, o outbound) (int, []byte, error) {
	var rdr io.Reader
	if o.body != nil {
		rdr = bytes.NewReader(o.body)
	}
	req, err := http.NewRequestWithContext(r.Context(), o.method, p.api+o.path, rdr)
	if err != nil {
		return 0, nil, err
	}
	if o.body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, h := range credHeaders {
		if v := r.Header.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	for k, v := range o.headers {
		req.Header.Set(k, v)
	}
	// The caller's trace continues into cleat. The trace-ID comes from the
	// caller's inbound traceparent and a fresh span-id is synthesised for this
	// hop by plugin.SetTraceparent -- the same helper the plugin path uses, and
	// cleat keeps only the trace-ID from an inbound header (cleat#1597), so
	// re-deriving here is that policy rather than a second one invented here.
	plugin.SetTraceparent(req, traceIDFromTraceparent(r.Header.Get(plugin.TraceparentHeader)))
	resp, err := p.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	limit := o.limit
	if limit == 0 {
		limit = defaultReadLimit
	}
	// limit+1, so a body EXACTLY at the ceiling is not reported as truncated.
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	if int64(len(b)) > limit {
		// Legible truncation. Without this the cut surfaces later as whatever
		// the DECODER says about the fragment -- for JSON, "unexpected end of
		// JSON input", which blames the document rather than the ceiling.
		return resp.StatusCode, nil, fmt.Errorf("response body exceeded the %d-byte read limit", limit)
	}
	return resp.StatusCode, b, nil
}

// traceIDFromTraceparent returns the 32-hex trace-ID of a W3C traceparent
// header, or "" when the header is absent or malformed.
//
// It only has to avoid INVENTING one: plugin.SetTraceparent validates again and
// ignores an invalid ID, so a malformed header degrades to "no trace to
// propagate" -- the honest answer for a caller that sent no usable trace. That
// is also why this need not agree with that validator exactly.
func traceIDFromTraceparent(tp string) string {
	parts := strings.Split(tp, "-")
	if len(parts) != 4 || parts[0] != "00" || len(parts[1]) != 32 {
		return ""
	}
	if _, err := hex.DecodeString(parts[1]); err != nil {
		return ""
	}
	return parts[1]
}
