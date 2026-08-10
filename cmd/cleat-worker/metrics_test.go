package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandleMetrics(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	handleMetrics(w, req)

	// When globalWorker.Metrics is nil, handleMetrics should return 503.
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected status 503, got %d", w.Code)
	}
}
