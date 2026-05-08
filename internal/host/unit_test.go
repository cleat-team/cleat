package host

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rcownie/cleat/internal/plugin"
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
		{"1.2", semverVersion{}, true},       // need 3 parts
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
		a, b  semverVersion
		want  int
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
		{"1.4.2", true},   // bare = exact
		{"1.4.3", false},  // bare = exact, no match
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
		input       string
		wantMajor   int
		wantMinor   int
		wantPatch   int
	}{
		{"v1.2.3", 1, 2, 3},
		{"v0.0.0", 0, 0, 0},
		{"v10.20.30", 10, 20, 30},
		{"1.2.3", 1, 2, 3},         // no v prefix
		{"v1.2.3-beta", 1, 2, 3},   // with pre-release
		{"v1.2", 1, 2, 0},          // partial
		{"v1", 1, 0, 0},            // major only
		{"", 0, 0, 0},              // empty
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
		input    string
		wantMin  string
		wantMax  string
		wantErr  bool
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
	log.SetOutput(&buf)
	defer log.SetOutput(nil) // restore

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
	rt, err := NewRuntime(ctx)
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
	rt, err := NewRuntime(ctx)
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
	l := NewWorkflowLoader(nil, nil)
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
	l := NewWorkflowLoader(nil, nil, 42)
	stats := l.CacheStats()
	if stats.MaxSize != 42 {
		t.Errorf("expected MaxSize 42, got %d", stats.MaxSize)
	}
}

func TestNewWorkflowLoader_ZeroMaxSize(t *testing.T) {
	l := NewWorkflowLoader(nil, nil, 0)
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
	rt, err := NewRuntime(ctx)
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

func (c *multiRowConn) Close() error  { return nil }
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

func (s *multiRowStmt) Close() error    { return nil }
func (s *multiRowStmt) NumInput() int    { return -1 }

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
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	compiled, err := rt.CompileModule(ctx, minimalWasm())
	if err != nil {
		t.Fatalf("CompileModule: %v", err)
	}
	defer compiled.Close(ctx)

	l := NewWorkflowLoader(nil, rt, 10)

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
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	compiled, err := rt.CompileModule(ctx, minimalWasm())
	if err != nil {
		t.Fatalf("CompileModule: %v", err)
	}

	l := NewWorkflowLoader(nil, rt, 10)

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
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	// Max size 1 means the second insert should evict the first.
	l := NewWorkflowLoader(nil, rt, 1)

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
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	// Max size 2.
	l := NewWorkflowLoader(nil, rt, 2)

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
	l := NewWorkflowLoader(nil, nil, 10)
	// Evict on empty cache should not panic.
	l.evictLocked()
}

func TestWorkflowLoader_CacheUpdateExisting(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
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

	l := NewWorkflowLoader(nil, rt, 10)
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
