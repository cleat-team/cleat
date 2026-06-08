//go:build cgo

package engine

import (
	"encoding/binary"
	"testing"
)

// ---------------------------------------------------------------------------
// Value constructors
// ---------------------------------------------------------------------------

func TestWitValI32(t *testing.T) {
	v := witValI32(42)
	if v.kind != witKindI32 || v.i32val != 42 {
		t.Errorf("expected i32=42, got kind=%v val=%d", v.kind, v.i32val)
	}
}

func TestWitValI64(t *testing.T) {
	v := witValI64(1234567890123)
	if v.kind != witKindI64 || v.i64val != 1234567890123 {
		t.Errorf("expected i64=1234567890123, got kind=%v val=%d", v.kind, v.i64val)
	}
}

func TestWitValF32(t *testing.T) {
	v := witValF32(3.14)
	if v.kind != witKindF32 || v.f32val != 3.14 {
		t.Errorf("expected f32=3.14, got kind=%v val=%f", v.kind, v.f32val)
	}
}

func TestWitValF64(t *testing.T) {
	v := witValF64(2.718281828459045)
	if v.kind != witKindF64 || v.f64val != 2.718281828459045 {
		t.Errorf("expected f64=2.718281828459045, got kind=%v val=%f", v.kind, v.f64val)
	}
}

func TestWitValString(t *testing.T) {
	v := witValString([]byte("hello"))
	if v.kind != witKindString || string(v.strData) != "hello" {
		t.Errorf("expected string 'hello', got kind=%v data=%q", v.kind, string(v.strData))
	}
}

func TestWitValString_Empty(t *testing.T) {
	v := witValString([]byte{})
	if v.kind != witKindString || len(v.strData) != 0 {
		t.Errorf("expected empty string, got kind=%v len=%d", v.kind, len(v.strData))
	}
}

// ---------------------------------------------------------------------------
// Stack operations (witDylibContext)
// ---------------------------------------------------------------------------

func TestWitDylibContext_PushPop(t *testing.T) {
	ctx := &witDylibContext{}
	ctx.push(witValI32(10))
	ctx.push(witValI64(20))
	v1 := ctx.pop()
	if v1.kind != witKindI64 || v1.i64val != 20 {
		t.Errorf("expected i64=20, got kind=%v val=%d", v1.kind, v1.i64val)
	}
	v2 := ctx.pop()
	if v2.kind != witKindI32 || v2.i32val != 10 {
		t.Errorf("expected i32=10, got kind=%v val=%d", v2.kind, v2.i32val)
	}
}

func TestWitDylibContext_PopEmpty(t *testing.T) {
	ctx := &witDylibContext{}
	v := ctx.pop()
	if v.kind != witKindEmpty {
		t.Errorf("expected witKindEmpty, got %v", v.kind)
	}
}

func TestWitDylibContext_Depth(t *testing.T) {
	ctx := &witDylibContext{}
	if ctx.depth() != 0 {
		t.Errorf("expected depth 0, got %d", ctx.depth())
	}
	ctx.push(witValI32(1))
	if ctx.depth() != 1 {
		t.Errorf("expected depth 1, got %d", ctx.depth())
	}
	ctx.push(witValI64(2))
	if ctx.depth() != 2 {
		t.Errorf("expected depth 2, got %d", ctx.depth())
	}
	ctx.pop()
	if ctx.depth() != 1 {
		t.Errorf("expected depth 1 after pop, got %d", ctx.depth())
	}
}

func TestWitDylibContext_MixedTypes(t *testing.T) {
	ctx := &witDylibContext{}
	vals := []witDylibValue{
		witValI32(-1), witValI64(1 << 40), witValF32(1.5), witValF64(2.5), witValString([]byte("world")),
	}
	for _, v := range vals {
		ctx.push(v)
	}
	if ctx.depth() != 5 {
		t.Fatalf("expected depth 5, got %d", ctx.depth())
	}
	v := ctx.pop()
	if v.kind != witKindString || string(v.strData) != "world" {
		t.Errorf("expected string 'world', got kind=%v", v.kind)
	}
	v = ctx.pop()
	if v.kind != witKindF64 || v.f64val != 2.5 {
		t.Errorf("expected f64=2.5, got kind=%v", v.kind)
	}
	v = ctx.pop()
	if v.kind != witKindF32 || v.f32val != 1.5 {
		t.Errorf("expected f32=1.5, got kind=%v", v.kind)
	}
	v = ctx.pop()
	if v.kind != witKindI64 || v.i64val != 1<<40 {
		t.Errorf("expected i64=%d, got kind=%v", uint64(1<<40), v.kind)
	}
	v = ctx.pop()
	if v.kind != witKindI32 || v.i32val != -1 {
		t.Errorf("expected i32=-1, got kind=%v", v.kind)
	}
}

// ---------------------------------------------------------------------------
// State management
// ---------------------------------------------------------------------------

func TestNewWitDylibState(t *testing.T) {
	state := newWitDylibState()
	if state == nil || state.contexts == nil {
		t.Fatal("expected non-nil state with contexts map")
	}
	if state.nextHandle != 1 {
		t.Errorf("expected nextHandle=1, got %d", state.nextHandle)
	}
}

func TestWitDylibState_ExportStartFinish(t *testing.T) {
	state := newWitDylibState()
	handle := state.exportStart(0)
	if handle != 1 {
		t.Errorf("expected handle=1, got %d", handle)
	}
	ctx := state.getContext(handle)
	if ctx == nil || ctx.depth() != 0 {
		t.Fatal("expected non-nil empty context")
	}
	state.exportFinish(handle)
	if state.getContext(handle) != nil {
		t.Error("expected nil context after finish")
	}
}

func TestWitDylibState_GetContext_Nil(t *testing.T) {
	state := newWitDylibState()
	if state.getContext(999) != nil {
		t.Error("expected nil for invalid handle")
	}
}

func TestWitDylibState_ExportStart_MultipleHandles(t *testing.T) {
	state := newWitDylibState()
	h1 := state.exportStart(0)
	h2 := state.exportStart(1)
	if h1 == h2 {
		t.Error("expected different handles")
	}
	c1, c2 := state.getContext(h1), state.getContext(h2)
	if c1 == nil || c2 == nil {
		t.Fatal("expected non-nil contexts")
	}
	c1.push(witValI32(100))
	c2.push(witValI32(200))
	if c1.stack[0].i32val != 100 || c2.stack[0].i32val != 200 {
		t.Error("contexts not isolated")
	}
}

// ---------------------------------------------------------------------------
// Push/Pop on state
// ---------------------------------------------------------------------------

func TestWitDylibState_PushPopI32(t *testing.T) {
	s := newWitDylibState()
	h := s.exportStart(0)
	s.pushI32(h, 42)
	if s.popI32(h) != 42 {
		t.Error("expected 42")
	}
}

func TestWitDylibState_PushPopI64(t *testing.T) {
	s := newWitDylibState()
	h := s.exportStart(0)
	s.pushI64(h, -999)
	if s.popI64(h) != -999 {
		t.Error("expected -999")
	}
}

func TestWitDylibState_PushPopF32(t *testing.T) {
	s := newWitDylibState()
	h := s.exportStart(0)
	s.pushF32(h, 3.14)
	if s.popF32(h) != 3.14 {
		t.Error("expected 3.14")
	}
}

func TestWitDylibState_PushPopF64(t *testing.T) {
	s := newWitDylibState()
	h := s.exportStart(0)
	s.pushF64(h, 2.718281828)
	if s.popF64(h) != 2.718281828 {
		t.Error("expected 2.718281828")
	}
}

func TestWitDylibState_PushPopString(t *testing.T) {
	s := newWitDylibState()
	h := s.exportStart(0)
	s.pushString(h, []byte("hello"))
	if string(s.popStringData(h)) != "hello" {
		t.Error("expected 'hello'")
	}
}

func TestWitDylibState_PushString_InvalidCtx(t *testing.T) {
	s := newWitDylibState()
	s.pushString(999, []byte("test")) // should not panic
}

func TestWitDylibState_PopStringData_NilCtx(t *testing.T) {
	if newWitDylibState().popStringData(999) != nil {
		t.Error("expected nil for invalid context")
	}
}

func TestWitDylibState_PushRecord(t *testing.T) {
	s := newWitDylibState()
	h := s.exportStart(0)
	s.pushRecord(h)
	if s.popRecord(h) != 1 {
		t.Error("expected 1")
	}
}

func TestWitDylibState_PushOption_Some(t *testing.T) {
	s := newWitDylibState()
	h := s.exportStart(0)
	s.pushOption(h, true)
	if s.popI32(h) != 1 {
		t.Error("expected 1 for some")
	}
}

func TestWitDylibState_PushOption_None(t *testing.T) {
	s := newWitDylibState()
	h := s.exportStart(0)
	s.pushOption(h, false)
	if s.popI32(h) != 0 {
		t.Error("expected 0 for none")
	}
}

func TestWitDylibState_PushResult_Ok(t *testing.T) {
	s := newWitDylibState()
	h := s.exportStart(0)
	s.pushResult(h, true)
	if s.popI32(h) != 1 {
		t.Error("expected 1 for ok")
	}
}

func TestWitDylibState_PushResult_Err(t *testing.T) {
	s := newWitDylibState()
	h := s.exportStart(0)
	s.pushResult(h, false)
	if s.popI32(h) != 0 {
		t.Error("expected 0 for err")
	}
}

func TestWitDylibState_PushList(t *testing.T) {
	s := newWitDylibState()
	h := s.exportStart(0)
	s.pushList(h, 7)
	if s.popI32(h) != 7 {
		t.Error("expected 7")
	}
}

func TestWitDylibState_PopRecord(t *testing.T) {
	s := newWitDylibState()
	h := s.exportStart(0)
	s.pushI32(h, 5)
	if s.popRecord(h) != 5 {
		t.Error("expected 5")
	}
}

func TestWitDylibState_PopI32_WrongKind(t *testing.T) {
	s := newWitDylibState()
	h := s.exportStart(0)
	s.pushI64(h, 123)
	if s.popI32(h) != 0 {
		t.Error("expected 0 for wrong kind")
	}
}

func TestWitDylibState_PopI64_WrongKind(t *testing.T) {
	s := newWitDylibState()
	h := s.exportStart(0)
	s.pushI32(h, 42)
	if s.popI64(h) != 0 {
		t.Error("expected 0 for wrong kind")
	}
}

func TestWitDylibState_PopF32_WrongKind(t *testing.T) {
	s := newWitDylibState()
	h := s.exportStart(0)
	s.pushI32(h, 1)
	if s.popF32(h) != 0 {
		t.Error("expected 0 for wrong kind")
	}
}

func TestWitDylibState_PopF64_WrongKind(t *testing.T) {
	s := newWitDylibState()
	h := s.exportStart(0)
	s.pushI32(h, 1)
	if s.popF64(h) != 0 {
		t.Error("expected 0 for wrong kind")
	}
}

func TestWitDylibState_PopI32_InvalidCtx(t *testing.T) {
	if newWitDylibState().popI32(999) != 0 {
		t.Error("expected 0 for invalid context")
	}
}

func TestWitDylibState_PopStringData_EmptyStack(t *testing.T) {
	s := newWitDylibState()
	h := s.exportStart(0)
	if s.popStringData(h) != nil {
		t.Error("expected nil for empty stack")
	}
}

func TestWitDylibState_PopStringData_WrongKind(t *testing.T) {
	s := newWitDylibState()
	h := s.exportStart(0)
	s.pushI32(h, 1)
	if s.popStringData(h) != nil {
		t.Error("expected nil for wrong kind")
	}
}

// ---------------------------------------------------------------------------
// Export function lookup
// ---------------------------------------------------------------------------

func TestWitDylibState_GetExportElemIndex_Empty(t *testing.T) {
	if newWitDylibState().getExportElemIndex(0) != -1 {
		t.Error("expected -1 for empty exportFuncs")
	}
}

func TestWitDylibState_GetExportElemIndex_OutOfRange(t *testing.T) {
	s := newWitDylibState()
	s.exportFuncs = []witDylibExportFunc{{funcName: "foo", syncElemIndex: 5}}
	if s.getExportElemIndex(1) != -1 {
		t.Error("expected -1 for out of range")
	}
}

func TestWitDylibState_GetExportElemIndex_Valid(t *testing.T) {
	s := newWitDylibState()
	s.exportFuncs = []witDylibExportFunc{
		{funcName: "foo", syncElemIndex: 5},
		{funcName: "bar", syncElemIndex: 10},
	}
	if s.getExportElemIndex(0) != 5 || s.getExportElemIndex(1) != 10 {
		t.Error("unexpected elem indices")
	}
}

func TestWitDylibState_GetExportFunc_Nil(t *testing.T) {
	if newWitDylibState().getExportFunc(0) != nil {
		t.Error("expected nil for empty exportFuncs")
	}
}

func TestWitDylibState_GetExportFunc_OutOfRange(t *testing.T) {
	s := newWitDylibState()
	s.exportFuncs = []witDylibExportFunc{{funcName: "foo", syncElemIndex: 5}}
	if s.getExportFunc(1) != nil || s.getExportFunc(-1) != nil {
		t.Error("expected nil for out of range")
	}
}

func TestWitDylibState_GetExportFunc_Valid(t *testing.T) {
	s := newWitDylibState()
	s.exportFuncs = []witDylibExportFunc{{funcName: "my_func", syncElemIndex: 3}}
	ef := s.getExportFunc(0)
	if ef == nil || ef.funcName != "my_func" || ef.syncElemIndex != 3 {
		t.Errorf("unexpected export func: %+v", ef)
	}
}

func TestWitDylibState_ExportNames_Empty(t *testing.T) {
	if len(newWitDylibState().exportNames()) != 0 {
		t.Error("expected empty names")
	}
}

func TestWitDylibState_ExportNames_WithEntries(t *testing.T) {
	s := newWitDylibState()
	s.exportFuncs = []witDylibExportFunc{
		{funcName: "alpha", syncElemIndex: 1},
		{funcName: "beta", syncElemIndex: 2},
	}
	names := s.exportNames()
	if len(names) != 2 || names[0] != "alpha" || names[1] != "beta" {
		t.Errorf("unexpected names: %v", names)
	}
}

// ---------------------------------------------------------------------------
// String helpers
// ---------------------------------------------------------------------------

func TestIsAllASCII_Valid(t *testing.T) {
	if !isAllASCII([]byte("hello")) || !isAllASCII([]byte("Hello World 123")) {
		t.Error("expected true for ASCII")
	}
}

func TestIsAllASCII_Empty(t *testing.T) {
	if isAllASCII([]byte{}) {
		t.Error("expected false for empty slice")
	}
}

func TestIsAllASCII_NonPrintable(t *testing.T) {
	if isAllASCII([]byte{0x01}) || isAllASCII([]byte{0x1F}) {
		t.Error("expected false for control chars")
	}
}

func TestIsAllASCII_HighBit(t *testing.T) {
	if isAllASCII([]byte{0x80}) || isAllASCII([]byte("hello\xc0world")) {
		t.Error("expected false for high-bit")
	}
}

func TestLooksLikeIdentifier_Valid(t *testing.T) {
	if !looksLikeIdentifier("myFunc") || !looksLikeIdentifier("_private") || !looksLikeIdentifier("A1_b2_C3") {
		t.Error("expected true for valid identifiers")
	}
}

func TestLooksLikeIdentifier_StartsWithDigit(t *testing.T) {
	if looksLikeIdentifier("1invalid") {
		t.Error("expected false for leading digit")
	}
}

func TestLooksLikeIdentifier_ContainsSpecial(t *testing.T) {
	if looksLikeIdentifier("my-func") || looksLikeIdentifier("foo.bar") {
		t.Error("expected false for special chars")
	}
}

func TestLooksLikeIdentifier_Empty(t *testing.T) {
	if looksLikeIdentifier("") {
		t.Error("expected false for empty")
	}
}

func TestResolveStringFromRaw_Valid(t *testing.T) {
	raw := []byte("prefix__hello_world__suffix")
	if resolveStringFromRaw(raw, 8, 11) != "hello_world" {
		t.Error("expected 'hello_world'")
	}
}

func TestResolveStringFromRaw_Empty(t *testing.T) {
	if resolveStringFromRaw([]byte("data"), 0, 0) != "" {
		t.Error("expected empty for zero length")
	}
}

func TestResolveStringFromRaw_OOB(t *testing.T) {
	if resolveStringFromRaw([]byte("abc"), 1, 5) != "" {
		t.Error("expected empty for OOB")
	}
}

func TestResolveStringFromRaw_NegativeOffset(t *testing.T) {
	if resolveStringFromRaw([]byte("abc"), -1, 2) != "" {
		t.Error("expected empty for negative offset")
	}
}

func TestResolveStringFromRaw_LengthTooLong(t *testing.T) {
	if resolveStringFromRaw([]byte("abc"), 0, 300) != "" {
		t.Error("expected empty for too long")
	}
}

func TestResolveStringFromRaw_NonASCII(t *testing.T) {
	if resolveStringFromRaw([]byte{0x80, 0x81, 0x82}, 0, 3) != "" {
		t.Error("expected empty for non-ASCII")
	}
}

// ---------------------------------------------------------------------------
// Metadata parsing helpers
// ---------------------------------------------------------------------------

func buildMetadataBlob(exportFuncs []witDylibExportFunc) []byte {
	counts := make([]uint32, 16)
	counts[14] = uint32(len(exportFuncs))

	buf := make([]byte, 64+len(exportFuncs)*20)
	for i := 0; i < 16; i++ {
		binary.LittleEndian.PutUint32(buf[i*4:], counts[i])
	}

	nameStart := 64 + len(exportFuncs)*20
	for i, ef := range exportFuncs {
		off := 64 + i*20
		binary.LittleEndian.PutUint32(buf[off:], 0)                         // type_id
		binary.LittleEndian.PutUint32(buf[off+4:], uint32(nameStart))       // name_offset
		binary.LittleEndian.PutUint32(buf[off+8:], uint32(len(ef.funcName))) // name_len
		binary.LittleEndian.PutUint32(buf[off+12:], uint32(ef.syncElemIndex))
		nameStart += len(ef.funcName)
	}

	for _, ef := range exportFuncs {
		buf = append(buf, []byte(ef.funcName)...)
	}

	return buf
}

func TestParseExportFuncs_Empty(t *testing.T) {
	s := newWitDylibState()
	counts := make([]uint32, 16)
	blob := buildMetadataBlob(nil)
	if len(s.parseExportFuncs(blob, counts)) != 0 {
		t.Error("expected 0 funcs")
	}
}

func TestParseExportFuncs_WithValidEntries(t *testing.T) {
	s := newWitDylibState()
	funcs := []witDylibExportFunc{
		{funcName: "hello", syncElemIndex: 1},
		{funcName: "world", syncElemIndex: 2},
	}
	blob := buildMetadataBlob(funcs)
	counts := []uint32{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 2, 0}
	result := s.parseExportFuncs(blob, counts)
	if len(result) != 2 {
		t.Fatalf("expected 2 funcs, got %d", len(result))
	}
	if result[0].funcName != "hello" || result[0].syncElemIndex != 1 {
		t.Errorf("unexpected: %+v", result[0])
	}
	if result[1].funcName != "world" || result[1].syncElemIndex != 2 {
		t.Errorf("unexpected: %+v", result[1])
	}
}

func TestTryParseExportFuncs_Valid(t *testing.T) {
	s := newWitDylibState()
	blob := buildMetadataBlob([]witDylibExportFunc{{funcName: "run", syncElemIndex: 7}})
	result := s.tryParseExportFuncs(blob, 64, 1)
	if len(result) != 1 || result[0].funcName != "run" {
		t.Errorf("unexpected: %+v", result)
	}
}

func TestTryParseExportFuncs_OOB(t *testing.T) {
	s := newWitDylibState()
	if s.tryParseExportFuncs([]byte("short"), 100, 10) != nil {
		t.Error("expected nil for OOB")
	}
}

func TestScanForExportFuncs_NoMatch(t *testing.T) {
	s := newWitDylibState()
	if len(s.scanForExportFuncs(make([]byte, 128), 10)) != 0 {
		t.Error("expected 0 funcs from all-zero blob")
	}
}

func TestScanForInlineStrings_WithValidName(t *testing.T) {
	s := newWitDylibState()
	blob := make([]byte, 256)
	binary.LittleEndian.PutUint32(blob[64:], 0) // type_id
	binary.LittleEndian.PutUint32(blob[76:], 5) // syncIdx
	copy(blob[80:], "myFuncXYZZ")
	blob[80+10] = 0 // NUL byte stops the ASCII scan

	funcs := s.scanForInlineStrings(blob, 5)
	if len(funcs) != 1 || funcs[0].funcName != "myFuncXYZZ" {
		t.Errorf("unexpected: %+v", funcs)
	}
}
