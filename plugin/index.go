//go:build !tinygo

package plugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/mod/semver"
	"gopkg.in/yaml.v3"
)

// IndexEntry represents a plugin entry in the index.
type IndexEntry struct {
	Name        string         `yaml:"name"`
	Description string         `yaml:"description"`
	Author      string         `yaml:"author"`
	Repository  string         `yaml:"repository,omitempty"`
	Versions    []IndexVersion `yaml:"versions"`
}

// IndexVersion represents a specific version in the index.
type IndexVersion struct {
	Version         string `yaml:"version"`
	WasmURL         string `yaml:"wasm_url"`
	ManifestURL     string `yaml:"manifest_url,omitempty"`
	Checksum        string `yaml:"checksum"`
	MinCleatVersion string `yaml:"min_cleat_version,omitempty"`
	Bundled         bool   `yaml:"bundled,omitempty"`
	Description     string `yaml:"description,omitempty"`

	// Signature is an optional Ed25519 hex-encoded signature of the WASM
	// binary SHA-256 checksum. When present and the plugin is official,
	// the signature is verified during deployment. The signing key is
	// identified by SigningKeyID.
	Signature string `yaml:"signature,omitempty"`

	// SigningKeyID identifies the Ed25519 public key used to produce
	// Signature. Example: "cleat-official-2026".
	SigningKeyID string `yaml:"signing_key_id,omitempty"`
}

// PluginIndex represents the parsed index.yaml file.
type PluginIndex struct {
	Plugins []IndexEntry `yaml:"plugins"`
}

// FetchIndex downloads and parses the plugin index from a URL or file path.
// Supports http://, https:// URLs and local file paths (absolute or relative).
func FetchIndex(ctx context.Context, urlStr string) (*PluginIndex, error) {
	var data []byte
	var err error

	if isHTTPURL(urlStr) {
		data, err = fetchURL(ctx, urlStr)
	} else {
		data, err = readLocalFile(urlStr)
	}
	if err != nil {
		return nil, fmt.Errorf("plugin index: %w", err)
	}

	var idx PluginIndex
	if err := yaml.Unmarshal(data, &idx); err != nil {
		return nil, fmt.Errorf("plugin index: parse YAML: %w", err)
	}

	if len(idx.Plugins) == 0 {
		return nil, fmt.Errorf("plugin index: no plugins found")
	}

	return &idx, nil
}

// Resolve finds the best matching version for a plugin name and constraint.
// Constraint can be empty (latest), "latest", or a semver constraint
// (^1.0.0, ~1.2.0, >=1.0.0, =1.0.0, or bare 1.0.0 treated as exact match).
func (idx *PluginIndex) Resolve(name, constraint string) (*IndexEntry, *IndexVersion, error) {
	var entry *IndexEntry
	for i := range idx.Plugins {
		if idx.Plugins[i].Name == name {
			entry = &idx.Plugins[i]
			break
		}
	}
	if entry == nil {
		return nil, nil, fmt.Errorf("plugin %q not found in index", name)
	}

	if len(entry.Versions) == 0 {
		return nil, nil, fmt.Errorf("plugin %q has no versions", name)
	}

	// No constraint or "latest" means pick the highest version.
	if constraint == "" || constraint == "latest" {
		var best *IndexVersion
		var bestVersion string
		for i := range entry.Versions {
			v := &entry.Versions[i]
			sv := ensureVPrefix(v.Version)
			if !semver.IsValid(sv) {
				continue
			}
			if best == nil || semver.Compare(sv, bestVersion) > 0 {
				best = v
				bestVersion = sv
			}
		}
		if best == nil {
			return nil, nil, fmt.Errorf("no valid versions for plugin %q", name)
		}
		return entry, best, nil
	}

	// Parse the constraint.
	cr, err := parseConstraint(constraint)
	if err != nil {
		return nil, nil, err
	}

	// Find the best (highest) matching version.
	var best *IndexVersion
	var bestVersion string

	for i := range entry.Versions {
		v := &entry.Versions[i]
		sv := ensureVPrefix(v.Version)
		if !semver.IsValid(sv) {
			continue
		}
		if cr.exact != "" {
			if sv == cr.exact {
				return entry, v, nil
			}
			continue
		}
		if !versionInRange(sv, cr) {
			continue
		}
		if best == nil || semver.Compare(sv, bestVersion) > 0 {
			best = v
			bestVersion = sv
		}
	}

	if best == nil {
		return nil, nil, fmt.Errorf("no matching version for plugin %q with constraint %q", name, constraint)
	}

	return entry, best, nil
}

// IsOfficial returns true if the plugin is a cleat official plugin.
// Official plugins use the "cleat/" prefix or have no slash in the name.
func (e *IndexEntry) IsOfficial() bool {
	return strings.HasPrefix(e.Name, "cleat/") || !strings.Contains(e.Name, "/")
}

// ---------------------------------------------------------------------------
// Download and checksum verification
// ---------------------------------------------------------------------------

// DownloadWASM downloads a WASM binary from the given URL.
func DownloadWASM(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("wasm download request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("wasm download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("wasm download: unexpected status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("wasm download read: %w", err)
	}
	return data, nil
}

// VerifyChecksum checks that the given data matches the expected SHA-256 checksum.
// If expectedChecksum is empty, verification is skipped.
func VerifyChecksum(data []byte, expectedChecksum string) error {
	if expectedChecksum == "" {
		return nil
	}
	sum := sha256.Sum256(data)
	actual := hex.EncodeToString(sum[:])
	if !strings.EqualFold(actual, expectedChecksum) {
		return fmt.Errorf("checksum mismatch: expected %s, got %s", expectedChecksum, actual)
	}
	return nil
}

// ---------------------------------------------------------------------------
// HTTP and file helpers
// ---------------------------------------------------------------------------

func isHTTPURL(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

func fetchURL(ctx context.Context, urlStr string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		return nil, fmt.Errorf("request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	return data, nil
}

func readLocalFile(path string) ([]byte, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve path: %w", err)
	}
	data, err := os.ReadFile(absPath)
	if err != nil {
		return nil, fmt.Errorf("read file: %w", err)
	}
	return data, nil
}

// ---------------------------------------------------------------------------
// Constraint parsing (mirrors host/plugin_loader.go for independent use)
// ---------------------------------------------------------------------------

type constraintRange struct {
	min   string // minimum version (inclusive), "v"-prefixed semver
	max   string // maximum version (exclusive), "v"-prefixed semver; empty = no upper bound
	exact string // exact version match; non-empty = exact only
}

func parseConstraint(c string) (constraintRange, error) {
	c = strings.TrimSpace(c)

	switch {
	case strings.HasPrefix(c, ">="):
		v := ensureVPrefix(strings.TrimPrefix(c, ">="))
		if !semver.IsValid(v) {
			return constraintRange{}, fmt.Errorf("invalid semver in constraint %q", c)
		}
		return constraintRange{min: v}, nil

	case strings.HasPrefix(c, "~"):
		v := ensureVPrefix(strings.TrimPrefix(c, "~"))
		if !semver.IsValid(v) {
			return constraintRange{}, fmt.Errorf("invalid semver in constraint %q", c)
		}
		major, minor, _ := splitSemver(v)
		return constraintRange{min: v, max: fmt.Sprintf("v%d.%d.0", major, minor+1)}, nil

	case strings.HasPrefix(c, "^"):
		v := ensureVPrefix(strings.TrimPrefix(c, "^"))
		if !semver.IsValid(v) {
			return constraintRange{}, fmt.Errorf("invalid semver in constraint %q", c)
		}
		major, _, _ := splitSemver(v)
		return constraintRange{min: v, max: fmt.Sprintf("v%d.0.0", major+1)}, nil

	case strings.HasPrefix(c, "="):
		v := ensureVPrefix(strings.TrimPrefix(c, "="))
		if !semver.IsValid(v) {
			return constraintRange{}, fmt.Errorf("invalid semver in constraint %q", c)
		}
		return constraintRange{exact: v}, nil

	default:
		// Bare version — treat as exact match.
		v := ensureVPrefix(c)
		if !semver.IsValid(v) {
			return constraintRange{}, fmt.Errorf("invalid semver version %q", c)
		}
		return constraintRange{exact: v}, nil
	}
}

func versionInRange(v string, r constraintRange) bool {
	if !semver.IsValid(v) {
		return false
	}
	if r.exact != "" {
		return v == r.exact
	}
	if r.min != "" && semver.Compare(v, r.min) < 0 {
		return false
	}
	if r.max != "" && semver.Compare(v, r.max) >= 0 {
		return false
	}
	return true
}

func ensureVPrefix(v string) string {
	if !strings.HasPrefix(v, "v") {
		return "v" + v
	}
	return v
}

func splitSemver(v string) (major, minor, patch int) {
	s := strings.TrimPrefix(v, "v")
	if idx := strings.Index(s, "-"); idx >= 0 {
		s = s[:idx]
	}
	parts := strings.Split(s, ".")
	if len(parts) > 0 {
		fmt.Sscanf(parts[0], "%d", &major)
	}
	if len(parts) > 1 {
		fmt.Sscanf(parts[1], "%d", &minor)
	}
	if len(parts) > 2 {
		fmt.Sscanf(parts[2], "%d", &patch)
	}
	return
}
