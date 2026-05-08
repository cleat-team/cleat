package blobstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// ---------------------------------------------------------------------------
// S3 backend tests using a mock HTTP transport so no real S3 server is needed.
// ---------------------------------------------------------------------------

// s3MockTransport implements http.RoundTripper by storing and retrieving
// objects in an in-memory map. It intercepts all HTTP requests made by the
// minio.Client and returns appropriate S3-like responses.
type s3MockTransport struct {
	mu   sync.Mutex
	data map[string][]byte // object key -> raw bytes
}

func newS3MockTransport() *s3MockTransport {
	return &s3MockTransport{data: make(map[string][]byte)}
}

func (t *s3MockTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	// Extract the object key from a path-style URL: /bucket/key
	path := strings.TrimPrefix(req.URL.Path, "/")
	parts := strings.SplitN(path, "/", 2)
	if len(parts) < 2 || parts[1] == "" {
		return nil, fmt.Errorf("s3mock: invalid path: %s", req.URL.Path)
	}
	key := parts[1]

	switch req.Method {
	case http.MethodPut:
		bodyData, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		req.Body.Close()
		t.data[key] = bodyData
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"ETag": {`"dummy-etag"`}},
			Body:       io.NopCloser(bytes.NewReader(nil)),
		}, nil

	case http.MethodGet:
		data, ok := t.data[key]
		if !ok {
			return &http.Response{
				StatusCode: http.StatusNotFound,
				Body:       io.NopCloser(bytes.NewReader(nil)),
			}, nil
		}
		hdr := make(http.Header)
		hdr.Set("Content-Length", strconv.Itoa(len(data)))
		hdr.Set("Last-Modified", time.Now().UTC().Format("Mon, 2 Jan 2006 15:04:05 GMT"))
		hdr.Set("Content-Type", "application/octet-stream")
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     hdr,
			Body:       io.NopCloser(bytes.NewReader(data)),
		}, nil

	case http.MethodDelete:
		delete(t.data, key)
		return &http.Response{
			StatusCode: http.StatusNoContent,
			Body:       io.NopCloser(bytes.NewReader(nil)),
		}, nil
	}

	return nil, fmt.Errorf("s3mock: unexpected method: %s", req.Method)
}

// newS3BackendForTest creates an s3Backend wired to a mock HTTP transport.
// Returns the backend and the transport so tests can inspect stored data.
func newS3BackendForTest(t *testing.T) (*s3Backend, *s3MockTransport) {
	t.Helper()

	transport := newS3MockTransport()
	// Use Secure: true so the client avoids chunked streaming signatures.
	// The custom Transport bypasses TLS entirely, so no real TLS is needed.
	client, err := minio.New("localhost:0", &minio.Options{
		Creds:     credentials.NewStaticV4("test-key", "test-secret", ""),
		Region:    "us-east-1",
		Secure:    true,
		Transport: transport,
	})
	if err != nil {
		t.Fatalf("minio.New: %v", err)
	}

	return &s3Backend{
		client: client,
		bucket: "test-bucket",
	}, transport
}

func TestS3BackendPutAndGet(t *testing.T) {
	b, _ := newS3BackendForTest(t)
	ctx := context.Background()

	data := []byte("hello s3 world")
	sha256Hex := fmt.Sprintf("%x", sha256.Sum256(data))

	// Put
	if err := b.Put(ctx, sha256Hex, data, "text/plain"); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Get
	got, err := b.Get(ctx, sha256Hex)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("Get: expected %q, got %q", data, got)
	}
}

func TestS3BackendPutWithContentType(t *testing.T) {
	b, _ := newS3BackendForTest(t)
	ctx := context.Background()

	data := []byte(`{"key": "value"}`)
	sha256Hex := fmt.Sprintf("%x", sha256.Sum256(data))

	// Put with explicit content type
	if err := b.Put(ctx, sha256Hex, data, "application/json"); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Get should return the same data
	got, err := b.Get(ctx, sha256Hex)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("Get: expected %q, got %q", data, got)
	}
}

func TestS3BackendDelete(t *testing.T) {
	b, transport := newS3BackendForTest(t)
	ctx := context.Background()

	data := []byte("to delete")
	sha256Hex := fmt.Sprintf("%x", sha256.Sum256(data))

	// Put first
	if err := b.Put(ctx, sha256Hex, data, ""); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Verify stored in mock
	if _, ok := transport.data[sha256Hex]; !ok {
		t.Fatal("expected data in mock transport before delete")
	}

	// Delete
	if err := b.Delete(ctx, sha256Hex); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// Verify removed from mock
	if _, ok := transport.data[sha256Hex]; ok {
		t.Error("data should be removed from mock transport after delete")
	}

	// Get after delete should fail
	_, err := b.Get(ctx, sha256Hex)
	if err == nil {
		t.Error("expected error for Get after Delete")
	}
}

func TestS3BackendGetNonExistent(t *testing.T) {
	b, _ := newS3BackendForTest(t)
	ctx := context.Background()

	// Use a bogus SHA256 hex string
	_, err := b.Get(ctx, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if err == nil {
		t.Fatal("expected error for non-existent blob")
	}
}

func TestS3BackendPutAndGetLargeBlob(t *testing.T) {
	b, _ := newS3BackendForTest(t)
	ctx := context.Background()

	// 64KB blob
	size := 64 * 1024
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i % 251)
	}
	sha256Hex := fmt.Sprintf("%x", sha256.Sum256(data))

	if err := b.Put(ctx, sha256Hex, data, "application/octet-stream"); err != nil {
		t.Fatalf("Put large: %v", err)
	}

	got, err := b.Get(ctx, sha256Hex)
	if err != nil {
		t.Fatalf("Get large: %v", err)
	}
	if len(got) != size {
		t.Errorf("expected %d bytes, got %d", size, len(got))
	}
	if !bytes.Equal(got, data) {
		t.Error("large blob data mismatch")
	}
}

func TestS3BackendOverwrite(t *testing.T) {
	b, _ := newS3BackendForTest(t)
	ctx := context.Background()

	// Use a synthetic SHA256 key
	sha256Hex := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	// First put
	if err := b.Put(ctx, sha256Hex, []byte("original"), ""); err != nil {
		t.Fatalf("Put original: %v", err)
	}

	// Overwrite
	if err := b.Put(ctx, sha256Hex, []byte("overwritten"), ""); err != nil {
		t.Fatalf("Put overwrite: %v", err)
	}

	// Get should return the new data
	got, err := b.Get(ctx, sha256Hex)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != "overwritten" {
		t.Errorf("expected 'overwritten', got %q", string(got))
	}
}
