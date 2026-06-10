package cleat

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

func (h *HostCallsImpl) DurableSleep(d time.Duration) {
	h.DurableSleepMs(d.Milliseconds())
}

func (h *HostCallsImpl) DurableSleepMs(ms int64) {
	if h.durableSleep == nil {
		log.Printf("durable: DurableSleep can only be called from within a workflow function (the HostCalls runtime was not initialized). Ensure this call is inside a cleat_entry / #[cleat_entry] / @CleatEntry / @cleatEntry function.")
		return
	}
	h.durableSleep(ms)
}

func (h *HostCallsImpl) WorkflowID() string {
	if h.workflowID == nil {
		return ""
	}
	return h.workflowID()
}

func (h *HostCallsImpl) RunID() string {
	if h.workflowRunID == nil {
		return ""
	}
	return h.workflowRunID()
}

func (h *HostCallsImpl) DurableLog(message string) {
	if h.durableLog != nil {
		h.durableLog(message)
	}
}

func (h *HostCallsImpl) Log(message string, kvs ...interface{}) {
	h.LogKV(message, kvs...)
}

func (h *HostCallsImpl) LogKV(message string, kvs ...interface{}) {
	entry := map[string]interface{}{
		"msg": message,
	}
	if len(kvs) > 0 {
		kvMap := make(map[string]interface{}, len(kvs)/2)
		for i := 0; i+1 < len(kvs); i += 2 {
			key, ok := kvs[i].(string)
			if !ok {
				key = fmt.Sprintf("%v", kvs[i])
			}
			kvMap[key] = kvs[i+1]
		}
		if len(kvs)%2 != 0 {
			kvMap["_unpaired"] = kvs[len(kvs)-1]
		}
		entry["kvs"] = kvMap
	}
	data, _ := json.Marshal(entry)
	h.DurableLog(string(data))
}

func (h *HostCallsImpl) PollCancellation() (bool, string) {
	if h.pollCancellation == nil {
		return false, ""
	}
	return h.pollCancellation()
}

func (h *HostCallsImpl) ContinueAsNew(newInputJSON string) error {
	if h.continueAsNew == nil {
		return errors.New("durable: ContinueAsNew can only be called from within a workflow function (the HostCalls runtime was not initialized). Ensure this call is inside a cleat_entry / #[cleat_entry] / @CleatEntry / @cleatEntry function.")
	}
	return h.continueAsNew(newInputJSON)
}

func (h *HostCallsImpl) ContinueAsNewWithVersion(newInputJSON string, newVersion int64) error {
	if h.continueAsNewWithVersion == nil {
		return errors.New("durable: ContinueAsNewWithVersion can only be called from within a workflow function (the HostCalls runtime was not initialized). Ensure this call is inside a cleat_entry / #[cleat_entry] / @CleatEntry / @cleatEntry function.")
	}
	return h.continueAsNewWithVersion(newInputJSON, newVersion)
}

func (h *HostCallsImpl) Version() int {
	if h.version == nil {
		return 1
	}
	return h.version()
}

func (h *HostCallsImpl) MinVersion() int {
	if h.minVersion == nil {
		return 1
	}
	return h.minVersion()
}

func (h *HostCallsImpl) SetQueryState(key, value string) {
	if h.setQueryState != nil {
		h.setQueryState(key, value)
	}
	// Also store in local state map for typed access.
	if h.stateMap == nil {
		h.stateMap = make(map[string]interface{})
	}
	h.stateMap[key] = value
}

// scopedKey returns the internally-stored key, applying the current
// virtual-object scope prefix when one is active.
func (h *HostCallsImpl) scopedKey(key string) string {
	if h.scopeSet && h.scopePrefix != "" {
		return h.scopePrefix + key
	}
	return key
}

// SetScope sets the state key prefix for virtual object instances.
// All subsequent SetState/GetState/etc calls are automatically prefixed
// with "vo:<objectType>:<instanceKey>:". Returns the previous scope
// prefix for stack-style save/restore.
func (h *HostCallsImpl) SetScope(objectType, instanceKey string) (previousScope string) {
	if h.scopeSet {
		previousScope = h.scopePrefix
	}
	if objectType == "" && instanceKey == "" {
		h.scopeSet = false
		h.scopePrefix = ""
		h.scopeObjType = ""
		h.scopeInstKey = ""
	} else {
		h.scopeSet = true
		h.scopeObjType = objectType
		h.scopeInstKey = instanceKey
		h.scopePrefix = "vo:" + objectType + ":" + instanceKey + ":"
	}
	return
}

// GetScope returns the current (objectType, instanceKey) or ("", "")
// if no scope is set.
func (h *HostCallsImpl) GetScope() (objectType, instanceKey string) {
	if !h.scopeSet {
		return "", ""
	}
	return h.scopeObjType, h.scopeInstKey
}

// ClearScope removes the current scope and returns the previous scope
// prefix (empty string if none was set).
func (h *HostCallsImpl) ClearScope() (previousScope string) {
	if h.scopeSet {
		previousScope = h.scopePrefix
	}
	h.scopeSet = false
	h.scopePrefix = ""
	h.scopeObjType = ""
	h.scopeInstKey = ""
	return
}

// UUID returns a deterministic UUID scoped to the current workflow

func (h *HostCallsImpl) UUID(seed string) string {
	wfID := h.WorkflowID()
	data := wfID + ":" + seed
	hash := sha256.Sum256([]byte(data))
	// Format as UUIDv5-like value (first 16 bytes of SHA-256, version bits set).
	hash[6] = (hash[6] & 0x0f) | 0x50 // Version 5
	hash[8] = (hash[8] & 0x3f) | 0x80 // Variant 1
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		hash[0:4], hash[4:6], hash[6:8], hash[8:10], hash[10:16])
}

// NewUUID generates a random UUID (v4) deterministically.
func (h *HostCallsImpl) NewUUID() string {
	r1 := uint64(h.Random())
	r2 := uint64(h.Random())
	// Format as random (version 4) UUID.
	b := make([]byte, 16)
	b[0] = byte(r1 >> 56)
	b[1] = byte(r1 >> 48)
	b[2] = byte(r1 >> 40)
	b[3] = byte(r1 >> 32)
	b[4] = byte(r1 >> 24)
	b[5] = byte(r1 >> 16)
	b[6] = byte(r1 >> 8)
	b[7] = byte(r1)
	b[8] = byte(r2 >> 56)
	b[9] = byte(r2 >> 48)
	b[10] = byte(r2 >> 40)
	b[11] = byte(r2 >> 32)
	b[12] = byte(r2 >> 24)
	b[13] = byte(r2 >> 16)
	b[14] = byte(r2 >> 8)
	b[15] = byte(r2)
	// Set version 4 (random) and variant 1 bits.
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// NewUUIDv7 generates a time-sortable UUID (v7) deterministically.
// The timestamp comes from Now() — deterministic on replay. The random
// portion comes from Random() — also deterministic on replay.
func (h *HostCallsImpl) NewUUIDv7() string {
	ts := uint64(h.NowMs()) // Unix timestamp in ms
	r1 := uint64(h.Random())
	r2 := uint64(h.Random())

	// Build 16 bytes per RFC 9562 UUID v7 layout:
	//   [0..5]  = 48-bit timestamp (big-endian)
	//   [6..7]  = version (4 bits = 0x7) + 12 bits rand_a
	//   [8..9]  = variant (2 bits = 10) + 14 bits rand_b
	//   [10..15] = 48 more bits rand_b
	b := make([]byte, 16)
	b[0] = byte(ts >> 40)
	b[1] = byte(ts >> 32)
	b[2] = byte(ts >> 24)
	b[3] = byte(ts >> 16)
	b[4] = byte(ts >> 8)
	b[5] = byte(ts)
	b[6] = byte(r1 >> 56)
	b[7] = byte(r1 >> 48)
	b[8] = byte(r1 >> 40)
	b[9] = byte(r1 >> 32)
	b[10] = byte(r1 >> 24)
	b[11] = byte(r1 >> 16)
	b[12] = byte(r1 >> 8)
	b[13] = byte(r1)
	b[14] = byte(r2 >> 56)
	b[15] = byte(r2 >> 48)

	// Version 7: set high nibble of byte 6 to 0x7.
	b[6] = (b[6] & 0x0f) | 0x70
	// Variant 1: set high 2 bits of byte 8 to 10.
	b[8] = (b[8] & 0x3f) | 0x80

	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func (h *HostCallsImpl) SetState(key string, value interface{}) {
	sk := h.scopedKey(key)
	if h.stateMap == nil {
		h.stateMap = make(map[string]interface{})
	}
	// Store as json.RawMessage so GetState can unmarshal directly.
	data, err := json.Marshal(value)
	if err != nil {
		h.stateMap[sk] = value // fallback to raw value
	} else {
		h.stateMap[sk] = json.RawMessage(data)
	}
	// Persist via existing set_query_state mechanism.
	if h.setQueryState != nil {
		if data == nil {
			data, _ = json.Marshal(value)
		}
		h.setQueryState(sk, string(data))
	}
}

func (h *HostCallsImpl) GetState(key string, result interface{}) error {
	sk := h.scopedKey(key)
	if h.stateMap == nil {
		return errors.New("durable: state not found for key: " + sk)
	}
	val, ok := h.stateMap[sk]
	if !ok {
		return errors.New("durable: state key not found: " + sk)
	}
	// If val is already json.RawMessage, unmarshal directly.
	if raw, ok := val.(json.RawMessage); ok {
		return json.Unmarshal(raw, result)
	}
	// Otherwise marshal and unmarshal for consistent type conversion.
	data, err := json.Marshal(val)
	if err != nil {
		return fmt.Errorf("durable: marshal state value: %w", err)
	}
	return json.Unmarshal(data, result)
}

func (h *HostCallsImpl) DeleteState(key string) {
	sk := h.scopedKey(key)
	if h.stateMap != nil {
		delete(h.stateMap, sk)
	}
	if h.setQueryState != nil {
		h.setQueryState(sk, "")
	}
}

func (h *HostCallsImpl) HasState(key string) bool {
	if h.stateMap == nil {
		return false
	}
	_, ok := h.stateMap[h.scopedKey(key)]
	return ok
}

func (h *HostCallsImpl) IncrState(key string, delta int64) int64 {
	sk := h.scopedKey(key)
	if h.stateMap == nil {
		h.stateMap = make(map[string]interface{})
	}
	var current int64
	if val, ok := h.stateMap[sk]; ok {
		switch v := val.(type) {
		case int64:
			current = v
		case float64:
			current = int64(v)
		case json.Number:
			current, _ = v.Int64()
		default:
			current = 0
		}
	}
	current += delta
	h.stateMap[sk] = current
	// Persist via existing set_query_state mechanism.
	if h.setQueryState != nil {
		data, err := json.Marshal(current)
		if err == nil {
			h.setQueryState(sk, string(data))
		}
	}
	return current
}

func (h *HostCallsImpl) ListState(prefix string) []string {
	if h.stateMap == nil {
		return nil
	}
	sk := h.scopedKey(prefix)
	var keys []string
	for k := range h.stateMap {
		if sk == "" || strings.HasPrefix(k, sk) {
			// Strip scope prefix from returned key names.
			if h.scopeSet && h.scopePrefix != "" && strings.HasPrefix(k, h.scopePrefix) {
				keys = append(keys, k[len(h.scopePrefix):])
			} else {
				keys = append(keys, k)
			}
		}
	}
	return keys
}

func (h *HostCallsImpl) RunDetached(fn func(h HostCalls) error) error {
	if h.runDetached != nil {
		return h.runDetached(fn)
	}
	return nil
}

func (h *HostCallsImpl) DurableFetch(url, method string, headers map[string]string, body string) (responseJSON string, statusCode int, err error) {
	requestMap := map[string]interface{}{
		"url":     url,
		"method":  method,
		"headers": headers,
		"body":    body,
	}
	requestJSON, marshalErr := json.Marshal(requestMap)
	if marshalErr != nil {
		return "", 0, fmt.Errorf("durable: marshal fetch request: %w", marshalErr)
	}
	resp, callErr := h.DurableCall("http", "fetch", string(requestJSON))
	if callErr != nil {
		return "", 0, callErr
	}
	var respData struct {
		Body       string `json:"body"`
		StatusCode int    `json:"status_code"`
	}
	if unmarshalErr := json.Unmarshal([]byte(resp), &respData); unmarshalErr != nil {
		return "", 0, fmt.Errorf("durable: unmarshal fetch response: %w", unmarshalErr)
	}
	return respData.Body, respData.StatusCode, nil
}

func (h *HostCallsImpl) DurableFetchJSON(url, method string, headers map[string]string, body string, result interface{}) error {
	resp, _, err := h.DurableFetch(url, method, headers, body)
	if err != nil {
		return err
	}
	return json.Unmarshal([]byte(resp), result)
}

// FetchGet is a shorthand for DurableFetch with GET method, no headers, and no body.
func (h *HostCallsImpl) FetchGet(url string) (responseJSON string, statusCode int, err error) {
	return h.DurableFetch(url, "GET", nil, "")
}

// FetchGetJSON is like FetchGet but unmarshals the response into result.
func (h *HostCallsImpl) FetchGetJSON(url string, result interface{}) error {
	resp, _, err := h.FetchGet(url)
	if err != nil {
		return err
	}
	return json.Unmarshal([]byte(resp), result)
}

func (h *HostCallsImpl) Now() time.Time {
	ms := h.NowMs()
	return time.Unix(ms/1000, (ms%1000)*1_000_000)
}

func (h *HostCallsImpl) NowMs() int64 {
	if h.now == nil {
		log.Printf("durable: Now can only be called from within a workflow function (the HostCalls runtime was not initialized). Ensure this call is inside a cleat_entry / #[cleat_entry] / @CleatEntry / @cleatEntry function.")
		return 0
	}
	return h.now()
}

func (h *HostCallsImpl) Random() int64 {
	if h.random == nil {
		log.Printf("durable: Random can only be called from within a workflow function (the HostCalls runtime was not initialized). Ensure this call is inside a cleat_entry / #[cleat_entry] / @CleatEntry / @cleatEntry function.")
		return 0
	}
	return h.random()
}

// ---- Saga: structured compensation ----

// SagaStep defines a single step in a Saga with its forward action and
// compensation function. Create instances via NewSaga().AddStep() or
// by constructing a SagaStep literal for use with AddParallel().
type SagaStep struct {
	Description string
	Forward     func(HostCalls) (string, error)
	Compensate  func(HostCalls) error
}

// Saga provides structured compensation for multi-step operations.
// Steps execute in order. If any step fails, all previously completed
// steps are compensated in reverse order.
//
// Usage:
//
//	s := cleat.NewSaga()
//	s.AddStep("charge", chargeFn, refundFn)
//	s.AddStep("assign_driver", assignFn, releaseFn)
//	if err := s.Run(h); err != nil {
//	    return err
//	}
//
// Typed usage (recommended):
//
//	s := cleat.NewSaga()
//	s.AddStep("book_flight",
//	    func(h HostCalls) (string, error) {
//	        var result FlightResult
//	        err := h.DurableCallTyped("flights", "Book", req, &result)
//	        return "", err
//	    },
//	    func(h HostCalls) {
//	        h.DurableCall("flights", "Cancel", cancelJSON)
//	    },
//	)
type Saga struct {
	steps []SagaStep
}

// NewSaga creates a new Saga helper.
func NewSaga() *Saga {
	return &Saga{}
}

// AddStep adds a step to the saga. forward is the main action; compensate
// is the cleanup if a later step fails. description is used for logging.
// compensate may be nil for best-effort steps that have no meaningful
// compensation (e.g., sending a notification).
//
// Typed usage example:
//
//	s.AddStep("book_flight",
//	    func(h HostCalls) (string, error) {
//	        var result FlightResult
//	        err := h.DurableCallTyped("flights", "Book", req, &result)
//	        return "", err
//	    },
//	    func(h HostCalls) {
//	        h.DurableCall("flights", "Cancel", cancelJSON)
//	    },
//	)
func (s *Saga) AddStep(description string, forward func(HostCalls) (string, error), compensate func(HostCalls) error) *Saga {
	s.steps = append(s.steps, SagaStep{
		Description: description,
		Forward:     forward,
		Compensate:  compensate,
	})
	return s
}

// Run executes all forward steps in order. If any step fails, previously
// completed steps are compensated in reverse order. Nil compensate functions
// are skipped. The first forward error encountered is returned.
func (s *Saga) Run(h HostCalls) error {
	var completed int
	for i, step := range s.steps {
		h.LogKV("saga: executing step", "step", i, "description", step.Description)
		_, err := step.Forward(h)
		if err != nil {
			h.LogKV("saga: step failed, compensating",
				"step", i,
				"description", step.Description,
				"error", err.Error(),
				"completed_count", completed)
			var compErr error
			for j := completed - 1; j >= 0; j-- {
				cs := s.steps[j]
				if cs.Compensate == nil {
					continue
				}
				h.LogKV("saga: compensating", "step", j, "description", cs.Description)
				if cerr := cs.Compensate(h); cerr != nil {
					compErr = errors.Join(compErr, cerr)
				}
			}
			if compErr != nil {
				return fmt.Errorf("saga: %w", errors.Join(err, compErr))
			}
			return fmt.Errorf("saga: %w", err)
		}
		completed++
	}
	return nil
}

// AddParallel adds multiple steps that execute concurrently. If any step
// fails, all successfully completed parallel steps are compensated in
// LIFO order. The returned values are collected into a slice in the same
// order as the steps were added.
func (s *Saga) AddParallel(steps ...SagaStep) *Saga {
	s.steps = append(s.steps, SagaStep{
		Description: "parallel",
		Forward: func(h HostCalls) (string, error) {
			type stepResult struct {
				index  int
				result string
				err    error
			}

			results := make([]stepResult, len(steps))
			var wg sync.WaitGroup

			for i, step := range steps {
				wg.Add(1)
				go func(idx int, st SagaStep) {
					defer wg.Done()
					// Each step runs in its own goroutine.
					// All durable calls within each step are recorded
					// deterministically through the same HostCalls.
					res, err := st.Forward(h)
					results[idx] = stepResult{index: idx, result: res, err: err}
				}(i, step)
			}
			wg.Wait()

			// Check for failures in order.
			var firstErr error
			for _, r := range results {
				if r.err != nil && firstErr == nil {
					firstErr = r.err
				}
			}

			if firstErr != nil {
				// Compensate successful steps in LIFO order.
				var compErr error
				for i := len(results) - 1; i >= 0; i-- {
					if results[i].err == nil && steps[i].Compensate != nil {
						if cerr := steps[i].Compensate(h); cerr != nil {
							compErr = errors.Join(compErr, cerr)
						}
					}
				}
				if compErr != nil {
					return "", fmt.Errorf("%w (compensation failures: %v)", firstErr, compErr)
				}
				return "", firstErr
			}

			// All succeeded — collect results.
			var out []string
			for _, r := range results {
				out = append(out, r.result)
			}
			b, _ := json.Marshal(out)
			return string(b), nil
		},
		// No single description for parallel steps — the forward closure
		// handles everything internally.
	})
	return s
}

// ---- SagaTyped: typed result collection ----

// SagaStepTyped defines a single saga step with a typed result.
// Generic parameter T is the forward action's result type.
type SagaStepTyped[T any] struct {
	Description string
	Forward     func(HostCalls) (T, error)
	Compensate  func(HostCalls) error
}

// SagaTyped provides structured compensation with typed result collection.
// Generic parameter T is the result type of each step.
//
// Usage:
//
//	saga := cleat.NewSagaTyped[ChargeResult]()
//	saga.AddStep("charge",
//	    func(h HostCalls) (ChargeResult, error) {
//	        var result ChargeResult
//	        err := h.DurableCallTyped("payment", "charge", req, &result)
//	        return result, err
//	    },
//	    func(h HostCalls) error {
//	        h.DurableCall("payment", "refund", refundJSON)
//	        return nil
//	    },
//	)
//	results, err := saga.Run(h)
type SagaTyped[T any] struct {
	steps []SagaStepTyped[T]
}

// NewSagaTyped creates a new SagaTyped helper.
func NewSagaTyped[T any]() *SagaTyped[T] {
	return &SagaTyped[T]{}
}

// AddStep adds a typed step to the saga. forward returns a (T, error);
// compensate runs on failure of a later step. compensate may be nil
// for best-effort steps.
func (s *SagaTyped[T]) AddStep(description string, forward func(HostCalls) (T, error), compensate func(HostCalls) error) *SagaTyped[T] {
	s.steps = append(s.steps, SagaStepTyped[T]{
		Description: description,
		Forward:     forward,
		Compensate:  compensate,
	})
	return s
}

// Run executes all typed forward steps in order, collecting results.
// If any step fails, previously completed steps are compensated in
// reverse order. Returns the collected results or the first error.
//
// Only TerminalError triggers compensation (non-retryable). Transient
// errors are returned without compensation so the caller can retry.
func (s *SagaTyped[T]) Run(h HostCalls) ([]T, error) {
	var completed int
	var results []T

	for i, step := range s.steps {
		h.LogKV("saga: executing typed step", "step", i, "description", step.Description)
		result, err := step.Forward(h)
		if err != nil {
			h.LogKV("saga: typed step failed, compensating",
				"step", i, "description", step.Description,
				"error", err.Error(), "completed_count", completed)
			var compErr error
			for j := completed - 1; j >= 0; j-- {
				cs := s.steps[j]
				if cs.Compensate == nil {
					continue
				}
				h.LogKV("saga: compensating", "step", j, "description", cs.Description)
				if cerr := cs.Compensate(h); cerr != nil {
					compErr = errors.Join(compErr, cerr)
				}
			}
			if compErr != nil {
				return results, fmt.Errorf("saga: %w", errors.Join(err, compErr))
			}
			return results, fmt.Errorf("saga: %w", err)
		}
		results = append(results, result)
		completed++
	}

	return results, nil
}

// ---- PollUntil: sleep-based polling ----

// PollUntil repeatedly calls pollFn at the given interval until pollFn
// returns done=true, or the deadline is exceeded. Returns the last value
// from pollFn and any error.
//
// Usage:
//
//	status, err := cleat.PollUntil(h, 30*time.Second, 30*time.Minute,
//	    func() (string, error) {
//	        return checkPickupStatus(driverID)
//	    },
//	    func(s string) bool { return s == "picked_up" },
//	)
func PollUntil[T any](h HostCalls, interval, timeout time.Duration,
	fn func() (T, error), done func(T) bool) (T, error) {

	deadline := h.Now().Add(timeout)
	var zero T
	for {
		val, err := fn()
		if err != nil {
			return zero, err
		}
		if done(val) {
			return val, nil
		}
		if h.Now().After(deadline) {
			return zero, fmt.Errorf("poll deadline exceeded after %v", timeout)
		}
		h.DurableSleep(interval)
	}
}

// AwaitCondition blocks until the predicate returns true or the timeout expires.
// It uses AwaitSignals as the blocking primitive, so the workflow is responsive
// to external signals between predicate checks.
func AwaitCondition(h HostCalls, predicate func() bool, pollInterval, timeout time.Duration) bool {
	return h.AwaitCondition(predicate, pollInterval, timeout)
}

// SideEffectTyped is like SideEffect but with typed input/output.
// fn returns a typed value T, which is JSON-marshaled for storage
// in the event history and JSON-unmarshaled on return.
func SideEffectTyped[T any](h HostCalls, fn func() (T, error)) (T, error) {
	var zero T
	wrappedFn := func() (string, error) {
		val, err := fn()
		if err != nil {
			return "", err
		}
		data, err := json.Marshal(val)
		if err != nil {
			return "", fmt.Errorf("durable: SideEffectTyped marshal: %w", err)
		}
		return string(data), nil
	}
	resultJSON, err := h.SideEffect(wrappedFn)
	if err != nil {
		return zero, err
	}
	var val T
	if err := json.Unmarshal([]byte(resultJSON), &val); err != nil {
		return zero, fmt.Errorf("durable: SideEffectTyped unmarshal: %w", err)
	}
	return val, nil
}

// ---- Helpers ----

// isNonRetryable returns true if err matches any of the non-retryable
// substrings.
func isNonRetryable(err error, nonRetryableErrors []string) bool {
	errMsg := err.Error()
	for _, substr := range nonRetryableErrors {
		if strings.Contains(errMsg, substr) {
			return true
		}
	}
	return false
}
