package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tetratelabs/wazero/api"
)

// newTruncTestMemory returns an api.Memory backed by a fixed byte slice.
//
// wazero's api.Memory is a wide interface and this test needs one method, so
// the rest are inherited from the zero value of an embedded interface: calling
// any of them panics, which is the right outcome for a fake that has quietly
// grown a second responsibility.
func newTruncTestMemory(n int) api.Memory {
	return &truncTestMemory{buf: make([]byte, n)}
}

type truncTestMemory struct {
	api.Memory
	buf []byte
}

func (m *truncTestMemory) Write(offset uint32, v []byte) bool {
	if int(offset)+len(v) > len(m.buf) {
		return false
	}
	copy(m.buf[offset:], v)
	return true
}

// TestATruncatedOutputIsReportedRatherThanSilentlyCut is cleat#1312.
//
// writeResult cut a value to the guest's buffer and returned only how many
// bytes it had written. The guest received a short value and errCode 0 and had
// nothing to compare against — a truncated response and a genuinely short one
// were the same observation.
//
// THE ASSERTION IS THE errCode, NOT THE BYTE COUNT. A test that checked only
// "fewer bytes came back" passes against the old behaviour unchanged, because
// the old behaviour also returned fewer bytes. What is new is that the call now
// says it failed.
func TestATruncatedOutputIsReportedRatherThanSilentlyCut(t *testing.T) {
	const capacity = 16
	buf := make([]byte, 1024)
	ctx := ctxWithMem(context.Background(), buf)
	s := &execSession{engine: NewEngine(nil, &mockCaller{})}

	t.Run("a value that fits is written whole and reports no error", func(t *testing.T) {
		// The control, and it runs first. Every assertion below is satisfied by
		// a writeOut that reports truncation for everything, which would fail
		// every host call rather than fix one.
		n, ec := s.writeOut(ctx, nil, 0, "short", capacity)
		if ec != 0 {
			t.Errorf("errCode = %d for a value that fits, want 0", ec)
		}
		if n != 5 {
			t.Errorf("wrote %d bytes, want 5", n)
		}
		if got := string(buf[:n]); got != "short" {
			t.Errorf("buffer holds %q, want %q", got, "short")
		}
	})

	t.Run("a value that does not fit reports errCodeOutputTruncated", func(t *testing.T) {
		long := strings.Repeat("x", capacity*4)
		n, ec := s.writeOut(ctx, nil, 0, long, capacity)
		if ec != errCodeOutputTruncated {
			t.Errorf("errCode = %d, want errCodeOutputTruncated (%d).\n\n"+
				"Without this the guest sees a short value and a success code, which is "+
				"indistinguishable from the service having returned a short value — the "+
				"defect cleat#1312 describes.", ec, errCodeOutputTruncated)
		}
		if n != capacity {
			t.Errorf("wrote %d bytes, want the buffer's capacity %d: the prefix is still "+
				"written so that adding the signal changes nothing for a caller that has "+
				"not been taught to read it", n, capacity)
		}
	})

	t.Run("the error carries both numbers", func(t *testing.T) {
		long := strings.Repeat("x", 100)
		_, err := s.writeResult(ctx, nil, 0, long, capacity)
		var trunc *OutputTruncatedError
		if !errors.As(err, &trunc) {
			t.Fatalf("writeResult returned %v, want an *OutputTruncatedError", err)
		}
		if trunc.Needed != 100 || trunc.Capacity != capacity {
			t.Errorf("OutputTruncatedError{Needed:%d, Capacity:%d}, want {100, %d}.\n\n"+
				"The difference is the actionable part: a guest that asked for 1 MiB and "+
				"needed 3 MiB has a payload problem; one that asked for 64 KB has a buffer "+
				"problem.", trunc.Needed, trunc.Capacity, capacity)
		}
	})
}

// TestTheWazeroWriterReportsTruncationToo pins the other writer.
//
// There are two, and they truncate identically: writeResult for the wasmtime
// raw-buffer path and writeWasmString for the wazero api.Memory path. Fixing
// one would leave the defect live on whichever backend the test did not use —
// and the engine suite runs mostly on one of them.
func TestTheWazeroWriterReportsTruncationToo(t *testing.T) {
	mem := newTruncTestMemory(64)
	n, err := writeWasmString(mem, 0, strings.Repeat("y", 50), 8)
	var trunc *OutputTruncatedError
	if !errors.As(err, &trunc) {
		t.Fatalf("writeWasmString returned %v, want an *OutputTruncatedError", err)
	}
	if n != 8 {
		t.Errorf("wrote %d bytes, want 8", n)
	}
	if trunc.Needed != 50 {
		t.Errorf("Needed = %d, want 50", trunc.Needed)
	}

	// Control: a value that fits reports nothing.
	if _, err := writeWasmString(mem, 0, "fits", 8); err != nil {
		t.Errorf("writeWasmString(%q, cap 8) = %v, want nil", "fits", err)
	}
}

// TestEveryHostCallThatCanSucceedPropagatesTruncation is the completeness half.
//
// The two tests above cover the writers. This covers the 40-odd call sites: a
// host call whose success path packs errCode 0 must take that code from
// writeOut rather than hardcoding a literal 0, or truncation is invisible again
// for that one call — which is exactly how this defect looked before, spread
// over every call at once.
func TestEveryHostCallThatCanSucceedPropagatesTruncation(t *testing.T) {
	var offenders []string
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 50 {
		t.Fatalf("found %d .go files in engine/, expected many more -- this scan is "+
			"matching almost nothing and would pass whatever the tree said", len(files))
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("reading %s: %v", f, err)
		}
		src := string(raw)
		lines := strings.Split(src, "\n")
		for i, line := range lines {
			if !strings.Contains(line, ", _ = s.writeResult(") &&
				!strings.Contains(line, ", _ := s.writeResult(") {
				continue
			}
			// Look ahead for a success pack: an errCode literal 0 paired with
			// this write's own variable.
			v := strings.TrimSpace(strings.SplitN(strings.TrimSpace(line), ",", 2)[0])
			for _, w := range lines[i+1 : min(i+5, len(lines))] {
				if !strings.Contains(w, "return pack") {
					continue
				}
				if strings.Contains(w, "(0, "+v+")") || strings.Contains(w, v+", 0)") {
					offenders = append(offenders,
						strings.TrimSpace(f)+": "+strings.TrimSpace(line)+"  ->  "+strings.TrimSpace(w))
				}
				break
			}
		}
	}
	if len(offenders) > 0 {
		t.Errorf("%d host call site(s) discard the write result and then report success:\n  %s\n\n"+
			"Use s.writeOut and pack the errCode it returns. A site that hardcodes 0 tells "+
			"the guest the call succeeded while handing it a prefix of the value.",
			len(offenders), strings.Join(offenders, "\n  "))
	}
}
