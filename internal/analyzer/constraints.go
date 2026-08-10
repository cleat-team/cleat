package analyzer

import (
	"fmt"
	"go/build/constraint"
	"path/filepath"
	"strings"
)

// WASM build target constants.
const (
	WasmTargetGOOS   = "wasip1"
	WasmTargetGOARCH = "wasm"
)

// knownGOOS records GOOS values that act as build constraints.
// wasm is intentionally absent — in Go 1.21+ it is an architecture only.
var knownGOOS = map[string]bool{
	"aix":       true,
	"android":   true,
	"darwin":    true,
	"dragonfly": true,
	"freebsd":   true,
	"hurd":      true,
	"illumos":   true,
	"ios":       true,
	"js":        true,
	"linux":     true,
	"nacl":      true,
	"netbsd":    true,
	"openbsd":   true,
	"plan9":     true,
	"solaris":   true,
	"wasip1":    true,
	"windows":   true,
	"zos":       true,
}

// knownGOARCH records GOARCH values that act as build constraints.
var knownGOARCH = map[string]bool{
	"386":      true,
	"amd64":    true,
	"arm":      true,
	"arm64":    true,
	"loong64":  true,
	"mips":     true,
	"mipsle":   true,
	"mips64":   true,
	"mips64le": true,
	"ppc64":    true,
	"ppc64le":  true,
	"riscv64":  true,
	"s390x":    true,
	"wasm":     true,
}

// MatchWasmBuildConstraint checks whether a Go source file should be included
// when building for the WASM target (GOOS=wasip1 GOARCH=wasm). It evaluates
// both filename-based constraints (e.g., *_linux.go, *_amd64.go) and
// //go:build constraints embedded in the file content.
//
// filename is used for suffix-based constraint matching (e.g., foo_linux.go
// implies a GOOS=linux constraint). content is scanned for a //go:build line.
// Returns true if the file passes all constraints for the WASM target.
func MatchWasmBuildConstraint(filename string, content []byte) (bool, error) {
	return matchBuildConstraint(filename, content, WasmTargetGOOS, WasmTargetGOARCH)
}

// matchBuildConstraint is the internal implementation parameterised by target
// GOOS and GOARCH so it can be tested or reused for other targets.
func matchBuildConstraint(filename string, content []byte, goos, goarch string) (bool, error) {
	base := filepath.Base(filename)
	if !strings.HasSuffix(base, ".go") {
		// Non-Go files are not subject to Go build constraints.
		return true, nil
	}

	// Step 1: evaluate filename-based GOOS/GOARCH constraints.
	if ok, matched := matchFilenameSuffix(base, goos, goarch); matched {
		return ok, nil
	}

	// Step 2: evaluate //go:build constraints.
	if ok, found := matchGoBuildConstraint(string(content), goos, goarch); found {
		return ok, nil
	}

	// No constraint found — include by default.
	return true, nil
}

// matchFilenameSuffix checks whether the base name contains a trailing
// _GOOS, _GOARCH, or _GOOS_GOARCH suffix and, if so, whether the
// constraint is satisfied by the given target.
//
// The implementation mirrors the logic in go/build.(*Context).goodOSArchFile.
// It returns (match, found) where found is false when the name carries no
// filename-based constraint.
func matchFilenameSuffix(name, goos, goarch string) (bool, bool) {
	// Strip the .go extension.
	if dot := strings.LastIndex(name, "."); dot >= 0 {
		name = name[:dot]
	}

	// The first underscore must appear — a name with no underscore is not
	// constrained.  (This matches Go 1.4+ behaviour.)
	i := strings.Index(name, "_")
	if i < 0 {
		return false, false
	}
	suffix := name[i:] // keep the leading underscore so split is clean

	parts := strings.Split(suffix, "_")
	// parts[0] is empty (the leading underscore).
	// Drop trailing "_test".
	n := len(parts)
	if n > 1 && parts[n-1] == "test" {
		parts = parts[:n-1]
		n = len(parts)
	}

	if n >= 3 && knownGOOS[parts[n-2]] && knownGOARCH[parts[n-1]] {
		// _GOOS_GOARCH suffix: both must match.
		return parts[n-2] == goos && parts[n-1] == goarch, true
	}

	if n >= 2 {
		last := parts[n-1]
		if knownGOOS[last] || knownGOARCH[last] {
			// Single suffix: check against both GOOS and GOARCH.
			return last == goos || last == goarch, true
		}
	}

	return false, false
}

// matchGoBuildConstraint scans content for a //go:build line and evaluates
// it against the given target. Returns (match, found) where found is false
// when no //go:build line is present.
func matchGoBuildConstraint(content, goos, goarch string) (bool, bool) {
	line, found := extractGoBuildLine(content)
	if !found {
		return false, false
	}

	expr, err := constraint.Parse(line)
	if err != nil {
		// If we can't parse the constraint, err on the side of caution and
		// exclude the file.
		return false, true
	}

	result := expr.Eval(func(tag string) bool {
		return evalBuildTag(tag, goos, goarch)
	})
	return result, true
}

// extractGoBuildLine finds the first //go:build directive in content.
func extractGoBuildLine(content string) (string, bool) {
	const prefix = "//go:build"
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, prefix) {
			return trimmed, true
		}
	}
	return "", false
}

// evalBuildTag returns true when tag is satisfied by the given GOOS/GOARCH
// target. This implements the subset of matchTag logic (from
// go/build.(*Context).matchTag) that matters for the WASM target.
func evalBuildTag(tag, goos, goarch string) bool {
	if tag == goos || tag == goarch {
		return true
	}

	// Version tags — assume a modern Go compiler.
	if strings.HasPrefix(tag, "go1.") {
		return true
	}

	// wasip1 is not a Unix-like OS.
	if tag == "unix" {
		return false
	}

	// CGo is never available in WASM.
	if tag == "cgo" {
		return false
	}

	// Any other known GOOS or GOARCH does not match our target.
	if knownGOOS[tag] || knownGOARCH[tag] {
		return false
	}

	// Unknown tag — treat as unsatisfied (conservative).
	return false
}

// WasmFilenameWarnings inspects a .go filename and returns a non-empty
// suggestion string when the file is likely excluded by the compiler due
// to its suffix for the WASM target.
func WasmFilenameWarnings(filename string) []string {
	base := filepath.Base(filename)
	if !strings.HasSuffix(base, ".go") {
		return nil
	}

	var warns []string

	// Strip .go suffix.
	name := base[:len(base)-3]

	// Check for _linux suffix.
	if strings.HasSuffix(name, "_linux") {
		warns = append(warns, fmt.Sprintf(
			"%s has a _linux suffix — excluded by the compiler for GOOS=wasip1 (WASM target)", base))
	}

	// Check for _amd64 suffix.
	if strings.HasSuffix(name, "_amd64") {
		warns = append(warns, fmt.Sprintf(
			"%s has an _amd64 suffix — excluded by the compiler for GOARCH=wasm (WASM target)", base))
	}

	return warns
}

// FilenameConstrainedOut reports whether the given filename would be
// excluded for the WASM target based on its suffix alone.  It is used
// by the closure validator to skip functions in platform-specific files.
func FilenameConstrainedOut(filename string) bool {
	base := filepath.Base(filename)
	if !strings.HasSuffix(base, ".go") {
		return false
	}
	ok, matched := matchFilenameSuffix(base, WasmTargetGOOS, WasmTargetGOARCH)
	return matched && !ok
}
