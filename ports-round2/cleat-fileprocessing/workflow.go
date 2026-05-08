package main

import (
	"time"

	"github.com/rcownie/cleat/durable"
)

// ---------------------------------------------------------------------------
// Request / response types for the file processing service
// ---------------------------------------------------------------------------

// DownloadRequest is the input for the download operation.
type DownloadRequest struct {
	FileName string `json:"file_name"`
}

// DownloadResponse is the output of the download operation.
type DownloadResponse struct {
	LocalPath string `json:"local_path"`
}

// ProcessRequest is the input for the process operation.
type ProcessRequest struct {
	FilePath string `json:"file_path"`
}

// ProcessResponse is the output of the process operation.
type ProcessResponse struct {
	ProcessedPath string `json:"processed_path"`
}

// UploadRequest is the input for the upload operation.
type UploadRequest struct {
	FilePath string `json:"file_path"`
}

// ProgressPayload is sent by the host service as heartbeat progress.
type ProgressPayload struct {
	Step     int `json:"step"`
	Progress int `json:"progress"`
}

// ---------------------------------------------------------------------------
// Service and operation names used with DurableCall.
// These identify the host-side service that performs the actual file I/O.
// ---------------------------------------------------------------------------

const (
	FileService = "FileService"
	DownloadOp  = "Download"
	ProcessOp   = "Process"
	UploadOp    = "Upload"
)

// ---------------------------------------------------------------------------
// Workflow
// ---------------------------------------------------------------------------

// SampleFileProcessingWorkflow is the Cleat port of the Temporal fileprocessing
// sample. It downloads a file, processes it (uppercase transform), and uploads
// the result. The entire sequence is retried up to 4 additional times on
// failure, mirroring the Temporal original's retry-the-whole-pipeline pattern.
//
// In the Temporal original this uses sessions to ensure all activities run on
// the same worker (for local file affinity). Cleat does not have a session /
// worker-affinity concept, so the pipeline runs as sequential DurableCall
// operations. File operations must live in host-side services because the
// workflow runs in WASM and cannot access the local filesystem directly.
func SampleFileProcessingWorkflow(h cleat.HostCalls, fileName string) (err error) {
	for i := 1; i < 5; i++ {
		err = processFile(h, fileName)
		if err == nil {
			break
		}
		h.LogKV("Retrying workflow after error", "attempt", i, "error", err.Error())
	}
	if err != nil {
		h.LogKV("Workflow failed.", "Error", err.Error())
	} else {
		h.DurableLog("Workflow completed.")
	}
	return err
}

// processFile executes the three-step file processing pipeline:
//  1. Download the file (obtain a local path from the host service)
//  2. Process the downloaded file (transform its contents)
//  3. Upload the processed file
//
// Each step calls the FileService host-side service via DurableCallTyped.
// The process step demonstrates DurableCallWithHeartbeat for progress
// reporting, and per-call retry via CallOptions.
func processFile(h cleat.HostCalls, fileName string) error {
	// Shared retry policy for all file operations, matching the Temporal
	// original's ActivityOptions.
	opts := cleat.CallOptions{
		Retry: &cleat.RetryPolicy{
			MaxAttempts:        3,
			InitialInterval:    1 * time.Second,
			BackoffCoefficient: 2.0,
			MaxInterval:        30 * time.Second,
		},
	}

	// -----------------------------------------------------------------------
	// Step 1: Download
	// -----------------------------------------------------------------------
	var downloadResp DownloadResponse
	err := h.DurableCallTypedWithOptions(opts, FileService, DownloadOp,
		DownloadRequest{FileName: fileName}, &downloadResp)
	if err != nil {
		h.LogKV("Download failed", "error", err.Error())
		return err
	}
	h.LogKV("Download complete", "local_path", downloadResp.LocalPath)

	// -----------------------------------------------------------------------
	// Step 2: Process the downloaded file with heartbeat progress reporting
	// -----------------------------------------------------------------------
	var processResp ProcessResponse
	err = h.DurableCallTypedWithHeartbeat(
		FileService, ProcessOp,
		ProcessRequest{FilePath: downloadResp.LocalPath},
		&processResp,
		1*time.Second, // heartbeat interval
		func(progressJSON string) {
			h.LogKV("Processing progress", "progress", progressJSON)
		},
	)
	if err != nil {
		h.LogKV("Processing failed", "error", err.Error())
		return err
	}
	h.LogKV("Processing complete", "processed_path", processResp.ProcessedPath)

	// -----------------------------------------------------------------------
	// Step 3: Upload the processed file
	// -----------------------------------------------------------------------
	err = h.DurableCallTypedWithOptions(opts, FileService, UploadOp,
		UploadRequest{FilePath: processResp.ProcessedPath}, nil)
	if err != nil {
		h.LogKV("Upload failed", "error", err.Error())
		return err
	}
	h.LogKV("Upload complete")
	return nil
}
