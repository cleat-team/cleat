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
	if service == "http" && operation == "fetch" {
		return c.handleHTTPFetch(ctx, requestJSON)
	}
	if c.benchSvcURL != "" {
		return c.forwardToBenchSvc(ctx, service, operation, requestJSON)
	}
	return "", fmt.Errorf("service %s.%s not configured: no endpoint registered", service, operation)
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

func (c *dbServiceCaller) forwardToBenchSvc(ctx context.Context, service, operation, requestJSON string) (string, error) {
	url := fmt.Sprintf("%s/call/%s/%s", c.benchSvcURL, service, operation)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(requestJSON))
	if err != nil {
		return "", fmt.Errorf("bench-svc: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	t0 := time.Now()
	resp, err := benchSvcHTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("bench-svc: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("bench-svc: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("bench-svc: %s", string(body))
	}
	slog.Info("BENCH-SVC-CALL", "duration_ms", time.Since(t0).Milliseconds(), "body_bytes", len(body))
	return string(body), nil
}

func (c *dbServiceCaller) handleHTTPFetch(ctx context.Context, requestJSON string) (string, error) {
	var req fetchRequest
	if err := json.Unmarshal([]byte(requestJSON), &req); err != nil {
		return "", fmt.Errorf("http.fetch: invalid request JSON: %w", err)
	}
	if req.URL == "" {
		return "", fmt.Errorf("http.fetch: url is required")
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
		return "", fmt.Errorf("http.fetch: invalid request %s %q: %w", req.Method, req.URL, err)
	}
	for k, v := range req.Headers {
		httpReq.Header.Set(k, v)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("http.fetch: request %s %q failed: %w", req.Method, req.URL, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("http.fetch: reading response: %w", err)
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
	memoryPages := uint32(0)
	if w.wasmMemoryMaxMB != nil && *w.wasmMemoryMaxMB > 0 {
		memoryPages = uint32(*w.wasmMemoryMaxMB * 1024 * 1024 / 65536)
		if memoryPages > 65536 {
			w.logger.WarnContext(context.Background(), "runDefers: wasm-memory-max-mb exceeded", "worker_id", w.id)
			memoryPages = 65536
		}
	}
	rt, err := engine.NewRuntime(w.ctx, memoryPages, uint64(*w.wasmInstructionLimit))
	rt.Metrics = w.Metrics
	if err != nil {
		rt.Metrics = w.Metrics
		w.logger.ErrorContext(context.Background(), "runDefers: create runtime failed", "worker_id", w.id, "error", err)
		return
	}
	defer rt.Close(w.ctx)

	eng := engine.NewEngine(rt, &dbServiceCaller{store: w.store, workerID: w.id, benchSvcURL: *benchSvcURL})
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
	id                   string
	logger               *slog.Logger
	store                engine.WorkflowStore
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

	memoryController            *MemoryController
	maxRetries                  int
	memorySampleRetention       int
	retentionDays               int
	schemaName                  string
	peerSchemas                 []string
	disableChecksumVerification *bool
	requireSignalAuth           *bool
	wasmMemoryMaxMB             *int
	wasmInstructionLimit        *int
	wasmDiskCache               *engine.WasmDiskCache
	wasmtimeBackend             engine.WasmBackend

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

	// initLoopCtx creates a cancellable per-loop context and registers the
	// loop for watchdog monitoring. The done channel is closed by launchLoop
	// when the goroutine exits.
	initLoopCtx := func(name string) {
		ctx, cancel := context.WithCancel(w.ctx)
		w.loopCtxMap[name] = &loopContext{
			ctx:    ctx,
			cancel: cancel,
			done:   make(chan struct{}),
		}
		w.healthTracker.registerLoop(name)
	}
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
	w.loopFuncs["heartbeat"] = w.heartbeatLoop
	w.launchLoop("heartbeat", w.heartbeatLoop)

	// Background zombie reaper goroutine.
	w.loopFuncs["reaper"] = w.reaperLoop
	w.launchLoop("reaper", w.reaperLoop)

	// Background concurrency key reaper goroutine (Feature 5).
	w.loopFuncs["concurrency_key_reaper"] = w.concurrencyKeyReaperLoop
	w.launchLoop("concurrency_key_reaper", w.concurrencyKeyReaperLoop)

	// Dispatch loop.
	w.loopFuncs["dispatch"] = w.dispatchLoop
	w.launchLoop("dispatch", w.dispatchLoop)

	// Cron schedule loop.
	w.loopFuncs["schedule"] = w.scheduleLoop
	w.launchLoop("schedule", w.scheduleLoop)

	// Memory estimate reload loop.
	w.loopFuncs["memory_reload"] = w.memoryReloadLoop
	w.launchLoop("memory_reload", w.memoryReloadLoop)

	// Memory sample cleanup loop.
	w.loopFuncs["memory_cleanup"] = func() { w.memoryCleanupLoop(w.memorySampleRetention) }
	w.launchLoop("memory_cleanup", func() { w.memoryCleanupLoop(w.memorySampleRetention) })

	// Retention loop.
	w.loopFuncs["retention"] = func() { w.retentionLoop(w.retentionDays) }
	w.launchLoop("retention", func() { w.retentionLoop(w.retentionDays) })

	// Update dispatch loop (Feature 3: Update Handler).
	w.loopFuncs["update_dispatch"] = func() { w.updateDispatchLoop(w.getLoopCtx("update_dispatch")) }
	w.launchLoop("update_dispatch", func() { w.updateDispatchLoop(w.getLoopCtx("update_dispatch")) })

	// Watchdog loop for background loop health monitoring.
	if w.healthCheckInterval > 0 {
		initLoopCtx("watchdog")
		w.loopFuncs["watchdog"] = w.watchdogLoop
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
		select {
		case <-w.getLoopCtx("dispatch").Done():
			return
		default:
		}
		// Re-check after the non-blocking select to narrow the
		// TOCTOU window between the check and the DB claims below.
		if w.ctx.Err() != nil {
			return
		}

		w.healthTracker.recordRun("dispatch")

		// If draining and no in-flight work, exit cleanly.
		if w.draining.Load() {
			inflight := 0
			w.inflight.Range(func(_, _ any) bool { inflight++; return true })
			if inflight == 0 {
				w.logger.InfoContext(w.ctx, "drain complete", "worker_id", w.id)
				return
			}
			time.Sleep(w.pollInterval)
			continue
		}
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
			generalWfs, err = w.store.ClaimWorkflows(w.ctx, w.id, remaining)
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
				w.store.ReleaseWorkflow(context.Background(), wf.ID, w.id, wf.Generation, wf.NextWakeAt)
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

	// ---- Assign trace ID ----
	traceID := wf.TraceID
	if traceID == "" {
		traceID = generateTraceID()
	}
	if err := w.store.TraceWorkflow(context.Background(), wf.ID, traceID); err != nil {
		w.logger.WarnContext(context.Background(), "failed to set trace_id", "worker_id", w.id, "workflow_id", wf.ID, "tenant_id", wf.TenantID, "error", err)
	}

	// ---- Load WASM ----
	wasmStart := time.Now()
	wasmBytes, err := w.loadWASM(wf.DefName, wf.DefVersion)
	w.Metrics.RecordWasmCompileDuration(context.Background(), time.Since(wasmStart), wf.DefName)
	if err != nil {
		w.logger.ErrorContext(context.Background(), "failed to load WASM", "worker_id", w.id, "workflow_id", wf.ID, "tenant_id", wf.TenantID, "error", err)
		w.Metrics.RecordWorkflowFailed(context.Background(), wf.DefName, "", "")
		w.Metrics.RecordWorkflowDuration(context.Background(), time.Since(workflowStartTime), wf.DefName, "failed", "")
		var ce *engine.CleatError
		errorCode := engine.ErrUnknown.String()
		errorOp := ""
		if errors.As(err, &ce) {
			errorCode = ce.Code.String()
			errorOp = ce.Op
		}
		errMsg := err.Error()
		if strings.Contains(errMsg, "retries exhausted") {
			w.store.MoveToDeadLetterQueue(context.Background(), wf.ID, w.id, wf.Generation, errMsg, errorCode, errorOp)
			w.Metrics.RecordWorkflowsDeadLettered(context.Background())
		} else {
			w.store.FailWorkflow(context.Background(), wf.ID, w.id, wf.Generation, errMsg, errorCode, errorOp, nil)
		}
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
			w.Metrics.RecordWorkflowFailed(context.Background(), wf.DefName, "", "")
			w.Metrics.RecordWorkflowDuration(context.Background(), time.Since(workflowStartTime), wf.DefName, "failed", "")
			w.store.FailWorkflow(context.Background(), wf.ID, w.id, wf.Generation, errMsg, engine.ErrUnknown.String(), "", nil)
			return
		}
	}

	// ---- Load event history ----
	history, err := w.store.LoadEventHistory(w.ctx, wf.ID)
	if err != nil {
		if isConnectionError(err) {
			w.logger.WarnContext(context.Background(), "DB down loading history", "worker_id", w.id, "workflow_id", wf.ID, "tenant_id", wf.TenantID)
			w.store.ReleaseWorkflow(context.Background(), wf.ID, w.id, wf.Generation, wf.NextWakeAt)
			return
		}
		w.Metrics.RecordWorkflowFailed(context.Background(), wf.DefName, "", "")
		w.Metrics.RecordWorkflowDuration(context.Background(), time.Since(workflowStartTime), wf.DefName, "failed", "")
		w.store.FailWorkflow(context.Background(), wf.ID, w.id, wf.Generation, fmt.Sprintf("workflow %s: history load: %v", wf.ID, err), engine.ErrUnknown.String(), "", nil)
		return
	}

	if engine.DebugTiming {
		w.logger.InfoContext(context.Background(), "loaded history events", "worker_id", w.id, "workflow_id", wf.ID, "tenant_id", wf.TenantID, "count", len(history))
	}

	// ---- Determine entry point ----
	entryPoint := determineEntryPoint(wf.Input, wasmBytes)
	if entryPoint == "" {
		w.Metrics.RecordWorkflowFailed(context.Background(), wf.DefName, "", "")
		w.Metrics.RecordWorkflowDuration(context.Background(), time.Since(workflowStartTime), wf.DefName, "failed", "")
		w.store.FailWorkflow(context.Background(), wf.ID, w.id, wf.Generation,
			"cannot determine entry point: no __entry_point in input and no handle_* export in WASM binary",
			engine.ErrPermanent.String(), "", nil)
		return
	}

	// ---- Load compaction state if present ----
	var compactionState *engine.CompactionState
	compactionState, err = w.store.LoadCompactionState(w.ctx, wf.ID)
	if err != nil {
		w.logger.WarnContext(context.Background(), "failed to load compaction state", "worker_id", w.id, "workflow_id", wf.ID, "tenant_id", wf.TenantID, "error", err)
		compactionState = nil
	}

	// ---- Create engine runtime ----
	// The wazero Runtime is only needed for non-Go languages or when the
	// wasmtime backend is unavailable. Go workflows use the wasmtime backend
	// which compiles and instantiates WASM independently of wazero.
	memoryPages := uint32(0)
	if w.wasmMemoryMaxMB != nil && *w.wasmMemoryMaxMB > 0 {
		memoryPages = uint32(*w.wasmMemoryMaxMB * 1024 * 1024 / 65536)
		if memoryPages > 65536 {
			w.logger.WarnContext(context.Background(), "wasm-memory-max-mb exceeded", "worker_id", w.id, "workflow_id", wf.ID, "tenant_id", wf.TenantID)
			memoryPages = 65536
		}
	}
	needsWazeroRuntime := w.wasmtimeBackend == nil || wasm.DetectLanguage(wasmBytes) != "go"
	var rt *engine.Runtime
	if needsWazeroRuntime {
		var rtErr error
		rt, rtErr = engine.NewRuntime(w.ctx, memoryPages, uint64(*w.wasmInstructionLimit))
		rt.Metrics = w.Metrics
		if rtErr != nil {
			w.Metrics.RecordWorkflowFailed(context.Background(), wf.DefName, "", "")
			w.Metrics.RecordWorkflowDuration(context.Background(), time.Since(workflowStartTime), wf.DefName, "failed", "")
			w.store.FailWorkflow(context.Background(), wf.ID, w.id, wf.Generation, fmt.Sprintf("workflow %s: create runtime: %v", wf.ID, rtErr), engine.ErrUnknown.String(), "", nil)
			if rt != nil {
				rt.Close(w.ctx)
			}
			return
		}
		defer rt.Close(w.ctx)
	}

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
		w.Metrics.RecordWorkflowFailed(context.Background(), wf.DefName, "", "")
		w.Metrics.RecordWorkflowDuration(context.Background(), time.Since(workflowStartTime), wf.DefName, "failed", "")
		w.store.FailWorkflow(context.Background(), wf.ID, w.id, wf.Generation, err.Error(), engine.ErrPermanent.String(), "version_check", nil)
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
				w.Metrics.RecordWorkflowFailed(context.Background(), wf.DefName, "", "")
				w.Metrics.RecordWorkflowDuration(context.Background(), time.Since(workflowStartTime), wf.DefName, "failed", "")
				w.store.FailWorkflow(context.Background(), wf.ID, w.id, wf.Generation, err.Error(), engine.ErrPermanent.String(), "plugin_check", nil)
				return
			}
			if workerVersion != requiredVersion {
				err := fmt.Errorf(
					"plugin version mismatch: workflow requires plugin %q version %s but worker has version %s",
					pluginName, requiredVersion, workerVersion)
				w.logger.ErrorContext(context.Background(), "execution error", "worker_id", w.id, "workflow_id", wf.ID, "tenant_id", wf.TenantID, "error", err)
				w.Metrics.RecordWorkflowFailed(context.Background(), wf.DefName, "", "")
				w.Metrics.RecordWorkflowDuration(context.Background(), time.Since(workflowStartTime), wf.DefName, "failed", "")
				w.store.FailWorkflow(context.Background(), wf.ID, w.id, wf.Generation, err.Error(), engine.ErrPermanent.String(), "plugin_check", nil)
				return
			}
		}
	}

	// Extract child binding policy from WASM metadata for the engine.
	var childBindingPolicy string
	if wfMeta != nil {
		childBindingPolicy = wfMeta.ChildBindingPolicy
	}

	caller := &dbServiceCaller{store: w.store, workerID: w.id, benchSvcURL: *benchSvcURL}
	engineOpts := []engine.EngineOption{
		engine.WithSignalStore(w.store.(engine.SignalStore)),
		engine.WithWorkflowState(&dbWorkflowState{version: wf.DefVersion, minVersion: wf.MinVersion, priority: wf.Priority, childVersions: childVersions}),
		engine.WithWorkflowID(wf.ID),
		engine.WithTraceID(traceID),
		engine.WithTenantID(wf.TenantID),
		engine.WithBackend("go", w.wasmtimeBackend),
		engine.WithWorkflowStore(w.store),
		engine.WithChildWorkflowStore(w.store),
		engine.WithPluginRegistry(w.pluginRegistry),
		engine.WithMaxRetryAttempts(w.maxRetries),
		engine.WithSchema(w.schemaName),
		engine.WithPeerSchemas(w.peerSchemas),
		engine.WithEncryption(w.encryption, w.encryptSensitivePayloads),
		engine.WithMaxQuotaEvents(w.maxQuotaEvents),
		engine.WithMaxQuotaChildren(w.maxQuotaChildren),
		engine.WithMaxQuotaConcurrencyKeys(w.maxQuotaConcurrencyKeys),
		engine.WithDefaultWorkflowTimeout(w.maxWorkflowDuration),
		engine.WithChildBindingPolicy(childBindingPolicy),
		engine.WithChildBindingOverride(w.childBindingOverride),
	}
	// If the store supports concurrency keys (PostgresStore, ShardedStore),
	// enable virtual object scope enforcement.
	if cks, ok := w.store.(engine.ConcurrencyKeyStore); ok {
		engineOpts = append(engineOpts, engine.WithConcurrencyKeyStore(cks))
	}
	// Enable event history checksum verification on replay by default.
	// Can be disabled with --disable-checksum-verification.
	if w.disableChecksumVerification != nil && !*w.disableChecksumVerification {
		engineOpts = append(engineOpts, engine.WithWorkflowEventVerifier(w.store.VerifyWorkflowEvents, false))
	}
	// Enable signal authorization if --require-signal-auth is set.
	if w.requireSignalAuth != nil && *w.requireSignalAuth {
		engineOpts = append(engineOpts,
			engine.WithRequireSignalAuth(true),
			engine.WithSignalAuthCheck(func(ctx context.Context, targetWorkflowID, callerDefName string) error {
				callers, err := w.store.GetAllowedSignalCallers(ctx, targetWorkflowID)
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
			}),
		)
	}
	// Enable event history checksum verification on replay by default.
	// Can be disabled with --disable-checksum-verification.
	if w.disableChecksumVerification != nil && !*w.disableChecksumVerification {
		engineOpts = append(engineOpts, engine.WithWorkflowEventVerifier(w.store.VerifyWorkflowEvents, true))
	}
	// Always provide DB so per-step flush and adaptive flusher work.
	engineOpts = append(engineOpts, engine.WithDB(w.db))
	// Use tenant-scoped database connection for plugin host functions if available.
	if w.tenantPools != nil && wf.TenantID != "" {
		tenantDB, err := w.tenantPools.For(w.ctx, wf.TenantID)
		if err != nil {
			w.logger.ErrorContext(context.Background(), "cannot get tenant pool", "worker_id", w.id, "workflow_id", wf.ID, "tenant_id", wf.TenantID, "error", err)
			w.Metrics.RecordWorkflowFailed(context.Background(), wf.DefName, "", "")
			w.Metrics.RecordWorkflowDuration(context.Background(), time.Since(workflowStartTime), wf.DefName, "failed", "")
			w.store.FailWorkflow(context.Background(), wf.ID, w.id, wf.Generation, fmt.Sprintf("tenant pool: %v", err), engine.ErrUnknown.String(), "", nil)
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
		oldDef, err := w.store.GetWorkflowDef(w.ctx, wf.DefName, wf.DefVersion)
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
		if count, err := w.store.GetEventCount(w.ctx, wf.ID); err == nil {
			engineOpts = append(engineOpts, engine.WithInitialEventCount(count))
		}
	}
	if noPerStepFlush != nil && *noPerStepFlush {
		engineOpts = append(engineOpts, engine.WithNoPerStepFlush(true))
	}
	if w.flusherRegistry != nil {
		engineOpts = append(engineOpts, engine.WithFlusherRegistry(w.flusherRegistry))
	} else {
		w.logger.InfoContext(context.Background(), "flusher registry not set on worker — using direct flush", "workflow_id", wf.ID)
	}
	// Throttle cancellation polls to at most once per 100ms wall-clock
	// to avoid a full DB transaction on every durable step.
	engineOpts = append(engineOpts, engine.WithCancellationCheckInterval(100*time.Millisecond))
	eng := engine.NewEngine(rt, caller, engineOpts...)
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
		w.Metrics.RecordWorkflowFailed(context.Background(), wf.DefName, "", "")
		w.Metrics.RecordWorkflowDuration(context.Background(), time.Since(workflowStartTime), wf.DefName, "failed", "")
		var ce *engine.CleatError
		errorCode := engine.ErrUnknown.String()
		errorOp := ""
		if errors.As(err, &ce) {
			errorCode = ce.Code.String()
			errorOp = ce.Op
		}
		errMsg := err.Error()
		if strings.Contains(errMsg, "retries exhausted") {
			w.store.MoveToDeadLetterQueue(context.Background(), wf.ID, w.id, wf.Generation, errMsg, errorCode, errorOp)
			w.Metrics.RecordWorkflowsDeadLettered(context.Background())
		} else {
			w.store.FailWorkflow(context.Background(), wf.ID, w.id, wf.Generation, errMsg, errorCode, errorOp, nil)
		}
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
		newRunID, err := w.store.ContinueAsNew(w.ctx, wf.ID, w.id, wf.Generation, wf.DefName, wf.DefVersion, json.RawMessage(suspended.NewInput), newEvents, result, queryState, wf.Priority)
		if err != nil {
			w.logger.ErrorContext(context.Background(), "continue_as_new failed", "worker_id", w.id, "workflow_id", wf.ID, "tenant_id", wf.TenantID, "error", err)
			w.Metrics.RecordWorkflowFailed(context.Background(), wf.DefName, "", "")
			w.Metrics.RecordWorkflowDuration(context.Background(), time.Since(workflowStartTime), wf.DefName, "failed", "")
			w.store.FailWorkflow(context.Background(), wf.ID, w.id, wf.Generation, fmt.Sprintf("continue_as_new: %v", err), engine.ErrUnknown.String(), "", nil)
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
	err = w.store.FinalizeWorkflowSegment(w.ctx, wf.ID, w.id, wf.Generation, newEvents, finalStatus, result, "", "", queryState, nextWakeAt)
	finalizeElapsed := time.Since(queryStart)
	if err != nil {
		if engine.DebugTiming {
			w.logger.InfoContext(context.Background(), "TIMING: finalize error", "worker_id", w.id, "workflow_id", wf.ID, "elapsed_ms", finalizeElapsed.Milliseconds())
		}
		w.Metrics.RecordDBQueryLatency(context.Background(), time.Since(queryStart), "finalize")
		if isConnectionError(err) {
			w.logger.WarnContext(context.Background(), "DB down finalizing", "worker_id", w.id, "workflow_id", wf.ID, "tenant_id", wf.TenantID)
			w.store.ReleaseWorkflow(context.Background(), wf.ID, w.id, wf.Generation, wf.NextWakeAt)
			return
		}
		w.logger.ErrorContext(context.Background(), "finalize error", "worker_id", w.id, "workflow_id", wf.ID, "tenant_id", wf.TenantID, "error", err)
		w.Metrics.RecordWorkflowFailed(context.Background(), wf.DefName, "", "")
		w.Metrics.RecordWorkflowDuration(context.Background(), time.Since(workflowStartTime), wf.DefName, "failed", "")
		var ce *engine.CleatError
		errorCode := engine.ErrUnknown.String()
		errorOp := ""
		if errors.As(err, &ce) {
			errorCode = ce.Code.String()
			errorOp = ce.Op
		}
		errMsg := err.Error()
		if strings.Contains(errMsg, "retries exhausted") {
			w.store.MoveToDeadLetterQueue(context.Background(), wf.ID, w.id, wf.Generation, errMsg, errorCode, errorOp)
			w.Metrics.RecordWorkflowsDeadLettered(context.Background())
		} else {
			w.store.FailWorkflow(context.Background(), wf.ID, w.id, wf.Generation, errMsg, errorCode, errorOp, nil)
		}
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
				w.Metrics.SetBackgroundLoopItemsProcessed(w.ctx, "concurrency_key_reaper", int64(reaped))
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
			schedules, err := w.store.GetDueSchedules(w.ctx)
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

				// Find latest version.
				versions, verr := w.store.ListVersions(w.ctx, sch.DefName)
				if verr != nil || len(versions) == 0 {
					w.logger.WarnContext(w.ctx, "Scheduler: definition not found", "worker_id", w.id, "schedule", sch.Name, "def_name", sch.DefName)
					continue
				}

				runID, _, serr := w.store.StartNewRun(w.ctx, "", sch.DefName, versions[0], input, "", engine.DefaultTenantUUID, 0)
				if serr != nil {
					w.logger.ErrorContext(w.ctx, "Scheduler: failed to start workflow", "worker_id", w.id, "schedule", sch.Name, "error", serr)
					continue
				}

				// Compute next run time and update.
				nextRun := engine.NextCronTime(sch.CronExpression, time.Now())
				if uerr := w.store.UpdateScheduleNextRun(w.ctx, sch.Name, nextRun); uerr != nil {
					w.logger.ErrorContext(w.ctx, "Scheduler: failed to update next run", "worker_id", w.id, "schedule", sch.Name, "error", uerr)
				}

				w.logger.InfoContext(w.ctx, "Scheduler: fired schedule", "worker_id", w.id, "schedule", sch.Name, "workflow_id", runID, "next_at", nextRun.Format(time.RFC3339))
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

func (w *Worker) retentionLoop(retentionDays int) {
	defer w.wg.Done()
	if retentionDays <= 0 {
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
			cutoff := time.Now().Add(-time.Duration(retentionDays) * 24 * time.Hour)
			deleted, err := w.store.DeleteExpiredEvents(w.ctx, cutoff)
			if err != nil {
				w.logger.ErrorContext(w.ctx, "retention: error deleting expired events", "worker_id", w.id, "error", err)
			} else if deleted > 0 {
				w.Metrics.RecordEventsDeleted(w.ctx, deleted)
				w.logger.InfoContext(w.ctx, "retention: deleted expired event rows", "worker_id", w.id, "count", deleted)
			}
			w.Metrics.SetRetentionLastRunTimestamp(w.ctx, time.Now().Unix())
		}
	}
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
					w.Metrics.SetBackgroundLoopItemsProcessed(w.ctx, "memory_cleanup", int64(deleted))
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

func (w *Worker) releaseOrFail(wf *engine.WorkflowInstance, errMsg string) {
	if errMsg != "" {
		if strings.Contains(errMsg, "retries exhausted") {
			w.store.MoveToDeadLetterQueue(context.Background(), wf.ID, w.id, wf.Generation, errMsg, "", "")
			w.Metrics.RecordWorkflowsDeadLettered(context.Background())
		} else {
			w.store.FailWorkflow(context.Background(), wf.ID, w.id, wf.Generation, errMsg, "", "", nil)
		}
	} else {
		w.store.ReleaseWorkflow(context.Background(), wf.ID, w.id, wf.Generation, wf.NextWakeAt)
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
