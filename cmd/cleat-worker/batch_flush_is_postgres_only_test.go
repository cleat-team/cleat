package main

import (
	"strings"
	"testing"
)

// cleat#2348: the adaptive batch flusher is PostgreSQL SQL. A worker on another
// driver must not build it, must not reserve the connections its pool would
// take, and must say so once at startup.
func TestBatchFlushIsBuiltOnlyOnPostgres(t *testing.T) {
	for _, tc := range []struct {
		driver          string
		disabled, noPer bool
		wantEnabled     bool
		wantNotice      bool
	}{
		{"postgres", false, false, true, false},
		{"postgres", true, false, false, false},
		{"postgres", false, true, false, false},
		{"mysql", false, false, false, true},
		{"mssql", false, false, false, true},
		// Already off by the operator's own choice: nothing to explain.
		{"mysql", true, false, false, false},
		{"mssql", false, true, false, false},
	} {
		if got := batchFlushEnabled(tc.driver, tc.disabled, tc.noPer); got != tc.wantEnabled {
			t.Errorf("batchFlushEnabled(%q, disabled=%v, noPerStep=%v) = %v, want %v",
				tc.driver, tc.disabled, tc.noPer, got, tc.wantEnabled)
		}
		notice := batchFlushIgnoredNotice(tc.driver, tc.disabled, tc.noPer)
		if (notice != "") != tc.wantNotice {
			t.Errorf("batchFlushIgnoredNotice(%q, disabled=%v, noPerStep=%v) = %q, want a notice: %v",
				tc.driver, tc.disabled, tc.noPer, notice, tc.wantNotice)
		}
		if tc.wantNotice && !strings.Contains(notice, "PostgreSQL-only") {
			t.Errorf("the notice for %q does not say why: %q", tc.driver, notice)
		}
	}
}
