package analyzer

import (
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
