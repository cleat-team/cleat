package main

import (
	"container/list"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"os"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/monitoring/prometheus"
	"github.com/cleat-team/cleat/plugin"
	"github.com/cleat-team/cleat/wasm"
)

// ---------------------------------------------------------------------------
// WASM LRU cache
// ---------------------------------------------------------------------------

// wasmLRUEntry is stored in container/list for LRU ordering.
type wasmLRUEntry struct {
	key   string
	bytes []byte
}

// wasmLRUCache is a bounded LRU cache for WASM byte slices keyed by
// "defName:version". Evicts LRU when entry count or total bytes exceeded.
type wasmLRUCache struct {
	mu       sync.Mutex
	list     *list.List
	index    map[string]*list.Element
	maxBytes int64
	maxEnts  int
}

func newWasmLRUCache(maxEntries, maxMB int) *wasmLRUCache {
	return &wasmLRUCache{
		list:     list.New(),
		index:    make(map[string]*list.Element),
		maxEnts:  maxEntries,
		maxBytes: int64(maxMB) * 1024 * 1024,
	}
}

func (c *wasmLRUCache) get(key string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if elem, ok := c.index[key]; ok {
		c.list.MoveToFront(elem)
		return elem.Value.(*wasmLRUEntry).bytes, true
	}
	return nil, false
}

func (c *wasmLRUCache) put(key string, data []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if elem, ok := c.index[key]; ok {
		c.list.MoveToFront(elem)
		elem.Value.(*wasmLRUEntry).bytes = data
		return
	}
	for c.list.Len() >= c.maxEnts || c.sizeBytesLocked()+int64(len(data)) > c.maxBytes {
		c.evictLocked()
	}
	entry := &wasmLRUEntry{key: key, bytes: data}
	elem := c.list.PushFront(entry)
	c.index[key] = elem
}

func (c *wasmLRUCache) sizeBytesLocked() int64 {
	var total int64
	for e := c.list.Front(); e != nil; e = e.Next() {
		total += int64(len(e.Value.(*wasmLRUEntry).bytes))
	}
	return total
}

func (c *wasmLRUCache) evictLocked() {
	if elem := c.list.Back(); elem != nil {
		entry := elem.Value.(*wasmLRUEntry)
		delete(c.index, entry.key)
		c.list.Remove(elem)
	}
}

func (c *wasmLRUCache) remove(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if elem, ok := c.index[key]; ok {
		delete(c.index, key)
		c.list.Remove(elem)
	}
}

// ---------------------------------------------------------------------------
// Loop context
// ---------------------------------------------------------------------------

// loopContext holds a per-loop cancellation signal and a done channel used by
// restartLoop to synchronise replacement of a stale background goroutine.
type loopContext struct {
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
}

// ---------------------------------------------------------------------------
// dbServiceCaller
// ---------------------------------------------------------------------------

// dbServiceCaller implements engine.ServiceCaller for the worker.
type dbServiceCaller struct {
	store       engine.WorkflowStore
	workerID    string
	benchSvcURL string
}

func (c *dbServiceCaller) Call(ctx context.Context, service, operation, requestJSON string) (string, error) {
	return c.call(ctx, service, operation, requestJSON, "")
}

// CallWithIdempotencyKey implements engine.IdempotentCaller.
//
// The key is stable across every replay of the same logical step, so a service
// that honours it returns the original outcome instead of performing the work
// again after a crash. See engine/idempotency.go and IMPROVEMENT-PLAN §1.4
// phase B.
//
// NOTE (WS-2 -> WS-3): cmd/cleat-worker/ is WS-3's under
// PARALLEL-WORKSTREAMS.md. Added here because the engine-side mechanism is inert
// without a caller that implements it, and shipping a mechanism nothing calls is
// the exact shape of the §1.4 defect this phase exists to avoid. Additive: the
// existing Call is unchanged in behaviour and delegates to the same helper.
func (c *dbServiceCaller) CallWithIdempotencyKey(ctx context.Context, service, operation, requestJSON, idempotencyKey string) (string, error) {
	return c.call(ctx, service, operation, requestJSON, idempotencyKey)
}

func (c *dbServiceCaller) call(ctx context.Context, service, operation, requestJSON, idempotencyKey string) (string, error) {
	if service == "http" && operation == "fetch" {
		return c.handleHTTPFetch(ctx, requestJSON)
	}
	if c.benchSvcURL != "" {
		return c.forwardToBenchSvc(ctx, service, operation, requestJSON, idempotencyKey)
	}
	return "", engine.NewPermanentError("call", "", fmt.Errorf("service %s.%s not configured: no endpoint registered", service, operation))
}

// benchSvcHTTPClient is a shared HTTP client for bench-svc forwarding with
// connection pooling enabled. Creating a new client per call exhausts ephemeral
// ports and adds TCP handshake latency under high concurrency.
var benchSvcHTTPClient = &http.Client{
	Timeout: 30 * time.Second,
	Transport: &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 100,
		IdleConnTimeout:     90 * time.Second,
	},
}

func (c *dbServiceCaller) forwardToBenchSvc(ctx context.Context, service, operation, requestJSON, idempotencyKey string) (string, error) {
	url := fmt.Sprintf("%s/call/%s/%s", c.benchSvcURL, service, operation)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(requestJSON))
	if err != nil {
		return "", engine.NewPermanentError("bench-svc", "", fmt.Errorf("create request: %w", err))
	}
	req.Header.Set("Content-Type", "application/json")
	if idempotencyKey != "" {
		// The conventional header name, as used by Stripe and others. A service
		// that does not recognise it ignores it, so this is safe to always send.
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	t0 := time.Now()
	resp, err := benchSvcHTTPClient.Do(req)
	if err != nil {
		return "", engine.NewTransientError("bench-svc", "", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", engine.NewTransientError("bench-svc", "", fmt.Errorf("read response: %w", err))
	}
	if resp.StatusCode != http.StatusOK {
		return "", benchSvcStatusError(resp.StatusCode, body)
	}
	slog.Debug("BENCH-SVC-CALL", "duration_ms", time.Since(t0).Milliseconds(), "body_bytes", len(body))
	return string(body), nil
}

// benchSvcStatusError classifies a non-200 from bench-svc by its status code.
//
// 4xx means bench-svc understood the request and rejected it, so sending the
// same bytes again produces the same rejection. 408 and 429 are the documented
// exceptions -- they are explicit invitations to try again. Everything else,
// including every 5xx, stays retryable.
func benchSvcStatusError(status int, body []byte) error {
	err := fmt.Errorf("%s", string(body))
	if status >= 400 && status < 500 && status != http.StatusRequestTimeout && status != http.StatusTooManyRequests {
		return engine.NewPermanentError("bench-svc", "", err)
	}
	return engine.NewTransientError("bench-svc", "", err)
}

func (c *dbServiceCaller) handleHTTPFetch(ctx context.Context, requestJSON string) (string, error) {
	var req fetchRequest
	if err := json.Unmarshal([]byte(requestJSON), &req); err != nil {
		return "", engine.NewPermanentError("http.fetch", "", fmt.Errorf("invalid request JSON: %w", err))
	}
	if req.URL == "" {
		return "", engine.NewPermanentError("http.fetch", "", errors.New("url is required"))
	}
	if req.Method == "" {
		req.Method = "GET"
	}
	var body io.Reader
	if req.Body != "" {
		body = strings.NewReader(req.Body)
	}
	httpReq, err := http.NewRequestWithContext(ctx, req.Method, req.URL, body)
	if err != nil {
		return "", engine.NewPermanentError("http.fetch", "", fmt.Errorf("invalid request %s %q: %w", req.Method, req.URL, err))
	}
	for k, v := range req.Headers {
		httpReq.Header.Set(k, v)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return "", engine.NewTransientError("http.fetch", "", fmt.Errorf("request %s %q failed: %w", req.Method, req.URL, err))
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", engine.NewTransientError("http.fetch", "", fmt.Errorf("reading response: %w", err))
	}
	respHeaders := make(map[string]string)
	for k := range resp.Header {
		respHeaders[k] = resp.Header.Get(k)
	}
	result, _ := json.Marshal(map[string]any{
		"status":  resp.StatusCode,
		"headers": respHeaders,
		"body":    string(respBody),
	})
	return string(result), nil
}

// ---------------------------------------------------------------------------
// dbWorkflowState
// ---------------------------------------------------------------------------

// dbWorkflowState implements engine.WorkflowState.
type dbWorkflowState struct {
	version       int
	minVersion    int
	priority      int
	childVersions map[string]int
}

func (s *dbWorkflowState) Version() int    { return s.version }
func (s *dbWorkflowState) MinVersion() int { return s.minVersion }
func (s *dbWorkflowState) Priority() int   { return s.priority }
func (s *dbWorkflowState) ChildVersion(name string) (int, bool) {
	if s.childVersions == nil {
		return 0, false
	}
	v, ok := s.childVersions[name]
	return v, ok
}

// ---------------------------------------------------------------------------
// hostPluginRegistryAdapter
// ---------------------------------------------------------------------------

// hostPluginRegistryAdapter bridges plugin.FuncRegistry and plugin.StreamFuncRegistry
// to engine.PluginRegistry and engine.PluginStreamRegistry.
type hostPluginRegistryAdapter struct {
	registry       *engine.PluginRegistry
	streamRegistry *engine.PluginStreamRegistry
	pluginName     string
}

func (a *hostPluginRegistryAdapter) Register(opts plugin.FuncOptions, fn plugin.PluginFunc) error {
	if opts.Name == "" {
		return fmt.Errorf("function name must not be empty")
	}
	if strings.Contains(opts.Name, "/") || strings.Contains(opts.Name, "\x00") {
		return fmt.Errorf("function name %q contains invalid characters", opts.Name)
	}
	if a.registry.Has(a.pluginName, opts.Name) {
		return fmt.Errorf("function %q already registered for plugin %q", opts.Name, a.pluginName)
	}
	if opts.Idempotent {
		return a.registry.RegisterIdempotent(a.pluginName, opts.Name, fn)
	}
	return a.registry.Register(a.pluginName, opts.Name, fn)
}

func (a *hostPluginRegistryAdapter) RegisterStream(opts plugin.FuncOptions, fn plugin.PluginStreamFunc) error {
	if opts.Name == "" {
		return fmt.Errorf("function name must not be empty")
	}
	if strings.Contains(opts.Name, "/") || strings.Contains(opts.Name, "\x00") {
		return fmt.Errorf("function name %q contains invalid characters", opts.Name)
	}
	if a.streamRegistry == nil {
		return fmt.Errorf("stream function registry not initialized")
	}
	if a.streamRegistry.Has(a.pluginName, opts.Name) {
		return fmt.Errorf("stream function %q already registered for plugin %q", opts.Name, a.pluginName)
	}
	return a.streamRegistry.RegisterStream(a.pluginName, opts, fn)
}

// ---------------------------------------------------------------------------
// WASM helpers
// ---------------------------------------------------------------------------

// determineEntryPoint extracts the entry point name from workflow input.
// If the input has an "__entry_point" field, that value is used.
// Otherwise it falls back to the first "handle_*" export in the WASM binary.
// If no exports match, it returns an empty string and the caller should fail.
func determineEntryPoint(input json.RawMessage, wasmBytes []byte) string {
	var meta struct {
		EntryPoint string `json:"__entry_point"`
	}
	if err := json.Unmarshal(input, &meta); err == nil && meta.EntryPoint != "" {
		return meta.EntryPoint
	}
	return firstHandleExport(wasmBytes)
}

// firstHandleExport scans a WASM binary's export section for the first
// exported function whose name starts with "handle_".
func firstHandleExport(wasmBytes []byte) string {
	if len(wasmBytes) < 8 {
		return ""
	}
	pos := 8 // skip magic + version
	sectionEnd := 0
	for pos < len(wasmBytes) {
		sectionID := wasmBytes[pos]
		pos++
		sectionLen, n := decodeULEB128(wasmBytes, pos)
		pos = n
		sectionEnd = pos + int(sectionLen)
		if sectionID == 7 { // export section
			count, n := decodeULEB128(wasmBytes, pos)
			pos = n
			for i := uint32(0); i < count; i++ {
				nameLen, n := decodeULEB128(wasmBytes, pos)
				pos = n
				name := string(wasmBytes[pos : pos+int(nameLen)])
				pos += int(nameLen)
				kind := wasmBytes[pos]
				pos++                                // kind (0=func)
				_, n = decodeULEB128(wasmBytes, pos) // index
				pos = n
				if kind == 0 && strings.HasPrefix(name, "handle_") {
					return name
				}
			}
			return ""
		}
		pos = sectionEnd
	}
	return ""
}

// decodeULEB128 reads an unsigned LEB128 value from buf at offset pos.
// Returns the value and the new offset.
func decodeULEB128(buf []byte, pos int) (uint32, int) {
	var result uint32
	var shift uint
	for {
		b := buf[pos]
		pos++
		result |= uint32(b&0x7F) << shift
		if b&0x80 == 0 {
			break
		}
		shift += 7
	}
	return result, pos
}

// tryClaimCumulativeAllocation atomically adds byteEstimate to counter if the
// new total would not exceed maxBytes. Returns true if the claim succeeded.
func tryClaimCumulativeAllocation(counter *atomic.Int64, byteEstimate, maxBytes int64) bool {
	for {
		cur := counter.Load()
		if cur+byteEstimate > maxBytes {
			return false
		}
		if counter.CompareAndSwap(cur, cur+byteEstimate) {
			return true
		}
	}
}

// pluginNames returns a sorted, human-readable list of plugin names from
// a map, for use in error messages.
func pluginNames(m map[string]string) string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// ---------------------------------------------------------------------------
// Defer execution
// ---------------------------------------------------------------------------

// runDefers executes registered defer callbacks in LIFO (reverse) order.
// Each defer is invoked as a WASM export named "cleat_defer_<deferID>".
// Errors during defer execution are logged but do not prevent other defers
// from running.
func (w *Worker) runDefers(wasmBytes []byte, deferrals map[string]string) {
	// Register the same backend the workflow itself ran on, so its defers are
	// fenced the same way it was. wasmtime is the only backend cleat has, and
	// it is registered for every language in engine.WasmtimeLanguages, so
	// Engine.RunDefer's backendForWasm lookup always resolves here -- there
	// is no runtime-less fallback left to reach.
	eng := engine.NewEngine(nil,
		&dbServiceCaller{store: w.store, workerID: w.id, benchSvcURL: *benchSvcURL},
		engine.WithBackends(wasmtimeLanguages, w.wasmtimeBackend))
	eng.Metrics = w.Metrics

	// Collect defer IDs sorted by step number for LIFO ordering.
	// Map iteration order is random in Go, so we always parse the step
	// number from "defer-N" and sort numerically.
	type defEntry struct {
		id     string
		desc   string
		stepNo int
	}
	var entries []defEntry
	for id, desc := range deferrals {
		var n int
		if _, err := fmt.Sscanf(id, "defer-%d", &n); err != nil {
			n = -1
		}
		entries = append(entries, defEntry{id: id, desc: desc, stepNo: n})
	}

	// Sort descending by step number for LIFO order.
	for i := 0; i < len(entries); i++ {
		for j := i + 1; j < len(entries); j++ {
			if entries[j].stepNo > entries[i].stepNo {
				entries[i], entries[j] = entries[j], entries[i]
			}
		}
	}

	for _, entry := range entries {
		deferName := "cleat_defer_" + entry.id
		_, err := eng.RunDefer(w.ctx, wasmBytes, deferName, nil)
		if err != nil {
			w.logger.ErrorContext(context.Background(), "defer execution failed", "worker_id", w.id, "defer_id", entry.id, "description", entry.desc, "error", err)
		} else {
			w.logger.InfoContext(context.Background(), "defer completed", "worker_id", w.id, "defer_id", entry.id, "description", entry.desc)
		}
	}
}

// ---------------------------------------------------------------------------
// SQL/DB helpers
// ---------------------------------------------------------------------------

// sqlDriverName maps the --driver flag value to a database/sql driver name.
func sqlDriverName(driver string) string {
	switch driver {
	case "postgres":
		return "postgres"
	case "mysql":
		return "mysql"
	case "mssql":
		return "sqlserver"
	default:
		return driver
	}
}

// mysqlBaseDSN strips the database name from a MySQL DSN, producing a base DSN
// suitable for NewMySQLStoreFactory (which expects a template without a database).
// "root:pass@tcp(host:3306)/mydb?parseTime=true" → "root:pass@tcp(host:3306)/?parseTime=true"
func mysqlBaseDSN(dsn string) string {
	// Find the last '/' which separates the database name from the address.
	slash := strings.LastIndex(dsn, "/")
	if slash < 0 {
		return dsn
	}
	// Strip the database name: keep everything up to and including '/', skip
	// the dbname, then append query params (if any).
	afterSlash := dsn[slash+1:]
	qIdx := strings.IndexByte(afterSlash, '?')
	if qIdx < 0 {
		// No query params — just return everything up to '/' plus an empty path.
		return dsn[:slash+1]
	}
	return dsn[:slash+1] + afterSlash[qIdx:]
}

// dsnWithSchema appends a PostgreSQL search_path parameter to the DSN when the
// schema is not "public".
func dsnWithSchema(dsn, schema string) string {
	if schema == "" || schema == "public" || strings.Contains(dsn, "search_path=") {
		return dsn
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	return dsn + sep + "search_path=" + schema
}

// baseDSNFromDSN parses a PostgreSQL DSN in key=value format and returns a
// base DSN with user and password stripped. If the input DSN cannot be parsed,
// it is returned as-is (the tenant pool constructor will fail gracefully).
func baseDSNFromDSN(dsn string) string {
	// Simple approach: remove user=... and password=... from the DSN.
	// This handles the common case for shard connection strings.
	var parts []string
	for _, part := range strings.Fields(dsn) {
		if strings.HasPrefix(part, "user=") || strings.HasPrefix(part, "password=") {
			continue
		}
		parts = append(parts, part)
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " ")
}

// baseDSNFromURL parses a PostgreSQL connection URL and returns the base
// connection DSN in key=value format (without user/password) for creating
// per-tenant databases.
func baseDSNFromURL(dbURL string) string {
	u, err := url.Parse(dbURL)
	if err != nil {
		return ""
	}
	host := u.Hostname()
	port := u.Port()
	if port == "" {
		port = "5432"
	}
	dbname := strings.TrimPrefix(u.Path, "/")
	sslmode := u.Query().Get("sslmode")
	if sslmode == "" {
		sslmode = "disable"
	}
	return fmt.Sprintf("host=%s port=%s dbname=%s sslmode=%s", host, port, dbname, sslmode)
}

// ---------------------------------------------------------------------------
// ID generation and error helpers
// ---------------------------------------------------------------------------

func generateWorkerID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func generateTraceID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// extractTraceIDFromTraceParent extracts the trace-id from a W3C traceparent header.
// Format: "00-{trace-id}-{parent-id}-{trace-flags}"
func extractTraceIDFromTraceParent(tp string) string {
	parts := strings.Split(tp, "-")
	if len(parts) >= 2 && len(parts[1]) == 32 {
		return parts[1]
	}
	return ""
}

func isConnectionError(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	patterns := []string{
		"connection refused",
		"connection reset",
		"connection closed",
		"no reachable servers",
		"server closed the connection",
		"connection timed out",
		"broken pipe",
		"EOF",
		"driver: bad connection",
	}
	for _, p := range patterns {
		if strings.Contains(strings.ToLower(s), strings.ToLower(p)) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Health tracker for background loop watchdog
// ---------------------------------------------------------------------------

// healthTracker records the last run time and panic status of each background loop
// for watchdog monitoring and auto-restart.
type healthTracker struct {
	mu           sync.Mutex
	lastRun      map[string]time.Time     // loop_name -> last successful run time
	panicked     map[string]bool          // loop_name -> has panicked
	restarts     map[string]int           // loop_name -> restart count
	intervals    map[string]time.Duration // loop_name -> expected run interval
	registeredAt map[string]time.Time     // loop_name -> when the loop was first registered
}

func newHealthTracker() healthTracker {
	return healthTracker{
		lastRun:      make(map[string]time.Time),
		panicked:     make(map[string]bool),
		restarts:     make(map[string]int),
		intervals:    make(map[string]time.Duration),
		registeredAt: make(map[string]time.Time),
	}
}

func (ht *healthTracker) recordRun(name string) {
	ht.mu.Lock()
	defer ht.mu.Unlock()
	ht.lastRun[name] = time.Now()
}

func (ht *healthTracker) recordPanic(name string) {
	ht.mu.Lock()
	defer ht.mu.Unlock()
	ht.panicked[name] = true
}

func (ht *healthTracker) recordRestart(name string) {
	ht.mu.Lock()
	defer ht.mu.Unlock()
	ht.restarts[name]++
}

func (ht *healthTracker) setInterval(name string, interval time.Duration) {
	ht.mu.Lock()
	defer ht.mu.Unlock()
	ht.intervals[name] = interval
}

func (ht *healthTracker) registerLoop(name string) {
	ht.mu.Lock()
	defer ht.mu.Unlock()
	ht.registeredAt[name] = time.Now()
}

// isStale re-checks a single loop atomically to prevent TOCTOU races where
// a loop recovered between the snapshot in staleLoops() and restartLoop().
func (ht *healthTracker) isStale(name string) bool {
	ht.mu.Lock()
	defer ht.mu.Unlock()
	lastRun, ok := ht.lastRun[name]
	if !ok {
		regAt, regOk := ht.registeredAt[name]
		if !regOk {
			return false
		}
		maxAge := 120 * time.Second
		if interval, iOk := ht.intervals[name]; iOk && interval > 0 {
			maxAge = interval * 6
		}
		return time.Since(regAt) > maxAge
	}
	interval, iOk := ht.intervals[name]
	maxAge := 120 * time.Second
	if iOk && interval > 0 {
		maxAge = interval * 6
	}
	return time.Since(lastRun) > maxAge
}

// registeredCount returns the total number of registered loops.
func (ht *healthTracker) registeredCount() int {
	ht.mu.Lock()
	defer ht.mu.Unlock()
	return len(ht.registeredAt)
}

// maxAge returns the maximum allowed time since the last run for a loop,
// defined as 6x the expected interval, or 120s if no interval is set.
func (ht *healthTracker) maxAge(name string) time.Duration {
	ht.mu.Lock()
	defer ht.mu.Unlock()
	interval, ok := ht.intervals[name]
	if ok && interval > 0 {
		return interval * 6
	}
	return 120 * time.Second
}

// staleLoops returns names of loops that haven't run within their maxAge.
// It also catches loops that were registered but never recorded a run
// (stuck during startup).
func (ht *healthTracker) staleLoops() []string {
	ht.mu.Lock()
	defer ht.mu.Unlock()
	var stale []string
	now := time.Now()
	for name, lastRun := range ht.lastRun {
		interval, ok := ht.intervals[name]
		maxAge := 120 * time.Second
		if ok && interval > 0 {
			maxAge = interval * 6
		}
		if now.Sub(lastRun) > maxAge {
			stale = append(stale, name)
		}
	}
	// Also check loops that were registered but never recorded a run.
	for name, regAt := range ht.registeredAt {
		if _, ok := ht.lastRun[name]; ok {
			continue
		}
		interval, iOk := ht.intervals[name]
		maxAge := 120 * time.Second
		if iOk && interval > 0 {
			maxAge = interval * 6
		}
		if now.Sub(regAt) > maxAge {
			stale = append(stale, name)
		}
	}
	return stale
}

// snapshot returns a copy of the health tracker state for metrics reporting.
func (ht *healthTracker) snapshot() (map[string]time.Time, map[string]bool, map[string]int) {
	ht.mu.Lock()
	defer ht.mu.Unlock()
	lastRun := make(map[string]time.Time)
	panicked := make(map[string]bool)
	restarts := make(map[string]int)
	for k, v := range ht.lastRun {
		lastRun[k] = v
	}
	for k, v := range ht.panicked {
		panicked[k] = v
	}
	for k, v := range ht.restarts {
		restarts[k] = v
	}
	return lastRun, panicked, restarts
}

// ---------------------------------------------------------------------------
// Signal authorization
// ---------------------------------------------------------------------------

// signalAuthCheckFor builds the signal-authorization check the worker installs
// when --require-signal-auth is set.
//
// A named function rather than a closure inside newWorker, so a test can drive
// the thing the worker actually runs. The only coverage this had was
// engine.TestWithSignalAuthCheck, which passes a stub closure and asserts that
// the option plumbing calls it -- so it could not have seen that the real check
// denies every signal, which is what IMPROVEMENT-PLAN 3.15 is about. Replacing
// the thing under test with a stub is the shape of defect 1.3 as well.
func signalAuthCheckFor(store engine.WorkflowStore) func(ctx context.Context, targetWorkflowID, callerDefName string) error {
	return func(ctx context.Context, targetWorkflowID, callerDefName string) error {
		callers, err := store.GetAllowedSignalCallers(ctx, targetWorkflowID)
		if err != nil {
			return err
		}
		if len(callers) == 0 {
			return fmt.Errorf("signal auth denied: workflow %s has no allowed callers configured", targetWorkflowID)
		}
		if signalCallerAllowed(callers, callerDefName) {
			return nil
		}
		return fmt.Errorf("signal auth denied: %s not in allowed_signals of %s", callerDefName, targetWorkflowID)
	}
}

// signalCallerAllowed checks whether a caller (by defName or "*" wildcard)
// is permitted to signal a target workflow based on its allowed_signals list.
func signalCallerAllowed(callers []string, callerDefName string) bool {
	for _, c := range callers {
		if c == "*" || c == callerDefName {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Shard configuration loading
// ---------------------------------------------------------------------------

// loadShardConfigs reads a JSON file containing an array of ShardConfig.
// The file format is:
//
//	[
//	  {"name": "shard-0", "conn_str": "postgres://...", "tenants": ["tenant-a"]},
//	  {"name": "shard-1", "conn_str": "postgres://...", "tenants": ["tenant-b"]}
//	]
func loadShardConfigs(path string) ([]engine.ShardConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read shards file: %w", err)
	}
	var configs []engine.ShardConfig
	if err := json.Unmarshal(data, &configs); err != nil {
		return nil, fmt.Errorf("parse shards file: %w", err)
	}
	if len(configs) == 0 {
		return nil, fmt.Errorf("shards file %q contains no shard definitions", path)
	}
	for i, cfg := range configs {
		if cfg.Name == "" {
			return nil, fmt.Errorf("shards file %q: entry %d has empty name", path, i)
		}
		if cfg.ConnStr == "" {
			return nil, fmt.Errorf("shards file %q: shard %q has empty conn_str", path, cfg.Name)
		}
	}
	return configs, nil
}

// ---------------------------------------------------------------------------
// Peer schemas parsing
// ---------------------------------------------------------------------------

// parsePeerSchemas splits a comma-separated list of schema names, trimming whitespace.
func parsePeerSchemas(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	result := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}

// ---------------------------------------------------------------------------
// Idempotency key cleanup
// ---------------------------------------------------------------------------

// idempotencyCleanupLoop periodically deletes idempotency keys whose associated
// workflows completed more than a week ago.
func idempotencyCleanupLoop(ctx context.Context, db *sql.DB, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Only run on Postgres. Other drivers skip silently.
			if db == nil {
				continue
			}
			cutoff := time.Now().Add(-7 * 24 * time.Hour)
			_, err := db.ExecContext(ctx, `DELETE FROM idempotency_keys WHERE created_at < $1`, cutoff)
			if err != nil {
				slog.Warn("idempotency key cleanup failed", "error", err)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Background loop helpers (watchdog)
// ---------------------------------------------------------------------------

// withPanicRecovery wraps a background loop function with panic recovery.
// The recovered panic is logged with stack trace, recorded in the health
// tracker, and the loop exits (the watchdog will restart it).
func (w *Worker) withPanicRecovery(name string, fn func()) func() {
	return func() {
		defer func() {
			if r := recover(); r != nil {
				w.logger.ErrorContext(w.ctx, "PANIC in loop", "worker_id", w.id, "loop", name, "error", r)
				stack := string(debug.Stack())
				w.logger.ErrorContext(w.ctx, "PANIC in loop", "worker_id", w.id, "loop", name, "error", r, "stack", stack)
				w.healthTracker.recordPanic(name)
				w.Metrics.RecordBackgroundLoop(context.Background(), name, "panic")
			}
		}()
		fn()
	}
}

// ---------------------------------------------------------------------------
// Worker struct and background loop methods (moved from main.go)
// ---------------------------------------------------------------------------

type Worker struct {
	id     string
	logger *slog.Logger
	store  engine.WorkflowStore

	// storeTenantID is the tenant `store` was opened as. `store` backs the
	// dispatch, heartbeat and scheduler loops, which are worker-level and stay
	// on it.
	storeTenantID string

	// storeFactory opens a store scoped to a particular tenant. Execution uses
	// it; the loops above do not.
	//
	// When it is nil -- tests, and any embedding that never set one -- there is
	// no way to route, so executeWorkflow falls back to `store` and refuses
	// anything outside storeTenantID, which is what it did before routing
	// existed.
	storeFactory engine.StoreFactory

	// tenantStores caches the result per tenant. OpenStore is cheap on
	// PostgreSQL (same *sql.DB, a new struct) but not on MySQL or SQL Server,
	// where it builds and caches a connection pool per tenant -- SQL Server has
	// to, because its RLS reads SESSION_CONTEXT, set per connection. Caching
	// here keeps the cost to once per tenant on every dialect.
	tenantStores sync.Map // map[string]engine.WorkflowStore

	// taskQueues is what `store` was opened with; a tenant-scoped store has to
	// poll the same set or it would see a different slice of the work.
	taskQueues []string

	// claimAcrossTenants makes the dispatch loop claim work for every tenant in
	// one query instead of only its own. Off by default: it needs a mechanism
	// the deployment has to grant (a BYPASSRLS owner on PostgreSQL, cleat_admin
	// membership on SQL Server), and turning it on without that should be a
	// deliberate act rather than an upgrade side effect.
	claimAcrossTenants bool

	// crossTenantUnsupportedOnce keeps the fallback warning to one line.
	crossTenantUnsupportedOnce sync.Once

	// crossTenantSchedulesUnsupportedOnce is separate from the claim's, because
	// 023 and 024 are separate migrations: a deployment can have the claim and
	// not the schedule read, and collapsing the two warnings would report only
	// whichever failed first.
	crossTenantSchedulesUnsupportedOnce sync.Once

	concurrency          int
	maxQueued            int
	heartbeatInterval    time.Duration
	pollInterval         time.Duration
	pluginRegistry       *engine.PluginRegistry
	pluginStreamRegistry *engine.PluginStreamRegistry
	tenantPools          *plugin.TenantPools
	plugList             []*plugin.LoadedPlugin

	ctx      context.Context
	cancel   context.CancelFunc
	draining atomic.Bool
	wg       sync.WaitGroup

	inflight    sync.Map // map[workflowID]*engine.WorkflowInstance
	execEngines sync.Map // map[workflowID]*engine.Engine
	wasmCache   *wasmLRUCache

	scheduleMu       sync.Mutex
	scheduleInterval time.Duration

	// Backpressure / circuit breaker state.
	consecutiveDBErrors int
	backoffUntil        time.Time
	circuitOpen         atomic.Bool

	// Compaction settings.
	Metrics             *prometheus.Metrics
	compactionThreshold int
	compactionInterval  time.Duration

	memoryController                 *MemoryController
	maxRetries                       int
	memorySampleRetention            int
	retentionDays                    int
	completedWorkflowRetentionDays   int
	schemaName                       string
	peerSchemas                      []string
	disableChecksumVerification      *bool
	requireSignalAuth                *bool
	wasmMemoryMaxMB                  *int
	wasmInstructionLimit             *int
	wasmInstanceTimeout              time.Duration
	wasmDiskCache                    *engine.WasmDiskCache
	wasmCumulativeAllocationMaxBytes int64
	cumulativeAlloc                  atomic.Int64
	wasmtimeBackend                  engine.WasmBackend

	drainCh   chan struct{}
	drainOnce sync.Once

	// Encryption at rest for sensitive event payloads.
	encryption               *engine.PayloadEncryption
	encryptSensitivePayloads bool

	// Database connection for background operations.
	db *sql.DB

	// Batch flusher for higher throughput event persistence.
	flusherRegistry *engine.TenantFlusherRegistry

	// Per-workflow resource quotas.
	maxQuotaEvents          int
	maxQuotaChildren        int
	maxQuotaConcurrencyKeys int

	// Per-tenant quota: a schedule outlives the run that created it, so it
	// cannot be counted against one.
	maxQuotaSchedules int

	// Maximum wall-clock duration per workflow execution (0 = no limit).
	maxWorkflowDuration time.Duration

	// childBindingOverride overrides the child binding policy for all tenants
	// on this worker. This is a worker-level, cross-tenant setting intended for
	// development/debugging only (e.g. "latest" forces all child workflows to
	// resolve to the latest version regardless of the compiled-in policy).
	childBindingOverride string

	// parentWakeCh is signaled when a workflow reaches a terminal status
	// (done/failed). The dispatch loop selects on this channel to skip its
	// idle sleep, waking immediately to claim the newly-ready parent.
	parentWakeCh chan struct{}

	// notifyCh receives PostgreSQL NOTIFY events for dispatch wake-up.
	// nil when not using Postgres or when --notify-channel is empty —
	// a nil channel blocks forever in select, so the case is a no-op.
	notifyCh <-chan struct{}

	// Health check interval for watchdog.
	healthCheckInterval time.Duration

	// healthTracker records the last run time of each background loop for
	// watchdog monitoring and auto-restart.
	healthTracker healthTracker

	// loopFuncs maps loop names to restart functions for the watchdog.
	loopFuncs map[string]func()

	// loopCtxMap holds per-loop cancellation contexts for clean restart.
	loopCtxMap map[string]*loopContext

	loopMu sync.Mutex // protects loopFuncs and loopCtxMap from concurrent access
}

// DrainComplete returns a channel that is closed when the drain completes
// (all in-flight workflows have finished).
func (w *Worker) DrainComplete() <-chan struct{} {
	return w.drainCh
}

// getLoopCtx returns the per-loop context for the named background loop.
// If no per-loop context has been set up yet (initial startup), it falls
// back to the worker-level context so that shutdown still works.
func (w *Worker) getLoopCtx(name string) context.Context {
	w.loopMu.Lock()
	lc, ok := w.loopCtxMap[name]
	w.loopMu.Unlock()
	if ok {
		return lc.ctx
	}
	return w.ctx
}

// initLoopCtx creates a cancellable per-loop context and registers the loop
// for watchdog monitoring. The done channel is closed by launchLoop when the
// goroutine exits.
//
// The map write is under loopMu, which it was not when this was a closure
// inside Run. That was not academic: Run initialises nine loops, launches them,
// and *then* calls this again for the watchdog -- so an unlocked write ran
// while nine goroutines were already up, and getLoopCtx reads the same map
// under the lock. A worker died of it in the cluster job:
//
//	fatal error: concurrent map read and map write
//	main.(*Worker).getLoopCtx        setup.go:1070
//	main.(*Worker).reaperLoop        setup.go:1868
//
// "concurrent map read and map write" is a runtime fatal, not a panic, so
// withPanicRecovery cannot catch it and the process dies -- which is what
// crash-looped cleat-worker-3.
//
// A method rather than a closure so it can be called from a test that runs it
// against a live reader under -race.
func (w *Worker) initLoopCtx(name string) {
	ctx, cancel := context.WithCancel(w.ctx)
	lc := &loopContext{
		ctx:    ctx,
		cancel: cancel,
		done:   make(chan struct{}),
	}
	w.loopMu.Lock()
	w.loopCtxMap[name] = lc
	w.loopMu.Unlock()
	w.healthTracker.registerLoop(name)
}

// registerLoopFunc records a loop's restart function under loopMu.
//
// Same reasoning as initLoopCtx: these assignments interleave with launchLoop
// calls, so by the time the later ones run several goroutines exist. restartLoop
// reads loopFuncs under the lock, and it is only reached from the watchdog --
// which starts last -- so this half was not yet reachable in practice. It is
// the same latent shape and costs one helper to close.
func (w *Worker) registerLoopFunc(name string, fn func()) {
	w.loopMu.Lock()
	w.loopFuncs[name] = fn
	w.loopMu.Unlock()
}

// launchLoop starts a background loop goroutine and ensures the per-loop
// done channel is closed when the goroutine exits. This makes the initial
// launch consistent with the restart path (restartLoop), eliminating the
// 5-second timeout on the first watchdog restart.
// The done channel is captured at call time so that if restartLoop swaps
// the loopCtxMap entry before this goroutine exits, we still close the
// correct (original) channel.
func (w *Worker) launchLoop(name string, fn func()) {
	w.wg.Add(1)
	w.loopMu.Lock()
	done := w.loopCtxMap[name].done
	w.loopMu.Unlock()
	go func() {
		defer close(done)
		w.withPanicRecovery(name, fn)()
	}()
}

func (w *Worker) Run() {
	// Initialize the global time seed so the first workflow execution
	// (before the dispatch loop updates it) sees a real wall clock.
	engine.UpdateNowMs()

	// Initialize health tracker and loop registry for watchdog.
	w.healthTracker = newHealthTracker()
	w.loopFuncs = make(map[string]func())
	w.loopCtxMap = make(map[string]*loopContext)

	initLoopCtx := w.initLoopCtx
	initLoopCtx("heartbeat")
	initLoopCtx("reaper")
	initLoopCtx("concurrency_key_reaper")
	initLoopCtx("dispatch")
	initLoopCtx("schedule")
	initLoopCtx("memory_reload")
	initLoopCtx("memory_cleanup")
	initLoopCtx("retention")
	initLoopCtx("compaction")
	initLoopCtx("update_dispatch")

	// Background heartbeat goroutine.
	w.registerLoopFunc("heartbeat", w.heartbeatLoop)
	w.launchLoop("heartbeat", w.heartbeatLoop)

	// Background zombie reaper goroutine.
	w.registerLoopFunc("reaper", w.reaperLoop)
	w.launchLoop("reaper", w.reaperLoop)

	// Background concurrency key reaper goroutine (Feature 5).
	w.registerLoopFunc("concurrency_key_reaper", w.concurrencyKeyReaperLoop)
	w.launchLoop("concurrency_key_reaper", w.concurrencyKeyReaperLoop)

	// Dispatch loop.
	w.registerLoopFunc("dispatch", w.dispatchLoop)
	// Before either loop runs, so the answer is in the log above the first
	// tick rather than inside it.
	w.reportCrossTenantCapability()

	w.launchLoop("dispatch", w.dispatchLoop)

	// Cron schedule loop.
	w.registerLoopFunc("schedule", w.scheduleLoop)
	w.launchLoop("schedule", w.scheduleLoop)

	// Memory estimate reload loop.
	w.registerLoopFunc("memory_reload", w.memoryReloadLoop)
	w.launchLoop("memory_reload", w.memoryReloadLoop)

	// Memory sample cleanup loop.
	w.registerLoopFunc("memory_cleanup", func() { w.memoryCleanupLoop(w.memorySampleRetention) })
	w.launchLoop("memory_cleanup", func() { w.memoryCleanupLoop(w.memorySampleRetention) })

	// Retention loop.
	w.registerLoopFunc("retention", func() { w.retentionLoop(w.retentionDays, w.completedWorkflowRetentionDays) })
	w.launchLoop("retention", func() { w.retentionLoop(w.retentionDays, w.completedWorkflowRetentionDays) })

	// Update dispatch loop (Feature 3: Update Handler).
	w.registerLoopFunc("update_dispatch", func() { w.updateDispatchLoop(w.getLoopCtx("update_dispatch")) })
	w.launchLoop("update_dispatch", func() { w.updateDispatchLoop(w.getLoopCtx("update_dispatch")) })

	// Watchdog loop for background loop health monitoring.
	if w.healthCheckInterval > 0 {
		initLoopCtx("watchdog")
		w.registerLoopFunc("watchdog", w.watchdogLoop)
		w.launchLoop("watchdog", w.watchdogLoop)
	}

	w.logger.InfoContext(w.ctx, "running", "worker_id", w.id)

	<-w.ctx.Done()

	// Graceful shutdown: wait for in-flight workflows.
	w.logger.InfoContext(w.ctx, "waiting for in-flight workflows to complete", "worker_id", w.id)
	w.wg.Wait()
}

func (w *Worker) dispatchLoop() {
	defer w.wg.Done()
	w.healthTracker.setInterval("dispatch", w.pollInterval)

	// Keep the global time seed fresh for workflow sessions.
	engine.UpdateNowMs()

	const maxBatchSize = 20 // cap claims per query to avoid oversized batches
	idleTicks := 0
	const maxIdleTicks = 6 // progressive backoff caps at 6 * pollInterval

	for {
		// When the worker is shutting down (context cancelled) or
		// draining, stop claiming new work and wait for in-flight
		// workflows to finish so events can be flushed cleanly.
		if w.ctx.Err() != nil || w.draining.Load() {
			if !w.draining.Load() {
				w.draining.Store(true)
				w.logger.InfoContext(w.ctx, "shutdown signal received; draining in-flight workflows", "worker_id", w.id)
			}
			inflight := 0
			w.inflight.Range(func(_, _ any) bool { inflight++; return true })
			if inflight == 0 {
				w.logger.InfoContext(w.ctx, "drain complete", "worker_id", w.id)
				return
			}
			time.Sleep(w.pollInterval)
			continue
		}

		w.healthTracker.recordRun("dispatch")
		// Memory-aware tick: read system memory, compute pressure, adjust concurrency.
		w.memoryController.Tick(w.ctx)
		state := w.memoryController.State()
		w.Metrics.RecordMemoryRSS(w.ctx, int64(state.UsedBytes))
		w.Metrics.RecordMemoryAvailable(w.ctx, int64(state.AvailableBytes))
		w.Metrics.RecordMemoryTotal(w.ctx, int64(state.TotalBytes))
		w.Metrics.RecordConcurrencyLimit(w.ctx, int64(state.DynamicConcurrency))
		w.Metrics.SetMemoryPressure(w.ctx, state.Pressure)
		w.Metrics.SetScalingPressure(w.ctx, state.ScalingPressure)
		for defName, bytes := range w.memoryController.DefEstimates() {
			w.Metrics.RecordWorkflowMemoryEstimate(w.ctx, defName, bytes)
		}
		w.Metrics.SetQueueDepth(w.ctx, state.QueueDepth)
		updateThroughputGauges()

		if !w.memoryController.CanClaim() {
			time.Sleep(w.pollInterval)
			continue
		}

		// Count in-flight workflows.
		count := 0
		w.inflight.Range(func(_, _ any) bool {
			count++
			return true
		})

		free := w.memoryController.DynamicConcurrency() - count
		if free <= 0 {
			time.Sleep(w.pollInterval)
			continue
		}

		// If draining, stop claiming new work.
		if w.draining.Load() {
			time.Sleep(w.pollInterval)
			continue
		}

		batchSize := free
		if batchSize > maxBatchSize {
			batchSize = maxBatchSize
		}

		// Improvement 2: Try sticky fast-path first (low contention).
		pollStart := time.Now()
		stickyWfs, err := w.store.ClaimStickyWorkflows(w.ctx, w.id, batchSize)
		if err != nil {
			if isConnectionError(err) {
				w.consecutiveDBErrors++
				backoff := time.Duration(w.consecutiveDBErrors) * time.Second
				if backoff > 30*time.Second {
					backoff = 30 * time.Second
				}
				w.logger.WarnContext(w.ctx, "DB unreachable during sticky claim", "worker_id", w.id, "backoff", backoff)
				select {
				case <-w.ctx.Done():
					return
				case <-w.parentWakeCh:
				case <-time.After(backoff):
				}
				continue
			}
			w.logger.ErrorContext(w.ctx, "sticky claim error", "worker_id", w.id, "error", err)
			time.Sleep(time.Second)
			continue
		}

		// Improvement 1: Fill remaining capacity with general batch claim.
		remaining := batchSize - len(stickyWfs)
		var generalWfs []*engine.WorkflowInstance
		if remaining > 0 {
			var err error
			generalWfs, err = w.claimGeneral(remaining)
			w.Metrics.RecordPollWaitDuration(w.ctx, time.Since(pollStart))
			if err != nil {
				if isConnectionError(err) {
					w.consecutiveDBErrors++
					backoff := time.Duration(w.consecutiveDBErrors) * time.Second
					if backoff > 30*time.Second {
						backoff = 30 * time.Second
					}
					w.logger.WarnContext(w.ctx, "DB unreachable during claim", "worker_id", w.id, "backoff", backoff)
					select {
					case <-w.ctx.Done():
						return
					case <-w.parentWakeCh:
					case <-time.After(backoff):
					}
					continue
				}
				w.logger.ErrorContext(w.ctx, "claim error", "worker_id", w.id, "error", err)
				time.Sleep(time.Second)
				continue
			}
		}

		// Combine results.
		wfs := append(stickyWfs, generalWfs...)

		if len(wfs) == 0 {
			// No work found — progressive backoff.
			idleTicks++
			sleep := time.Duration(idleTicks) * w.pollInterval
			if idleTicks > maxIdleTicks {
				sleep = maxIdleTicks * w.pollInterval
			}
			select {
			case <-w.ctx.Done():
				return
			case <-w.parentWakeCh:
				idleTicks = 0 // reset backoff, poll immediately
			case <-w.notifyCh:
				idleTicks = 0 // PostgreSQL NOTIFY: reset backoff, poll immediately
			case <-time.After(sleep):
			}
			continue
		}

		// Improvement 3: Found work — reset idle counter (coalesced polling).
		// When the claim returned a full batch there is likely more work;
		// add a brief pause to avoid a tight polling loop against the DB.
		idleTicks = 0
		if len(wfs) == batchSize {
			time.Sleep(10 * time.Millisecond)
		}
		w.consecutiveDBErrors = 0 // reset circuit breaker on success

		w.Metrics.RecordWorkflowsClaimed(w.ctx, int64(len(wfs)))

		// Re-check draining after claim to close the TOCTOU window between
		// the drain check above and the DB claim calls.
		if w.draining.Load() {
			for _, wf := range wfs {
				w.releaseWorkflow(wf)
			}
			continue
		}

		for _, wf := range wfs {
			w.logger.InfoContext(w.ctx, "claimed workflow", "worker_id", w.id, "workflow_id", wf.ID, "def_name", wf.DefName, "def_version", wf.DefVersion)
			w.Metrics.RecordDispatchLatency(w.ctx, time.Since(wf.CreatedAt), "")

			w.inflight.Store(wf.ID, wf)
			w.wg.Add(1)
			go w.executeWorkflow(wf)
		}
	}
}

func (w *Worker) executeWorkflow(wf *engine.WorkflowInstance) {
	defer w.wg.Done()
	defer w.execEngines.Delete(wf.ID)
	defer w.inflight.Delete(wf.ID)
	defer func() {
		if r := recover(); r != nil {
			w.logger.ErrorContext(context.Background(), "PANIC in workflow", "worker_id", w.id, "workflow_id", wf.ID, "tenant_id", wf.TenantID, "error", r)
			w.releaseOrFail(wf, fmt.Sprintf("panic: %v", r))
		}
	}()
	w.Metrics.RecordWorkflowStarted(context.Background(), wf.DefName)
	w.Metrics.AddWorkflowActive(context.Background(), 1, wf.DefName)
	defer w.Metrics.AddWorkflowActive(context.Background(), -1, wf.DefName)
	workflowStartTime := time.Now()

	// Measure memory usage before and after to estimate per-workflow footprint.
	beforeMem := w.memoryController.monitor.SampleUsage()
	defer func() {
		afterMem := w.memoryController.monitor.SampleUsage()
		if afterMem > beforeMem {
			delta := afterMem - beforeMem
			if delta > 0 {
				w.memoryController.RecordWorkflowMemory(context.Background(), wf.DefName, delta)
			}
		}
	}()

	// ---- Tenant-scoped store ----
	//
	// Execution writes through this store, not w.store: event history, state,
	// child workflows, schedules. Routing on wf.TenantID is what lets a worker
	// run a tenant other than the one its dispatch loop is scoped to -- and
	// because the store comes from OpenStore(wf.TenantID), it is correct by
	// construction rather than by a check downstream.
	execStore, storeErr := w.storeForTenant(wf.TenantID)
	if storeErr != nil {
		errMsg := fmt.Sprintf("workflow %s: no store for tenant %s: %v", wf.ID, wf.TenantID, storeErr)
		w.logger.ErrorContext(context.Background(), "tenant store unavailable",
			"worker_id", w.id, "workflow_id", wf.ID, "tenant_id", wf.TenantID, "error", storeErr)
		w.recordTerminalFailure(wf, workflowStartTime, errMsg, engine.ErrUnknown.String(), "tenant_store")
		return
	}

	// ---- Tenant scope check ----
	//
	// This worker holds ONE store, opened as storeTenantID, and every write
	// below goes through it: the trace ID, the event history, state, child
	// workflows, schedules. The engine is separately told
	// WithTenantID(wf.TenantID), so there are two notions of tenant here and
	// nothing else compares them.
	//
	// Today they cannot disagree, because the claim only ever returns rows for
	// the store's own tenant -- which means this check never fires, and that is
	// the point of adding it now rather than later. The same restriction that
	// makes a non-default tenant's workflows never run (see
	// engine.TestScheduleLoop_OnlySeesItsOwnTenantsSchedules) is also the only
	// thing preventing a much worse failure: widen the claim without this, and
	// another tenant's history, state and schedules get written under
	// storeTenantID's ID, with RLS satisfied at every step because the store
	// genuinely is that tenant. Silent cross-tenant corruption, invisible to
	// every isolation test we have.
	//
	// Failing the run is the right answer rather than releasing it: no
	// correctly-scoped worker exists to pick it up, so releasing would spin.
	//
	// Only when there is no factory to route with. With one, execStore IS
	// wf.TenantID's store and there is nothing to refuse. Without one the old
	// hazard is unchanged: every write would go through w.store, under
	// storeTenantID's ID, with RLS satisfied because the store genuinely is
	// that tenant.
	if w.storeFactory == nil && wf.TenantID != "" && w.storeTenantID != "" && wf.TenantID != w.storeTenantID {
		errMsg := fmt.Sprintf("workflow %s belongs to tenant %s but this worker executes tenant %s "+
			"and has no store factory to route with; refusing rather than writing its history "+
			"under the wrong tenant", wf.ID, wf.TenantID, w.storeTenantID)
		w.logger.ErrorContext(context.Background(), "tenant scope violation",
			"worker_id", w.id, "workflow_id", wf.ID,
			"workflow_tenant_id", wf.TenantID, "worker_tenant_id", w.storeTenantID)
		w.recordTerminalFailure(wf, workflowStartTime, errMsg, engine.ErrPermanent.String(), "tenant_scope")
		return
	}

	// ---- Assign trace ID ----
	traceID := wf.TraceID
	if traceID == "" {
		traceID = generateTraceID()
	}
	if err := execStore.TraceWorkflow(context.Background(), wf.ID, traceID); err != nil {
		w.logger.WarnContext(context.Background(), "failed to set trace_id", "worker_id", w.id, "workflow_id", wf.ID, "tenant_id", wf.TenantID, "error", err)
	}

	// ---- Load WASM ----
	wasmStart := time.Now()
	wasmBytes, err := w.loadWASM(wf.DefName, wf.DefVersion)
	w.Metrics.RecordWasmCompileDuration(context.Background(), time.Since(wasmStart), wf.DefName)
	if err != nil {
		w.logger.ErrorContext(context.Background(), "failed to load WASM", "worker_id", w.id, "workflow_id", wf.ID, "tenant_id", wf.TenantID, "error", err)
		var ce *engine.CleatError
		errorCode := engine.ErrUnknown.String()
		errorOp := ""
		if errors.As(err, &ce) {
			errorCode = ce.Code.String()
			errorOp = ce.Op
		}
		errMsg := err.Error()
		w.recordTerminalFailure(wf, workflowStartTime, errMsg, errorCode, errorOp)
		return
	}

	// ---- Memory cap check ----
	if w.wasmMemoryMaxMB != nil && *w.wasmMemoryMaxMB > 0 {
		requiredPages := wasm.ReadMemoryInitialPages(wasmBytes)
		allowedPages := uint32(*w.wasmMemoryMaxMB * 1024 * 1024 / 65536)
		if allowedPages > 65536 {
			allowedPages = 65536
		}
		if requiredPages > allowedPages {
			requiredMB := float64(requiredPages) * 65536 / 1024 / 1024
			errMsg := fmt.Sprintf("module requires %d pages (%.0f MB) but max is %d pages (%d MB); increase --wasm-memory-max-mb or reduce module memory usage",
				requiredPages, requiredMB, allowedPages, *w.wasmMemoryMaxMB)
			w.logger.ErrorContext(context.Background(), "execution error", "worker_id", w.id, "workflow_id", wf.ID, "tenant_id", wf.TenantID, "error", errMsg)
			w.recordTerminalFailure(wf, workflowStartTime, errMsg, engine.ErrUnknown.String(), "")
			return
		}
	}

	// ---- Cumulative WASM allocation check ----
	if w.wasmCumulativeAllocationMaxBytes > 0 {
		requiredPages := wasm.ReadMemoryInitialPages(wasmBytes)
		byteEstimate := int64(requiredPages) * 65536
		if !tryClaimCumulativeAllocation(&w.cumulativeAlloc, byteEstimate, w.wasmCumulativeAllocationMaxBytes) {
			cur := w.cumulativeAlloc.Load()
			errMsg := fmt.Sprintf("cumulative WASM allocation limit reached: current %d bytes (%.0f MB) + required %d bytes (%.0f MB) exceeds max %d bytes (%.0f MB)",
				cur, float64(cur)/1024/1024, byteEstimate, float64(byteEstimate)/1024/1024, w.wasmCumulativeAllocationMaxBytes, float64(w.wasmCumulativeAllocationMaxBytes)/1024/1024)
			w.logger.ErrorContext(context.Background(), "execution error", "worker_id", w.id, "workflow_id", wf.ID, "tenant_id", wf.TenantID, "error", errMsg)
			w.recordTerminalFailure(wf, workflowStartTime, errMsg, engine.ErrUnknown.String(), "")
			return
		}
		defer w.cumulativeAlloc.Add(-byteEstimate)
	}

	// ---- Load event history ----
	history, err := execStore.LoadEventHistory(w.ctx, wf.ID)
	if err != nil {
		if isConnectionError(err) {
			w.logger.WarnContext(context.Background(), "DB down loading history", "worker_id", w.id, "workflow_id", wf.ID, "tenant_id", wf.TenantID)
			w.releaseWorkflow(wf)
			return
		}
		w.recordTerminalFailure(wf, workflowStartTime, fmt.Sprintf("workflow %s: history load: %v", wf.ID, err), engine.ErrUnknown.String(), "")
		return
	}

	if engine.DebugTiming {
		w.logger.InfoContext(context.Background(), "loaded history events", "worker_id", w.id, "workflow_id", wf.ID, "tenant_id", wf.TenantID, "count", len(history))
	}

	// ---- Determine entry point ----
	entryPoint := determineEntryPoint(wf.Input, wasmBytes)
	if entryPoint == "" {
		w.recordTerminalFailure(wf, workflowStartTime,
			"cannot determine entry point: no __entry_point in input and no handle_* export in WASM binary",
			engine.ErrPermanent.String(), "")
		return
	}

	// ---- Load compaction state if present ----
	var compactionState *engine.CompactionState
	compactionState, err = execStore.LoadCompactionState(w.ctx, wf.ID)
	if err != nil {
		w.logger.WarnContext(context.Background(), "failed to load compaction state", "worker_id", w.id, "workflow_id", wf.ID, "tenant_id", wf.TenantID, "error", err)
		compactionState = nil
	}

	// wasmtime is the only WASM backend cleat has (w.wasmtimeBackend is
	// guaranteed non-nil -- main.go exits fatally if NewWasmtimeBackend
	// fails), and it is registered for every language in
	// engine.WasmtimeLanguages, so the engine never needs a Runtime here.

	// Extract child version pins from WASM metadata (compile-time resolution).
	var childVersions map[string]int
	var wfMeta *wasm.Metadata
	if m, err := wasm.ReadMetadata(wasmBytes); err == nil {
		childVersions = m.ChildVersions
		wfMeta = m
	}

	// ---- Pre-flight correctness checks ----

	// (a) Verify the WASM binary version matches the workflow
	// definition version stored in workflow_defs.  A mismatch means
	// the wrong binary was deployed or the DB row is stale.
	if wfMeta != nil && wfMeta.WorkflowVersion != wf.DefVersion {
		err := fmt.Errorf(
			"version mismatch: workflow instance %s expects def_version %d but WASM binary metadata reports version %d (def=%s). The workflow_defs row and the deployed WASM binary are out of sync.",
			wf.ID, wf.DefVersion, wfMeta.WorkflowVersion, wf.DefName)
		w.logger.ErrorContext(context.Background(), "execution error", "worker_id", w.id, "workflow_id", wf.ID, "tenant_id", wf.TenantID, "error", err)
		w.recordTerminalFailure(wf, workflowStartTime, err.Error(), engine.ErrPermanent.String(), "version_check")
		return
	}

	// (b) Verify plugin dependencies in the WASM binary match the
	// plugins loaded in this worker.  A missing or mismatched plugin
	// version will cause runtime failures in host function calls.
	if wfMeta != nil && len(wfMeta.PluginDeps) > 0 {
		// Build a map of loaded plugin versions.
		workerPlugins := make(map[string]string)
		for _, lp := range w.plugList {
			info := lp.Plugin.Info()
			workerPlugins[info.Name] = info.Version
		}
		for pluginName, requiredVersion := range wfMeta.PluginDeps {
			workerVersion, ok := workerPlugins[pluginName]
			if !ok {
				err := fmt.Errorf(
					"missing plugin: workflow requires plugin %q version %s but it is not installed in this worker. Available plugins: %v",
					pluginName, requiredVersion, pluginNames(workerPlugins))
				w.logger.ErrorContext(context.Background(), "execution error", "worker_id", w.id, "workflow_id", wf.ID, "tenant_id", wf.TenantID, "error", err)
				w.recordTerminalFailure(wf, workflowStartTime, err.Error(), engine.ErrPermanent.String(), "plugin_check")
				return
			}
			if workerVersion != requiredVersion {
				err := fmt.Errorf(
					"plugin version mismatch: workflow requires plugin %q version %s but worker has version %s",
					pluginName, requiredVersion, workerVersion)
				w.logger.ErrorContext(context.Background(), "execution error", "worker_id", w.id, "workflow_id", wf.ID, "tenant_id", wf.TenantID, "error", err)
				w.recordTerminalFailure(wf, workflowStartTime, err.Error(), engine.ErrPermanent.String(), "plugin_check")
				return
			}
		}
	}

	// Extract child binding policy from WASM metadata for the engine.
	var childBindingPolicy string
	if wfMeta != nil {
		childBindingPolicy = wfMeta.ChildBindingPolicy
	}

	caller := &dbServiceCaller{store: execStore, workerID: w.id, benchSvcURL: *benchSvcURL}
	engineOpts := []engine.EngineOption{
		engine.WithSignalStore(execStore.(engine.SignalStore)),
		engine.WithWorkflowState(&dbWorkflowState{version: wf.DefVersion, minVersion: wf.MinVersion, priority: wf.Priority, childVersions: childVersions}),
		engine.WithWorkflowID(wf.ID),
		// B4: the same claim identity (w.id, wf.Generation) every terminal
		// write below (ContinueAsNew, FinalizeWorkflowSegment) already
		// fences on. Without these two, Engine.fencingEnabled() is false and
		// the per-step flush / write-ahead-intent paths stay unfenced, which
		// was the whole finding -- so this is not optional wiring.
		engine.WithWorkerID(w.id),
		engine.WithGeneration(wf.Generation),
		engine.WithTraceID(traceID),
		engine.WithTenantID(wf.TenantID),
		engine.WithBackends(wasmtimeLanguages, w.wasmtimeBackend),
		engine.WithWorkflowStore(execStore),
		engine.WithChildWorkflowStore(execStore),
		engine.WithPluginRegistry(w.pluginRegistry),
		engine.WithMaxRetryAttempts(w.maxRetries),
		engine.WithSchema(w.schemaName),
		engine.WithPeerSchemas(w.peerSchemas),
		engine.WithEncryption(w.encryption, w.encryptSensitivePayloads),
		engine.WithMaxQuotaEvents(w.maxQuotaEvents),
		engine.WithMaxQuotaChildren(w.maxQuotaChildren),
		engine.WithMaxQuotaConcurrencyKeys(w.maxQuotaConcurrencyKeys),
		engine.WithMaxQuotaSchedules(w.maxQuotaSchedules),
		engine.WithDefaultWorkflowTimeout(w.maxWorkflowDuration),
		engine.WithWASMInstanceTimeout(w.wasmInstanceTimeout),
		engine.WithChildBindingPolicy(childBindingPolicy),
		engine.WithChildBindingOverride(w.childBindingOverride),
	}
	// If the store supports concurrency keys (PostgresStore, ShardedStore),
	// enable virtual object scope enforcement.
	if cks, ok := execStore.(engine.ConcurrencyKeyStore); ok {
		engineOpts = append(engineOpts, engine.WithConcurrencyKeyStore(cks))
	}
	// Enable event history checksum verification on replay by default.
	// Can be disabled with --disable-checksum-verification.
	if w.disableChecksumVerification != nil && !*w.disableChecksumVerification {
		engineOpts = append(engineOpts, engine.WithWorkflowEventVerifier(execStore.VerifyWorkflowEvents, false))
	}
	// Enable signal authorization if --require-signal-auth is set.
	if w.requireSignalAuth != nil && *w.requireSignalAuth {
		engineOpts = append(engineOpts,
			engine.WithRequireSignalAuth(true),
			engine.WithSignalAuthCheck(signalAuthCheckFor(execStore)),
		)
	}
	// Enable event history checksum verification on replay by default.
	// Can be disabled with --disable-checksum-verification.
	if w.disableChecksumVerification != nil && !*w.disableChecksumVerification {
		engineOpts = append(engineOpts, engine.WithWorkflowEventVerifier(execStore.VerifyWorkflowEvents, true))
	}
	// Always provide DB so per-step flush and adaptive flusher work.
	engineOpts = append(engineOpts, engine.WithDB(w.db))
	// Use tenant-scoped database connection for plugin host functions if available.
	if w.tenantPools != nil && wf.TenantID != "" {
		tenantDB, err := w.tenantPools.For(w.ctx, wf.TenantID)
		if err != nil {
			w.logger.ErrorContext(context.Background(), "cannot get tenant pool", "worker_id", w.id, "workflow_id", wf.ID, "tenant_id", wf.TenantID, "error", err)
			w.recordTerminalFailure(wf, workflowStartTime, fmt.Sprintf("tenant pool: %v", err), engine.ErrUnknown.String(), "")
			return
		}
		engineOpts = append(engineOpts, engine.WithDB(tenantDB))
	}
	if compactionState != nil {
		engineOpts = append(engineOpts, engine.WithCompactionState(compactionState))
		w.logger.InfoContext(context.Background(), "loaded compaction state", "worker_id", w.id, "workflow_id", wf.ID, "tenant_id", wf.TenantID, "compacted_step", compactionState.CompactedStep)
	}
	// When replaying, validate version compatibility between the old
	// workflow definition (from the instance) and the new definition
	// (from the WASM binary) so incompatible transitions fail fast.
	if len(history) > 0 {
		oldDef, err := execStore.GetWorkflowDef(w.ctx, wf.DefName, wf.DefVersion)
		if err == nil && oldDef != nil && wfMeta != nil {
			newDef := &engine.WorkflowDef{
				Name:       wfMeta.WorkflowName,
				Version:    wfMeta.WorkflowVersion,
				ABIVersion: wfMeta.ABIVersion,
				MinVersion: wfMeta.MinCompatibleVersion,
				PluginDeps: wfMeta.PluginDeps,
			}
			engineOpts = append(engineOpts, engine.WithVersionValidation(func() error {
				return engine.ValidateVersionCompatibility(oldDef, newDef)
			}))
		}
	}
	// Load initial event count so the engine tracks events locally.
	if w.maxQuotaEvents > 0 {
		if count, err := execStore.GetEventCount(w.ctx, wf.ID); err == nil {
			engineOpts = append(engineOpts, engine.WithInitialEventCount(count))
		}
	}
	if noPerStepFlush != nil && *noPerStepFlush {
		engineOpts = append(engineOpts, engine.WithNoPerStepFlush(true))
	}
	// IMPROVEMENT-PLAN 1.4 phase D. Without this the engine-side mechanism is
	// reachable only by an embedder, and the worker -- the artifact a
	// deployment actually runs -- could not turn it on. That is the shape 1.4
	// is about: durability code that is tested, believed and unreachable.
	if ops := parseWriteAheadIntentOps(writeAheadIntentOps); len(ops) > 0 {
		engineOpts = append(engineOpts, engine.WithWriteAheadIntentOps(ops...))
	}
	if w.flusherRegistry != nil {
		engineOpts = append(engineOpts, engine.WithFlusherRegistry(w.flusherRegistry))
	} else {
		w.logger.InfoContext(context.Background(), "flusher registry not set on worker — using direct flush", "workflow_id", wf.ID)
	}
	// Throttle cancellation polls to at most once per 100ms wall-clock
	// to avoid a full DB transaction on every durable step.
	engineOpts = append(engineOpts, engine.WithCancellationCheckInterval(100*time.Millisecond))
	eng := engine.NewEngine(nil, caller, engineOpts...)
	eng.Metrics = w.Metrics

	w.execEngines.Store(wf.ID, eng)

	// ---- Execute/Resume ----
	inputJSON := wf.Input
	setupElapsed := time.Since(workflowStartTime)
	engineStart := time.Now()
	result, resultHistory, suspended, deferrals, queryState, err := eng.Replay(w.ctx, wasmBytes, entryPoint, inputJSON, history)
	engineElapsed := time.Since(engineStart)
	if len(history) > 0 {
		w.Metrics.RecordReplayDuration(context.Background(), engineElapsed)
	} else {
		w.Metrics.RecordFreshDuration(context.Background(), engineElapsed, wf.DefName)
	}
	// Update throughput gauges (events/sec).
	if engineElapsed.Seconds() > 0 {
		eventsPerSec := float64(len(resultHistory)) / engineElapsed.Seconds()
		if len(history) > 0 {
			w.Metrics.SetReplayThroughput(context.Background(), eventsPerSec)
		} else {
			w.Metrics.SetFreshThroughput(context.Background(), eventsPerSec)
		}
	}
	if err != nil {
		w.logger.ErrorContext(context.Background(), "execution error", "worker_id", w.id, "workflow_id", wf.ID, "tenant_id", wf.TenantID, "error", err)
		var ce *engine.CleatError
		errorCode := engine.ErrUnknown.String()
		errorOp := ""
		if errors.As(err, &ce) {
			errorCode = ce.Code.String()
			errorOp = ce.Op
		}
		errMsg := err.Error()
		w.recordTerminalFailure(wf, workflowStartTime, errMsg, errorCode, errorOp)
		return
	}

	// ---- Handle result ----
	// Determine the final status before any DB writes so we can use the
	// appropriate atomic method for each path.

	// Collect new events (if any) so the same slice is available to all branches.
	var newEvents []engine.EventRecord
	if len(resultHistory) > len(history) {
		newEvents = resultHistory[len(history):]
		// Redact sensitive fields in new events before persisting.
		for i := range newEvents {
			newEvents[i].Request = engine.Redact(newEvents[i].Request)
			newEvents[i].Response = engine.Redact(newEvents[i].Response)
		}
	}

	if suspended != nil && suspended.Reason == "continue_as_new" {
		// ContinueAsNew: atomically append events, create a new run, and
		// complete the current one — all in a single database transaction.
		w.logger.InfoContext(context.Background(), "continue_as_new: starting new run", "worker_id", w.id, "workflow_id", wf.ID, "tenant_id", wf.TenantID)
		newRunID, err := execStore.ContinueAsNew(w.ctx, wf.ID, w.id, wf.Generation, wf.DefName, wf.DefVersion, json.RawMessage(suspended.NewInput), newEvents, result, queryState, wf.Priority)
		if errors.Is(err, engine.ErrFenceLost) {
			// Normal and expected under reaping: another worker now owns
			// this workflow (this one was reaped as stale and reclaimed).
			// Not an error -- return cleanly without retrying or failing
			// the workflow out from under its new owner.
			w.logger.DebugContext(context.Background(), "continue_as_new: fence lost, workflow reassigned to another worker", "worker_id", w.id, "workflow_id", wf.ID, "tenant_id", wf.TenantID)
			return
		}
		if err != nil {
			w.logger.ErrorContext(context.Background(), "continue_as_new failed", "worker_id", w.id, "workflow_id", wf.ID, "tenant_id", wf.TenantID, "error", err)
			w.recordTerminalFailure(wf, workflowStartTime, fmt.Sprintf("continue_as_new: %v", err), engine.ErrUnknown.String(), "")
			return
		}
		w.logger.InfoContext(context.Background(), "continued as new run", "worker_id", w.id, "workflow_id", wf.ID, "tenant_id", wf.TenantID, "new_run_id", newRunID)
		w.Metrics.RecordWorkflowCompleted(context.Background(), wf.DefName, "")
		w.Metrics.RecordWorkflowDuration(context.Background(), time.Since(workflowStartTime), wf.DefName, "done", "")
		return
	}

	// Non-ContinueAsNew: atomically append events and finalize the workflow status
	// in a single database transaction.
	finalStatus := "done"
	var nextWakeAt time.Time
	if suspended != nil {
		finalStatus = "ready"
		nextWakeAt = suspended.SuspendUntil
	}

	queryStart := time.Now()
	err = execStore.FinalizeWorkflowSegment(w.ctx, wf.ID, w.id, wf.Generation, newEvents, finalStatus, result, "", "", queryState, nextWakeAt)
	finalizeElapsed := time.Since(queryStart)
	if errors.Is(err, engine.ErrFenceLost) {
		// Normal and expected under reaping: another worker now owns this
		// workflow (this one was reaped as stale and reclaimed). Not an
		// error -- return cleanly without retrying or failing the
		// workflow out from under its new owner.
		w.Metrics.RecordDBQueryLatency(context.Background(), time.Since(queryStart), "finalize")
		w.logger.DebugContext(context.Background(), "finalize: fence lost, workflow reassigned to another worker", "worker_id", w.id, "workflow_id", wf.ID, "tenant_id", wf.TenantID)
		return
	}
	if err != nil {
		if engine.DebugTiming {
			w.logger.InfoContext(context.Background(), "TIMING: finalize error", "worker_id", w.id, "workflow_id", wf.ID, "elapsed_ms", finalizeElapsed.Milliseconds())
		}
		w.Metrics.RecordDBQueryLatency(context.Background(), time.Since(queryStart), "finalize")
		if isConnectionError(err) {
			w.logger.WarnContext(context.Background(), "DB down finalizing", "worker_id", w.id, "workflow_id", wf.ID, "tenant_id", wf.TenantID)
			w.releaseWorkflow(wf)
			return
		}
		w.logger.ErrorContext(context.Background(), "finalize error", "worker_id", w.id, "workflow_id", wf.ID, "tenant_id", wf.TenantID, "error", err)
		var ce *engine.CleatError
		errorCode := engine.ErrUnknown.String()
		errorOp := ""
		if errors.As(err, &ce) {
			errorCode = ce.Code.String()
			errorOp = ce.Op
		}
		errMsg := err.Error()
		w.recordTerminalFailure(wf, workflowStartTime, errMsg, errorCode, errorOp)
		return
	}
	w.Metrics.RecordDBQueryLatency(context.Background(), time.Since(queryStart), "finalize")

	// Signal the dispatch loop to poll immediately. The parent
	// was woken atomically inside FinalizeWorkflowSegment.
	if finalStatus == "done" || finalStatus == "failed" {
		select {
		case w.parentWakeCh <- struct{}{}:
		default:
		}
	}

	// Post-finalization: logging and non-DB side effects.
	if finalStatus == "done" {
		// Workflow completed. Run any registered defer callbacks in LIFO order.
		if len(deferrals) > 0 {
			w.runDefers(wasmBytes, deferrals)
		}

		duration := time.Since(workflowStartTime)
		w.Metrics.RecordWorkflowDuration(context.Background(), duration, wf.DefName, "done", "")
		w.Metrics.RecordWorkflowCompleted(context.Background(), wf.DefName, "")
		w.logger.InfoContext(context.Background(), "workflow completed", "worker_id", w.id, "workflow_id", wf.ID, "tenant_id", wf.TenantID, "duration", duration, "replay_ms", engineElapsed.Milliseconds(), "finalize_ms", finalizeElapsed.Milliseconds())
		if engine.DebugTiming {
			w.logger.InfoContext(context.Background(), "TIMING: breakdown", "worker_id", w.id, "workflow_id", wf.ID, "total_ms", duration.Milliseconds(), "setup_ms", setupElapsed.Milliseconds(), "replay_ms", engineElapsed.Milliseconds(), "finalize_ms", finalizeElapsed.Milliseconds())
		}
	} else {
		w.logger.InfoContext(context.Background(), "workflow suspended", "worker_id", w.id, "workflow_id", wf.ID, "tenant_id", wf.TenantID, "reason", suspended.Reason, "wake_at", suspended.SuspendUntil)
	}
}

func (w *Worker) heartbeatLoop() {
	defer w.wg.Done()
	w.healthTracker.setInterval("heartbeat", w.heartbeatInterval)
	ticker := time.NewTicker(w.heartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-w.getLoopCtx("heartbeat").Done():
			return
		case <-ticker.C:
			w.healthTracker.recordRun("heartbeat")
			hbStart := time.Now()
			_, err := w.store.BatchHeartbeat(w.ctx, w.id)
			if err != nil {
				w.Metrics.RecordBackgroundLoop(w.ctx, "heartbeat", "error")
				if isConnectionError(err) {
					w.logger.WarnContext(w.ctx, "BatchHeartbeat failed: DB appears down", "worker_id", w.id)
				} else {
					w.logger.ErrorContext(w.ctx, "BatchHeartbeat error", "worker_id", w.id, "error", err)
				}
			} else {
				w.Metrics.RecordBackgroundLoop(w.ctx, "heartbeat", "ok")
			}
			w.Metrics.SetBackgroundLoopDuration(w.ctx, "heartbeat", time.Since(hbStart).Seconds())
		}
	}
}

func (w *Worker) reaperLoop() {
	defer w.wg.Done()
	// Reap stale instances on a configurable interval derived from the
	// heartbeat interval so that the reaper never runs more often than
	// the heartbeat (but at least every 10s).
	interval := max(w.heartbeatInterval, 10*time.Second)
	w.healthTracker.setInterval("reaper", interval)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-w.getLoopCtx("reaper").Done():
			return
		case <-ticker.C:
			w.healthTracker.recordRun("reaper")
			reaperStart := time.Now()
			// A workflow must miss at least two consecutive heartbeats
			// before it is considered stale — otherwise a slow heartbeat
			// could cause false-positive reaping.
			staleTimeout := max(w.heartbeatInterval*2, 10*time.Second)
			reaped, err := w.store.ReapStaleInstances(w.ctx, staleTimeout)
			if err != nil {
				if isConnectionError(err) {
					w.logger.WarnContext(w.ctx, "Reaper: DB appears down", "worker_id", w.id)
				} else {
					w.logger.ErrorContext(w.ctx, "Reaper error", "worker_id", w.id, "error", err)
				}
				w.Metrics.RecordBackgroundLoop(w.ctx, "reaper", "error")
				w.Metrics.SetBackgroundLoopDuration(w.ctx, "reaper", time.Since(reaperStart).Seconds())
				continue
			}
			if reaped > 0 {
				w.logger.InfoContext(w.ctx, "Reaper: reclaimed stale instances", "worker_id", w.id, "count", reaped)
				w.Metrics.SetBackgroundLoopItemsProcessed(w.ctx, "reaper", int64(reaped))
			}
			w.Metrics.RecordBackgroundLoop(w.ctx, "reaper", "ok")
			w.Metrics.SetBackgroundLoopDuration(w.ctx, "reaper", time.Since(reaperStart).Seconds())
		}
	}
}

func (w *Worker) concurrencyKeyReaperLoop() {
	defer w.wg.Done()
	// Reap expired concurrency keys every 60 seconds.
	w.healthTracker.setInterval("concurrency_key_reaper", 60*time.Second)
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-w.getLoopCtx("concurrency_key_reaper").Done():
			return
		case <-ticker.C:
			w.healthTracker.recordRun("concurrency_key_reaper")
			ckStart := time.Now()
			reaped, err := w.store.ReapExpiredConcurrencyKeys(w.ctx)
			if err != nil {
				if isConnectionError(err) {
					w.logger.WarnContext(w.ctx, "Concurrency key reaper: DB appears down", "worker_id", w.id)
				} else {
					w.logger.ErrorContext(w.ctx, "Concurrency key reaper error", "worker_id", w.id, "error", err)
				}
				w.Metrics.RecordBackgroundLoop(w.ctx, "concurrency_key_reaper", "error")
				w.Metrics.SetBackgroundLoopDuration(w.ctx, "concurrency_key_reaper", time.Since(ckStart).Seconds())
				continue
			}
			if reaped > 0 {
				w.logger.InfoContext(w.ctx, "Concurrency key reaper: removed expired keys", "worker_id", w.id, "count", reaped)
				w.Metrics.SetBackgroundLoopItemsProcessed(w.ctx, "concurrency_key_reaper", reaped)
			}
			w.Metrics.RecordBackgroundLoop(w.ctx, "concurrency_key_reaper", "ok")
			w.Metrics.SetBackgroundLoopDuration(w.ctx, "concurrency_key_reaper", time.Since(ckStart).Seconds())
		}
	}
}

func (w *Worker) scheduleLoop() {
	defer w.wg.Done()
	w.healthTracker.setInterval("schedule", w.scheduleInterval)
	ticker := time.NewTicker(w.scheduleInterval)
	defer ticker.Stop()

	for {
		select {
		case <-w.getLoopCtx("schedule").Done():
			return
		case <-ticker.C:
			w.healthTracker.recordRun("schedule")
			schStart := time.Now()
			w.scheduleMu.Lock()
			schedules, err := w.dueSchedules()
			if err != nil {
				w.scheduleMu.Unlock()
				if isConnectionError(err) {
					w.logger.WarnContext(w.ctx, "Scheduler: DB appears down", "worker_id", w.id)
				} else {
					w.logger.ErrorContext(w.ctx, "Scheduler error", "worker_id", w.id, "error", err)
				}
				w.Metrics.RecordBackgroundLoop(w.ctx, "schedule", "error")
				w.Metrics.SetBackgroundLoopDuration(w.ctx, "schedule", time.Since(schStart).Seconds())
				continue
			}

			for _, sch := range schedules {
				// Build input with entry point if specified.
				input := sch.Input
				if len(input) == 0 {
					input = json.RawMessage("{}")
				}
				if sch.EntryPoint != "" {
					var m map[string]any
					json.Unmarshal(input, &m)
					if m == nil {
						m = make(map[string]any)
					}
					m["__entry_point"] = sch.EntryPoint
					input, _ = json.Marshal(m)
				}

				// Everything below this line runs against the schedule's OWN
				// tenant, not the worker's. That is the whole bargain of the
				// cross-tenant read above: one widened query finds the due set,
				// and the firing itself -- the definition lookup, the overlap
				// check, the run, the compare-and-swap -- is scoped again
				// immediately. Nothing downstream of here sees another tenant.
				tenantID := sch.TenantID
				if tenantID == "" {
					// Rows written before tenant_id existed, and every fixture
					// that does not set one.
					tenantID = engine.DefaultTenantUUID
				}
				schStore, terr := w.storeForTenant(tenantID)
				if terr != nil {
					// Refuse rather than fall back to w.store: firing this
					// schedule through the wrong tenant's store would write one
					// tenant's run under another's isolation, which is worse
					// than a schedule that is late.
					w.logger.ErrorContext(w.ctx, "Scheduler: no store for the schedule's tenant; not firing",
						"worker_id", w.id, "schedule", sch.Name, "tenant_id", tenantID, "error", terr)
					continue
				}

				// Find latest version.
				versions, verr := schStore.ListVersions(w.ctx, sch.DefName)
				if verr != nil || len(versions) == 0 {
					w.logger.WarnContext(w.ctx, "Scheduler: definition not found", "worker_id", w.id, "schedule", sch.Name, "def_name", sch.DefName)
					continue
				}

				// The run belongs to the tenant that owns the schedule, which
				// is not necessarily the constant that used to be hardcoded
				// here (engine.DefaultTenantUUID).
				//
				// Today those are always the same value, and it is worth being
				// precise about WHY, because the reason is not "schedules are
				// single-tenant". The worker opens exactly one store, scoped to
				// the default tenant (cmd/cleat-worker/main.go), and every
				// dialect scopes GetDueSchedules to the store's tenant -- RLS on
				// Postgres and SQL Server, an explicit predicate on MySQL. So
				// the loop only ever SEES default-tenant schedules, and the
				// constant was right by accident rather than by construction.
				//
				// Measured 2026-08-08 against SQL Server: a schedule created
				// through POST /api/schedules by a non-default tenant is
				// returned by that tenant's own store and is NOT returned to
				// this loop. It is listed in the dashboard, shows as enabled
				// with a next_run_at, and never fires. That is a property of
				// the worker's single-tenant execution scope -- dispatch has it
				// too -- and not something this line can fix. It is recorded in
				// TestScheduleLoop_OnlySeesItsOwnTenantsSchedules so the next
				// person does not have to rediscover it.
				//
				// tenantID and schStore were resolved above, before the first
				// store call, so no read in this iteration can precede the
				// re-scoping.
				// In the SCHEDULE's zone, not the worker's. This used to be
				// engine.NextCronTime(sch.CronExpression, time.Now()), and
				// time.Now() carries the local zone of whichever machine in the
				// fleet happened to claim the row -- so "0 7 * * *" fired at
				// 07:00 in a zone nobody chose, and two workers in different
				// regions computed different next-run times for the same
				// schedule.
				loc, fellBack := engine.LoadScheduleLocation(sch.Timezone)
				if fellBack && sch.Timezone != "" {
					// Distinct from "this schedule is UTC": the zone was named
					// and this process could not load it, which on a container
					// without tzdata would otherwise silently re-time every
					// schedule to UTC. cmd/cleat-worker embeds tzdata to make
					// this unreachable, so if it fires something is wrong.
					w.logger.WarnContext(w.ctx, "Scheduler: unknown timezone, falling back to UTC",
						"worker_id", w.id, "schedule", sch.Name, "timezone", sch.Timezone)
				}

				// The instant this firing IS FOR, which is not the same as the
				// instant we noticed it. Everything below keys off the former.
				scheduled := sch.NextRunAt

				// OVERLAP. Under "skip", an instant that arrives while the run
				// this schedule started last is still going is dropped rather
				// than stacked. The schedule still advances -- otherwise it
				// would stay due and re-check every tick, which is the same
				// answer at higher cost.
				//
				// "allow" is the default only because it is what the scheduler
				// has always done and changing it would silently alter existing
				// deployments. It is the wrong default for most real schedules:
				// a job that occasionally overruns its interval quietly becomes
				// an unbounded fan-out.
				if engine.OverlapPolicyOrDefault(sch.OverlapPolicy) == engine.OverlapSkip && sch.LastRunID != "" {
					prev, gerr := schStore.GetWorkflowByID(w.ctx, sch.LastRunID)
					switch {
					case gerr != nil:
						// Cannot tell. Fire rather than skip: the promise is
						// at-least-once, so an unanswerable overlap check must
						// fail towards delivery.
						w.logger.WarnContext(w.ctx, "Scheduler: overlap check failed, firing anyway",
							"worker_id", w.id, "schedule", sch.Name, "last_run_id", sch.LastRunID, "error", gerr)
					case prev != nil && (prev.Status == "running" || prev.Status == "ready"):
						nextRun, _ := scheduleAdvance(sch.CronExpression, scheduled, loc, time.Now(), engine.CatchUpLimitOrDefault(sch.CatchUpLimit))
						if _, cerr := schStore.ClaimDueSchedule(w.ctx, sch.Name, scheduled, nextRun, ""); cerr != nil {
							w.logger.ErrorContext(w.ctx, "Scheduler: failed to advance a skipped-for-overlap schedule", "worker_id", w.id, "schedule", sch.Name, "error", cerr)
						}
						w.logger.InfoContext(w.ctx, "Scheduler: firing skipped, previous run still in flight",
							"worker_id", w.id, "schedule", sch.Name, "last_run_id", sch.LastRunID,
							"status", prev.Status, "scheduled_at", scheduled.Format(time.RFC3339))
						w.Metrics.SetBackgroundLoopItemsProcessed(w.ctx, "schedule_overlap_skipped", 1)
						continue
					}
				}

				// MISFIRE. "skip" resumes at the next future instant and
				// delivers none of the backlog, for schedules where a late
				// firing is worse than no firing.
				if engine.MisfirePolicyOrDefault(sch.MisfirePolicy) == engine.MisfireSkip {
					future := engine.NextCronTimeIn(sch.CronExpression, time.Now(), loc)
					if scheduled.Before(time.Now().Add(-time.Minute)) {
						w.logger.InfoContext(w.ctx, "Scheduler: misfire policy is skip; not delivering the backlog",
							"worker_id", w.id, "schedule", sch.Name,
							"was_due_at", scheduled.Format(time.RFC3339), "resuming_at", future.Format(time.RFC3339))
						if _, cerr := schStore.ClaimDueSchedule(w.ctx, sch.Name, scheduled, future, ""); cerr != nil {
							w.logger.ErrorContext(w.ctx, "Scheduler: failed to advance a misfired schedule", "worker_id", w.id, "schedule", sch.Name, "error", cerr)
						}
						w.Metrics.SetBackgroundLoopItemsProcessed(w.ctx, "schedule_misfire_skipped", 1)
						continue
					}
				}

				nextRun, skipped := scheduleAdvance(sch.CronExpression, scheduled, loc, time.Now(), engine.CatchUpLimitOrDefault(sch.CatchUpLimit))
				if skipped > 0 {
					// A silent skip is the failure mode at-least-once exists to
					// rule out, so it is logged with a count rather than just
					// happening.
					w.logger.WarnContext(w.ctx, "Scheduler: schedule was too far behind to catch up; instants were skipped",
						"worker_id", w.id, "schedule", sch.Name, "skipped_firings", skipped,
						"was_due_at", scheduled.Format(time.RFC3339), "resuming_at", nextRun.Format(time.RFC3339))
					w.Metrics.SetBackgroundLoopItemsProcessed(w.ctx, "schedule_skipped_firings", int64(skipped))
				}

				// START FIRST, ADVANCE SECOND. The order is the guarantee.
				//
				// Advancing first would give at-most-once: a crash between the
				// two loses the firing entirely, with nothing recording that it
				// was lost. Starting first means a crash before the advance
				// leaves the schedule still due, so the next poll retries it --
				// and the idempotency key below makes that retry produce the
				// same run rather than a second one.
				//
				// The key is (schedule, tenant, SCHEDULED INSTANT), not
				// time.Now(): two workers racing the same instant, or one
				// worker retrying after a crash, must derive the same key.
				// StartNewRun hashes it, looks it up scoped by tenant, and
				// returns the existing run with alreadyExisted=true on a hit
				// (engine/store_lifecycle.go). Duplicate DELIVERY still
				// happens at this layer; it stops at admission.
				//
				// Bound worth knowing: the idempotency key TTL is 30 days
				// (720h, identical in all three stores). A catch-up firing for
				// an instant staler than that would not dedup -- irrelevant up
				// to weekly, real for a monthly schedule after a long outage.
				idemKey := fmt.Sprintf("cron:%s:%s:%d", tenantID, sch.Name, scheduled.UTC().Unix())
				runID, alreadyExisted, serr := schStore.StartNewRun(w.ctx, "", sch.DefName, versions[0], input, idemKey, tenantID, 0)
				if serr != nil {
					// Deliberately NOT advancing. The schedule stays due and
					// the next tick retries it, which is what at-least-once
					// means.
					w.logger.ErrorContext(w.ctx, "Scheduler: failed to start workflow; leaving the schedule due for retry", "worker_id", w.id, "schedule", sch.Name, "error", serr)
					continue
				}
				if alreadyExisted {
					// Suppression must be visible. If this is silent we lose
					// the ability to tell "dedup is working" from "dedup
					// silently stopped engaging", and the latter looks
					// identical to a healthy scheduler right up until it
					// double-bills someone.
					w.logger.InfoContext(w.ctx, "Scheduler: duplicate firing suppressed at admission", "worker_id", w.id, "schedule", sch.Name, "workflow_id", runID, "scheduled_at", scheduled.Format(time.RFC3339))
					w.Metrics.SetBackgroundLoopItemsProcessed(w.ctx, "schedule_duplicates_suppressed", 1)
				}

				// Compare-and-swap the schedule forward. Whoever wins owns this
				// instant. GetDueSchedules' row locks are released when its own
				// transaction ends -- before any of the above ran -- so two
				// workers polling milliseconds apart both saw this row as due.
				claimed, cerr := schStore.ClaimDueSchedule(w.ctx, sch.Name, scheduled, nextRun, runID)
				if cerr != nil {
					w.logger.ErrorContext(w.ctx, "Scheduler: failed to advance next run", "worker_id", w.id, "schedule", sch.Name, "error", cerr)
					continue
				}
				if !claimed {
					w.logger.DebugContext(w.ctx, "Scheduler: another worker advanced this schedule first", "worker_id", w.id, "schedule", sch.Name, "scheduled_at", scheduled.Format(time.RFC3339))
					continue
				}

				w.logger.InfoContext(w.ctx, "Scheduler: fired schedule", "worker_id", w.id, "schedule", sch.Name, "workflow_id", runID, "scheduled_at", scheduled.Format(time.RFC3339), "next_at", nextRun.Format(time.RFC3339), "timezone", loc.String())
			}
			w.scheduleMu.Unlock()
			w.Metrics.RecordBackgroundLoop(w.ctx, "schedule", "ok")
			w.Metrics.SetBackgroundLoopDuration(w.ctx, "schedule", time.Since(schStart).Seconds())
			w.Metrics.SetBackgroundLoopItemsProcessed(w.ctx, "schedule", int64(len(schedules)))
		}
	}
}

func (w *Worker) compactionLoop() {
	defer w.wg.Done()
	w.healthTracker.setInterval("compaction", w.compactionInterval)
	ticker := time.NewTicker(w.compactionInterval)
	defer ticker.Stop()

	for {
		select {
		case <-w.getLoopCtx("compaction").Done():
			return
		case <-ticker.C:
			w.healthTracker.recordRun("compaction")
			compStart := time.Now()
			candidates, err := w.store.GetCompactionCandidates(w.ctx, w.compactionThreshold, 10)
			if err != nil {
				w.logger.ErrorContext(w.ctx, "compaction: error finding candidates", "worker_id", w.id, "error", err)
				w.Metrics.RecordBackgroundLoop(w.ctx, "compaction", "error")
				w.Metrics.SetBackgroundLoopDuration(w.ctx, "compaction", time.Since(compStart).Seconds())
				continue
			}
			for _, wfID := range candidates {
				if err := engine.CompactWorkflowHistory(w.ctx, w.store, wfID, w.compactionThreshold, w.Metrics); err != nil {
					w.logger.ErrorContext(w.ctx, "compaction error", "worker_id", w.id, "workflow_id", wfID, "error", err)
				}
			}
			w.Metrics.RecordBackgroundLoop(w.ctx, "compaction", "ok")
			w.Metrics.SetBackgroundLoopDuration(w.ctx, "compaction", time.Since(compStart).Seconds())
			w.Metrics.SetBackgroundLoopItemsProcessed(w.ctx, "compaction", int64(len(candidates)))
		}
	}
}

func (w *Worker) memoryReloadLoop() {
	defer w.wg.Done()
	w.healthTracker.setInterval("memory_reload", 5*time.Minute)
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-w.getLoopCtx("memory_reload").Done():
			return
		case <-ticker.C:
			w.healthTracker.recordRun("memory_reload")
			mrStart := time.Now()
			if err := w.memoryController.LoadEstimates(w.ctx); err != nil {
				w.logger.ErrorContext(w.ctx, "memory reload error", "worker_id", w.id, "error", err)
				w.Metrics.RecordBackgroundLoop(w.ctx, "memory_reload", "error")
			} else {
				w.Metrics.RecordBackgroundLoop(w.ctx, "memory_reload", "ok")
			}
			w.Metrics.SetBackgroundLoopDuration(w.ctx, "memory_reload", time.Since(mrStart).Seconds())
		}
	}
}

// retentionLoop runs the two independent retention sweeps on a shared
// 24-hour ticker: event-history retention (retentionDays, on by default --
// see --retention-days) and completed-workflow retention
// (completedWorkflowRetentionDays, off by default -- see
// --completed-workflow-retention-days). Either can be disabled independently
// by passing 0; the loop itself only exits (does nothing, ever) if both are
// disabled, since there is nothing left for it to do.
//
// Why the default differs between the two: --retention-days deletes
// event_history rows, the step-by-step replay log of a workflow that has
// already reached a terminal state -- the workflow's outcome (status,
// result, error, def_name) survives untouched in workflow_instances.
// --completed-workflow-retention-days deletes the workflow_instances row
// itself: the record that the workflow ever ran, what it returned, and why
// it failed, gone from ListWorkflows and the admin dashboard permanently.
// That is a materially more destructive default to ship silently-on, so it
// defaults to 0 (disabled) -- an operator has to opt in, having decided how
// long their own compliance/audit requirements need a workflow's outcome
// retrievable. Finding S2 is real (the table is unbounded by anything but
// lifetime workflow count) but "unbounded growth" and "silently deleting
// user-visible records by default" are different classes of problem, and
// only one of them is safe to default on.
func (w *Worker) retentionLoop(retentionDays, completedWorkflowRetentionDays int) {
	defer w.wg.Done()
	if retentionDays <= 0 && completedWorkflowRetentionDays <= 0 {
		return
	}
	interval := 24 * time.Hour
	w.healthTracker.setInterval("retention", interval)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-w.getLoopCtx("retention").Done():
			return
		case <-ticker.C:
			w.healthTracker.recordRun("retention")
			w.runRetentionSweep(retentionDays, completedWorkflowRetentionDays)
		}
	}
}

// runRetentionSweep runs one iteration of both retention sweeps. Split out
// of retentionLoop so it is callable directly from a test without waiting on
// the loop's 24-hour ticker.
func (w *Worker) runRetentionSweep(retentionDays, completedWorkflowRetentionDays int) {
	if retentionDays > 0 {
		cutoff := time.Now().Add(-time.Duration(retentionDays) * 24 * time.Hour)
		deleted, err := w.store.DeleteExpiredEvents(w.ctx, cutoff)
		if err != nil {
			w.logger.ErrorContext(w.ctx, "retention: error deleting expired events", "worker_id", w.id, "error", err)
		} else if deleted > 0 {
			w.Metrics.RecordEventsDeleted(w.ctx, deleted)
			w.logger.InfoContext(w.ctx, "retention: deleted expired event rows", "worker_id", w.id, "count", deleted)
		}
	}
	if completedWorkflowRetentionDays > 0 {
		cutoff := time.Now().Add(-time.Duration(completedWorkflowRetentionDays) * 24 * time.Hour)
		deleted, err := w.store.DeleteCompletedWorkflows(w.ctx, cutoff)
		if err != nil {
			w.logger.ErrorContext(w.ctx, "retention: error deleting completed workflows", "worker_id", w.id, "error", err)
		} else if deleted > 0 {
			w.Metrics.RecordWorkflowsPurged(w.ctx, deleted)
			w.logger.InfoContext(w.ctx, "retention: deleted completed workflow rows", "worker_id", w.id, "count", deleted)
		}
	}
	w.Metrics.SetRetentionLastRunTimestamp(w.ctx, time.Now().Unix())
}

func (w *Worker) memoryCleanupLoop(maxSamples int) {
	defer w.wg.Done()
	w.healthTracker.setInterval("memory_cleanup", 10*time.Minute)
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-w.getLoopCtx("memory_cleanup").Done():
			return
		case <-ticker.C:
			w.healthTracker.recordRun("memory_cleanup")
			mcStart := time.Now()
			deleted, err := w.store.CleanupMemorySamples(w.ctx, maxSamples)
			if err != nil {
				w.logger.ErrorContext(w.ctx, "memory cleanup error", "worker_id", w.id, "error", err)
				w.Metrics.RecordBackgroundLoop(w.ctx, "memory_cleanup", "error")
			} else if deleted > 0 {
				w.logger.InfoContext(w.ctx, "memory cleanup: removed old samples", "worker_id", w.id, "count", deleted)
			}
			if err == nil {
				w.Metrics.RecordBackgroundLoop(w.ctx, "memory_cleanup", "ok")
				if deleted > 0 {
					w.Metrics.SetBackgroundLoopItemsProcessed(w.ctx, "memory_cleanup", deleted)
				}
			}
			w.Metrics.SetBackgroundLoopDuration(w.ctx, "memory_cleanup", time.Since(mcStart).Seconds())
		}
	}
}

func (w *Worker) updateDispatchLoop(ctx context.Context) {
	defer w.wg.Done()
	w.healthTracker.setInterval("update_dispatch", 5*time.Second)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.healthTracker.recordRun("update_dispatch")
			w.dispatchPendingUpdates()
		}
	}
}

func (w *Worker) dispatchPendingUpdates() {
	ctx := context.Background()

	// Iterate over all claimed workflows.
	w.inflight.Range(func(key, value any) bool {
		wfID := key.(string)

		// Get pending update requests for this workflow.
		updates, err := w.store.GetPendingUpdateRequests(ctx, wfID)
		if err != nil {
			w.logger.ErrorContext(w.ctx, "error fetching pending updates", "worker_id", w.id, "workflow_id", wfID, "error", err)
			return true
		}
		if len(updates) == 0 {
			return true
		}

		// Find the running engine for this workflow.
		envVal, ok := w.execEngines.Load(wfID)
		if !ok {
			// Engine not found (maybe not running on this worker right now).
			// Leave the updates pending for the next claim cycle.
			return true
		}
		env, ok := envVal.(*engine.Engine)
		if !ok {
			return true
		}

		for _, upd := range updates {
			// Dispatch the update via the engine.
			result, dErr := env.DispatchUpdate(ctx, upd.UpdateName, upd.Payload)

			var resultStr, errStr string
			if dErr != nil {
				errStr = dErr.Error()
				w.logger.ErrorContext(w.ctx, "update failed", "worker_id", w.id, "workflow_id", wfID, "update_name", upd.UpdateName, "error", dErr)
			} else {
				resultStr = result
				w.logger.InfoContext(w.ctx, "update completed", "worker_id", w.id, "workflow_id", wfID, "update_name", upd.UpdateName)
			}

			// Store the result in the workflow_update_requests table.
			if cErr := w.store.CompleteUpdateRequest(ctx, wfID, upd.UpdateName, resultStr, errStr); cErr != nil {
				w.logger.ErrorContext(w.ctx, "error completing update", "worker_id", w.id, "workflow_id", wfID, "update_name", upd.UpdateName, "error", cErr)
			}

			// If the update request has an associated promise, resolve or reject it.
			if upd.PromiseID != "" {
				if dErr != nil {
					if rErr := w.store.RejectPromise(ctx, wfID, upd.PromiseID, errStr); rErr != nil {
						w.logger.ErrorContext(w.ctx, "error rejecting promise", "worker_id", w.id, "workflow_id", wfID, "promise_id", upd.PromiseID, "error", rErr)
					}
				} else {
					if rErr := w.store.ResolvePromise(ctx, wfID, upd.PromiseID, resultStr); rErr != nil {
						w.logger.ErrorContext(w.ctx, "error resolving promise", "worker_id", w.id, "workflow_id", wfID, "promise_id", upd.PromiseID, "error", rErr)
					}
				}
			}
		}
		return true
	})
}

func (w *Worker) loadWASM(defName string, defVersion int) ([]byte, error) {
	key := fmt.Sprintf("%s:%d", defName, defVersion)

	// Check in-memory cache first.
	if cached, ok := w.wasmCache.get(key); ok {
		dbLen, err := w.store.GetWASMLength(w.ctx, defName, defVersion)
		if err == nil {
			if dbLen == int64(len(cached)) {
				w.Metrics.RecordWasmCacheHit(w.ctx)
				return cached, nil
			}
			w.logger.InfoContext(w.ctx, "WASM cache stale, reloading", "worker_id", w.id, "key", key)
		} else {
			w.Metrics.RecordWasmCacheHit(w.ctx)
			return cached, nil
		}
		w.wasmCache.remove(key)
	}

	// Check disk cache before going to the database.
	if w.wasmDiskCache != nil {
		if cached := w.wasmDiskCache.LookupDef(defName, defVersion); cached != nil {
			w.Metrics.RecordWasmCacheMiss(w.ctx)
			w.wasmCache.put(key, cached)
			return cached, nil
		}
	}

	w.Metrics.RecordWasmCacheMiss(w.ctx)

	wasmBytes, err := w.store.LoadWASM(w.ctx, defName, defVersion)
	if err != nil {
		return nil, err
	}

	// Store to disk cache for future restarts.
	if w.wasmDiskCache != nil {
		w.wasmDiskCache.StoreDef(defName, defVersion, wasmBytes)
	}

	w.wasmCache.put(key, wasmBytes)
	return wasmBytes, nil
}

func (w *Worker) waitForDB() {
	backoff := 500 * time.Millisecond
	for i := 0; i < 20; i++ {
		select {
		case <-w.ctx.Done():
			return
		default:
		}
		if w.ctx.Err() != nil {
			return
		}

		if _, err := w.store.ClaimWorkflow(w.ctx, ""); err == nil || !isConnectionError(err) {
			// DB is back (or claim returned no work, which means DB is reachable).
			w.logger.InfoContext(w.ctx, "DB connection re-established", "worker_id", w.id)
			return
		}

		w.logger.WarnContext(w.ctx, "DB reconnect attempt failed", "worker_id", w.id, "attempt", i+1, "delay", backoff)
		time.Sleep(backoff)
		backoff = time.Duration(math.Min(float64(backoff*2), 10e9))
	}
}

// recordTerminalFailure writes the terminal failure for wf and records the
// failure metrics -- but only if the fenced write actually applied.
//
// Every terminal store write is fenced on (assigned_to, generation). A lost
// fence is not an error: it means this worker stalled long enough to be
// reaped, another worker legitimately reclaimed the workflow, and the store
// correctly refused this worker's write. What was missing was any caller
// noticing. Two things went wrong as a result:
//
//   - the failure was invisible. Nothing logged it, so a worker losing every
//     race looked identical to one doing its job.
//   - RecordWorkflowFailed was emitted *before* the store call, so a workflow
//     the new owner goes on to complete successfully was still counted as
//     failed. The failure counter disagreed with the database.
//
// Metrics are therefore recorded after the write, conditional on it applying.
// The precedent is the two call sites that already handled ErrFenceLost (the
// ContinueAsNew and FinalizeWorkflowSegment paths): debug-log and return,
// having done nothing. See IMPROVEMENT-PLAN.md 1.2.
func (w *Worker) writeTerminalFailure(wf *engine.WorkflowInstance, errMsg, errorCode, errorOp string) (applied, deadLettered bool) {
	st := w.storeFor(wf)
	ctx := context.Background()

	deadLettered = strings.Contains(errMsg, "retries exhausted")
	var err error
	if deadLettered {
		err = st.MoveToDeadLetterQueue(ctx, wf.ID, w.id, wf.Generation, errMsg, errorCode, errorOp)
	} else {
		err = st.FailWorkflow(ctx, wf.ID, w.id, wf.Generation, errMsg, errorCode, errorOp, nil)
	}

	if errors.Is(err, engine.ErrFenceLost) {
		w.logger.DebugContext(ctx, "terminal failure: fence lost, workflow reassigned to another worker",
			"worker_id", w.id, "workflow_id", wf.ID, "tenant_id", wf.TenantID)
		return false, deadLettered
	}
	if err != nil {
		w.logger.ErrorContext(ctx, "terminal failure write failed, workflow stays claimed until its lease expires",
			"worker_id", w.id, "workflow_id", wf.ID, "tenant_id", wf.TenantID, "error", err)
		return false, deadLettered
	}
	return true, deadLettered
}

// recordTerminalFailure is writeTerminalFailure plus the failure metrics the
// dispatch paths record, emitted only when the write applied.
func (w *Worker) recordTerminalFailure(wf *engine.WorkflowInstance, startedAt time.Time, errMsg, errorCode, errorOp string) {
	applied, deadLettered := w.writeTerminalFailure(wf, errMsg, errorCode, errorOp)
	if !applied {
		return
	}
	ctx := context.Background()
	w.Metrics.RecordWorkflowFailed(ctx, wf.DefName, "", "")
	w.Metrics.RecordWorkflowDuration(ctx, time.Since(startedAt), wf.DefName, "failed", "")
	if deadLettered {
		w.Metrics.RecordWorkflowsDeadLettered(ctx)
	}
}

// releaseWorkflow returns wf to the ready pool, treating a lost fence as the
// no-op it is: another worker owns the workflow, so there is nothing to
// release. See recordTerminalFailure for why this is not an error.
func (w *Worker) releaseWorkflow(wf *engine.WorkflowInstance) {
	st := w.storeFor(wf)
	ctx := context.Background()
	err := st.ReleaseWorkflow(ctx, wf.ID, w.id, wf.Generation, wf.NextWakeAt)
	if errors.Is(err, engine.ErrFenceLost) {
		w.logger.DebugContext(ctx, "release: fence lost, workflow reassigned to another worker",
			"worker_id", w.id, "workflow_id", wf.ID, "tenant_id", wf.TenantID)
		return
	}
	if err != nil {
		w.logger.WarnContext(ctx, "release failed, workflow stays claimed until its lease expires",
			"worker_id", w.id, "workflow_id", wf.ID, "tenant_id", wf.TenantID, "error", err)
	}
}

func (w *Worker) releaseOrFail(wf *engine.WorkflowInstance, errMsg string) {
	if errMsg == "" {
		w.releaseWorkflow(wf)
		return
	}
	// Deliberately not recordTerminalFailure: this path never recorded the
	// failed/duration pair, and it has no start time to report a duration
	// from. Only the dead-letter counter, as before -- now conditional on the
	// write applying.
	if applied, deadLettered := w.writeTerminalFailure(wf, errMsg, "", ""); applied && deadLettered {
		w.Metrics.RecordWorkflowsDeadLettered(context.Background())
	}
}

// dbServiceCaller implements engine.ServiceCaller for the worker.

// watchdogLoop periodically checks the health of all background loops.
// If a loop has not run within its expected interval, it is considered
// stale and gets restarted.
func (w *Worker) watchdogLoop() {
	defer w.wg.Done()
	w.healthTracker.setInterval("watchdog", w.healthCheckInterval)
	ticker := time.NewTicker(w.healthCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-w.ctx.Done():
			return
		case <-ticker.C:
			w.healthTracker.recordRun("watchdog")

			stale := w.healthTracker.staleLoops()

			// Poison-pill: if the vast majority of loops are stale at once,
			// the worker is likely hung (GC storm, OS stall, etc.). Exit
			// cleanly and let external infrastructure restart the process.
			total := w.healthTracker.registeredCount()
			if len(stale) > 0 && total >= 3 && len(stale) >= (total*4/5) {
				w.logger.ErrorContext(w.ctx, "CRITICAL: loops stale, exiting for external restart", "worker_id", w.id, "stale", len(stale), "total", total)
				w.cancel()
				return
			}

			for _, name := range stale {
				if name == "watchdog" {
					// The watchdog cannot restart itself. A stale watchdog
					// entry indicates healthTracker state corruption.
					w.logger.WarnContext(w.ctx, "watchdog self-reported as stale", "worker_id", w.id)
					continue
				}
				w.logger.WarnContext(w.ctx, "watchdog: loop is stale, restarting", "worker_id", w.id, "loop", name)
				w.restartLoop(name)
			}

			// Report health metrics.
			lastRun, panicked, restarts := w.healthTracker.snapshot()
			for name, t := range lastRun {
				w.Metrics.SetBackgroundLoopLastRun(w.ctx, name, float64(t.Unix()))
			}
			for name, count := range restarts {
				w.Metrics.RecordBackgroundLoopRestart(w.ctx, name, int64(count))
			}
			for name := range panicked {
				_ = name // available for future alerting
			}
		}
	}
}

// restartLoop re-launches a background loop by name using the loopFuncs registry.
// restartLoop cancels the running loop (if any), waits for it to exit, then
// launches a replacement goroutine using a fresh per-loop context. This
// prevents goroutine leaks and double execution when the watchdog detects a
// stale loop.
func (w *Worker) restartLoop(name string) {
	w.loopMu.Lock()
	fn, ok := w.loopFuncs[name]
	var prev *loopContext
	if p, pok := w.loopCtxMap[name]; pok {
		prev = p
	}
	w.loopMu.Unlock()

	if !ok {
		w.logger.WarnContext(w.ctx, "watchdog: no restart function registered", "worker_id", w.id, "loop", name)
		return
	}

	// Re-check staleness atomically to avoid killing a loop that recovered
	// between the staleLoops() snapshot and this call (TOCTOU fix).
	if !w.healthTracker.isStale(name) {
		w.logger.InfoContext(w.ctx, "watchdog: loop recovered before restart", "worker_id", w.id, "loop", name)
		return
	}
	w.healthTracker.recordRestart(name)

	// Cancel the old loop context (if one exists) and wait for the old goroutine
	// to acknowledge cancellation via its done channel. Do this outside the lock
	// because the wait can take up to 5 seconds.
	if prev != nil {
		prev.cancel()
		select {
		case <-prev.done:
			// old goroutine exited cleanly
		case <-time.After(5 * time.Second):
			w.logger.WarnContext(w.ctx, "WATCHDOG: loop did not exit within 5s of cancellation", "worker_id", w.id, "loop", name)
		}
	}

	// Create a fresh per-loop context and done channel.
	ctx, cancel := context.WithCancel(w.ctx)
	done := make(chan struct{})
	w.loopMu.Lock()
	w.loopCtxMap[name] = &loopContext{ctx: ctx, cancel: cancel, done: done}
	w.loopMu.Unlock()

	w.wg.Add(1)
	go func() {
		defer close(done)
		defer cancel()
		w.withPanicRecovery(name, fn)()
	}()
	w.logger.InfoContext(w.ctx, "watchdog: restarted loop", "worker_id", w.id, "loop", name)
}

// The catch-up bound is now per-schedule (workflow_schedules.catch_up_limit,
// engine.DefaultCatchUpLimit when unset), which is what the package-level
// maxCatchUpFirings constant used to be. See engine.DefaultCatchUpLimit for
// why the default is what it is.

// scheduleAdvance computes the next firing instant after `scheduled`, and
// reports how many owed firings were DROPPED to stop the schedule falling
// further behind than maxCatchUpFirings.
//
// Advancing from `scheduled` rather than from `now` is the whole point: it is
// what lets a firing missed during an outage be delivered rather than silently
// forgotten. NextCronTimeIn(expr, now, loc) -- what this used to do -- jumps
// straight to the next future instant and loses every instant in between with
// nothing recording that it did.
//
// THE NORMAL BEHIND-CASE DROPS NOTHING. A schedule that is behind advances by
// exactly ONE interval, and the poll loop delivers the backlog one instant per
// tick until it catches up. That is what at-least-once means here, and it is
// why the second return value is 0 in every case except the bounded one.
//
// Only when the backlog exceeds maxCatchUpFirings does it give up, jump to the
// next future instant, and report a floor on how many it abandoned. The count
// is a floor rather than the exact number because establishing the exact number
// means walking the entire backlog, which for a per-minute schedule down for a
// week is 10,080 steps to produce a figure only used for a log line.
func scheduleAdvance(expr string, scheduled time.Time, loc *time.Location, now time.Time, catchUpLimit int) (next time.Time, droppedAtLeast int) {
	next = engine.NextCronTimeIn(expr, scheduled, loc)
	if !next.Before(now) {
		// Caught up: the next owed instant has not happened yet.
		return next, 0
	}

	// Behind. Find out by how much, but stop counting once past the bound --
	// the only thing the exact number beyond it would change is a log line.
	count := 0
	probe := next
	for count <= catchUpLimit && probe.Before(now) {
		advanced := engine.NextCronTimeIn(expr, probe, loc)
		if !advanced.After(probe) {
			// Defensive: an expression whose "next" does not advance would
			// spin here forever. NextCronTimeIn's daily fallback always
			// advances, so this is unreachable rather than expected -- but an
			// infinite loop inside a background daemon is not a failure mode
			// worth leaving to a proof.
			break
		}
		probe = advanced
		count++
	}

	if count > catchUpLimit {
		// Too far behind to walk out of. Resume in the future so the schedule
		// starts firing on time again instead of staying permanently behind
		// and re-entering this path on every tick.
		return engine.NextCronTimeIn(expr, now, loc), count
	}

	// Within the bound: step one interval and drop nothing.
	return next, 0
}

// storeForTenant returns the store execution should write through for a
// workflow belonging to tenantID.
//
// With no factory configured there is nothing to route with, so the caller
// gets the worker's own store and executeWorkflow's scope check refuses
// anything outside it. That is the pre-routing behaviour, kept deliberately
// rather than silently degrading to "write it under whatever tenant we have".
func (w *Worker) storeForTenant(tenantID string) (engine.WorkflowStore, error) {
	if w.storeFactory == nil || tenantID == "" || tenantID == w.storeTenantID {
		return w.store, nil
	}
	if cached, ok := w.tenantStores.Load(tenantID); ok {
		return cached.(engine.WorkflowStore), nil
	}
	st, _, err := w.storeFactory.OpenStore(w.ctx, tenantID, w.taskQueues...)
	if err != nil {
		return nil, err
	}
	// LoadOrStore rather than Store: two workflows for a new tenant can arrive
	// together, and both should end up using the same store rather than one
	// silently replacing the other's.
	actual, _ := w.tenantStores.LoadOrStore(tenantID, st)
	return actual.(engine.WorkflowStore), nil
}

// storeFor is storeForTenant for the paths that cannot report an error: the
// terminal-failure and release helpers, which run when something has already
// gone wrong.
//
// executeWorkflow resolves the same tenant before any of them can run and
// fails the workflow if it cannot, so by the time one of these is reached the
// lookup is a cache hit. Falling back to w.store if that ever stops being true
// is the lesser evil -- a workflow stuck in `running` forever with no record
// of why is worse than a failure row under the wrong tenant, and the log line
// says which happened.
func (w *Worker) storeFor(wf *engine.WorkflowInstance) engine.WorkflowStore {
	st, err := w.storeForTenant(wf.TenantID)
	if err != nil {
		w.logger.ErrorContext(context.Background(), "no tenant store on a failure path; falling back to the worker store",
			"worker_id", w.id, "workflow_id", wf.ID, "tenant_id", wf.TenantID, "error", err)
		return w.store
	}
	return st
}

// reportCrossTenantCapability logs, once at startup, which mode this worker is
// actually in.
//
// Both cross-tenant paths degrade rather than fail: a missing grant narrows the
// worker instead of stopping it, which is the right default and is what the
// per-loop warnings say when they fire. What that leaves is an operator who set
// --claim-across-tenants, saw nothing, and cannot tell "it is working" from "it
// will tell me on the first tick, in a line that has already scrolled past".
//
// So this states the outcome in both directions, before either loop runs.
//
// It is a report and not a gate. Refusing to start would contradict the
// degradation the rest of this feature is built on, and would turn a revoked
// GRANT into an outage for the worker's own tenant, which was never affected.
// The one thing it does that no runtime path can is catch a lost BYPASSRLS on
// PostgreSQL: that failure does not raise, it just returns fewer rows, so
// without this check a silently single-tenant worker looks exactly like a
// healthy one. See engine.PostgresStore.CheckCrossTenantCapability.
func (w *Worker) reportCrossTenantCapability() {
	if !w.claimAcrossTenants {
		return
	}
	checker, ok := w.store.(engine.CrossTenantCapabilityChecker)
	if !ok {
		w.logger.WarnContext(w.ctx,
			"claim-across-tenants is set but this store cannot report whether it supports it; "+
				"the loops will fall back and say so on their first tick",
			"worker_id", w.id)
		return
	}

	capability := checker.CheckCrossTenantCapability(w.ctx)
	for _, c := range []struct {
		what   string
		ok     bool
		reason string
		effect string
	}{
		{"workflow claim", capability.Claim, capability.ClaimReason,
			"only this worker's own tenant's workflows will execute"},
		{"due-schedule read", capability.Schedules, capability.SchedulesReason,
			"only this worker's own tenant's cron will fire"},
	} {
		if c.ok {
			w.logger.InfoContext(w.ctx, "cross-tenant "+c.what+" is available",
				"worker_id", w.id, "tenant_id", w.storeTenantID)
			continue
		}
		w.logger.WarnContext(w.ctx, "cross-tenant "+c.what+" is NOT available; "+c.effect,
			"worker_id", w.id, "tenant_id", w.storeTenantID, "reason", c.reason)
	}
}

// dueSchedules reads the schedules that are ready to fire.
//
// With claimAcrossTenants set it reads for every tenant in one query rather
// than polling each separately -- the same bargain claimGeneral makes, and the
// half that makes a non-default tenant's cron actually fire. 023 lets the
// dispatch loop CLAIM another tenant's workflow; without this, nothing ever
// enqueues one for it to claim, because the loop that would fire the schedule
// cannot see the schedule.
//
// The widened view covers this read and nothing after it. scheduleLoop resolves
// storeForTenant(sch.TenantID) before its first store call and runs the whole
// firing through that.
//
// The provisioning gap is answered the same way as the claim's, and separately
// from it: 024 is a different migration from 023, so a deployment can have one
// and not the other. A store that cannot read across tenants warns once and
// falls back to its own tenant's schedules -- the pre-existing behaviour --
// rather than failing the loop, because a missing GRANT should narrow the
// worker rather than stop it firing anything at all.
func (w *Worker) dueSchedules() ([]engine.Schedule, error) {
	if w.claimAcrossTenants {
		if xt, ok := w.store.(engine.CrossTenantScheduleReader); ok {
			schedules, err := xt.GetDueSchedulesAcrossTenants(w.ctx)
			if !errors.Is(err, engine.ErrCrossTenantClaimUnsupported) {
				return schedules, err
			}
			w.crossTenantSchedulesUnsupportedOnce.Do(func() {
				w.logger.WarnContext(w.ctx,
					"claim-across-tenants is set but this store cannot read due schedules across "+
						"tenants; only this worker's own tenant's schedules will fire",
					"worker_id", w.id, "tenant_id", w.storeTenantID, "reason", err)
			})
			return w.store.GetDueSchedules(w.ctx)
		}
		w.crossTenantSchedulesUnsupportedOnce.Do(func() {
			w.logger.WarnContext(w.ctx,
				"claim-across-tenants is set but this store does not support reading due schedules "+
					"across tenants; only this worker's own tenant's schedules will fire",
				"worker_id", w.id, "tenant_id", w.storeTenantID)
		})
	}
	return w.store.GetDueSchedules(w.ctx)
}

// claimGeneral claims the next batch of runnable work.
//
// With claimAcrossTenants set it claims for every tenant in one query rather
// than polling each separately, which is the whole reason the cross-tenant path
// exists: one round trip per tick regardless of how many tenants there are.
// Each claimed workflow then executes against a store scoped to its OWN tenant
// -- see storeForTenant -- so the widened view lasts exactly as long as the
// claim.
//
// A store that does not implement CrossTenantClaimer falls back rather than
// failing. The flag says what the operator wants; the store says what the
// dialect and the deployment's grants can actually do, and those can disagree
// on a mixed fleet.
func (w *Worker) claimGeneral(limit int) ([]*engine.WorkflowInstance, error) {
	if w.claimAcrossTenants {
		if xt, ok := w.store.(engine.CrossTenantClaimer); ok {
			wfs, err := xt.ClaimWorkflowsAcrossTenants(w.ctx, w.id, limit)
			if !errors.Is(err, engine.ErrCrossTenantClaimUnsupported) {
				return wfs, err
			}
			// The store implements the interface but cannot honour it here --
			// a MySQL store on the per-tenant-database topology, for instance.
			// Fall through to the scoped claim rather than returning the error
			// every tick, which would look like a database fault.
			w.crossTenantUnsupportedOnce.Do(func() {
				w.logger.WarnContext(w.ctx,
					"claim-across-tenants is set but this store's topology cannot honour it; "+
						"claiming only this worker's own tenant",
					"worker_id", w.id, "tenant_id", w.storeTenantID, "reason", err)
			})
			return w.store.ClaimWorkflows(w.ctx, w.id, limit)
		}
		// Once, not every tick: this is a static property of the store, and at
		// the poll interval it would otherwise be one line per second forever.
		w.crossTenantUnsupportedOnce.Do(func() {
			w.logger.WarnContext(w.ctx,
				"claim-across-tenants is set but this store does not support it; "+
					"claiming only this worker's own tenant",
				"worker_id", w.id, "tenant_id", w.storeTenantID)
		})
	}
	return w.store.ClaimWorkflows(w.ctx, w.id, limit)
}
