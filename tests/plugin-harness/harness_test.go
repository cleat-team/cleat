package pluginharness

import (
	"io"
	"net/http"
	"testing"
)

// TestNewTestPluginEnvInMemory verifies that the in-memory environment can be
// created and that its non-nil fields are populated.
func TestNewTestPluginEnvInMemory(t *testing.T) {
	env := NewTestPluginEnvInMemory(t)
	defer env.Close()

	if env.Ctx == nil {
		t.Error("expected non-nil Ctx")
	}
	if env.Registry == nil {
		t.Error("expected non-nil Registry")
	}
	if env.StreamReg == nil {
		t.Error("expected non-nil StreamReg")
	}
	if env.MockServers == nil {
		t.Error("expected non-nil MockServers")
	}
	if env.MockServers.LLM == nil {
		t.Error("expected non-nil LLM mock server")
	}
	if env.MockServers.PagerDuty == nil {
		t.Error("expected non-nil PagerDuty mock server")
	}
	if env.MockServers.Slack == nil {
		t.Error("expected non-nil Slack mock server")
	}
	if env.MockServers.Kafka == nil {
		t.Error("expected non-nil Kafka mock server")
	}

	// Plugins may be empty (nil or zero-length) when no plugin packages
	// are imported into the test binary — this is normal for standalone
	// unit tests. The registry itself must still be non-nil.
	if len(env.Plugins) != 0 {
		t.Logf("plugins discovered: %d", len(env.Plugins))
	}
}

// TestMockServers verifies that the mock servers respond correctly to requests.
func TestMockServers(t *testing.T) {
	ms := StartMockServers()
	defer StopMockServers(ms)

	// GET /v1/models should return 200 with a list of models.
	resp, err := ms.LLM.Client().Get(ms.LLM.URL + "/v1/models")
	if err != nil {
		t.Fatalf("GET /v1/models: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	if len(body) == 0 {
		t.Fatal("expected non-empty response body")
	}

	// Verify request tracking.
	if n := ms.RequestCount("GET /v1/models"); n != 1 {
		t.Errorf("expected 1 request to GET /v1/models, got %d", n)
	}

	// POST to LLM chat completions.
	resp, err = ms.LLM.Client().Post(ms.LLM.URL+"/v1/chat/completions", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /v1/chat/completions: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	if n := ms.RequestCount("POST /v1/chat/completions"); n != 1 {
		t.Errorf("expected 1 request to POST /v1/chat/completions, got %d", n)
	}

	// POST to PagerDuty.
	resp, err = ms.PagerDuty.Client().Post(ms.PagerDuty.URL+"/v2/enqueue", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /v2/enqueue: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	if n := ms.RequestCount("POST /v2/enqueue"); n != 1 {
		t.Errorf("expected 1 request to POST /v2/enqueue, got %d", n)
	}

	// POST to Slack.
	resp, err = ms.Slack.Client().Post(ms.Slack.URL+"/", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	// POST to Kafka.
	resp, err = ms.Kafka.Client().Post(ms.Kafka.URL+"/topics/test-topic", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /topics/test-topic: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}
