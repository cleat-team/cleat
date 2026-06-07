package backendkit_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/cleat/backendkit"
)

func TestNew(t *testing.T) {
	t.Run("strips trailing slash", func(t *testing.T) {
		c := backendkit.New("http://localhost:8080/")
		if c.BaseURL != "http://localhost:8080" {
			t.Errorf("BaseURL = %q, want %q", c.BaseURL, "http://localhost:8080")
		}
	})

	t.Run("no trailing slash", func(t *testing.T) {
		c := backendkit.New("http://localhost:8080")
		if c.BaseURL != "http://localhost:8080" {
			t.Errorf("BaseURL = %q, want %q", c.BaseURL, "http://localhost:8080")
		}
	})

	t.Run("default timeout", func(t *testing.T) {
		c := backendkit.New("http://localhost:8080")
		if c.HTTPClient == nil {
			t.Fatal("HTTPClient is nil")
		}
		if c.HTTPClient.Timeout != 30*time.Second {
			t.Errorf("HTTPClient.Timeout = %v, want 30s", c.HTTPClient.Timeout)
		}
	})
}

func TestStartWorkflow(t *testing.T) {
	t.Run("happy path", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				t.Errorf("method = %s, want POST", r.Method)
			}
			if want := "/api/workflows/test-wf/start"; r.URL.Path != want {
				t.Errorf("path = %s, want %s", r.URL.Path, want)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]string{"id": "wf-123"})
		}))
		defer srv.Close()

		c := backendkit.New(srv.URL)
		id, err := c.StartWorkflow(context.Background(), "test-wf", "", map[string]string{"key": "val"}, "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if id != "wf-123" {
			t.Errorf("id = %q, want %q", id, "wf-123")
		}
	})

	t.Run("with explicit tenantID", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var body map[string]interface{}
			json.NewDecoder(r.Body).Decode(&body)
			if body["tenant_id"] != "t-456" {
				t.Errorf("tenant_id = %v, want t-456", body["tenant_id"])
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]string{"id": "wf-789"})
		}))
		defer srv.Close()

		c := backendkit.New(srv.URL)
		id, err := c.StartWorkflow(context.Background(), "test-wf", "", nil, "t-456")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if id != "wf-789" {
			t.Errorf("id = %q, want %q", id, "wf-789")
		}
	})

	t.Run("with Client.TenantID fallback", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var body map[string]interface{}
			json.NewDecoder(r.Body).Decode(&body)
			if body["tenant_id"] != "client-tenant" {
				t.Errorf("tenant_id = %v, want client-tenant", body["tenant_id"])
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]string{"id": "wf-999"})
		}))
		defer srv.Close()

		c := backendkit.New(srv.URL)
		c.TenantID = "client-tenant"
		id, err := c.StartWorkflow(context.Background(), "test-wf", "", nil, "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if id != "wf-999" {
			t.Errorf("id = %q, want %q", id, "wf-999")
		}
	})

	t.Run("with entryPoint", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var body map[string]interface{}
			json.NewDecoder(r.Body).Decode(&body)
			if body["entry_point"] != "MyHandler" {
				t.Errorf("entry_point = %v, want MyHandler", body["entry_point"])
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]string{"id": "wf-ep"})
		}))
		defer srv.Close()

		c := backendkit.New(srv.URL)
		id, err := c.StartWorkflow(context.Background(), "test-wf", "MyHandler", nil, "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if id != "wf-ep" {
			t.Errorf("id = %q, want %q", id, "wf-ep")
		}
	})

	t.Run("empty entryPoint omitted", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var body map[string]interface{}
			json.NewDecoder(r.Body).Decode(&body)
			if _, ok := body["entry_point"]; ok {
				t.Error("entry_point should be omitted when empty")
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]string{"id": "wf-noep"})
		}))
		defer srv.Close()

		c := backendkit.New(srv.URL)
		_, err := c.StartWorkflow(context.Background(), "test-wf", "", nil, "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("HTTP 500 error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("internal failure"))
		}))
		defer srv.Close()

		c := backendkit.New(srv.URL)
		_, err := c.StartWorkflow(context.Background(), "test-wf", "", nil, "")
		if err == nil {
			t.Fatal("expected error for 500 response")
		}
		if !strings.Contains(err.Error(), "500") {
			t.Errorf("error should mention status 500: %v", err)
		}
	})

	t.Run("HTTP 400 error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte("bad input"))
		}))
		defer srv.Close()

		c := backendkit.New(srv.URL)
		_, err := c.StartWorkflow(context.Background(), "test-wf", "", nil, "")
		if err == nil {
			t.Fatal("expected error for 400 response")
		}
	})

	t.Run("invalid JSON response", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("not json"))
		}))
		defer srv.Close()

		c := backendkit.New(srv.URL)
		_, err := c.StartWorkflow(context.Background(), "test-wf", "", nil, "")
		if err == nil {
			t.Fatal("expected decode error")
		}
	})

	t.Run("connection refused", func(t *testing.T) {
		c := backendkit.New("http://127.0.0.1:1") // no server listening
		_, err := c.StartWorkflow(context.Background(), "test-wf", "", nil, "")
		if err == nil {
			t.Fatal("expected connection error")
		}
	})
}

func TestStartWorkflowRaw(t *testing.T) {
	t.Run("happy path", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var body map[string]interface{}
			json.NewDecoder(r.Body).Decode(&body)
			if _, ok := body["input"]; !ok {
				t.Error("input should be present")
			}
			if body["entry_point"] != "Run" {
				t.Errorf("entry_point = %v, want Run", body["entry_point"])
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]string{"id": "wf-raw"})
		}))
		defer srv.Close()

		c := backendkit.New(srv.URL)
		id, err := c.StartWorkflowRaw(context.Background(), "test-wf", "Run", json.RawMessage(`{"a":1}`), "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if id != "wf-raw" {
			t.Errorf("id = %q, want %q", id, "wf-raw")
		}
	})

	t.Run("with Client.TenantID fallback", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var body map[string]interface{}
			json.NewDecoder(r.Body).Decode(&body)
			if body["tenant_id"] != "ct" {
				t.Errorf("tenant_id = %v, want ct", body["tenant_id"])
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]string{"id": "wf-ct"})
		}))
		defer srv.Close()

		c := backendkit.New(srv.URL)
		c.TenantID = "ct"
		id, err := c.StartWorkflowRaw(context.Background(), "test-wf", "Run", json.RawMessage(`{}`), "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if id != "wf-ct" {
			t.Errorf("id = %q, want %q", id, "wf-ct")
		}
	})

	t.Run("with explicit tenantID", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var body map[string]interface{}
			json.NewDecoder(r.Body).Decode(&body)
			if body["tenant_id"] != "explicit-tid" {
				t.Errorf("tenant_id = %v, want explicit-tid", body["tenant_id"])
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]string{"id": "wf-exp"})
		}))
		defer srv.Close()

		c := backendkit.New(srv.URL)
		id, err := c.StartWorkflowRaw(context.Background(), "test-wf", "Run", json.RawMessage(`{}`), "explicit-tid")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if id != "wf-exp" {
			t.Errorf("id = %q, want %q", id, "wf-exp")
		}
	})
}

func TestSignalWorkflow(t *testing.T) {
	t.Run("happy path", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				t.Errorf("method = %s, want POST", r.Method)
			}
			if want := "/api/workflows/wf-1/signal"; r.URL.Path != want {
				t.Errorf("path = %s, want %s", r.URL.Path, want)
			}
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		c := backendkit.New(srv.URL)
		err := c.SignalWorkflow(context.Background(), "wf-1", "cancel", `{"reason":"test"}`)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("HTTP 500 error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()

		c := backendkit.New(srv.URL)
		err := c.SignalWorkflow(context.Background(), "wf-1", "cancel", "")
		if err == nil {
			t.Fatal("expected error for 500 response")
		}
	})
}

func TestListWorkflows(t *testing.T) {
	t.Run("happy path", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				t.Errorf("method = %s, want GET", r.Method)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode([]backendkit.WorkflowSummary{
				{ID: "wf-1", Status: "running"},
				{ID: "wf-2", Status: "done"},
			})
		}))
		defer srv.Close()

		c := backendkit.New(srv.URL)
		workflows, err := c.ListWorkflows(context.Background(), "", 0)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(workflows) != 2 {
			t.Errorf("got %d workflows, want 2", len(workflows))
		}
	})

	t.Run("with status filter and limit", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			if q.Get("status") != "running" {
				t.Errorf("status = %q, want running", q.Get("status"))
			}
			if q.Get("limit") != "10" {
				t.Errorf("limit = %q, want 10", q.Get("limit"))
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode([]backendkit.WorkflowSummary{})
		}))
		defer srv.Close()

		c := backendkit.New(srv.URL)
		_, err := c.ListWorkflows(context.Background(), "running", 10)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("no query params when empty", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.RawQuery != "" {
				t.Errorf("RawQuery = %q, want empty", r.URL.RawQuery)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode([]backendkit.WorkflowSummary{})
		}))
		defer srv.Close()

		c := backendkit.New(srv.URL)
		_, err := c.ListWorkflows(context.Background(), "", 0)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("invalid JSON response", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("garbage"))
		}))
		defer srv.Close()

		c := backendkit.New(srv.URL)
		_, err := c.ListWorkflows(context.Background(), "", 0)
		if err == nil {
			t.Fatal("expected decode error")
		}
	})
}

func TestGetWorkflow(t *testing.T) {
	t.Run("happy path", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				t.Errorf("method = %s, want GET", r.Method)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(backendkit.WorkflowDetail{
				ID:      "wf-1",
				Status:  "running",
				DefName: "test-wf",
			})
		}))
		defer srv.Close()

		c := backendkit.New(srv.URL)
		detail, err := c.GetWorkflow(context.Background(), "wf-1")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if detail.ID != "wf-1" {
			t.Errorf("ID = %q, want wf-1", detail.ID)
		}
		if detail.DefName != "test-wf" {
			t.Errorf("DefName = %q, want test-wf", detail.DefName)
		}
	})

	t.Run("404 not found", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte("not found"))
		}))
		defer srv.Close()

		c := backendkit.New(srv.URL)
		_, err := c.GetWorkflow(context.Background(), "wf-missing")
		if err == nil {
			t.Fatal("expected error for 404")
		}
	})

	t.Run("invalid JSON", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("not json"))
		}))
		defer srv.Close()

		c := backendkit.New(srv.URL)
		_, err := c.GetWorkflow(context.Background(), "wf-1")
		if err == nil {
			t.Fatal("expected decode error")
		}
	})
}

func TestQueryState(t *testing.T) {
	t.Run("happy path", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("key") != "my-key" {
				t.Errorf("key = %q, want my-key", r.URL.Query().Get("key"))
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]string{"key": "my-key", "value": "hello"})
		}))
		defer srv.Close()

		c := backendkit.New(srv.URL)
		value, err := c.QueryState(context.Background(), "wf-1", "my-key")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if value != "hello" {
			t.Errorf("value = %q, want hello", value)
		}
	})

	t.Run("HTTP 500 error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()

		c := backendkit.New(srv.URL)
		_, err := c.QueryState(context.Background(), "wf-1", "key")
		if err == nil {
			t.Fatal("expected error for 500")
		}
	})
}

func TestGetWorkflowState(t *testing.T) {
	t.Run("happy path", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if want := "/api/workflows/wf-1/state"; r.URL.Path != want {
				t.Errorf("path = %s, want %s", r.URL.Path, want)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]string{"k1": "v1", "k2": "v2"})
		}))
		defer srv.Close()

		c := backendkit.New(srv.URL)
		state, err := c.GetWorkflowState(context.Background(), "wf-1")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(state) != 2 {
			t.Errorf("got %d entries, want 2", len(state))
		}
	})

	t.Run("empty state", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("{}"))
		}))
		defer srv.Close()

		c := backendkit.New(srv.URL)
		state, err := c.GetWorkflowState(context.Background(), "wf-1")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(state) != 0 {
			t.Errorf("got %d entries, want 0", len(state))
		}
	})

	t.Run("invalid JSON", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("bad"))
		}))
		defer srv.Close()

		c := backendkit.New(srv.URL)
		_, err := c.GetWorkflowState(context.Background(), "wf-1")
		if err == nil {
			t.Fatal("expected decode error")
		}
	})
}

func TestGetHistory(t *testing.T) {
	t.Run("happy path", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			if q.Get("offset") != "0" {
				t.Errorf("offset = %q, want 0", q.Get("offset"))
			}
			if q.Get("limit") != "20" {
				t.Errorf("limit = %q, want 20", q.Get("limit"))
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode([]backendkit.HistoryEvent{
				{Step: 1, Type: "WorkflowStarted"},
			})
		}))
		defer srv.Close()

		c := backendkit.New(srv.URL)
		events, err := c.GetHistory(context.Background(), "wf-1", 0, 20)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(events) != 1 {
			t.Errorf("got %d events, want 1", len(events))
		}
	})

	t.Run("HTTP 500 error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()

		c := backendkit.New(srv.URL)
		_, err := c.GetHistory(context.Background(), "wf-1", 0, 10)
		if err == nil {
			t.Fatal("expected error for 500")
		}
	})
}

func TestDeleteWorkflow(t *testing.T) {
	t.Run("happy path", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodDelete {
				t.Errorf("method = %s, want DELETE", r.Method)
			}
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		c := backendkit.New(srv.URL)
		err := c.DeleteWorkflow(context.Background(), "wf-1")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("HTTP 404 error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		defer srv.Close()

		c := backendkit.New(srv.URL)
		err := c.DeleteWorkflow(context.Background(), "wf-missing")
		if err == nil {
			t.Fatal("expected error for 404")
		}
	})
}

func TestCallPlugin(t *testing.T) {
	t.Run("happy path", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				t.Errorf("method = %s, want POST", r.Method)
			}
			if want := "/api/plugins/llm/generate"; r.URL.Path != want {
				t.Errorf("path = %s, want %s", r.URL.Path, want)
			}
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("plugin response"))
		}))
		defer srv.Close()

		c := backendkit.New(srv.URL)
		resp, err := c.CallPlugin(context.Background(), "llm", "generate", `{"prompt":"hi"}`)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if resp != "plugin response" {
			t.Errorf("response = %q, want %q", resp, "plugin response")
		}
	})

	t.Run("HTTP 400 error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
		}))
		defer srv.Close()

		c := backendkit.New(srv.URL)
		_, err := c.CallPlugin(context.Background(), "bad", "fn", "{}")
		if err == nil {
			t.Fatal("expected error for 400")
		}
	})

	t.Run("HTTP 500 error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()

		c := backendkit.New(srv.URL)
		_, err := c.CallPlugin(context.Background(), "plugin", "fn", "{}")
		if err == nil {
			t.Fatal("expected error for 500")
		}
	})
}

func TestHealth(t *testing.T) {
	t.Run("healthy", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if want := "/healthz"; r.URL.Path != want {
				t.Errorf("path = %s, want %s", r.URL.Path, want)
			}
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		c := backendkit.New(srv.URL)
		healthy, err := c.Health(context.Background())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !healthy {
			t.Error("Health() = false, want true")
		}
	})

	t.Run("unhealthy 503", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer srv.Close()

		c := backendkit.New(srv.URL)
		healthy, err := c.Health(context.Background())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if healthy {
			t.Error("Health() = true, want false for 503")
		}
	})

	t.Run("connection error", func(t *testing.T) {
		c := backendkit.New("http://127.0.0.1:1")
		healthy, err := c.Health(context.Background())
		if err == nil {
			t.Fatal("expected connection error")
		}
		if healthy {
			t.Error("Health() = true, want false on connection error")
		}
	})
}
