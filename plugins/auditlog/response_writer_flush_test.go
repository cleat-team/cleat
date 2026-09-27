package auditlog

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// flushCounter is a ResponseWriter with a Flush the test can count, which httptest.ResponseRecorder
// also has but does not count.
type flushCounter struct {
	*httptest.ResponseRecorder
	flushes int
}

func (f *flushCounter) Flush() { f.flushes++; f.ResponseRecorder.Flush() }

// The audit wrapper sits in front of every route, so it must pass Flush through and let
// http.ResponseController reach the real writer. cleat#2254.
func TestTheAuditResponseWriterPassesFlushAndUnwrapThrough(t *testing.T) {
	inner := &flushCounter{ResponseRecorder: httptest.NewRecorder()}
	rw := &responseWriter{ResponseWriter: inner, statusCode: http.StatusOK}

	var w http.ResponseWriter = rw
	f, ok := w.(http.Flusher)
	if !ok {
		t.Fatal("the audit responseWriter is not an http.Flusher: every streaming route behind it answers 500")
	}
	f.Flush()
	if inner.flushes != 1 {
		t.Errorf("Flush reached the real writer %d times, want 1: the wrapper accepts a flush and swallows it", inner.flushes)
	}
	if !rw.written {
		t.Error("a flush sends an implicit 200, so it counts as the header written")
	}
	// After that a late WriteHeader must not change what is recorded (the response already said 200).
	w.WriteHeader(http.StatusTeapot)
	if rw.statusCode != http.StatusOK {
		t.Errorf("recorded status %d after a flush sent the implicit 200, want 200", rw.statusCode)
	}

	if rw.Unwrap() != http.ResponseWriter(inner) {
		t.Error("Unwrap does not return the wrapped writer")
	}
	if err := http.NewResponseController(w).Flush(); err != nil {
		t.Errorf("http.ResponseController cannot flush through the wrapper: %v", err)
	}
	if inner.flushes != 2 {
		t.Errorf("the ResponseController's flush reached the real writer %d times in all, want 2", inner.flushes)
	}
}
