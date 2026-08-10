package engine

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestVendoredWasmtimeHeadersMatchModule guards the one hazard vendoring
// introduces.
//
// engine/wasmtimeinc holds a verbatim copy of the C headers from
// github.com/bytecodealliance/wasmtime-go/v44's build/include, because cgo
// cannot express an -I into another module (see component_cgo.go). Nothing
// links against those copies: the actual libwasmtime comes from wasmtime-go's
// own #cgo LDFLAGS, combined at final link time. So the headers here describe
// a library they do not come from, and a go.mod bump that is not accompanied
// by re-copying them would leave the two describing different ABIs -- struct
// layouts and enum values decided at compile time against headers for one
// version, called against a library built from another.
//
// That failure would not be a build error. It would be silent memory
// corruption at a cgo boundary, which is the worst debugging experience this
// repo could hand someone. Hence a test rather than a comment asking people
// to remember.
//
// It compares the full file set in both directions: a header that upstream
// added is as much a divergence as one whose contents changed, because
// component_cgo.go's #include lines resolve against this tree and a missing
// file would silently pick up a system header of the same name if one exists.
func TestVendoredWasmtimeHeadersMatchModule(t *testing.T) {
	vendored := "wasmtimeinc"

	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}",
		"github.com/bytecodealliance/wasmtime-go/v44").Output()
	if err != nil {
		// Deliberately not a skip. A skip is indistinguishable from a pass,
		// and this test exists precisely to be the thing that notices.
		// `go list -m` reads the module cache that `go test` already
		// populated to build this package, so a failure here is a broken
		// environment, not an absent one.
		t.Fatalf("locating the wasmtime-go module (needed to diff the vendored "+
			"headers in engine/%s against it): %v", vendored, err)
	}
	upstream := filepath.Join(strings.TrimSpace(string(out)), "build", "include")

	collect := func(root string) (map[string][]byte, error) {
		files := make(map[string][]byte)
		err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() || filepath.Ext(path) != ".h" {
				return nil
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			files[rel] = data
			return nil
		})
		return files, err
	}

	ours, err := collect(vendored)
	if err != nil {
		t.Fatalf("reading vendored headers at engine/%s: %v", vendored, err)
	}
	theirs, err := collect(upstream)
	if err != nil {
		t.Fatalf("reading upstream headers at %s: %v", upstream, err)
	}
	if len(ours) == 0 {
		t.Fatalf("no vendored headers found at engine/%s -- component_cgo.go's "+
			"-I points there and its #include lines would resolve against "+
			"system headers instead", vendored)
	}

	var missing, extra, differing []string
	for name, want := range theirs {
		got, ok := ours[name]
		if !ok {
			missing = append(missing, name)
			continue
		}
		if !bytes.Equal(got, want) {
			differing = append(differing, name)
		}
	}
	for name := range ours {
		if _, ok := theirs[name]; !ok {
			extra = append(extra, name)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	sort.Strings(differing)

	if len(missing) == 0 && len(extra) == 0 && len(differing) == 0 {
		return
	}
	const refresh = "engine/wasmtimeinc/README.md has the copy command; re-run it and commit the result."
	if len(differing) > 0 {
		t.Errorf("%d vendored header(s) differ from wasmtime-go's own copy, so "+
			"cgo is compiling against a different ABI than it links: %v\n%s",
			len(differing), differing, refresh)
	}
	if len(missing) > 0 {
		t.Errorf("%d header(s) present upstream but not vendored: %v\n%s",
			len(missing), missing, refresh)
	}
	if len(extra) > 0 {
		t.Errorf("%d vendored header(s) no longer exist upstream: %v\n%s",
			len(extra), extra, refresh)
	}
}
