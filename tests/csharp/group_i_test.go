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

// hostBuffer was the fixed size of the buffer the component bridge gave a host call's response (65536) until cleat#4154: it is now sized to the
// response, up to responseCeiling, so a response that used to be refused arrives whole. hostBuffer is kept as the old edge (65536) because the
// sizes around it are where the old defect lived.
const hostBuffer = 65536

// responseCeiling is the largest response a component guest can receive (wasm/adapter_component.go, _cleatOutBufCeiling): 2 MiB. One character
// past it is refused with code 7 and the prefix that landed.
const responseCeiling = 2 << 20

func fnvOf(s string) string {
	h := fnv.New64a()
	h.Write([]byte(s))
	return strconv.FormatUint(h.Sum64(), 10)
}

func bigResponse(n int) string { return `"` + strings.Repeat("y", n-2) + `"` }

func groupICallInput(requestChars int) string {
	return fmt.Sprintf(`{"service":"svc","operation":"big","requestChars":%d}`, requestChars)
}

// A response up to the ceiling arrives whole, including the sizes that were refused while the buffer was a fixed 64 KiB. The hash is of the full
// text, so a truncated or altered response cannot pass by having the right length.
func TestCSharpGroupICallResponsesUpToTheCeilingArriveWhole(t *testing.T) {
	for _, n := range []int{2, 1000, 60000, hostBuffer - 1, hostBuffer, hostBuffer + 1, 100000, 1000000, responseCeiling} {
		resp := bigResponse(n)
		m := objectResult(t, runConformanceEntry(t, "i_call", entryRun{Input: groupICallInput(1000), Responses: map[string]string{"svc/big": resp}}))
		if m["failed"] == true {
			t.Errorf("a %d-char response was refused: %v", n, m)
			continue
		}
		if int(m["responseChars"].(float64)) != n || m["responseHash"] != fnvOf(resp) {
			t.Errorf("a %d-char response arrived as %v chars with hash %v, want the whole text (hash %s)", n, m["responseChars"], m["responseHash"], fnvOf(resp))
		}
	}
}

// One character past the ceiling is a FAILURE, not a silent truncation: code 7 (the host's OutputTruncated), with the prefix that landed as the
// message. The call itself ran once and recorded its effect, so a caller that retries on this would repeat it; the engine classifies it non-retryable
// but the C# SDK, like the Python one, leaves codes 6-8 on the base exception class, which is read here rather than assumed.
func TestCSharpGroupICallAResponseOverTheCeilingIsRefusedWithItsPrefixNotTruncatedSilently(t *testing.T) {
	for _, n := range []int{responseCeiling + 1} {
		res := runConformanceEntry(t, "i_call", entryRun{Input: groupICallInput(10), Responses: map[string]string{"svc/big": bigResponse(n)}})
		m := objectResult(t, res)
		if m["failed"] != true {
			t.Errorf("a %d-char response was reported as a success (%v): the guest was handed a truncated answer as if it were whole", n, m)
			continue
		}
		if m["code"] != float64(7) || m["type"] != "CleatCallException" {
			t.Errorf("a %d-char response: code %v type %v, want code 7 on the base exception class", n, m["code"], m["type"])
		}
		if m["messageChars"] != float64(responseCeiling) {
			t.Errorf("a %d-char response: the failure message is %v chars, want the %d-char prefix that landed", n, m["messageChars"], responseCeiling)
		}
		if len(res.Calls) != 1 {
			t.Errorf("a %d-char response: the service was asked %d time(s), want once (the effect happened; only the write-back was refused)", n, len(res.Calls))
		}
	}
}

// Requests are not bounded by the response ceiling (that bounds RESPONSES, and is 2 MiB since cleat#4154): a request up to 2 MiB, the default MaxWasmStringLen for every
// language since cleat#4130, reaches the service intact. The over-limit case is the next test.
func TestCSharpGroupICallRequestsLargerThanTheHostBufferReachTheServiceIntact(t *testing.T) {
	for _, n := range []int{hostBuffer, hostBuffer + 1, 200000, 1 << 20, 2 << 20} {
		res := runConformanceEntry(t, "i_call", entryRun{Input: groupICallInput(n)})
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

// One character over the limit is refused BEFORE the service is asked: the call fails visibly and the service saw nothing. The component path
// applied no bound at all until cleat#4130 (the guest could hand the host a request of any size); it now refuses at the same limit as a
// core-module guest.
func TestCSharpGroupICallRequestsOverTheLimitAreRefusedBeforeTheServiceIsAsked(t *testing.T) {
	res := runConformanceEntry(t, "i_call", entryRun{Input: groupICallInput(2<<20 + 1)})
	m := objectResult(t, res)
	if m["failed"] != true {
		t.Errorf("a request one character over 2 MiB was not refused: %v", m)
	}
	if len(res.Calls) != 0 {
		t.Errorf("the service was asked %d time(s) for a request over the limit, want 0", len(res.Calls))
	}
}

// The memory limit (the engine's wasmtime default, 256 MiB of linear memory since cleat#4171; it was 32 MiB, raised to 64 for C#) ends a runaway
// allocation as a FAILED RUN naming the guest's OutOfMemoryException, not a host crash or a hang. The exact edge depends on the guest's garbage
// collector, so only the two ends are pinned: 1 MiB works and 512 MiB (twice the limit) does not.
func TestCSharpGroupIMemoryLimit(t *testing.T) {
	ok := objectResult(t, runConformanceEntry(t, "i_alloc", entryRun{Input: `{"mebibytes":1}`}))
	if ok["allocatedMebibytes"] != float64(1) {
		t.Errorf("a 1 MiB allocation = %v, want it to succeed", ok)
	}
	res := runConformanceEntry(t, "i_alloc", entryRun{Input: `{"mebibytes":512}`})
	if res.Err == nil {
		t.Fatalf("a 512 MiB allocation succeeded (%q): the memory limit did not apply", res.Result)
	}
	if !strings.Contains(res.Err.Error(), "OutOfMemoryException") || !strings.Contains(res.Err.Error(), `"i_alloc"`) {
		t.Errorf("a 512 MiB allocation failed as %v, want the guest's OutOfMemoryException naming the entry", res.Err)
	}
}
