package backendkit

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

type flushCounter struct {
	*httptest.ResponseRecorder
	flushes int
}

func (f *flushCounter) Flush() { f.flushes++; f.ResponseRecorder.Flush() }

// LoggingMiddleware wraps handlers that may stream. cleat#2254.
func TestTheLoggingResponseWriterPassesFlushAndUnwrapThrough(t *testing.T) {
	inner := &flushCounter{ResponseRecorder: httptest.NewRecorder()}
	rw := newResponseWriter(inner)
	var w http.ResponseWriter = rw
	f, ok := w.(http.Flusher)
	if !ok {
		t.Fatal("the logging responseWriter is not an http.Flusher: a streaming handler behind it answers 500")
	}
	f.Flush()
	if inner.flushes != 1 {
		t.Errorf("Flush reached the real writer %d times, want 1", inner.flushes)
	}
	if rw.Unwrap() != http.ResponseWriter(inner) {
		t.Error("Unwrap does not return the wrapped writer")
	}
	if err := http.NewResponseController(w).Flush(); err != nil || inner.flushes != 2 {
		t.Errorf("http.ResponseController: err=%v flushes=%d, want nil and 2", err, inner.flushes)
	}
}
