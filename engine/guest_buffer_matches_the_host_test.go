package engine

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/wasm"
)

// TestTheGuestOutputBufferMatchesWhatTheHostWillWrite is cleat#1312.
//
// The host writes up to DefaultOutBufSize (1 MiB) into whatever buffer the
// guest supplies, truncating silently to the guest's size — writeResult in
// engine/flush.go does `data = data[:maxLen]` and reports only how many bytes
// it wrote, never how many there were. The generated Go guest asked for 65536.
//
// So a durable-call response between 64 KB and 1 MB was cut at 64 KB, and the
// usual symptom is a JSON unmarshal error pointing at the response body rather
// than at a buffer limit — which sends the author to debug the service they
// called.
//
// THIS LIVES IN engine/ BECAUSE THE DEPENDENCY ONLY RUNS ONE WAY. engine
// imports wasm, so the generated constant cannot reference
// engine.DefaultOutBufSize and the two numbers are related by nothing but this
// assertion. A comment in either file would not go red.
func TestTheGuestOutputBufferMatchesWhatTheHostWillWrite(t *testing.T) {
	// Generate the adapter the way `cleat build` does, and read the constant
	// out of the emitted source rather than out of the generator's own
	// variables: what ships is the string, and an emitter that stops using its
	// constant would pass an assertion about the constant.
	src := string(wasm.GenerateHostAdapter("probe", &wasm.UsageInfo{}, "go"))

	// The CEILING is the number that has to match the host: it is the largest
	// buffer the guest will ever offer, and the host writes up to
	// DefaultOutBufSize. The floor is a starting point and is deliberately
	// smaller -- see the adapter's own comment for why allocating the ceiling
	// up front broke the defer pass of an OOM-killed workflow.
	got, ok := constFromSource(src, "_cleatOutBufCeiling")
	if !ok {
		t.Fatalf("no `const _cleatOutBufCeiling = <n>` in the generated adapter.\n\n"+
			"A lookup that finds nothing passes vacuously. Either the constant was "+
			"renamed — re-point this test — or the guest stopped declaring a buffer "+
			"size, in which case it is using some other number and nothing checks it.\n\n"+
			"generated source began:\n%s", head(src, 400))
	}
	if got != int64(DefaultOutBufSize) {
		t.Errorf("the generated guest will grow a durable-call response buffer to at most "+
			"%d bytes; the host writes up to DefaultOutBufSize = %d.\n\n"+
			"The smaller of the two wins and the difference used to be truncated in "+
			"silence. It is reported now, but a ceiling below the host's means a payload "+
			"the host would have delivered can never be received at any size.",
			got, DefaultOutBufSize)
	}

	// And the floor must be below the ceiling, or there is nothing to grow and
	// the adaptive design is a 1 MiB up-front allocation wearing its name.
	floor, ok := constFromSource(src, "_cleatOutBufFloor")
	if !ok {
		t.Fatal("no `const _cleatOutBufFloor = <n>` in the generated adapter")
	}
	if floor > got {
		t.Errorf("the buffer floor (%d) exceeds its ceiling (%d)", floor, got)
	}
	if floor == got {
		t.Errorf("floor == ceiling == %d: every call allocates the ceiling up front, which "+
			"is what TestTheHostRunsDefersOfAnOOMKilledWorkflow fails on -- a 1 MiB make "+
			"in a guest that has just exhausted its heap", floor)
	}
}

// TestTheGuestInputBufferMatchesWhatTheHostWillWrite is the other half, and it
// is the one the issue does not mention.
//
// cleat_poll_work hands the workflow its input through the same mechanism:
// `clampToMaxLen(len(b.workInput), argsMaxLen)` in the host function, and the
// guest supplies argsMaxLen. At 65536 a workflow started with more than 64 KB
// of input silently receives a truncated one -- its own arguments, not a
// service's response.
//
// THE INPUT BUFFER IS DELIBERATELY *NOT* RAISED TO THE HOST'S CEILING, WHICH
// IS THE OPPOSITE OF WHAT THE OUTPUT BUFFER DOES, AND THE ASYMMETRY IS THE
// POINT.
//
// The output buffer can start at 64 KiB and GROW, because the host reports
// truncation (errCode 7) and the guest can react. The input buffer cannot:
// clampToMaxLen truncates silently and returns the CLAMPED length, so a guest
// has no way to tell a 64 KB input from a 64 KB slice of a larger one. Sizing
// it adaptively is not available without an ABI change to cleat_poll_work.
//
// That leaves raising it outright, and it was raised to 1 MiB and reverted,
// because it breaks a guarantee this repo has a test for.
// TestTheHostRunsDefersOfAnOOMKilledWorkflow fails with a 1 MiB input buffer
// and passes at 64 KiB. Isolated by reverting one file at a time against an
// otherwise identical tree, and all three of these were measured, not reasoned
// about:
//
//	var argsBuf [1048576]byte, package-level   FAIL
//	argsBuf := make([]byte, 1048576) in main   FAIL
//	  ... plus copy-out, drop, and runtime.GC  FAIL
//	var argsBuf [65536]byte, local             ok
//
// The heap forms fail for the reason the static one does: wasmtime's limit is
// on the guest's LINEAR MEMORY, which only ever grows. A guest that has once
// taken a megabyte never gives it back to the host, so the cost is permanent
// however the megabyte was obtained -- and the defer pass of a workflow killed
// for exhausting its memory is exactly the case with no slack to spend.
//
// So the silent-truncation defect on the input side is real and is NOT fixed
// here. It needs the host to refuse an oversized input loudly rather than
// clamp it, which is a change to the host function and not to this constant.
// Recorded as a test rather than a comment in a plan file so that the next
// person to raise this number finds out why within one test run.
func TestTheGuestInputBufferMatchesWhatTheHostWillWrite(t *testing.T) {
	stub := wasm.MainStubSource()

	const wantInput = 65536

	if !strings.Contains(stub, fmt.Sprintf("const argsBufSize = %d", wantInput)) {
		t.Errorf("the generated entry stub does not declare argsBufSize = %d.\n\n"+
			"Raising it is not a free improvement: see this test's doc comment, and "+
			"run TestTheHostRunsDefersOfAnOOMKilledWorkflow before changing it.\n\n"+
			"stub began:\n%s", wantInput, head(stub, 400))
	}

	// The size is used in TWO places -- the array declaration and the maxLen
	// argument -- and a mismatch either wastes the tail of the buffer or asks
	// the host to write past its end. Both must be the named constant, so that
	// there is one number rather than two that happen to agree.
	for _, form := range []string{
		"var argsBuf [argsBufSize]byte",
		"unsafe.Pointer(&argsBuf[0]), argsBufSize,",
	} {
		if !strings.Contains(stub, form) {
			t.Errorf("the stub does not contain %q.\n\nThe declared size and the "+
				"advertised size must both come from argsBufSize, not from a literal "+
				"that happens to match it.", form)
		}
	}

	// And the constant must still be below the host's ceiling, because a guest
	// that advertised MORE than the host writes would read uninitialised tail.
	if wantInput > DefaultOutBufSize {
		t.Errorf("argsBufSize %d exceeds DefaultOutBufSize %d", wantInput, DefaultOutBufSize)
	}
}

func constFromSource(src, name string) (int64, bool) {
	for _, line := range strings.Split(src, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "const "+name+" = ") {
			continue
		}
		v, err := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(line, "const "+name+" = ")), 10, 64)
		if err != nil {
			return 0, false
		}
		return v, true
	}
	return 0, false
}

func arrayLenFromSource(src, name string) (int64, bool) {
	for _, line := range strings.Split(src, "\n") {
		line = strings.TrimSpace(line)
		prefix := "var " + name + " ["
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		rest := strings.TrimPrefix(line, prefix)
		idx := strings.Index(rest, "]")
		if idx < 0 {
			return 0, false
		}
		v, err := strconv.ParseInt(rest[:idx], 10, 64)
		if err != nil {
			return 0, false
		}
		return v, true
	}
	return 0, false
}

func head(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// TestEveryAdapterWithAnOutputBufferGrowsIt is the completeness half of the
// adaptive-buffer design (cleat#1312).
//
// The buffer starts at 64 KiB and doubles to the host's ceiling when the host
// reports errCode 7, rather than being allocated at 1 MiB up front — that was
// measured to break the defer pass of an OOM-killed workflow, because a 1 MiB
// `make` needs heap the guest has just exhausted.
//
// The growth hook therefore has to reach EVERY adapter that allocates a buffer.
// The obvious place to put it — withSuspendCheck — is the wrong population:
// that is "calls the host can refuse mid-segment", and WorkflowID and RunID
// write a value without being in it. An adapter that allocates and never grows
// is stuck at 64 KiB forever, silently.
func TestEveryAdapterWithAnOutputBufferGrowsIt(t *testing.T) {
	src := string(wasm.GenerateHostAdapter("probe", wasm.AllUsage(), "go"))

	// Split into per-field closures: "\n\t\tFieldName: func".
	blocks := regexp.MustCompile(`\n\t\t(\w+): func`).Split(src, -1)
	names := regexp.MustCompile(`\n\t\t(\w+): func`).FindAllStringSubmatch(src, -1)
	if len(names) < 20 {
		t.Fatalf("found %d adapter closures in the generated source, expected many more -- "+
			"this scan is matching almost nothing and would pass whatever was emitted", len(names))
	}

	var offenders []string
	withBuffer := 0
	for i, m := range names {
		body := blocks[i+1]
		if !strings.Contains(body, "make([]byte, _cleatOutBufCap)") {
			continue
		}
		withBuffer++
		if !strings.Contains(body, "_cleatGrowOutBuf()") {
			offenders = append(offenders, m[1])
		}
	}
	if withBuffer == 0 {
		t.Fatal("no adapter allocates an output buffer, so this test is checking nothing")
	}
	if len(offenders) > 0 {
		t.Errorf("%d of %d adapters allocate an output buffer and never grow it: %s\n\n"+
			"They are pinned at the 64 KiB floor: the host reports errCode 7, nothing acts on "+
			"it, and every later call truncates at the same size.",
			len(offenders), withBuffer, strings.Join(offenders, ", "))
	}
	t.Logf("%d adapters allocate an output buffer; all of them grow it", withBuffer)
}
