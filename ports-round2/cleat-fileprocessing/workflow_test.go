package main

import (
	"testing"

	"github.com/rcownie/cleat/cleat/cleattest"
)

// TestSampleFileProcessingWorkflow_Success verifies the happy path:
// download -> process -> upload, all operations succeed.
func TestSampleFileProcessingWorkflow_Success(t *testing.T) {
	env := cleattest.NewTestEnv()
	h := env.H()

	// Stub all three service calls.
	env.OnCall(FileService, DownloadOp, nil).ReturnJSON(DownloadResponse{
		LocalPath: "/tmp/test_download.txt",
	}, nil)
	env.OnCall(FileService, ProcessOp, nil).ReturnJSON(ProcessResponse{
		ProcessedPath: "/tmp/test_processed.txt",
	}, nil)
	env.OnCall(FileService, UploadOp, nil).Return(`{}`, nil)

	// Execute the workflow.
	err := SampleFileProcessingWorkflow(h, "test_file.txt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify all three service calls were made.
	env.AssertCalled(t, FileService, DownloadOp)
	env.AssertCalled(t, FileService, ProcessOp)
	env.AssertCalled(t, FileService, UploadOp)

	// Verify the call order by inspecting the history.
	history := env.CallHistory()
	if len(history) != 3 {
		t.Fatalf("expected 3 service calls, got %d", len(history))
	}

	calls := []struct {
		service   string
		operation string
	}{
		{FileService, DownloadOp},
		{FileService, ProcessOp},
		{FileService, UploadOp},
	}
	for i, want := range calls {
		if history[i].Service != want.service || history[i].Operation != want.operation {
			t.Errorf("call[%d]: expected %s.%s, got %s.%s",
				i, want.service, want.operation, history[i].Service, history[i].Operation)
		}
	}
}

// TestSampleFileProcessingWorkflow_RetryThenSucceed verifies that the outer
// retry loop re-invokes the pipeline when a step fails, and succeeds when the
// retry succeeds.
//
// We simulate failure by exhausting stubs on the first attempt (register only
// one set of stubs) -- on retry we provide fresh stubs. The test env does not
// re-arm stubs automatically, so we leave a second set available for the retry.
func TestSampleFileProcessingWorkflow_RetryThenSucceed(t *testing.T) {
	env := cleattest.NewTestEnv()
	h := env.H()

	// First attempt stubs: Download succeeds, Process fails.
	env.OnCall(FileService, DownloadOp, nil).ReturnJSON(DownloadResponse{
		LocalPath: "/tmp/dl_attempt1.txt",
	}, nil)
	env.OnCall(FileService, ProcessOp, nil).Return("", assertError("processing failed"))

	// Second attempt stubs (retry): all succeed.
	env.OnCall(FileService, DownloadOp, nil).ReturnJSON(DownloadResponse{
		LocalPath: "/tmp/dl_attempt2.txt",
	}, nil)
	env.OnCall(FileService, ProcessOp, nil).ReturnJSON(ProcessResponse{
		ProcessedPath: "/tmp/proc_attempt2.txt",
	}, nil)
	env.OnCall(FileService, UploadOp, nil).Return(`{}`, nil)

	err := SampleFileProcessingWorkflow(h, "retry_file.txt")
	if err != nil {
		t.Fatalf("expected success after retry, got: %v", err)
	}

	// We expect 5 total calls: 2 Downloads, 2 Processes, 1 Upload.
	history := env.CallHistory()
	if len(history) != 5 {
		t.Fatalf("expected 5 service calls across retry, got %d", len(history))
	}
}

// TestSampleFileProcessingWorkflow_AllRetriesExhausted verifies that when the
// outer retry loop is exhausted the workflow returns an error.
func TestSampleFileProcessingWorkflow_AllRetriesExhausted(t *testing.T) {
	env := cleattest.NewTestEnv()
	h := env.H()

	// All Download calls fail -- the outer loop will retry 4 times then give up.
	// We register 5 stubs (initial + 4 retries).
	for i := 0; i < 5; i++ {
		env.OnCall(FileService, DownloadOp, nil).Return("", assertError("service unavailable"))
	}

	err := SampleFileProcessingWorkflow(h, "fail_file.txt")
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}
}

// assertError returns a simple error value for stub error simulation.
func assertError(msg string) error {
	return &simpleError{msg: msg}
}

type simpleError struct {
	msg string
}

func (e *simpleError) Error() string { return e.msg }
