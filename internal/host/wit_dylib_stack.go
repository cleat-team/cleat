//go:build cgo

package host

import (
	"encoding/binary"
	"fmt"
	"sync"

	"github.com/bytecodealliance/wasmtime-go/v44"
)

// ---------------------------------------------------------------------------
// wit_dylib value stack and metadata parser
//
// The wit_dylib_* functions are a stack machine ABI used by the
// componentize-py adapter. They operate on an abstract value stack
// maintained by the host (Go code), keyed by a context handle (opaque i32).
// The first parameter to push/pop functions is always the context handle,
// NOT a memory pointer.
//
// Metadata format (from the wit-dylib Rust crate's Metadata::encode()):
//
//	Offset 0: 16 x u32 counts for each type category:
//	  [0] num_resources, [1] num_records, [2] num_flags, [3] num_tuples,
//	  [4] num_variants, [5] num_enums, [6] num_options, [7] num_results,
//	  [8] num_lists, [9] num_fixed_length_lists, [10] num_futures,
//	  [11] num_streams, [12] num_aliases, [13] num_import_funcs,
//	  [14] num_export_funcs, [15] unused
//
// After the 16 counts: packed binary arrays for each category in order.
// Each export_func entry likely has 5 x u32 (type_id, name_offset,
// name_len, sync_elem_index, async_elem_index).
// ---------------------------------------------------------------------------

// witDylibValueKind classifies a value on the wit_dylib value stack.
type witDylibValueKind int

const (
	witKindEmpty  witDylibValueKind = iota
	witKindI32
	witKindI64
	witKindF32
	witKindF64
	witKindString
)

// witDylibValue holds one value on the wit_dylib value stack.
// Only one of the value fields is valid, determined by kind.
type witDylibValue struct {
	kind    witDylibValueKind
	i32val  int32
	i64val  int64
	f32val  float32
	f64val  float64
	strData []byte
}

func witValI32(v int32) witDylibValue {
	return witDylibValue{kind: witKindI32, i32val: v}
}

func witValI64(v int64) witDylibValue {
	return witDylibValue{kind: witKindI64, i64val: v}
}

func witValF32(v float32) witDylibValue {
	return witDylibValue{kind: witKindF32, f32val: v}
}

func witValF64(v float64) witDylibValue {
	return witDylibValue{kind: witKindF64, f64val: v}
}

func witValString(data []byte) witDylibValue {
	return witDylibValue{kind: witKindString, strData: data}
}

// ---------------------------------------------------------------------------
// Per-context value stack
// ---------------------------------------------------------------------------

// witDylibContext holds the per-call-context value stack.
type witDylibContext struct {
	stack []witDylibValue
}

// push adds a value to the top of the stack.
func (c *witDylibContext) push(v witDylibValue) {
	c.stack = append(c.stack, v)
}

// pop removes and returns the top value from the stack.
// Returns an empty value (witKindEmpty) if the stack is empty.
func (c *witDylibContext) pop() witDylibValue {
	if len(c.stack) == 0 {
		return witDylibValue{kind: witKindEmpty}
	}
	v := c.stack[len(c.stack)-1]
	c.stack = c.stack[:len(c.stack)-1]
	return v
}

// depth returns the number of values currently on the stack.
func (c *witDylibContext) depth() int {
	return len(c.stack)
}

// ---------------------------------------------------------------------------
// Metadata types
// ---------------------------------------------------------------------------

// witDylibExportFunc describes an exported function found in the metadata.
type witDylibExportFunc struct {
	funcName      string
	syncElemIndex int32 // index into __indirect_function_table
}

// witDylibMetadata holds the 16 type-category counts from the metadata header.
type witDylibMetadata struct {
	counts [16]uint32
}

// ---------------------------------------------------------------------------
// Global state machine
// ---------------------------------------------------------------------------

// witDylibState is the global state for the wit_dylib stack machine.
// It manages per-context value stacks and cached metadata from the
// componentize-py adapter.
type witDylibState struct {
	mu          sync.Mutex
	contexts    map[int32]*witDylibContext
	nextHandle  int32
	metadata    *witDylibMetadata
	exportFuncs []witDylibExportFunc
}

// newWitDylibState creates a fresh wit_dylib state manager.
func newWitDylibState() *witDylibState {
	return &witDylibState{
		contexts:   make(map[int32]*witDylibContext),
		nextHandle: 1,
	}
}

// ---------------------------------------------------------------------------
// Metadata parsing
// ---------------------------------------------------------------------------

// initialize reads and parses the metadata blob at metadataPtr in the given
// WASM linear memory. After this call, the export function table is available
// via getExportElemIndex.
func (s *witDylibState) initialize(mem *wasmtime.Memory, store wasmtime.Storelike, metadataPtr int32) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data := mem.UnsafeData(store)
	if metadataPtr < 0 || int(metadataPtr)+64 > len(data) {
		return fmt.Errorf("wit_dylib: metadata pointer %d out of bounds (mem size %d)", metadataPtr, len(data))
	}

	raw := data[metadataPtr:]

	// Read the 16 category counts.
	if len(raw) < 16*4 {
		return fmt.Errorf("wit_dylib: metadata too short for counts (%d bytes)", len(raw))
	}
	var counts [16]uint32
	for i := 0; i < 16; i++ {
		counts[i] = binary.LittleEndian.Uint32(raw[i*4:])
	}
	s.metadata = &witDylibMetadata{counts: counts}

	// Diagnostic: dump first 64 u32s.
	dumpU32s(data, int(metadataPtr), 64, "wit_dylib metadata")
	fmt.Printf("wit_dylib: metadata counts: %v\n", counts)

	// Parse export function entries from the metadata.
	exportFuncs := s.parseExportFuncs(raw, counts[:])
	s.exportFuncs = exportFuncs

	fmt.Printf("wit_dylib: parsed %d export functions\n", len(exportFuncs))
	for i, ef := range exportFuncs {
		fmt.Printf("wit_dylib:   export[%d] name=%q elem=%d\n", i, ef.funcName, ef.syncElemIndex)
	}

	return nil
}

// parseExportFuncs extracts export function entries from the metadata blob
// by scanning for structured entries with ASCII function names.
func (s *witDylibState) parseExportFuncs(raw []byte, counts []uint32) []witDylibExportFunc {
	numExportFuncs := int(counts[14])
	if numExportFuncs <= 0 {
		return nil
	}

	// Skip past the 16 counts (64 bytes) to reach the packed arrays.
	offset := 64

	// Estimate entry sizes in u32 words for each type category.
	// These match the expected layout from the wit-dylib Rust crate.
	categoryEntryWords := []int{
		2, // 0:  resources
		3, // 1:  records
		1, // 2:  flags
		2, // 3:  tuples
		3, // 4:  variants
		1, // 5:  enums
		1, // 6:  options
		1, // 7:  results
		1, // 8:  lists
		2, // 9:  fixed_length_lists
		1, // 10: futures
		1, // 11: streams
		1, // 12: aliases
		3, // 13: import_funcs
		5, // 14: export_funcs
		0, // 15: unused
	}

	// Compute the byte offset to the export_funcs section.
	exportOff := offset
	for cat := 0; cat < 14; cat++ {
		exportOff += int(counts[cat]) * categoryEntryWords[cat] * 4
	}

	// Try parsing at the computed offset. Fall back to scanning if out of bounds.
	if exportOff < 0 || exportOff+numExportFuncs*20 > len(raw) {
		fmt.Printf("wit_dylib: computed export offset %d out of bounds, scanning instead\n", exportOff)
		return s.scanForExportFuncs(raw, numExportFuncs)
	}

	fmt.Printf("wit_dylib: export funcs at offset %d, count=%d\n", exportOff, numExportFuncs)

	funcs := s.tryParseExportFuncs(raw, exportOff, numExportFuncs)
	if len(funcs) > 0 {
		return funcs
	}

	// Try relaxed scanning fallback.
	fmt.Printf("wit_dylib: falling back to string-scanning for export funcs\n")
	return s.scanForExportFuncs(raw, numExportFuncs)
}

// tryParseExportFuncs attempts to parse export entries at a known offset.
// Each entry is expected to be 5 x u32: type_id, name_offset, name_len,
// sync_elem_index, async_elem_index.
func (s *witDylibState) tryParseExportFuncs(raw []byte, exportOff, count int) []witDylibExportFunc {
	const entryBytes = 20 // 5 u32s

	if exportOff+count*entryBytes > len(raw) {
		return nil
	}

	funcs := make([]witDylibExportFunc, 0, count)
	for i := 0; i < count; i++ {
		entry := raw[exportOff+i*entryBytes:]
		typeID := binary.LittleEndian.Uint32(entry[0:])
		nameOff := binary.LittleEndian.Uint32(entry[4:])
		nameLen := binary.LittleEndian.Uint32(entry[8:])
		syncIdx := int32(binary.LittleEndian.Uint32(entry[12:]))
		_ = binary.LittleEndian.Uint32(entry[16:]) // async_elem_index
		_ = typeID

		name := resolveStringFromRaw(raw, int(nameOff), int(nameLen))
		if name != "" {
			funcs = append(funcs, witDylibExportFunc{
				funcName:      name,
				syncElemIndex: syncIdx,
			})
		} else {
			fmt.Printf("wit_dylib: export[%d] type_id=%d nameOff=%d nameLen=%d sync=%d (unresolved)\n",
				i, typeID, nameOff, nameLen, syncIdx)
		}
	}
	return funcs
}

// scanForExportFuncs scans the metadata blob looking for 20-byte sequences
// that look like valid export func entries (u32 type_id, u32 name_offset,
// u32 name_len, u32 sync_idx, u32 async_idx) where name_offset and name_len
// point to a printable ASCII string.
func (s *witDylibState) scanForExportFuncs(raw []byte, maxResults int) []witDylibExportFunc {
	if maxResults <= 0 {
		maxResults = 100
	}
	var funcs []witDylibExportFunc

	for scan := 64; scan < len(raw)-20 && len(funcs) < maxResults; scan++ {
		typeID := binary.LittleEndian.Uint32(raw[scan:])
		nameOff := binary.LittleEndian.Uint32(raw[scan+4:])
		nameLen := binary.LittleEndian.Uint32(raw[scan+8:])
		elemIdx := binary.LittleEndian.Uint32(raw[scan+12:])

		if nameOff == 0 || nameLen == 0 || nameLen > 256 || typeID > 10000 {
			continue
		}
		if int(nameOff)+int(nameLen) > len(raw) {
			continue
		}

		if !isAllASCII(raw[nameOff : nameOff+nameLen]) {
			continue
		}
		name := string(raw[nameOff : nameOff+nameLen])
		if !looksLikeIdentifier(name) {
			continue
		}

		funcs = append(funcs, witDylibExportFunc{
			funcName:      name,
			syncElemIndex: int32(elemIdx),
		})
		// Skip past this entry.
		scan += 19 // +1 for loop increment
	}

	// If scanning didn't find any, also scan for inline strings
	// (names that are stored directly in the entry, not referenced by offset).
	if len(funcs) == 0 {
		funcs = s.scanForInlineStrings(raw, maxResults)
	}

	return funcs
}

// scanForInlineStrings scans for export entries where the function name is
// stored inline within the entry rather than referenced by offset.
func (s *witDylibState) scanForInlineStrings(raw []byte, maxResults int) []witDylibExportFunc {
	var funcs []witDylibExportFunc

	for scan := 64; scan < len(raw)-20 && len(funcs) < maxResults; scan++ {
		typeID := binary.LittleEndian.Uint32(raw[scan:])
		syncIdx := int32(binary.LittleEndian.Uint32(raw[scan+12:]))
		_ = typeID

		// Check if there's an ASCII string starting 16 bytes in.
		candidateOff := scan + 16
		if candidateOff+4 > len(raw) {
			continue
		}
		end := candidateOff
		for end < len(raw) && raw[end] >= 32 && raw[end] < 127 {
			end++
		}
		if end-candidateOff >= 3 {
			name := string(raw[candidateOff:end])
			if looksLikeIdentifier(name) {
				funcs = append(funcs, witDylibExportFunc{
					funcName:      name,
					syncElemIndex: syncIdx,
				})
				scan += 19
			}
		}
	}
	return funcs
}

// resolveStringFromRaw tries to read a printable ASCII string from the raw
// metadata blob at the given offset and length.
func resolveStringFromRaw(raw []byte, off, length int) string {
	if length <= 0 || length > 256 || off < 0 || off+length > len(raw) {
		return ""
	}
	if !isAllASCII(raw[off : off+length]) {
		return ""
	}
	return string(raw[off : off+length])
}

// ---------------------------------------------------------------------------
// Context management
// ---------------------------------------------------------------------------

// exportStart creates a new value-stack context and returns its handle.
func (s *witDylibState) exportStart(funcIndex int32) int32 {
	s.mu.Lock()
	defer s.mu.Unlock()

	handle := s.nextHandle
	s.nextHandle++
	s.contexts[handle] = &witDylibContext{}
	_ = funcIndex
	return handle
}

// exportFinish destroys a context identified by handle.
func (s *witDylibState) exportFinish(ctx int32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.contexts, ctx)
}

// getContext returns the context for the given handle, or nil.
func (s *witDylibState) getContext(ctx int32) *witDylibContext {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.contexts[ctx]
}

// ---------------------------------------------------------------------------
// Export function lookup
// ---------------------------------------------------------------------------

// getExportElemIndex returns the sync_elem_index for the given export
// function index. This is the index into the WASM __indirect_function_table
// that dispatches to the correct function implementation. Returns -1 if
// the index is out of range.
func (s *witDylibState) getExportElemIndex(funcIndex int32) int32 {
	if int(funcIndex) < len(s.exportFuncs) {
		return s.exportFuncs[funcIndex].syncElemIndex
	}
	return -1
}

// getExportFunc returns the export function at the given index, or nil.
func (s *witDylibState) getExportFunc(funcIndex int32) *witDylibExportFunc {
	if funcIndex >= 0 && int(funcIndex) < len(s.exportFuncs) {
		return &s.exportFuncs[funcIndex]
	}
	return nil
}

// exportNames returns the names of all parsed export functions.
func (s *witDylibState) exportNames() []string {
	names := make([]string, len(s.exportFuncs))
	for i, ef := range s.exportFuncs {
		names[i] = ef.funcName
	}
	return names
}

// ---------------------------------------------------------------------------
// Stack push operations
// ---------------------------------------------------------------------------

// pushI32 pushes a 32-bit signed integer onto the given context's stack.
func (s *witDylibState) pushI32(ctx int32, val int32) {
	c := s.getContext(ctx)
	if c != nil {
		c.push(witValI32(val))
	}
}

// pushI64 pushes a 64-bit signed integer onto the given context's stack.
func (s *witDylibState) pushI64(ctx int32, val int64) {
	c := s.getContext(ctx)
	if c != nil {
		c.push(witValI64(val))
	}
}

// pushF32 pushes a 32-bit float onto the given context's stack.
func (s *witDylibState) pushF32(ctx int32, val float32) {
	c := s.getContext(ctx)
	if c != nil {
		c.push(witValF32(val))
	}
}

// pushF64 pushes a 64-bit float onto the given context's stack.
func (s *witDylibState) pushF64(ctx int32, val float64) {
	c := s.getContext(ctx)
	if c != nil {
		c.push(witValF64(val))
	}
}

// pushString pushes a byte slice (owned string data) onto the given
// context's stack. The data is copied and owned by the stack.
func (s *witDylibState) pushString(ctx int32, data []byte) {
	c := s.getContext(ctx)
	if c != nil {
		owned := make([]byte, len(data))
		copy(owned, data)
		c.push(witValString(owned))
	}
}

// pushStringFromMemory reads a string from WASM linear memory at (ptr, len)
// and pushes it onto the given context's stack.
func (s *witDylibState) pushStringFromMemory(ctx int32, mem *wasmtime.Memory, store wasmtime.Storelike, ptr, length int32) {
	raw := mem.UnsafeData(store)
	if ptr < 0 || length < 0 || int(ptr)+int(length) > len(raw) {
		s.pushString(ctx, nil)
		return
	}
	data := make([]byte, length)
	copy(data, raw[ptr:ptr+length])
	s.pushString(ctx, data)
}

// pushRecord pushes a record/tuple/variant/option/result marker onto the
// stack. These are treated as simple stack markers (count indicators).
func (s *witDylibState) pushRecord(ctx int32) {
	s.pushI32(ctx, 1)
}

// pushOption pushes an option marker (1 = some, 0 = none).
func (s *witDylibState) pushOption(ctx int32, isSome bool) {
	if isSome {
		s.pushI32(ctx, 1)
	} else {
		s.pushI32(ctx, 0)
	}
}

// pushResult pushes a result marker (1 = ok, 0 = err).
func (s *witDylibState) pushResult(ctx int32, isOk bool) {
	if isOk {
		s.pushI32(ctx, 1)
	} else {
		s.pushI32(ctx, 0)
	}
}

// pushList pushes a list marker with a type index.
func (s *witDylibState) pushList(ctx int32, typeIndex int32) {
	s.pushI32(ctx, typeIndex)
}

// ---------------------------------------------------------------------------
// Stack pop operations
// ---------------------------------------------------------------------------

// popI32 pops a 32-bit signed integer from the given context's stack.
// Returns 0 if the stack is empty or the top value is not an i32.
func (s *witDylibState) popI32(ctx int32) int32 {
	c := s.getContext(ctx)
	if c != nil {
		v := c.pop()
		if v.kind == witKindI32 {
			return v.i32val
		}
	}
	return 0
}

// popI64 pops a 64-bit signed integer from the given context's stack.
// Returns 0 if the stack is empty or the top value is not an i64.
func (s *witDylibState) popI64(ctx int32) int64 {
	c := s.getContext(ctx)
	if c != nil {
		v := c.pop()
		if v.kind == witKindI64 {
			return v.i64val
		}
	}
	return 0
}

// popF32 pops a 32-bit float from the given context's stack.
// Returns 0 if the stack is empty or the top value is not an f32.
func (s *witDylibState) popF32(ctx int32) float32 {
	c := s.getContext(ctx)
	if c != nil {
		v := c.pop()
		if v.kind == witKindF32 {
			return v.f32val
		}
	}
	return 0
}

// popF64 pops a 64-bit float from the given context's stack.
// Returns 0 if the stack is empty or the top value is not an f64.
func (s *witDylibState) popF64(ctx int32) float64 {
	c := s.getContext(ctx)
	if c != nil {
		v := c.pop()
		if v.kind == witKindF64 {
			return v.f64val
		}
	}
	return 0
}

// popString pops a string from the given context's stack and writes it to
// WASM linear memory at scratchAddr. The string data is written at the
// scratch address as a pointer+length pair (8 bytes: 4-byte ptr to string
// data, 4-byte length), followed by the actual string bytes.
//
// Returns the length of the string data written, or 0 on failure.
func (s *witDylibState) popString(ctx int32, mem *wasmtime.Memory, store wasmtime.Storelike, scratchAddr int32) int32 {
	c := s.getContext(ctx)
	if c == nil {
		return 0
	}
	v := c.pop()
	if v.kind != witKindString {
		return 0
	}
	if len(v.strData) == 0 {
		// Write null pointer and zero length.
		data := mem.UnsafeData(store)
		if scratchAddr >= 0 && int(scratchAddr)+8 <= len(data) {
			binary.LittleEndian.PutUint32(data[scratchAddr:], 0)
			binary.LittleEndian.PutUint32(data[scratchAddr+4:], 0)
		}
		return 0
	}

	data := mem.UnsafeData(store)
	strLen := len(v.strData)
	totalNeeded := 8 + strLen
	if scratchAddr < 0 || int(scratchAddr)+totalNeeded > len(data) {
		return 0
	}

	// Write pointer to string data (at scratchAddr+8) and length.
	dataPtr := scratchAddr + 8
	binary.LittleEndian.PutUint32(data[scratchAddr:], uint32(dataPtr))
	binary.LittleEndian.PutUint32(data[scratchAddr+4:], uint32(strLen))
	copy(data[dataPtr:], v.strData)

	return int32(strLen)
}

// popStringData pops a string and returns the raw bytes without writing
// to linear memory. The returned slice is owned by the caller.
func (s *witDylibState) popStringData(ctx int32) []byte {
	c := s.getContext(ctx)
	if c == nil {
		return nil
	}
	v := c.pop()
	if v.kind != witKindString {
		return nil
	}
	return v.strData
}

// popRecord pops a record/tuple/variant/option/result/list marker.
// Returns the type index that was pushed with the corresponding push call.
func (s *witDylibState) popRecord(ctx int32) int32 {
	return s.popI32(ctx)
}

// ---------------------------------------------------------------------------
// Diagnostic helpers
// ---------------------------------------------------------------------------

// dumpU32s prints the first n u32 values starting at baseOff in data.
func dumpU32s(data []byte, baseOff int, n int, label string) {
	maxCount := (len(data) - baseOff) / 4
	if maxCount <= 0 {
		fmt.Printf("%s: no data at offset %d\n", label, baseOff)
		return
	}
	if n > maxCount {
		n = maxCount
	}
	fmt.Printf("%s: first %d u32s at offset %d:\n", label, n, baseOff)
	for i := 0; i < n; i++ {
		if i%8 == 0 {
			fmt.Printf("  [%4d]", baseOff+i*4)
		}
		fmt.Printf(" %08x", binary.LittleEndian.Uint32(data[baseOff+4*i:]))
		if i%8 == 7 {
			fmt.Println()
		}
	}
	if n%8 != 0 {
		fmt.Println()
	}
}

// isAllASCII returns true if all bytes in b are printable ASCII characters
// (space 0x20 through tilde 0x7E).
func isAllASCII(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	for _, c := range b {
		if c < 32 || c > 126 {
			return false
		}
	}
	return true
}

// looksLikeIdentifier returns true if s is a valid identifier-like string
// (starts with a letter or underscore, contains only letters, digits,
// underscores).
func looksLikeIdentifier(s string) bool {
	if len(s) == 0 {
		return false
	}
	for i, c := range []byte(s) {
		if i == 0 {
			if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && c != '_' {
				return false
			}
		} else {
			if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') &&
				(c < '0' || c > '9') && c != '_' {
				return false
			}
		}
	}
	return true
}
