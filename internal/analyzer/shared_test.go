package analyzer

import (
	"go/ast"
	"go/token"
	"go/types"
	"testing"
)

// ---------------------------------------------------------------------------
// IsHostCallsType
// ---------------------------------------------------------------------------

func TestIsHostCallsTypeReturnsTrueForInterface(t *testing.T) {
	fset := token.NewFileSet()
	result, err := LoadPackages("github.com/rcownie/cleat/testdata/basic", fset)
	if err != nil {
		t.Fatalf("LoadPackages failed: %v", err)
	}

	fd := result.Funcs["github.com/rcownie/cleat/testdata/basic.PlaceOrder"]
	if fd == nil {
		t.Fatal("could not find PlaceOrder function")
	}
	if fd.Type == nil || fd.Type.Params() == nil || fd.Type.Params().Len() == 0 {
		t.Fatal("PlaceOrder has no parameters")
	}

	paramType := fd.Type.Params().At(0).Type()
	if !IsHostCallsType(paramType) {
		t.Error("IsHostCallsType returned false for cleat.HostCalls interface type")
	}
}

func TestIsHostCallsTypeReturnsFalseForNonHostCallsType(t *testing.T) {
	fset := token.NewFileSet()
	result, err := LoadPackages("github.com/rcownie/cleat/testdata/basic", fset)
	if err != nil {
		t.Fatalf("LoadPackages failed: %v", err)
	}

	fd := result.Funcs["github.com/rcownie/cleat/testdata/basic.PlaceOrder"]
	if fd == nil {
		t.Fatal("could not find PlaceOrder function")
	}
	if fd.Type == nil || fd.Type.Params() == nil || fd.Type.Params().Len() < 2 {
		t.Fatal("PlaceOrder does not have enough parameters")
	}

	// Second parameter is string (userID).
	paramType := fd.Type.Params().At(1).Type()
	if IsHostCallsType(paramType) {
		t.Error("IsHostCallsType returned true for string type")
	}
}

func TestIsHostCallsTypeReturnsFalseForNil(t *testing.T) {
	if IsHostCallsType(nil) {
		t.Error("IsHostCallsType returned true for nil")
	}
}

func TestIsHostCallsTypeReturnsFalseForBasicType(t *testing.T) {
	intType := types.Typ[types.Int]
	if IsHostCallsType(intType) {
		t.Error("IsHostCallsType returned true for int")
	}
}

// ---------------------------------------------------------------------------
// ShortName
// ---------------------------------------------------------------------------

func TestShortNameExtractsCorrectName(t *testing.T) {
	tests := []struct {
		fqname string
		want   string
	}{
		{"pkg.Func", "Func"},
		{"github.com/rcownie/cleat/testdata/basic.PlaceOrder", "PlaceOrder"},
		{"basic.checkItemAvailability", "checkItemAvailability"},
		{"(*pkg.Type).Method", "Method"},
		{"single", "single"},
		{"", ""},
	}
	for _, tt := range tests {
		t.Run(tt.fqname, func(t *testing.T) {
			got := ShortName(tt.fqname)
			if got != tt.want {
				t.Errorf("ShortName(%q) = %q, want %q", tt.fqname, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// LastComponent
// ---------------------------------------------------------------------------

func TestLastComponentExtractsCorrectComponent(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"github.com/rcownie/cleat", "durable"},
		{"/usr/local/go", "go"},
		{"single", "single"},
		{"", ""},
		{"a/b/c/d", "d"},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got := LastComponent(tt.path)
			if got != tt.want {
				t.Errorf("LastComponent(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// ContainsNode
// ---------------------------------------------------------------------------

func TestContainsNodeFindsTarget(t *testing.T) {
	fset := token.NewFileSet()
	result, err := LoadPackages("github.com/rcownie/cleat/testdata/basic", fset)
	if err != nil {
		t.Fatalf("LoadPackages failed: %v", err)
	}

	fd := result.Funcs["github.com/rcownie/cleat/testdata/basic.checkItemAvailability"]
	if fd == nil {
		t.Fatal("could not find checkItemAvailability")
	}

	// Find a selector expression for DurableCall within the function body.
	var target ast.Node
	ast.Inspect(fd.Ast.Body, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "DurableCall" {
			target = n
			return false
		}
		return true
	})
	if target == nil {
		t.Fatal("could not find DurableCall selector in checkItemAvailability")
	}

	if !ContainsNode(fd.Ast.Body, target) {
		t.Error("ContainsNode should find DurableCall selector in function body")
	}
}

func TestContainsNodeReturnsFalseForMissingNode(t *testing.T) {
	fset := token.NewFileSet()
	result, err := LoadPackages("github.com/rcownie/cleat/testdata/basic", fset)
	if err != nil {
		t.Fatalf("LoadPackages failed: %v", err)
	}

	fd := result.Funcs["github.com/rcownie/cleat/testdata/basic.checkItemAvailability"]
	if fd == nil {
		t.Fatal("could not find checkItemAvailability")
	}

	// A node that doesn't exist in the function body.
	fakeNode := &ast.Ident{Name: "nonexistent"}

	if ContainsNode(fd.Ast.Body, fakeNode) {
		t.Error("ContainsNode should return false for a non-existent node")
	}
}

func TestContainsNodeHandlesNilTarget(t *testing.T) {
	fset := token.NewFileSet()
	result, err := LoadPackages("github.com/rcownie/cleat/testdata/basic", fset)
	if err != nil {
		t.Fatalf("LoadPackages failed: %v", err)
	}

	fd := result.Funcs["github.com/rcownie/cleat/testdata/basic.checkItemAvailability"]
	if fd == nil {
		t.Fatal("could not find checkItemAvailability")
	}

	// ast.Inspect calls its visitor with nil as a sentinel value, so
	// ContainsNode(root, nil) always matches. This is inherent to the
	// ast.Inspect contract — it does not indicate a bug.
	if !ContainsNode(fd.Ast.Body, nil) {
		t.Error("ContainsNode(root, nil) should return true because ast.Inspect emits nil")
	}
}

// ---------------------------------------------------------------------------
// FindEnclosingFuncName
// ---------------------------------------------------------------------------

func TestFindEnclosingFuncNameFindsEnclosingFunction(t *testing.T) {
	fset := token.NewFileSet()
	result, err := LoadPackages("github.com/rcownie/cleat/testdata/basic", fset)
	if err != nil {
		t.Fatalf("LoadPackages failed: %v", err)
	}

	fd := result.Funcs["github.com/rcownie/cleat/testdata/basic.checkItemAvailability"]
	if fd == nil {
		t.Fatal("could not find checkItemAvailability")
	}

	// Find a node inside checkItemAvailability.
	var target ast.Node
	ast.Inspect(fd.Ast.Body, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "DurableCall" {
				target = n
				return false
			}
		}
		return true
	})
	if target == nil {
		t.Fatal("could not find DurableCall call in checkItemAvailability")
	}

	name := FindEnclosingFuncName(result.TargetPkg.Files, target)
	if name != "checkItemAvailability" {
		t.Errorf("expected 'checkItemAvailability', got %q", name)
	}
}

func TestFindEnclosingFuncNameReturnsEmptyForMissingNode(t *testing.T) {
	fset := token.NewFileSet()
	result, err := LoadPackages("github.com/rcownie/cleat/testdata/basic", fset)
	if err != nil {
		t.Fatalf("LoadPackages failed: %v", err)
	}

	// A synthetic node that doesn't belong to any function.
	fakeNode := &ast.Ident{Name: "orphan"}

	name := FindEnclosingFuncName(result.TargetPkg.Files, fakeNode)
	if name != "" {
		t.Errorf("expected empty string, got %q", name)
	}
}
