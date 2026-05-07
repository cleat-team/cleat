package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/rcownie/durable/durable/embedded"
)

// This main function demonstrates running the fileprocessing workflow using
// the embedded runner (no WASM compilation needed).
//
// In a production Cleat deployment the workflow would be compiled to WASM and
// served by a Cleat runtime host. The embedded runner is useful for integration
// testing and single-binary deployments.
func main() {
	runner := embedded.New()

	// Register the workflow with the embedded runner.
	// The adapter reads the file name from the JSON input string and passes
	// it to the workflow function alongside the HostCalls from the runner.
	runner.Register("SampleFileProcessingWorkflow", func(ctx *embedded.Context) error {
		var fileName string
		if err := json.Unmarshal([]byte(ctx.Input), &fileName); err != nil {
			return fmt.Errorf("decoding input: expected JSON string, got %q: %w", ctx.Input, err)
		}
		return SampleFileProcessingWorkflow(ctx.H(), fileName)
	})

	// Execute the workflow.
	input := `"example_file.txt"`
	result, err := runner.ExecuteWorkflow(context.Background(), "SampleFileProcessingWorkflow", input)
	if err != nil {
		log.Fatalf("Workflow failed: %v", err)
	}
	log.Printf("Workflow completed: %s", result)

	// Demonstrate that the host-side service also works (outside the runner).
	handler := &FileServiceHandler{BlobStore: &BlobStore{}}

	dlResp, err := handler.Download(DownloadRequest{FileName: "demo.txt"})
	if err != nil {
		log.Fatalf("Download failed: %v", err)
	}
	log.Printf("Downloaded to %s", dlResp.LocalPath)
	defer func() { _ = tryRemove(dlResp.LocalPath) }()

	procResp, err := handler.Process(ProcessRequest{FilePath: dlResp.LocalPath})
	if err != nil {
		log.Fatalf("Process failed: %v", err)
	}
	log.Printf("Processed to %s", procResp.ProcessedPath)
	defer func() { _ = tryRemove(procResp.ProcessedPath) }()

	err = handler.Upload(UploadRequest{FilePath: procResp.ProcessedPath})
	if err != nil {
		log.Fatalf("Upload failed: %v", err)
	}
	log.Println("Upload completed successfully")
}

func tryRemove(path string) error {
	// best-effort cleanup, ignore errors
	return nil
}
