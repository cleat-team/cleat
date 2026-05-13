package pluginharness

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
)

// MockServers holds httptest.Server instances for external services that
// plugins may interact with.
type MockServers struct {
	LLM       *httptest.Server
	PagerDuty *httptest.Server
	Slack     *httptest.Server
	Kafka     *httptest.Server

	// tracked tracks request counts and last bodies per method+path.
	tracked map[string]*mockServerState
}

// mockServerState tracks requests to a specific endpoint.
type mockServerState struct {
	mu       sync.Mutex
	count    int
	lastBody []byte
}

// StartMockServers creates and starts four mock HTTP servers (LLM, PagerDuty,
// Slack, Kafka REST) and returns them in a MockServers struct.
//
// Each server handler records request count and last request body so tests
// can inspect them via MockServers.RequestCount("POST /v1/chat/completions")
// and MockServers.LastBody("POST /v1/chat/completions").
func StartMockServers() *MockServers {
	ms := &MockServers{
		tracked: make(map[string]*mockServerState),
	}

	ms.LLM = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.Path
		body := readBody(r)
		ms.trackRequest(key, body)

		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"chat-mock-1","object":"chat.completion","created":1700000000,"model":"mock-model","choices":[{"index":0,"message":{"role":"assistant","content":"Hello! This is a mock response from the test harness."},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":10,"total_tokens":20}}`))

		case r.Method == http.MethodPost && r.URL.Path == "/v1/embeddings":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"object":"list","data":[{"object":"embedding","embedding":[0.01,0.02,0.03],"index":0}],"model":"text-embedding-mock","usage":{"prompt_tokens":2,"total_tokens":2}}`))

		case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"mock-model","object":"model","created":1700000000,"owned_by":"mock"}]}`))

		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"msg_mock1","type":"message","role":"assistant","content":[{"type":"text","text":"Hello! Mock response."}],"model":"claude-mock","stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":10}}`))

		default:
			http.NotFound(w, r)
		}
	}))

	ms.PagerDuty = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.Path
		body := readBody(r)
		ms.trackRequest(key, body)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"success","message":"Event processed","dedup_key":"abc123","incident_key":"mock-key-001"}`))
	}))

	ms.Slack = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.Path
		body := readBody(r)
		ms.trackRequest(key, body)

		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))

	ms.Kafka = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.Path
		body := readBody(r)
		ms.trackRequest(key, body)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"offsets":[{"partition":0,"offset":0,"error_code":null,"error":null}],"key_schema_id":null,"value_schema_id":null}`))
	}))

	return ms
}

// StopMockServers closes all mock HTTP servers.
func StopMockServers(ms *MockServers) {
	if ms == nil {
		return
	}
	closeServer(ms.LLM)
	closeServer(ms.PagerDuty)
	closeServer(ms.Slack)
	closeServer(ms.Kafka)
}

// RequestCount returns the number of requests made to the given endpoint key
// (e.g., "POST /v1/chat/completions"). Returns 0 if the endpoint was never
// hit.
func (ms *MockServers) RequestCount(key string) int {
	if ms.tracked == nil {
		return 0
	}
	s, ok := ms.tracked[key]
	if !ok {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.count
}

// LastBody returns the raw body of the last request made to the given
// endpoint key. Returns nil if the endpoint was never hit.
func (ms *MockServers) LastBody(key string) []byte {
	if ms.tracked == nil {
		return nil
	}
	s, ok := ms.tracked[key]
	if !ok {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastBody
}

// LastBodyString returns the last request body as a string, or "" if never hit.
func (ms *MockServers) LastBodyString(key string) string {
	b := ms.LastBody(key)
	if b == nil {
		return ""
	}
	return string(b)
}

// LastBodyJSON unmarshals the last request body for the given endpoint into
// the provided target. Returns an error if the endpoint was never hit or if
// JSON decoding fails.
func (ms *MockServers) LastBodyJSON(key string, target interface{}) error {
	b := ms.LastBody(key)
	if b == nil {
		return fmt.Errorf("mock server: no request recorded for %s", key)
	}
	return json.Unmarshal(b, target)
}

// Reset clears all tracked request data for all endpoints.
func (ms *MockServers) Reset() {
	if ms.tracked == nil {
		return
	}
	for _, s := range ms.tracked {
		s.mu.Lock()
		s.count = 0
		s.lastBody = nil
		s.mu.Unlock()
	}
}

// trackRequest records a request to the given key.
func (ms *MockServers) trackRequest(key string, body []byte) {
	if ms.tracked == nil {
		ms.tracked = make(map[string]*mockServerState)
	}
	s, ok := ms.tracked[key]
	if !ok {
		s = &mockServerState{}
		ms.tracked[key] = s
	}
	s.mu.Lock()
	s.count++
	s.lastBody = body
	s.mu.Unlock()
}

// readBody reads the full request body and returns it.
func readBody(r *http.Request) []byte {
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	return b
}

// closeServer closes an httptest.Server if non-nil.
func closeServer(s *httptest.Server) {
	if s != nil {
		s.Close()
	}
}
