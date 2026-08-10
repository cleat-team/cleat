package localdev

import (
	"strings"
	"testing"
)

// TestCronCallsExplainWhyTheyCannotWork: LocalRunner keeps its history in
// memory and has no schedule store, so a schedule created here would have
// nothing to fire it. The nil-hook message ("the HostCalls runtime was not
// initialized") would blame the caller for a limit of the runner.
func TestCronCallsExplainWhyTheyCannotWork(t *testing.T) {
	h := NewLocalRunner().H()

	if _, err := h.ScheduleCron("wf", "* * * * *", "UTC", "{}"); err == nil {
		t.Error("ScheduleCron reported success; nothing in localdev can fire a schedule")
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
