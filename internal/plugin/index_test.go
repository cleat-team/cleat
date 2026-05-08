package plugin

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestFetchIndexFromFile(t *testing.T) {
	dir := t.TempDir()
	indexPath := filepath.Join(dir, "index.yaml")
	content := `
plugins:
  - name: test-plugin
    description: A test plugin
    author: test
    versions:
      - version: 1.0.0
        wasm_url: https://example.com/plugin.wasm
        checksum: abc123
`
	if err := os.WriteFile(indexPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	idx, err := FetchIndex(context.Background(), indexPath)
	if err != nil {
		t.Fatalf("FetchIndex failed: %v", err)
	}
	if len(idx.Plugins) != 1 {
		t.Fatalf("expected 1 plugin, got %d", len(idx.Plugins))
	}
	if idx.Plugins[0].Name != "test-plugin" {
		t.Errorf("expected name test-plugin, got %q", idx.Plugins[0].Name)
	}
	if len(idx.Plugins[0].Versions) != 1 {
		t.Fatalf("expected 1 version, got %d", len(idx.Plugins[0].Versions))
	}
	if idx.Plugins[0].Versions[0].Version != "1.0.0" {
		t.Errorf("expected version 1.0.0, got %q", idx.Plugins[0].Versions[0].Version)
	}
}

func TestFetchIndexNonexistentFile(t *testing.T) {
	_, err := FetchIndex(context.Background(), "/nonexistent/path/index.yaml")
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}
}

func TestFetchIndexEmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.yaml")
	if err := os.WriteFile(path, []byte{}, 0644); err != nil {
		t.Fatal(err)
	}
	_, err := FetchIndex(context.Background(), path)
	if err == nil {
		t.Fatal("expected error for empty index")
	}
}

func TestResolveLatest(t *testing.T) {
	idx := &PluginIndex{
		Plugins: []IndexEntry{
			{
				Name: "test-plugin",
				Versions: []IndexVersion{
					{Version: "1.0.0", WasmURL: "https://example.com/1.0.0", Checksum: "a"},
					{Version: "1.1.0", WasmURL: "https://example.com/1.1.0", Checksum: "b"},
					{Version: "2.0.0", WasmURL: "https://example.com/2.0.0", Checksum: "c"},
				},
			},
		},
	}

	entry, ver, err := idx.Resolve("test-plugin", "")
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if entry.Name != "test-plugin" {
		t.Errorf("expected entry name test-plugin, got %q", entry.Name)
	}
	if ver.Version != "2.0.0" {
		t.Errorf("expected version 2.0.0, got %q", ver.Version)
	}
}

func TestResolveLatestKeyword(t *testing.T) {
	idx := &PluginIndex{
		Plugins: []IndexEntry{
			{
				Name: "test-plugin",
				Versions: []IndexVersion{
					{Version: "0.9.0", Checksum: "a"},
					{Version: "1.0.0", Checksum: "b"},
				},
			},
		},
	}

	_, ver, err := idx.Resolve("test-plugin", "latest")
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if ver.Version != "1.0.0" {
		t.Errorf("expected version 1.0.0, got %q", ver.Version)
	}
}

func TestResolveExact(t *testing.T) {
	idx := &PluginIndex{
		Plugins: []IndexEntry{
			{
				Name: "test-plugin",
				Versions: []IndexVersion{
					{Version: "1.0.0", Checksum: "a"},
					{Version: "1.1.0", Checksum: "b"},
				},
			},
		},
	}

	_, ver, err := idx.Resolve("test-plugin", "1.0.0")
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if ver.Version != "1.0.0" {
		t.Errorf("expected version 1.0.0, got %q", ver.Version)
	}
}

func TestResolveExactEquals(t *testing.T) {
	idx := &PluginIndex{
		Plugins: []IndexEntry{
			{
				Name: "test-plugin",
				Versions: []IndexVersion{
					{Version: "1.0.0", Checksum: "a"},
					{Version: "2.0.0", Checksum: "b"},
				},
			},
		},
	}

	_, ver, err := idx.Resolve("test-plugin", "=2.0.0")
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if ver.Version != "2.0.0" {
		t.Errorf("expected version 2.0.0, got %q", ver.Version)
	}
}

func TestResolveCaret(t *testing.T) {
	idx := &PluginIndex{
		Plugins: []IndexEntry{
			{
				Name: "test-plugin",
				Versions: []IndexVersion{
					{Version: "1.0.0", Checksum: "a"},
					{Version: "1.2.0", Checksum: "b"},
					{Version: "1.5.0", Checksum: "c"},
					{Version: "2.0.0", Checksum: "d"},
				},
			},
		},
	}

	// ^1.0.0 should match 1.x versions but not 2.0.0.
	_, ver, err := idx.Resolve("test-plugin", "^1.0.0")
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if ver.Version != "1.5.0" {
		t.Errorf("expected version 1.5.0, got %q", ver.Version)
	}
}

func TestResolveTilde(t *testing.T) {
	idx := &PluginIndex{
		Plugins: []IndexEntry{
			{
				Name: "test-plugin",
				Versions: []IndexVersion{
					{Version: "1.2.0", Checksum: "a"},
					{Version: "1.2.5", Checksum: "b"},
					{Version: "1.3.0", Checksum: "c"},
				},
			},
		},
	}

	// ~1.2.0 should match 1.2.x but not 1.3.0.
	_, ver, err := idx.Resolve("test-plugin", "~1.2.0")
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if ver.Version != "1.2.5" {
		t.Errorf("expected version 1.2.5, got %q", ver.Version)
	}
}

func TestResolveGreaterEqual(t *testing.T) {
	idx := &PluginIndex{
		Plugins: []IndexEntry{
			{
				Name: "test-plugin",
				Versions: []IndexVersion{
					{Version: "1.0.0", Checksum: "a"},
					{Version: "1.2.0", Checksum: "b"},
					{Version: "1.5.0", Checksum: "c"},
				},
			},
		},
	}

	_, ver, err := idx.Resolve("test-plugin", ">=1.2.0")
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if ver.Version != "1.5.0" {
		t.Errorf("expected version 1.5.0, got %q", ver.Version)
	}
}

func TestResolveNotFound(t *testing.T) {
	idx := &PluginIndex{
		Plugins: []IndexEntry{
			{
				Name:     "test-plugin",
				Versions: []IndexVersion{{Version: "1.0.0"}},
			},
		},
	}
	_, _, err := idx.Resolve("nonexistent", "")
	if err == nil {
		t.Fatal("expected error for nonexistent plugin")
	}
}

func TestResolveNoMatch(t *testing.T) {
	idx := &PluginIndex{
		Plugins: []IndexEntry{
			{
				Name: "test-plugin",
				Versions: []IndexVersion{
					{Version: "1.0.0", Checksum: "a"},
				},
			},
		},
	}
	_, _, err := idx.Resolve("test-plugin", "^2.0.0")
	if err == nil {
		t.Fatal("expected error for no matching version")
	}
}

func TestIsOfficial(t *testing.T) {
	tests := []struct {
		name     string
		official bool
	}{
		{name: "blobstore", official: true},
		{name: "cleat/llm", official: true},
		{name: "cleat/pgvector", official: true},
		{name: "acme/salesforce", official: false},
		{name: "community/my-plugin", official: false},
		{name: "org/name", official: false},
	}
	for _, tc := range tests {
		e := &IndexEntry{Name: tc.name}
		got := e.IsOfficial()
		if got != tc.official {
			t.Errorf("IsOfficial(%q) = %v, want %v", tc.name, got, tc.official)
		}
	}
}

func TestVerifyChecksum(t *testing.T) {
	data := []byte("hello world")
	// SHA-256 of "hello world".
	expected := "b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9"
	if err := VerifyChecksum(data, expected); err != nil {
		t.Errorf("expected no error, got: %v", err)
	}

	// Wrong checksum should fail.
	if err := VerifyChecksum(data, "0000000000000000000000000000000000000000000000000000000000000000"); err == nil {
		t.Error("expected error for wrong checksum")
	}

	// Empty checksum should pass (skip verification).
	if err := VerifyChecksum(data, ""); err != nil {
		t.Errorf("expected no error for empty checksum, got: %v", err)
	}
}

func TestParseConstraint(t *testing.T) {
	tests := []struct {
		input string
		exact string
		min   string
		max   string
		err   bool
	}{
		{input: "1.2.3", exact: "v1.2.3", min: "", max: ""},
		{input: "=1.2.3", exact: "v1.2.3", min: "", max: ""},
		{input: ">=1.2.3", exact: "", min: "v1.2.3", max: ""},
		{input: "^1.2.3", exact: "", min: "v1.2.3", max: "v2.0.0"},
		{input: "~1.2.3", exact: "", min: "v1.2.3", max: "v1.3.0"},
		{input: ">=0.9.0", exact: "", min: "v0.9.0", max: ""},
		{input: "^0.0.5", exact: "", min: "v0.0.5", max: "v1.0.0"},
		{input: "invalid", err: true},
		{input: ">=abc", err: true},
	}
	for _, tc := range tests {
		cr, err := parseConstraint(tc.input)
		if tc.err {
			if err == nil {
				t.Errorf("parseConstraint(%q): expected error, got nil", tc.input)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseConstraint(%q) failed: %v", tc.input, err)
			continue
		}
		if cr.exact != tc.exact {
			t.Errorf("parseConstraint(%q) exact = %q, want %q", tc.input, cr.exact, tc.exact)
		}
		if cr.min != tc.min {
			t.Errorf("parseConstraint(%q) min = %q, want %q", tc.input, cr.min, tc.min)
		}
		if cr.max != tc.max {
			t.Errorf("parseConstraint(%q) max = %q, want %q", tc.input, cr.max, tc.max)
		}
	}
}

func TestEnsureVPrefix(t *testing.T) {
	if got := ensureVPrefix("1.0.0"); got != "v1.0.0" {
		t.Errorf("expected v1.0.0, got %q", got)
	}
	if got := ensureVPrefix("v1.0.0"); got != "v1.0.0" {
		t.Errorf("expected v1.0.0, got %q", got)
	}
}

func TestSplitSemver(t *testing.T) {
	major, minor, patch := splitSemver("v1.2.3")
	if major != 1 || minor != 2 || patch != 3 {
		t.Errorf("expected 1.2.3, got %d.%d.%d", major, minor, patch)
	}

	major, minor, patch = splitSemver("v10.20.30-beta")
	if major != 10 || minor != 20 || patch != 30 {
		t.Errorf("expected 10.20.30, got %d.%d.%d", major, minor, patch)
	}
}

func TestVersionInRange(t *testing.T) {
	tests := []struct {
		v     string
		r     constraintRange
		in    bool
	}{
		{v: "v1.0.0", r: constraintRange{min: "v1.0.0", max: "v2.0.0"}, in: true},
		{v: "v1.5.0", r: constraintRange{min: "v1.0.0", max: "v2.0.0"}, in: true},
		{v: "v2.0.0", r: constraintRange{min: "v1.0.0", max: "v2.0.0"}, in: false},  // max exclusive
		{v: "v0.9.0", r: constraintRange{min: "v1.0.0", max: "v2.0.0"}, in: false},
		{v: "v1.0.0", r: constraintRange{exact: "v1.0.0"}, in: true},
		{v: "v1.1.0", r: constraintRange{exact: "v1.0.0"}, in: false},
	}
	for _, tc := range tests {
		got := versionInRange(tc.v, tc.r)
		if got != tc.in {
			t.Errorf("versionInRange(%q, %+v) = %v, want %v", tc.v, tc.r, got, tc.in)
		}
	}
}

func TestDownloadWASMInvalidURL(t *testing.T) {
	_, err := DownloadWASM(context.Background(), "http://nonexistent.example.com/plugin.wasm")
	if err == nil {
		t.Skip("expected error for invalid URL (may succeed in some network environments)")
	}
}

func TestIsOfficialEdgeCases(t *testing.T) {
	tests := []struct {
		name     string
		official bool
	}{
		{"", true},            // no slash, empty string — technically official
		{"cleat/", true},      // cleat/ prefix
		{"/name", false},      // starts with slash — contains slash but not cleat/
	}
	for _, tc := range tests {
		e := &IndexEntry{Name: tc.name}
		got := e.IsOfficial()
		if got != tc.official {
			t.Errorf("IsOfficial(%q) = %v, want %v", tc.name, got, tc.official)
		}
	}
}
