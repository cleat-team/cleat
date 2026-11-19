//go:build cgo

package csharp

import (
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"
	"testing"
)

// Group I of cleat#4080's host-call coverage: large payloads and the memory limit (packages/cleat-csharp/conformance/GroupI.cs). It has no WIT
// function of its own. These are the sizes at which the boundary between the guest and the host can silently lose or refuse data, measured through
// the real engine and wasmtime backend. Nothing here claims C# is supported.

// hostBuffer is the size of the buffer the component bridge gives a host call's response (component_callbacks.go: make([]byte, 65536)).
const hostBuffer = 65536

func fnvOf(s string) string {
	h := fnv.New64a()
	h.Write([]byte(s))
	return strconv.FormatUint(h.Sum64(), 10)
}

func bigResponse(n int) string { return `"` + strings.Repeat("y", n-2) + `"` }

func callInput(requestChars int) string {
	return fmt.Sprintf(`{"service":"svc","operation":"big","requestChars":%d}`, requestChars)
}

// A response up to the buffer arrives whole. The hash is of the full text, so a truncated or altered response cannot pass by having the right length.
func TestCSharpGroupICallResponsesUpToTheHostBufferArriveWhole(t *testing.T) {
	for _, n := range []int{2, 1000, 60000, hostBuffer - 1, hostBuffer} {
		resp := bigResponse(n)
		m := objectResult(t, runConformanceEntry(t, "i_call", entryRun{Input: callInput(1000), Responses: map[string]string{"svc/big": resp}}))
		if m["failed"] == true {
			t.Errorf("a %d-char response was refused: %v", n, m)
			continue
		}
		if int(m["responseChars"].(float64)) != n || m["responseHash"] != fnvOf(resp) {
			t.Errorf("a %d-char response arrived as %v chars with hash %v, want the whole text (hash %s)", n, m["responseChars"], m["responseHash"], fnvOf(resp))
		}
	}
}

// One character past the buffer is a FAILURE, not a silent truncation: code 7 (the host's OutputTruncated), with the prefix that landed as the
// message. The call itself ran once and recorded its effect, so a caller that retries on this would repeat it; the engine classifies it non-retryable
// but the C# SDK, like the Python one, leaves codes 6-8 on the base exception class, which is read here rather than assumed.
func TestCSharpGroupICallAResponseOverTheHostBufferIsRefusedWithItsPrefixNotTruncatedSilently(t *testing.T) {
	for _, n := range []int{hostBuffer + 1, 100000, 1000000} {
		res := runConformanceEntry(t, "i_call", entryRun{Input: callInput(10), Responses: map[string]string{"svc/big": bigResponse(n)}})
		m := objectResult(t, res)
		if m["failed"] != true {
			t.Errorf("a %d-char response was reported as a success (%v): the guest was handed a truncated answer as if it were whole", n, m)
			continue
		}
		if m["code"] != float64(7) || m["type"] != "CleatCallException" {
			t.Errorf("a %d-char response: code %v type %v, want code 7 on the base exception class", n, m["code"], m["type"])
		}
		if m["messageChars"] != float64(hostBuffer) {
			t.Errorf("a %d-char response: the failure message is %v chars, want the %d-char prefix that landed", n, m["messageChars"], hostBuffer)
		}
		if len(res.Calls) != 1 {
			t.Errorf("a %d-char response: the service was asked %d time(s), want once (the effect happened; only the write-back was refused)", n, len(res.Calls))
		}
	}
}

// Requests are not bounded by the 64 KiB host buffer (that bounds RESPONSES): a request up to 1 MiB, the default MaxWasmStringLen, reaches the
// service intact. Nothing above 1 MiB is asserted here: the component path currently applies no bound at all (cleat#4130), which is being changed to
// the core-module path's, so the over-limit case belongs to that change.
func TestCSharpGroupICallRequestsLargerThanTheHostBufferReachTheServiceIntact(t *testing.T) {
	for _, n := range []int{hostBuffer, hostBuffer + 1, 200000, 1 << 20} {
		res := runConformanceEntry(t, "i_call", entryRun{Input: callInput(n)})
		m := objectResult(t, res)
		if m["failed"] == true {
			t.Errorf("a %d-char request was refused: %v", n, m)
			continue
		}
		if len(res.Calls) != 1 || len(res.Calls[0].Request) != n {
			t.Errorf("a %d-char request: the service saw %d call(s), first request %d chars, want one call with the request whole", n, len(res.Calls), func() int {
				if len(res.Calls) == 0 {
					return -1
				}
				return len(res.Calls[0].Request)
			}())
		}
	}
}

// The memory limit (the engine's wasmtime default, 32 MiB of linear memory) ends a runaway allocation as a FAILED RUN naming the guest's
// OutOfMemoryException, not a host crash or a hang. The exact edge depends on the guest's garbage collector, so only the two ends are pinned:
// 1 MiB works and 64 MiB does not.
func TestCSharpGroupIMemoryLimit(t *testing.T) {
	ok := objectResult(t, runConformanceEntry(t, "i_alloc", entryRun{Input: `{"mebibytes":1}`}))
	if ok["allocatedMebibytes"] != float64(1) {
		t.Errorf("a 1 MiB allocation = %v, want it to succeed", ok)
	}
	res := runConformanceEntry(t, "i_alloc", entryRun{Input: `{"mebibytes":64}`})
	if res.Err == nil {
		t.Fatalf("a 64 MiB allocation succeeded (%q): the memory limit did not apply", res.Result)
	}
	if !strings.Contains(res.Err.Error(), "OutOfMemoryException") || !strings.Contains(res.Err.Error(), `"i_alloc"`) {
		t.Errorf("a 64 MiB allocation failed as %v, want the guest's OutOfMemoryException naming the entry", res.Err)
	}
}
