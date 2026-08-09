package embedded

import (
	"strings"
	"testing"
)

// TestCronCallsExplainWhyTheyCannotWork.
//
// A nil hook would answer "the HostCalls runtime was not initialized", which
// is about workflow context and reads as the caller's fault. The embedded
// runner has no schedule store, so the honest answer names that.
func TestCronCallsExplainWhyTheyCannotWork(t *testing.T) {
	e := &execution{}
	h := e.hostCalls()

	if _, err := h.ScheduleCron("wf", "* * * * *", "UTC", "{}"); err == nil {
		t.Error("ScheduleCron reported success; nothing in this runner can fire a schedule")
	} else if !strings.Contains(err.Error(), "schedule store") {
		t.Errorf("ScheduleCron error = %v, want it to name the missing schedule store", err)
	}

	if err := h.DeleteCron("cron-1"); err == nil || !strings.Contains(err.Error(), "schedule store") {
		t.Errorf("DeleteCron error = %v, want it to name the missing schedule store", err)
	}

	if _, err := h.ListCrons(); err == nil || !strings.Contains(err.Error(), "schedule store") {
		t.Errorf("ListCrons error = %v, want it to name the missing schedule store", err)
	}
}
