// Package wasm — measured per-package size attribution for a compiled binary.
//
// This exists because `cleat build --size-report` did not read the binary. It
// multiplied the artifact's total length by a table of hardcoded constants --
// `{"reflect", int64(float64(totalSize) * 0.25)}` and twenty more like it --
// and printed the products as a per-package breakdown. The only input from the
// artifact was its length, so every binary ever built got the same shape of
// answer, and the constants could sum past 100% of the file (a workflow
// importing reflect, encoding/json, fmt, net/http, crypto/tls, time, os and
// strings reached 108%, with the "other" remainder line silently suppressed
// because it had gone negative). cleat#1314.
//
// The information is in the artifact. Go's wasip1 builds keep the custom
// `name` section, whose function-name subsection maps function indices to
// fully-qualified Go symbols; the code section carries each function's body
// size. Joining them attributes real bytes to real packages.
//
// `go tool nm` cannot do this -- it reports `unrecognized object file` on a
// wasip1 artifact -- which is why this is a parser rather than a shell-out.
package wasm

import (
	"fmt"
	"sort"
	"strings"
)

// PackageSize is the measured code-section bytes attributed to one package.
type PackageSize struct {
	Package string
	Size    int64
	Funcs   int
}

// SectionSize is one WASM section's size on disk.
type SectionSize struct {
	ID   byte
	Name string // custom sections only
	Size int64
}

// SizeBreakdown is what a size report can say about a binary after reading it.
//
// HaveNames is the field that keeps this honest. A binary built without the
// name section -- stripped, or produced by another toolchain -- yields no
// package attribution at all, and the report must say so rather than falling
// back to a model. Presenting modelled numbers in the one case nobody tests is
// how the defect this replaces would come back.
type SizeBreakdown struct {
	TotalSize    int64
	CodeSize     int64
	Sections     []SectionSize
	Packages     []PackageSize
	Attributed   int64
	Unattributed int64
	HaveNames    bool
}

// AnalyzeSize reads a WASM binary and attributes its code section to packages.
func AnalyzeSize(wasmBytes []byte) (*SizeBreakdown, error) {
	if !hasWasmHeader(wasmBytes) {
		return nil, fmt.Errorf("not a valid WASM binary (bad magic/version)")
	}
	br := &SizeBreakdown{TotalSize: int64(len(wasmBytes))}

	var codeSection []byte
	var nameSection []byte
	importedFuncs := 0

	offset := 8
	for offset < len(wasmBytes) {
		id := wasmBytes[offset]
		offset++
		size, n := decodeULEB128(wasmBytes[offset:])
		if n <= 0 {
			return nil, fmt.Errorf("corrupt WASM: bad section size at offset %d", offset)
		}
		offset += n
		if int(size) > len(wasmBytes)-offset {
			return nil, fmt.Errorf("corrupt WASM at offset %d: section size %d overflows binary", offset, size)
		}
		body := wasmBytes[offset : offset+int(size)]
		sec := SectionSize{ID: id, Size: int64(size)}
		if id == 0 {
			if nm, n := readName(body); n > 0 {
				sec.Name = nm
				if nm == "name" {
					nameSection = body[n:]
				}
			}
		}
		br.Sections = append(br.Sections, sec)
		switch id {
		case 2: // import
			importedFuncs = countImportedFunctions(body)
		case 10: // code
			codeSection = body
			br.CodeSize = int64(size)
		}
		offset += int(size)
	}

	sizes := functionBodySizes(codeSection)
	names := functionNames(nameSection)
	br.HaveNames = len(names) > 0

	byPkg := map[string]*PackageSize{}
	for i, sz := range sizes {
		// Code-section entry i is function index importedFuncs+i: the index
		// space the name map uses counts imports first. Getting this wrong
		// shifts every attribution by the import count rather than failing,
		// which is why TestFunctionIndicesAccountForImports pins it.
		name, ok := names[uint32(importedFuncs+i)]
		if !ok {
			br.Unattributed += sz
			continue
		}
		p := packageOf(name)
		if p == "" {
			br.Unattributed += sz
			continue
		}
		e := byPkg[p]
		if e == nil {
			e = &PackageSize{Package: p}
			byPkg[p] = e
		}
		e.Size += sz
		e.Funcs++
		br.Attributed += sz
	}
	for _, e := range byPkg {
		br.Packages = append(br.Packages, *e)
	}
	sort.Slice(br.Packages, func(i, j int) bool {
		if br.Packages[i].Size != br.Packages[j].Size {
			return br.Packages[i].Size > br.Packages[j].Size
		}
		return br.Packages[i].Package < br.Packages[j].Package
	})
	return br, nil
}

// functionBodySizes returns the on-disk size of each function body in the code
// section, in code-section order. The size includes the body's own length
// prefix, so the values sum to the section's payload rather than to something
// slightly smaller -- a breakdown that does not add up to the thing it breaks
// down invites exactly the doubt this report exists to remove.
func functionBodySizes(code []byte) []int64 {
	if len(code) == 0 {
		return nil
	}
	count, n := decodeULEB128(code)
	if n <= 0 {
		return nil
	}
	off := n
	out := make([]int64, 0, count)
	for i := uint32(0); i < count && off < len(code); i++ {
		sz, n := decodeULEB128(code[off:])
		if n <= 0 {
			break
		}
		if int(sz) > len(code)-off-n {
			break
		}
		out = append(out, int64(sz)+int64(n))
		off += n + int(sz)
	}
	return out
}

// functionNames parses the function-name subsection (id 1) of the `name`
// custom section into index -> symbol.
func functionNames(name []byte) map[uint32]string {
	out := map[uint32]string{}
	off := 0
	for off < len(name) {
		subID := name[off]
		off++
		sz, n := decodeULEB128(name[off:])
		if n <= 0 || int(sz) > len(name)-off-n {
			return out
		}
		off += n
		body := name[off : off+int(sz)]
		off += int(sz)
		if subID != 1 {
			continue
		}
		count, n := decodeULEB128(body)
		if n <= 0 {
			return out
		}
		b := body[n:]
		for i := uint32(0); i < count; i++ {
			idx, n1 := decodeULEB128(b)
			if n1 <= 0 {
				return out
			}
			b = b[n1:]
			nm, n2 := readName(b)
			if n2 <= 0 {
				return out
			}
			out[idx] = nm
			b = b[n2:]
		}
	}
	return out
}

// countImportedFunctions returns how many of the import section's entries are
// functions. Only those occupy the function index space.
func countImportedFunctions(imports []byte) int {
	count, n := decodeULEB128(imports)
	if n <= 0 {
		return 0
	}
	b := imports[n:]
	funcs := 0
	for i := uint32(0); i < count; i++ {
		_, n1 := readName(b) // module
		if n1 <= 0 {
			return funcs
		}
		b = b[n1:]
		_, n2 := readName(b) // field
		if n2 <= 0 {
			return funcs
		}
		b = b[n2:]
		if len(b) == 0 {
			return funcs
		}
		kind := b[0]
		b = b[1:]
		switch kind {
		case 0x00: // function: typeidx
			funcs++
			_, n := decodeULEB128(b)
			if n <= 0 {
				return funcs
			}
			b = b[n:]
		case 0x01: // table: reftype + limits
			if len(b) == 0 {
				return funcs
			}
			b = b[1:]
			var ok bool
			if b, ok = skipLimits(b); !ok {
				return funcs
			}
		case 0x02: // memory: limits
			var ok bool
			if b, ok = skipLimits(b); !ok {
				return funcs
			}
		case 0x03: // global: valtype + mutability
			if len(b) < 2 {
				return funcs
			}
			b = b[2:]
		default:
			return funcs
		}
	}
	return funcs
}

func skipLimits(b []byte) ([]byte, bool) {
	if len(b) == 0 {
		return b, false
	}
	flags := b[0]
	b = b[1:]
	_, n := decodeULEB128(b)
	if n <= 0 {
		return b, false
	}
	b = b[n:]
	if flags&0x01 != 0 {
		_, n := decodeULEB128(b)
		if n <= 0 {
			return b, false
		}
		b = b[n:]
	}
	return b, true
}

// packageOf extracts the Go import path from a linker symbol.
//
// Symbols look like `runtime.mapaccess1`, `encoding/json.Marshal`,
// `github.com/cleat-team/cleat/engine.(*Engine).Execute`, and there are
// compiler-generated ones -- `type:.eq.[3]string`, `go:string."..."` -- that
// belong to no package and are counted as unattributed rather than invented
// into one.
func packageOf(sym string) string {
	if sym == "" {
		return ""
	}
	// NAMES IN THIS SECTION ARE MANGLED BY THE GO LINKER, and the mangling is
	// lossy: `/`, `:`, `(`, `)` and `*` all become `_`. So `internal/abi.NoEscape`
	// appears as `internal_abi.NoEscape` and `(*Type).Len` as `__Type_.Len`.
	//
	// This does NOT try to undo it. `internal_runtime_math` is provably
	// `internal/runtime/math`, but the inverse is ambiguous in general -- a
	// package whose name legitimately contains `_` is indistinguishable -- and a
	// size report that silently guesses at identifiers is the genre of defect
	// this replaces. The mangled form is reported, and the header says so.
	dot := strings.IndexByte(sym, '.')
	if dot < 0 {
		// `go_buildid` and friends: a symbol belonging to no package.
		return ""
	}
	pkg := sym[:dot]
	// Compiler-generated families. `type:.eq.[3]string` mangles to
	// `type_.eq.[3]string`, and the `go:` symbol families to `go_...`. Counting
	// these as a package called "type_" would invent one; they are real bytes
	// with no package, which is what Unattributed is for.
	if pkg == "type_" || strings.HasPrefix(pkg, "go_") || strings.HasPrefix(pkg, "gcbits_") {
		return ""
	}
	return pkg
}
