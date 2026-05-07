package main

import (
	"os"
	"strings"
)

// ---------------------------------------------------------------------------
// Host-side file processing services
//
// These functions run on the host (not in WASM) and have access to the local
// filesystem. In a real Cleat deployment they would be registered with the
// Cleat runtime as host-callable service handlers.
//
// In unit tests (see workflow_test.go) these are replaced by stubs registered
// on the TestEnv via OnCall.
// ---------------------------------------------------------------------------

// BlobStore simulates a remote blob / object store.
type BlobStore struct{}

// downloadFile simulates downloading content from a remote store.
func (b *BlobStore) downloadFile(fileID string) []byte {
	dummyContent := "dummy content for fileID:" + fileID
	return []byte(dummyContent)
}

// uploadFile simulates uploading a local file to a remote store.
func (b *BlobStore) uploadFile(filename string) error {
	_, err := os.ReadFile(filename)
	return err
}

// FileServiceHandler is the host-side handler for file processing operations.
// It mirrors the Temporal sample's Activities struct.
type FileServiceHandler struct {
	BlobStore *BlobStore
}

// Download stores the downloaded content in a temp file and returns the path.
func (h *FileServiceHandler) Download(req DownloadRequest) (DownloadResponse, error) {
	data := h.BlobStore.downloadFile(req.FileName)
	tmpFile, err := saveToTmpFile(data)
	if err != nil {
		return DownloadResponse{}, err
	}
	return DownloadResponse{LocalPath: tmpFile.Name()}, nil
}

// Process reads the file, transforms its content (uppercase), and writes the
// result to a new temp file. The original temp file is cleaned up.
func (h *FileServiceHandler) Process(req ProcessRequest) (ProcessResponse, error) {
	defer func() { _ = os.Remove(req.FilePath) }() // cleanup input temp file

	data, err := os.ReadFile(req.FilePath)
	if err != nil {
		return ProcessResponse{}, err
	}

	transData := transcodeData(data)
	tmpFile, err := saveToTmpFile(transData)
	if err != nil {
		return ProcessResponse{}, err
	}

	return ProcessResponse{ProcessedPath: tmpFile.Name()}, nil
}

// Upload reads the processed file (simulating a remote upload) and cleans up.
func (h *FileServiceHandler) Upload(req UploadRequest) error {
	defer func() { _ = os.Remove(req.FilePath) }() // cleanup processed temp file

	return h.BlobStore.uploadFile(req.FilePath)
}

// --- helpers (mirrored from the Temporal sample) ---

// transcodeData transforms file content to uppercase.
func transcodeData(data []byte) []byte {
	return []byte(strings.ToUpper(string(data)))
}

// saveToTmpFile writes data to a new temp file and returns the file handle.
func saveToTmpFile(data []byte) (f *os.File, err error) {
	tmpFile, err := os.CreateTemp("", "cleat_file_sample")
	if err != nil {
		return nil, err
	}
	_, err = tmpFile.Write(data)
	if err != nil {
		_ = os.Remove(tmpFile.Name())
		return nil, err
	}
	return tmpFile, nil
}
