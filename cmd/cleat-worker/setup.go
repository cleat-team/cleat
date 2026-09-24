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
	"regexp"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/internal/tenantctx"
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

// stats reports the cache's occupancy for cleat_wasm_cache_entries and
// cleat_wasm_cache_bytes (cleat#1317). Both gauges existed, were registered and
// described, and nothing ever called them -- so an operator sizing
// --wasm-cache-max-mb had no way to see what the cache actually held.
//
// Reuses sizeBytesLocked rather than tracking a running total, because a
// running total is a second source of truth for a number this already computes
// and would drift silently on any future eviction path that forgot to update
// it. The walk is over at most --wasm-cache-max-entries elements, once per
// memory tick.
func (c *wasmLRUCache) stats() (entries int, bytes int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.index), c.sizeBytesLocked()
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

	// traceID is this run's W3C trace-id, propagated on guest-initiated
	// fetches so the callee joins the caller's trace. cleat#1596.
	//
	// A FIELD, not a context value, because this caller is already per-run:
	// it is constructed inside executeWorkflow alongside engine.WithTraceID,
	// from the same traceID. Reaching for context plumbing would add a second
	// way to answer a question this struct's lifetime already answers.
	traceID string

	// egress is the network policy for guest-initiated fetches. Nil means the
	// strict default, which is what production builds -- main.go never sets
	// this, and TestNoProductionCodeAllowsLoopbackEgress keeps it that way.
	// It is a field only so that tests pinning non-egress behaviour can reach
	// an httptest server. cleat#1565.
	egress *engine.EgressGuard

	// privateHosts is the operator's set of hosts permitted to be private
	// addresses, from --plugin-egress-allow-private. Never nil: the zero value
	// permits nothing.
	privateHosts *pluginPrivateHosts

	// egressAllow is the per-tenant allowlist source. Nil denies every
	// guest-initiated fetch, which is the correct behaviour for a worker
	// that was not given one: an absent policy is not permission.
	egressAllow *engine.TenantEgressStore

	// operatorEgress is the DEPLOYMENT's egress policy, from
	// --egress-allowlist. Nil or empty permits every public host -- the
	// operator layer defaults open where the tenant layer defaults closed,
	// because the floor sits underneath it. cleat#1565.
	operatorEgress *engine.HostAllowlist

	// serviceEndpoints maps a service name to the base URL that serves it, from
	// --service-endpoints. Empty means no service is registered and every named
	// call falls through to --bench-svc-url or to a permanent refusal.
	//
	// A MAP RATHER THAN A DB LOOKUP, deliberately, and this is the first half of
	// a two-step. A flag map removes the worker rebuild for a deployment whose
	// service set is fixed at boot, which is most of them, and costs no
	// migration, no cache and no per-call query. A tenant-scoped
	// service_endpoints table is the half that makes the set changeable without
	// a restart and different per tenant -- and it needs a cache, because this
	// sits on the call path.
	serviceEndpoints map[string]string

	// secrets resolves ${secret:name} in a request on the way OUT to the
	// service, and the direction is the whole point.
	//
	// THE ORDERING IS WHAT KEEPS THE SECRET OUT OF THE HISTORY, exactly as it
	// does for plugins at withSecrets. engine/durablecalls.go calls
	// callService(..., requestJSON, ...) and then records `Request:
	// requestJSON` from the SAME variable; freshCallWithIntent writes its
	// intent row from that variable too, before dispatch. Because the
	// substitution happens in HERE -- past both of those -- neither the event
	// nor the intent can observe it: history keeps the reference the workflow
	// wrote.
	//
	// Move this any earlier -- into callService, into the host call, into the
	// guest -- and the recorded request becomes the plaintext credential. The
	// only thing standing between that and an operator reading it is
	// engine.Redact, a field-name heuristic that matches seven substrings and
	// returns non-JSON input untouched. That is a guess; this is a guarantee.
	secrets *engine.SecretStore
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
// WORKSTREAM.md's shared-files table. Added here because the engine-side mechanism is inert
// without a caller that implements it, and shipping a mechanism nothing calls is
// the exact shape of the §1.4 defect this phase exists to avoid. Additive: the
// existing Call is unchanged in behaviour and delegates to the same helper.
func (c *dbServiceCaller) CallWithIdempotencyKey(ctx context.Context, service, operation, requestJSON, idempotencyKey string) (string, error) {
	return c.call(ctx, service, operation, requestJSON, idempotencyKey)
}

func (c *dbServiceCaller) call(ctx context.Context, service, operation, requestJSON, idempotencyKey string) (string, error) {
	requestJSON, err := c.resolveSecrets(ctx, service, operation, requestJSON)
	if err != nil {
		return "", err
	}
	if service == "http" && operation == "fetch" {
		return c.handleHTTPFetch(ctx, requestJSON, idempotencyKey)
	}
	// A REGISTERED ENDPOINT WINS OVER THE CATCH-ALL, so adding one changes
	// nothing for a deployment that has none. --bench-svc-url stays what it was:
	// a single forwarder for every service, which a benchmark wants and a real
	// deployment does not.
	if base, ok := c.serviceEndpoints[service]; ok {
		return c.forwardToService(ctx, base, service, operation, requestJSON, idempotencyKey)
	}
	if c.benchSvcURL != "" {
		return c.forwardToService(ctx, c.benchSvcURL, service, operation, requestJSON, idempotencyKey)
	}
	// Names the way out, because "not configured" without it sends a reader to
	// look for a plugin they do not need to write.
	return "", engine.NewPermanentError("call", "", fmt.Errorf(
		"service %s.%s not configured: no endpoint registered. Register one with "+
			"--service-endpoints %s=https://your-service, or implement it as a plugin",
		service, operation, service))
}

// resolveSecrets substitutes ${secret:name} in a request on its way to the
// service, and is the ONE funnel both entry points already share -- Call and
// CallWithIdempotencyKey both delegate to call, so a single resolution here
// covers the plain path, the idempotency-key path, and (because callService
// and freshCallWithIntent both dispatch through engine.ServiceCaller) the
// write-ahead-intent path as well.
//
// DONE HERE RATHER THAN IN A DECORATOR, and that is not a style preference.
// engine.IdempotentCaller EMBEDS engine.ServiceCaller, and
// engine/idempotency.go type-asserts `s.engine.caller.(IdempotentCaller)` to
// decide whether to forward an idempotency key at all. A wrapper implementing
// only Call would fail that assertion silently: every call would keep working,
// keys would stop being forwarded, and exactly-once would quietly degrade to
// at-least-once with nothing red anywhere. Resolving inside the existing type
// cannot make that mistake.
//
// A tenantless context passes through rather than guessing. This mirrors
// withSecrets: one worker serves many tenants, a reference cannot be resolved
// against "some default tenant", and letting the literal text reach the service
// fails loudly at the far end instead of silently sending another tenant's
// credential.
//
// A reference naming a secret the tenant does not have is PERMANENT, not
// retryable. ResolveSecretRefs already refuses to substitute empty for a
// missing name; retrying cannot create the secret, and a retry budget spent on
// a typo is a worse diagnostic than one clear failure.
func (c *dbServiceCaller) resolveSecrets(ctx context.Context, service, operation, requestJSON string) (string, error) {
	if c.secrets == nil {
		return requestJSON, nil
	}
	tid, ok := tenantctx.From(ctx)
	if !ok {
		return requestJSON, nil
	}
	resolved, err := engine.ResolveSecretRefs(ctx, c.secrets, tid.String(), requestJSON)
	if err != nil {
		return "", engine.NewPermanentError(service+"."+operation, "", err)
	}
	return resolved, nil
}

// forwardToService POSTs a durable call to a registered service.
//
// TAKES THE BASE URL RATHER THAN READING c.benchSvcURL, which is the whole of
// the change that turns a benchmarking hook into an extension point. Everything
// else here was already right for the job: the conventional route, the trace
// header, the idempotency key, and an error classification that decides whether
// the engine retries.
func (c *dbServiceCaller) forwardToService(ctx context.Context, baseURL, service, operation, requestJSON, idempotencyKey string) (string, error) {
	url := fmt.Sprintf("%s/call/%s/%s", baseURL, service, operation)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(requestJSON))
	if err != nil {
		return "", engine.NewPermanentError(service+"."+operation, "", fmt.Errorf("create request: %w", err))
	}
	plugin.SetTraceparent(req, c.traceID)
	req.Header.Set("Content-Type", "application/json")
	if idempotencyKey != "" {
		// The conventional header name, as used by Stripe and others. A service
		// that does not recognise it ignores it, so this is safe to always send.
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	// GUARDED, AND PER CALL RATHER THAN POOLED, which reverses the trade this
	// used to make. It held a package-level http.Client with a bare
	// &http.Transport{} and connection pooling, on the reasoning that a client
	// per call exhausts ephemeral ports under load. That is true, and it bought
	// the speed by skipping the egress floor entirely: the one forwarder that
	// reaches an operator-named service was the one outbound path in the worker
	// that could dial loopback, RFC1918 or 169.254.169.254.
	//
	// POOLING AND A PER-TENANT POLICY ARE NOT COMPOSABLE THE OBVIOUS WAY, which
	// is why handleHTTPFetch builds its client per call too and why copying that
	// is the fix rather than adding a guarded dialer to the shared client.
	// EgressGuard checks at DIAL. net/http keys idle connections by scheme, host
	// and proxy -- not by tenant -- so a connection opened for tenant A and
	// reused for tenant B never dials again, and B's allowlist is never
	// consulted. A shared pool with a guarded dialer would be guarded only for
	// whichever tenant happened to open the connection.
	//
	// The cost is one handshake per call on a path that today carries
	// --bench-svc-url only. If this forwarder becomes the way a workflow reaches
	// a registered service -- the "no endpoint registered" error below is
	// written as though it will -- this becomes a hot path and the answer is a
	// pool PER TENANT, not a shared one.
	client := &http.Client{
		Timeout: 30 * time.Second,
		// THE ENDPOINT IN HAND, not c.benchSvcURL. This forwarder now serves
		// every registered service, so exempting the bench URL would grant a
		// host this call is not dialling and refuse the one it is: every
		// --service-endpoints destination on a private address would be denied,
		// which is most of them -- a sidecar on loopback, or a cluster-internal
		// name, which is RFC1918 by construction.
		Transport: &http.Transport{DialContext: c.serviceEgressGuard(ctx, baseURL).DialContext},
	}
	t0 := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return "", engine.NewTransientError(service+"."+operation, "", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", engine.NewTransientError(service+"."+operation, "", fmt.Errorf("read response: %w", err))
	}
	if resp.StatusCode != http.StatusOK {
		return "", serviceStatusError(resp.StatusCode, body)
	}
	slog.Debug("BENCH-SVC-CALL", "duration_ms", time.Since(t0).Milliseconds(), "body_bytes", len(body))
	return string(body), nil
}

// serviceStatusError turns a non-200 from a called service into a classified
// error, preferring what the service SAID over what its status implies.
//
// TWO CHANNELS, IN ORDER.
//
// If the body is the structured error contract -- a JSON object with a
// non-empty "code" -- the service has described its own failure, and this
// keeps that description intact: the code a workflow's non-retryable
// declarations match, a correlation id linking to the callee's logs, and the
// service's own statement about whether retrying is worth anything. A stated
// "retryable" wins over the status, because a service knows things a status
// code cannot express -- a 500 that is genuinely permanent, a 400 that will
// succeed once a dependency catches up.
//
// Otherwise the status decides, exactly as before: 4xx means the service
// understood the request and rejected it, so sending the same bytes again
// produces the same rejection; 408 and 429 are the documented exceptions,
// explicit invitations to try again; everything else, including every 5xx,
// stays retryable. That path is what every service not speaking this contract
// gets, and it is unchanged.
//
// THE CLASSIFICATION IS DECIDED HERE AND NOWHERE ELSE. ServiceError
// deliberately does not implement RetryableError, so there is no second
// answer for errors.As to find first depending on unwrap order.
//
// WHAT IS NOT CARRIED YET: a diagnostic tier. A service's internal detail is
// useful to a caller inside the same trust boundary and must not leave it, and
// cleat cannot tell those apart until exposure classes exist. Until then this
// carries only the part that is contract.
func serviceStatusError(status int, body []byte) error {
	statusSaysPermanent := status >= 400 && status < 500 &&
		status != http.StatusRequestTimeout && status != http.StatusTooManyRequests

	if se := engine.ParseServiceError(body, status); se != nil {
		permanent := statusSaysPermanent
		if stated, retryable := se.RetryableFromService(); stated {
			permanent = !retryable
		}
		if permanent {
			return engine.NewPermanentError("service", "", se)
		}
		return engine.NewTransientError("service", "", se)
	}

	err := fmt.Errorf("%s", string(body))
	if statusSaysPermanent {
		return engine.NewPermanentError("service", "", err)
	}
	return engine.NewTransientError("service", "", err)
}

// egressGuard is the policy this caller enforces for one request.
//
// Built per call rather than once, because the allowlist is PER TENANT and the
// tenant comes from the context of the workflow being executed --
// engine.execSession.callService scopes it (cleat#1565).
func (c *dbServiceCaller) egressGuard(ctx context.Context) *engine.EgressGuard {
	if c.egress != nil {
		return c.egress // a test supplied one
	}
	g := &engine.EgressGuard{OperatorAllows: operatorAllowFunc(c.operatorEgress)}
	if c.egressAllow == nil {
		// No allowlist source configured: AllowHost stays nil and the guard
		// denies. Deliberately not a nil-check that opens the gate.
		return g
	}
	tid, ok := auth.TenantIDFromContext(ctx)
	if !ok {
		return g // no tenant, so no list, so nothing is permitted
	}
	tenant := tid.String()
	g.AllowHost = func(ctx context.Context, host string) (bool, error) {
		list, err := c.egressAllow.For(ctx, tenant)
		if err != nil {
			// An error is NOT a grant. Returned rather than swallowed so the
			// refusal says "could not read the allowlist" instead of "not on
			// the allowlist", which would send someone to edit a list that is
			// not the problem.
			return false, err
		}
		return list.Permits(host), nil
	}
	return g
}

func (c *dbServiceCaller) handleHTTPFetch(ctx context.Context, requestJSON, idempotencyKey string) (string, error) {
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
	// cleat#1565. Scheme first, because a refused scheme should not depend on
	// whether the host happens to resolve.
	if err := engine.CheckScheme(httpReq.URL.Scheme); err != nil {
		return "", engine.NewPermanentError("http.fetch", "", err)
	}
	for k, v := range req.Headers {
		httpReq.Header.Set(k, v)
	}
	// The engine's replay-stable key, AFTER the guest's headers so a guest that
	// set its own keeps it. cleat#1837.
	//
	// http.fetch is the only way a workflow reaches an external service without
	// a Go plugin and a worker rebuild, so it is the path a team takes when they
	// do what this architecture pushes them toward: keep the workflow an
	// orchestrator and put the rest behind an idempotent HTTP API. That depends
	// on the service being able to deduplicate. The engine computed exactly the
	// value it would deduplicate on -- DurableCallIdempotencyKey, stable across
	// replays -- handed it down the whole chain, and this function dropped it.
	//
	// It compounds with a default: --write-ahead-intent-ops is "" unless set, so
	// an unflagged DurableCall is at-least-once. At-least-once dispatch with no
	// key means a duplicate side effect that the receiver cannot notice.
	//
	// THE EMPTY-STRING CASE IS DELIBERATE, and it is the one the traceparent
	// comment below records getting wrong. Get() returns "" both for a header
	// the guest never set and for one it set empty, and both take the engine's
	// key here. A guest that wants no deduplication has to mean it some other
	// way, because the alternative -- an empty header silently disabling
	// deduplication -- is the failure this change exists to remove, and it would
	// be reachable by accident.
	if idempotencyKey != "" && httpReq.Header.Get("Idempotency-Key") == "" {
		httpReq.Header.Set("Idempotency-Key", idempotencyKey)
	}
	// AFTER the guest's headers. SetTraceparent leaves a non-empty traceparent
	// alone, so a guest that set one deliberately keeps it either way -- the
	// ordering is NOT what protects that, and an earlier version of this
	// comment claimed it was. Moving the call above the loop passes every test
	// but one.
	//
	// The one it fails is the reason for the placement: a guest supplying
	// traceparent as an EMPTY STRING. After the loop, SetTraceparent sees no
	// usable value and supplies the run's trace; before it, the loop's empty
	// value wins and the hop goes silent. Pinned by
	// TestAnEmptyGuestTraceparentDoesNotSilenceTheHop. cleat#1596.
	plugin.SetTraceparent(httpReq, c.traceID)
	// The guard is on the TRANSPORT, not on req.URL, and that placement is the
	// whole point: a stock client follows up to ten redirects and re-resolves
	// each hop, so a check on the guest's URL says nothing about where the
	// request finally goes. See engine.EgressGuard.
	client := &http.Client{
		Timeout:   30 * time.Second,
		Transport: &http.Transport{DialContext: c.egressGuard(ctx).DialContext},
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		// A refusal is PERMANENT. The generic failure below is transient, and
		// retrying a policy decision burns the attempt budget against an
		// answer that cannot change. cleat#1565 open question 5.
		var denied *engine.EgressDeniedError
		if errors.As(err, &denied) {
			return "", engine.NewPermanentError("http.fetch", "", denied)
		}
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

	// secrets resolves ${secret:name} in a call's argument on the way IN to the
	// plugin. Nil when no master key is configured, in which case a call
	// carrying a reference fails rather than passing the literal text through
	// to a third-party service as if it were a credential.
	secrets *engine.SecretStore
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
	// BOTH adapters must carry the split, and there are exactly two:
	// engine/app.go and cmd/cleat-worker/setup.go. Passing only Idempotent
	// through either would leave that path on the pre-cleat#1318 meaning --
	// silently, because the registry would then read SameValueOnReplay as
	// false and simply stop re-invoking, which looks exactly like the fix
	// working.
	return a.registry.RegisterWithPolicy(a.pluginName, opts.Name, a.withSecrets(fn), engine.ReplayPolicy{Idempotent: opts.Idempotent, SameValueOnReplay: opts.SameValueOnReplay})
}

// withSecrets wraps a plugin function so ${secret:name} in its argument is
// replaced by the value before the plugin sees it.
//
// WRAPPING AT REGISTRATION IS WHAT KEEPS THE SECRET OUT OF EVENT HISTORY, and
// that is a structural property rather than a convention. PluginCall records
// `PluginInput: inputJSON` and separately calls `fn(callCtx, inputJSON)` with
// the same variable (engine/plugins.go). Because the substitution happens
// INSIDE fn, the recorder cannot see it: history keeps the reference the
// workflow wrote. Move this substitution any earlier -- into the host call, or
// into the guest -- and the recorded input becomes the plaintext, which no
// amount of redaction reliably fixes, engine.Redact being a field-name
// heuristic.
//
// The tenant comes from the call context rather than from the adapter, because
// one worker serves many tenants and this wrapper is built once at startup.
func (a *hostPluginRegistryAdapter) withSecrets(fn plugin.PluginFunc) plugin.PluginFunc {
	if a.secrets == nil {
		return fn
	}
	return func(ctx context.Context, inputJSON string) (string, error) {
		tid, ok := tenantctx.From(ctx)
		if !ok {
			// No tenant means no secrets to resolve. A reference here would be
			// unresolvable anyway, and ResolveSecretRefs would have to guess
			// whose secret was meant -- so pass through and let the reference
			// reach the plugin as literal text rather than silently resolving
			// it against some default tenant.
			return fn(ctx, inputJSON)
		}
		resolved, err := engine.ResolveSecretRefs(ctx, a.secrets, tid.String(), inputJSON)
		if err != nil {
			return "", err
		}
		return fn(ctx, resolved)
	}
}

// withSecretsStream is withSecrets for PluginStreamFunc (cleat#1987).
//
// RegisterStream used to hand fn straight to the stream registry with no
// equivalent of withSecrets, so ${secret:NAME} in a streaming call's input --
// llm.chat_stream is the one that matters today -- reached the plugin as the
// literal reference text. Same tenant lookup, same pass-through on no tenant,
// and the same unwrapped error on resolution failure as withSecrets above:
// there is exactly one error-classification decision to make for this family,
// not one per registration path.
func (a *hostPluginRegistryAdapter) withSecretsStream(fn plugin.PluginStreamFunc) plugin.PluginStreamFunc {
	if a.secrets == nil {
		return fn
	}
	return func(ctx context.Context, inputJSON string) (<-chan plugin.StreamEvent, error) {
		tid, ok := tenantctx.From(ctx)
		if !ok {
			return fn(ctx, inputJSON)
		}
		resolved, err := engine.ResolveSecretRefs(ctx, a.secrets, tid.String(), inputJSON)
		if err != nil {
			return nil, err
		}
		return fn(ctx, resolved)
	}
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
	return a.streamRegistry.RegisterStream(a.pluginName, opts, a.withSecretsStream(fn))
}

// ---------------------------------------------------------------------------
// WASM helpers
// ---------------------------------------------------------------------------

// determineEntryPoint extracts the entry point name a workflow start should
// call. Three sources, in order:
//
//  1. an explicit "__entry_point" field in the start input;
//  2. the WASM's own cleat.metadata, which cleat build now stamps with the
//     entry points it already computed to generate the exports in the first
//     place (cleat#2066). Exactly one declared entry point resolves without
//     the caller saying so; more than one is genuinely ambiguous and errors
//     naming the candidates, rather than silently picking one.
//  3. firstHandleExport, kept for WASM built before this or by an SDK that
//     has not adopted EntryPoints yet -- see its own doc comment for why this
//     alone was never reliable.
//
// cleat#2066 measured that no SDK in this repo's codegen ever produces a
// "handle_"-prefixed export by convention (confirmed for Go, Rust, Java and
// AssemblyScript), so (2) is the one of these three that is not a guess.
func determineEntryPoint(input json.RawMessage, wasmBytes []byte) (string, error) {
	var in struct {
		EntryPoint string `json:"__entry_point"`
	}
	if err := json.Unmarshal(input, &in); err == nil && in.EntryPoint != "" {
		return in.EntryPoint, nil
	}
	if meta, err := wasm.ReadMetadata(wasmBytes); err == nil && len(meta.EntryPoints) > 0 {
		if len(meta.EntryPoints) == 1 {
			return meta.EntryPoints[0], nil
		}
		return "", fmt.Errorf("cannot determine entry point: this workflow declares %d entry points (%s) and the input named none of them via __entry_point",
			len(meta.EntryPoints), strings.Join(meta.EntryPoints, ", "))
	}
	if ep := firstHandleExport(wasmBytes); ep != "" {
		return ep, nil
	}
	return "", fmt.Errorf("cannot determine entry point: no __entry_point in input, no entry points declared in cleat.metadata, and no handle_* export in WASM binary")
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
//
// IT TAKES THE DRIVER, and that is the fix rather than a tidy-up. `search_path`
// is a PostgreSQL concept and --schema is documented as PostgreSQL-only, but
// two of the worker's three pools called this without checking the driver:
// the migration pool and the adaptive flusher pool, the second of which opens
// on every worker by default. Measured against live servers (cleat#1374):
//
//	root:...@tcp(...)/cleat?search_path=x   Error 1193: Unknown system variable
//	sqlserver://...?search_path=x            accepted and SILENTLY IGNORED
//
// So a non-default --schema broke MySQL at the first query and did nothing at
// all on SQL Server, while the main pool -- which got it right, inside its
// `case "postgres":` arm -- put its tables where they were asked for. The
// worst of the three outcomes is SQL Server's: two pools disagreeing about
// which schema they address, with no error anywhere.
//
// Deciding here rather than at each call site means a new pool cannot get it
// wrong by forgetting a check that lives somewhere else.
func dsnWithSchema(dsn, schema, driver string) string {
	if driver != "postgres" {
		return dsn
	}
	if schema == "" || schema == "public" || strings.Contains(dsn, "search_path=") {
		return dsn
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	return dsn + sep + "search_path=" + schema
}

// baseDSNFromURL parses a PostgreSQL connection URL and returns the base
// connection DSN in libpq key=value format, WITHOUT user or password.
// plugin.TenantPools appends `user=<role> password=<derived>` per tenant.
//
// RESTORED. #1350 deleted this, correctly: its only production caller was the
// unreachable tenant-pool branch, so it was dead, and two guards said so --
// check-test-only-code.sh and check-unreachable-main.sh. cleat#1307 makes that
// branch reachable behind --tenant-isolation=role, so the helper is needed
// again and comes back from git, which is the cost that was accepted when it
// went. Recovered from a06a0b72^ rather than rewritten, so the format it emits
// is byte-for-byte what plugin/tenant_db.go was written against.
//
// The 5432 default and the libpq output are why role isolation is PostgreSQL
// only: this produces `host=… port=… dbname=… sslmode=…`, which no other
// driver accepts.
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
	s := strings.ToLower(err.Error())

	// EOF IS MATCHED AS A WORD, NOT A SUBSTRING. cleat#1465.
	//
	// It used to be in the list below, and `strings.Contains` on a lowercased
	// haystack meant it matched inside any word containing "eof" -- most
	// importantly `typeof`, which is t-y-p-e-o-f. So a guest error like
	//
	//     TypeError: undefined is not a function (typeof x)
	//
	// classified as a connection error. cleat ships an AssemblyScript SDK and a
	// JS-family toolchain, where `typeof` is ordinary error text rather than an
	// exotic string.
	//
	// The consequence is liveness, not tidiness: at the :2127 call site a match
	// means releaseWorkflow and a re-claim, and a TypeError in guest code
	// reproduces deterministically, so it produces the same message every time.
	// The run never reaches a terminal state and never records an error_code.
	//
	// \b before the E is what does the work: in `typeof` the preceding `p` and
	// the `e` are both word characters, so there is no boundary and no match.
	// The trailing `f` at end-of-input IS a boundary, which is exactly why the
	// substring form matched and this does not.
	if eofWord.MatchString(s) {
		return true
	}

	// The rest stay substring matches: every one is a multi-word phrase, so the
	// collision this fixes does not arise for them.
	patterns := []string{
		"connection refused",
		"connection reset",
		"connection closed",
		"no reachable servers",
		"server closed the connection",
		"connection timed out",
		"broken pipe",
		"driver: bad connection",
	}
	for _, p := range patterns {
		if strings.Contains(s, p) {
			return true
		}
	}
	return false
}

// eofWord matches EOF as a standalone token. Case-insensitive deliberately:
// nothing about the collision requires being stricter about case, and a driver
// emitting a lowercase `eof` token should still be caught.
var eofWord = regexp.MustCompile(`\beof\b`)

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

	// now stands in for time.Now in every method below. Always time.Now in
	// production; a test overrides it to advance the clock past maxAge
	// without a real sleep (cleat#2004) -- same shape as
	// engine/tenant_egress_store.go's `now func() time.Time // tests`.
	now func() time.Time
}

func newHealthTracker() healthTracker {
	return healthTracker{
		lastRun:      make(map[string]time.Time),
		panicked:     make(map[string]bool),
		restarts:     make(map[string]int),
		intervals:    make(map[string]time.Duration),
		registeredAt: make(map[string]time.Time),
		now:          time.Now,
	}
}

func (ht *healthTracker) recordRun(name string) {
	ht.mu.Lock()
	defer ht.mu.Unlock()
	ht.lastRun[name] = ht.now()
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
	ht.registeredAt[name] = ht.now()
}

// isStale re-checks a single loop atomically to prevent TOCTOU races where
// a loop recovered between the snapshot in staleLoops() and restartLoop().
func (ht *healthTracker) isStale(name string) bool {
	ht.mu.Lock()
	defer ht.mu.Unlock()
	now := ht.now()
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
		return now.Sub(regAt) > maxAge
	}
	interval, iOk := ht.intervals[name]
	maxAge := 120 * time.Second
	if iOk && interval > 0 {
		maxAge = interval * 6
	}
	return now.Sub(lastRun) > maxAge
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
	now := ht.now()
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

// ---------------------------------------------------------------------------
// Idempotency key cleanup
// ---------------------------------------------------------------------------

// expiredIdempotencyKeysSQL returns the delete for one dialect.
//
// The predicate is expires_at, the column the insert already writes from the
// store's configured TTL (cleat#1261). It used to be `created_at < now() - 7
// days`, a Go constant that duplicated the schema default and ignored the
// configured value entirely -- so WithIdempotencyKeyTTL was computed, stored,
// indexed by idx_idempotency_expires, and read by nothing. A one-hour TTL still
// honoured a key for a week; a thirty-day one lost it after seven.
//
// No parameter: the comparison is against the database's own clock, so the
// worker's clock never enters it. That also keeps the statement portable enough
// to differ only in the clock function.
//
// ON POSTGRESQL THIS NO LONGER RUNS ON THE PLAIN POOL. idempotency_keys carried
// no policy for as long as PostgresStore.startNewRun read it before any RLS
// context existed -- migrations 031 and 061 both record that as the reason for
// declining it. cleat#1534 moved those reads onto transactions that have the
// tenant set and migration 083 gives the table its policy, at which point this
// statement, which has no tenant predicate and wants none, stops being able to
// run at all. Measured as cleat_app with no tenant set:
//
//	ERROR:  cleat.tenant_id is not set -- tenant context required for RLS-scoped query
//
// See sweepExpiredIdempotencyKeys below. MySQL and SQL Server are unchanged:
// neither has a policy on this table.
func expiredIdempotencyKeysSQL(driver string) (string, bool) {
	switch driver {
	case "postgres":
		return `DELETE FROM idempotency_keys WHERE expires_at < now()`, true
	case "mysql":
		return `DELETE FROM idempotency_keys WHERE expires_at < NOW(6)`, true
	case "mssql", "sqlserver":
		return `DELETE FROM idempotency_keys WHERE expires_at < SYSUTCDATETIME()`, true
	default:
		return "", false
	}
}

// idempotencyCleanupLoop periodically deletes idempotency keys whose expiry has
// passed.
//
// It runs on every dialect. It used to be started only under
// `if *driver == "postgres"`, and nothing removed keys on the other two
// (cleat#1256) -- so the table grew for the life of the deployment and, worse,
// a key was honoured forever there: the same client code got a fresh run on
// PostgreSQL and `already_started` from an arbitrarily old run on MySQL or SQL
// Server, with nothing in the API to say so.
//
// An unknown driver is logged once and the loop exits rather than ticking
// forever doing nothing, so a dialect added later fails loudly here instead of
// silently inheriting the old behaviour.
func idempotencyCleanupLoop(ctx context.Context, db *sql.DB, driver string, interval time.Duration) {
	// Started with `go` from two sites in main.go. cleat#1769: this ran
	// unrecovered, and it is a ticker loop doing database work, so a driver
	// panic or a nil dereference here killed the worker process.
	defer recoverBackgroundGoroutine(slog.Default(), "", "idempotency-cleanup")

	stmt, ok := expiredIdempotencyKeysSQL(driver)
	if !ok {
		slog.Warn("idempotency key cleanup not started: no delete for this driver", "driver", driver)
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if db == nil {
				continue
			}
			if err := sweepExpiredIdempotencyKeys(ctx, db, driver, stmt); err != nil {
				slog.Warn("idempotency key cleanup failed", "driver", driver, "error", err)
			}
		}
	}
}

// sweepExpiredIdempotencyKeys runs one tick's delete.
//
// POSTGRESQL runs it inside a transaction that has entered cleat_sweep, because
// migration 083 gives idempotency_keys a fail-closed policy and this statement
// is cross-tenant ON PURPOSE -- it collects every tenant's expired keys on one
// tick, which is the property cleat#1256 fixed and is worth keeping. The shape
// is migration 077's, established for the plugin tables and measured there:
//
//	idempotency_keys_tenant_isolation  TO PUBLIC       USING (tenant_id = cleat.assert_tenant_set())
//	idempotency_keys_cross_tenant      TO cleat_sweep  USING (true)
//
// SET LOCAL, so the role reverts with the transaction and cannot follow the
// connection back into the pool.
//
// A FAILURE TO ENTER THE ROLE IS REPORTED, NOT ABSORBED. The obvious
// alternative -- fall back to the plain statement when SET ROLE fails -- is the
// worst of the three options available: under the policy the fallback raises
// anyway, and on a connection that bypasses RLS it succeeds, so the fallback
// works exactly where it is not needed and fails silently into a warning
// everywhere else. The loop already logs and ticks again, so a missing grant
// shows up as a repeating, nameable error rather than a table that quietly
// stops being swept.
//
// MYSQL AND SQL SERVER keep the plain pool statement. Neither has a policy on
// this table, and SQL Server has no SET ROLE to enter even if it did -- see
// engine's markCrossTenantOnTx for why that asymmetry is structural.
func sweepExpiredIdempotencyKeys(ctx context.Context, db *sql.DB, driver, stmt string) error {
	if driver != "postgres" {
		_, err := db.ExecContext(ctx, stmt)
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SET LOCAL ROLE cleat_sweep`); err != nil {
		return fmt.Errorf(
			"could not enter cleat_sweep: %w (the connecting role needs "+
				"GRANT cleat_sweep ... WITH INHERIT FALSE; see migrations/postgres/077 and 083)",
			err)
	}
	if _, err := tx.ExecContext(ctx, stmt); err != nil {
		return err
	}
	return tx.Commit()
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

// recoverBackgroundGoroutine is withPanicRecovery for goroutines that have no
// Worker to hang off. Use it as the FIRST deferred call in the goroutine body:
//
//	go func() {
//		defer recoverBackgroundGoroutine(logger, workerID, "plugin-pool-monitor")
//		...
//	}()
//
// Two such goroutines exist: the plugin connection pool monitor, which starts in
// main() well before the Worker is constructed, and idempotencyCleanupLoop,
// which is a package-level function started with `go` from two call sites. Both
// ran unrecovered until cleat#1769; a panic in either took the process down.
//
// This deliberately does NOT touch healthTracker or Metrics. Neither exists yet
// at the pool monitor's call site, and a helper that works in one of its two
// callers is the kind of partial fix cleat#1769 is about. The log line carries
// the stack, which is what an operator needs to act.
func recoverBackgroundGoroutine(logger *slog.Logger, workerID, name string) {
	r := recover()
	if r == nil {
		return
	}
	logger.ErrorContext(context.Background(),
		"PANIC in background goroutine — this loop has stopped; the worker continues",
		"worker_id", workerID, "loop", name, "error", r, "stack", string(debug.Stack()))
}

// ---------------------------------------------------------------------------
// Worker struct and background loop methods (moved from main.go)
// ---------------------------------------------------------------------------

type Worker struct {
	id     string
	logger *slog.Logger
	store  engine.WorkflowStore

	// heartbeatStore is HeartbeatBatchFenced's own store, backed by a
	// connection pool isolated from `store`'s -- nil unless
	// --heartbeat-max-connections > 0 (main.go), in which case
	// heartbeatBatchStore() prefers it. cleat#2009 investigation: `store`'s
	// pool is shared with every claim, execution and defer-phase write
	// (main.go SetMaxOpenConns(concurrency+5)), so a burst of long-held
	// connections can queue a heartbeat write behind them with no priority
	// -- indistinguishable, from the reaper's side, from the worker being
	// genuinely gone. This closes that specific cause (pool exhaustion) the
	// same way pluginDB and flusherDB are already isolated from `store`'s
	// pool; it does not address a GC pause or a network path to the
	// database degraded for this worker only, which #2009 covers instead.
	//
	// Nil falls back to `store`, so a Worker built without one (tests, or
	// --heartbeat-max-connections=0) behaves exactly as before.
	heartbeatStore engine.WorkflowStore

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

	// taskQueues is what `store` was opened with; a tenant-scoped store has to
	// poll the same set or it would see a different slice of the work.
	taskQueues []string

	// claimAcrossTenants makes the dispatch loop claim work for every tenant in
	// one query instead of only its own. Off by default: it needs a mechanism
	// the deployment has to grant (a BYPASSRLS owner on PostgreSQL, cleat_admin
	// membership on SQL Server), and turning it on without that should be a
	// deliberate act rather than an upgrade side effect.
	claimAcrossTenants bool

	// claimTenantsPerTick bounds how many tenants one rotating tick polls.
	claimTenantsPerTick int

	// tenantRotation is the cursor the claim resumes from. Worker-local: see
	// the type's doc for why there is no shared counter table.
	tenantRotation tenantRotation

	// The two "this worker cannot serve other tenants" warnings, one per loop.
	//
	// SEPARATE, and not merged now that they say nearly the same thing: the
	// two loops reach the answer independently, so one Once covering both
	// would report whichever fired first and silence the other -- leaving an
	// operator told that cron is narrowed with nothing said about dispatch, or
	// the reverse, according to which ticked first after boot.
	rotatingSchedulesUnavailableOnce sync.Once
	rotatingUnavailableOnce          sync.Once

	concurrency       int
	maxReclaimPerTick int

	// unservableBackoff is how long a run waits before it can be claimed again
	// after a worker released it for a sibling. Zero means
	// defaultUnservableBackoff. cleat#1710.
	unservableBackoff time.Duration

	// bgPlugins are the plugins implementing plugin.HasBackground, started by
	// Run once the health tracker and metrics exist. cleat#1347.
	bgPlugins []plugin.HasBackground

	// finalizeObservers are the plugins implementing plugin.HasFinalizeObserver
	// -- discovered the same way as bgPlugins, above. Called after a run
	// reaches "done" or "failed"; see that interface's doc comment for what
	// the hook does and does not guarantee. cleat#1715.
	finalizeObservers []plugin.HasFinalizeObserver

	// bgWg is main()'s waitgroup, NOT w.wg, and the difference is load
	// bearing. w.wg is awaited by Run itself with no timeout; bgWg is awaited
	// after Run returns, under a 30-second cap. A plugin loop is third-party
	// code that may not honour ctx promptly, and putting one on w.wg would let
	// it block shutdown indefinitely.
	bgWg                 *sync.WaitGroup
	maxQueued            int
	heartbeatInterval    time.Duration
	reclaimTimeout       time.Duration // 0 = derive from heartbeatInterval; see reclaimAfter
	flushRetryWindow     time.Duration // 0 = engine.DefaultFlushRetryWindow; see --flush-retry-window
	pollInterval         time.Duration
	pluginRegistry       *engine.PluginRegistry
	pluginStreamRegistry *engine.PluginStreamRegistry

	// streamHub is the worker-local live tail of plugin stream chunks, shared
	// between the engines this worker builds and the SSE route that serves
	// them. cleat#1572.
	streamHub *engine.StreamHub

	tenantPools *plugin.TenantPools
	plugList    []*plugin.LoadedPlugin

	// privateHosts is the operator's --plugin-egress-allow-private set, carried
	// to every dbServiceCaller.
	privateHosts *pluginPrivateHosts

	// egress is a test-supplied guard, mirroring dbServiceCaller.egress and
	// carrying the same caveat: production never sets it, and
	// TestNoProductionCodeAllowsLoopbackEgress keeps it that way. It exists
	// because a test driving executeWorkflow builds a Worker rather than a
	// caller, so the caller's own field is out of reach from there.
	egress *engine.EgressGuard

	// serviceEndpoints maps a service name to its base URL, from
	// --service-endpoints. Passed to every dbServiceCaller this worker builds.
	serviceEndpoints map[string]string

	// egressAllow answers "which hosts may this tenant's workflows reach".
	// Nil denies every guest-initiated fetch. cleat#1565.
	egressAllow *engine.TenantEgressStore

	// operatorEgress answers "which hosts may this DEPLOYMENT reach at all".
	// Egress needs both this and the tenant's permission. cleat#1565.
	operatorEgress *engine.HostAllowlist

	// secrets resolves ${secret:name} in a DurableCall's request, on the way
	// out to the service. Nil leaves references as literal text -- the same
	// pass-through a worker started without CLEAT_SECRET_MASTER_KEY has always
	// had for plugins.
	secrets *engine.SecretStore

	// Worker membership, and this worker's slice of the cluster connection
	// budget. cleat#1487.
	//
	// The registry and the membership loop exist on EVERY worker, because the
	// registry is also how a writer learns which secret keys the live workers
	// can open (cleat#1991). The connection share, and the budget fields that
	// feed it, are nil or zero unless --cluster-connection-budget is set: a
	// worker that was upgraded must not start resizing its pools against a
	// budget nobody configured -- the same opt-in rule the per-worker budget
	// follows.
	workerRegistry *engine.WorkerRegistry
	// membershipLastBeat is when the last membership tick succeeded, or, until the
	// first one does, when this worker registered (main sets it; a zero value would
	// mean a slow boot is never a lapse -- cleat#2167). Read and written only by the
	// membership loop's goroutine once that starts.
	membershipLastBeat        time.Time
	connectionShare           *connectionShare
	connectionBudgetParts     connectionBudget
	clusterConnectionBudget   int
	perWorkerConnectionBudget int

	ctx      context.Context
	cancel   context.CancelFunc
	draining atomic.Bool
	wg       sync.WaitGroup

	inflight    sync.Map // map[workflowID]*engine.WorkflowInstance
	execEngines sync.Map // map[workflowID]*engine.Engine
	wasmCache   *wasmLRUCache

	// execCancel cancels one execution's own context, independent of every
	// other execution and of w.ctx. Still done for whatever DOES respect a
	// cancelled context between host calls -- checksum verification and
	// other engine-side reads that take Replay's own ctx argument directly --
	// but it is NOT what stops the next durable call; see fencedRuns below
	// for why and what does.
	execCancel sync.Map // map[workflowID]context.CancelFunc

	// fencedRuns is set for a workflow ID the moment its fenced heartbeat
	// reports the run lost, and is what freshCall actually refuses on via
	// WithCanStartNewWork.
	//
	// NOT execCtx cancellation, despite that being the first mechanism this
	// shipped with and despite a comment at this file's Replay call site
	// having claimed for a time that cancelling execCtx "is what actually
	// stops a fenced-out execution from making one more outbound call".
	// Measured 2026-09-22 with TestAFencedExecutionMakesNoFurtherCallsAndASecondWorkerFinishesTheWorkflow
	// (cmd/cleat-worker/fenced_execution_acceptance_test.go): a real guest's
	// SECOND DurableCall, issued after execCancel() had already fired for
	// its run, still reached the service. engine/wasmtime_hostfuncs.go's
	// registerCleatCall builds every host call's context from
	// context.Background() (`ctxWithMem(context.Background(), buf)`), not
	// from whatever ctx Replay was given -- wasmtime's Go bindings cannot
	// observe a live context's cancellation mid fn.Call (see this repo's
	// CLAUDE.md on wazero's identical limitation), so nothing durable_call's
	// dispatch reaches was ever going to see execCtx.Done() close. A unit
	// test that hands freshCall an already-cancelled context directly
	// (engine/can_start_new_work_test.go) cannot catch this, because it
	// skips exactly the layer that drops the context.
	//
	// canStartNewWork is a plain Go function call, not a context, so it has
	// no such gap -- decision 2 (worker-wide presumed loss) was never
	// affected by this. fencedRuns reuses that same proven call site for
	// decision 1, scoped to the one run rather than every run this worker
	// holds.
	fencedRuns sync.Map // map[workflowID]struct{}

	// lastHeartbeatOK is the UnixNano of the last heartbeatAndFenceInFlight
	// call that could actually ask the store -- a successful
	// HeartbeatBatchFenced, including the trivial case of zero in-flight
	// runs. cleat#2008 decision 2: an ERROR from that call does not advance
	// it, so heartbeatPresumedLost() below turns from false to true only
	// after heartbeats have been failing continuously for reclaimAfter(),
	// not on the first blip -- the same threshold ReapStaleInstances uses to
	// decide another worker may take this run.
	//
	// An atomic.Int64 rather than a mutex-guarded time.Time: it is written by
	// the single heartbeat loop goroutine and read by every execution
	// goroutine on every fresh durable call, so a lock-free read matters more
	// here than it does for execCancel above (Store/Load, rarely on the hot
	// path).
	//
	// Initialized in newWorker (or by the test constructors) to the worker's
	// start time -- never the zero Time -- so a worker whose heartbeat loop
	// has not ticked even once yet is not immediately presumed lost before
	// it has had any chance to prove otherwise.
	lastHeartbeatOK atomic.Int64

	// lastDBTrouble is the UnixNano of this worker's most recent database
	// round trip -- on the heartbeat OR the reaper path -- that failed or
	// did not return before dbCallDeadline. The reaper's own counterpart to
	// lastHeartbeatOK above, guarding a different decision with the OPPOSITE
	// default. heartbeatPresumedLost defaults optimistic (false, from
	// lastHeartbeatOK seeded to now): a delayed refusal to accept new work
	// is recoverable, and presuming loss too early costs nothing but idle
	// capacity. This defaults pessimistic instead: reclaiming a run out from
	// under a worker that is actually still alive is not recoverable by
	// trying again later, so a worker that has not yet PROVEN a full
	// reclaimAfter() window of clean database contact must not act on
	// anyone else's staleness. Seeded to the worker's own start time (see
	// newWorker and the test constructors), not the zero value, for exactly
	// that reason: a freshly started worker has demonstrated nothing about
	// its own database contact yet, and must earn the same window any other
	// worker's heartbeat would have to lapse before being called dead.
	// cleat#2005.
	lastDBTrouble atomic.Int64

	// lastDBContactOK is the UnixNano of this worker's most recent CONFIRMED
	// successful, bounded database round trip -- heartbeat (busy, via
	// HeartbeatBatchFenced, or idle, via a DBPinger probe) or a successful
	// ReapStaleInstances. reapingIsSafe requires this to be recent, not just
	// lastDBTrouble to be old: those are different questions.
	// lastDBTrouble absent means "nothing has been recorded as having gone
	// wrong," which an idle worker with no DBPinger-capable store, or one
	// whose ticker simply has not fired yet, satisfies trivially without
	// ever having checked anything. lastDBContactOK absent means "nothing
	// has been recorded as having gone RIGHT," which cannot be satisfied by
	// silence -- only by an actual round trip returning success. cleat#2005's
	// follow-up review: a worker with zero in-flight runs made no database
	// call at all in heartbeatAndFenceInFlight's old early-return path, so
	// its lastDBTrouble stayed clean throughout a stall it never observed,
	// and its reaper reclaimed a run whose true holder was alive the whole
	// time. See reapingIsSafe.
	//
	// Left at its atomic.Int64 zero value (1970) rather than seeded in
	// newWorker: that is already the pessimistic starting point this needs
	// -- "no confirmed contact yet" -- so seeding it would only be able to
	// make it LESS pessimistic, which is the wrong direction for the same
	// reason lastDBTrouble is seeded to now rather than to the zero value.
	lastDBContactOK atomic.Int64

	// consecutiveReapSkips counts reaper ticks in a row that skipped because
	// reapingIsSafe() was false, reset to 0 by any tick that actually calls
	// ReapStaleInstances (whether or not it reclaims anything). Purely for
	// observability: reapOnce warns once this crosses
	// reapSkipWarningThreshold, so a link that is chronically slow or
	// unreliable -- which keeps the grace period from ever clearing, see
	// reapingIsSafe's doc -- is visible as "the reaper is not running," not
	// silent. cleat#2005 follow-up review, GAP3.
	consecutiveReapSkips atomic.Int64

	// stallEpisodes tracks cleat#2006's suspected-database-stall suppression
	// state, one *stallSuppressionEpisode per reap unit -- keyed by shard
	// name for a MultiShard store, or by "" for a plain one. A sync.Map
	// rather than a plain map because reapOnce runs on its own loop
	// goroutine but shard names are not known until the store exists, so
	// there is no single point to pre-size a guarded map at construction.
	stallEpisodes sync.Map

	scheduleMu       sync.Mutex
	scheduleInterval time.Duration

	// Backpressure / circuit breaker state.
	consecutiveDBErrors int
	backoffUntil        time.Time
	circuitOpen         atomic.Bool

	// Compaction settings.
	Metrics                 *prometheus.Metrics
	compactionThreshold     int
	compactionInterval      time.Duration
	retentionInterval       time.Duration
	stallThreshold          time.Duration
	metricsSweepInterval    time.Duration
	keyExpiryWindow         time.Duration
	deadLetterRetentionDays int

	// Version GC. versionGCInterval is the switch: 0 means the sweep never
	// runs and GC is reachable only through cleatctl or the HTTP endpoint.
	// cleat#1315.
	versionGCInterval    time.Duration
	versionGCMinVersions int
	versionGCMaxAge      time.Duration

	memoryController                 *MemoryController
	maxRetries                       int
	memorySampleRetention            int
	retentionDays                    int
	completedWorkflowRetentionDays   int
	schemaName                       string
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
	maxWorkflowDuration  time.Duration
	wasmWallClockCeiling time.Duration
	hostRetryBudget      time.Duration

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
	w.initLoopCtxUnmonitored(name)
	w.healthTracker.registerLoop(name)
}

// initLoopCtxUnmonitored is initLoopCtx without the health-tracker
// registration -- for a loop that launchLoop always starts (so it still
// needs a loopCtxMap entry, or launchLoop's map lookup panics on a nil
// *loopContext), but that may permanently no-op depending on configuration.
//
// cleat#2004: versionGCLoop is launched unconditionally, and returns at once
// when --version-gc-interval is 0, the default -- before ever calling
// setInterval or recordRun. initLoopCtx's unconditional registerLoop still
// ran, so healthTracker.staleLoops() flagged a loop that was never going to
// run again, and /healthz turned 503 about two minutes after every default
// worker started (maxAge with no interval set is 120s). versionGCLoop calls
// w.healthTracker.registerLoop itself, at the same point it already calls
// setInterval -- once it has confirmed it is actually going to keep running,
// not before. A loop that is registered without ever running is exactly the
// state a health tracker exists to complain about; the bug was registering
// one that had already decided not to.
func (w *Worker) initLoopCtxUnmonitored(name string) {
	ctx, cancel := context.WithCancel(w.ctx)
	lc := &loopContext{
		ctx:    ctx,
		cancel: cancel,
		done:   make(chan struct{}),
	}
	w.loopMu.Lock()
	w.loopCtxMap[name] = lc
	w.loopMu.Unlock()
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

	// Here rather than at the end of Run, and rather than in main() where it
	// used to be. It has to be after the health tracker exists, and it should
	// be as early after that as possible: the loops previously started ~124
	// lines earlier in main(), so they had a head start over the dispatch loop.
	// Starting them before the worker loops launch keeps that ordering as close
	// to what it was as the health tracker allows. cleat#1347.
	w.startPluginBackground()

	initLoopCtx := w.initLoopCtx
	initLoopCtx("heartbeat")
	initLoopCtx("reaper")
	initLoopCtx("concurrency_key_reaper")
	// tenant_pool_reaper's initLoopCtx moved below, next to the same
	// w.hasReapablePools() guard that decides whether it is registered and
	// launched at all -- see that guard's comment, and cleat#2004.
	initLoopCtx("dispatch")
	initLoopCtx("schedule")
	initLoopCtx("memory_reload")
	initLoopCtx("memory_cleanup")
	initLoopCtx("retention")
	// version_gc is launched unconditionally below but no-ops when
	// --version-gc-interval is 0, the default -- initLoopCtxUnmonitored
	// still gives launchLoop the context entry it needs without telling the
	// health tracker to expect a run. versionGCLoop registers itself once it
	// knows it is actually going to keep running. cleat#2004.
	w.initLoopCtxUnmonitored("version_gc")
	initLoopCtx("compaction")
	if *metricsSweepInterval > 0 {
		initLoopCtx("metrics_sweep")
	}
	// Conditional on the same thing that launches it below. A loop launched
	// without its context has no entry in getLoopCtx, so it can never be
	// cancelled or restarted by the watchdog -- which is what
	// TestEveryPreparedLoopIsLaunched caught here.
	if w.workerRegistry != nil {
		initLoopCtx("worker_membership")
	}

	// Background heartbeat goroutine.
	w.registerLoopFunc("heartbeat", w.heartbeatLoop)
	w.launchLoop("heartbeat", w.heartbeatLoop)

	// Worker membership and the cluster connection share. cleat#1487.
	//
	// Launched on every worker, since the registry carries the secret keys it
	// can open (cleat#1991). Only the connection share is opt-in, and the loop
	// leaves it alone when --cluster-connection-budget is not set.
	if w.workerRegistry != nil {
		w.registerLoopFunc("worker_membership", w.workerMembershipLoop)
		w.launchLoop("worker_membership", w.workerMembershipLoop)
	}

	// Background zombie reaper goroutine.
	w.registerLoopFunc("reaper", w.reaperLoop)
	w.launchLoop("reaper", w.reaperLoop)

	// Background concurrency key reaper goroutine (Feature 5).
	w.registerLoopFunc("concurrency_key_reaper", w.concurrencyKeyReaperLoop)
	w.launchLoop("concurrency_key_reaper", w.concurrencyKeyReaperLoop)

	// Background tenant-pool reaper. cleat#1470.
	//
	// LAUNCHED only when there is something to reap. There are two independent
	// sources of per-tenant pools and a worker can have either, both or
	// neither:
	//
	//   - plugin.TenantPools, built solely under --tenant-isolation=role,
	//     which is PostgreSQL-only.
	//   - the store factory itself, on MySQL and SQL Server, where a tenant's
	//     RLS or database makes a shared pool impossible. cleat#1928.
	//
	// The second used to have no reaper at all, so a MySQL or SQL Server worker
	// held a pool for every tenant it had ever served until the process ended
	// -- the asymmetry was invisible from either side, because each of the two
	// looked complete on its own.
	//
	// A loop that ticks forever over neither is a health-tracked goroutine
	// reporting success for doing nothing, which is why this is still a guard
	// rather than an unconditional launch.
	//
	// initLoopCtx is INSIDE the guard, with registerLoopFunc and launchLoop --
	// all three, or none. TestEveryPreparedLoopIsLaunched caught the version of
	// this with the launch inside the guard and no initLoopCtx at all: the
	// loop ran with no entry in the loop-context map, so the watchdog could
	// neither cancel nor restart it. "Preparing a context for a loop that is
	// not launched is harmless" used to be this comment's conclusion, and
	// cleat#2004 found the half that isn't: initLoopCtx also registers the
	// loop with the health tracker, so a worker with no reapable pools
	// registered a loop that would never run and never recordRun, and
	// staleLoops() flagged it forever once maxAge (120s, no interval ever
	// set) passed -- /healthz turned 503 about two minutes after every such
	// worker started. Moving initLoopCtx inside the same guard as the launch
	// it was already conditioned on keeps the watchdog's invariant
	// (never launch without a context) and drops the health tracker's false
	// positive: unregistered is the correct state for a loop that is not
	// running, same as metrics_sweep and worker_membership above, which
	// already gate their initLoopCtx this way.
	if w.hasReapablePools() {
		initLoopCtx("tenant_pool_reaper")
		w.registerLoopFunc("tenant_pool_reaper", w.tenantPoolReaperLoop)
		w.launchLoop("tenant_pool_reaper", w.tenantPoolReaperLoop)
	}

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
	w.registerLoopFunc("retention", func() { w.retentionLoop(w.retentionDays, w.completedWorkflowRetentionDays, w.deadLetterRetentionDays) })
	w.launchLoop("retention", func() { w.retentionLoop(w.retentionDays, w.completedWorkflowRetentionDays, w.deadLetterRetentionDays) })

	// Version GC. Separate loop rather than a fourth arm of retentionLoop:
	// its interval is its own flag, it is off by default while --retention-days
	// is on, and the health tracker reports per-loop -- folding it in would
	// make "retention is running" mean two different things.
	w.registerLoopFunc("version_gc", w.versionGCLoop)
	w.launchLoop("version_gc", w.versionGCLoop)

	// Metrics sweep loop.
	//
	// Same shape as cleat#877 below, one layer out: the four queries this
	// publishes were written, an interface to reach them was declared in
	// metrics_store.go -- and nothing ever called it. Everything existed
	// except the caller, so cleat_workflows_stuck, the event-history gauges
	// and the concurrency-key gauge emitted no series at all. An OTel gauge
	// never Set is absent rather than zero, so an alert on any of them could
	// never fire. cleat#1317.
	if w.metricsSweepInterval > 0 {
		w.registerLoopFunc("metrics_sweep", w.metricsSweepLoop)
		w.launchLoop("metrics_sweep", w.metricsSweepLoop)
	}

	// Compaction loop.
	//
	// This line did not exist until cleat#877, and its absence was the whole
	// defect: compactionLoop has been defined and never started since it was
	// written -- at 0d006730^ it was likewise defined and never called, so
	// history compaction had NEVER run in any deployment.
	//
	// Everything around it was already here, which is what made it invisible:
	// the --compaction-threshold and --compaction-interval flags, both plumbed
	// through main.go; GetCompactionCandidates on all three dialects;
	// CompactWorkflowHistory; extractCompactionState's fuzz test;
	// compactionLoop's own unit tests; the health tracker's "compaction"
	// interval; and initLoopCtx("compaction") three lines up, preparing a
	// context for a goroutine nothing spawned.
	w.registerLoopFunc("compaction", w.compactionLoop)
	w.launchLoop("compaction", w.compactionLoop)

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

// startPluginBackground starts each plugin's background loop, here rather than
// in main(), so that a panic in one reaches the health tracker and the
// background-loop metric. cleat#1347.
//
// They were started 124 lines before the *Worker existed, which put them
// outside all three of withPanicRecovery, healthTracker.recordPanic and
// Metrics.RecordBackgroundLoop -- so a panicking plugin loop stopped that
// plugin's background work, logged, and was invisible to /healthz and to
// monitoring. An operator found out by reading logs.
//
// DELIBERATELY NOT REGISTERED WITH initLoopCtx OR THE WATCHDOG, and this is
// the part that needs stating rather than looking like an omission.
//
// Staleness is a heartbeat model: isStale falls back to registeredAt when a
// loop has no lastRun entry and calls it stale after 120s, and recordRun is
// what clears it. plugin.HasBackground.Run(ctx) has no access to the health
// tracker, so it can never call recordRun -- a registered plugin loop would be
// permanently stale from 120 seconds after startup however healthy it is.
// With --health-check-interval defaulting to 30s the watchdog would then
// cancel its context and restart it twice a minute, forever. A plugin that
// sleeps between iterations -- scheduledbackup, scheduler -- would be
// interrupted mid-sleep and re-entered, which is a worse failure than the
// blindness this fixes, arriving as the fix.
//
// So a stopped plugin loop is still not restarted. Making that work needs
// plugins to be able to heartbeat, which is an addition to plugin.Environment
// and an obligation on plugin authors: a design decision, not a follow-through.
//
// The metric name is prefixed rather than bare, so a plugin cannot collide
// with a worker loop -- "dispatch" is taken.
func (w *Worker) startPluginBackground() {
	for _, bg := range w.bgPlugins {
		bg := bg
		name := "plugin:" + bg.Info().Name
		if w.bgWg != nil {
			w.bgWg.Add(1)
		}
		go func() {
			if w.bgWg != nil {
				defer w.bgWg.Done()
			}
			defer func() {
				if r := recover(); r != nil {
					w.healthTracker.recordPanic(name)
					if w.Metrics != nil {
						w.Metrics.RecordBackgroundLoop(context.Background(), name, "panic")
					}
					w.logger.ErrorContext(context.Background(),
						"PANIC in plugin background worker — this plugin's background work has stopped; the worker continues",
						"worker_id", w.id, "plugin", bg.Info().Name,
						"error", r, "stack", string(debug.Stack()))
				}
			}()
			if err := bg.Run(w.ctx); err != nil {
				if w.Metrics != nil {
					w.Metrics.RecordBackgroundLoop(context.Background(), name, "error")
				}
				w.logger.ErrorContext(context.Background(), "plugin background worker exited",
					"worker_id", w.id, "plugin", bg.Info().Name, "error", err)
			}
		}()
	}
}

// maxIdleTicks caps the dispatch loop's progressive idle backoff at
// maxIdleTicks * pollInterval.
//
// Package-scoped rather than function-local so a test can bind to the real
// value instead of a copy: TestTheRunnableCountIsAskedOnlyAtFullBackoff asserts
// the runnable count is asked exactly once per idle streak, which is only
// meaningful against the same number the loop uses.
const maxIdleTicks = 6

func (w *Worker) dispatchLoop() {
	defer w.wg.Done()
	w.healthTracker.setInterval("dispatch", w.pollInterval)

	// Keep the global time seed fresh for workflow sessions.
	engine.UpdateNowMs()

	const maxBatchSize = 20 // cap claims per query to avoid oversized batches
	idleTicks := 0

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
		for key, bytes := range w.memoryController.DefEstimates() {
			w.Metrics.RecordWorkflowMemoryEstimate(w.ctx, key.tenantID, key.defName, bytes)
		}
		w.Metrics.SetQueueDepth(w.ctx, state.QueueDepth)

		// BYTE-cache occupancy, on the same tick as the other gauges. It is
		// the observable for --wasm-cache-max-entries and --wasm-cache-max-mb,
		// which an operator otherwise tunes blind.
		//
		// NOT THE COMPILED-MODULE CACHE, though this comment said "compiled-
		// module cache" until cleat#1563 named the confusion. w.wasmCache is a
		// *wasmLRUCache holding WASM BYTES with LRU eviction; the compiled
		// wasmtime Modules live in a separate process-wide sync.Map
		// (engine/backend_wasmtime.go:70) which has no eviction, no bound and
		// no metric at all. The gauge names do not distinguish them --
		// cleat_wasm_cache_entries is described as "the WASM module cache" --
		// so an operator watching this gauge is not watching the cache that
		// grows without limit. cleat#1563 tracks that gap; this comment only
		// stops claiming to cover it.
		if w.wasmCache != nil {
			ents, cbytes := w.wasmCache.stats()
			w.Metrics.SetWasmCacheEntries(w.ctx, int64(ents))
			w.Metrics.SetWasmCacheBytes(w.ctx, cbytes)
		}

		// And the cache the comment above says these gauges do NOT cover.
		// cleat#1563 bounded it; this is what makes the bound observable, so an
		// operator tuning --wasm-module-cache-max-entries is not doing it blind.
		if c, ok := w.wasmtimeBackend.(interface{ CompiledModuleCacheEntries() int }); ok {
			w.Metrics.SetWasmCompiledModuleCacheEntries(w.ctx, int64(c.CompiledModuleCacheEntries()))
		}
		if c, ok := w.wasmtimeBackend.(interface{ CompiledModuleCacheBytes() int64 }); ok {
			w.Metrics.SetWasmCompiledModuleCacheBytes(w.ctx, c.CompiledModuleCacheBytes())
		}
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

			// A zero claim has two meanings and this loop cannot tell them
			// apart: ClaimWorkflows uses FOR UPDATE SKIP LOCKED (READPAST on
			// SQL Server), so a locked row is REMOVED from the result set
			// rather than blocking. "Nothing to run" and "every candidate was
			// locked" arrive here identically, and the second backs off to 6x
			// while the work sits there. IMPROVEMENT-PLAN 3.249 and 3.250.
			//
			// Asking costs a query, so it is asked ONLY once fully backed off.
			// That is the only state where the answer changes what anyone would
			// do -- a briefly-idle worker does not need to know, one that has
			// been at 6x for minutes with runnable rows does -- and at that
			// point it is one query per six poll intervals rather than one per
			// poll, on the loop's most common path.
			//
			// Deliberately NOT a behavioural change. Measured 2026-09-07,
			// claim-vs-claim contention never produces this at any ratio: SKIP
			// LOCKED skips, so with more candidates than lockers it takes the
			// next free row. It needs a long-held lock, and nothing in the
			// engine holds one today. Resetting the backoff here would add a
			// mechanism against a condition nothing currently produces; saying
			// so out loud costs a log line and catches the day that changes.
			if idleTicks == maxIdleTicks {
				if runnable, cErr := w.store.CountRunnableWorkflows(w.ctx); cErr != nil {
					w.logger.DebugContext(w.ctx, "could not count runnable workflows while backed off",
						"worker_id", w.id, "error", cErr)
				} else if runnable > 0 {
					w.logger.WarnContext(w.ctx,
						"backed off to the maximum poll interval while runnable work exists; "+
							"the claim is losing every race for these rows, which means something is "+
							"holding a lock on them across poll intervals",
						"worker_id", w.id,
						"runnable", runnable,
						"backoff", sleep,
						"poll_interval", w.pollInterval)
				}
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

		// Per definition, not per batch. A claim returns whatever is queued --
		// sticky and general work for any mix of definitions -- so a single
		// Add(len(wfs)) cannot carry a definition label, and cleat_workflows_claimed_total's
		// dashboard panel drew one undifferentiated line (cleat#1444). Grouping
		// here is what gives the panel something to group BY.
		claimedByDef := make(map[string]int64, len(wfs))
		for _, wf := range wfs {
			claimedByDef[wf.DefName]++
		}
		for defName, n := range claimedByDef {
			w.Metrics.RecordWorkflowsClaimed(w.ctx, n, defName)
		}

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
	defer w.fencedRuns.Delete(wf.ID)

	// cleat#2008: this execution's OWN context, cancelled independently of
	// every other execution and of w.ctx, the moment its fenced heartbeat
	// reports the fence lost. Kept for whatever engine-side code between host
	// calls DOES take this ctx directly (checksum verification and similar --
	// see executeWithBackend). It does NOT stop the guest from making another
	// durable call; fencedRuns above does that, via WithCanStartNewWork below,
	// because wasmtime's host-call dispatch never sees this context at all
	// (see fencedRuns' doc comment for the measurement). Registered before
	// anything else so there is no window where a freshly-claimed run is in
	// w.inflight (and so eligible to be heartbeat) but has no cancel func for
	// the heartbeat loop to call.
	execCtx, execCancel := context.WithCancel(w.ctx)
	w.execCancel.Store(wf.ID, execCancel)
	defer w.execCancel.Delete(wf.ID)
	defer execCancel()

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
				// Persist through the workflow's OWN tenant store, not the
				// controller's. The controller holds the worker's single
				// store; this worker runs workflows for any tenant it can
				// claim. Resolved here rather than captured from execStore
				// below because this defer is registered before that
				// assignment and must also record for a workflow whose
				// execution failed. The repeat lookup is a map read plus a
				// struct; the pool it leases is already open, because the
				// execution's own lease has not dropped yet. See cleat#1040.
				memStore, memRelease, memStoreErr := w.storeForTenant(wf.TenantID)
				defer memRelease()
				if memStoreErr != nil {
					// Recording a sample is not worth failing anything over,
					// and the execution path above has already reported this
					// tenant's store as unavailable with more context.
					memStore = nil
				}
				w.memoryController.RecordWorkflowMemory(context.Background(), memStore, wf.TenantID, wf.DefName, delta)
			}
		}
	}()

	// ---- Tenant-scoped store ----
	//
	// EVERY store call below this line goes through execStore, not w.store.
	// Not "the writes" and not a list of them -- every call, reads included.
	// This comment used to enumerate: "event history, state, child workflows,
	// schedules". The enumeration was the defect. loadWASM is a READ, so it
	// was not on the list, it was not routed, and because LoadWASM's statement
	// carries no tenant predicate of its own it came back empty for every
	// tenant but the worker's -- failing those runs terminally while their
	// history was being written correctly one line away (cleat#1931).
	//
	// Routing on wf.TenantID is what lets a worker run a tenant other than the
	// one its dispatch loop is scoped to -- and because the store comes from
	// OpenStore(wf.TenantID), it is correct by construction rather than by a
	// check downstream. Anything cached in front of such a call has to carry
	// the tenant in its key for the same reason; a cache is consulted before
	// the store and will answer ahead of it.
	execStore, execRelease, storeErr := w.storeForTenant(wf.TenantID)
	// THE LEASE SPANS THE WHOLE EXECUTION, and this is the call site the lease
	// exists for. Every other holder of a tenant store is bounded -- a claim
	// tick, a schedule fire, an HTTP request -- but a workflow can sit inside
	// one activity for an hour without touching the database, so "nothing has
	// opened a store for this tenant recently" says nothing about whether one
	// is in use. Releasing here and re-resolving per statement would reopen
	// that window; holding it for the run closes it.
	defer execRelease()
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
	//
	// THIS MEASURES A LOAD AND USED TO FEED THE COMPILE METRIC. loadWASM checks
	// the in-memory cache, then the disk cache, then the database, and returns
	// []byte -- it compiles nothing. So cleat_wasm_compile_duration_seconds
	// ("WASM compile duration by def_name") has been carrying storage latency,
	// and cleat_wasm_load_latency_seconds ("Time to load a WASM module from
	// storage"), which is exactly this quantity, was never fed at all
	// (cleat#1317).
	//
	// An operator alerting on compile duration was therefore watching the
	// database and the disk cache. Not a missing metric -- a confident wrong
	// one, which is the worse of the two because it invites action.
	//
	// Real compilation is Runtime.CompileModule (engine/runtime.go:209), which
	// lives in a package with no Metrics handle. Feeding the compile metric
	// from here as well would publish one measurement under two names, which is
	// the defect cleat#1317 already found in SetMemoryPressureRatio.
	wasmStart := time.Now()
	wasmBytes, err := w.loadWASM(execStore, w.cacheTenantFor(wf.TenantID), wf.DefName, wf.DefVersion)
	w.Metrics.RecordWasmLoadLatency(context.Background(), time.Since(wasmStart), wf.DefName)
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
	entryPoint, entryPointErr := determineEntryPoint(wf.Input, wasmBytes)
	if entryPointErr != nil {
		w.recordTerminalFailure(wf, workflowStartTime, entryPointErr.Error(), engine.ErrPermanent.String(), "")
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
	// engine.WasmtimeLanguages, so the engine needs no Runtime for any
	// supported guest.
	//
	// A module declaring an unsupported language in its own cleat.metadata used
	// to reach Replay's legacy path and dereference that nil Runtime, panicking
	// the workflow. Engine.resolveBackend now rejects it with an error naming
	// the language.

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
	// wfMeta comes from the binary THIS worker loaded, so a mismatch says this
	// worker's binary disagrees with the def row -- not that the run is bad.
	// During a rolling deploy that is the expected transient state, and a
	// freshly-deployed sibling resolves it. cleat#1710.
	if wfMeta != nil && wfMeta.WorkflowVersion != wf.DefVersion {
		err := fmt.Errorf(
			"version mismatch: workflow instance %s expects def_version %d but the WASM binary THIS worker loaded reports version %d (def=%s). This worker's binary and the workflow_defs row are out of sync; another worker may have the right one.",
			wf.ID, wf.DefVersion, wfMeta.WorkflowVersion, wf.DefName)
		w.releaseForAnotherWorker(wf, err.Error(), "version_check")
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
		// workerPlugins is what THIS process linked in. A pool mid-rollout is
		// heterogeneous by definition, so an unsatisfied dep means this worker
		// is the wrong one, not that the run is unrunnable. cleat#1710.
		if err := checkPluginDeps(workerPlugins, wfMeta.PluginDeps); err != nil {
			w.releaseForAnotherWorker(wf, err.Error(), "plugin_check")
			return
		}
	}

	// Extract child binding policy from WASM metadata for the engine.
	var childBindingPolicy string
	if wfMeta != nil {
		childBindingPolicy = wfMeta.ChildBindingPolicy
	}

	// egressAllow is what makes http.fetch usable at all: without it the guard
	// has no allowlist and refuses every destination. cleat#1565.
	caller := &dbServiceCaller{
		store:            execStore,
		workerID:         w.id,
		benchSvcURL:      *benchSvcURL,
		serviceEndpoints: w.serviceEndpoints,
		egressAllow:      w.egressAllow,
		operatorEgress:   w.operatorEgress,
		privateHosts:     w.privateHosts,
		egress:           w.egress,
		traceID:          traceID,
		secrets:          w.secrets,
	}
	engineOpts := []engine.EngineOption{
		engine.WithSignalStore(execStore.(engine.SignalStore)),
		// The promise store, which the worker never wired.
		//
		// Everything else existed: the workflow_promises table, its
		// migrations, and PostgresStore's full PromiseStore implementation.
		// Only this line was missing, and every promise path checks
		// `s.engine.promiseStore != nil` and quietly does nothing when it is.
		// So CreatePromise recorded its event, skipped the insert and returned
		// SUCCESS; AwaitPromise then found no row, fell past both the resolved
		// and rejected branches, and suspended forever. Zero rows had ever been
		// written to workflow_promises.
		//
		// IMPROVEMENT-PLAN 3.218 already fixed this hang for the case where the
		// store returns an error -- "the failure was not unreportable, it was
		// unreported". The nil-store path produces the identical hang and was
		// left reporting success, which is the half of that fix that could not
		// be seen: WithPromiseStore was called from cleat/wasmtest and from a
		// unit test, and from nothing that ships, so every test had a promise
		// store and the worker never did.
		engine.WithPromiseStore(execStore.(engine.PromiseStore)),
		// The update store. Without this DurablePollUpdate finds nothing and a
		// workflow never sees an update -- which is what every environment did
		// until updates were implemented end to end.
		//
		// Type-asserted rather than guarded, deliberately, and for the reason
		// the promise store above records: a nil store here is indistinguishable
		// from "no updates pending", so a missing wire-up would be silent. Every
		// WorkflowStore satisfies UpdateStore -- all four of its methods are on
		// WorkflowStore already -- so this cannot fail at runtime without the
		// store itself having changed shape, and then it should be loud.
		engine.WithUpdateStore(execStore.(engine.UpdateStore)),
		engine.WithWorkflowState(&dbWorkflowState{version: wf.DefVersion, minVersion: wf.MinVersion, priority: wf.Priority, childVersions: childVersions}),
		engine.WithWorkflowID(wf.ID),
		// Anchors the session clock when the workflow has no history yet.
		// Without it a workflow whose first durable operation is a sleep has
		// nothing to measure its deadline from, so the deadline is re-seeded
		// from the wall clock every segment and never arrives -- it wakes,
		// re-executes, and re-suspends forever. IMPROVEMENT-PLAN 3.67.
		engine.WithWorkflowStartTime(wf.CreatedAt.UnixMilli()),
		// B4: the same claim identity (w.id, wf.Generation) every terminal
		// write below (ContinueAsNew, FinalizeWorkflowSegment) already
		// fences on. Without these two, Engine.fencingEnabled() is false and
		// the per-step flush / write-ahead-intent paths stay unfenced, which
		// was the whole finding -- so this is not optional wiring.
		engine.WithWorkerID(w.id),
		engine.WithGeneration(wf.Generation),
		// cleat#2008: refuse fresh durable calls once this worker can no
		// longer vouch for them -- decision 1, this run's own fenced
		// heartbeat already reporting it lost (runIsFenced), or decision 2,
		// this worker's heartbeats failing longer than the reclaim window
		// worker-wide (heartbeatPresumedLost). See fencedRuns' doc comment
		// for why decision 1 has to be checked here rather than left to
		// execCtx cancellation.
		engine.WithCanStartNewWork(func() bool {
			return !w.heartbeatPresumedLost() && !w.runIsFenced(wf.ID)
		}),
		// cleat#2020: w.ctx is cancelled on SIGINT/SIGTERM (main.go's signal
		// handler) and on the watchdog's poison-pill exit (w.cancel() in
		// watchdogLoop) -- a real, already-firing shutdown signal that never
		// reached durablecalls.go before, because every wasmtime host call's
		// own ctx is context.Background()-derived rather than descended from
		// this one. WithCanStartNewWork above gates the START of a fresh
		// call; this lets a call already WAITING (a backoff, a scheduled
		// send's delay) abort instead of riding out its own timeout.
		engine.WithShutdownSignal(w.ctx.Done()),
		engine.WithTraceID(traceID),
		engine.WithTenantID(wf.TenantID),
		engine.WithBackends(wasmtimeLanguages, w.wasmtimeBackend),
		engine.WithWorkflowStore(execStore),
		engine.WithChildWorkflowStore(execStore),
		engine.WithPluginRegistry(w.pluginRegistry),
		engine.WithPluginStreamRegistry(w.pluginStreamRegistry),
		engine.WithStreamHub(w.streamHub),
		engine.WithMaxRetryAttempts(w.maxRetries),
		engine.WithSchema(w.schemaName),
		engine.WithEncryption(w.encryption, w.encryptSensitivePayloads),
		engine.WithMaxQuotaEvents(w.maxQuotaEvents),
		engine.WithMaxQuotaChildren(w.maxQuotaChildren),
		engine.WithMaxQuotaConcurrencyKeys(w.maxQuotaConcurrencyKeys),
		engine.WithMaxQuotaSchedules(w.maxQuotaSchedules),
		engine.WithDefaultWorkflowTimeout(w.maxWorkflowDuration),
		engine.WithWASMInstanceTimeout(w.wasmInstanceTimeout),
		engine.WithWasmWallClockCeiling(w.wasmWallClockCeiling),
		engine.WithHostRetryBudget(w.hostRetryBudget),
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
	// Scope the ENGINE's statements to this tenant. cleat#1278.
	//
	// The comment here used to read "use tenant-scoped database connection for
	// plugin host functions", and that is worth correcting loudly rather than
	// quietly: two sessions independently built an architecture argument on it
	// and both were wrong for hours. It describes a wire that exists and is
	// dead.
	//
	// WHAT THIS ACTUALLY SCOPES. WithDB sets engine.db. That is the handle the
	// per-step flush, the adaptive flusher and the engine's own statements use,
	// covering the core tenant-scoped tables. Under --tenant-isolation=role it
	// becomes a pool authenticated AS the tenant's login role, so those
	// statements are isolated by the connection itself.
	//
	// WHAT IT DOES NOT SCOPE: plugin statements. A plugin holds the handle in
	// plugin.Environment.DB, built ONCE at worker startup from the owner or
	// the dedicated plugin pool -- cmd/cleat-worker/main.go's getPluginDB --
	// and nothing here reaches it. Every statement in a plugin's HTTP handlers
	// and background loops runs on that owner-privileged handle regardless of
	// this block.
	//
	// THE DEAD WIRE, because "it cannot reach plugins" is the tempting summary
	// and is not true either. execSession.pluginCallContext copies engine.db
	// into plugin.CallContext.DB, so a plugin invoked over a HOST CALL is
	// handed this tenant pool. No plugin reads that field: across plugins/,
	// CallContext is read 48 times for TenantID and 6 for WorkflowID, and
	// zero times for DB. So the path is built and unused rather than absent,
	// which is the distinction that makes the original comment aspirational
	// rather than simply false -- and the reason to state it here, since a
	// plugin author who starts reading cc.DB silently changes which pool
	// their statements run on.
	//
	// WHAT ISOLATES PLUGIN TABLES INSTEAD is row-level security plus
	// cleat.tenant_id set per transaction by engine/plugindb_tenant.go's
	// beginTenantTx, from the tenant in the context -- supplied by the HTTP
	// middleware or, for host calls, by pluginCallContext. That mechanism is
	// independent of this flag and works with --tenant-isolation=rls, which is
	// the default.
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
	// Load the persisted event count. UNCONDITIONALLY, which it was not:
	// this was gated on `w.maxQuotaEvents > 0` because the only consumer was
	// the event cap, and a cap of 0 meant nobody needed the number.
	//
	// The replay tail check (engine/replayer.go, cleat#1507) is a second
	// consumer with a different reachability requirement. Left behind the
	// gate it would be green in every test that configures a quota and absent
	// from every deployment that does not -- a detector that reports only
	// where nobody is looking. Seeding it when no cap is set costs one query
	// per claim and changes no cap behaviour: durablecalls.go's check is
	// guarded by `maxEventsPerWorkflow > 0`, which WithMaxQuotaEvents is the
	// only thing that sets.
	//
	// An error is deliberately swallowed: the count stays 0, and
	// reportShortReplayHistory declines to speak on 0 precisely because it
	// cannot tell "no events" from "I did not manage to ask".
	if count, err := execStore.GetEventCount(w.ctx, wf.ID); err == nil {
		engineOpts = append(engineOpts, engine.WithInitialEventCount(count))
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
	// Set unconditionally, and note it is NOT enough on its own: this governs
	// the direct flush path only. The batch path takes the same value from the
	// flusher registry, which is built in main.go from the same flag, because
	// one flusher serves every workflow of a tenant and cannot read a
	// per-execution engine. Both or neither -- a value set here alone would
	// leave the high-rate path on the default and look configured.
	engineOpts = append(engineOpts, engine.WithFlushRetryWindow(w.flushRetryWindow))
	if w.flusherRegistry != nil {
		engineOpts = append(engineOpts, engine.WithFlusherRegistry(w.flusherRegistry))
	} else {
		w.logger.InfoContext(context.Background(), "flusher registry not set on worker — using direct flush", "workflow_id", wf.ID)
	}
	// Throttle cancellation polls to at most once per 100ms wall-clock
	// to avoid a full DB transaction on every durable step.
	engineOpts = append(engineOpts, engine.WithCancellationCheckInterval(100*time.Millisecond))

	// A claim that carries a pending terminal outcome is not ordinary work:
	// it is the second phase of a terminal transition whose first phase
	// already decided how this workflow ends (IMPROVEMENT-PLAN 3.75 step 2).
	// The segment replays the history to rebuild a live instance, refuses the
	// body any NEW work, and runs the outstanding defers in it.
	//
	// One option on a per-execution engine, which is all this needs: the
	// worker builds an engine per workflow a few lines above, not one shared
	// engine reused across dispatches. WS2-STATUS.md (retired 2026-09-04;
	// see git history) recorded the opposite as a
	// constraint to decide before implementing ("the worker needs a second
	// engine", 2026-09-03) -- it named setup.go:1705, which is this
	// NewEngine call, and read it as construction-time state shared between
	// executions. It is not, so there was nothing to decide.
	deferPhase := wf.PendingTerminalStatus != ""
	if deferPhase {
		engineOpts = append(engineOpts, engine.WithDeferPhase())
	}
	eng := engine.NewEngine(nil, caller, engineOpts...)
	eng.Metrics = w.Metrics

	w.execEngines.Store(wf.ID, eng)

	// ---- Execute/Resume ----
	inputJSON := wf.Input
	setupElapsed := time.Since(workflowStartTime)
	engineStart := time.Now()
	// execCtx, not w.ctx: cleat#2008, so a fenced-out run does not also stop
	// every OTHER execution this worker holds. What actually refuses this
	// run's next durable call is fencedRuns via WithCanStartNewWork above,
	// not this context -- see fencedRuns' doc comment for the measurement
	// showing wasmtime's host-call dispatch never observes it.
	result, resultHistory, suspended, _, queryState, err := eng.Replay(execCtx, wasmBytes, entryPoint, inputJSON, history)
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
		if deferPhase {
			// A defer segment that could not run is still a terminate that
			// has to complete. The outcome was decided and recorded before
			// this segment was ever claimed, and there is no failure this
			// replay can report that changes it -- so the workflow is
			// finalized with the recorded outcome and the cleanup is
			// reported as lost, rather than left in 'terminating' for the
			// deadline to pick up five minutes later.
			//
			// Not left to writeTerminalFailure's guard, which covers the
			// failure paths that run before the segment: this one has the
			// segment's own events, and they still belong in the history
			// even when the execution that produced them ended badly.
			w.logger.ErrorContext(context.Background(), "defer phase failed; terminating without its cleanup",
				"worker_id", w.id, "workflow_id", wf.ID, "tenant_id", wf.TenantID, "error", err)
			w.finishDeferPhase(wf, execStore, history, resultHistory, workflowStartTime)
			return
		}
		w.logger.ErrorContext(context.Background(), "execution error", "worker_id", w.id, "workflow_id", wf.ID, "tenant_id", wf.TenantID, "error", err)
		var ce *engine.CleatError
		errorCode := engine.ErrUnknown.String()
		errorOp := ""
		if errors.As(err, &ce) {
			errorCode = ce.Code.String()
			errorOp = ce.Op
		}
		errMsg := err.Error()
		// resultHistory, not history: the exhaustion that ended this workflow
		// was recorded by the segment that just ran, so it is only in the
		// post-execution history. Passing the pre-execution `history` here
		// would look correct and dead-letter nothing on a first failure.
		w.recordTerminalFailureWithHistory(wf, workflowStartTime, errMsg, errorCode, errorOp, resultHistory)
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

	// A defer segment ends by applying the outcome its first phase recorded,
	// whatever shape it came back in. It normally comes back SUSPENDED -- the
	// body replays, asks for its first piece of new work, and is refused -- so
	// the ordinary reading of that suspension (reschedule and run again later)
	// is exactly the wrong one here, and this branch has to come before it.
	if deferPhase {
		w.finishDeferPhase(wf, execStore, history, resultHistory, workflowStartTime)
		return
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

	// finalStatus here is only ever "done" or "ready" (set a few lines up),
	// never "failed" -- a segment that fails does not reach FinalizeWorkflowSegment
	// at all, it goes through recordTerminalFailureWithHistory instead. "ready"
	// is a suspend, not a terminal outcome, so it owes none of the three
	// post-settle side effects. See notifyTerminal for what those are and why
	// this used to gate on "done" || "failed" here, which made half of every
	// gate below dead. cleat#1976.
	if finalStatus == "done" {
		w.notifyTerminal(wf, finalStatus)
	}

	// Post-finalization: logging and non-DB side effects.
	if finalStatus == "done" {
		// No defer pass here.
		//
		// finalStatus "done" means the guest reached cleat_complete with a
		// result, so it came out through its entry point wrapper -- and that
		// wrapper runs the registered defer bodies before reporting
		// (wasm/exports.go, IMPROVEMENT-PLAN 3.70). This used to call
		// w.runDefers, which looked for an export named "cleat_defer_<id>"
		// that no guest in any language has ever had, and logged the miss as
		// "defer execution failed" for cleanup that had just run.
		//
		// The abnormal paths are unaffected: a guest that trapped, was fenced
		// or timed out never reached its wrapper, and Engine.Execute still
		// runs its defers.

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

	// A Timer, not a Ticker, re-armed by hand each iteration: on a failed or
	// troubled call this reschedules soon rather than waiting out the full
	// interval -- "heartbeat immediately on reconnect" (cleat#2005). The
	// sooner this worker's own heartbeat lands again after a stall, the
	// sooner reapingIsSafe() -- on this worker's own reaper, and via
	// heartbeat_at on every OTHER worker's -- trusts it again. See
	// heartbeatRetryInterval's doc for the bound.
	timer := time.NewTimer(w.heartbeatInterval)
	defer timer.Stop()

	for {
		select {
		case <-w.getLoopCtx("heartbeat").Done():
			return
		case <-timer.C:
			w.healthTracker.recordRun("heartbeat")
			hbStart := time.Now()
			ok := w.heartbeatAndFenceInFlight()
			w.Metrics.SetBackgroundLoopDuration(w.ctx, "heartbeat", time.Since(hbStart).Seconds())
			if ok {
				timer.Reset(w.heartbeatInterval)
			} else {
				timer.Reset(w.heartbeatRetryInterval())
			}
		}
	}
}

// heartbeatRetryInterval is how soon heartbeatLoop retries after a failed or
// troubled call, rather than waiting out the full --heartbeat interval.
// Capped at one second: short enough that a worker recovering from a stall
// re-proves itself quickly, and never longer than heartbeatInterval itself
// (an operator running with --heartbeat below one second gets no slower a
// retry than their own normal cadence). cleat#2005.
func (w *Worker) heartbeatRetryInterval() time.Duration {
	return heartbeatRetryIntervalFor(w.heartbeatInterval)
}

// heartbeatRetryIntervalFor is heartbeatRetryInterval's arithmetic as a pure
// function of heartbeat, split out (cleat-review's fourth round on
// cleat#2005) so minimumReclaimAfter can fold this delay into the stated
// invariant instead of restating `min(heartbeat, time.Second)` as a second,
// separately-maintained literal.
func heartbeatRetryIntervalFor(heartbeat time.Duration) time.Duration {
	return min(heartbeat, time.Second)
}

// heartbeatBatchStore is the store HeartbeatBatchFenced actually calls
// through: heartbeatStore when one was built (--heartbeat-max-connections >
// 0), else `store` -- see heartbeatStore's own doc comment for why this
// exists. Not used for the idle-ping path in heartbeatAndFenceInFlight below,
// which has no in-flight work of this worker's own competing for `store`'s
// pool at the moment it runs.
func (w *Worker) heartbeatBatchStore() engine.WorkflowStore {
	if w.heartbeatStore != nil {
		return w.heartbeatStore
	}
	return w.store
}

// heartbeatAndFenceInFlight heartbeats every run this worker's OWN goroutines
// are currently executing, fenced individually per (run, generation) rather
// than in the single unfenced statement BatchHeartbeat issues. cleat#2008:
// this replaces that call at this one site (BatchHeartbeat itself is
// unchanged and still correct for whatever else may call it) because it does
// everything BatchHeartbeat did here PLUS the fence check, in the same
// number of round trips.
//
// w.inflight is exactly the population this needs, and exactly the
// population BatchHeartbeat's own WHERE clause matched: a run leaves
// w.inflight (executeWorkflow's own deferred Delete) in the same moment it
// leaves status='running' (FinalizeWorkflowSegment always changes one when it
// changes the other), so the two have never had a gap to close.
// heartbeatAndFenceInFlight's bool return is "did this call land" -- used
// only by heartbeatLoop to decide whether to retry immediately rather than
// wait out the full interval. cleat#2005: see the loop's own comment on why
// that matters for how soon this worker's own reaper (and every other
// worker's) trusts it again after a stall.
func (w *Worker) heartbeatAndFenceInFlight() bool {
	var runs []engine.GenerationKey
	w.inflight.Range(func(key, value any) bool {
		wf, ok := value.(*engine.WorkflowInstance)
		if !ok {
			return true
		}
		runs = append(runs, engine.GenerationKey{WorkflowID: wf.ID, Generation: wf.Generation})
		return true
	})
	if len(runs) == 0 {
		// cleat#2005 follow-up review: this used to return success here
		// without ever touching the database, so an idle worker's
		// lastDBTrouble stayed clean through a stall it never observed --
		// "no trouble recorded" meant "never checked," and its reaper
		// trusted a staleness reading it had no basis to trust. A DBPinger
		// probe is what an idle worker's heartbeat tick calls instead, so
		// the two read the same. See DBPinger and lastDBContactOK.
		//
		// A store that doesn't implement DBPinger (every test double today)
		// is deliberately NOT treated as clean here: recordDBContactOK is
		// called only after a REAL successful probe, never merely because
		// none was available. A store with no way to prove liveness while
		// idle must not let its reaper act on staleness while idle either --
		// failing closed, not open. It still reaps normally once it has
		// in-flight work of its own, via the busy path below.
		if pinger, ok := w.store.(engine.DBPinger); ok {
			if err := w.probeBoundedCall(pinger.PingDB); err != nil {
				w.recordDBTrouble()
				w.Metrics.RecordBackgroundLoop(w.ctx, "heartbeat", "error")
				if isConnectionError(err) {
					w.logger.WarnContext(w.ctx, "idle heartbeat ping failed: DB appears down", "worker_id", w.id)
				} else {
					w.logger.ErrorContext(w.ctx, "idle heartbeat ping error", "worker_id", w.id, "error", err)
				}
				return false
			}
			w.recordDBContactOK()
		}
		w.lastHeartbeatOK.Store(time.Now().UnixNano())
		w.Metrics.RecordBackgroundLoop(w.ctx, "heartbeat", "ok")
		return true
	}

	// Bounded so a stalled connection is recorded as trouble instead of
	// silently outliving the stall -- see dbCallDeadline's doc. A database
	// that merely delays rather than refuses (cleat#2005's docker-pause
	// reproduction) never returns a connection error at all; only a
	// deadline catches that shape.
	var lost []string
	err := w.probeBoundedCall(func(hbCtx context.Context) error {
		var err error
		lost, err = w.heartbeatBatchStore().HeartbeatBatchFenced(hbCtx, w.id, runs)
		return err
	})
	if err != nil {
		w.recordDBTrouble()
		w.Metrics.RecordBackgroundLoop(w.ctx, "heartbeat", "error")
		if isConnectionError(err) {
			w.logger.WarnContext(w.ctx, "HeartbeatBatchFenced failed: DB appears down", "worker_id", w.id)
		} else {
			w.logger.ErrorContext(w.ctx, "HeartbeatBatchFenced error", "worker_id", w.id, "error", err)
		}
		// A failed heartbeat tells us nothing about which runs are lost --
		// only that we could not ask. Cancelling on an error here would
		// stop every in-flight execution on a single transient DB blip.
		// cleat#2008 decision 2 (stand down after sustained failure) is the
		// deliberately separate, slower response to this case.
		return false
	}
	w.recordDBContactOK()
	w.lastHeartbeatOK.Store(time.Now().UnixNano())
	w.Metrics.RecordBackgroundLoop(w.ctx, "heartbeat", "ok")

	for _, id := range lost {
		wfAny, ok := w.inflight.Load(id)
		wf, _ := wfAny.(*engine.WorkflowInstance)
		var defName string
		var generation int64
		if ok && wf != nil {
			defName = wf.DefName
			generation = wf.Generation
		}
		cancelAny, ok := w.execCancel.Load(id)
		if !ok {
			// Already gone -- the execution finished (and deregistered
			// itself) between the snapshot above and this heartbeat
			// returning. Nothing to cancel.
			continue
		}
		cancel, ok := cancelAny.(context.CancelFunc)
		if !ok {
			continue
		}
		w.logger.WarnContext(w.ctx, "execution fenced out: run superseded by a later generation",
			"worker_id", w.id, "workflow_id", id, "def_name", defName, "generation", generation)
		w.Metrics.RecordExecutionFencedOut(w.ctx, defName)
		// This, not cancel() below, is what stops the NEXT durable call --
		// see fencedRuns' doc comment. Set only here, alongside the execCancel
		// that proves the execution is still registered, so a run that has
		// already finished (and deregistered) by the time this loop reaches
		// it never gets an entry nothing would go on to delete.
		w.fencedRuns.Store(id, struct{}{})
		cancel()
	}
	return true
}

// reclaimAfter is how long a run may go without a heartbeat before another
// worker may claim it.
//
// WHY THIS IS A SEPARATE KNOB FROM --heartbeat. It used to be
// max(heartbeatInterval*2, 10s) inline, which welded two unrelated questions
// together: how often a LIVE worker checks in, and how long a DEAD worker's
// work stays stranded. The rule that a run must miss two consecutive heartbeats
// before it is stale is a sound default -- a slow heartbeat should not cause
// false-positive reaping -- but it is only a default.
//
// A DATABASE OUTAGE STOPS THE HEARTBEAT WITHOUT THE WORKER BEING DEAD, and that
// is the case the coupling served badly. The heartbeat is written to the same
// database the workflow's events are, so a failover silences every worker at
// once; at the default 5s heartbeat every run in the fleet is reclaimable ten
// seconds in. Buying tolerance for that by raising --heartbeat also made
// heartbeats sparse, so a genuinely crashed worker's runs sat stranded for just
// as long -- paying for an occasional outage with every crash. cleat#1717.
//
// Zero means derive, so an operator who tuned --heartbeat keeps exactly the
// window they had.
//
// It deliberately does NOT govern the worker-membership lease in
// worker_membership.go, which answers a different question -- which workers
// exist, for shard distribution -- and still follows --heartbeat.
func (w *Worker) reclaimAfter() time.Duration {
	return reclaimWindow(w.reclaimTimeout, w.heartbeatInterval)
}

// heartbeatPresumedLost reports whether this worker's own heartbeats have
// been failing (or the DB has been unreachable) for at least as long as
// another worker is allowed to wait before reclaiming a run of ours.
// cleat#2008 decision 2.
//
// It is deliberately keyed on the SAME window ReapStaleInstances uses,
// rather than a separate knob: the question this answers -- "might another
// worker legitimately hold what I think is still mine?" -- is the mirror of
// the question the reaper answers, so the two must agree. A shorter window
// here would refuse new work before any reclaim could possibly have
// happened; a longer one would keep starting new durable calls after a
// reclaim already could have landed.
//
// Passed to every execution's Engine as WithCanStartNewWork -- see setup.go
// where execEngines are constructed.
func (w *Worker) heartbeatPresumedLost() bool {
	last := time.Unix(0, w.lastHeartbeatOK.Load())
	return time.Since(last) > w.reclaimAfter()
}

// runIsFenced reports whether THIS run's own fenced heartbeat has already
// reported it lost -- decision 1, scoped to one workflow ID rather than
// every run this worker holds. See fencedRuns' doc comment for why this,
// and not execCtx cancellation, is what freshCall actually refuses on.
func (w *Worker) runIsFenced(workflowID string) bool {
	_, ok := w.fencedRuns.Load(workflowID)
	return ok
}

// recordDBTrouble marks this worker's own database contact as having just
// had trouble: a connection error, or a round trip that did not return
// before dbCallDeadline. Called from the heartbeat and reaper loops alike,
// because both are the same question asked from two directions -- can THIS
// worker currently reach the database at all? A worker that cannot has no
// basis for trusting a stale heartbeat_at as evidence that some OTHER
// worker died, rather than evidence that an outage silenced every worker's
// writes at once, itself included. See lastDBTrouble and reapingIsSafe.
// cleat#2005.
func (w *Worker) recordDBTrouble() {
	w.lastDBTrouble.Store(time.Now().UnixNano())
}

// recordDBContactOK marks this worker's own database contact as having just
// been CONFIRMED clean: a heartbeat, idle ping, or reap that actually
// returned success within its deadline. The companion to recordDBTrouble,
// and reapingIsSafe requires both -- see lastDBContactOK's doc for why an
// absence of trouble is not the same claim as a confirmed success.
// cleat#2005 follow-up review.
func (w *Worker) recordDBContactOK() {
	w.lastDBContactOK.Store(time.Now().UnixNano())
}

// probeBoundedCall runs fn with a context bounded by dbCallDeadline, and
// promotes a nil error that still took longer than the deadline to return
// into an error of its own.
//
// WHY A "SUCCESSFUL" CALL CAN STILL BE TROUBLE. dbCallDeadline exists so a
// call in flight from before a stall began fails rather than silently
// outliving it -- but that guarantee depends on the underlying call actually
// aborting when its context is cancelled, and cleat-review measured that
// this does not universally hold: lib/pq's PingContext, in particular, can
// return a late success well past the context deadline under a real
// docker-pause stall, same as a driver returning success from an autocommit
// statement the server already finished (see MySQLStore.ReapStaleInstances'
// doc for that variant, which is why its fix is a transaction rather than
// this check alone -- this check is the generic backstop for calls where a
// transaction isn't the applicable fix, such as a plain ping). A call that
// took longer than its own deadline to return proves nothing about whether
// the round trip actually raced a stall, so it is treated the same as an
// explicit failure rather than trusted.
func (w *Worker) probeBoundedCall(fn func(ctx context.Context) error) error {
	ctx, cancel := context.WithTimeout(w.ctx, w.dbCallDeadline())
	start := time.Now()
	err := fn(ctx)
	cancel()
	if err == nil {
		if elapsed := time.Since(start); elapsed > w.dbCallDeadline() {
			return fmt.Errorf("call returned success after %v, past its %v deadline -- not trusted", elapsed, w.dbCallDeadline())
		}
	}
	return err
}

// reapingIsSafe reports whether this worker can currently trust a stale
// heartbeat_at as evidence that some OTHER worker died, rather than evidence
// that a stall silenced every worker's writes at once, itself included.
//
// TWO SEPARATE CONDITIONS, both required -- not one. cleat-review's follow-up
// on the first version of this gate (cleat#2005) found that "no trouble in
// reclaimAfter()" alone reopens the exact hole the gate exists to close: an
// IDLE worker with nothing to heartbeat made no database call at all in the
// old heartbeatAndFenceInFlight, so its lastDBTrouble stayed clean through a
// stall it never observed, and its reaper reclaimed a run whose true holder
// was alive throughout. "No trouble recorded" and "recently confirmed clean"
// are different claims, and only the second is evidence:
//
//  1. lastDBContactOK must be RECENT. This is the positive-evidence half --
//     a confirmed successful round trip, not merely an absence of a failed
//     one. The bound is 2*(heartbeatInterval + dbCallDeadline()), not a bare
//     2*heartbeatInterval: a probe attempt can legitimately take up to
//     dbCallDeadline() to return (success or failure) before the next one
//     starts, so a tighter bound flaps on ordinary probe latency, especially
//     at a low --heartbeat where the deadline is a larger fraction of the
//     interval. cleat-review measured this: at --heartbeat 1s, a merely
//     slow-but-healthy probe cycle can take longer than a bare 2x bound
//     allows.
//  2. lastDBTrouble must be OLD ENOUGH -- at least reclaimAfter() in the
//     past. This is the existing clean-window half: even with recent
//     confirmed contact, a failure inside the window means the window has
//     not genuinely elapsed yet.
//
// Condition 1 alone would accept a worker that just proved it can reach the
// database, even if it saw trouble a moment before -- which is exactly the
// window a real stall's recovery moves through, so both are required.
//
// THIS IS NOT heartbeatPresumedLost, DESPITE SHARING reclaimAfter() AS PART
// OF THE THRESHOLD. That one asks whether ANOTHER worker might legitimately
// have reclaimed one of THIS worker's own runs by now, so THIS worker should
// refuse to start new durable calls. This one asks the mirror question for
// the reaper: can THIS worker trust what it is about to read well enough to
// reclaim someone ELSE's run?
//
// See lastDBTrouble's and lastDBContactOK's docs for why both default
// pessimistic (false) rather than optimistic, the opposite of
// heartbeatPresumedLost's default.
func (w *Worker) reapingIsSafe() bool {
	lastOK := time.Unix(0, w.lastDBContactOK.Load())
	if recency := 2 * (w.heartbeatInterval + w.dbCallDeadline()); time.Since(lastOK) > recency {
		return false
	}
	lastTrouble := time.Unix(0, w.lastDBTrouble.Load())
	return time.Since(lastTrouble) >= w.reclaimAfter()
}

// dbCallDeadline bounds a single heartbeat, idle ping, or reaper database
// round trip. See dbCallDeadlineFor for the arithmetic and why it must not
// grow independently of heartbeatInterval.
func (w *Worker) dbCallDeadline() time.Duration {
	return dbCallDeadlineFor(w.heartbeatInterval)
}

// dbCallDeadlineFor is dbCallDeadline's arithmetic, as a function of its one
// input -- split out for the same reason reclaimWindow is: startup
// validation needs it before any Worker exists, and a second copy of
// `heartbeat / 2` somewhere else is a second source of truth nothing would
// notice diverging.
//
// WHY THIS HAS TO EXIST AT ALL, not just recordDBTrouble/reapingIsSafe. A
// call issued with no deadline can be in flight from BEFORE a stall began,
// and a stall that merely delays a connection (rather than severing it)
// lets that call return SUCCESS the moment the database responds -- which,
// per ReapStaleInstances' own predicate, is exactly when a heartbeat_at
// written before the stall looks old enough to reclaim. reapingIsSafe
// cannot protect against a call that was already past its own gate check
// before the stall was detectable. Bounding the call is what turns "hangs
// silently for the whole stall, then succeeds" into "fails partway through,
// repeatedly, keeping lastDBTrouble current until the stall genuinely
// clears" -- see cleat#2005's issue body for the reproduction this closes.
//
// dbCallDeadlineFloor is the minimum value dbCallDeadlineFor ever returns
// (cleat-review's GAP1, third round on cleat#2005). This used to have no
// independent floor at all, on the theory that any value keeps
// reclaimWindow's and validateReclaimTimeout's derivations consistent with
// each other because both key off this function -- true, but beside the
// point. Below --heartbeat 4s, plain hb/2 shrinks past what a real driver's
// own ordinary latency needs (lib/pq and go-mssqldb's own internal timeouts
// run several seconds under normal conditions -- see cleat#2005's issue
// body), so at --heartbeat 1s a perfectly healthy round trip could exceed a
// 500ms deadline and get recorded as DB trouble.
//
// A PACKAGE-LEVEL VAR, NOT A CONST, and this is the one place in this file
// that bends the "no test hooks in production code" instinct. Every
// timing-based test in reaper_recovery_grace_period_test.go runs at
// millisecond-scale heartbeat intervals specifically so it completes in
// milliseconds rather than tens of seconds -- a real 2s floor would make
// dbCallDeadlineFor(200ms) return 2s instead of 100ms, silently turning
// every one of those tests' timing assumptions to nonsense (and, in two
// cases, into a hung-call test that never actually gets cut off within its
// own 2s wait). Production code never assigns to this; tests that need the
// pre-floor arithmetic call withDBCallDeadlineFloor to substitute a smaller
// one for their own duration, restored via t.Cleanup. Never overridden by a
// test that is actually testing the floor itself.
var dbCallDeadlineFloor = 2 * time.Second

func dbCallDeadlineFor(heartbeat time.Duration) time.Duration {
	if d := heartbeat / 2; d > dbCallDeadlineFloor {
		return d
	}
	return dbCallDeadlineFloor
}

// minimumReclaimAfter is THE STATED INVARIANT, in one place, that both
// reclaimWindow's default and validateReclaimTimeout's refusal enforce.
// Third round on cleat#2005: the first version of this file used
// `heartbeat + 2*dbCallDeadlineFor(heartbeat)`, restated separately in each
// of those two call sites, and cleat-review found that undercounted a single
// failed-then-retried heartbeat. Fourth round: cleat-review found the
// THIRD round's own correction -- heartbeat + 3*deadline -- still
// undercounts, by the retry delay itself, and this file's own regression
// test for round 3 could not have caught it, because its mock modeled the
// wrong failure shape. Both are fixed here.
//
// THE INVARIANT: reclaimAfter must be at least
//
//	heartbeat + 3*dbCallDeadlineFor(heartbeat) + heartbeatRetryIntervalFor(heartbeat)
//
// which cleat-review also writes as `2*heartbeat + deadline + retryInterval`
// -- identical once dbCallDeadlineFor(heartbeat) is exactly heartbeat/2 (the
// 2-second floor not binding), but this file keeps the 3*deadline form
// because it is deadline's OWN floor-inclusive value, so a low --heartbeat
// that hits the floor raises this invariant automatically rather than
// silently falling short of it -- the same reason round 3 chose it.
//
// WHAT ROUND 4 FOUND, AND WHY ROUND 3's OWN TEST MISSED IT. Round 3's doc
// comment (and its regression test's mock) assumed a call that fails during
// a stall does so "using its full deadline" -- i.e. that dbCallDeadlineFor
// bounds how long a FAILING call can take to return. That is true for a
// slow-but-reachable server, where the context deadline can carry a real
// cancellation to a server that is still listening. It is FALSE for a
// genuine network-level stall -- exactly what a docker-pause reproduction,
// and a real outage, are -- because lib/pq's context cancellation for
// Exec/Query sends an actual PostgreSQL CancelRequest over a NEW
// connection, and that connection cannot complete either while the server
// is unreachable. Measured directly against a real Postgres container,
// `docker pause`d mid-call, on both ExecContext (matching
// HeartbeatBatchFenced's own call shape) and QueryRowContext: a call given
// a 2-second context deadline did not return until the pause was lifted --
// 11.7s later, not 2s -- with the context error delivered only once the
// connection could finally act on it. THE CALL'S OWN DURATION IS BOUNDED BY
// THE STALL, NOT BY dbCallDeadlineFor. A client-side deadline cannot cut off
// a call that has nothing left alive on the other end to cancel.
//
// So the holder-side race has three terms, not two:
//
//   - up to `heartbeat`, waiting for the next scheduled attempt after the
//     last successful write;
//   - up to `heartbeat` AGAIN for that attempt's own duration, once it is
//     caught by a stall shorter than one heartbeat interval -- not
//     `deadline`, because (see above) the call cannot return faster than
//     the stall itself clears;
//   - `heartbeatRetryIntervalFor(heartbeat)`, the wait before the retry is
//     even ISSUED once the failed call finally returns -- previously
//     missing from this invariant entirely;
//   - plus `deadline` once more, as the retry's own worst-case latency
//     (this one legitimately bounded, since the retry runs against a
//     healthy connection).
//
// That is `2*heartbeat + deadline + retryInterval`, i.e. this function's
// `heartbeat + 3*deadline + retryInterval` in its floor-safe form.
//
// MEASURED: reproduced with two real Workers running their real
// heartbeatLoop and a mock whose stall blocks the call until explicitly
// released -- IGNORING its context, matching the real-Postgres measurement
// above, not racing ctx.Done() the way round 3's own mock did -- at
// heartbeat=200ms (deadline=100ms unfloored, retryInterval=200ms): a 95ms
// round trip caught by a 190ms stall does not return until the stall
// clears, then retries after a further 200ms wait, landing around 635ms
// after the last successful write. That is safe under this round's 700ms
// (now 1700ms with reclaimSlack, see below) invariant and NOT safe under
// round 3's own 500ms one -- see
// TestASingleFailedHeartbeatRetryCanOutlastTheOldReclaimInvariantButNotTheNewOne
// in reaper_recovery_grace_period_test.go.
//
// ROUND 5: THE BOUND ABOVE HAS ZERO SLACK, AND THAT IS ITSELF THE DEFECT.
// heartbeat + 3*deadline + retryInterval is not a bound WITH margin on the
// modeled worst case -- it IS the modeled worst case, exactly, whenever
// dbCallDeadlineFor's floor does not bind (deadline == heartbeat/2, true
// for every heartbeat >= 4s, including the 5s default): substituting
// d = hb/2 into `hb + 3d + r` gives `2.5*hb + r`, which is exactly
// `2*heartbeat + deadline + retryInterval`, the derivation's own total.
// Algebra, not measurement -- reproduce with substitution.
//
// A model with no margin is only as good as its inputs, and one input was
// wrong: the retry's own latency was modeled as `deadline`, its worst-case
// QUERY execution time, but the retry can also need a NEW CONNECTION. On
// SQL Server this is not a corner case, it is documented driver behaviour --
// github.com/microsoft/go-mssqldb's token.go, on the same cancel-drain path
// this file's stall model already exercises: "If the drain fails for any
// reason (timeout, I/O error, or context cancellation), the connection is
// marked bad via checkBadConn" -- and checkBadConn's own doc says a bad
// connection is "dropped from the connection pool rather than reused."
// A genuine stall is exactly a case where the drain fails (nothing is
// answering), so the failed call's connection is discarded and the RETRY
// dials, TLS-handshakes and logs in fresh -- none of which `deadline`
// represents, because `deadline` bounds a query's execution, not a
// connection's setup. Confirmed by reading that source directly, not
// assumed from the symptom.
//
// So the invariant needs a term for what it does not model at all, not a
// bigger version of a term that already accounts for something else.
// reclaimSlack is that term: fixed, independent of heartbeat, and named for
// what it absorbs (a reconnect, the retry's own initial round trip, and
// ordinary Timer/scheduler lateness) rather than folded silently into
// dbCallDeadline or retryInterval where a future reader would have to
// re-derive why it is there.
//
// Changes the default --reclaim-timeout from ~13.5s (round 4) to ~14.5s at
// the default --heartbeat (5s: 5 + 3*2.5 + min(5, 1) + 1). Worth noting in
// the changelog: it is a wider window before a genuinely dead worker's run
// is reclaimed, not a behaviour change anyone has to opt into.
func minimumReclaimAfter(heartbeat time.Duration) time.Duration {
	return heartbeat + 3*dbCallDeadlineFor(heartbeat) + heartbeatRetryIntervalFor(heartbeat) + reclaimSlack
}

// reclaimSlack is fixed headroom on top of minimumReclaimAfter's modeled
// worst case, added in round 5 on cleat#2005 because that model has ZERO
// margin at heartbeat >= 4s (see minimumReclaimAfter's doc) and does not
// account for a reconnect before the retry -- real on SQL Server, where a
// cancelled call whose drain also fails (a genuine stall) marks the
// connection bad, so the retry pays a fresh dial, TLS handshake and login
// that dbCallDeadlineFor was never meant to cover. Deliberately a constant,
// not a function of heartbeat: it is not modeling a heartbeat-scaled
// quantity, it is covering everything the model leaves out.
const reclaimSlack = 1 * time.Second

// ---------------------------------------------------------------------------
// Suspected database stall detection (cleat#2006)
// ---------------------------------------------------------------------------
//
// #2166 (minimumReclaimAfter, reapingIsSafe) covers a worker that itself
// observed database trouble. This covers the complementary gap: the reaper
// had contact (its own reads succeed), but heartbeat WRITES were silenced
// fleet-wide -- a paused volume that reads survive but writes stall on, or a
// failover only some workers saw. Every running row ages past reclaimAfter()
// at once, and whichever worker's reaper reaches the database first after
// recovery reclaims runs that are still alive, including its own.
//
// THE RAMP HOLE, AND WHY DETECTION USES A SHORTER THRESHOLD THAN RECLAIM
// ELIGIBILITY. A first version of this gated the "is this a stall" ratio on
// the SAME threshold ReapStaleInstances reclaims at (reclaimAfter, R). That
// is wrong: each worker's own last-good heartbeat, right before a stall
// begins, lands anywhere in its own current cycle -- up to
// missedBeatThreshold's own width -- so individual rows cross R STAGGERED
// over that same window, not all at once. For a whole-fleet stall lasting
// somewhat less than R, only a minority of rows are already R-stale at any
// single tick; the ratio never reads as "nearly everyone", and each row gets
// reclaimed the instant it individually crosses R -- precisely the harm this
// file exists to prevent, arriving through the detector's own blind spot.
//
// So DETECTION uses a much shorter threshold -- "has this row missed at
// least one expected heartbeat" -- and RECLAIM ELIGIBILITY stays exactly
// where it already was (R, via reclaimAfter()). The two are deliberately
// different questions answered by different thresholds.
const missedBeatSlack = 1 * time.Second

// missedBeatThreshold is the DETECTION threshold: how stale a row has to be
// before it counts as "missed at least one heartbeat", not "eligible to
// reclaim". heartbeat + dbCallDeadlineFor(heartbeat) is the width a single
// worker's own write cycle can legitimately span (a call caught right at
// its own deadline), plus missedBeatSlack for the same reason reclaimSlack
// exists on minimumReclaimAfter -- ordinary scheduling and round-trip
// latency the model does not itemize. 8.5s at the 5s default.
func missedBeatThreshold(heartbeat time.Duration) time.Duration {
	return heartbeat + dbCallDeadlineFor(heartbeat) + missedBeatSlack
}

// suspectedDBStall implements #2006's proposal, on the criterion
// cleat-review substituted for the original fraction+spread one
// (2026-09-24): a stall is suspected when NOT EVEN ONE running row in
// scope has a heartbeat newer than missedBeatThreshold -- shape.
// NoRecentHeartbeat, computed server-side -- across more than one worker.
//
// WHAT THIS REPLACED, AND WHY. The original test was
// `fraction > 0.80 && distinctAssignedTo > 1 && spread <= missedBeatThreshold`,
// against the MissedBeat population. It had a residual false-negative at
// its own boundary: 5 workers, the 4 oldest rows already past
// missedBeatThreshold and the 5th (freshest) not quite there yet, is
// 4/5 = 80% -- and 80% is not > 80%, so a genuine whole-fleet stall one
// worker's phase away from full staleness went undetected. It also
// degraded as the running set aged: one long-dead, never-reclaimed row
// widens MissedBeatOldest..MissedBeatNewest without bound, eventually
// exceeding the spread gate on its own regardless of what every other row
// is doing.
//
// NoRecentHeartbeat has neither problem: it doesn't average or compare
// against a spread window, it asks the single sharpest form of the
// question this heuristic exists to answer -- has ANYTHING happened,
// anywhere, recently. A single fresh survivor anywhere in scope answers
// "yes" and the whole gate reports false, with no fraction arithmetic
// to sit exactly on a boundary.
//
// A single-worker fleet can never trip this: every running row shares one
// assigned_to, so DistinctAssignedTo > 1 never holds. That is correct, not
// a gap -- if that one worker's own writes are what's failing, its own
// probeBoundedCall failure already calls recordDBTrouble on itself, and
// reapingIsSafe (#2166) already refuses its own reaper on that signal. This
// heuristic exists for the case #2166 cannot see: worker B reclaiming
// worker A's row with no trouble of its own recorded anywhere.
func suspectedDBStall(shape engine.StaleSetShape) bool {
	if shape.Running == 0 {
		return false
	}
	return shape.NoRecentHeartbeat && shape.DistinctAssignedTo > 1
}

// stallSuppressionEpisode tracks one independent instance of the
// suspected-stall suppression decision -- one per shard for a MultiShard
// store, or one for a plain store. Zero value is ready to use.
type stallSuppressionEpisode struct {
	// since is the UnixNano of the tick suspicion FIRST fired for the
	// current episode. Zero means no active episode. Deliberately does not
	// reset merely because one tick briefly sees a fresh heartbeat -- see
	// evaluate's doc -- so a mass-death event with one flickering survivor
	// cannot win a fresh grace period on the strength of a single blip.
	since atomic.Int64
	// alerted marks that the bound-hit warning has already fired for the
	// current episode, so it logs once at the transition rather than on
	// every tick past the bound.
	alerted atomic.Bool
}

// stallSuppressionDecision is what one reaper tick does for one episode.
type stallSuppressionDecision struct {
	// Suppress: true means skip reclaiming entirely this tick for this unit.
	Suppress bool
	// BoundHit: true the tick the suppression bound is first crossed --
	// exactly when an operator alert should fire. False on every other
	// tick, including subsequent ticks where the bound is still exceeded
	// (BoundHit only marks the TRANSITION, Suppress==false covers the rest).
	BoundHit bool
}

// stallProtectionLower and stallProtectionUpper bound how long a
// whole-fleet stall can last and still be fully protected from a wrongful
// reclaim -- cleat-review asked this be documented explicitly with a
// number, on cleat#2006 (2026-09-24), rather than left implicit in the
// arithmetic below.
//
// Lower bound: missedBeatThreshold (detection latency -- how long a stall
// has to run before NoRecentHeartbeat can even become true) plus
// reclaimAfter (the suppression window itself, evaluate's `reclaimAfter`
// argument). At the 5s heartbeat default, 8.5s + 14.5s = 23s.
//
// Upper bound: the lower bound plus one full reaper tick interval
// (max(heartbeat, 10s)) -- reapOnce only samples StaleSetShape once per
// tick, so a stall that begins just after a tick fires is not observed,
// and therefore not detected, until nearly a full interval later. At the
// default, 23s + 10s = 33s.
//
// A stall shorter than the lower bound is always protected; a stall
// longer than the upper bound is never protected (BoundHit fires and
// reclaiming resumes mid-stall); a stall between the two may or may not
// be, depending on tick phase. This does not model reapingIsSafe's own
// grace period (#2166), which covers the reaper's OWN observed trouble
// and runs independently of this suppression.
func stallProtectionLower(heartbeat, reclaimAfter time.Duration) time.Duration {
	return missedBeatThreshold(heartbeat) + reclaimAfter
}

func stallProtectionUpper(heartbeat, reclaimAfter time.Duration) time.Duration {
	return stallProtectionLower(heartbeat, reclaimAfter) + max(heartbeat, 10*time.Second)
}

// evaluate is one reaper tick's decision for one episode. reclaimAfter is
// the caller's current R (minimumReclaimAfter-derived window); the
// suppression bound reuses that same duration rather than a new constant --
// see the doc comment on suspectedDBStall's call site (reapOnce) for why.
//
// WORST-CASE DEAD-RUN RECOVERY. A genuinely dead worker's run, discovered
// mid-episode right as an unrelated fleet-wide stall trips this suppression,
// can wait up to 2*R for the bound to hit -- R for the episode already in
// progress when it happened to start observing the dead row, plus another
// full R because the row's own individual staleness only just crossed the
// gate as the bound was reached, then one more reaper tick (interval,
// floored at 10s) before the next reapOnce actually reclaims it. At the
// heartbeat default this is 2*14.5s + 10s = 39s -- worse than the pre-#2006
// R alone, and deliberately so: the alternative is reclaiming a run that is
// still alive, which #2166 exists to prevent.
//
// THE RECOVERY TAIL (cleat-review on cleat#2006, round 2, 2026-09-24). A
// stall does not end for every worker at once: one worker's heartbeat can
// land before its siblings', and the moment it does, shape.NoRecentHeartbeat
// flips false -- suspectedDBStall stops firing -- while the siblings' rows
// are still individually stale and still eligible under reclaimAfter alone.
// Un-suppressing on THAT tick reclaims the siblings on the strength of one
// survivor's heartbeat, which is the exact wrongful reclaim this whole
// mechanism exists to prevent; it is just reached through the un-suspected
// branch instead of the suspected one. So suppression is STICKY: once an
// episode is open, a tick that stops looking suspected but still has
// reclaim-eligible rows (shape.Stale > 0) keeps suppressing on the SAME
// clock, exactly as if it were still suspected. Only true quiescence
// (shape.Stale == 0 -- nothing left that reclaimAfter alone would take)
// resets the episode; a tick with no episode open (since == 0) and nothing
// suspected has nothing to stay suppressed on behalf of, and reclaims
// normally, which is what keeps a genuinely non-suspected stale set (a
// single dead worker, TestReapOnceDoesNotSuppressAHealthyStaleSet)
// unaffected.
//
// A reaper that starts up (or whose episode map is otherwise empty) mid-tail
// -- after the stall was suspected but before this reaper ever observed
// it -- has no episode to be sticky about, and reclaims the still-stale
// laggards as soon as they individually cross reclaimAfter. That window is
// bounded by missedBeatSlack (~1s): the gap between one worker's heartbeat
// landing and the rest following is not itself something this suppression
// models or protects.
func (e *stallSuppressionEpisode) evaluate(shape engine.StaleSetShape, reclaimAfter time.Duration, now time.Time) stallSuppressionDecision {
	suspected := suspectedDBStall(shape)
	if !suspected && shape.Stale == 0 {
		// True quiescence: nothing even reclaim-eligible remains. Reset,
		// so the NEXT suspected episode gets its own full grace period
		// rather than inheriting elapsed time from a stall that already
		// ended.
		e.since.Store(0)
		e.alerted.Store(false)
		return stallSuppressionDecision{}
	}
	if !suspected && e.since.Load() == 0 {
		// Not suspected, and no episode is open to be sticky about --
		// ordinary dead-worker staleness (or the reaper missed the stall
		// entirely, see the recovery-tail doc above). Reclaim normally.
		return stallSuppressionDecision{}
	}
	// Either genuinely suspected this tick, or sticky: an episode is
	// already open and reclaim-eligible rows remain even though this
	// particular tick no longer looks suspected (the recovery tail).
	// Both share the same clock and bound -- a survivor's heartbeat must
	// not buy the laggards a fresh window, and must not cut their existing
	// one short either.
	sinceNano := e.since.Load()
	if sinceNano == 0 {
		e.since.Store(now.UnixNano())
		return stallSuppressionDecision{Suppress: true}
	}
	elapsed := now.Sub(time.Unix(0, sinceNano))
	if elapsed < reclaimAfter {
		return stallSuppressionDecision{Suppress: true}
	}
	// Bound hit: stop suppressing. Deliberately does NOT reset `since` --
	// a persistent stall past the bound must keep losing the benefit of the
	// doubt on every subsequent tick, not earn a fresh window each time.
	firstAlert := !e.alerted.Swap(true)
	return stallSuppressionDecision{Suppress: false, BoundHit: firstAlert}
}

// reapUnit is one independently-processed reap target: a whole store, or
// one shard of a MultiShard one.
type reapUnit struct {
	// name is the shard name for a MultiShard store, "" for a plain one.
	name  string
	store engine.WorkflowStore
}

// logLabel is what reapOnce logs and tags the suspected-stall metric with.
func (u reapUnit) logLabel() string {
	if u.name == "" {
		return "default"
	}
	return u.name
}

// reapUnits enumerates what reapOnce processes this tick. A MultiShard
// store (ShardedStore) yields one unit per shard, so cleat#2006's
// suspected-stall suppression -- and any per-call failure -- is decided
// independently per shard: one stalled shard must not pause reclaiming on
// a healthy sibling, and a healthy majority must not mask a genuinely
// stalled minority. A plain store yields exactly one unit.
func (w *Worker) reapUnits() []reapUnit {
	ms, ok := w.store.(engine.MultiShard)
	if !ok {
		return []reapUnit{{store: w.store}}
	}
	names := ms.ShardNames()
	units := make([]reapUnit, 0, len(names))
	for _, name := range names {
		if store, ok := ms.ShardStore(name); ok {
			units = append(units, reapUnit{name: name, store: store})
		}
	}
	return units
}

// stallEpisodeFor returns the cleat#2006 suppression episode for one reap
// unit, creating it on first use. One episode per unit for the life of the
// Worker, so the "first suspected" clock and the alert-once flag persist
// across ticks -- see stallSuppressionEpisode's doc.
func (w *Worker) stallEpisodeFor(unitName string) *stallSuppressionEpisode {
	v, _ := w.stallEpisodes.LoadOrStore(unitName, &stallSuppressionEpisode{})
	return v.(*stallSuppressionEpisode)
}

// reclaimWindow is reclaimAfter's arithmetic, as a function of its two
// inputs. Split out so that startup advice which has to reason about the
// window -- flushRetryWindowAdvice, before any Worker exists -- computes it
// rather than restating it. A second copy of the same arithmetic somewhere
// else is a second source of truth that nothing would notice diverging.
//
// The 10s floor is a separate, simpler concern from minimumReclaimAfter's
// invariant: a sane minimum wait even at a very low --heartbeat, where the
// invariant itself might compute something smaller. It only ever raises the
// value minimumReclaimAfter already guarantees is safe.
func reclaimWindow(reclaimTimeout, heartbeat time.Duration) time.Duration {
	if reclaimTimeout > 0 {
		return reclaimTimeout
	}
	return max(minimumReclaimAfter(heartbeat), 10*time.Second)
}

// reapSkipWarnThreshold is how many reaper ticks in a row reapOnce will skip
// via its grace-period gate before escalating to a distinct, harder-to-miss
// warning (see reapOnce). Three, not one: a single skip right after a real
// stall is the gate doing exactly its job and is already logged at the
// normal level: this is for the case where it never seems to clear.
const reapSkipWarnThreshold = 3

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
			w.reapOnce()
		}
	}
}

// reapOnce is one reaper tick, extracted from reaperLoop so a test can drive
// it directly without a ticker. cleat#2005.
func (w *Worker) reapOnce() {
	reaperStart := time.Now()

	// THE GRACE PERIOD. A stale heartbeat_at only proves a run's holder is
	// dead if THIS worker could have seen a fresh one arrive -- see
	// reapingIsSafe's doc. Skipping here, before ever calling
	// ReapStaleInstances, is what cleat#2005's reproduction had nothing
	// doing: the reaper reclaimed its own worker's live run 56ms after a
	// database stall ended, because nothing distinguished "the holder died"
	// from "my own observation of every worker just went dark, briefly,
	// including of myself".
	if !w.reapingIsSafe() {
		skips := w.consecutiveReapSkips.Add(1)
		w.logger.WarnContext(w.ctx, "Reaper: skipping this tick -- recent database trouble on this worker, waiting out the recovery grace period before trusting a stale heartbeat",
			"worker_id", w.id, "reclaim_after", w.reclaimAfter(), "consecutive_skips", skips)
		if skips == reapSkipWarnThreshold {
			// A single skip, or a handful right after a real outage, is the
			// gate working as designed. A STREAK this long is a different
			// thing worth an operator's attention: either the grace period
			// keeps re-arming because the link is chronically slow or
			// unreliable (cleat-review's GAP3), or something is wrong with
			// this worker's clock or database contact that never clears on
			// its own -- either way, this worker's reaper is not reclaiming
			// anyone's dead work right now, silently, and could stay that
			// way indefinitely without this. Fires once per streak, at the
			// threshold, rather than on every tick past it, so a genuinely
			// long outage does not spam the log once a minute for its
			// duration.
			w.logger.WarnContext(w.ctx, "Reaper: skipped its last several ticks in a row -- this worker has not reclaimed a stale run in a while; check its database connectivity",
				"worker_id", w.id, "consecutive_skips", skips)
			w.Metrics.RecordBackgroundLoop(w.ctx, "reaper", "chronically_skipped")
		}
		w.Metrics.RecordBackgroundLoop(w.ctx, "reaper", "skipped")
		w.Metrics.SetBackgroundLoopDuration(w.ctx, "reaper", time.Since(reaperStart).Seconds())
		return
	}
	w.consecutiveReapSkips.Store(0)

	// Bounded for the same reason heartbeatAndFenceInFlight's call is: a
	// call already in flight from before a stall began would otherwise
	// return SUCCESS the moment the database responds, with the stale
	// predicate satisfied by the stall's own duration -- see
	// dbCallDeadlineFor's doc. The gate above cannot protect against a call
	// that passed it before the stall was detectable; this can.
	staleTimeout := w.reclaimAfter()
	missedBeat := missedBeatThreshold(w.heartbeatInterval)

	// One unit per shard for a MultiShard store, or one plain unit -- see
	// reapUnits' doc. Processed independently: cleat#2006's suspected-stall
	// suppression, and any failure, on one unit must not affect a healthy
	// sibling's reclaim this tick.
	var reaped int
	var lastErr error
	for _, unit := range w.reapUnits() {
		suppressed := false
		if detector, ok := unit.store.(engine.DBStallDetector); ok {
			var shape engine.StaleSetShape
			shapeErr := w.probeBoundedCall(func(reapCtx context.Context) error {
				var err error
				shape, err = detector.StaleSetShape(reapCtx, staleTimeout, missedBeat)
				return err
			})
			if shapeErr != nil {
				// FAIL CLOSED, not open (cleat-review on cleat#2006,
				// 2026-09-24). This used to fall through to the ordinary
				// reclaim attempt below, unsuppressed -- but a slow or
				// failing aggregate over the very table ReapStaleInstances
				// is about to UPDATE is exactly the shape a genuine
				// database stall takes. Treating "I could not check" as
				// "assume it's fine" defeats the whole point of checking:
				// the one tick this probe is most likely to fail is the
				// one where suppressing matters most. Skip this unit's
				// reclaim for the tick instead, the same as an explicit
				// Suppress=true decision; recordDBTrouble below still
				// covers the probe failure itself.
				lastErr = shapeErr
				w.logger.WarnContext(w.ctx, "Reaper: suspected-database-stall probe failed -- skipping this unit's reclaim this tick rather than reclaiming blind",
					"worker_id", w.id, "shard", unit.logLabel(), "error", shapeErr)
				suppressed = true
			} else {
				episode := w.stallEpisodeFor(unit.name)
				decision := episode.evaluate(shape, staleTimeout, time.Now())
				if decision.BoundHit {
					w.logger.WarnContext(w.ctx, "Reaper: suspected-database-stall suppression bound reached -- reclaiming despite a stall-shaped stale set; this may be a genuine mass worker failure rather than a database stall",
						"worker_id", w.id, "shard", unit.logLabel(), "running", shape.Running, "missed_beat", shape.MissedBeat, "bound", staleTimeout,
						"protection_lower", stallProtectionLower(w.heartbeatInterval, staleTimeout),
						"protection_upper", stallProtectionUpper(w.heartbeatInterval, staleTimeout))
				}
				if decision.Suppress {
					w.logger.WarnContext(w.ctx, "Reaper: stale set looks like a suspected database stall, not dead workers -- deferring reclaim by one tick",
						"worker_id", w.id, "shard", unit.logLabel(), "running", shape.Running, "missed_beat", shape.MissedBeat,
						"missed_beat_distinct_assigned_to", shape.MissedBeatDistinctAssignedTo)
					w.Metrics.RecordSuspectedDBStall(w.ctx, attribute.String("shard", unit.logLabel()))
					suppressed = true
				}
			}
		}
		if suppressed {
			continue
		}
		var n int
		err := w.probeBoundedCall(func(reapCtx context.Context) error {
			var err error
			n, err = unit.store.ReapStaleInstances(reapCtx, staleTimeout, w.maxReclaimPerTick)
			return err
		})
		reaped += n
		if err != nil {
			lastErr = err
		}
	}
	if lastErr != nil {
		w.recordDBTrouble()
		if isConnectionError(lastErr) {
			w.logger.WarnContext(w.ctx, "Reaper: DB appears down", "worker_id", w.id)
		} else {
			w.logger.ErrorContext(w.ctx, "Reaper error", "worker_id", w.id, "error", lastErr)
		}
		w.Metrics.RecordBackgroundLoop(w.ctx, "reaper", "error")
		w.Metrics.SetBackgroundLoopDuration(w.ctx, "reaper", time.Since(reaperStart).Seconds())
		return
	}
	w.recordDBContactOK()
	if reaped > 0 {
		w.logger.InfoContext(w.ctx, "Reaper: reclaimed stale instances", "worker_id", w.id, "count", reaped)
		w.Metrics.SetBackgroundLoopItemsProcessed(w.ctx, "reaper", int64(reaped))
		// The fleet-wide reclaim counter. It had NO call site until
		// now, so cleat_reaper_instances_claimed_total has never
		// emitted a sample -- despite migration 052 and
		// engine/reclaim_count_records_reclaims_only_test.go both
		// describing it as an existing counter that "counts every row
		// it takes". cleat#1317.
		//
		// Fed here rather than inside ReapStaleInstances: the store
		// has no Metrics handle, and this is the one funnel every
		// dialect's reaper returns through.
		w.Metrics.RecordReaperInstancesClaimed(w.ctx, int64(reaped))
	}
	// A full tick is the signal worth surfacing, and it is the only
	// place this is observable: the sweep bounded at the limit looks
	// exactly like a sweep that found precisely that many. An
	// ordinary failure does not fill a tick -- 200 is twenty workers'
	// worth at the default concurrency -- so this means either a
	// stall aged the whole running set past the threshold at once, or
	// there is more to reclaim than one tick can carry. Both want an
	// operator's attention, and neither is visible from the count
	// alone. cleat#1320.
	if w.maxReclaimPerTick > 0 && reaped >= w.maxReclaimPerTick {
		w.logger.WarnContext(w.ctx, "Reaper: hit the per-tick reclaim limit; more instances remain stale",
			"worker_id", w.id, "count", reaped, "limit", w.maxReclaimPerTick,
			"hint", "a whole-set stall (a boot migration holding a lock, a failover, a paused volume) ages every heartbeat at once; recovery continues on later ticks")
	}
	w.expireDeferPhases()
	w.Metrics.RecordBackgroundLoop(w.ctx, "reaper", "ok")
	w.Metrics.SetBackgroundLoopDuration(w.ctx, "reaper", time.Since(reaperStart).Seconds())
}

// expireDeferPhases bounds the number of ATTEMPTS a defer phase gets, which is
// a different job from the sweep it rides along with.
//
// The stale-instance sweep above handles a worker that DIED mid-phase: the
// heartbeat goes stale, the workflow returns to 'terminating' and another
// worker replays it. That is the right answer once and the wrong answer
// forever -- a guest that traps every time it replays would be re-queued on
// every tick, and the workflow would never leave 'terminating'. Past
// defer_phase_deadline this applies the outcome that was recorded when the
// transition was marked, without the cleanup. A terminate that cannot run its
// defers must still terminate.
//
// It shares the reaper's tick rather than having its own loop: both are "sweep
// rows whose time is up", the deadline is minutes and the tick is seconds, and
// a second goroutine would buy nothing but another thing to shut down.
//
// A store that cannot expire is silent, not an error. It is also a store that
// cannot MARK a phase -- TerminateWorkflow and the DeferPhaseStore pair ship
// together -- so it has nothing to sweep.
func (w *Worker) expireDeferPhases() {
	dps, ok := w.store.(engine.DeferPhaseStore)
	if !ok {
		return
	}
	n, err := dps.ExpireDeferPhases(w.ctx)
	if err != nil {
		if isConnectionError(err) {
			w.logger.WarnContext(w.ctx, "Defer-phase deadline sweep: DB appears down", "worker_id", w.id)
		} else {
			w.logger.ErrorContext(w.ctx, "Defer-phase deadline sweep error", "worker_id", w.id, "error", err)
		}
		return
	}
	if n > 0 {
		w.logger.WarnContext(w.ctx, "Defer-phase deadline sweep: terminated workflows whose cleanup did not finish in time",
			"worker_id", w.id, "count", n)
	}
}

// reapIdleTenantPools releases what both sources will give up, and reports the
// total.
//
// SEPARATE FROM THE LOOP ON PURPOSE. The loop's interval is five minutes, so a
// test of the tick body through the ticker would have to either wait or fake
// time; a test of the loop's WIRING that builds its own two sources proves
// only that the test can add. Extracting the decision means a test can hand a
// worker a factory and a pool manager and assert which of them got reaped --
// the same lesson as perTenantPoolCeiling in connection_budget.go.
func (w *Worker) reapIdleTenantPools(idleWindow time.Duration) int {
	evicted := 0
	if w.tenantPools != nil {
		evicted += w.tenantPools.EvictIdle(idleWindow)
	}
	if reaper, ok := w.storeFactory.(engine.TenantPoolReaper); ok {
		// The store factory's pools are LEASED, so this closes only the ones
		// no store is holding -- a workflow sitting inside a long activity
		// keeps its tenant's pool whatever the clock says. See
		// engine.TenantPoolReaper.
		evicted += reaper.EvictIdle(idleWindow)
	}
	return evicted
}

// hasReapablePools reports whether this worker holds per-tenant pools that can
// be released without stopping the process. See tenantPoolReaperLoop for the
// two sources and why they are reaped together.
func (w *Worker) hasReapablePools() bool {
	if w.tenantPools != nil {
		return true
	}
	_, ok := w.storeFactory.(engine.TenantPoolReaper)
	return ok
}

// tenantPoolReaperLoop releases tenant pools nobody has touched.
//
// IT IS HOUSEKEEPING, NOT A BUDGET, and the distinction is the whole of
// cleat#1470. A timer-driven sweep can only shrink an overshoot after the fact:
// between two ticks the pool count is whatever demand made it. A connection
// budget has to hold an INVARIANT, and an invariant is enforced where it would
// be violated -- admission, in TenantPools.For -- which is why the repository
// owner rejected TTL as the mechanism. This loop exists so that a worker which
// served a thousand tenants overnight is not still holding a thousand pool
// objects at breakfast; it is not what will keep the worker under a limit, and
// wiring a limit to it would rebuild the rejected design under a new name.
//
// THE IDLE WINDOW IS LONG ON PURPOSE. Fifteen minutes is three times the pools'
// own SetConnMaxLifetime, so by the time a pool is evicted its connections have
// already been closed by database/sql and what is reclaimed is the struct and
// the map entry. Evicting sooner would trade a real reconnect -- role lookup,
// TCP, TLS, auth -- for a few bytes.
//
// IT REAPS BOTH SOURCES OF PER-TENANT POOL. cleat#1928 added the second: the
// store factory's own pools on MySQL and SQL Server, which had no reaper and so
// accumulated one *sql.DB -- and the connection-opener goroutine database/sql
// runs behind it -- per tenant the worker had ever served. They are reaped
// together because they are the same fact about a worker ("what is this tenant
// still costing me when nobody is asking for it"), and splitting them into two
// loops would mean two health entries, two intervals and two chances for one of
// them to be the one that was never launched.
func (w *Worker) tenantPoolReaperLoop() {
	defer w.wg.Done()
	const (
		interval   = 5 * time.Minute
		idleWindow = 15 * time.Minute
	)
	w.healthTracker.setInterval("tenant_pool_reaper", interval)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-w.getLoopCtx("tenant_pool_reaper").Done():
			return
		case <-ticker.C:
			w.healthTracker.recordRun("tenant_pool_reaper")
			start := time.Now()
			// No error to classify: eviction touches no database. Closing the
			// pools happens asynchronously inside EvictIdle precisely so this
			// tick cannot block on another tenant's in-flight query.
			evicted := w.reapIdleTenantPools(idleWindow)
			if evicted > 0 {
				w.logger.InfoContext(w.ctx, "Tenant pool reaper: released idle pools",
					"worker_id", w.id, "count", evicted, "idle_window", idleWindow)
				w.Metrics.SetBackgroundLoopItemsProcessed(w.ctx, "tenant_pool_reaper", int64(evicted))
			}
			w.Metrics.RecordBackgroundLoop(w.ctx, "tenant_pool_reaper", "ok")
			w.Metrics.SetBackgroundLoopDuration(w.ctx, "tenant_pool_reaper", time.Since(start).Seconds())
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

			// Tenant stores opened while firing this tick's schedules, released
			// together when the tick ends.
			//
			// NOT DEFERRED, because the enclosing function is the schedule loop
			// and it does not return until the worker stops -- a defer here
			// would pin every tenant's pool for the life of the process, which
			// is the leak cleat#1928 is about, rebuilt one level up. NOT
			// RELEASED AT EACH `continue` EITHER: the body below has six of
			// them at three nesting depths, and the cost of missing one is an
			// invisible pinned pool. Collecting them makes the release
			// unmissable and bounds the hold to a single tick.
			var tickStoreReleases []func()
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
				schStore, releaseSchStore, terr := w.storeForTenant(tenantID)
				tickStoreReleases = append(tickStoreReleases, releaseSchStore)
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
				if errors.Is(serr, engine.ErrIdempotencyKeyInputMismatch) {
					// A DIFFERENT input under the same (schedule, instant) key.
					// For an HTTP caller that is a refusal (cleat#1170): their
					// key derivation does not capture something their requests
					// distinguish, and replaying would hand them a run started
					// with someone else's arguments.
					//
					// Here it is neither surprising nor the caller's mistake.
					// The key is (schedule, tenant, scheduled instant) by
					// design, so two workers racing one firing derive the same
					// key -- and if the schedule's input was edited between
					// their reads of the row, they present different payloads
					// for the same firing. The contract this loop owes is ONE
					// RUN PER INSTANT, and that run already exists.
					//
					// Treating it as an error would be worse than useless: the
					// branch below leaves the schedule due, the retry reads the
					// NEW input, and it mismatches the stored digest again --
					// for as long as the key lives, which is 30 days. A
					// schedule edited at the wrong moment would wedge.
					// serr only. Setting alreadyExisted would ALSO fire the
					// suppression log below, twice-counting the metric and
					// printing an empty workflow_id -- this branch does not
					// learn which run won.
					//
					// runID stays "", which ClaimDueSchedule already models:
					// `last_run_id = CASE WHEN $5 = '' THEN last_run_id ELSE $5
					// END` leaves the column alone rather than blanking it.
					w.logger.InfoContext(w.ctx, "Scheduler: duplicate firing suppressed at admission; the schedule's input changed between two readings of the same firing", "worker_id", w.id, "schedule", sch.Name, "scheduled_at", scheduled.Format(time.RFC3339))
					w.Metrics.SetBackgroundLoopItemsProcessed(w.ctx, "schedule_duplicates_suppressed", 1)
					serr = nil
				}
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
			for _, release := range tickStoreReleases {
				release()
			}
			w.scheduleMu.Unlock()
			w.Metrics.RecordBackgroundLoop(w.ctx, "schedule", "ok")
			w.Metrics.SetBackgroundLoopDuration(w.ctx, "schedule", time.Since(schStart).Seconds())
			w.Metrics.SetBackgroundLoopItemsProcessed(w.ctx, "schedule", int64(len(schedules)))
		}
	}
}

// metricsSweepLoop publishes the four gauges that no other code path feeds.
//
// The queries already existed (engine/db_metrics.go) and MetricsStore already
// declared the way to reach them. Nothing called it, so cleat_workflows_stuck,
// cleat_event_history_size_bytes, cleat_event_history_rows and
// cleat_concurrency_keys_total emitted NO SERIES -- an OTel gauge that is never
// Set is absent, not zero, so an alert on any of them could never fire and a
// panel read "No data" rather than a stale value. cleat#1317.
//
// ONE SWEEP FOR FOUR GAUGES rather than hanging each off a related loop. They
// are all "ask the database a question nothing else asks", their cost is the
// query rather than the publish, and a single interval is one thing for an
// operator to turn down when it is too expensive. --metrics-sweep-interval 0
// disables it, and the loop is then never registered at all.
//
// A FAILED SWEEP LEAVES THE PREVIOUS VALUE STANDING, which is the right
// behaviour and worth naming: these are gauges, so not publishing is not the
// same as publishing zero. Reporting zero stuck workflows because the count
// query failed would be the lying-metric failure this whole area keeps paying
// for. The error is logged and the loop reports "error" to the health tracker.
func (w *Worker) metricsSweepLoop() {
	defer w.wg.Done()
	w.healthTracker.setInterval("metrics_sweep", w.metricsSweepInterval)
	ticker := time.NewTicker(w.metricsSweepInterval)
	defer ticker.Stop()

	// Asserted ONCE, not per tick: it is a static property of the store, and
	// the answer is worth a line in the log either way. A store that cannot
	// answer these leaves the gauges absent, and absent is exactly what a
	// healthy system looks like -- so an operator on a dialect that does not
	// implement MetricsStore would otherwise have no way to tell "nothing is
	// stuck" from "nobody is counting". MySQL and SQL Server are in that
	// position today.
	ms, ok := w.store.(MetricsStore)
	if !ok {
		w.logger.WarnContext(w.ctx,
			"metrics sweep: this store does not implement MetricsStore, so the stuck-workflow, "+
				"event-history and concurrency-key gauges will emit no series; alerts on them "+
				"cannot fire and panels will read \"No data\" rather than zero",
			"worker_id", w.id, "store", fmt.Sprintf("%T", w.store))
		return
	}

	for {
		select {
		case <-w.getLoopCtx("metrics_sweep").Done():
			return
		case <-ticker.C:
			w.healthTracker.recordRun("metrics_sweep")
			sweepStart := time.Now()
			failed := 0

			if n, err := ms.CountStalledWorkflows(w.ctx, w.stallThreshold); err != nil {
				failed++
				w.logger.ErrorContext(w.ctx, "metrics sweep: count stalled workflows", "worker_id", w.id, "error", err)
			} else {
				w.Metrics.SetWorkflowsStuck(w.ctx, int64(n))
			}

			if n, err := ms.CountEventHistoryTotal(w.ctx); err != nil {
				failed++
				w.logger.ErrorContext(w.ctx, "metrics sweep: count event history", "worker_id", w.id, "error", err)
			} else {
				w.Metrics.SetEventHistoryRowCount(w.ctx, int64(n))
			}

			if b, err := ms.EstimateEventHistorySize(w.ctx); err != nil {
				failed++
				w.logger.ErrorContext(w.ctx, "metrics sweep: estimate event history size", "worker_id", w.id, "error", err)
			} else {
				// Empty workflow name: the gauge is labelled per workflow but
				// pg_total_relation_size is whole-table, so there is no name to
				// give it. "" is the established unlabelled value here --
				// RecordDispatchLatency passes it for the same reason. Making
				// this per-workflow would mean a size query per definition,
				// which is the cost this estimator exists to avoid.
				w.Metrics.SetEventHistorySize(w.ctx, b, "")
			}

			if n, err := ms.CountActiveConcurrencyKeys(w.ctx); err != nil {
				failed++
				w.logger.ErrorContext(w.ctx, "metrics sweep: count active concurrency keys", "worker_id", w.id, "error", err)
			} else {
				w.Metrics.SetConcurrencyKeysTotal(w.ctx, int64(n))
			}

			// The leading indicator, on the same sweep as the total it sits
			// beside. Both come from the same table, so asking twice here costs
			// one extra query rather than a second loop.
			if n, err := ms.CountConcurrencyKeysExpiringSoon(w.ctx, w.keyExpiryWindow); err != nil {
				failed++
				w.logger.ErrorContext(w.ctx, "metrics sweep: count concurrency keys expiring soon", "worker_id", w.id, "error", err)
			} else {
				w.Metrics.SetConcurrencyKeysExpiringSoon(w.ctx, int64(n))
			}

			status := "ok"
			if failed > 0 {
				status = "error"
			}
			w.Metrics.RecordBackgroundLoop(w.ctx, "metrics_sweep", status)
			w.Metrics.SetBackgroundLoopDuration(w.ctx, "metrics_sweep", time.Since(sweepStart).Seconds())
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
			if err := w.memoryController.LoadEstimates(w.ctx, w.storeTenantID); err != nil {
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
// Why the default differs between the two: --retention-days was written to
// delete event_history rows -- the step-by-step replay log of a workflow that
// has already reached a terminal state -- leaving the workflow's outcome
// (status, result, error, def_name) untouched in workflow_instances. That is
// a safe thing to default on, which is why it is on.
//
// THIS COMMENT USED TO SAY IT "NO LONGER DOES THAT" (cleat#1016), reasoning
// that finalize_workflow_status purges events at 'done' or 'failed' before
// this sweep ever looks. That was wrong about 'failed': finalize_workflow_status
// purges a 'done' workflow's events, but a 'failed' workflow's events go
// through store.FailWorkflow instead, which purges nothing -- so this sweep is
// what removes them, and it still does real, first-hand work on any deployment
// that fails workflows. See PostgresStore.DeleteExpiredEvents's doc comment in
// engine/db.go for the full correction (cleat#2038).
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
func (w *Worker) retentionLoop(retentionDays, completedWorkflowRetentionDays, deadLetterRetentionDays int) {
	defer w.wg.Done()
	if retentionDays <= 0 && completedWorkflowRetentionDays <= 0 && deadLetterRetentionDays <= 0 {
		return
	}
	interval := w.retentionInterval
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	w.healthTracker.setInterval("retention", interval)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// SWEEP ONCE BEFORE THE FIRST TICK. Every other loop here is tick-first
	// and none of them pre-runs, which is the house style and is right for
	// them: the next-longest interval is memoryCleanupLoop's 10 minutes, so a
	// worker that dies before its first tick has barely started.
	//
	// Retention's period is 24 hours -- 144x that -- and a worker restarting
	// inside a day is ordinary rather than exceptional. Tick-first at this
	// interval means a deploy cadence under 24h disables retention entirely,
	// while --retention-days reports it as on and the health tracker reports
	// the loop as running. cleat#1002.
	//
	// Safe to run unconditionally because the sweep is idempotent and bounded
	// by its own cutoff: a second run within the same day deletes nothing the
	// first did not, so the cost of an unnecessary one is a query.
	w.healthTracker.recordRun("retention")
	w.runRetentionSweep(retentionDays, completedWorkflowRetentionDays, deadLetterRetentionDays)

	for {
		select {
		case <-w.getLoopCtx("retention").Done():
			return
		case <-ticker.C:
			w.healthTracker.recordRun("retention")
			w.runRetentionSweep(retentionDays, completedWorkflowRetentionDays, deadLetterRetentionDays)
		}
	}
}

// versionGCLoop collects deprecated workflow-definition versions on a ticker.
// cleat#1315.
//
// OFF BY DEFAULT, and the reason is the same one --completed-workflow-retention-days
// gives for defaulting to 0: this deletes workflow DEFINITIONS permanently, and
// an in-flight instance whose version has been collected cannot find the WASM
// binary to replay against. The module cache is keyed by def_name:def_version,
// so the failure lands on a running workflow rather than at the point of
// deletion. That is materially more destructive than clearing compaction state,
// which is why --retention-days ships on and this does not.
//
// Until this loop existed GC ran only when a person invoked it, through
// `cleatctl versions gc` or POST /api/versions/gc, and its policy was
// engine.DefaultGCOptions() -- compiled in and unreachable from either surface.
//
// TICK-FIRST, NOT SWEEP-FIRST, which is the opposite of retentionLoop and
// deliberate. retentionLoop pre-runs because its 24-hour period means a deploy
// cadence under a day would disable it entirely (cleat#1002). That argument
// does not transfer: this sweep is opt-in, so an operator who set the flag
// chose the cadence, and a pre-run would make every worker restart delete
// definitions immediately -- turning a rolling deploy into a burst of
// destructive sweeps, on a knob whose whole point is that the operator controls
// when it fires.
func (w *Worker) versionGCLoop() {
	defer w.wg.Done()
	if w.versionGCInterval <= 0 {
		return
	}
	// Registered HERE, not by initLoopCtx at startup -- Run() calls
	// initLoopCtxUnmonitored("version_gc") so launchLoop always has a context
	// to start this goroutine with, but the health tracker only learns about
	// this loop once it has passed the check above and is actually going to
	// keep running. cleat#2004: registering unconditionally made a disabled
	// loop (the default) permanently stale six lines from now, in the health
	// tracker's eyes, without ever reaching this line.
	w.healthTracker.registerLoop("version_gc")
	w.healthTracker.setInterval("version_gc", w.versionGCInterval)
	ticker := time.NewTicker(w.versionGCInterval)
	defer ticker.Stop()

	for {
		select {
		case <-w.getLoopCtx("version_gc").Done():
			return
		case <-ticker.C:
			w.healthTracker.recordRun("version_gc")
			w.runVersionGCSweep()
		}
	}
}

// runVersionGCSweep runs one GC pass with the worker's configured policy.
// Split out of versionGCLoop so a test can call it without waiting on the
// ticker, the same seam runRetentionSweep provides for retention.
func (w *Worker) runVersionGCSweep() {
	opts := engine.DefaultGCOptions()
	if w.versionGCMinVersions > 0 {
		opts.MinVersionsToKeep = w.versionGCMinVersions
	}
	if w.versionGCMaxAge > 0 {
		opts.MaxVersionAge = w.versionGCMaxAge
	}
	result, err := engine.GarbageCollectVersions(w.ctx, w.store, opts)
	if err != nil {
		w.logger.ErrorContext(w.ctx, "version gc failed", "worker_id", w.id, "error", err)
		return
	}
	// Logged even at zero, because "the sweep ran and found nothing" and "the
	// sweep is disabled" are the two states an operator most needs to tell
	// apart -- the distinction cleat#1315's troubleshooting entry exists for.
	w.logger.InfoContext(w.ctx, "version gc swept",
		"worker_id", w.id,
		"versions_removed", result.VersionsRemoved,
		"versions_skipped", result.VersionsSkipped,
		"min_versions_to_keep", opts.MinVersionsToKeep,
		"max_version_age", opts.MaxVersionAge.String(),
	)
}

// runRetentionSweep runs one iteration of both retention sweeps. Split out
// of retentionLoop so it is callable directly from a test without waiting on
// the loop's 24-hour ticker.
// retentionSweepResult reports what one sweep did, per arm.
//
// Per arm and never summed. The four arms delete from different tables under
// different flags that default differently -- --retention-days is on at 30,
// the other two are off at 0 -- so one total would be a number that means four
// things, and an operator could not tell "nothing was old enough" from "that
// arm is disabled". runRetentionSweep's own comment already refuses to sum two
// of them for the same reason; this carries that out to the API.
type retentionSweepResult struct {
	EventsDeleted          int64 `json:"events_deleted"`
	CompactionStateCleared int64 `json:"compaction_state_cleared"`
	CompletedWorkflows     int64 `json:"completed_workflows_deleted"`
	DeadLetteredWorkflows  int64 `json:"dead_lettered_workflows_deleted"`

	// Skipped names the arms that did not run because their flag is 0, so a
	// zero count is never ambiguous between "disabled" and "found nothing".
	Skipped []string `json:"skipped,omitempty"`
	// Errors names arms that failed. The sweep is best-effort per arm -- one
	// failing must not stop the others -- so an empty result with no errors
	// and no skips means the sweep ran and found nothing.
	Errors []string `json:"errors,omitempty"`

	// DryRun marks the counts above as a PREVIEW: nothing was deleted.
	//
	// Present and true only on a preview, so an existing caller parsing a real
	// sweep sees the field it always saw. A reader who ignores this field reads
	// a preview as a sweep that deleted things, which is the wrong direction to
	// be wrong in -- hence the field rather than a separate response shape,
	// which cleat#1457 chose so a preview and the sweep that follows it can be
	// diffed against each other.
	DryRun bool `json:"dry_run,omitempty"`
}

// runRetentionSweepWindow is runRetentionSweep with the window supplied rather
// than derived from the day-granularity flags.
//
// WHY A WINDOW OVERRIDE EXISTS AT ALL, because a trigger without one would be
// inert. The flags are integer DAYS and 0 disables, so the smallest window the
// configuration can express is 24 hours -- and the sweep predicate is
// `completed_at < cutoff`. An endpoint that ran the configured sweep on demand
// would therefore match nothing for any run completed today, on every call,
// and report success while doing so. That is the shape this repository has
// spent a lot of effort on elsewhere: the operation reports success without
// doing the thing, and the report is the CORRECT report.
//
// The override does NOT enable a disabled arm. A flag at 0 is a deliberate
// decision -- --completed-workflow-retention-days deletes the workflow record
// itself and is off by default for that reason -- and a request body is not
// the place to reverse it. Those arms are named in Skipped instead.
func (w *Worker) runRetentionSweepWindow(retentionDays, completedWorkflowRetentionDays, deadLetterRetentionDays int, window time.Duration) retentionSweepResult {
	var res retentionSweepResult
	sweptAt := time.Now()
	at := func(days int) time.Time {
		if window > 0 {
			return sweptAt.Add(-window)
		}
		return sweptAt.Add(-time.Duration(days) * 24 * time.Hour)
	}
	if retentionDays > 0 {
		cutoff := at(retentionDays)
		if n, err := w.store.DeleteExpiredEvents(w.ctx, cutoff); err != nil {
			res.Errors = append(res.Errors, "events: "+err.Error())
		} else {
			res.EventsDeleted = n
		}
		if n, err := w.store.ClearExpiredCompactionState(w.ctx, cutoff); err != nil {
			res.Errors = append(res.Errors, "compaction_state: "+err.Error())
		} else {
			res.CompactionStateCleared = n
		}
	} else {
		res.Skipped = append(res.Skipped, "events and compaction_state (--retention-days is 0)")
	}
	if completedWorkflowRetentionDays > 0 {
		if n, err := w.store.DeleteCompletedWorkflows(w.ctx, at(completedWorkflowRetentionDays)); err != nil {
			res.Errors = append(res.Errors, "completed_workflows: "+err.Error())
		} else {
			res.CompletedWorkflows = n
		}
	} else {
		res.Skipped = append(res.Skipped, "completed_workflows (--completed-workflow-retention-days is 0)")
	}
	if deadLetterRetentionDays > 0 {
		if n, err := w.store.DeleteDeadLetteredWorkflows(w.ctx, at(deadLetterRetentionDays)); err != nil {
			res.Errors = append(res.Errors, "dead_lettered: "+err.Error())
		} else {
			res.DeadLetteredWorkflows = n
		}
	} else {
		res.Skipped = append(res.Skipped, "dead_lettered (--dead-letter-retention-days is 0)")
	}
	w.Metrics.SetRetentionLastRunTimestamp(w.ctx, time.Now().Unix())
	return res
}

// previewRetentionSweepWindow reports what runRetentionSweepWindow WOULD remove,
// without removing anything. cleat#1457.
//
// Retention is the least reversible of cleat's destructive operations and was
// the only one without a preview: cleatctl drop-tenant, revokeapikey, the
// version GC and --uninstall-dry-run all have one, and none of them deletes
// event history. The endpoint accepts an older_than override, so an operator can
// scope a sweep to any window -- which is exactly the case where the blast
// radius is worth seeing first, and exactly the case that had no way to see it.
//
// IT MIRRORS THE SWEEP ARM FOR ARM, INCLUDING THE SKIPS. A disabled arm is
// reported in Skipped here exactly as it is there, because a preview whose zero
// could mean either "disabled" or "nothing matched" reintroduces the ambiguity
// retentionSweepResult's own doc comment exists to prevent -- and it would do so
// in the report an operator reads BEFORE deciding whether to run the real thing.
//
// THE COUNTS ARE BEST EFFORT AND THE RESPONSE SAYS SO. They are read at one
// instant from a database other workers are writing to; by the time the sweep
// runs, workflows have completed and rows have aged past the cutoff. The number
// is the right size for catching a mistyped older_than, which is what this is
// for. It is not a promise about which rows a later sweep will delete, and an
// operator who diffs a preview against the sweep that follows it should expect
// them to differ by whatever the workload did in between.
func (w *Worker) previewRetentionSweepWindow(retentionDays, completedWorkflowRetentionDays, deadLetterRetentionDays int, window time.Duration) retentionSweepResult {
	res := retentionSweepResult{DryRun: true}
	previewedAt := time.Now()
	at := func(days int) time.Time {
		if window > 0 {
			return previewedAt.Add(-window)
		}
		return previewedAt.Add(-time.Duration(days) * 24 * time.Hour)
	}
	if retentionDays > 0 {
		cutoff := at(retentionDays)
		if n, err := w.store.CountExpiredEvents(w.ctx, cutoff); err != nil {
			res.Errors = append(res.Errors, "events: "+err.Error())
		} else {
			res.EventsDeleted = n
		}
		if n, err := w.store.CountExpiredCompactionState(w.ctx, cutoff); err != nil {
			res.Errors = append(res.Errors, "compaction_state: "+err.Error())
		} else {
			res.CompactionStateCleared = n
		}
	} else {
		res.Skipped = append(res.Skipped, "events and compaction_state (--retention-days is 0)")
	}
	if completedWorkflowRetentionDays > 0 {
		if n, err := w.store.CountCompletedWorkflows(w.ctx, at(completedWorkflowRetentionDays)); err != nil {
			res.Errors = append(res.Errors, "completed_workflows: "+err.Error())
		} else {
			res.CompletedWorkflows = n
		}
	} else {
		res.Skipped = append(res.Skipped, "completed_workflows (--completed-workflow-retention-days is 0)")
	}
	if deadLetterRetentionDays > 0 {
		if n, err := w.store.CountDeadLetteredWorkflows(w.ctx, at(deadLetterRetentionDays)); err != nil {
			res.Errors = append(res.Errors, "dead_lettered: "+err.Error())
		} else {
			res.DeadLetteredWorkflows = n
		}
	} else {
		res.Skipped = append(res.Skipped, "dead_lettered (--dead-letter-retention-days is 0)")
	}
	// Deliberately NOT SetRetentionLastRunTimestamp: nothing ran. A preview that
	// moved the last-run clock would make an operator checking a preview look
	// like a retention pass to every dashboard reading that metric.
	return res
}

func (w *Worker) runRetentionSweep(retentionDays, completedWorkflowRetentionDays, deadLetterRetentionDays int) {
	// ONE clock reading for the whole sweep. Each arm used to call time.Now()
	// itself, so the three cutoffs differed by microseconds -- harmless in
	// effect, but they describe one retention window and a test asserting they
	// agree should not have to tolerate a gap. Reading the clock once removes
	// the question rather than widening an assertion around it.
	sweptAt := time.Now()
	if retentionDays > 0 {
		cutoff := sweptAt.Add(-time.Duration(retentionDays) * 24 * time.Hour)
		deleted, err := w.store.DeleteExpiredEvents(w.ctx, cutoff)
		if err != nil {
			w.logger.ErrorContext(w.ctx, "retention: error deleting expired events", "worker_id", w.id, "error", err)
		} else if deleted > 0 {
			w.Metrics.RecordEventsDeleted(w.ctx, deleted)
			w.logger.InfoContext(w.ctx, "retention: deleted expired event rows", "worker_id", w.id, "count", deleted)
		}
	}

	// The compaction-state half of the same cutoff, reported separately.
	//
	// It used to live inside DeleteExpiredEvents and its row count was thrown
	// away, so the sweep logged "deleted 0" on runs where it had cleared
	// thousands of workflow_instances rows -- and on a deployment where every
	// terminal workflow is 'done', that "0" was structurally guaranteed,
	// because finalize_workflow_status already purged those events. THIS IS
	// NOT TRUE OF 'failed' WORKFLOWS (cleat#1016's claim that the event half
	// "can never match" was wrong there -- see cleat#2038 and
	// engine/db.go's DeleteExpiredEvents doc comment). They are counted apart
	// rather than summed because they are different tables and different
	// operations; one counter reporting both would be a number that means two
	// things.
	if retentionDays > 0 {
		cutoff := sweptAt.Add(-time.Duration(retentionDays) * 24 * time.Hour)
		cleared, err := w.store.ClearExpiredCompactionState(w.ctx, cutoff)
		if err != nil {
			w.logger.ErrorContext(w.ctx, "retention: clear expired compaction state", "worker_id", w.id, "error", err)
		} else if cleared > 0 {
			w.Metrics.RecordCompactionStateCleared(w.ctx, cleared)
			w.logger.InfoContext(w.ctx, "retention: cleared compaction state",
				"worker_id", w.id, "count", cleared, "older_than", cutoff)
		}
	}
	if completedWorkflowRetentionDays > 0 {
		cutoff := sweptAt.Add(-time.Duration(completedWorkflowRetentionDays) * 24 * time.Hour)
		deleted, err := w.store.DeleteCompletedWorkflows(w.ctx, cutoff)
		if err != nil {
			w.logger.ErrorContext(w.ctx, "retention: error deleting completed workflows", "worker_id", w.id, "error", err)
		} else if deleted > 0 {
			w.Metrics.RecordWorkflowsPurged(w.ctx, deleted)
			w.logger.InfoContext(w.ctx, "retention: deleted completed workflow rows", "worker_id", w.id, "count", deleted)
		}
	}

	// Dead-lettered workflows are deliberately excluded from the sweep above --
	// DeleteCompletedWorkflows covers 'done', 'failed' and 'terminated' and
	// says in its own doc that dead_lettered "has its own lifecycle and its own
	// deletion path". That path is DeleteDeadLetteredWorkflows, and until
	// cleat#1023 nothing called it: the method existed on all three stores,
	// cascaded correctly, and had no flag and no caller. The design said these
	// rows have their own lifecycle and then shipped no way to express one.
	//
	// Opt-in like its sibling, and for a stronger reason. Deleting a
	// dead-lettered workflow destroys exactly the record an operator kept it
	// for -- it is the run they most want to inspect -- so defaulting this on
	// would silently reverse a documented decision on every existing
	// deployment.
	if deadLetterRetentionDays > 0 {
		cutoff := sweptAt.Add(-time.Duration(deadLetterRetentionDays) * 24 * time.Hour)
		deleted, err := w.store.DeleteDeadLetteredWorkflows(w.ctx, cutoff)
		if err != nil {
			w.logger.ErrorContext(w.ctx, "retention: delete dead-lettered workflows", "worker_id", w.id, "error", err)
		} else if deleted > 0 {
			w.logger.InfoContext(w.ctx, "retention: deleted dead-lettered workflows",
				"worker_id", w.id, "count", deleted, "older_than", cutoff)
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

// updateDispatchLoop and dispatchPendingUpdates lived here until updates were
// implemented end to end. Both are gone, and it is worth recording why rather
// than only that.
//
// The loop was a 5-second ticker over w.inflight, which is populated only for
// the lifetime of ONE SEGMENT -- executeWorkflow stores the entry on claim and
// deletes it on return. Segments run in tens to hundreds of milliseconds, so
// the delivery condition was "the ticker happens to fire mid-segment", which is
// a fraction of a percent of wall-clock time for a busy workflow and exactly
// zero for one waiting on a sleep, a signal, a promise or a child. Measured on
// PostgreSQL: 5 runs, 0 dispatched (cleat#849).
//
// Even in the lucky case it delivered nothing, because it called
// Engine.DispatchUpdate, which needed an updateHandler that nothing ever
// configured -- so the request would have been completed with "no update
// handler configured for this engine" and the caller's promise REJECTED. A fix
// for the scheduling alone, verified by "the request is no longer pending",
// would have read as success while delivering nothing.
//
// That hook is gone too: WithUpdateHandler and Engine.DispatchUpdate were
// exported API with no caller outside their own tests, and keeping them meant
// keeping two names that read as the update path and are not.
//
// Updates are now delivered inside the segment, by the guest, at dispatch
// points -- see cleat.HostCallsImpl.DispatchUpdates and engine/updater.go.
// Nothing polls from outside the workflow any more, which is what lets the
// delivery be an event in the history and therefore replayable.

// loadWASM returns the raw WASM bytes for one tenant's workflow definition.
//
// # It takes the store rather than using w.store, and that is the whole point
//
// Every other read and write in executeWorkflow goes through execStore, the
// store resolved for the WORKFLOW's tenant. This call did not, and it was the
// only one that did not. That matters more here than it would almost anywhere
// else, because PostgresStore.LoadWASM's statement is
//
//	SELECT wasm_bytes FROM workflow_defs WHERE name = $1 AND version = $2
//
// with no tenant predicate at all (engine/store_deployment.go). Isolation is
// entirely row-level security, and the RLS tenant is a property of the store
// instance -- setRLSOnTx interpolates s.tenantID (engine/db.go) -- not of the
// context. So reading through the worker's own store filtered every other
// tenant's definition out, ErrNoRows became "wasm not found", and the run was
// failed terminally. Every workflow belonging to a tenant other than the
// worker's died that way, while its history, state and trace were being
// written correctly through the right store. cleat#1931.
//
// # cacheTenant is not always the workflow's tenant
//
// storeForTenant returns the worker's own store when the workflow's tenant is
// unset, so for those rows the bytes come from w.storeTenantID's store and
// must be cached under that id. Keying on the workflow's "" instead would
// file one worker's definitions under a name that means "whoever asked",
// which is the same collision in a smaller costume.
func (w *Worker) loadWASM(store engine.WorkflowStore, cacheTenant, defName string, defVersion int) ([]byte, error) {
	// THE TENANT IS IN THE KEY. Without it the two caches below answer a
	// lookup of ("orders", 1) with whichever tenant stored those bytes first,
	// and definition names are caller-chosen. Note the ordering hazard: the
	// caches are consulted BEFORE the store, so routing the store correctly
	// without also keying the caches would convert the failure above into a
	// silent cross-tenant execution -- strictly worse. Both halves are one
	// change for that reason.
	key := cacheTenant + "/" + defName + ":" + strconv.Itoa(defVersion)

	// Check in-memory cache first.
	if cached, ok := w.wasmCache.get(key); ok {
		dbLen, err := store.GetWASMLength(w.ctx, defName, defVersion)
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
		if cached := w.wasmDiskCache.LookupDef(cacheTenant, defName, defVersion); cached != nil {
			w.Metrics.RecordWasmCacheMiss(w.ctx)
			w.wasmCache.put(key, cached)
			return cached, nil
		}
	}

	w.Metrics.RecordWasmCacheMiss(w.ctx)

	wasmBytes, err := store.LoadWASM(w.ctx, defName, defVersion)
	if err != nil {
		return nil, err
	}

	// Store to disk cache for future restarts.
	if w.wasmDiskCache != nil {
		w.wasmDiskCache.StoreDef(cacheTenant, defName, defVersion, wasmBytes)
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
// failStrandedUpdates rejects every update request still pending against a
// workflow that has just reached a terminal status, and rejects the promise
// each one carries.
//
// A pending update is dispatched only while its workflow is mid-segment (see
// dispatchPendingUpdates and IMPROVEMENT-PLAN 3.238). Once the workflow is
// done, failed or terminated there is no future segment, so a request left
// pending at that moment can never be handled -- and the caller is holding the
// promise_id the API handed back with its 202, waiting on a promise nothing
// will ever settle. Rejecting it turns a permanent silent hang into an answer.
//
// Composed from GetPendingUpdateRequests + CompleteUpdateRequest +
// RejectPromise rather than added as a store method. All three are already on
// engine.WorkflowStore, so this needs no SQL and no per-dialect work -- the
// same reasoning as SendSignalAndWait becoming a composite in §3.220.
//
// Errors are logged and not returned. The workflow's terminal status is
// already committed; failing to tidy a stranded request must not change what
// happened to the workflow, and there is no caller here that could act on it.
func (w *Worker) failStrandedUpdates(wf *engine.WorkflowInstance, terminalStatus string) {
	ctx := context.Background()
	st, release := w.storeFor(wf)
	defer release()
	if st == nil {
		return
	}

	updates, err := st.GetPendingUpdateRequests(ctx, wf.ID)
	if err != nil {
		w.logger.ErrorContext(ctx, "error fetching pending updates to strand",
			"worker_id", w.id, "workflow_id", wf.ID, "error", err)
		return
	}
	if len(updates) == 0 {
		return
	}

	reason := fmt.Sprintf("workflow %s reached terminal status %q with this update still pending, "+
		"so it can never be handled", wf.ID, terminalStatus)

	// upd.RequestID, not upd.UpdateName. cleat#1416 made a name reusable, so a
	// workflow can strand SEVERAL requests sharing one -- and completing by
	// name would close all of them on the first iteration while rejecting only
	// the first promise, leaving the rest of these callers holding a promise
	// nothing settles. That is the failure this sweep exists to prevent, so it
	// would have been this loop reintroducing it.
	for _, upd := range updates {
		if cErr := st.CompleteUpdateRequest(ctx, wf.ID, upd.RequestID, "", reason); cErr != nil {
			w.logger.ErrorContext(ctx, "error failing stranded update",
				"worker_id", w.id, "workflow_id", wf.ID,
				"update_name", upd.UpdateName, "request_id", upd.RequestID, "error", cErr)
			continue
		}
		if upd.PromiseID == "" {
			continue
		}
		// RejectPromise is keyed by promise ID alone and reports not-found
		// rather than silently succeeding (#818), so a promise already settled
		// by some other path logs rather than being overwritten.
		if rErr := st.RejectPromise(ctx, upd.PromiseID, reason); rErr != nil {
			w.logger.ErrorContext(ctx, "error rejecting stranded update promise",
				"worker_id", w.id, "workflow_id", wf.ID, "update_name", upd.UpdateName,
				"promise_id", upd.PromiseID, "error", rErr)
		}
	}
	w.logger.InfoContext(ctx, "failed stranded update requests",
		"worker_id", w.id, "workflow_id", wf.ID, "terminal_status", terminalStatus, "count", len(updates))
}

// notifyTerminal performs the three side effects every settled workflow owes,
// regardless of which of the four terminal code paths produced the outcome:
// waking any parent awaiting this run, failing stranded update requests, and
// telling finalize observers (jobqueue, currently the only implementer of
// plugin.HasFinalizeObserver) the real status. cleat#1976.
//
// Before this, three of the four terminal paths reached only some of the
// three: the normal finalize path did all three but gated every one of them
// on `finalStatus == "done" || finalStatus == "failed"`, when finalStatus
// there is never "failed" -- so that half of every gate was dead. The
// terminal-failure path (recordTerminalFailureWithHistory) failed stranded
// updates but never woke the parent or notified observers, which is why
// jobqueue's abandonment sweep -- not ObserveFinalize -- was what eventually
// marked a failed run's job row, and marked it 'abandoned' rather than
// 'failed'. The panic path (releaseOrFail) did none of the three. Only
// finishDeferPhase did parent-wake and stranded-updates but never observers.
//
// finalStatus is passed straight to observers rather than narrowed to
// "done"/"failed": workflow_instances.status has five terminal values in
// production (engine/store_lifecycle.go's own census -- 'done', 'failed',
// 'dead_lettered', 'terminated', 'cancelled'), and an observer that only
// ever saw two of them could not tell a dead-lettered run from an ordinary
// failure, which is exactly the ambiguity jobqueue's own status column
// exists to remove. See jobqueue's ObserveFinalize for how it maps the
// other three.
func (w *Worker) notifyTerminal(wf *engine.WorkflowInstance, finalStatus string) {
	select {
	case w.parentWakeCh <- struct{}{}:
	default:
	}

	w.failStrandedUpdates(wf, finalStatus)

	for _, obs := range w.finalizeObservers {
		if obsErr := obs.ObserveFinalize(context.Background(), wf.ID, finalStatus); obsErr != nil {
			w.logger.ErrorContext(context.Background(), "finalize observer failed",
				"worker_id", w.id, "workflow_id", wf.ID, "plugin", obs.Info().Name, "error", obsErr)
		}
	}
}

// classifyTerminalErrorCode upgrades an unclassified terminal failure to
// retries_exhausted when the engine's own signal says that is what happened.
//
// Extracted so it can be tested as a rule rather than mirrored in a test file:
// a copy of a rule passes happily after the rule changes.
func classifyTerminalErrorCode(errorCode string, deadLettered bool) string {
	if deadLettered && errorCode == engine.ErrUnknown.String() {
		return engine.ErrRetriesExhausted.String()
	}
	return errorCode
}

// writeTerminalFailure writes a workflow's terminal failure, to the dead-letter
// queue or to 'failed'.
//
// eligibleForDLQ is the CALLER's answer to "could this failure be a retry
// exhaustion at all", and it is a parameter rather than something decided here
// because the two callers differ on it absolutely rather than by degree. The
// panic path cannot be one: a recovered panic is a crash, not a call that ran
// out of attempts.
func (w *Worker) writeTerminalFailure(wf *engine.WorkflowInstance, errMsg, errorCode, errorOp string, eligibleForDLQ bool, history []engine.EventRecord) (applied, deadLettered bool) {
	st, release := w.storeFor(wf)
	defer release()
	ctx := context.Background()

	// A claim carrying a pending terminal outcome cannot be failed, because
	// its outcome was decided before it was claimed: this execution is the
	// defer phase of a terminate, and writing 'failed' over it would turn a
	// terminate into a failure on the strength of the cleanup going wrong.
	//
	// The guard lives here rather than in executeWorkflow's error branch
	// because it has to cover the failure paths that run BEFORE the segment
	// does -- no store for the tenant, history that would not load, WASM that
	// would not load, a version check, and the panic recovery. Every one of
	// them reaches a terminal failure through this function, and every one of
	// them is a defer phase that could not run rather than a workflow that
	// failed.
	//
	// It applies the recorded outcome rather than merely refusing, so a
	// workflow whose defer phase cannot start terminates now instead of
	// sitting in 'terminating' until its deadline.
	if wf.PendingTerminalStatus != "" {
		w.logger.WarnContext(ctx, "a defer phase could not run; applying the recorded terminal outcome without its cleanup",
			"worker_id", w.id, "workflow_id", wf.ID, "tenant_id", wf.TenantID,
			"status", wf.PendingTerminalStatus, "error", errMsg)
		w.finishDeferPhase(wf, st, nil, nil, time.Now())
		return false, false
	}

	// Dead-lettering is decided by facts the ENGINE recorded -- what the last
	// durable act was, and how the engine classified it -- with no reading of
	// the error text at all.
	//
	// It was `strings.Contains(errMsg, "retries exhausted")` until cleat#902,
	// which was not laziness: engine/durablecalls.go knows retries were
	// exhausted but hands the error to the GUEST, which returns a plain string
	// out of its entry point, so no typed error survives to here.
	// EventRecord.RetriesExhausted is the channel that does survive.
	//
	// #902 still compared the terminal text against the recorded event to tie
	// the failure to a specific call. cleat#979 is why that had to go: it made
	// retention depend on the workflow author wrapping with %w. See
	// endedOnAnExhaustedCall for what replaced it and what it cannot tell.
	deadLettered = eligibleForDLQ && endedOnAnExhaustedCall(history)

	// ...and the same fact classifies the failure, which it did not before:
	// error_code was ALWAYS "unknown" on this path. cleat#1009.
	//
	// The producer is the gap, not the storage or the projection. The caller
	// derives its code with `errors.As(err, &ce)` against a *engine.CleatError
	// (setup.go, the execution-error branch), and that never matches here for
	// the reason the comment above gives: the engine's typed error is handed to
	// the GUEST, which returns a plain string out of its entry point. So the
	// assertion succeeds, the code stays ErrUnknown, and an operator asking
	// "why did this stop retrying" gets "unknown" for the one case the engine
	// knows the answer to.
	//
	// Only when the caller has nothing. A code that was genuinely classified --
	// ErrPermanent from a version check, ErrCancelled -- is information this
	// signal does not have, and overwriting it would trade a real answer for a
	// narrower one.
	//
	// Gated on `deadLettered` rather than on endedOnAnExhaustedCall alone, so
	// the code and the status always agree: a run dead-lettered for exhaustion
	// reads retries_exhausted, and the panic path -- which passes
	// eligibleForDLQ=false because a recovered panic is a crash rather than a
	// call that ran out of attempts -- keeps its own code.
	errorCode = classifyTerminalErrorCode(errorCode, deadLettered)

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
// endedOnAnExhaustedCall reports whether the last durable thing this workflow
// did was a call the ENGINE classified as having exhausted its retries.
//
// RetriesExhausted is the typed half: only engine/durablecalls.go sets it, and
// only where its `exhausted` bool is true, so no other kind of call failure can
// satisfy it.
//
// POSITION IS THE OTHER HALF, and it used to be a text comparison against the
// terminal error. That is cleat#979: the engine's error reaches the workflow as
// a plain string, so requiring the terminal message to contain it required the
// GUEST to relay it -- `fmt.Errorf("...: %w", err)` was dead-lettered and
// `fmt.Errorf("could not reach the billing provider")` was not, for two
// workflows differing by one line. Reporting a failure in domain terms is what
// good error handling looks like, so the workflows written most carefully were
// the ones losing retention, and nothing on any side showed why.
//
// What actually separates "died of the exhaustion" from "caught it, recovered,
// and failed later for an unrelated reason" is not phrasing -- it is whether
// the workflow WENT ON TO DO MORE DURABLE WORK. If it did, there is a later
// event and the exhausted call is not the last thing that happened. If it did
// not, the run's last durable act was a call that ran out of attempts, which is
// exactly what an operator would want to redrive.
//
// THE LIMIT, STATED RATHER THAN LEFT TO BE FOUND. A workflow that catches the
// exhaustion, does no further durable work, and then fails on pure computation
// for a genuinely unrelated reason is indistinguishable from one that died of
// the exhaustion -- there is no third signal, and reading the guest's words is
// the thing being removed. So it is decided rather than detected, and it is
// decided toward RETENTION: a workflow wrongly held for redrive costs an
// operator one dismissal; one wrongly dropped costs the work.
// TestTheLimitOfWhatPositionCanTell asserts that choice on purpose.
func endedOnAnExhaustedCall(history []engine.EventRecord) bool {
	// Trailing defer-phase events are skipped, because they are not the
	// workflow going on to do more work -- they are its cleanup, and the rule
	// above is about the former. cleat#1155.
	//
	// A defer body's host calls are durable calls, deliberately: "a defer that
	// cannot call the host cannot release the lock it took"
	// (engine/defer_phase.go). So they land in the history AFTER the call that
	// ended the run, and before EventRecord.InDeferPhase existed there was
	// nothing to tell them apart from the body's own. The effect was precise
	// and backwards: a workflow whose cleanup touched the host -- the cleanup
	// worth having, the one that releases something -- moved itself OUT of the
	// dead-letter queue by cleaning up, and retention then deleted it.
	//
	// Only trailing ones. A defer that ran, and was then followed by more body
	// work, means the workflow carried on; that is the case this rule already
	// judges correctly and it must keep judging it the same way.
	//
	// Events written before cleat#1155 carry no flag, so this loop stops
	// immediately and the answer is exactly what it was. A workflow in flight
	// across the upgrade keeps the behaviour it started with, which is the
	// same compatibility retries_exhausted itself relies on.
	i := len(history) - 1
	for i >= 0 && history[i].InDeferPhase {
		i--
	}
	if i < 0 {
		// Every event was a defer. There is no body act to judge, which is not
		// the same as a body act that was not an exhaustion -- but it is not
		// an exhaustion either, and inventing one here would dead-letter a
		// workflow on the strength of its cleanup alone.
		return false
	}
	return history[i].RetriesExhausted
}

// defaultUnservableBackoff is how long a run waits before another worker may
// claim it, after one released it as unservable here. cleat#1710.
//
// Short, because the common case is a rolling deploy where a capable sibling is
// already running and the delay is pure latency on work that could proceed. It
// is not zero, because that would let the releasing worker win the same run
// back immediately and spin.
const defaultUnservableBackoff = 5 * time.Second

// releaseForAnotherWorker returns a claimed run to the queue because THIS
// worker cannot serve it. It is the counterpart to recordTerminalFailure for a
// pre-flight check that fails on a WORKER-LOCAL fact. cleat#1710.
//
// # The discriminator is where the fact lives, not how serious it is
//
// A pre-flight check fails for one of two reasons, and they want opposite
// verdicts:
//
//   - RUN-INTRINSIC -- a malformed definition, an input that cannot be decoded,
//     a def row that is wrong on its own terms. Every worker in the pool would
//     reach the same conclusion, so terminating is correct and releasing would
//     circulate a run that can never run. recordTerminalFailure stays right for
//     these.
//   - WORKER-LOCAL -- this worker's loaded plugins, this worker's WASM binary.
//     Another worker may well satisfy it, so the run is not doomed; this worker
//     is merely the wrong one. Terminating destroys work a sibling could have
//     done.
//
// Both pre-flight checks below are the second kind, and both used to do the
// first. `w.plugList` is what this process linked in; `wfMeta` is read from the
// binary this process loaded. During a rolling deploy those are exactly the
// facts that differ between workers, which is why a version change was a
// drain-and-replace operation rather than a rolling one -- not by anyone's
// decision, but as a consequence of claiming being blind to a condition that
// execution then treats as fatal.
//
// # What happens when the satisfying worker never arrives
//
// Stated because the failure this replaces was at least visible, and an
// unbounded release would trade it for a livelock.
//
// The run does not circulate hot. ReleaseWorkflow writes next_wake_at on the
// ROW, so the backoff applied here throttles the whole pool rather than just
// this worker: a run that nobody can serve costs one claim-and-release per
// backoff interval across the entire cluster, not one per worker. It stays
// 'ready', it keeps its history, and it is visible to ListWorkflows and to this
// log line, which names the unmet requirement.
//
// That is deliberately a QUIESCENT STUCK STATE rather than a terminal one. A
// run nothing can serve today is served the moment a capable worker is
// deployed, which is the ordinary end of a rolling deploy; killing it at some
// attempt threshold would make the rollout window destroy exactly the work it
// is trying to preserve. The same argument, in the same direction, is already
// written down for ReclaimCount in engine/store_types.go: the count exists so
// an operator can SEE a loop, and is deliberately not a bound, because
// "dead-lettering past a threshold would turn a node being redeployed into
// permanent failure of a workflow that did nothing wrong".
//
// So this deliberately adds no counter and no threshold. If a bound is wanted
// later it needs persisted per-run state, and cleat#1702 is sequencing the
// shape of that for the non-workflow entities; inventing a column here would be
// the second place it gets designed.
func (w *Worker) releaseForAnotherWorker(wf *engine.WorkflowInstance, reason, op string) {
	ctx := context.Background()
	w.logger.WarnContext(ctx,
		"this worker cannot serve this workflow; returning it to the queue for one that can",
		"worker_id", w.id, "workflow_id", wf.ID, "tenant_id", wf.TenantID,
		"def_name", wf.DefName, "def_version", wf.DefVersion,
		"check", op, "reason", reason)

	st, release := w.storeFor(wf)
	defer release()
	backoff := w.unservableBackoff
	if backoff <= 0 {
		backoff = defaultUnservableBackoff
	}
	err := st.ReleaseWorkflow(ctx, wf.ID, w.id, wf.Generation, time.Now().Add(backoff))
	if errors.Is(err, engine.ErrFenceLost) {
		// Another worker already has it, which is the outcome this function
		// exists to produce. Not a failure.
		w.logger.DebugContext(ctx, "release for another worker: fence already lost",
			"worker_id", w.id, "workflow_id", wf.ID, "tenant_id", wf.TenantID)
		return
	}
	if err != nil {
		// The run stays claimed until its lease expires, and then the reaper
		// returns it -- slower than this release, but the same destination. It
		// is NOT terminated, which is the property that matters.
		w.logger.WarnContext(ctx,
			"could not return this workflow to the queue; it stays claimed until its lease expires",
			"worker_id", w.id, "workflow_id", wf.ID, "tenant_id", wf.TenantID, "error", err)
	}
}

// recordTerminalFailure is the no-history form, for the failure paths that run
// BEFORE any segment does -- no store for the tenant, history that would not
// load, WASM that would not load. None of them can be a retry exhaustion,
// because none of them ran a durable call, so passing no history says exactly
// that rather than losing information.
//
// This list used to include "a version check". It does not any more, and the
// distinction is cleat#1710: the version and plugin pre-flight checks fail on
// facts local to THIS worker, so they release the run for a worker that can
// serve it rather than destroying it. See releaseForAnotherWorker above for
// which side of that line a new pre-flight check belongs on.
func (w *Worker) recordTerminalFailure(wf *engine.WorkflowInstance, startedAt time.Time, errMsg, errorCode, errorOp string) {
	w.recordTerminalFailureWithHistory(wf, startedAt, errMsg, errorCode, errorOp, nil)
}

// recordTerminalFailureWithHistory is the form used where a segment actually
// executed, so its history can answer whether a retry exhaustion is what ended
// the workflow.
func (w *Worker) recordTerminalFailureWithHistory(wf *engine.WorkflowInstance, startedAt time.Time, errMsg, errorCode, errorOp string, history []engine.EventRecord) {
	applied, deadLettered := w.writeTerminalFailure(wf, errMsg, errorCode, errorOp, true, history)
	if !applied {
		return
	}
	// "dead_lettered", not "failed", when the retry-exhaustion classifier
	// said so -- notifyTerminal passes this straight to finalize observers,
	// and jobqueue's ObserveFinalize records it as its own distinct status
	// rather than folding it into an ordinary failure. cleat#1976.
	status := "failed"
	if deadLettered {
		status = "dead_lettered"
	}
	w.notifyTerminal(wf, status)
	ctx := context.Background()
	w.Metrics.RecordWorkflowFailed(ctx, wf.DefName, "")
	w.Metrics.RecordWorkflowDuration(ctx, time.Since(startedAt), wf.DefName, "failed", "")
	if deadLettered {
		w.Metrics.RecordWorkflowsDeadLettered(ctx)
	}
}

// finishDeferPhase applies the terminal outcome that this workflow's first
// phase recorded, after its defer segment has run. IMPROVEMENT-PLAN 3.75
// step 2.
//
// It takes no status and no error. That is the property the whole two-phase
// transition rests on: the outcome was written to the row before this segment
// was claimed, and FinalizeDeferPhase reads it from there, so nothing that
// happens in the defer phase -- a trap, a timeout, a body that suspends, a
// worker that is fenced out -- can turn a terminate into a completion or a
// failure into a different failure. The segment's job is to run the cleanup and
// get out of the way.
//
// The events it appends are real, though, and they are the reason this is not
// just an UPDATE: a defer body's host calls are durable calls, and their event
// rows have to land in the same transaction as the terminal write.
func (w *Worker) finishDeferPhase(wf *engine.WorkflowInstance, execStore engine.WorkflowStore, history, resultHistory []engine.EventRecord, startedAt time.Time) {
	ctx := context.Background()

	dps, ok := execStore.(engine.DeferPhaseStore)
	if !ok {
		// Unreachable through any store that could have marked the phase --
		// TerminateWorkflow and the DeferPhaseStore pair are implemented
		// together, and engine asserts that at compile time. Said out loud
		// rather than ignored, because the failure it describes is a workflow
		// stuck in 'terminating' with nothing able to move it.
		w.logger.ErrorContext(ctx, "store cannot finalize a defer phase; this workflow stays in 'terminating' until its deadline",
			"worker_id", w.id, "workflow_id", wf.ID, "tenant_id", wf.TenantID)
		return
	}

	var newEvents []engine.EventRecord
	if len(resultHistory) > len(history) {
		newEvents = resultHistory[len(history):]
		for i := range newEvents {
			newEvents[i].Request = engine.Redact(newEvents[i].Request)
			newEvents[i].Response = engine.Redact(newEvents[i].Response)
		}
	}

	err := dps.FinalizeDeferPhase(ctx, wf.ID, w.id, wf.Generation, newEvents)
	if errors.Is(err, engine.ErrFenceLost) {
		// Normal rather than exceptional here, and it has one more cause than
		// elsewhere: as well as the ordinary "reaped and reclaimed", the
		// reaper's ExpireDeferPhases takes a phase away from a worker still
		// grinding on it by bumping the generation. Either way the outcome has
		// been applied by whoever holds it now.
		w.logger.DebugContext(ctx, "defer phase: fence lost, the terminal outcome was applied by another owner",
			"worker_id", w.id, "workflow_id", wf.ID, "tenant_id", wf.TenantID)
		return
	}
	if err != nil {
		// No recordTerminalFailure: this workflow's outcome is already
		// decided and writing a different one over it is the one thing this
		// path must not do. Leaving it in 'terminating' is correct and
		// bounded -- the deadline sweep applies the recorded outcome.
		w.logger.ErrorContext(ctx, "finalizing a defer phase failed; it stays in 'terminating' until its deadline",
			"worker_id", w.id, "workflow_id", wf.ID, "tenant_id", wf.TenantID, "error", err)
		return
	}

	// The recorded outcome is now applied, so this workflow is terminal: wake
	// any parent awaiting it, fail any update still pending against it, and
	// tell finalize observers the real outcome -- previously the one thing
	// this path never did, so a job dispatched through a workflow that was
	// cancelled or force-terminated (both routed here, per
	// engine.WorkflowStore's TerminateWorkflow/CancelWorkflow doc comments)
	// never got a write-back and was only ever recovered by the abandonment
	// sweep, marked 'abandoned' rather than its real outcome. cleat#1976.
	w.notifyTerminal(wf, wf.PendingTerminalStatus)

	w.Metrics.RecordWorkflowDuration(ctx, time.Since(startedAt), wf.DefName, wf.PendingTerminalStatus, "")
	w.logger.InfoContext(ctx, "defer phase complete; terminal outcome applied",
		"worker_id", w.id, "workflow_id", wf.ID, "tenant_id", wf.TenantID,
		"status", wf.PendingTerminalStatus, "defer_events", len(newEvents))
}

// releaseWorkflow returns wf to the ready pool, treating a lost fence as the
// no-op it is: another worker owns the workflow, so there is nothing to
// release. See recordTerminalFailure for why this is not an error.
func (w *Worker) releaseWorkflow(wf *engine.WorkflowInstance) {
	st, release := w.storeFor(wf)
	defer release()
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
	// Deliberately not recordTerminalFailure: this path has no start time to
	// report a duration from.
	//
	// eligibleForDLQ is false and the code is no longer blank. The only caller
	// is the panic recovery in executeWorkflow, so errMsg here is always
	// "panic: <value>" -- and a panic is not a retry exhaustion. Until this
	// argument existed the routing was decided by whether the panic VALUE
	// happened to contain the words "retries exhausted", which is an accident
	// either way it lands. eligibleForDLQ=false short-circuits deadLettered to
	// always false, so there is nothing left to distinguish here.
	//
	// Until cleat#1976 this was the only one of the four terminal paths that
	// woke no parent, failed no stranded update, notified no finalize
	// observer, and recorded no workflows_failed_total: a workflow that
	// panicked left its parent waiting out its full timer, and its job (if
	// jobqueue-dispatched) recoverable only by the abandonment sweep, marked
	// 'abandoned' rather than 'failed'.
	applied, _ := w.writeTerminalFailure(wf, errMsg, engine.ErrUnknown.String(), "panic", false, nil)
	if !applied {
		return
	}
	w.notifyTerminal(wf, "failed")
	w.Metrics.RecordWorkflowFailed(context.Background(), wf.DefName, "")
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

// cacheTenantFor names the tenant whose store storeForTenant will hand back
// for tenantID, which is what any cache over that store has to be keyed on.
// It has to track storeForTenant's own first condition; a second copy of that
// rule is why this is a function rather than an expression at the call site.
func (w *Worker) cacheTenantFor(tenantID string) string {
	if w.storeFactory == nil || tenantID == "" || tenantID == w.storeTenantID {
		return w.storeTenantID
	}
	return tenantID
}

// storeForTenant returns the store execution should write through for a
// workflow belonging to tenantID, and the release the caller must call when it
// is finished with it.
//
// With no factory configured there is nothing to route with, so the caller
// gets the worker's own store and executeWorkflow's scope check refuses
// anything outside it. That is the pre-routing behaviour, kept deliberately
// rather than silently degrading to "write it under whatever tenant we have".
//
// # RELEASE IS NOT OPTIONAL, AND IT IS NOT A POOL SHUTDOWN
//
// On MySQL and SQL Server the factory keeps a connection pool per tenant, and
// cleat#1928 gave those pools a reaper so a worker no longer holds one for
// every tenant it has ever served. The closer OpenStore returns is the lease
// that reaper honours: a pool with an outstanding lease is never evicted,
// however idle it looks. Skipping the release pins the pool forever -- which
// looks exactly like the leak the reaper was added to fix, with more code.
// Calling it early closes nothing; it says this caller is done.
//
// # Why there is no cache here any more
//
// There used to be a sync.Map of tenant -> store, on the grounds that OpenStore
// was expensive on the per-tenant-pool dialects. It is not: the pool is cached
// inside the factory, so a warm tenant's OpenStore is a struct allocation on
// every dialect. What the cache actually did was hold a store -- and therefore
// a pool -- for the life of the process, which is precisely what made the pools
// unreapable. Removing it leaves one lifetime instead of two.
//
// The one path where OpenStore was NOT free was PostgreSQL with a non-public
// schema, which issued CREATE SCHEMA IF NOT EXISTS per call; that is now
// latched after the first success. See engine.PostgresStoreFactory.OpenStore.
func (w *Worker) storeForTenant(tenantID string) (engine.WorkflowStore, func(), error) {
	if w.storeFactory == nil || tenantID == "" || tenantID == w.storeTenantID {
		// The worker's own store holds a lease for the life of the process,
		// taken where it was opened in main. Nothing to release here.
		return w.store, func() {}, nil
	}
	st, closer, err := w.storeFactory.OpenStore(w.ctx, tenantID, w.taskQueues...)
	if err != nil {
		return nil, func() {}, err
	}
	return st, func() { _ = closer.Close() }, nil
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
func (w *Worker) storeFor(wf *engine.WorkflowInstance) (engine.WorkflowStore, func()) {
	st, release, err := w.storeForTenant(wf.TenantID)
	if err != nil {
		w.logger.ErrorContext(context.Background(), "no tenant store on a failure path; falling back to the worker store",
			"worker_id", w.id, "workflow_id", wf.ID, "tenant_id", wf.TenantID, "error", err)
		return w.store, func() {}
	}
	return st, release
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

	// ONE MECHANISM, so one answer. This used to report on two: the widened
	// query behind admin.claim_workflows, whose availability was a property of
	// the 023/024 grants, and the rotation, whose availability is a property
	// of this worker. The widened query is retired, so the grant questions are
	// gone with it -- including the warning about a missing 024, which on
	// managed PostgreSQL fired on every healthy worker forever and pointed at
	// a migration nobody there can apply.
	if err := w.rotatingClaimAvailability(); err != nil {
		w.logger.WarnContext(w.ctx,
			"claim-across-tenants is set but this worker cannot serve other tenants; "+
				"it will claim and fire cron for its own tenant only",
			"worker_id", w.id, "tenant_id", w.storeTenantID, "reason", err)
		return
	}
	// BOTH loops, said separately because they are bounded differently: the
	// claim serves a share of tenants per tick, the schedule read serves every
	// tenant every tick. See dueSchedulesByTenant for why that asymmetry is
	// deliberate.
	w.logger.InfoContext(w.ctx,
		"cross-tenant workflow claim is available by tenant rotation; no database grant is required",
		"worker_id", w.id, "tenant_id", w.storeTenantID,
		"tenants_per_tick", w.claimTenantsPerTick)
	w.logger.InfoContext(w.ctx,
		"cross-tenant due-schedule read is available by tenant rotation; no database grant is required",
		"worker_id", w.id, "tenant_id", w.storeTenantID)
}

// rotatingClaimAvailability reports whether this worker can serve tenants
// other than its own, with the same reasons claimRotating would give.
//
// Separate from claimRotating so that startup can answer the question without
// claiming anything, which is the difference between "it will tell you on its
// first tick" and "it told you before the loops began".
func (w *Worker) rotatingClaimAvailability() error {
	if w.storeFactory == nil {
		return fmt.Errorf("%w: no store factory to open per-tenant stores with", errRotatingClaimUnavailable)
	}
	if _, ok := w.store.(engine.TenantLister); !ok {
		return fmt.Errorf("%w: this store cannot enumerate tenants", errRotatingClaimUnavailable)
	}
	return nil
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
		// The per-tenant read is tried FIRST, and is the default, for the same
		// reason the rotating claim is: it needs no database-side grant, so a
		// deployment that never applied 024 -- or cannot, because BYPASSRLS is
		// ungrantable on managed PostgreSQL -- still fires every tenant's cron.
		//
		schedules, err := w.dueSchedulesByTenant()
		if !errors.Is(err, errRotatingClaimUnavailable) {
			return schedules, err
		}
		w.rotatingSchedulesUnavailableOnce.Do(func() {
			w.logger.WarnContext(w.ctx,
				"claim-across-tenants is set but this worker cannot read due schedules per "+
					"tenant; only this worker's own tenant's cron will fire",
				"worker_id", w.id, "tenant_id", w.storeTenantID, "reason", err)
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
	// Every claim path returns through here -- cross-tenant, the fallback after
	// ErrCrossTenantClaimUnsupported, and the scoped claim -- so one deferred
	// record covers all three. It covers the ERROR returns too, deliberately: a
	// claim that is slow because the database is struggling is exactly the
	// latency an operator wants to see, and excluding it would make the metric
	// look healthiest at the moment it matters most.
	//
	// The name argument is empty because a batch claim has no single workflow.
	// RecordDispatchLatency already passes "" for the same reason. cleat#1317.
	claimStart := time.Now()
	defer func() { w.Metrics.RecordClaimLatency(w.ctx, time.Since(claimStart), "") }()

	if w.claimAcrossTenants {
		// The rotating claim is tried FIRST, and is the default, because it
		// needs no database-side grant: it enumerates tenants from
		// admin.tenants -- which carries no RLS -- and then claims within each
		// through that tenant's own store. A deployment that never applied 023
		// still gets cross-tenant dispatch, and one that did gets a fair share
		// per tenant instead of whoever has the oldest backlog.
		//
		wfs, err := w.claimRotating(limit)
		if !errors.Is(err, errRotatingClaimUnavailable) {
			return wfs, err
		}
		// The rotation is unavailable as a STATIC property of this worker --
		// no store factory, or a store that cannot enumerate tenants -- so
		// this is one line, not one per tick. Claiming this worker's own
		// tenant is the honest fallback: it is what the worker can actually
		// do, and stopping instead would turn a narrowed worker into a dead
		// one.
		w.rotatingUnavailableOnce.Do(func() {
			w.logger.WarnContext(w.ctx,
				"claim-across-tenants is set but this worker cannot rotate; "+
					"claiming only this worker's own tenant",
				"worker_id", w.id, "tenant_id", w.storeTenantID, "reason", err)
		})
	}
	return w.store.ClaimWorkflows(w.ctx, w.id, limit)
}

// checkPluginDeps reports whether the plugins loaded in this worker satisfy a
// workflow's declared dependencies.
//
// Extracted from the inline check it replaces so that it can be tested at all:
// the behaviour it encodes had no test, which is how the defect below survived.
func checkPluginDeps(workerPlugins, deps map[string]string) error {
	for pluginName, requiredVersion := range deps {
		workerVersion, ok := workerPlugins[pluginName]
		if !ok {
			return fmt.Errorf(
				"missing plugin: workflow requires plugin %q version %s but it is not installed in this worker. Available plugins: %v",
				pluginName, requiredVersion, pluginNames(workerPlugins))
		}
		// A CONSTRAINT, not a version, and this used to compare the two with
		// `!=`. cleat/version.go documents plugin_deps as "a JSON object
		// mapping plugin names to semver constraints" and gives
		// {"llm": ">=1.2.0", "blobstore": "~2.0.0"} as the example -- so the
		// documented form compared ">=1.2.0" against "1.3.0", found them
		// unequal, and failed the workflow with ErrPermanent. Seven of the
		// eight forms a workflow can write were rejected on a worker that
		// satisfied them; the only one that worked was a bare literal equal to
		// the installed version, which needs no constraint vocabulary at all.
		// cleat#1260.
		if !engine.VersionSatisfies(workerVersion, requiredVersion) {
			return fmt.Errorf(
				"plugin version mismatch: workflow requires plugin %q %s but worker has version %s",
				pluginName, requiredVersion, workerVersion)
		}
	}
	return nil
}

// checkHostBindingConfigured refuses a worker that was told to enforce host
// binding on a deployment that has configured no hostnames.
//
// WHY REFUSE RATHER THAN WARN. With --require-host-match set and
// tenant_domains empty, every authenticated request is answered 404 --
// correctly, by the middleware's own rule, since no host belongs to anyone.
// The operator sees a worker that starts cleanly and rejects all traffic, which
// reads as "cleat is broken" rather than "this is misconfigured".
//
// checkConnectionBudget is the precedent and the same argument: a budget the
// fixed pools cannot honour is refused at boot, because it cannot be honoured
// under any traffic. So is cleat#1581 in the negative -- the rate limiter warns
// and downgrades, and an operator who asked for a cluster-wide limit gets a
// per-process one with nothing but a log line to say so.
//
// It checks only that SOMETHING is configured. Whether the RIGHT hostnames are
// configured is not a question a worker can answer at boot.
func checkHostBindingConfigured(ctx context.Context, store *auth.TenantStore) error {
	n, err := store.CountTenantDomains(ctx)
	if err != nil {
		return fmt.Errorf("could not read tenant_domains: %w "+
			"(the table arrives in migration 080; run migrations before enabling this)", err)
	}
	if n == 0 {
		return fmt.Errorf("tenant_domains is empty, so every authenticated request would be " +
			"refused; register at least one hostname, or unset --require-host-match")
	}
	return nil
}

// secretKeyChecker is the part of *engine.SecretStore that checkSecretsUsable
// reads, named so a test can make the read fail. That is not hypothetical: the
// failure this check must not swallow is a read that errors (cleat#2123).
type secretKeyChecker interface {
	HasMasterKey() bool
	CountSecrets(ctx context.Context) (int, error)
	CheckKeyRing(ctx context.Context) (engine.SecretKeyCheck, error)
}

// checkSecretsUsable refuses a worker that would fail on first use.
//
// TWO WAYS A WORKER CAN FAIL ON ITS FIRST SECRET, and both are refusals at boot:
//
//  1. NO KEY, AND SECRETS EXIST. A deployment holding secrets and started
//     without CLEAT_SECRET_MASTER_KEY can do everything except the one thing the
//     secrets were for. The failure arrives later, from inside a plugin call, as
//     a workflow error with no obvious connection to a missing environment
//     variable -- on whichever workflow needs a credential first, which may be
//     hours after the deploy that dropped the variable.
//
//  2. A KEY RING THAT LACKS A VERSION STILL IN THE TABLE (cleat#1991). This is
//     the state specs/CleatKeyRotation.tla's S1 forbids -- a serving worker
//     holding a row it cannot open -- and the boot check is the mechanism that
//     keeps a NEW worker out of it: without it, retiring the old key while rows
//     still carry its version starts a worker that fails on the first
//     resolution. The model finds it in four states (BootCheck = FALSE). The
//     refusal names the missing version and how many rows carry it, because
//     those are what an operator needs to put the right key back.
//
// Rows still on a PREVIOUS key that the ring can open are not a refusal, since
// they work; they are a warning, because they are what has to be resealed
// before that key can be removed.
//
// A READ THAT ERRORS REFUSES, in both cases and with no override (cleat#2123).
// The rule here used to be "a count that errors is not fatal, because the table
// arrives in migration 080", which was true of the place the check ran -- BEFORE
// the migrations -- and is why the check never noticed that its read could not
// see the rows on PostgreSQL (it raised) or SQL Server (it returned 0): the error
// was read as "cannot tell" and the caller went on as though there were nothing
// to protect. A check that swallows the failure of its own measurement reports
// the same thing as a check that ran and found nothing.
//
// The call therefore sits AFTER the migrations (cmd/cleat-worker/main.go), where
// the table exists by construction, so an error here is a real inability to
// establish the answer.
func checkSecretsUsable(ctx context.Context, store secretKeyChecker, warn func(string)) error {
	if store == nil {
		return nil
	}
	if !store.HasMasterKey() {
		n, err := store.CountSecrets(ctx)
		if err != nil {
			return fmt.Errorf("CLEAT_SECRET_MASTER_KEY is not set and this worker could not "+
				"establish whether the database holds secrets: %w; refusing to start rather "+
				"than assume it holds none. Set the key, or fix the database access this read needs", err)
		}
		if n > 0 {
			return refuseSecrets(fmt.Errorf("%d secret(s) are stored and CLEAT_SECRET_MASTER_KEY is not set, "+
				"so every workflow that references one would fail at the plugin call; "+
				"set it, or remove the secrets", n))
		}
		return nil
	}

	chk, err := store.CheckKeyRing(ctx)
	if err != nil {
		return fmt.Errorf("this worker could not establish whether its secret keys can open every "+
			"stored secret: %w; refusing to start rather than assume they can", err)
	}
	if len(chk.Unopenable) > 0 {
		versions := make([]int, 0, len(chk.Unopenable))
		for v := range chk.Unopenable {
			versions = append(versions, v)
		}
		sort.Ints(versions)
		parts := make([]string, 0, len(versions))
		for _, v := range versions {
			parts = append(parts, fmt.Sprintf("%d secret(s) under key_version %d", chk.Unopenable[v], v))
		}
		return refuseSecrets(fmt.Errorf("%s, and no configured master key carries that version (configured: %v): "+
			"every workflow that references one would fail at the plugin call. Add the key that has "+
			"that version as CLEAT_SECRET_MASTER_KEY or CLEAT_SECRET_MASTER_KEY_PREVIOUS (with its "+
			"_VERSION), or do not remove it until `cleatctl reseal-secrets` has moved every row off it",
			strings.Join(parts, ", "), chk.Configured))
	}
	if warn != nil {
		versions := make([]int, 0, len(chk.OnPrevious))
		for v := range chk.OnPrevious {
			versions = append(versions, v)
		}
		sort.Ints(versions)
		for _, v := range versions {
			warn(fmt.Sprintf("%d secret(s) are still sealed under the previous key (key_version %d); they "+
				"resolve, but run `cleatctl reseal-secrets` before removing that key from the ring",
				chk.OnPrevious[v], v))
		}
	}
	return nil
}

// secretsRefusal marks a check failure that is a DEFINITE answer -- this worker
// cannot open something that is stored -- as opposed to a read that failed and
// leaves the question open. At startup both refuse; a worker that is already
// serving stops only on the first, because a database that cannot be read is
// failing everything else too and a stopped worker is not evidence of safety.
type secretsRefusal struct{ err error }

func (e *secretsRefusal) Error() string { return e.err.Error() }
func (e *secretsRefusal) Unwrap() error { return e.err }

func refuseSecrets(err error) error { return &secretsRefusal{err: err} }

// registerWithKeyCheck publishes this worker's key set and runs the secrets
// check as one span under the shared secret-key gate (engine.RegisterUnderKeyGate).
// It is the ONE place that pairs the two, used at startup and again after a
// lapse, because the model's obligation is that they are never separated.
func registerWithKeyCheck(ctx context.Context, registry *engine.WorkerRegistry, store *engine.SecretStore,
	reg engine.WorkerRegistration, warn func(string)) error {
	reg.SecretKeyVersions = store.KeyVersions()
	var checker secretKeyChecker
	if store != nil {
		checker = store
	}
	return registry.RegisterUnderKeyGate(ctx, reg, func(cctx context.Context) error {
		return checkSecretsUsable(cctx, checker, warn)
	})
}
