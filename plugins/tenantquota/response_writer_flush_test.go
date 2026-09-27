package tenantquota

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

// statusRecorder wraps every request the middleware sees, streaming ones included. cleat#2254.
func TestTheQuotaStatusRecorderPassesFlushAndUnwrapThrough(t *testing.T) {
	inner := &flushCounter{ResponseRecorder: httptest.NewRecorder()}
	rec := &statusRecorder{ResponseWriter: inner, status: http.StatusOK}
	var w http.ResponseWriter = rec
	f, ok := w.(http.Flusher)
	if !ok {
		t.Fatal("statusRecorder is not an http.Flusher: every streaming route behind the quota middleware answers 500")
	}
	f.Flush()
	if inner.flushes != 1 {
		t.Errorf("Flush reached the real writer %d times, want 1", inner.flushes)
	}
	if !rec.wroteHeader {
		t.Error("a flush sends an implicit 200, so it counts as the header written")
	}
	if rec.Unwrap() != http.ResponseWriter(inner) {
		t.Error("Unwrap does not return the wrapped writer")
	}
	if err := http.NewResponseController(w).Flush(); err != nil || inner.flushes != 2 {
		t.Errorf("http.ResponseController: err=%v flushes=%d, want nil and 2", err, inner.flushes)
	}
}
