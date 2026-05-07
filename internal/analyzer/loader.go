package analyzer

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"path/filepath"

	"golang.org/x/tools/go/packages"
)

// LoadMode is the set of package load flags needed for full analysis.
const LoadMode = packages.NeedName |
	packages.NeedFiles |
	packages.NeedCompiledGoFiles |
	packages.NeedTypes |
	packages.NeedTypesInfo |
	packages.NeedSyntax |
	packages.NeedDeps |
	packages.NeedImports |
	packages.NeedModule

// LoadPackages loads the packages matching the given pattern and returns
// the analysis result with all functions, types, and entry points identified.
func LoadPackages(pattern string, fset *token.FileSet) (*AnalysisResult, error) {
	cfg := &packages.Config{
		Mode:  LoadMode,
		Fset:  fset,
		Tests: false,
	}

	pkgs, err := packages.Load(cfg, pattern)
	if err != nil {
		return nil, fmt.Errorf("loading packages: %w", err)
	}

	if len(pkgs) == 0 {
		return nil, fmt.Errorf("no packages found matching %q", pattern)
	}

	// Check for package load errors.
	var loadErrs []error
	packages.Visit(pkgs, func(pkg *packages.Package) bool {
		for _, e := range pkg.Errors {
			loadErrs = append(loadErrs, fmt.Errorf("%s: %v", pkg.PkgPath, e))
		}
		return true
	}, nil)
	if len(loadErrs) > 0 {
		return nil, fmt.Errorf("package load errors: %v", loadErrs)
	}

	// Identify the target module.
	targetModule := ""
	moduleDir := ""
	if pkgs[0].Module != nil {
		targetModule = pkgs[0].Module.Path
		moduleDir = pkgs[0].Module.Dir
	}

	goVersion := ""
	if pkgs[0].Module != nil {
		goVersion = pkgs[0].Module.GoVersion
	}

	result := &AnalysisResult{
		Funcs:      make(map[string]*FuncDecl),
		ModulePath: targetModule,
		ModuleDir:  moduleDir,
		GoVersion:  goVersion,
	}

	for _, pkg := range pkgs {
		if pkg.Types == nil || pkg.TypesInfo == nil {
			continue
		}

		wp := &Package{
			Name:  pkg.Name,
			Path:  pkg.PkgPath,
			Dir:   pkgDir(pkg),
			Files: pkg.Syntax,
			Fset:  fset,
			Types: pkg.Types,
			Info:  pkg.TypesInfo,
		}

		if isUserPackage(pkg, targetModule) {
			result.UserPkgs = append(result.UserPkgs, wp)
		}

		if pkg.PkgPath == pkgs[0].PkgPath {
			result.TargetPkg = wp
		}

		// Collect function declarations.
		for _, file := range pkg.Syntax {
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok {
					continue
				}
				fd := newFuncDecl(fn, wp)
				fqname := fd.FullyQualifiedName()
				result.Funcs[fqname] = fd
				result.NumFuncs++
				if fd.IsExported {
					result.NumExported++
				}
			}
		}
	}

	// Detect entry points.
	for _, fd := range result.Funcs {
		if IsEntryPoint(fd) {
			fd.IsEntryPoint = true
			result.EntryPoints = append(result.EntryPoints, fd.FullyQualifiedName())
		}
	}

	if len(result.EntryPoints) == 0 {
		return nil, fmt.Errorf("no workflow entry points found in %s (entry points must be exported functions with cleat.HostCalls as first parameter)", pattern)
	}

	return result, nil
}

// newFuncDecl creates a FuncDecl from an AST function declaration.
func newFuncDecl(fn *ast.FuncDecl, pkg *Package) *FuncDecl {
	fd := &FuncDecl{
		Name:       fn.Name.Name,
		Pkg:        pkg,
		Ast:        fn,
		IsExported: fn.Name.IsExported(),
	}

	if pkg.Info != nil {
		if obj, ok := pkg.Info.Defs[fn.Name]; ok {
			if typeFunc, ok := obj.(*types.Func); ok {
				fd.Type = typeFunc.Type().(*types.Signature)
				if recv := fd.Type.Recv(); recv != nil {
					fd.RecvType = recv.Type()
				}
			}
		}
	}

	return fd
}

// IsEntryPoint checks if a function is a workflow entry point.
// It must be exported, not a method, and have cleat.HostCalls as
// its first parameter.
func IsEntryPoint(fd *FuncDecl) bool {
	if !fd.IsExported {
		return false
	}
	if fd.RecvType != nil {
		return false
	}
	if fd.Type == nil {
		return false
	}
	params := fd.Type.Params()
	if params == nil || params.Len() == 0 {
		return false
	}
	return IsHostCallsType(params.At(0).Type())
}

// isUserPackage returns true if the package belongs to the target module.
func isUserPackage(pkg *packages.Package, targetModule string) bool {
	if targetModule == "" {
		return true
	}
	return pkg.PkgPath == targetModule ||
		(len(pkg.PkgPath) > len(targetModule) &&
			pkg.PkgPath[:len(targetModule)] == targetModule &&
			pkg.PkgPath[len(targetModule)] == '/')
}

// pkgDir returns the directory of a package.
func pkgDir(pkg *packages.Package) string {
	if len(pkg.GoFiles) > 0 {
		return filepath.Dir(pkg.GoFiles[0])
	}
	return ""
}
