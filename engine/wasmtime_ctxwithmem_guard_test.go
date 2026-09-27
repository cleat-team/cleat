package engine

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestWasmtimeWrappersPassGuestMemory is a drift guard, not a behaviour test.
//
// Under wasmtime the api.Module handed to a host handler is always nil, so
// writeResult can only reach guest memory via the raw buffer that ctxWithMem
// stores in the context. A wrapper that takes an out-pointer but calls its
// handler with a bare context.Background() writes zero bytes and still returns
// errCode 0 -- a silent, total failure that looks like success.
//
// This defect has now been found three times in the same shape: §2.14 fixed
// cleat_json_parse and cleat_json_stringify, then §2.18 found six more, then
// auditing every wrapper for this test found the rest. Each round fixed the
// instances it knew about and left the siblings, because nothing checked the
// class.
//
// Runtime tests cannot close this cheaply -- most of these handlers need a
// live engine, store and session to reach their write path, which is exactly
// why the existing closure tests settle for mockHostHandler and an assertion
// that cannot fail (§2.16). A source-level invariant costs nothing and covers
// every wrapper, including ones added later.
//
// The rule: if a wrapper's signature has an out-length parameter, it must
// either pass ctxWithMem to its handler, or write into the buffer itself.
func TestWasmtimeWrappersPassGuestMemory(t *testing.T) {
	files, err := filepath.Glob("wasmtime_hostfuncs*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no wasmtime_hostfuncs*.go files found -- has the layout changed?")
	}

	// b.hostFunc, not linker.FuncWrap: IMPROVEMENT-PLAN 3.90 routed every host
	// function through the backend so the guest's epoch budget can be bracketed
	// around it (engine/wasmtime_hostbudget.go). This guard's `checked == 0`
	// check is what caught the rename -- it is the reason a source-level
	// invariant is allowed to depend on a syntax, and worth keeping that way.
	funcWrap := regexp.MustCompile(`b\.hostFunc\(linker, "env", "(\w+)"`)
	outParam := regexp.MustCompile(`\w*(MaxLen|maxLen)`)

	var checked int
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		s := string(src)

		for _, m := range funcWrap.FindAllStringSubmatchIndex(s, -1) {
			name := s[m[2]:m[3]]
			start := m[0]
			end := strings.Index(s[start:], "\n\t})")
			if end < 0 {
				t.Errorf("%s: %s: could not find end of wrapper body", f, name)
				continue
			}
			body := s[start : start+end]

			sig := body
			if i := strings.Index(body, ") int64"); i >= 0 {
				sig = body[:i]
			}
			if !outParam.MatchString(sig) {
				continue // no out-pointer, nothing to write
			}
			checked++

			// Either route the guest memory through the context, or write
			// into the buffer directly (cleat_poll_work does the latter).
			if strings.Contains(body, "ctxWithMem") || strings.Contains(body, "copy(buf[") {
				continue
			}
			t.Errorf("%s: %s takes an out-pointer but never passes the guest memory "+
				"buffer to its handler via ctxWithMem.\n"+
				"Under wasmtime the module is nil, so writeResult has nowhere to write: "+
				"this reports success and writes zero bytes.\n"+
				"Fix: capture buf from callerMemBuf and call the handler with "+
				"ctxWithMem(context.Background(), buf).", f, name)
		}
	}

	if checked == 0 {
		t.Fatal("guard matched no wrappers with out-parameters -- the regex has " +
			"drifted from the code and this test is no longer checking anything")
	}
	t.Logf("checked %d wasmtime wrappers with out-parameters across %d files", checked, len(files))
}
