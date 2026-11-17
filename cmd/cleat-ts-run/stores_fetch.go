// A scripted Fetcher for cleat-ts-run (cleat#3827, group J: cleat_fetch).
//
// WHY. `cleat_fetch` reaches `engine.Fetcher`, and an engine built without one refuses the call with "cleat_fetch is
// disabled: this engine has no Fetcher". That refusal is the DEFAULT on a cleat-worker (fetch is opt-in behind
// --enable-workflow-fetch), so a test that does not configure a Fetcher is testing the refusal whether it meant to or
// not -- and would look identical to a test of a fetch that returned nothing.
//
// WHAT IT IS NOT. It is not an egress policy. The real fetcher answers to a tenant's allowlist and refuses what the
// policy excludes; this answers from the test's script. Where that differs in a way a workflow could observe, it
// belongs in the PR body's divergence list, not silently here.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/cleat-team/cleat/engine"
)

// fetchScript is one scripted answer, keyed by "<METHOD> <url>".
type fetchScript struct {
	// Status and Body build a FetchResult-shaped answer. Headers is optional.
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body,omitempty"`
	// Error makes the fetch FAIL instead of answering -- the shape an egress refusal takes, which the engine and the
	// SDK both carry to the guest as an error rather than a status.
	Error string `json:"error,omitempty"`
}

// loadFetchScript reads {"<METHOD> <url>": {"status"|"body"|"error": ...}}.
//
// A key without a space is refused: "GET https://x" is the form, and a bare URL would script a fetch whose method the
// fixture never chose, which is exactly the mistake the key is meant to make visible.
func loadFetchScript(path string) (map[string]fetchScript, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading the fetch script: %w", err)
	}
	var out map[string]fetchScript
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("the fetch script is not a JSON object of \"METHOD url\" answers: %w", err)
	}
	for key, s := range out {
		if !strings.Contains(key, " ") {
			return nil, fmt.Errorf("fetch script key %q is not \"<METHOD> <url>\"", key)
		}
		if s.Error != "" && s.Status != 0 {
			return nil, fmt.Errorf("fetch script %q answers AND fails: status and error are exclusive", key)
		}
		if s.Error == "" && s.Status == 0 {
			return nil, fmt.Errorf("fetch script %q has neither a status nor an error", key)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("the fetch script is empty: a run with nothing scripted would exercise the refusal, not a fetch")
	}
	return out, nil
}

// scriptedFetcher answers from the fixture and records every request it saw.
type scriptedFetcher struct {
	script map[string]fetchScript

	mu   sync.Mutex
	seen []string
}

func (f *scriptedFetcher) Fetch(_ context.Context, method, url, headersJSON, body string) (string, error) {
	key := method + " " + url
	f.mu.Lock()
	f.seen = append(f.seen, key)
	f.mu.Unlock()

	s, ok := f.script[key]
	if !ok {
		return "", fmt.Errorf("no fetch script for %q: the test scripted %s",
			key, strings.Join(f.scriptedKeys(), ", "))
	}
	if s.Error != "" {
		return "", fmt.Errorf("%s", s.Error)
	}
	headers := s.Headers
	if headers == nil {
		headers = map[string]string{}
	}
	// The guest decodes {status, headers, body}: the SDK wrapper turns this text into a FetchResult, so an answer
	// shaped any other way would fail inside the guest rather than here.
	out, err := json.Marshal(map[string]any{
		"status":  s.Status,
		"headers": headers,
		"body":    s.Body,
	})
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func (f *scriptedFetcher) scriptedKeys() []string {
	keys := make([]string, 0, len(f.script))
	for k := range f.script {
		keys = append(keys, k)
	}
	// Sorted: map order is random, and this list is the body of the error a test author reads, so it must read the same every run.
	sort.Strings(keys)
	return keys
}

func (f *scriptedFetcher) requestList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.seen...)
}

// newScriptedFetch is the registry factory for `-script fetch=file`.
func newScriptedFetch(path string) (storeSpec, error) {
	script, err := loadFetchScript(path)
	if err != nil {
		return storeSpec{}, err
	}
	f := &scriptedFetcher{script: script}
	return storeSpec{
		Option: engine.WithFetcher(f),
		Extra:  func() (string, any) { return "fetchRequests", f.requestList() },
	}, nil
}

func init() { registerStore("fetch", newScriptedFetch) }
