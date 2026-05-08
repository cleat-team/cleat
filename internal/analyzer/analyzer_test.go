package analyzer

import (
	"go/token"
	"go/types"
	"testing"
)

func TestHostCallsMethodNil(t *testing.T) {
	if HostCallsMethod(nil) {
		t.Error("HostCallsMethod(nil) should return false")
	}
}

func TestIsEntryPointNotExported(t *testing.T) {
	fd := &FuncDecl{
		Name:       "notExported",
		IsExported: false,
	}
	if IsEntryPoint(fd) {
		t.Error("IsEntryPoint should return false for non-exported function")
	}
}

func TestIsEntryPointWithReceiver(t *testing.T) {
	fd := &FuncDecl{
		Name:       "TestMethod",
		IsExported: true,
		RecvType:   types.Typ[types.Int],
	}
	if IsEntryPoint(fd) {
		t.Error("IsEntryPoint should return false for method with receiver")
	}
}

func TestIsEntryPointNoType(t *testing.T) {
	fd := &FuncDecl{
		Name:       "TestFunc",
		IsExported: true,
		RecvType:   nil,
		Type:       nil,
	}
	if IsEntryPoint(fd) {
		t.Error("IsEntryPoint should return false when Type is nil")
	}
}

func TestIsEntryPointNoParams(t *testing.T) {
	sig := &types.Signature{}
	fd := &FuncDecl{
		Name:       "TestFunc",
		IsExported: true,
		RecvType:   nil,
		Type:       sig,
	}
	if IsEntryPoint(fd) {
		t.Error("IsEntryPoint should return false when Signature has no params")
	}
}

func TestFullyQualifiedNameTopLevel(t *testing.T) {
	fd := &FuncDecl{
		Name: "PlaceOrder",
		Pkg: &Package{
			Name: "workflow",
			Path: "github.com/rcownie/cleat/workflow",
		},
		RecvType:   nil,
		IsExported: true,
	}
	fq := fd.FullyQualifiedName()
	expected := "github.com/rcownie/cleat/workflow.PlaceOrder"
	if fq != expected {
		t.Errorf("FullyQualifiedName() = %q, want %q", fq, expected)
	}
}

func TestFullyQualifiedNameMethod(t *testing.T) {
	// For methods, the FQN uses types.TypeString which needs real types.
	// We test the top-level case which is more commonly used.
	fd := &FuncDecl{
		Name: "Process",
		Pkg: &Package{
			Name: "workflow",
			Path: "github.com/rcownie/cleat/workflow",
		},
	}
	fq := fd.FullyQualifiedName()
	expected := "github.com/rcownie/cleat/workflow.Process"
	if fq != expected {
		t.Errorf("FullyQualifiedName() = %q, want %q", fq, expected)
	}
}

func TestFullyQualifiedNameWithReceiver(t *testing.T) {
	// When RecvType is set, FullyQualifiedName uses types.TypeString with the
	// qualifier function, producing a receiver-qualified name.
	otherPkg := types.NewPackage("other/pkg", "other")
	recvType := types.NewNamed(
		types.NewTypeName(token.Pos(0), otherPkg, "MyStruct", nil),
		types.Typ[types.Int],
		nil,
	)
	fd := &FuncDecl{
		Name: "Process",
		Pkg: &Package{
			Name: "workflow",
			Path: "github.com/rcownie/cleat/workflow",
		},
		RecvType: recvType,
	}
	fq := fd.FullyQualifiedName()
	// "other" from the package name of the receiver type + ".MyStruct.Process"
	expected := "other.MyStruct.Process"
	if fq != expected {
		t.Errorf("FullyQualifiedName() = %q, want %q", fq, expected)
	}
}

func TestFullyQualifiedNameWithSamePackageReceiver(t *testing.T) {
	// When the receiver type belongs to the same package as the function,
	// the qualifier returns "" (no package prefix).
	samePkg := types.NewPackage("github.com/rcownie/cleat/workflow", "workflow")
	recvType := types.NewNamed(
		types.NewTypeName(token.Pos(0), samePkg, "MyStruct", nil),
		types.Typ[types.Int],
		nil,
	)
	fd := &FuncDecl{
		Name: "Process",
		Pkg: &Package{
			Name: "workflow",
			Path: "github.com/rcownie/cleat/workflow",
		},
		RecvType: recvType,
	}
	fq := fd.FullyQualifiedName()
	expected := "MyStruct.Process"
	if fq != expected {
		t.Errorf("FullyQualifiedName() = %q, want %q", fq, expected)
	}
}

func TestLastComponentEdgeCases(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"/", ""},
		{"a/", ""},
		{"/a", "a"},
		{"a/b/c", "c"},
	}
	for _, tc := range tests {
		got := LastComponent(tc.path)
		if got != tc.want {
			t.Errorf("LastComponent(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

// Test FindEnclosingFuncName with nil target returns "".
func TestFindEnclosingFuncNameNilTarget(t *testing.T) {
	result := FindEnclosingFuncName(nil, nil)
	if result != "" {
		t.Errorf("expected empty string, got %q", result)
	}
}

// Test IsHostCallsType with nil returns false.
func TestIsHostCallsTypeNil(t *testing.T) {
	if IsHostCallsType(nil) {
		t.Error("IsHostCallsType(nil) should return false")
	}
}

func TestShortNameEdgeCases(t *testing.T) {
	tests := []struct {
		fqname string
		want   string
	}{
		{"a.b.c.Func", "Func"},
		{"a.b.c", "c"},
		{".", ""},
		{"a.", ""},
		{".a", "a"},
	}
	for _, tc := range tests {
		t.Run(tc.fqname, func(t *testing.T) {
			got := ShortName(tc.fqname)
			if got != tc.want {
				t.Errorf("ShortName(%q) = %q, want %q", tc.fqname, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// IsHostCallsType -- pointer type (backward compatibility) paths
// ---------------------------------------------------------------------------

func TestIsHostCallsTypeWithPointerToHostCalls(t *testing.T) {
	pkg := types.NewPackage("github.com/rcownie/cleat", "cleat")
	obj := types.NewTypeName(token.Pos(0), pkg, "HostCalls", nil)
	named := types.NewNamed(obj, types.Typ[types.Int], nil)
	ptr := types.NewPointer(named)
	if !IsHostCallsType(ptr) {
		t.Error("IsHostCallsType(*cleat.HostCalls) should return true for backward compat")
	}
}

func TestIsHostCallsTypeWithPointerToNonHostCalls(t *testing.T) {
	ptr := types.NewPointer(types.Typ[types.Int])
	if IsHostCallsType(ptr) {
		t.Error("IsHostCallsType(*int) should return false")
	}
}

func TestIsHostCallsTypeWithPointerToWrongNamedType(t *testing.T) {
	pkg := types.NewPackage("my/workflow", "workflow")
	obj := types.NewTypeName(token.Pos(0), pkg, "OrderProcessor", nil)
	named := types.NewNamed(obj, types.Typ[types.Int], nil)
	ptr := types.NewPointer(named)
	if IsHostCallsType(ptr) {
		t.Error("IsHostCallsType(*workflow.OrderProcessor) should return false")
	}
}

func TestIsHostCallsTypeWithHostCallsNameWrongPackage(t *testing.T) {
	pkg := types.NewPackage("other/cleat", "workflow")
	obj := types.NewTypeName(token.Pos(0), pkg, "HostCalls", nil)
	named := types.NewNamed(obj, types.Typ[types.Int], nil)
	if IsHostCallsType(named) {
		t.Error("IsHostCallsType(workflow.HostCalls) should return false for non-cleat pkg")
	}
}

func TestIsHostCallsTypeWithNamedNonHostCalls(t *testing.T) {
	pkg := types.NewPackage("my/workflow", "workflow")
	obj := types.NewTypeName(token.Pos(0), pkg, "SomeType", nil)
	named := types.NewNamed(obj, types.Typ[types.Int], nil)
	if IsHostCallsType(named) {
		t.Error("IsHostCallsType(workflow.SomeType) should return false")
	}
}

func TestIsHostCallsTypeWithPointerToNamedHostCallsWrongPkg(t *testing.T) {
	pkg := types.NewPackage("other/cleat", "workflow")
	obj := types.NewTypeName(token.Pos(0), pkg, "HostCalls", nil)
	named := types.NewNamed(obj, types.Typ[types.Int], nil)
	ptr := types.NewPointer(named)
	if IsHostCallsType(ptr) {
		t.Error("IsHostCallsType(*workflow.HostCalls) should return false for non-cleat pkg")
	}
}
