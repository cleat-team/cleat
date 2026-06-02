package engine

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/cleat-team/cleat/plugin"
	"github.com/tetratelabs/wazero"
)

// ---------------------------------------------------------------------------
// PluginCallGuard tests
// ---------------------------------------------------------------------------

func TestPluginCallGuard_New(t *testing.T) {
	g := NewPluginCallGuard()
	if g == nil {
		t.Fatal("NewPluginCallGuard returned nil")
	}
}

func TestPluginCallGuard_AllowAndCheck(t *testing.T) {
	g := NewPluginCallGuard()

	// Caller not restricted → always allowed.
	if err := g.Check("unknown_caller", "any_plugin"); err != nil {
		t.Errorf("unrestricted caller should be allowed: %v", err)
	}

	// Restrict caller-a to only call plugin-b.
	g.Allow("caller-a", []string{"plugin-b"})

	// Allowed specific target.
	if err := g.Check("caller-a", "plugin-b"); err != nil {
		t.Errorf("allowed target should pass: %v", err)
	}

	// Denied target.
	if err := g.Check("caller-a", "plugin-c"); err == nil {
		t.Error("denied target should fail")
	}

	// Another caller still unrestricted.
	if err := g.Check("caller-b", "anything"); err != nil {
		t.Errorf("unrestricted caller-b should be allowed: %v", err)
	}
}

func TestPluginCallGuard_Wildcard(t *testing.T) {
	g := NewPluginCallGuard()
	g.Allow("super-plugin", []string{"*"})

	// Wildcard allows all targets.
	if err := g.Check("super-plugin", "any-plugin"); err != nil {
		t.Errorf("wildcard should allow any: %v", err)
	}
	if err := g.Check("super-plugin", "another-plugin"); err != nil {
		t.Errorf("wildcard should allow any: %v", err)
	}
}

func TestPluginCallGuard_CheckDeniedErrorMessage(t *testing.T) {
	g := NewPluginCallGuard()
	g.Allow("caller", []string{"allowed-target"})

	err := g.Check("caller", "forbidden-target")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "forbidden-target") {
		t.Errorf("error should mention the forbidden target: %v", err)
	}
	if !strings.Contains(err.Error(), "caller") {
		t.Errorf("error should mention the caller: %v", err)
	}
}

// ---------------------------------------------------------------------------
// ValidateVersionCompatibility tests
// ---------------------------------------------------------------------------

func TestValidateVersionCompatibility_NilDefs(t *testing.T) {
	oldDef := &WorkflowDef{Version: 1, MinVersion: 1, ABIVersion: 1}
	newDef := &WorkflowDef{Version: 2, MinVersion: 1, ABIVersion: 1}

	if err := ValidateVersionCompatibility(nil, newDef); err == nil {
		t.Error("expected error for nil oldDef")
	}
	if err := ValidateVersionCompatibility(oldDef, nil); err == nil {
		t.Error("expected error for nil newDef")
	}
}

func TestValidateVersionCompatibility_VersionMustIncrease(t *testing.T) {
	oldDef := &WorkflowDef{Version: 2, MinVersion: 1, ABIVersion: 1}

	tests := []struct {
		name    string
		newDef  *WorkflowDef
		wantErr string
	}{
		{"same version", &WorkflowDef{Version: 2, MinVersion: 1, ABIVersion: 1}, "must be greater"},
		{"lower version", &WorkflowDef{Version: 1, MinVersion: 1, ABIVersion: 1}, "must be greater"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateVersionCompatibility(oldDef, tt.newDef)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("got %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestValidateVersionCompatibility_BelowMinVersion(t *testing.T) {
	oldDef := &WorkflowDef{Version: 3, MinVersion: 5, ABIVersion: 1}
	// new version (4) > old version (3) ✓, but new version (4) < old.MinVersion (5) → error
	newDef := &WorkflowDef{Version: 4, MinVersion: 1, ABIVersion: 1}

	err := ValidateVersionCompatibility(oldDef, newDef)
	if err == nil || !strings.Contains(err.Error(), "below old version's MinVersion") {
		t.Errorf("expected MinVersion error, got: %v", err)
	}
}

func TestValidateVersionCompatibility_ABIMismatch(t *testing.T) {
	oldDef := &WorkflowDef{Version: 1, MinVersion: 1, ABIVersion: 1}
	newDef := &WorkflowDef{Version: 2, MinVersion: 1, ABIVersion: 2}

	err := ValidateVersionCompatibility(oldDef, newDef)
	if err == nil || !strings.Contains(err.Error(), "ABI version mismatch") {
		t.Errorf("expected ABI mismatch error, got: %v", err)
	}
}

func TestValidateVersionCompatibility_NewMinVersionTooHigh(t *testing.T) {
	oldDef := &WorkflowDef{Version: 1, MinVersion: 1, ABIVersion: 1}
	// old.Version(1) < new.MinVersion(3) → error
	newDef := &WorkflowDef{Version: 2, MinVersion: 3, ABIVersion: 1}

	err := ValidateVersionCompatibility(oldDef, newDef)
	if err == nil || !strings.Contains(err.Error(), "below new definition's MinVersion") {
		t.Errorf("expected MinVersion too high error, got: %v", err)
	}
}

func TestValidateVersionCompatibility_PluginDepMissing(t *testing.T) {
	oldDef := &WorkflowDef{
		Version:    1,
		MinVersion: 1,
		ABIVersion: 1,
		PluginDeps: map[string]string{"llm": "1.0.0"},
	}
	newDef := &WorkflowDef{
		Version:    2,
		MinVersion: 1,
		ABIVersion: 1,
		PluginDeps: map[string]string{}, // missing "llm"
	}

	err := ValidateVersionCompatibility(oldDef, newDef)
	if err == nil || !strings.Contains(err.Error(), "missing plugin dependency") {
		t.Errorf("expected missing plugin dep error, got: %v", err)
	}
}

func TestValidateVersionCompatibility_PluginVersionMismatch(t *testing.T) {
	oldDef := &WorkflowDef{
		Version:    1,
		MinVersion: 1,
		ABIVersion: 1,
		PluginDeps: map[string]string{"llm": "1.0.0"},
	}
	newDef := &WorkflowDef{
		Version:    2,
		MinVersion: 1,
		ABIVersion: 1,
		PluginDeps: map[string]string{"llm": "2.0.0"},
	}

	err := ValidateVersionCompatibility(oldDef, newDef)
	if err == nil || !strings.Contains(err.Error(), "version mismatch") {
		t.Errorf("expected version mismatch error, got: %v", err)
	}
}

func TestValidateVersionCompatibility_Success(t *testing.T) {
	oldDef := &WorkflowDef{
		Version:    1,
		MinVersion: 1,
		ABIVersion: 1,
		PluginDeps: map[string]string{"llm": "1.0.0", "db": "2.0.0"},
	}
	newDef := &WorkflowDef{
		Version:    2,
		MinVersion: 1,
		ABIVersion: 1,
		PluginDeps: map[string]string{"llm": "1.0.0", "db": "2.0.0"},
	}

	if err := ValidateVersionCompatibility(oldDef, newDef); err != nil {
		t.Errorf("expected success, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Plugin Resolver: parseVersion, matchesConstraint, compareTo
// ---------------------------------------------------------------------------

func TestParseVersion(t *testing.T) {
	tests := []struct {
		input   string
		want    semverVersion
		wantErr bool
	}{
		{"1.2.3", semverVersion{1, 2, 3}, false},
		{"v1.2.3", semverVersion{1, 2, 3}, false},
		{"V1.2.3", semverVersion{1, 2, 3}, false},
		{" 1.2.3", semverVersion{1, 2, 3}, false},
		{"0.0.0", semverVersion{0, 0, 0}, false},
		{"10.20.30", semverVersion{10, 20, 30}, false},
		{"1.2", semverVersion{}, true},         // need 3 parts
		{"abc.def.ghi", semverVersion{}, true}, // non-numeric
		// "1.2.3.4" parses as 1.2.3 (only first 3 parts used)
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := parseVersion(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Error("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestCompareTo(t *testing.T) {
	tests := []struct {
		a, b semverVersion
		want int
	}{
		{semverVersion{1, 0, 0}, semverVersion{1, 0, 0}, 0},
		{semverVersion{2, 0, 0}, semverVersion{1, 0, 0}, 1},
		{semverVersion{1, 0, 0}, semverVersion{2, 0, 0}, -1},
		{semverVersion{1, 2, 0}, semverVersion{1, 1, 0}, 1},
		{semverVersion{1, 1, 0}, semverVersion{1, 2, 0}, -1},
		{semverVersion{1, 1, 3}, semverVersion{1, 1, 2}, 1},
		{semverVersion{1, 1, 2}, semverVersion{1, 1, 3}, -1},
	}
	for _, tt := range tests {
		t.Run("", func(t *testing.T) {
			got := tt.a.compareTo(tt.b)
			if got != tt.want {
				t.Errorf("%+v.compareTo(%+v) = %d, want %d", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestMatchesConstraint(t *testing.T) {
	v := semverVersion{1, 4, 2}

	tests := []struct {
		constraint string
		want       bool
	}{
		{"=1.4.2", true},
		{"==1.4.2", true},
		{"=1.4.3", false},
		{"1.4.2", true},  // bare = exact
		{"1.4.3", false}, // bare = exact, no match
		{">=1.4.0", true},
		{">=1.5.0", false},
		{">1.4.0", true},
		{">1.4.2", false},
		{"<1.5.0", true},
		{"<1.4.0", false},
		{"<=1.4.2", true},
		{"<=1.4.1", false},
		// Patch-locked
		{"~1.4.0", true},
		{"~1.4.2", true},
		{"~1.5.0", false},
		{"~2.4.0", false},
		// Minor-locked
		{"^1.4.0", true},
		{"^1.3.0", true},
		{"^1.5.0", false},
		{"^2.0.0", false},
		// Invalid constraint
		{"invalid", false},
	}

	for _, tt := range tests {
		t.Run(tt.constraint, func(t *testing.T) {
			got := matchesConstraint(v, tt.constraint)
			if got != tt.want {
				t.Errorf("matchesConstraint(%+v, %q) = %v, want %v", v, tt.constraint, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Plugin Loader: parseConstraint, versionInRange, ensureVPrefix, splitSemver
// ---------------------------------------------------------------------------

func TestEnsureVPrefix(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"1.2.3", "v1.2.3"},
		{"v1.2.3", "v1.2.3"},
		{"0.0.0", "v0.0.0"},
		{"", "v"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := ensureVPrefix(tt.input)
			if got != tt.want {
				t.Errorf("ensureVPrefix(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestSplitSemver(t *testing.T) {
	tests := []struct {
		input     string
		wantMajor int
		wantMinor int
		wantPatch int
	}{
		{"v1.2.3", 1, 2, 3},
		{"v0.0.0", 0, 0, 0},
		{"v10.20.30", 10, 20, 30},
		{"1.2.3", 1, 2, 3},       // no v prefix
		{"v1.2.3-beta", 1, 2, 3}, // with pre-release
		{"v1.2", 1, 2, 0},        // partial
		{"v1", 1, 0, 0},          // major only
		{"", 0, 0, 0},            // empty
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			major, minor, patch := splitSemver(tt.input)
			if major != tt.wantMajor || minor != tt.wantMinor || patch != tt.wantPatch {
				t.Errorf("splitSemver(%q) = (%d,%d,%d), want (%d,%d,%d)",
					tt.input, major, minor, patch, tt.wantMajor, tt.wantMinor, tt.wantPatch)
			}
		})
	}
}

func TestParseConstraint(t *testing.T) {
	tests := []struct {
		input   string
		wantMin string
		wantMax string
		wantErr bool
	}{
		// Empty/wildcard
		{"", "v0.0.0", "", false},
		{"*", "v0.0.0", "", false},
		// >=
		{">=1.2.3", "v1.2.3", "", false},
		// ~ (patch-locked)
		{"~1.2.3", "v1.2.3", "v1.3.0", false},
		{"~0.1.0", "v0.1.0", "v0.2.0", false},
		// ^ (minor-locked)
		{"^1.2.3", "v1.2.3", "v2.0.0", false},
		{"^0.0.1", "v0.0.1", "v1.0.0", false},
		// = exact
		{"=1.2.3", "v1.2.3", "v1.2.3", false},
		// bare version
		{"1.2.3", "v1.2.3", "v1.2.3", false},
		// invalid
		{">=notasemver", "", "", true},
		{"invalid", "", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			r, err := parseConstraint(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if r.Min != tt.wantMin {
				t.Errorf("Min = %q, want %q", r.Min, tt.wantMin)
			}
			if r.Max != tt.wantMax {
				t.Errorf("Max = %q, want %q", r.Max, tt.wantMax)
			}
		})
	}
}

func TestVersionInRange(t *testing.T) {
	tests := []struct {
		version string
		r       constraintRange
		want    bool
	}{
		// In range: >=1.0.0 (no upper bound)
		{"v1.0.0", constraintRange{Min: "v1.0.0"}, true},
		{"v2.0.0", constraintRange{Min: "v1.0.0"}, true},
		{"v0.9.0", constraintRange{Min: "v1.0.0"}, false},
		// In range: >=1.0.0, <2.0.0 (Max is exclusive)
		{"v1.5.0", constraintRange{Min: "v1.0.0", Max: "v2.0.0"}, true},
		{"v1.9.9", constraintRange{Min: "v1.0.0", Max: "v2.0.0"}, true},
		{"v2.0.0", constraintRange{Min: "v1.0.0", Max: "v2.0.0"}, false}, // exclusive upper
		{"v0.9.0", constraintRange{Min: "v1.0.0", Max: "v2.0.0"}, false},
		// Exact match fails because Max is exclusive: Min=v Max=v means v < v is false
		{"v1.2.3", constraintRange{Min: "v1.2.3", Max: "v1.2.3"}, false},
		// Invalid semver
		{"invalid", constraintRange{Min: "v0.0.0"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.version, func(t *testing.T) {
			got := versionInRange(tt.version, tt.r)
			if got != tt.want {
				t.Errorf("versionInRange(%q, %+v) = %v, want %v", tt.version, tt.r, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Version Metrics: formatDuration, LogStaleAlerts
// ---------------------------------------------------------------------------

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		name string
		d    time.Duration
		want string
	}{
		{"30 seconds", 30 * time.Second, "30s"},
		{"59 seconds truncated", 59 * time.Second, "59s"},
		{"1 minute", 60 * time.Second, "1m"},
		{"1m30s truncated to 1m", 90 * time.Second, "1m"},
		{"5 minutes", 5 * time.Minute, "5m"},
		{"1h30m truncated to 1h", 90 * time.Minute, "1h"},
		{"3 hours", 3 * time.Hour, "3h"},
		{"25 hours = 1 day", 25 * time.Hour, "1d"},
		{"3 days", 72 * time.Hour, "3d"},
		{"zero", 0, "0s"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatDuration(tt.d)
			if got != tt.want {
				t.Errorf("formatDuration(%v) = %q, want %q", tt.d, got, tt.want)
			}
		})
	}
}

func TestLogStaleAlerts_Empty(t *testing.T) {
	// Should not panic or log anything.
	LogStaleAlerts(nil)
	LogStaleAlerts([]StaleVersionAlert{})
}

func TestLogStaleAlerts_WithAlerts(t *testing.T) {
	// Capture log output.
	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(orig)

	alerts := []StaleVersionAlert{
		{Name: "test-workflow", Version: 1, Message: "test alert"},
	}
	LogStaleAlerts(alerts)

	output := buf.String()
	if !strings.Contains(output, "test-workflow") {
		t.Errorf("log output should contain workflow name, got: %s", output)
	}
	if !strings.Contains(output, "test alert") {
		t.Errorf("log output should contain message, got: %s", output)
	}
}

// ---------------------------------------------------------------------------
// DefaultGCOptions
// ---------------------------------------------------------------------------

func TestDefaultGCOptions(t *testing.T) {
	opts := DefaultGCOptions()
	if opts.MinVersionsToKeep != DefaultMinVersionsToKeep {
		t.Errorf("MinVersionsToKeep = %d, want %d", opts.MinVersionsToKeep, DefaultMinVersionsToKeep)
	}
	if opts.MaxVersionAge != DefaultMaxVersionAge {
		t.Errorf("MaxVersionAge = %v, want %v", opts.MaxVersionAge, DefaultMaxVersionAge)
	}
	if opts.DryRun {
		t.Error("DryRun should be false by default")
	}
}

// ---------------------------------------------------------------------------
// FaultInjector tests (without a real database)
// ---------------------------------------------------------------------------

func TestFaultType_String(t *testing.T) {
	tests := []struct {
		ft   FaultType
		want string
	}{
		{FaultNetworkPartition, "network_partition"},
		{FaultDiskFull, "disk_full"},
		{FaultDiskSlow, "disk_slow"},
		{FaultClockSkew, "clock_skew"},
		{FaultWorkerCrash, "worker_crash"},
		{FaultType(99), "unknown(99)"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			got := tt.ft.String()
			if got != tt.want {
				t.Errorf("FaultType(%d).String() = %q, want %q", int(tt.ft), got, tt.want)
			}
		})
	}
}

func TestNewFaultInjector(t *testing.T) {
	fi := NewFaultInjector(nil)
	if fi == nil {
		t.Fatal("NewFaultInjector returned nil")
	}
	if fi.IsActive(FaultNetworkPartition) {
		t.Error("new injector should not be active")
	}
}

func TestFaultInjector_IsActiveAndActiveFaults(t *testing.T) {
	fi := NewFaultInjector(nil)

	// Initially nothing active.
	if fi.IsActive(FaultNetworkPartition) {
		t.Error("should not be active initially")
	}
	if len(fi.ActiveFaults()) != 0 {
		t.Error("ActiveFaults should be empty initially")
	}

	// Inject network partition (works without DB).
	fi.InjectNetworkPartition()
	if !fi.IsActive(FaultNetworkPartition) {
		t.Error("network partition should be active after InjectNetworkPartition")
	}

	faults := fi.ActiveFaults()
	found := false
	for _, ft := range faults {
		if ft == FaultNetworkPartition {
			found = true
		}
	}
	if !found {
		t.Error("ActiveFaults should include FaultNetworkPartition")
	}
}

func TestFaultInjector_InjectDiskLatency(t *testing.T) {
	fi := NewFaultInjector(nil)
	fi.InjectDiskLatency(10*time.Millisecond, 50*time.Millisecond)
	if !fi.IsActive(FaultDiskSlow) {
		t.Error("disk slow should be active after InjectDiskLatency")
	}
}

func TestFaultInjector_InjectNetworkPartitionTwice(t *testing.T) {
	fi := NewFaultInjector(nil)
	fi.InjectNetworkPartition()
	fi.InjectNetworkPartition() // second call should be a no-op
	if !fi.IsActive(FaultNetworkPartition) {
		t.Error("should still be active")
	}
}

func TestFaultInjector_Cleanup(t *testing.T) {
	fi := NewFaultInjector(nil)
	fi.InjectNetworkPartition()
	fi.InjectDiskLatency(10*time.Millisecond, 50*time.Millisecond)

	fi.Cleanup()
	if fi.IsActive(FaultNetworkPartition) {
		t.Error("should not be active after Cleanup")
	}
	if fi.IsActive(FaultDiskSlow) {
		t.Error("should not be active after Cleanup")
	}
	if len(fi.ActiveFaults()) != 0 {
		t.Error("ActiveFaults should be empty after Cleanup")
	}
}

func TestFaultInjector_Reset(t *testing.T) {
	fi := NewFaultInjector(nil)
	fi.InjectNetworkPartition()
	fi.Reset()
	if fi.IsActive(FaultNetworkPartition) {
		t.Error("should not be active after Reset")
	}
	if len(fi.ActiveFaults()) != 0 {
		t.Error("ActiveFaults should be empty after Reset")
	}
}

func TestFaultInjector_Context(t *testing.T) {
	fi := NewFaultInjector(nil)
	ctx := context.Background()

	// Without active partition, context should be unchanged.
	gotCtx := fi.Context(ctx)
	if gotCtx != ctx {
		t.Error("Context should return original context when no partition active")
	}

	// With active partition, context should be cancelled.
	fi.InjectNetworkPartition()
	partitionCtx := fi.Context(ctx)
	if partitionCtx.Err() == nil {
		t.Error("partition context should be cancelled")
	}
}

// ---------------------------------------------------------------------------
// Runtime: Stdout, Stderr getters
// ---------------------------------------------------------------------------

func TestRuntime_StdoutStderr(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	// Initially empty.
	if rt.Stdout() != "" {
		t.Errorf("Stdout should be empty initially, got: %q", rt.Stdout())
	}
	if rt.Stderr() != "" {
		t.Errorf("Stderr should be empty initially, got: %q", rt.Stderr())
	}
}

func TestRuntime_InstantiateAndInit(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	// minimalWasm has no _start, so InitModule is a no-op.
	mod, err := rt.InstantiateAndInit(ctx, minimalWasm())
	if err != nil {
		t.Fatalf("InstantiateAndInit: %v", err)
	}
	defer mod.Close(ctx)

	if mod == nil {
		t.Fatal("InstantiateAndInit returned nil module")
	}
}

// ---------------------------------------------------------------------------
// WorkflowLoader: NewWorkflowLoader, CacheStats
// ---------------------------------------------------------------------------

func TestNewWorkflowLoader_Defaults(t *testing.T) {
	l := NewWorkflowLoader(nil, nil, nil)
	if l == nil {
		t.Fatal("NewWorkflowLoader returned nil")
	}
	stats := l.CacheStats()
	if stats.Size != 0 {
		t.Errorf("expected empty cache, got size %d", stats.Size)
	}
	if stats.MaxSize != 100 {
		t.Errorf("expected MaxSize 100, got %d", stats.MaxSize)
	}
	if stats.Hits != 0 || stats.Misses != 0 {
		t.Errorf("expected zero stats, got hits=%d misses=%d", stats.Hits, stats.Misses)
	}
}

func TestNewWorkflowLoader_CustomMaxSize(t *testing.T) {
	l := NewWorkflowLoader(nil, nil, nil, 42)
	stats := l.CacheStats()
	if stats.MaxSize != 42 {
		t.Errorf("expected MaxSize 42, got %d", stats.MaxSize)
	}
}

func TestNewWorkflowLoader_ZeroMaxSize(t *testing.T) {
	l := NewWorkflowLoader(nil, nil, nil, 0)
	stats := l.CacheStats()
	if stats.MaxSize != 100 {
		t.Errorf("zero maxSize should default to 100, got %d", stats.MaxSize)
	}
}

// ---------------------------------------------------------------------------
// PluginLoader: NewPluginLoader
// ---------------------------------------------------------------------------

func TestNewPluginLoader_Defaults(t *testing.T) {
	l := NewPluginLoader(nil, nil)
	if l == nil {
		t.Fatal("NewPluginLoader returned nil")
	}
}

func TestNewPluginLoader_CustomMaxSize(t *testing.T) {
	l := NewPluginLoader(nil, nil, 10)
	if l == nil {
		t.Fatal("NewPluginLoader returned nil")
	}
}

func TestNewPluginLoader_ZeroMaxSize(t *testing.T) {
	l := NewPluginLoader(nil, nil, 0)
	if l == nil {
		t.Fatal("NewPluginLoader returned nil")
	}
}

// ---------------------------------------------------------------------------
// Additional FaultInjector tests
// ---------------------------------------------------------------------------

func TestFaultInjector_InjectClockSkewNilDB(t *testing.T) {
	fi := NewFaultInjector(nil)
	fi.InjectClockSkew(5 * time.Minute)
	if !fi.IsActive(FaultClockSkew) {
		t.Error("clock skew should be active after InjectClockSkew")
	}
}

func TestFaultInjector_InjectNegativeClockSkew(t *testing.T) {
	fi := NewFaultInjector(nil)
	fi.InjectClockSkew(-10 * time.Minute)
	if !fi.IsActive(FaultClockSkew) {
		t.Error("clock skew should be active with negative offset")
	}
}

func TestFaultInjector_InjectWorkerCrashNilDB(t *testing.T) {
	fi := NewFaultInjector(nil)
	fi.InjectWorkerCrash("worker-1")
	if !fi.IsActive(FaultWorkerCrash) {
		t.Error("worker crash should be active after InjectWorkerCrash")
	}
}

func TestFaultInjector_CleanupAllFaultTypes(t *testing.T) {
	fi := NewFaultInjector(nil)

	// Inject all fault types that work with nil DB.
	fi.InjectNetworkPartition()
	fi.InjectDiskLatency(10*time.Millisecond, 50*time.Millisecond)
	fi.InjectClockSkew(5 * time.Minute)
	fi.InjectWorkerCrash("worker-1")

	if len(fi.ActiveFaults()) != 4 {
		t.Errorf("expected 4 active faults, got %d", len(fi.ActiveFaults()))
	}

	fi.Cleanup()

	if len(fi.ActiveFaults()) != 0 {
		t.Errorf("expected 0 active faults after Cleanup, got %d", len(fi.ActiveFaults()))
	}
	if fi.IsActive(FaultNetworkPartition) {
		t.Error("network partition should be inactive after Cleanup")
	}
	if fi.IsActive(FaultDiskSlow) {
		t.Error("disk slow should be inactive after Cleanup")
	}
	if fi.IsActive(FaultClockSkew) {
		t.Error("clock skew should be inactive after Cleanup")
	}
	if fi.IsActive(FaultWorkerCrash) {
		t.Error("worker crash should be inactive after Cleanup")
	}
}

// ---------------------------------------------------------------------------
// Additional Runtime tests: InitModule with no _start
// ---------------------------------------------------------------------------

func TestRuntime_InitModuleNoStart(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	compiled, err := rt.CompileModule(ctx, minimalWasm())
	if err != nil {
		t.Fatalf("CompileModule: %v", err)
	}
	defer compiled.Close(ctx)

	mod, err := rt.InstantiateModule(ctx, compiled)
	if err != nil {
		t.Fatalf("InstantiateModule: %v", err)
	}
	defer mod.Close(ctx)

	// InitModule with a module that has no _start should be a no-op.
	if err := rt.InitModule(ctx, mod); err != nil {
		t.Errorf("InitModule on module without _start: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Additional parseConstraint edge cases
// ---------------------------------------------------------------------------

func TestParseConstraint_InvalidEdgeCases(t *testing.T) {
	tests := []string{
		"~notasemver",
		"^notasemver",
		"=notasemver",
	}
	for _, input := range tests {
		t.Run(input, func(t *testing.T) {
			_, err := parseConstraint(input)
			if err == nil {
				t.Error("expected error")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// PluginLoader: SetLimits
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// ResolvePlugins tests (with mock DB)
// ---------------------------------------------------------------------------

var multiRowDBState struct {
	mu      sync.Mutex
	counter int
}

// multiRowDB creates a *sql.DB backed by a mock that returns multiple rows
// for QueryContext. Each call registers a unique driver name so parallel
// tests do not share driver state.
func multiRowDB(rowsData [][]driver.Value, queryRowFunc func(query string) []driver.Value) *sql.DB {
	multiRowDBState.mu.Lock()
	multiRowDBState.counter++
	name := fmt.Sprintf("multirow_mock_%d", multiRowDBState.counter)
	multiRowDBState.mu.Unlock()
	mu := &multiRowMock{rowsData: rowsData, queryRowFunc: queryRowFunc}
	sql.Register(name, mu)
	db, err := sql.Open(name, "")
	if err != nil {
		panic(err)
	}
	return db
}

type multiRowMock struct {
	driver.Driver
	rowsData     [][]driver.Value
	queryRowFunc func(query string) []driver.Value
}

type multiRowConn struct {
	driver.Conn
	m *multiRowMock
}

func (m *multiRowMock) Open(name string) (driver.Conn, error) {
	return &multiRowConn{m: m}, nil
}

func (c *multiRowConn) Prepare(query string) (driver.Stmt, error) {
	return &multiRowStmt{conn: c, query: query}, nil
}

func (c *multiRowConn) Close() error { return nil }
func (c *multiRowConn) Begin() (driver.Tx, error) {
	return &multiRowTx{}, nil
}

type multiRowTx struct{ driver.Tx }

func (t *multiRowTx) Commit() error   { return nil }
func (t *multiRowTx) Rollback() error { return nil }

type multiRowStmt struct {
	conn  *multiRowConn
	query string
}

func (s *multiRowStmt) Close() error  { return nil }
func (s *multiRowStmt) NumInput() int { return -1 }

func (s *multiRowStmt) Exec(args []driver.Value) (driver.Result, error) {
	return &multiRowResult{}, nil
}

type multiRowResult struct{ driver.Result }

func (r *multiRowResult) LastInsertId() (int64, error) { return 0, nil }
func (r *multiRowResult) RowsAffected() (int64, error) { return 0, nil }

func (s *multiRowStmt) Query(args []driver.Value) (driver.Rows, error) {
	if len(s.conn.m.rowsData) > 0 {
		cols := make([]string, len(s.conn.m.rowsData[0]))
		for i := range cols {
			cols[i] = "version"
		}
		return &multirowRows{cols: cols, data: s.conn.m.rowsData}, nil
	}
	return &multirowRows{}, nil
}

type multirowRows struct {
	driver.Rows
	cols []string
	data [][]driver.Value
	pos  int
}

func (r *multirowRows) Columns() []string { return r.cols }
func (r *multirowRows) Close() error      { return nil }
func (r *multirowRows) Next(dest []driver.Value) error {
	if r.pos >= len(r.data) {
		return io.EOF
	}
	copy(dest, r.data[r.pos])
	r.pos++
	return nil
}

func TestResolvePlugins_EmptyJSON(t *testing.T) {
	ctx := context.Background()
	result, err := ResolvePlugins(ctx, nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 0 {
		t.Errorf("expected empty result, got %v", result)
	}

	result, err = ResolvePlugins(ctx, nil, "{}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 0 {
		t.Errorf("expected empty result, got %v", result)
	}
}

func TestResolvePlugins_ArrayFormat(t *testing.T) {
	// Test that ResolvePlugins handles array format JSON by providing a mock DB
	// with rows for the referenced plugin.
	rows := [][]driver.Value{
		{"1.0.0"},
		{"2.0.0"},
	}
	db := multiRowDB(rows, nil)
	ctx := context.Background()
	result, err := ResolvePlugins(ctx, db, `[{"name":"llm","constraint":"=1.0.0"}]`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 result, got %d", len(result))
	}
}

func TestResolvePlugins_InvalidJSON(t *testing.T) {
	ctx := context.Background()
	_, err := ResolvePlugins(ctx, nil, `{{{invalid}}`)
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestResolvePlugins_MockDB(t *testing.T) {
	// Create a mock DB that returns some version rows for plugin "llm".
	// ORDER BY created_at DESC means newest first.
	rows := [][]driver.Value{
		{"2.0.0"},
		{"1.5.0"},
		{"1.0.0"},
	}
	db := multiRowDB(rows, nil)
	ctx := context.Background()

	result, err := ResolvePlugins(ctx, db, `{"llm": ">=1.5.0"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 result, got %d", len(result))
	}
	// Should return the first matching version (ordered by created_at DESC).
	// "2.0.0" is the newest, matches >=1.5.0.
	got, ok := result["llm"]
	if !ok {
		t.Fatal("expected result for 'llm'")
	}
	if got != "2.0.0" {
		t.Errorf("expected '2.0.0', got %q", got)
	}
}

func TestResolvePlugins_MockDBNonMatchingConstraint(t *testing.T) {
	// Plugin exists but no version satisfies the constraint.
	rows := [][]driver.Value{
		{"1.5.0"},
		{"1.0.0"},
	}
	db := multiRowDB(rows, nil)
	ctx := context.Background()

	_, err := ResolvePlugins(ctx, db, `{"llm": ">=2.0.0"}`)
	if err == nil {
		t.Error("expected error when no version satisfies constraint")
	}
}

func TestPluginLoader_SetLimits(t *testing.T) {
	l := NewPluginLoader(nil, nil)
	// SetLimits with zero value (IsSet() = false) should not panic.
	l.SetLimits(plugin.CapabilityLimits{})
	if l == nil {
		t.Fatal("loader should not be nil after SetLimits")
	}
}

// ---------------------------------------------------------------------------
// FaultInjector: applyLatencySleep (no-op with no active latency)
// ---------------------------------------------------------------------------

func TestFaultInjector_ApplyLatencySleepNoop(t *testing.T) {
	fi := NewFaultInjector(nil)
	// Just ensure it doesn't panic when no latency is configured.
	// This is a no-op, so we just test it runs.
	fi.applyLatencySleep()
}

func TestFaultInjector_ApplyLatencySleepWithConfig(t *testing.T) {
	fi := NewFaultInjector(nil)
	fi.InjectDiskLatency(1*time.Millisecond, 2*time.Millisecond)
	// Should sleep briefly without panicking.
	fi.applyLatencySleep()
}

// ---------------------------------------------------------------------------
// Memory utility functions
// ---------------------------------------------------------------------------

func TestMinU32(t *testing.T) {
	tests := []struct {
		a, b uint32
		want uint32
	}{
		{5, 10, 5},
		{10, 5, 5},
		{0, 100, 0},
		{100, 0, 0},
		{42, 42, 42},
	}
	for _, tt := range tests {
		got := minU32(tt.a, tt.b)
		if got != tt.want {
			t.Errorf("minU32(%d, %d) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// WorkflowLoader LRU cache tests
// ---------------------------------------------------------------------------

func TestWorkflowLoader_CachePutAndGet(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	compiled, err := rt.CompileModule(ctx, minimalWasm())
	if err != nil {
		t.Fatalf("CompileModule: %v", err)
	}
	defer compiled.Close(ctx)

	l := NewWorkflowLoader(nil, rt, nil, 10)

	key := defKey{Name: "test-wf", Version: 1}

	// cacheGet on empty cache should miss and return nil.
	got, ok := l.cacheGet(key)
	if ok {
		t.Error("expected cache miss for empty cache")
	}
	if got != nil {
		t.Error("expected nil module on miss")
	}

	// cachePut should insert.
	l.cachePut(key, compiled)

	// cacheGet should now hit.
	got, ok = l.cacheGet(key)
	if !ok {
		t.Fatal("expected cache hit after put")
	}
	if got == nil {
		t.Fatal("expected non-nil module")
	}

	stats := l.CacheStats()
	if stats.Size != 1 {
		t.Errorf("expected size 1, got %d", stats.Size)
	}
}

func TestWorkflowLoader_CacheRemove(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	compiled, err := rt.CompileModule(ctx, minimalWasm())
	if err != nil {
		t.Fatalf("CompileModule: %v", err)
	}

	l := NewWorkflowLoader(nil, rt, nil, 10)

	key := defKey{Name: "test-wf", Version: 1}
	l.cachePut(key, compiled)

	// Remove should succeed.
	l.cacheRemove(key)

	_, ok := l.cacheGet(key)
	if ok {
		t.Error("expected cache miss after remove")
	}

	// Remove non-existent key should not panic.
	l.cacheRemove(defKey{Name: "nonexistent", Version: 999})
}

func TestWorkflowLoader_CacheEviction(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	// Max size 1 means the second insert should evict the first.
	l := NewWorkflowLoader(nil, rt, nil, 1)

	mod1, err := rt.CompileModule(ctx, minimalWasm())
	if err != nil {
		t.Fatalf("CompileModule mod1: %v", err)
	}

	key1 := defKey{Name: "wf-1", Version: 1}
	l.cachePut(key1, mod1)

	if stats := l.CacheStats(); stats.Size != 1 {
		t.Errorf("expected size 1 after first insert, got %d", stats.Size)
	}

	mod2, err := rt.CompileModule(ctx, minimalWasm())
	if err != nil {
		t.Fatalf("CompileModule mod2: %v", err)
	}

	key2 := defKey{Name: "wf-2", Version: 1}
	l.cachePut(key2, mod2)

	// After second insert, cache should still be size 1 (evicted first).
	if stats := l.CacheStats(); stats.Size != 1 {
		t.Errorf("expected size 1 after eviction, got %d", stats.Size)
	}

	// key1 should be evicted (LRU).
	_, ok := l.cacheGet(key1)
	if ok {
		t.Error("expected key1 to be evicted")
	}

	// key2 should still be present.
	_, ok = l.cacheGet(key2)
	if !ok {
		t.Error("expected key2 to be present after eviction")
	}
}

func TestWorkflowLoader_CacheLRUPromotion(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	// Max size 2.
	l := NewWorkflowLoader(nil, rt, nil, 2)

	mods := make([]wazero.CompiledModule, 3)
	for i := 0; i < 3; i++ {
		m, err := rt.CompileModule(ctx, minimalWasm())
		if err != nil {
			t.Fatalf("CompileModule mod%d: %v", i, err)
		}
		mods[i] = m
	}

	key1 := defKey{Name: "wf-1", Version: 1}
	key2 := defKey{Name: "wf-2", Version: 1}
	key3 := defKey{Name: "wf-3", Version: 1}

	// Insert two entries.
	l.cachePut(key1, mods[0])
	l.cachePut(key2, mods[1])

	// Access key1 to promote it to front.
	l.cacheGet(key1)

	// Insert key3 — should evict key2 (LRU), not key1 (most recently used).
	l.cachePut(key3, mods[2])

	// key1 should survive.
	_, ok1 := l.cacheGet(key1)
	if !ok1 {
		t.Error("expected key1 to survive (it was promoted)")
	}

	// key3 should be present.
	_, ok3 := l.cacheGet(key3)
	if !ok3 {
		t.Error("expected key3 to be present")
	}
}

func TestWorkflowLoader_EvictLockedEmptyCache(t *testing.T) {
	l := NewWorkflowLoader(nil, nil, nil, 10)
	// Evict on empty cache should not panic.
	l.evictLocked()
}

func TestWorkflowLoader_CacheUpdateExisting(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	mod1, err := rt.CompileModule(ctx, minimalWasm())
	if err != nil {
		t.Fatalf("CompileModule mod1: %v", err)
	}

	mod2, err := rt.CompileModule(ctx, minimalWasm())
	if err != nil {
		t.Fatalf("CompileModule mod2: %v", err)
	}

	l := NewWorkflowLoader(nil, rt, nil, 10)
	key := defKey{Name: "wf", Version: 1}

	// Put first module.
	l.cachePut(key, mod1)
	stats1 := l.CacheStats()
	if stats1.Size != 1 {
		t.Errorf("expected size 1, got %d", stats1.Size)
	}

	// Put second module with same key — should update in place, not add.
	l.cachePut(key, mod2)
	stats2 := l.CacheStats()
	if stats2.Size != 1 {
		t.Errorf("expected size still 1 after update, got %d", stats2.Size)
	}
}

// ---------------------------------------------------------------------------
// SuspendError.Error() test
// ---------------------------------------------------------------------------

func TestSuspendError_Error(t *testing.T) {
	// Without Until set.
	e1 := &SuspendError{Reason: "workflow suspended for signal"}
	got1 := e1.Error()
	want1 := "cleat: suspend: workflow suspended for signal"
	if got1 != want1 {
		t.Errorf("SuspendError.Error() = %q, want %q", got1, want1)
	}

	// With Until set.
	until := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	e2 := &SuspendError{Reason: "sleeping", Until: until}
	got2 := e2.Error()
	want2 := "cleat: suspend until 2026-05-10 12:00:00 +0000 UTC: sleeping"
	if got2 != want2 {
		t.Errorf("SuspendError.Error() = %q, want %q", got2, want2)
	}

	// Zero Until (time.Time{} has IsZero() true).
	zeroTime := time.Time{}
	e3 := &SuspendError{Reason: "no until", Until: zeroTime}
	got3 := e3.Error()
	want3 := "cleat: suspend: no until"
	if got3 != want3 {
		t.Errorf("SuspendError.Error() with zero Until = %q, want %q", got3, want3)
	}
}

// ---------------------------------------------------------------------------
// Mock WorkflowStore for version metrics and GC tests
// ---------------------------------------------------------------------------

// stubWorkflowStore implements WorkflowStore with all methods returning zero
// values. Embed this in a more specific mock and override only the methods
// you need for a given test.
type stubWorkflowStore struct{}

func (s *stubWorkflowStore) ClaimWorkflow(ctx context.Context, workerID string) (*WorkflowInstance, error) {
	return nil, nil
}
func (s *stubWorkflowStore) ClaimWorkflows(ctx context.Context, workerID string, limit int) ([]*WorkflowInstance, error) {
	return nil, nil
}
func (s *stubWorkflowStore) ClaimStickyWorkflows(ctx context.Context, workerID string, limit int) ([]*WorkflowInstance, error) {
	return nil, nil
}
func (s *stubWorkflowStore) LoadEventHistory(ctx context.Context, workflowID string) ([]EventRecord, error) {
	return nil, nil
}
func (s *stubWorkflowStore) AppendEventHistory(ctx context.Context, workflowID string, rec EventRecord) error {
	return nil
}
func (s *stubWorkflowStore) AppendEventHistoryBatch(ctx context.Context, workflowID string, recs []EventRecord) error {
	return nil
}
func (s *stubWorkflowStore) LoadWASM(ctx context.Context, defName string, defVersion int) ([]byte, error) {
	return nil, nil
}
func (s *stubWorkflowStore) GetWASMLength(ctx context.Context, defName string, defVersion int) (int64, error) {
	return 0, nil
}
func (s *stubWorkflowStore) ListVersions(ctx context.Context, defName string) ([]int, error) {
	return nil, nil
}
func (s *stubWorkflowStore) Heartbeat(ctx context.Context, workflowID, workerID string, generation int64) (bool, error) {
	return false, nil
}
func (s *stubWorkflowStore) CompleteWorkflow(ctx context.Context, workflowID, workerID string, generation int64, result string, queryState map[string]string) error {
	return nil
}
func (s *stubWorkflowStore) FailWorkflow(ctx context.Context, workflowID, workerID string, generation int64, errMsg, errorCode, errorOp string, queryState map[string]string) error {
	return nil
}
func (s *stubWorkflowStore) ReleaseWorkflow(ctx context.Context, workflowID, workerID string, generation int64, nextWakeAt time.Time) error {
	return nil
}
func (s *stubWorkflowStore) RequestCancellation(ctx context.Context, workflowID, reason string) error {
	return nil
}
func (s *stubWorkflowStore) CheckCancellation(ctx context.Context, workflowID string) (bool, string, error) {
	return false, "", nil
}
func (s *stubWorkflowStore) DeliverSignal(ctx context.Context, workflowID, signalName, payload string) error {
	return nil
}
func (s *stubWorkflowStore) PollAndClaimSignal(ctx context.Context, workflowID, signalName string) (string, bool, error) {
	return "", false, nil
}
func (s *stubWorkflowStore) StartNewRun(ctx context.Context, runID, defName string, defVersion int, input json.RawMessage, idempotencyKey, tenantID string, priority int) (string, bool, error) {
	return "", false, nil
}
func (s *stubWorkflowStore) StartChildWorkflow(ctx context.Context, parentID, defName, inputJSON string, defVersion int, parentClosePolicy string, priority int) (string, error) {
	return "", nil
}
func (s *stubWorkflowStore) StartChildWorkflowAtomic(ctx context.Context, childID, parentID, defName, inputJSON string, defVersion int, parentClosePolicy string, event EventRecord, priority int) (string, error) {
	return s.StartChildWorkflow(ctx, parentID, defName, inputJSON, defVersion, parentClosePolicy, priority)
}
func (s *stubWorkflowStore) GetChildResult(ctx context.Context, runID string) (string, bool, error) {
	return "", false, nil
}
func (s *stubWorkflowStore) ReapStaleInstances(ctx context.Context, timeout time.Duration) (int, error) {
	return 0, nil
}
func (s *stubWorkflowStore) PollSignal(ctx context.Context, workflowID, signalName string) (string, bool, error) {
	return "", false, nil
}
func (s *stubWorkflowStore) PollCancellation(ctx context.Context, workflowID string) (bool, string, error) {
	return false, "", nil
}
func (s *stubWorkflowStore) GetQueryState(ctx context.Context, workflowID, key string) (string, error) {
	return "", nil
}
func (s *stubWorkflowStore) ListWorkflows(ctx context.Context, filter WorkflowFilter) ([]WorkflowInstance, error) {
	return nil, nil
}
func (s *stubWorkflowStore) GetWorkflowByID(ctx context.Context, id string) (*WorkflowInstance, error) {
	return nil, nil
}
func (s *stubWorkflowStore) CreateSchedule(ctx context.Context, sch Schedule) error {
	return nil
}
func (s *stubWorkflowStore) ListSchedules(ctx context.Context) ([]Schedule, error) {
	return nil, nil
}
func (s *stubWorkflowStore) DeleteSchedule(ctx context.Context, name string) error {
	return nil
}
func (s *stubWorkflowStore) SetScheduleEnabled(ctx context.Context, name string, enabled bool) error {
	return nil
}
func (s *stubWorkflowStore) GetDueSchedules(ctx context.Context) ([]Schedule, error) {
	return nil, nil
}
func (s *stubWorkflowStore) UpdateScheduleNextRun(ctx context.Context, name string, nextRun time.Time) error {
	return nil
}
func (s *stubWorkflowStore) LoadWorkflowConfig(ctx context.Context, defName string, defVersion int) (int, error) {
	return 0, nil
}
func (s *stubWorkflowStore) LoadDAGSpec(ctx context.Context, defName string, defVersion int) (json.RawMessage, error) {
	return nil, nil
}
func (s *stubWorkflowStore) TraceWorkflow(ctx context.Context, workflowID, traceID string) error {
	return nil
}
func (s *stubWorkflowStore) GetCompactionCandidates(ctx context.Context, threshold int, limit int) ([]string, error) {
	return nil, nil
}
func (s *stubWorkflowStore) LoadCompactionState(ctx context.Context, workflowID string) (*CompactionState, error) {
	return nil, nil
}
func (s *stubWorkflowStore) CompactHistory(ctx context.Context, workflowID string, compactionState []byte, compactionStep int, keepStep int) error {
	return nil
}
func (s *stubWorkflowStore) CreatePromise(ctx context.Context, workflowID, promiseName, promiseID string) error {
	return nil
}
func (s *stubWorkflowStore) ResolvePromise(ctx context.Context, workflowID, promiseID, result string) error {
	return nil
}
func (s *stubWorkflowStore) RejectPromise(ctx context.Context, workflowID, promiseID, errMsg string) error {
	return nil
}
func (s *stubWorkflowStore) GetPromise(ctx context.Context, workflowID, promiseID string) (string, string, string, error) {
	return "", "", "", nil
}
func (s *stubWorkflowStore) ListPromises(ctx context.Context, workflowID string) ([]PromiseInfo, error) {
	return nil, nil
}
func (s *stubWorkflowStore) CreateUpdateRequest(ctx context.Context, workflowID, updateName, payload, promiseID string) error {
	return nil
}
func (s *stubWorkflowStore) GetPendingUpdateRequests(ctx context.Context, workflowID string) ([]UpdateRequestInfo, error) {
	return nil, nil
}
func (s *stubWorkflowStore) CompleteUpdateRequest(ctx context.Context, workflowID, updateName, result, errMsg string) error {
	return nil
}
func (s *stubWorkflowStore) AcquireConcurrencyKey(ctx context.Context, key, workflowID string, ttl time.Duration) (bool, error) {
	return false, nil
}
func (s *stubWorkflowStore) ReleaseConcurrencyKey(ctx context.Context, key string) error {
	return nil
}
func (s *stubWorkflowStore) ReleaseWorkflowConcurrencyKeys(ctx context.Context, workflowID string) error {
	return nil
}
func (s *stubWorkflowStore) ReapExpiredConcurrencyKeys(ctx context.Context) (int64, error) {
	return 0, nil
}
func (s *stubWorkflowStore) UpdateStickyWorker(ctx context.Context, workflowID, workerID string) error {
	return nil
}
func (s *stubWorkflowStore) ClearStickyWorker(ctx context.Context, workflowID string) error {
	return nil
}
func (s *stubWorkflowStore) DeployWorkflowDef(ctx context.Context, def *WorkflowDef) error {
	return nil
}
func (s *stubWorkflowStore) GetWorkflowDef(ctx context.Context, name string, version int) (*WorkflowDef, error) {
	return nil, nil
}
func (s *stubWorkflowStore) MarkVersionDeprecated(ctx context.Context, name string, version int, deprecated bool) error {
	return nil
}
func (s *stubWorkflowStore) CountActiveInstances(ctx context.Context, name string, version int) (int, error) {
	return 0, nil
}
func (s *stubWorkflowStore) GetActiveInstanceCountsByVersion(ctx context.Context) (map[string]int, error) {
	return nil, nil
}
func (s *stubWorkflowStore) ResolveLatestVersion(ctx context.Context, defName string) (int, error) {
	return 0, nil
}
func (s *stubWorkflowStore) ValidateVersion(ctx context.Context, defName string, defVersion int) (bool, error) {
	return false, nil
}
func (s *stubWorkflowStore) RecordWorkflowMemorySample(ctx context.Context, defName string, sampleBytes int64) error {
	return nil
}
func (s *stubWorkflowStore) LoadMemoryEstimates(ctx context.Context) (map[string]float64, error) {
	return nil, nil
}
func (s *stubWorkflowStore) LoadMemoryStats(ctx context.Context) ([]WorkflowMemoryStats, error) {
	return nil, nil
}
func (s *stubWorkflowStore) QueueDepth(ctx context.Context) (int64, error) {
	return 0, nil
}
func (s *stubWorkflowStore) CleanupMemorySamples(ctx context.Context, maxSamplesPerDef int) (int64, error) {
	return 0, nil
}
func (s *stubWorkflowStore) DeleteExpiredEvents(ctx context.Context, olderThan time.Time) (int64, error) {
	return 0, nil
}
func (s *stubWorkflowStore) ListWorkflowDefs(ctx context.Context, name string) ([]WorkflowDef, error) {
	return nil, nil
}
func (s *stubWorkflowStore) PurgeWorkflowDef(ctx context.Context, name string, version int) error {
	return nil
}

func (s *stubWorkflowStore) TerminateWorkflow(ctx context.Context, workflowID, reason string) error {
	return nil
}

func (s *stubWorkflowStore) DeleteDeadLetteredWorkflows(ctx context.Context, olderThan time.Time) (int64, error) {
	return 0, nil
}

func (s *stubWorkflowStore) StreamEventHistory(ctx context.Context, workflowID string, pageSize int) (<-chan EventRecord, <-chan error) {
	eventCh := make(chan EventRecord)
	errCh := make(chan error, 1)
	close(eventCh)
	close(errCh)
	return eventCh, errCh
}

func (s *stubWorkflowStore) ContinueAsNew(ctx context.Context, currentRunID, workerID string, generation int64, defName string, defVersion int, newInput json.RawMessage, newEvents []EventRecord, result string, queryState map[string]string, priority int) (string, error) {
	return "", nil
}

func (s *stubWorkflowStore) FinalizeWorkflowSegment(ctx context.Context, runID, workerID string, generation int64, newEvents []EventRecord, finalStatus string, result string, errorCode string, errorOp string, queryState map[string]string, nextWakeAt time.Time) error {
	return nil
}

// ---------------------------------------------------------------------------
// CollectVersionMetrics tests
// ---------------------------------------------------------------------------

type mockCollectMetricsStore struct {
	*stubWorkflowStore
	defs   []WorkflowDef
	counts map[string]int
}

func (m *mockCollectMetricsStore) ListWorkflowDefs(_ context.Context, name string) ([]WorkflowDef, error) {
	return m.defs, nil
}

func (m *mockCollectMetricsStore) GetActiveInstanceCountsByVersion(_ context.Context) (map[string]int, error) {
	return m.counts, nil
}

func TestCollectVersionMetrics_Empty(t *testing.T) {
	store := &mockCollectMetricsStore{
		stubWorkflowStore: &stubWorkflowStore{},
		defs:              []WorkflowDef{},
		counts:            map[string]int{},
	}
	summary, err := CollectVersionMetrics(context.Background(), store)
	if err != nil {
		t.Fatalf("CollectVersionMetrics: %v", err)
	}
	if summary.TotalVersions != 0 {
		t.Errorf("TotalVersions = %d, want 0", summary.TotalVersions)
	}
	if summary.TotalActiveInstances != 0 {
		t.Errorf("TotalActiveInstances = %d, want 0", summary.TotalActiveInstances)
	}
	if summary.ActiveVersions != 0 {
		t.Errorf("ActiveVersions = %d, want 0", summary.ActiveVersions)
	}
}

func TestCollectVersionMetrics_WithData(t *testing.T) {
	now := time.Now()
	store := &mockCollectMetricsStore{
		stubWorkflowStore: &stubWorkflowStore{},
		defs: []WorkflowDef{
			{Name: "wf-alpha", Version: 2, Deprecated: false, CreatedAt: now.Add(-24 * time.Hour), ABIVersion: 1, MinVersion: 1},
			{Name: "wf-alpha", Version: 1, Deprecated: true, CreatedAt: now.Add(-72 * time.Hour), ABIVersion: 1, MinVersion: 1},
			{Name: "wf-beta", Version: 1, Deprecated: false, CreatedAt: now.Add(-48 * time.Hour), ABIVersion: 2, MinVersion: 2},
		},
		counts: map[string]int{
			"wf-alpha:2": 5,
			"wf-alpha:1": 2,
			"wf-beta:1":  3,
		},
	}
	summary, err := CollectVersionMetrics(context.Background(), store)
	if err != nil {
		t.Fatalf("CollectVersionMetrics: %v", err)
	}
	if summary.TotalVersions != 3 {
		t.Errorf("TotalVersions = %d, want 3", summary.TotalVersions)
	}
	if summary.TotalActiveInstances != 10 {
		t.Errorf("TotalActiveInstances = %d, want 10", summary.TotalActiveInstances)
	}
	if summary.ActiveVersions != 2 {
		t.Errorf("ActiveVersions = %d, want 2", summary.ActiveVersions)
	}
	if summary.Deprecated != 1 {
		t.Errorf("Deprecated = %d, want 1", summary.Deprecated)
	}
	if len(summary.Workflows) != 3 {
		t.Errorf("len(Workflows) = %d, want 3", len(summary.Workflows))
	}
}

// ---------------------------------------------------------------------------
// CheckStaleVersions tests
// ---------------------------------------------------------------------------

type mockCheckStaleStore struct {
	*stubWorkflowStore
	defs   []WorkflowDef
	counts map[string]int
}

func (m *mockCheckStaleStore) ListWorkflowDefs(_ context.Context, name string) ([]WorkflowDef, error) {
	return m.defs, nil
}

func (m *mockCheckStaleStore) GetActiveInstanceCountsByVersion(_ context.Context) (map[string]int, error) {
	return m.counts, nil
}

func TestCheckStaleVersions_NoAlerts(t *testing.T) {
	store := &mockCheckStaleStore{
		stubWorkflowStore: &stubWorkflowStore{},
		defs: []WorkflowDef{
			{Name: "wf-fresh", Version: 1, Deprecated: false, CreatedAt: time.Now()},
		},
		counts: map[string]int{"wf-fresh:1": 0},
	}
	alerts, err := CheckStaleVersions(context.Background(), store, 7*24*time.Hour, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("CheckStaleVersions: %v", err)
	}
	if len(alerts) != 0 {
		t.Errorf("expected 0 alerts, got %d", len(alerts))
	}
}

func TestCheckStaleVersions_StaleNonDeprecated(t *testing.T) {
	store := &mockCheckStaleStore{
		stubWorkflowStore: &stubWorkflowStore{},
		defs: []WorkflowDef{
			{Name: "wf-old", Version: 1, Deprecated: false, CreatedAt: time.Now().Add(-14 * 24 * time.Hour)},
		},
		counts: map[string]int{"wf-old:1": 3},
	}
	alerts, err := CheckStaleVersions(context.Background(), store, 7*24*time.Hour, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("CheckStaleVersions: %v", err)
	}
	if len(alerts) != 1 {
		t.Fatalf("expected 1 alert, got %d", len(alerts))
	}
	if alerts[0].Deprecated {
		t.Error("expected non-deprecated alert")
	}
	if alerts[0].ActiveInstances != 3 {
		t.Errorf("ActiveInstances = %d, want 3", alerts[0].ActiveInstances)
	}
}

func TestCheckStaleVersions_DeprecatedWithInstances(t *testing.T) {
	store := &mockCheckStaleStore{
		stubWorkflowStore: &stubWorkflowStore{},
		defs: []WorkflowDef{
			{Name: "wf-dep", Version: 1, Deprecated: true, CreatedAt: time.Now().Add(-14 * 24 * time.Hour)},
		},
		counts: map[string]int{"wf-dep:1": 2},
	}
	alerts, err := CheckStaleVersions(context.Background(), store, 7*24*time.Hour, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("CheckStaleVersions: %v", err)
	}
	if len(alerts) != 1 {
		t.Fatalf("expected 1 alert, got %d", len(alerts))
	}
	if !alerts[0].Deprecated {
		t.Error("expected deprecated alert")
	}
	if alerts[0].ActiveInstances != 2 {
		t.Errorf("ActiveInstances = %d, want 2", alerts[0].ActiveInstances)
	}
}

func TestCheckStaleVersions_DeprecatedNoInstancesGC(t *testing.T) {
	store := &mockCheckStaleStore{
		stubWorkflowStore: &stubWorkflowStore{},
		defs: []WorkflowDef{
			{Name: "wf-gc", Version: 1, Deprecated: true, CreatedAt: time.Now().Add(-60 * 24 * time.Hour)},
		},
		counts: map[string]int{"wf-gc:1": 0},
	}
	alerts, err := CheckStaleVersions(context.Background(), store, 7*24*time.Hour, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("CheckStaleVersions: %v", err)
	}
	if len(alerts) != 1 {
		t.Fatalf("expected 1 alert, got %d", len(alerts))
	}
	if !alerts[0].Deprecated {
		t.Error("expected deprecated alert")
	}
	if alerts[0].ActiveInstances != 0 {
		t.Errorf("ActiveInstances = %d, want 0", alerts[0].ActiveInstances)
	}
}

// ---------------------------------------------------------------------------
// GarbageCollectVersions tests
// ---------------------------------------------------------------------------

type mockGCStore struct {
	*stubWorkflowStore
	defs   []WorkflowDef
	counts map[string]int
	purged []string
}

func (m *mockGCStore) ListWorkflowDefs(_ context.Context, name string) ([]WorkflowDef, error) {
	return m.defs, nil
}

func (m *mockGCStore) GetActiveInstanceCountsByVersion(_ context.Context) (map[string]int, error) {
	return m.counts, nil
}

func (m *mockGCStore) PurgeWorkflowDef(_ context.Context, name string, version int) error {
	m.purged = append(m.purged, fmt.Sprintf("%s:%d", name, version))
	return nil
}

func TestGarbageCollectVersions_NothingToGC(t *testing.T) {
	store := &mockGCStore{
		stubWorkflowStore: &stubWorkflowStore{},
		defs: []WorkflowDef{
			{Name: "wf", Version: 1, Deprecated: false, CreatedAt: time.Now().Add(-60 * 24 * time.Hour)},
		},
		counts: map[string]int{},
	}
	result, err := GarbageCollectVersions(context.Background(), store, DefaultGCOptions())
	if err != nil {
		t.Fatalf("GarbageCollectVersions: %v", err)
	}
	if result.VersionsRemoved != 0 {
		t.Errorf("VersionsRemoved = %d, want 0", result.VersionsRemoved)
	}
}

func TestGarbageCollectVersions_RemovesDeprecatedOld(t *testing.T) {
	store := &mockGCStore{
		stubWorkflowStore: &stubWorkflowStore{},
		defs: []WorkflowDef{
			{Name: "wf", Version: 3, Deprecated: false, CreatedAt: time.Now().Add(-24 * time.Hour)},
			{Name: "wf", Version: 2, Deprecated: true, CreatedAt: time.Now().Add(-60 * 24 * time.Hour)},
			{Name: "wf", Version: 1, Deprecated: true, CreatedAt: time.Now().Add(-90 * 24 * time.Hour)},
		},
		counts: map[string]int{},
	}
	opts := GCOptions{
		MinVersionsToKeep: 1,
		MaxVersionAge:     30 * 24 * time.Hour,
		Now:               time.Now(),
	}
	result, err := GarbageCollectVersions(context.Background(), store, opts)
	if err != nil {
		t.Fatalf("GarbageCollectVersions: %v", err)
	}
	if result.VersionsRemoved != 2 {
		t.Errorf("VersionsRemoved = %d, want 2 (v1 and v2), got purged=%v", result.VersionsRemoved, store.purged)
	}
}

func TestGarbageCollectVersions_ProtectedByMinKeep(t *testing.T) {
	now := time.Now()
	store := &mockGCStore{
		stubWorkflowStore: &stubWorkflowStore{},
		defs: []WorkflowDef{
			{Name: "wf", Version: 3, Deprecated: false, CreatedAt: now.Add(-24 * time.Hour)},
			{Name: "wf", Version: 2, Deprecated: true, CreatedAt: now.Add(-60 * 24 * time.Hour)},
			{Name: "wf", Version: 1, Deprecated: true, CreatedAt: now.Add(-90 * 24 * time.Hour)},
		},
		counts: map[string]int{},
	}
	opts := GCOptions{
		MinVersionsToKeep: 3,
		MaxVersionAge:     30 * 24 * time.Hour,
		Now:               now,
	}
	result, err := GarbageCollectVersions(context.Background(), store, opts)
	if err != nil {
		t.Fatalf("GarbageCollectVersions: %v", err)
	}
	if result.VersionsRemoved != 0 {
		t.Errorf("VersionsRemoved = %d, want 0 (all protected by MinVersionsToKeep)", result.VersionsRemoved)
	}
}

func TestGarbageCollectVersions_SkippedActiveInstances(t *testing.T) {
	now := time.Now()
	store := &mockGCStore{
		stubWorkflowStore: &stubWorkflowStore{},
		defs: []WorkflowDef{
			{Name: "wf", Version: 2, Deprecated: false, CreatedAt: now.Add(-24 * time.Hour)},
			{Name: "wf", Version: 1, Deprecated: true, CreatedAt: now.Add(-60 * 24 * time.Hour)},
		},
		counts: map[string]int{"wf:1": 3},
	}
	opts := GCOptions{
		MinVersionsToKeep: 1,
		MaxVersionAge:     30 * 24 * time.Hour,
		Now:               now,
	}
	result, err := GarbageCollectVersions(context.Background(), store, opts)
	if err != nil {
		t.Fatalf("GarbageCollectVersions: %v", err)
	}
	if result.VersionsRemoved != 0 {
		t.Errorf("VersionsRemoved = %d, want 0 (v1 has active instances)", result.VersionsRemoved)
	}
	if result.VersionsSkipped != 1 {
		t.Errorf("VersionsSkipped = %d, want 1", result.VersionsSkipped)
	}
}

func TestGarbageCollectVersions_DryRun(t *testing.T) {
	now := time.Now()
	store := &mockGCStore{
		stubWorkflowStore: &stubWorkflowStore{},
		defs: []WorkflowDef{
			{Name: "wf", Version: 2, Deprecated: false, CreatedAt: now.Add(-24 * time.Hour)},
			{Name: "wf", Version: 1, Deprecated: true, CreatedAt: now.Add(-60 * 24 * time.Hour)},
		},
		counts: map[string]int{},
	}
	opts := GCOptions{
		MinVersionsToKeep: 1,
		MaxVersionAge:     30 * 24 * time.Hour,
		DryRun:            true,
		Now:               now,
	}
	result, err := GarbageCollectVersions(context.Background(), store, opts)
	if err != nil {
		t.Fatalf("GarbageCollectVersions: %v", err)
	}
	if result.VersionsRemoved != 1 {
		t.Errorf("VersionsRemoved = %d, want 1 (dry run counts v1)", result.VersionsRemoved)
	}
	if len(store.purged) != 0 {
		t.Errorf("expected no purges in dry run, got %v", store.purged)
	}
}

func TestGarbageCollectVersions_DefaultsApplied(t *testing.T) {
	store := &mockGCStore{
		stubWorkflowStore: &stubWorkflowStore{},
		defs:              []WorkflowDef{},
		counts:            map[string]int{},
	}
	result, err := GarbageCollectVersions(context.Background(), store, GCOptions{})
	if err != nil {
		t.Fatalf("GarbageCollectVersions with zero opts: %v", err)
	}
	if result.VersionsRemoved != 0 {
		t.Errorf("VersionsRemoved = %d, want 0", result.VersionsRemoved)
	}
}

// ---------------------------------------------------------------------------
// PurgeVersions tests
// ---------------------------------------------------------------------------

type mockPurgeStore struct {
	*stubWorkflowStore
	defs   []WorkflowDef
	counts map[string]int
	purged []string
}

func (m *mockPurgeStore) ListWorkflowDefs(_ context.Context, name string) ([]WorkflowDef, error) {
	return m.defs, nil
}

func (m *mockPurgeStore) CountActiveInstances(_ context.Context, name string, version int) (int, error) {
	return m.counts[fmt.Sprintf("%s:%d", name, version)], nil
}

func (m *mockPurgeStore) PurgeWorkflowDef(_ context.Context, name string, version int) error {
	m.purged = append(m.purged, fmt.Sprintf("%s:%d", name, version))
	return nil
}

func TestPurgeVersions_RemovesOldDeprecated(t *testing.T) {
	now := time.Now()
	store := &mockPurgeStore{
		stubWorkflowStore: &stubWorkflowStore{},
		defs: []WorkflowDef{
			{Name: "wf", Version: 2, Deprecated: true, CreatedAt: now.Add(-60 * 24 * time.Hour)},
			{Name: "wf", Version: 1, Deprecated: true, CreatedAt: now.Add(-90 * 24 * time.Hour)},
		},
		counts: map[string]int{"wf:1": 0, "wf:2": 0},
	}
	result, err := PurgeVersions(context.Background(), store, "wf", 30*24*time.Hour)
	if err != nil {
		t.Fatalf("PurgeVersions: %v", err)
	}
	if result.VersionsRemoved != 2 {
		t.Errorf("VersionsRemoved = %d, want 2", result.VersionsRemoved)
	}
}

func TestPurgeVersions_NotOldEnough(t *testing.T) {
	now := time.Now()
	store := &mockPurgeStore{
		stubWorkflowStore: &stubWorkflowStore{},
		defs: []WorkflowDef{
			{Name: "wf", Version: 1, Deprecated: true, CreatedAt: now.Add(-10 * 24 * time.Hour)},
		},
		counts: map[string]int{"wf:1": 0},
	}
	result, err := PurgeVersions(context.Background(), store, "wf", 30*24*time.Hour)
	if err != nil {
		t.Fatalf("PurgeVersions: %v", err)
	}
	if result.VersionsRemoved != 0 {
		t.Errorf("VersionsRemoved = %d, want 0 (not old enough)", result.VersionsRemoved)
	}
}

func TestPurgeVersions_NotDeprecated(t *testing.T) {
	now := time.Now()
	store := &mockPurgeStore{
		stubWorkflowStore: &stubWorkflowStore{},
		defs: []WorkflowDef{
			{Name: "wf", Version: 1, Deprecated: false, CreatedAt: now.Add(-60 * 24 * time.Hour)},
		},
		counts: map[string]int{"wf:1": 0},
	}
	result, err := PurgeVersions(context.Background(), store, "wf", 30*24*time.Hour)
	if err != nil {
		t.Fatalf("PurgeVersions: %v", err)
	}
	if result.VersionsRemoved != 0 {
		t.Errorf("VersionsRemoved = %d, want 0 (not deprecated)", result.VersionsRemoved)
	}
}

func TestPurgeVersions_SkippedActiveInstances(t *testing.T) {
	now := time.Now()
	store := &mockPurgeStore{
		stubWorkflowStore: &stubWorkflowStore{},
		defs: []WorkflowDef{
			{Name: "wf", Version: 1, Deprecated: true, CreatedAt: now.Add(-60 * 24 * time.Hour)},
		},
		counts: map[string]int{"wf:1": 5},
	}
	result, err := PurgeVersions(context.Background(), store, "wf", 30*24*time.Hour)
	if err != nil {
		t.Fatalf("PurgeVersions: %v", err)
	}
	if result.VersionsRemoved != 0 {
		t.Errorf("VersionsRemoved = %d, want 0", result.VersionsRemoved)
	}
	if result.VersionsSkipped != 1 {
		t.Errorf("VersionsSkipped = %d, want 1", result.VersionsSkipped)
	}
}

// ---------------------------------------------------------------------------
// PluginRegistry RegisterIdempotent duplicate error
// ---------------------------------------------------------------------------

func TestPluginRegistry_RegisterIdempotent_Duplicate(t *testing.T) {
	pr := NewPluginRegistry()
	fn := func(ctx context.Context, inputJSON string) (string, error) {
		return `{"result":"ok"}`, nil
	}

	if err := pr.RegisterIdempotent("my-plugin", "my-func", fn); err != nil {
		t.Fatalf("first registration should succeed: %v", err)
	}
	// Second registration with same name+func should fail.
	if err := pr.RegisterIdempotent("my-plugin", "my-func", fn); err == nil {
		t.Error("expected error for duplicate registration")
	} else if !strings.Contains(err.Error(), "already registered") {
		t.Errorf("expected 'already registered' error, got: %v", err)
	}
}

func TestPluginRegistry_RegisterIdempotent_SameFuncDiffPlugin(t *testing.T) {
	pr := NewPluginRegistry()
	fn := func(ctx context.Context, inputJSON string) (string, error) {
		return `{}`, nil
	}

	if err := pr.Register("plugin-a", "some-func", fn); err != nil {
		t.Fatalf("register on plugin-a: %v", err)
	}
	// Same function name under a different plugin is fine.
	if err := pr.RegisterIdempotent("plugin-b", "some-func", fn); err != nil {
		t.Errorf("same func name under different plugin should succeed: %v", err)
	}
}

// ---------------------------------------------------------------------------
// releaseHeldScopes tests
// ---------------------------------------------------------------------------

// mockConcurrencyKeyStore provides a minimal in-memory ConcurrencyKeyStore
// implementation for unit tests.
type mockConcurrencyKeyStore struct{}

func (m *mockConcurrencyKeyStore) AcquireConcurrencyKey(ctx context.Context, key, workflowID string, ttl time.Duration) (bool, error) {
	return true, nil
}
func (m *mockConcurrencyKeyStore) ReleaseConcurrencyKey(ctx context.Context, key string) error {
	return nil
}

func newMockConcurrencyKeyStore() *mockConcurrencyKeyStore {
	return &mockConcurrencyKeyStore{}
}

// releaseErrorStore wraps the existing mockConcurrencyKeyStore and injects
// errors on ReleaseConcurrencyKey.
type releaseErrorStore struct {
	mockConcurrencyKeyStore
}

func (r *releaseErrorStore) ReleaseConcurrencyKey(ctx context.Context, key string) error {
	return fmt.Errorf("simulated release failure")
}

func TestReleaseHeldScopes_NilStore(t *testing.T) {
	// When concurrencyKeyStore is nil, releaseHeldScopes should return
	// immediately without iterating heldScopes. heldScopes is NOT cleared
	// in this path because there's nothing to release.
	e := &Engine{}
	s := &execSession{
		engine:     e,
		heldScopes: []string{"vo:obj-type:inst-key"},
	}
	// Should not panic or block.
	s.releaseHeldScopes(context.Background())
	// heldScopes is still set because the early return path does not clear it.
	if len(s.heldScopes) != 1 {
		t.Errorf("heldScopes should be preserved when store is nil, got %v", s.heldScopes)
	}
}

func TestReleaseHeldScopes_EmptyList(t *testing.T) {
	store := newMockConcurrencyKeyStore()
	e := &Engine{concurrencyKeyStore: store}
	s := &execSession{
		engine:     e,
		heldScopes: []string{},
	}
	s.releaseHeldScopes(context.Background())
	if s.heldScopes != nil {
		t.Error("heldScopes should be nil after release")
	}
}

func TestReleaseHeldScopes_Success(t *testing.T) {
	ctx := context.Background()
	store := newMockConcurrencyKeyStore()

	// Pre-acquire keys as if a workflow had set scopes.
	store.AcquireConcurrencyKey(ctx, "vo:obj-a:key-1", "wf-1", time.Hour)
	store.AcquireConcurrencyKey(ctx, "vo:obj-b:key-2", "wf-1", time.Hour)

	e := &Engine{concurrencyKeyStore: store}
	s := &execSession{
		engine:     e,
		heldScopes: []string{"vo:obj-a:key-1", "vo:obj-b:key-2"},
	}
	s.releaseHeldScopes(ctx)

	if s.heldScopes != nil {
		t.Error("heldScopes should be nil after release")
	}

	// Keys should now be releasable (re-acquirable by a different workflow).
	acquired, err := store.AcquireConcurrencyKey(ctx, "vo:obj-a:key-1", "wf-2", time.Hour)
	if err != nil {
		t.Fatalf("re-acquire key-1: %v", err)
	}
	if !acquired {
		t.Error("expected key-1 to be released and acquirable by different workflow")
	}
}

func TestReleaseHeldScopes_ReleaseError(t *testing.T) {
	// When ReleaseConcurrencyKey returns an error, releaseHeldScopes should
	// log the error and continue with the remaining scopes.
	store := &releaseErrorStore{}
	e := &Engine{concurrencyKeyStore: store}
	s := &execSession{
		engine:     e,
		heldScopes: []string{"vo:obj-a:key-1", "vo:obj-b:key-2"},
	}
	// Should not panic despite the error -- the error is only logged.
	s.releaseHeldScopes(context.Background())
	if s.heldScopes != nil {
		t.Error("heldScopes should be nil after release")
	}
}

// ---------------------------------------------------------------------------
// PluginStreamRegistry tests
// ---------------------------------------------------------------------------

func TestPluginStreamRegistry_RegisterAndHas(t *testing.T) {
	psr := NewPluginStreamRegistry()
	fn := func(ctx context.Context, inputJSON string) (<-chan plugin.StreamEvent, error) {
		return nil, nil
	}

	if err := psr.Register("plugin-a", "stream-func", fn); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if !psr.Has("plugin-a", "stream-func") {
		t.Error("Has should return true after Register")
	}
	if psr.Has("plugin-a", "nonexistent") {
		t.Error("Has should return false for nonexistent function")
	}
}

func TestPluginStreamRegistry_RegisterDuplicate(t *testing.T) {
	psr := NewPluginStreamRegistry()
	fn := func(ctx context.Context, inputJSON string) (<-chan plugin.StreamEvent, error) {
		return nil, nil
	}

	if err := psr.Register("plugin", "func", fn); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	if err := psr.Register("plugin", "func", fn); err == nil {
		t.Error("expected error for duplicate registration")
	}
}

func TestPluginStreamRegistry_Lookup(t *testing.T) {
	psr := NewPluginStreamRegistry()
	fn := func(ctx context.Context, inputJSON string) (<-chan plugin.StreamEvent, error) {
		return nil, nil
	}

	psr.Register("plugin", "func", fn)
	got, ok := psr.Lookup("plugin", "func")
	if !ok {
		t.Fatal("Lookup should return true for registered function")
	}
	if got == nil {
		t.Error("Lookup should return non-nil function")
	}

	// Lookup nonexistent.
	_, ok = psr.Lookup("plugin", "missing")
	if ok {
		t.Error("Lookup should return false for missing function")
	}
}

func TestPluginStreamRegistry_RegisterStream(t *testing.T) {
	psr := NewPluginStreamRegistry()
	fn := plugin.PluginStreamFunc(func(ctx context.Context, inputJSON string) (<-chan plugin.StreamEvent, error) {
		return nil, nil
	})

	opts := plugin.FuncOptions{Name: "test-func"}
	if err := psr.RegisterStream("plugin", opts, fn); err != nil {
		t.Fatalf("RegisterStream: %v", err)
	}
	if !psr.Has("plugin", "test-func") {
		t.Error("Has should return true after RegisterStream")
	}
}

// ---------------------------------------------------------------------------
// PluginRegistry basic tests
// ---------------------------------------------------------------------------

func TestPluginRegistry_Lookup(t *testing.T) {
	pr := NewPluginRegistry()
	fn := func(ctx context.Context, inputJSON string) (string, error) {
		return "result", nil
	}

	pr.Register("plugin", "func", fn)

	f, idempotent, ok := pr.Lookup("plugin", "func")
	if !ok {
		t.Fatal("Lookup should return ok=true for registered func")
	}
	if f == nil {
		t.Error("Lookup should return non-nil function")
	}
	if idempotent {
		t.Error("Lookup should return idempotent=false for non-idempotent func")
	}

	// Lookup missing.
	_, _, ok = pr.Lookup("plugin", "missing")
	if ok {
		t.Error("Lookup should return ok=false for missing func")
	}
}

func TestPluginRegistry_RegisterAlreadyExists(t *testing.T) {
	pr := NewPluginRegistry()
	fn := func(ctx context.Context, inputJSON string) (string, error) {
		return "", nil
	}

	if err := pr.Register("p", "f", fn); err != nil {
		t.Fatalf("first register: %v", err)
	}
	if err := pr.Register("p", "f", fn); err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Errorf("expected 'already registered' error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Engine option functions
// ---------------------------------------------------------------------------

func TestWithSignalStore(t *testing.T) {
	store := &stubWorkflowStore{}
	opt := WithSignalStore(store)
	e := NewEngine(nil, nil)
	opt(e)
	if e.signalStore != store {
		t.Error("WithSignalStore did not set signalStore")
	}
}

func TestWithPromiseStore(t *testing.T) {
	store := &stubWorkflowStore{}
	opt := WithPromiseStore(store)
	e := NewEngine(nil, nil)
	opt(e)
	if e.promiseStore != store {
		t.Error("WithPromiseStore did not set promiseStore")
	}
}

func TestWithWorkflowID(t *testing.T) {
	want := "test-workflow-id"
	opt := WithWorkflowID(want)
	e := NewEngine(nil, nil)
	opt(e)
	if e.workflowID != want {
		t.Errorf("WithWorkflowID: got %q, want %q", e.workflowID, want)
	}
}

func TestWithChildWorkflowStore(t *testing.T) {
	store := &stubWorkflowStore{}
	opt := WithChildWorkflowStore(store)
	e := NewEngine(nil, nil)
	opt(e)
	if e.childWfStore != store {
		t.Error("WithChildWorkflowStore did not set childWfStore")
	}
}

func TestWithConcurrencyKeyStore(t *testing.T) {
	store := &stubWorkflowStore{}
	opt := WithConcurrencyKeyStore(store)
	e := NewEngine(nil, nil)
	opt(e)
	if e.concurrencyKeyStore != store {
		t.Error("WithConcurrencyKeyStore did not set concurrencyKeyStore")
	}
}

func TestWithCompactionState(t *testing.T) {
	cs := &CompactionState{Version: 1, CompactedStep: 5}
	opt := WithCompactionState(cs)
	e := NewEngine(nil, nil)
	opt(e)
	if e.compactionState != cs {
		t.Error("WithCompactionState did not set compactionState")
	}
}

func TestWithPluginRegistry(t *testing.T) {
	pr := NewPluginRegistry()
	opt := WithPluginRegistry(pr)
	e := NewEngine(nil, nil)
	opt(e)
	if e.pluginRegistry != pr {
		t.Error("WithPluginRegistry did not set pluginRegistry")
	}
}

func TestWithPluginStreamRegistry(t *testing.T) {
	psr := NewPluginStreamRegistry()
	opt := WithPluginStreamRegistry(psr)
	e := NewEngine(nil, nil)
	opt(e)
	if e.pluginStreamRegistry != psr {
		t.Error("WithPluginStreamRegistry did not set pluginStreamRegistry")
	}
}

func TestWithTenantID(t *testing.T) {
	want := "test-tenant"
	opt := WithTenantID(want)
	e := NewEngine(nil, nil)
	opt(e)
	if e.tenantID != want {
		t.Errorf("WithTenantID: got %q, want %q", e.tenantID, want)
	}
}

func TestWithPluginCallGuard(t *testing.T) {
	g := NewPluginCallGuard()
	opt := WithPluginCallGuard(g)
	e := NewEngine(nil, nil)
	opt(e)
	if e.pluginCallGuard != g {
		t.Error("WithPluginCallGuard did not set pluginCallGuard")
	}
}

func TestWithDB(t *testing.T) {
	opt := WithDB(nil)
	e := NewEngine(nil, nil)
	opt(e)
	if e.db != nil {
		t.Error("WithDB(nil) should set db to nil")
	}
}

func TestWithOptionsViaNewEngine(t *testing.T) {
	// Verify that options work when passed via NewEngine.
	store := &stubWorkflowStore{}
	psr := NewPluginStreamRegistry()
	e := NewEngine(nil, nil,
		WithSignalStore(store),
		WithPromiseStore(store),
		WithWorkflowID("wf-1"),
		WithTenantID("tenant-1"),
		WithPluginStreamRegistry(psr),
	)
	if e.signalStore != store {
		t.Error("WithSignalStore not applied via NewEngine")
	}
	if e.promiseStore != store {
		t.Error("WithPromiseStore not applied via NewEngine")
	}
	if e.workflowID != "wf-1" {
		t.Errorf("WithWorkflowID: got %q", e.workflowID)
	}
	if e.tenantID != "tenant-1" {
		t.Errorf("WithTenantID: got %q", e.tenantID)
	}
	if e.pluginStreamRegistry != psr {
		t.Error("WithPluginStreamRegistry not applied via NewEngine")
	}
}
func (s *stubWorkflowStore) BatchHeartbeat(ctx context.Context, workerID string) (int64, error) { return 0, nil }
func (m *mockCollectMetricsStore) BatchHeartbeat(ctx context.Context, workerID string) (int64, error) { return 0, nil }
func (m *mockCheckStaleStore) BatchHeartbeat(ctx context.Context, workerID string) (int64, error) { return 0, nil }
func (m *mockGCStore) BatchHeartbeat(ctx context.Context, workerID string) (int64, error) { return 0, nil }
func (m *mockPurgeStore) BatchHeartbeat(ctx context.Context, workerID string) (int64, error) { return 0, nil }

func (s *stubWorkflowStore) LoadEventHistoryPaginated(ctx context.Context, workflowID string, offset, limit int) ([]EventRecord, error) { return nil, nil }
func (s *stubWorkflowStore) VerifyWorkflowEvents(ctx context.Context, workflowID string) error { return nil }
func (s *stubWorkflowStore) MoveToDeadLetterQueue(ctx context.Context, workflowID, workerID string, generation int64, errMsg, errorCode, errorOp string) error { return nil }
func (s *stubWorkflowStore) CountEventHistory(ctx context.Context, workflowID string) (int, error) { return 0, nil }
func (s *stubWorkflowStore) RetryWorkflow(ctx context.Context, workflowID string) error { return nil }
func (s *stubWorkflowStore) ResolveTenantFromAPIKey(ctx context.Context, keyHash []byte) (uuid.UUID, error) { return uuid.Nil, nil }

func (s *stubWorkflowStore) GetChildCount(ctx context.Context, parentWorkflowID string) (int, error) { return 0, nil }

func (s *stubWorkflowStore) GetConcurrencyKeyCount(ctx context.Context, workflowID string) (int, error) { return 0, nil }
func (s *stubWorkflowStore) GetEventCount(ctx context.Context, workflowID string) (int, error) { return 0, nil }
func (s *stubWorkflowStore) GetAllowedSignalCallers(ctx context.Context, workflowID string) ([]string, error) { return nil, nil }
func (m *mockCollectMetricsStore) ClaimWorkflow(ctx context.Context, workerID string) (*WorkflowInstance, error) { return nil, nil }
func (m *mockCollectMetricsStore) ClaimWorkflows(ctx context.Context, workerID string, limit int) ([]*WorkflowInstance, error) { return nil, nil }
func (m *mockCollectMetricsStore) ClaimStickyWorkflows(ctx context.Context, workerID string, limit int) ([]*WorkflowInstance, error) { return nil, nil }
func (m *mockCollectMetricsStore) LoadEventHistory(ctx context.Context, workflowID string) ([]EventRecord, error) { return nil, nil }
func (m *mockCollectMetricsStore) LoadEventHistoryPaginated(ctx context.Context, workflowID string, offset, limit int) ([]EventRecord, error) { return nil, nil }
func (m *mockCollectMetricsStore) AppendEventHistory(ctx context.Context, workflowID string, rec EventRecord) error { return nil }
func (m *mockCollectMetricsStore) AppendEventHistoryBatch(ctx context.Context, workflowID string, recs []EventRecord) error { return nil }
func (m *mockCollectMetricsStore) VerifyWorkflowEvents(ctx context.Context, workflowID string) error { return nil }
func (m *mockCollectMetricsStore) LoadWASM(ctx context.Context, defName string, defVersion int) ([]byte, error) { return nil, nil }
func (m *mockCollectMetricsStore) GetWASMLength(ctx context.Context, defName string, defVersion int) (int64, error) { return 0, nil }
func (m *mockCollectMetricsStore) ListVersions(ctx context.Context, defName string) ([]int, error) { return nil, nil }
func (m *mockCollectMetricsStore) Heartbeat(ctx context.Context, workflowID, workerID string, generation int64) (bool, error) { return false, nil }
func (m *mockCollectMetricsStore) CompleteWorkflow(ctx context.Context, workflowID, workerID string, generation int64, result string, queryState map[string]string) error { return nil }
func (m *mockCollectMetricsStore) FailWorkflow(ctx context.Context, workflowID, workerID string, generation int64, errorMsg, errorCode, errorOp string, queryState map[string]string) error { return nil }
func (m *mockCollectMetricsStore) MoveToDeadLetterQueue(ctx context.Context, workflowID, workerID string, generation int64, errMsg, errorCode, errorOp string) error { return nil }
func (m *mockCollectMetricsStore) ReleaseWorkflow(ctx context.Context, workflowID, workerID string, generation int64, nextWakeAt time.Time) error { return nil }
func (m *mockCollectMetricsStore) RequestCancellation(ctx context.Context, workflowID, reason string) error { return nil }
func (m *mockCollectMetricsStore) CheckCancellation(ctx context.Context, workflowID string) (cancelled bool, reason string, err error) { return false, "", nil }
func (m *mockCollectMetricsStore) DeliverSignal(ctx context.Context, workflowID, signalName, payload string) error { return nil }
func (m *mockCollectMetricsStore) PollAndClaimSignal(ctx context.Context, workflowID, signalName string) (payload string, found bool, err error) { return "", false, nil }
func (m *mockCollectMetricsStore) StartNewRun(ctx context.Context, runID, defName string, defVersion int, input json.RawMessage, idempotencyKey, tenantID string, priority int) (string, bool, error) { return "", false, nil }
func (m *mockCollectMetricsStore) StartChildWorkflow(ctx context.Context, parentID, defName, inputJSON string, defVersion int, parentClosePolicy string, priority int) (runID string, err error) { return "", nil }
func (m *mockCollectMetricsStore) StartChildWorkflowAtomic(ctx context.Context, childID, parentID, defName, inputJSON string, defVersion int, parentClosePolicy string, event EventRecord, priority int) (runID string, err error) { return m.StartChildWorkflow(ctx, parentID, defName, inputJSON, defVersion, parentClosePolicy, priority) }
func (m *mockCollectMetricsStore) GetChildResult(ctx context.Context, runID string) (resultJSON string, completed bool, err error) { return "", false, nil }
func (m *mockCollectMetricsStore) ReapStaleInstances(ctx context.Context, timeout time.Duration) (int, error) { return 0, nil }
func (m *mockCollectMetricsStore) GetQueryState(ctx context.Context, workflowID, key string) (string, error) { return "", nil }
func (m *mockCollectMetricsStore) ListWorkflows(ctx context.Context, filter WorkflowFilter) ([]WorkflowInstance, error) { return nil, nil }
func (m *mockCollectMetricsStore) GetWorkflowByID(ctx context.Context, id string) (*WorkflowInstance, error) { return nil, nil }
func (m *mockCollectMetricsStore) CreateSchedule(ctx context.Context, s Schedule) error { return nil }
func (m *mockCollectMetricsStore) ListSchedules(ctx context.Context) ([]Schedule, error) { return nil, nil }
func (m *mockCollectMetricsStore) DeleteSchedule(ctx context.Context, name string) error { return nil }
func (m *mockCollectMetricsStore) SetScheduleEnabled(ctx context.Context, name string, enabled bool) error { return nil }
func (m *mockCollectMetricsStore) GetDueSchedules(ctx context.Context) ([]Schedule, error) { return nil, nil }
func (m *mockCollectMetricsStore) UpdateScheduleNextRun(ctx context.Context, name string, nextRun time.Time) error { return nil }
func (m *mockCollectMetricsStore) LoadWorkflowConfig(ctx context.Context, defName string, defVersion int) (maxHistoryLength int, err error) { return 0, nil }
func (m *mockCollectMetricsStore) LoadDAGSpec(ctx context.Context, defName string, defVersion int) (json.RawMessage, error) { return nil, nil }
func (m *mockCollectMetricsStore) TraceWorkflow(ctx context.Context, workflowID, traceID string) error { return nil }
func (m *mockCollectMetricsStore) GetCompactionCandidates(ctx context.Context, threshold int, limit int) ([]string, error) { return nil, nil }
func (m *mockCollectMetricsStore) LoadCompactionState(ctx context.Context, workflowID string) (*CompactionState, error) { return nil, nil }
func (m *mockCollectMetricsStore) CompactHistory(ctx context.Context, workflowID string, compactionState []byte, compactionStep int, keepStep int) error { return nil }
func (m *mockCollectMetricsStore) CreatePromise(ctx context.Context, workflowID, promiseName, promiseID string) error { return nil }
func (m *mockCollectMetricsStore) ResolvePromise(ctx context.Context, workflowID, promiseID, result string) error { return nil }
func (m *mockCollectMetricsStore) RejectPromise(ctx context.Context, workflowID, promiseID, errMsg string) error { return nil }
func (m *mockCollectMetricsStore) GetPromise(ctx context.Context, workflowID, promiseID string) (status string, result string, errMsg string, err error) { return "", "", "", nil }
func (m *mockCollectMetricsStore) ListPromises(ctx context.Context, workflowID string) ([]PromiseInfo, error) { return nil, nil }
func (m *mockCollectMetricsStore) CreateUpdateRequest(ctx context.Context, workflowID, updateName, payload, promiseID string) error { return nil }
func (m *mockCollectMetricsStore) GetPendingUpdateRequests(ctx context.Context, workflowID string) ([]UpdateRequestInfo, error) { return nil, nil }
func (m *mockCollectMetricsStore) CompleteUpdateRequest(ctx context.Context, workflowID, updateName, result, errMsg string) error { return nil }
func (m *mockCollectMetricsStore) AcquireConcurrencyKey(ctx context.Context, key, workflowID string, ttl time.Duration) (acquired bool, err error) { return false, nil }
func (m *mockCollectMetricsStore) ReleaseConcurrencyKey(ctx context.Context, key string) error { return nil }
func (m *mockCollectMetricsStore) ReleaseWorkflowConcurrencyKeys(ctx context.Context, workflowID string) error { return nil }
func (m *mockCollectMetricsStore) ReapExpiredConcurrencyKeys(ctx context.Context) (int64, error) { return 0, nil }
func (m *mockCollectMetricsStore) UpdateStickyWorker(ctx context.Context, workflowID, workerID string) error { return nil }
func (m *mockCollectMetricsStore) ClearStickyWorker(ctx context.Context, workflowID string) error { return nil }
func (m *mockCollectMetricsStore) DeployWorkflowDef(ctx context.Context, def *WorkflowDef) error { return nil }
func (m *mockCollectMetricsStore) GetWorkflowDef(ctx context.Context, name string, version int) (*WorkflowDef, error) { return nil, nil }
func (m *mockCollectMetricsStore) MarkVersionDeprecated(ctx context.Context, name string, version int, deprecated bool) error { return nil }
func (m *mockCollectMetricsStore) PurgeWorkflowDef(ctx context.Context, name string, version int) error { return nil }
func (m *mockCollectMetricsStore) CountActiveInstances(ctx context.Context, name string, version int) (int, error) { return 0, nil }
func (m *mockCollectMetricsStore) RecordWorkflowMemorySample(ctx context.Context, defName string, sampleBytes int64) error { return nil }
func (m *mockCollectMetricsStore) LoadMemoryEstimates(ctx context.Context) (map[string]float64, error) { return nil, nil }
func (m *mockCollectMetricsStore) LoadMemoryStats(ctx context.Context) ([]WorkflowMemoryStats, error) { return nil, nil }
func (m *mockCollectMetricsStore) QueueDepth(ctx context.Context) (int64, error) { return 0, nil }
func (m *mockCollectMetricsStore) CleanupMemorySamples(ctx context.Context, maxSamplesPerDef int) (int64, error) { return 0, nil }
func (m *mockCollectMetricsStore) DeleteExpiredEvents(ctx context.Context, olderThan time.Time) (int64, error) { return 0, nil }
func (m *mockCollectMetricsStore) ResolveTenantFromAPIKey(ctx context.Context, keyHash []byte) (uuid.UUID, error) { return uuid.Nil, nil }
func (m *mockCheckStaleStore) ClaimWorkflow(ctx context.Context, workerID string) (*WorkflowInstance, error) { return nil, nil }
func (m *mockCheckStaleStore) ClaimWorkflows(ctx context.Context, workerID string, limit int) ([]*WorkflowInstance, error) { return nil, nil }
func (m *mockCheckStaleStore) ClaimStickyWorkflows(ctx context.Context, workerID string, limit int) ([]*WorkflowInstance, error) { return nil, nil }
func (m *mockCheckStaleStore) LoadEventHistory(ctx context.Context, workflowID string) ([]EventRecord, error) { return nil, nil }
func (m *mockCheckStaleStore) LoadEventHistoryPaginated(ctx context.Context, workflowID string, offset, limit int) ([]EventRecord, error) { return nil, nil }
func (m *mockCheckStaleStore) AppendEventHistory(ctx context.Context, workflowID string, rec EventRecord) error { return nil }
func (m *mockCheckStaleStore) AppendEventHistoryBatch(ctx context.Context, workflowID string, recs []EventRecord) error { return nil }
func (m *mockCheckStaleStore) VerifyWorkflowEvents(ctx context.Context, workflowID string) error { return nil }
func (m *mockCheckStaleStore) LoadWASM(ctx context.Context, defName string, defVersion int) ([]byte, error) { return nil, nil }
func (m *mockCheckStaleStore) GetWASMLength(ctx context.Context, defName string, defVersion int) (int64, error) { return 0, nil }
func (m *mockCheckStaleStore) ListVersions(ctx context.Context, defName string) ([]int, error) { return nil, nil }
func (m *mockCheckStaleStore) Heartbeat(ctx context.Context, workflowID, workerID string, generation int64) (bool, error) { return false, nil }
func (m *mockCheckStaleStore) CompleteWorkflow(ctx context.Context, workflowID, workerID string, generation int64, result string, queryState map[string]string) error { return nil }
func (m *mockCheckStaleStore) FailWorkflow(ctx context.Context, workflowID, workerID string, generation int64, errorMsg, errorCode, errorOp string, queryState map[string]string) error { return nil }
func (m *mockCheckStaleStore) MoveToDeadLetterQueue(ctx context.Context, workflowID, workerID string, generation int64, errMsg, errorCode, errorOp string) error { return nil }
func (m *mockCheckStaleStore) ReleaseWorkflow(ctx context.Context, workflowID, workerID string, generation int64, nextWakeAt time.Time) error { return nil }
func (m *mockCheckStaleStore) RequestCancellation(ctx context.Context, workflowID, reason string) error { return nil }
func (m *mockCheckStaleStore) CheckCancellation(ctx context.Context, workflowID string) (cancelled bool, reason string, err error) { return false, "", nil }
func (m *mockCheckStaleStore) DeliverSignal(ctx context.Context, workflowID, signalName, payload string) error { return nil }
func (m *mockCheckStaleStore) PollAndClaimSignal(ctx context.Context, workflowID, signalName string) (payload string, found bool, err error) { return "", false, nil }
func (m *mockCheckStaleStore) StartNewRun(ctx context.Context, runID, defName string, defVersion int, input json.RawMessage, idempotencyKey, tenantID string, priority int) (string, bool, error) { return "", false, nil }
func (m *mockCheckStaleStore) StartChildWorkflow(ctx context.Context, parentID, defName, inputJSON string, defVersion int, parentClosePolicy string, priority int) (runID string, err error) { return "", nil }
func (m *mockCheckStaleStore) StartChildWorkflowAtomic(ctx context.Context, childID, parentID, defName, inputJSON string, defVersion int, parentClosePolicy string, event EventRecord, priority int) (runID string, err error) { return m.StartChildWorkflow(ctx, parentID, defName, inputJSON, defVersion, parentClosePolicy, priority) }
func (m *mockCheckStaleStore) GetChildResult(ctx context.Context, runID string) (resultJSON string, completed bool, err error) { return "", false, nil }
func (m *mockCheckStaleStore) ReapStaleInstances(ctx context.Context, timeout time.Duration) (int, error) { return 0, nil }
func (m *mockCheckStaleStore) GetQueryState(ctx context.Context, workflowID, key string) (string, error) { return "", nil }
func (m *mockCheckStaleStore) ListWorkflows(ctx context.Context, filter WorkflowFilter) ([]WorkflowInstance, error) { return nil, nil }
func (m *mockCheckStaleStore) GetWorkflowByID(ctx context.Context, id string) (*WorkflowInstance, error) { return nil, nil }
func (m *mockCheckStaleStore) CreateSchedule(ctx context.Context, s Schedule) error { return nil }
func (m *mockCheckStaleStore) ListSchedules(ctx context.Context) ([]Schedule, error) { return nil, nil }
func (m *mockCheckStaleStore) DeleteSchedule(ctx context.Context, name string) error { return nil }
func (m *mockCheckStaleStore) SetScheduleEnabled(ctx context.Context, name string, enabled bool) error { return nil }
func (m *mockCheckStaleStore) GetDueSchedules(ctx context.Context) ([]Schedule, error) { return nil, nil }
func (m *mockCheckStaleStore) UpdateScheduleNextRun(ctx context.Context, name string, nextRun time.Time) error { return nil }
func (m *mockCheckStaleStore) LoadWorkflowConfig(ctx context.Context, defName string, defVersion int) (maxHistoryLength int, err error) { return 0, nil }
func (m *mockCheckStaleStore) LoadDAGSpec(ctx context.Context, defName string, defVersion int) (json.RawMessage, error) { return nil, nil }
func (m *mockCheckStaleStore) TraceWorkflow(ctx context.Context, workflowID, traceID string) error { return nil }
func (m *mockCheckStaleStore) GetCompactionCandidates(ctx context.Context, threshold int, limit int) ([]string, error) { return nil, nil }
func (m *mockCheckStaleStore) LoadCompactionState(ctx context.Context, workflowID string) (*CompactionState, error) { return nil, nil }
func (m *mockCheckStaleStore) CompactHistory(ctx context.Context, workflowID string, compactionState []byte, compactionStep int, keepStep int) error { return nil }
func (m *mockCheckStaleStore) CreatePromise(ctx context.Context, workflowID, promiseName, promiseID string) error { return nil }
func (m *mockCheckStaleStore) ResolvePromise(ctx context.Context, workflowID, promiseID, result string) error { return nil }
func (m *mockCheckStaleStore) RejectPromise(ctx context.Context, workflowID, promiseID, errMsg string) error { return nil }
func (m *mockCheckStaleStore) GetPromise(ctx context.Context, workflowID, promiseID string) (status string, result string, errMsg string, err error) { return "", "", "", nil }
func (m *mockCheckStaleStore) ListPromises(ctx context.Context, workflowID string) ([]PromiseInfo, error) { return nil, nil }
func (m *mockCheckStaleStore) CreateUpdateRequest(ctx context.Context, workflowID, updateName, payload, promiseID string) error { return nil }
func (m *mockCheckStaleStore) GetPendingUpdateRequests(ctx context.Context, workflowID string) ([]UpdateRequestInfo, error) { return nil, nil }
func (m *mockCheckStaleStore) CompleteUpdateRequest(ctx context.Context, workflowID, updateName, result, errMsg string) error { return nil }
func (m *mockCheckStaleStore) AcquireConcurrencyKey(ctx context.Context, key, workflowID string, ttl time.Duration) (acquired bool, err error) { return false, nil }
func (m *mockCheckStaleStore) ReleaseConcurrencyKey(ctx context.Context, key string) error { return nil }
func (m *mockCheckStaleStore) ReleaseWorkflowConcurrencyKeys(ctx context.Context, workflowID string) error { return nil }
func (m *mockCheckStaleStore) ReapExpiredConcurrencyKeys(ctx context.Context) (int64, error) { return 0, nil }
func (m *mockCheckStaleStore) UpdateStickyWorker(ctx context.Context, workflowID, workerID string) error { return nil }
func (m *mockCheckStaleStore) ClearStickyWorker(ctx context.Context, workflowID string) error { return nil }
func (m *mockCheckStaleStore) DeployWorkflowDef(ctx context.Context, def *WorkflowDef) error { return nil }
func (m *mockCheckStaleStore) GetWorkflowDef(ctx context.Context, name string, version int) (*WorkflowDef, error) { return nil, nil }
func (m *mockCheckStaleStore) MarkVersionDeprecated(ctx context.Context, name string, version int, deprecated bool) error { return nil }
func (m *mockCheckStaleStore) PurgeWorkflowDef(ctx context.Context, name string, version int) error { return nil }
func (m *mockCheckStaleStore) CountActiveInstances(ctx context.Context, name string, version int) (int, error) { return 0, nil }
func (m *mockCheckStaleStore) RecordWorkflowMemorySample(ctx context.Context, defName string, sampleBytes int64) error { return nil }
func (m *mockCheckStaleStore) LoadMemoryEstimates(ctx context.Context) (map[string]float64, error) { return nil, nil }
func (m *mockCheckStaleStore) LoadMemoryStats(ctx context.Context) ([]WorkflowMemoryStats, error) { return nil, nil }
func (m *mockCheckStaleStore) QueueDepth(ctx context.Context) (int64, error) { return 0, nil }
func (m *mockCheckStaleStore) CleanupMemorySamples(ctx context.Context, maxSamplesPerDef int) (int64, error) { return 0, nil }
func (m *mockCheckStaleStore) DeleteExpiredEvents(ctx context.Context, olderThan time.Time) (int64, error) { return 0, nil }
func (m *mockCheckStaleStore) ResolveTenantFromAPIKey(ctx context.Context, keyHash []byte) (uuid.UUID, error) { return uuid.Nil, nil }
func (m *mockGCStore) ClaimWorkflow(ctx context.Context, workerID string) (*WorkflowInstance, error) { return nil, nil }
func (m *mockGCStore) ClaimWorkflows(ctx context.Context, workerID string, limit int) ([]*WorkflowInstance, error) { return nil, nil }
func (m *mockGCStore) ClaimStickyWorkflows(ctx context.Context, workerID string, limit int) ([]*WorkflowInstance, error) { return nil, nil }
func (m *mockGCStore) LoadEventHistory(ctx context.Context, workflowID string) ([]EventRecord, error) { return nil, nil }
func (m *mockGCStore) LoadEventHistoryPaginated(ctx context.Context, workflowID string, offset, limit int) ([]EventRecord, error) { return nil, nil }
func (m *mockGCStore) AppendEventHistory(ctx context.Context, workflowID string, rec EventRecord) error { return nil }
func (m *mockGCStore) AppendEventHistoryBatch(ctx context.Context, workflowID string, recs []EventRecord) error { return nil }
func (m *mockGCStore) VerifyWorkflowEvents(ctx context.Context, workflowID string) error { return nil }
func (m *mockGCStore) LoadWASM(ctx context.Context, defName string, defVersion int) ([]byte, error) { return nil, nil }
func (m *mockGCStore) GetWASMLength(ctx context.Context, defName string, defVersion int) (int64, error) { return 0, nil }
func (m *mockGCStore) ListVersions(ctx context.Context, defName string) ([]int, error) { return nil, nil }
func (m *mockGCStore) Heartbeat(ctx context.Context, workflowID, workerID string, generation int64) (bool, error) { return false, nil }
func (m *mockGCStore) CompleteWorkflow(ctx context.Context, workflowID, workerID string, generation int64, result string, queryState map[string]string) error { return nil }
func (m *mockGCStore) FailWorkflow(ctx context.Context, workflowID, workerID string, generation int64, errorMsg, errorCode, errorOp string, queryState map[string]string) error { return nil }
func (m *mockGCStore) MoveToDeadLetterQueue(ctx context.Context, workflowID, workerID string, generation int64, errMsg, errorCode, errorOp string) error { return nil }
func (m *mockGCStore) RetryWorkflow(ctx context.Context, workflowID string) error { return nil }
func (m *mockGCStore) ReleaseWorkflow(ctx context.Context, workflowID, workerID string, generation int64, nextWakeAt time.Time) error { return nil }
func (m *mockGCStore) RequestCancellation(ctx context.Context, workflowID, reason string) error { return nil }
func (m *mockGCStore) CheckCancellation(ctx context.Context, workflowID string) (cancelled bool, reason string, err error) { return false, "", nil }
func (m *mockGCStore) DeliverSignal(ctx context.Context, workflowID, signalName, payload string) error { return nil }
func (m *mockGCStore) PollAndClaimSignal(ctx context.Context, workflowID, signalName string) (payload string, found bool, err error) { return "", false, nil }
func (m *mockGCStore) StartNewRun(ctx context.Context, runID, defName string, defVersion int, input json.RawMessage, idempotencyKey, tenantID string, priority int) (string, bool, error) { return "", false, nil }
func (m *mockGCStore) StartChildWorkflow(ctx context.Context, parentID, defName, inputJSON string, defVersion int, parentClosePolicy string, priority int) (runID string, err error) { return "", nil }
func (m *mockGCStore) StartChildWorkflowAtomic(ctx context.Context, childID, parentID, defName, inputJSON string, defVersion int, parentClosePolicy string, event EventRecord, priority int) (runID string, err error) { return m.StartChildWorkflow(ctx, parentID, defName, inputJSON, defVersion, parentClosePolicy, priority) }
func (m *mockGCStore) GetChildResult(ctx context.Context, runID string) (resultJSON string, completed bool, err error) { return "", false, nil }
func (m *mockGCStore) ReapStaleInstances(ctx context.Context, timeout time.Duration) (int, error) { return 0, nil }
func (m *mockGCStore) GetQueryState(ctx context.Context, workflowID, key string) (string, error) { return "", nil }
func (m *mockGCStore) ListWorkflows(ctx context.Context, filter WorkflowFilter) ([]WorkflowInstance, error) { return nil, nil }
func (m *mockGCStore) GetWorkflowByID(ctx context.Context, id string) (*WorkflowInstance, error) { return nil, nil }
func (m *mockGCStore) CreateSchedule(ctx context.Context, s Schedule) error { return nil }
func (m *mockGCStore) ListSchedules(ctx context.Context) ([]Schedule, error) { return nil, nil }
func (m *mockGCStore) DeleteSchedule(ctx context.Context, name string) error { return nil }
func (m *mockGCStore) SetScheduleEnabled(ctx context.Context, name string, enabled bool) error { return nil }
func (m *mockGCStore) GetDueSchedules(ctx context.Context) ([]Schedule, error) { return nil, nil }
func (m *mockGCStore) UpdateScheduleNextRun(ctx context.Context, name string, nextRun time.Time) error { return nil }
func (m *mockGCStore) LoadWorkflowConfig(ctx context.Context, defName string, defVersion int) (maxHistoryLength int, err error) { return 0, nil }
func (m *mockGCStore) LoadDAGSpec(ctx context.Context, defName string, defVersion int) (json.RawMessage, error) { return nil, nil }
func (m *mockGCStore) TraceWorkflow(ctx context.Context, workflowID, traceID string) error { return nil }
func (m *mockGCStore) GetCompactionCandidates(ctx context.Context, threshold int, limit int) ([]string, error) { return nil, nil }
func (m *mockGCStore) LoadCompactionState(ctx context.Context, workflowID string) (*CompactionState, error) { return nil, nil }
func (m *mockGCStore) CompactHistory(ctx context.Context, workflowID string, compactionState []byte, compactionStep int, keepStep int) error { return nil }
func (m *mockGCStore) CreatePromise(ctx context.Context, workflowID, promiseName, promiseID string) error { return nil }
func (m *mockGCStore) ResolvePromise(ctx context.Context, workflowID, promiseID, result string) error { return nil }
func (m *mockGCStore) RejectPromise(ctx context.Context, workflowID, promiseID, errMsg string) error { return nil }
func (m *mockGCStore) GetPromise(ctx context.Context, workflowID, promiseID string) (status string, result string, errMsg string, err error) { return "", "", "", nil }
func (m *mockGCStore) ListPromises(ctx context.Context, workflowID string) ([]PromiseInfo, error) { return nil, nil }
func (m *mockGCStore) CreateUpdateRequest(ctx context.Context, workflowID, updateName, payload, promiseID string) error { return nil }
func (m *mockGCStore) GetPendingUpdateRequests(ctx context.Context, workflowID string) ([]UpdateRequestInfo, error) { return nil, nil }
func (m *mockGCStore) CompleteUpdateRequest(ctx context.Context, workflowID, updateName, result, errMsg string) error { return nil }
func (m *mockGCStore) AcquireConcurrencyKey(ctx context.Context, key, workflowID string, ttl time.Duration) (acquired bool, err error) { return false, nil }
func (m *mockGCStore) ReleaseConcurrencyKey(ctx context.Context, key string) error { return nil }
func (m *mockGCStore) ReleaseWorkflowConcurrencyKeys(ctx context.Context, workflowID string) error { return nil }
func (m *mockGCStore) ReapExpiredConcurrencyKeys(ctx context.Context) (int64, error) { return 0, nil }
func (m *mockGCStore) UpdateStickyWorker(ctx context.Context, workflowID, workerID string) error { return nil }
func (m *mockGCStore) ClearStickyWorker(ctx context.Context, workflowID string) error { return nil }
func (m *mockGCStore) DeployWorkflowDef(ctx context.Context, def *WorkflowDef) error { return nil }
func (m *mockGCStore) GetWorkflowDef(ctx context.Context, name string, version int) (*WorkflowDef, error) { return nil, nil }
func (m *mockGCStore) MarkVersionDeprecated(ctx context.Context, name string, version int, deprecated bool) error { return nil }
func (m *mockGCStore) CountActiveInstances(ctx context.Context, name string, version int) (int, error) { return 0, nil }
func (m *mockGCStore) RecordWorkflowMemorySample(ctx context.Context, defName string, sampleBytes int64) error { return nil }
func (m *mockGCStore) LoadMemoryEstimates(ctx context.Context) (map[string]float64, error) { return nil, nil }
func (m *mockGCStore) LoadMemoryStats(ctx context.Context) ([]WorkflowMemoryStats, error) { return nil, nil }
func (m *mockGCStore) QueueDepth(ctx context.Context) (int64, error) { return 0, nil }
func (m *mockGCStore) CleanupMemorySamples(ctx context.Context, maxSamplesPerDef int) (int64, error) { return 0, nil }
func (m *mockGCStore) DeleteExpiredEvents(ctx context.Context, olderThan time.Time) (int64, error) { return 0, nil }
func (m *mockGCStore) ResolveTenantFromAPIKey(ctx context.Context, keyHash []byte) (uuid.UUID, error) { return uuid.Nil, nil }
func (m *mockPurgeStore) ClaimWorkflow(ctx context.Context, workerID string) (*WorkflowInstance, error) { return nil, nil }
func (m *mockPurgeStore) ClaimWorkflows(ctx context.Context, workerID string, limit int) ([]*WorkflowInstance, error) { return nil, nil }
func (m *mockPurgeStore) ClaimStickyWorkflows(ctx context.Context, workerID string, limit int) ([]*WorkflowInstance, error) { return nil, nil }
func (m *mockPurgeStore) LoadEventHistory(ctx context.Context, workflowID string) ([]EventRecord, error) { return nil, nil }
func (m *mockPurgeStore) LoadEventHistoryPaginated(ctx context.Context, workflowID string, offset, limit int) ([]EventRecord, error) { return nil, nil }
func (m *mockPurgeStore) AppendEventHistory(ctx context.Context, workflowID string, rec EventRecord) error { return nil }
func (m *mockPurgeStore) AppendEventHistoryBatch(ctx context.Context, workflowID string, recs []EventRecord) error { return nil }
func (m *mockPurgeStore) VerifyWorkflowEvents(ctx context.Context, workflowID string) error { return nil }
func (m *mockPurgeStore) LoadWASM(ctx context.Context, defName string, defVersion int) ([]byte, error) { return nil, nil }
func (m *mockPurgeStore) GetWASMLength(ctx context.Context, defName string, defVersion int) (int64, error) { return 0, nil }
func (m *mockPurgeStore) ListVersions(ctx context.Context, defName string) ([]int, error) { return nil, nil }
func (m *mockPurgeStore) Heartbeat(ctx context.Context, workflowID, workerID string, generation int64) (bool, error) { return false, nil }
func (m *mockPurgeStore) CompleteWorkflow(ctx context.Context, workflowID, workerID string, generation int64, result string, queryState map[string]string) error { return nil }
func (m *mockPurgeStore) FailWorkflow(ctx context.Context, workflowID, workerID string, generation int64, errorMsg, errorCode, errorOp string, queryState map[string]string) error { return nil }
func (m *mockPurgeStore) MoveToDeadLetterQueue(ctx context.Context, workflowID, workerID string, generation int64, errMsg, errorCode, errorOp string) error { return nil }
func (m *mockPurgeStore) ReleaseWorkflow(ctx context.Context, workflowID, workerID string, generation int64, nextWakeAt time.Time) error { return nil }
func (m *mockPurgeStore) RequestCancellation(ctx context.Context, workflowID, reason string) error { return nil }
func (m *mockPurgeStore) CheckCancellation(ctx context.Context, workflowID string) (cancelled bool, reason string, err error) { return false, "", nil }
func (m *mockPurgeStore) DeliverSignal(ctx context.Context, workflowID, signalName, payload string) error { return nil }
func (m *mockPurgeStore) PollAndClaimSignal(ctx context.Context, workflowID, signalName string) (payload string, found bool, err error) { return "", false, nil }
func (m *mockPurgeStore) StartNewRun(ctx context.Context, runID, defName string, defVersion int, input json.RawMessage, idempotencyKey, tenantID string, priority int) (string, bool, error) { return "", false, nil }
func (m *mockPurgeStore) StartChildWorkflow(ctx context.Context, parentID, defName, inputJSON string, defVersion int, parentClosePolicy string, priority int) (runID string, err error) { return "", nil }
func (m *mockPurgeStore) StartChildWorkflowAtomic(ctx context.Context, childID, parentID, defName, inputJSON string, defVersion int, parentClosePolicy string, event EventRecord, priority int) (runID string, err error) { return m.StartChildWorkflow(ctx, parentID, defName, inputJSON, defVersion, parentClosePolicy, priority) }
func (m *mockPurgeStore) GetChildResult(ctx context.Context, runID string) (resultJSON string, completed bool, err error) { return "", false, nil }
func (m *mockPurgeStore) ReapStaleInstances(ctx context.Context, timeout time.Duration) (int, error) { return 0, nil }
func (m *mockPurgeStore) GetQueryState(ctx context.Context, workflowID, key string) (string, error) { return "", nil }
func (m *mockPurgeStore) ListWorkflows(ctx context.Context, filter WorkflowFilter) ([]WorkflowInstance, error) { return nil, nil }
func (m *mockPurgeStore) GetWorkflowByID(ctx context.Context, id string) (*WorkflowInstance, error) { return nil, nil }
func (m *mockPurgeStore) CreateSchedule(ctx context.Context, s Schedule) error { return nil }
func (m *mockPurgeStore) ListSchedules(ctx context.Context) ([]Schedule, error) { return nil, nil }
func (m *mockPurgeStore) DeleteSchedule(ctx context.Context, name string) error { return nil }
func (m *mockPurgeStore) SetScheduleEnabled(ctx context.Context, name string, enabled bool) error { return nil }
func (m *mockPurgeStore) GetDueSchedules(ctx context.Context) ([]Schedule, error) { return nil, nil }
func (m *mockPurgeStore) UpdateScheduleNextRun(ctx context.Context, name string, nextRun time.Time) error { return nil }
func (m *mockPurgeStore) LoadWorkflowConfig(ctx context.Context, defName string, defVersion int) (maxHistoryLength int, err error) { return 0, nil }
func (m *mockPurgeStore) LoadDAGSpec(ctx context.Context, defName string, defVersion int) (json.RawMessage, error) { return nil, nil }
func (m *mockPurgeStore) TraceWorkflow(ctx context.Context, workflowID, traceID string) error { return nil }
func (m *mockPurgeStore) GetCompactionCandidates(ctx context.Context, threshold int, limit int) ([]string, error) { return nil, nil }
func (m *mockPurgeStore) LoadCompactionState(ctx context.Context, workflowID string) (*CompactionState, error) { return nil, nil }
func (m *mockPurgeStore) CompactHistory(ctx context.Context, workflowID string, compactionState []byte, compactionStep int, keepStep int) error { return nil }
func (m *mockPurgeStore) CreatePromise(ctx context.Context, workflowID, promiseName, promiseID string) error { return nil }
func (m *mockPurgeStore) ResolvePromise(ctx context.Context, workflowID, promiseID, result string) error { return nil }
func (m *mockPurgeStore) RejectPromise(ctx context.Context, workflowID, promiseID, errMsg string) error { return nil }
func (m *mockPurgeStore) GetPromise(ctx context.Context, workflowID, promiseID string) (status string, result string, errMsg string, err error) { return "", "", "", nil }
func (m *mockPurgeStore) ListPromises(ctx context.Context, workflowID string) ([]PromiseInfo, error) { return nil, nil }
func (m *mockPurgeStore) CreateUpdateRequest(ctx context.Context, workflowID, updateName, payload, promiseID string) error { return nil }
func (m *mockPurgeStore) GetPendingUpdateRequests(ctx context.Context, workflowID string) ([]UpdateRequestInfo, error) { return nil, nil }
func (m *mockPurgeStore) CompleteUpdateRequest(ctx context.Context, workflowID, updateName, result, errMsg string) error { return nil }
func (m *mockPurgeStore) AcquireConcurrencyKey(ctx context.Context, key, workflowID string, ttl time.Duration) (acquired bool, err error) { return false, nil }
func (m *mockPurgeStore) ReleaseConcurrencyKey(ctx context.Context, key string) error { return nil }
func (m *mockPurgeStore) ReleaseWorkflowConcurrencyKeys(ctx context.Context, workflowID string) error { return nil }
func (m *mockPurgeStore) ReapExpiredConcurrencyKeys(ctx context.Context) (int64, error) { return 0, nil }
func (m *mockPurgeStore) UpdateStickyWorker(ctx context.Context, workflowID, workerID string) error { return nil }
func (m *mockPurgeStore) ClearStickyWorker(ctx context.Context, workflowID string) error { return nil }
func (m *mockPurgeStore) DeployWorkflowDef(ctx context.Context, def *WorkflowDef) error { return nil }
func (m *mockPurgeStore) GetWorkflowDef(ctx context.Context, name string, version int) (*WorkflowDef, error) { return nil, nil }
func (m *mockPurgeStore) MarkVersionDeprecated(ctx context.Context, name string, version int, deprecated bool) error { return nil }
func (m *mockPurgeStore) GetActiveInstanceCountsByVersion(ctx context.Context) (map[string]int, error) { return nil, nil }
func (m *mockPurgeStore) RecordWorkflowMemorySample(ctx context.Context, defName string, sampleBytes int64) error { return nil }
func (m *mockPurgeStore) LoadMemoryEstimates(ctx context.Context) (map[string]float64, error) { return nil, nil }
func (m *mockPurgeStore) LoadMemoryStats(ctx context.Context) ([]WorkflowMemoryStats, error) { return nil, nil }
func (m *mockPurgeStore) QueueDepth(ctx context.Context) (int64, error) { return 0, nil }
func (m *mockPurgeStore) CleanupMemorySamples(ctx context.Context, maxSamplesPerDef int) (int64, error) { return 0, nil }
func (m *mockPurgeStore) DeleteExpiredEvents(ctx context.Context, olderThan time.Time) (int64, error) { return 0, nil }
func (m *mockPurgeStore) ResolveTenantFromAPIKey(ctx context.Context, keyHash []byte) (uuid.UUID, error) { return uuid.Nil, nil }
