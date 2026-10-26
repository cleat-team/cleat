// Package main is the fixture for cleat#3408: a Go guest that fetches a URL
// through the SDK's own DurableFetch and returns what the SDK DECODED.
//
// It exists because the SDK's tests stubbed the host's answer with a key the
// real worker never sends (cleat/runtime_behavioral_test.go answered
// {"status_code":200,...}), so DurableFetch could return status 0 for every
// response on a real worker while every test passed. This runs the SDK's
// decoder against the worker's real handler.
package main

import (
	"encoding/json"
	"fmt"

	"github.com/cleat-team/cleat/cleat"
)

// FetchStatus does one GET and reports the status and body the SDK handed back.
// A lone string parameter receives the whole input JSON.
func FetchStatus(h cleat.HostCalls, input string) (string, error) {
	var in struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal([]byte(input), &in); err != nil {
		return "", fmt.Errorf("decode input: %w", err)
	}
	body, status, err := h.DurableFetch(in.URL, "GET", nil, "")
	if err != nil {
		return "", err
	}
	out, _ := json.Marshal(map[string]any{"status": status, "body": body})
	return string(out), nil
}
