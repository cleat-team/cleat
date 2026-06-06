package plugin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetchIndexFromURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`
plugins:
  - name: http-plugin
    description: Plugin fetched from HTTP
    author: test
    versions:
      - version: 1.0.0
        wasm_url: https://example.com/plugin.wasm
        checksum: abc123
`))
	}))
	defer server.Close()

	idx, err := FetchIndex(context.Background(), server.URL)
	if err != nil {
		t.Fatalf("FetchIndex from URL failed: %v", err)
	}
	if len(idx.Plugins) != 1 {
		t.Fatalf("expected 1 plugin, got %d", len(idx.Plugins))
	}
	if idx.Plugins[0].Name != "http-plugin" {
		t.Errorf("expected name http-plugin, got %q", idx.Plugins[0].Name)
	}
}

func TestFetchIndexFromURLNotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	_, err := FetchIndex(context.Background(), server.URL)
	if err == nil {
		t.Fatal("expected error for 404 response")
	}
}

func TestDownloadWASMSuccess(t *testing.T) {
	wasmContent := []byte("\x00asm\x01\x00\x00\x00")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write(wasmContent)
	}))
	defer server.Close()

	data, err := DownloadWASM(context.Background(), server.URL)
	if err != nil {
		t.Fatalf("DownloadWASM failed: %v", err)
	}
	if len(data) != len(wasmContent) {
		t.Errorf("expected %d bytes, got %d", len(wasmContent), len(data))
	}
}

func TestDownloadWASMNotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	_, err := DownloadWASM(context.Background(), server.URL)
	if err == nil {
		t.Fatal("expected error for 404")
	}
}

func TestIsHTTPURL(t *testing.T) {
	tests := []struct {
		url  string
		want bool
	}{
		{"http://example.com", true},
		{"https://example.com", true},
		{"HTTP://example.com", false},   // case-sensitive check
		{"HTTPS://example.com", false},  // case-sensitive check
		{"/local/path", false},
		{"relative/path.yaml", false},
		{"file:///tmp/test.yaml", false},
	}
	for _, tc := range tests {
		got := isHTTPURL(tc.url)
		if got != tc.want {
			t.Errorf("isHTTPURL(%q) = %v, want %v", tc.url, got, tc.want)
		}
	}
}
