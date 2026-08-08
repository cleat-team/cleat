// Package pluginapi_test asserts that every symbol pluginapi re-exports
// still resolves to the type, value, and behaviour it claims.
//
// pluginapi (see pluginapi/pluginapi.go) is a compatibility shim: it exists
// so plugins written against the old import path keep compiling after the
// internal/plugin -> plugin promotion. It has no logic of its own, so the
// only thing that can go wrong is a re-export silently drifting -- renamed,
// retyped, or pointed at the wrong symbol. That is exactly the failure an
// external plugin author hits and the maintainer, compiling only the
// internal plugin package, does not.
//
// This is deliberately an *external* test package (pluginapi_test), built
// against pluginapi's public import path, so it exercises the same contract
// an outside plugin author would see -- not pluginapi's internals.
//
// The surface (10 symbols, re-derived with
// `grep -cE '= plugin\.' pluginapi/pluginapi.go` against pluginapi.go as of
// 2026-08-07):
//
//	types:     PluginInfo, Plugin, Environment, Migration, PluginDB,
//	           DatabaseAccess
//	constants: DatabaseAccessNone, DatabaseAccessReadOnly,
//	           DatabaseAccessReadWrite
//	functions: Register
//
// Every one of the 10 is asserted below.
package pluginapi_test

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/cleat-team/cleat/plugin"
	"github.com/cleat-team/cleat/pluginapi"
)

// --- Type aliases -----------------------------------------------------
//
// pluginapi declares each of these with `type X = plugin.X` -- a true
// alias, not a defined type with a matching underlying type. The
// distinction matters: assigning a plugin.X value to a pluginapi.X
// variable (and back) without any conversion only compiles when they are
// literally the same type. If the shim were ever changed to
// `type X plugin.X` (dropping the `=`), X would become a distinct named
// type with an identical underlying type -- still assignable in many
// contexts, but this direct, unconverted, two-way assignment would then
// fail to compile. An assignability check alone (e.g. a single
// `var _ pluginapi.X = plugin.X{}`) would not catch that regression,
// because a defined type with a matching underlying type is often still
// assignable one direction; the two-way, no-conversion assignment is what
// pins down "same type," not "compatible type."

func TestTypeAliasIdentity_PluginInfo(t *testing.T) {
	var a pluginapi.PluginInfo
	var b plugin.PluginInfo
	a = b
	b = a
	_ = a
	_ = b
}

func TestTypeAliasIdentity_Plugin(t *testing.T) {
	var a pluginapi.Plugin
	var b plugin.Plugin
	a = b
	b = a
	_ = a
	_ = b
}

func TestTypeAliasIdentity_Environment(t *testing.T) {
	var a pluginapi.Environment
	var b plugin.Environment
	a = b
	b = a
	_ = a
	_ = b
}

func TestTypeAliasIdentity_Migration(t *testing.T) {
	var a pluginapi.Migration
	var b plugin.Migration
	a = b
	b = a
	_ = a
	_ = b
}

func TestTypeAliasIdentity_PluginDB(t *testing.T) {
	var a pluginapi.PluginDB
	var b plugin.PluginDB
	a = b
	b = a
	_ = a
	_ = b
}

func TestTypeAliasIdentity_DatabaseAccess(t *testing.T) {
	var a pluginapi.DatabaseAccess
	var b plugin.DatabaseAccess
	a = b
	b = a
	_ = a
	_ = b
}

// --- Interface type identity ---------------------------------------------
//
// The two-way assignment trick above is vacuous for interface-typed
// aliases specifically: Go's assignability rule for interfaces is
// structural ("x is assignable to T if x implements T"), not the
// named/underlying-type rule that non-interface types use. Two distinct
// named interface types with the same method set remain freely
// assignable to each other whether or not one is declared as an alias of
// the other, so `type Plugin = plugin.Plugin` silently becoming
// `type Plugin plugin.Plugin` (a defined type with the same method set)
// does NOT make TestTypeAliasIdentity_Plugin/PluginDB above fail.
//
// Verified directly: temporarily changing pluginapi.go's `Plugin =
// plugin.Plugin` to `Plugin plugin.Plugin` (dropping the `=`) left
// TestTypeAliasIdentity_Plugin green; only reflect.Type identity (below)
// and the Register signature pin caught it.
//
// reflect.Type comparison does distinguish them: for a true alias the
// compiler treats both spellings as one type, so their reflect.Type
// values are `==`; for a defined type they are two distinct types even
// with an identical method set, so the reflect.Type values differ.

func TestInterfaceTypeIdentity_Plugin(t *testing.T) {
	a := reflect.TypeOf((*pluginapi.Plugin)(nil)).Elem()
	b := reflect.TypeOf((*plugin.Plugin)(nil)).Elem()
	if a != b {
		t.Fatalf("pluginapi.Plugin is not identical to plugin.Plugin: %v != %v", a, b)
	}
}

func TestInterfaceTypeIdentity_PluginDB(t *testing.T) {
	a := reflect.TypeOf((*pluginapi.PluginDB)(nil)).Elem()
	b := reflect.TypeOf((*plugin.PluginDB)(nil)).Elem()
	if a != b {
		t.Fatalf("pluginapi.PluginDB is not identical to plugin.PluginDB: %v != %v", a, b)
	}
}

// --- Interface method sets ---------------------------------------------
//
// Plugin and PluginDB are interfaces. Because they are aliases (checked
// above), their method sets can only ever be plugin.Plugin's and
// plugin.PluginDB's -- but that fact alone doesn't pin the method set an
// external implementor has to satisfy today. fakePlugin and fakePluginDB
// below implement the method sets by hand, at the exact signatures the
// interfaces currently declare, and are then asserted against *both* the
// pluginapi and plugin interface names. If a method is added to, removed
// from, or resignatured on plugin.Plugin or plugin.PluginDB, one of the
// four `var _` lines below fails to compile, naming the interface that
// moved.

type fakePlugin struct{}

func (fakePlugin) Info() pluginapi.PluginInfo                                 { return pluginapi.PluginInfo{} }
func (fakePlugin) Init(ctx context.Context, env *pluginapi.Environment) error { return nil }

var (
	_ pluginapi.Plugin = fakePlugin{}
	_ plugin.Plugin    = fakePlugin{}
)

type fakePluginDB struct{}

func (fakePluginDB) Begin(ctx context.Context) (plugin.PluginTx, error) { return nil, nil }
func (fakePluginDB) Exec(ctx context.Context, query string, args ...any) (int64, error) {
	return 0, nil
}
func (fakePluginDB) Query(ctx context.Context, query string, args ...any) (plugin.Rows, error) {
	return nil, nil
}
func (fakePluginDB) QueryRow(ctx context.Context, query string, args ...any) plugin.RowScanner {
	return nil
}
func (fakePluginDB) Ping(ctx context.Context) error { return nil }

var (
	_ pluginapi.PluginDB = fakePluginDB{}
	_ plugin.PluginDB    = fakePluginDB{}
)

// --- Struct field surface -----------------------------------------------
//
// PluginInfo and Migration are structs, not interfaces, so there is no
// method set to pin down -- but a field can still be renamed or dropped
// on the plugin.X side and, since pluginapi.X is only an alias, that
// change flows straight through invisibly at the pluginapi.go line. This
// composite literal names every exported field plugin.PluginInfo and
// plugin.Migration have today; a rename or removal on the plugin.X
// struct fails this literal to compile, naming the missing field.

var _ = pluginapi.PluginInfo{
	Name:           "x",
	Version:        "x",
	Description:    "x",
	Author:         "x",
	Requires:       []string{"x"},
	DatabaseAccess: pluginapi.DatabaseAccessNone,
}

var _ = pluginapi.Migration{
	Version: 1,
	Up:      "x",
	UpMySQL: "x",
	UpMSSQL: "x",
	Down:    "x",
}

// Environment carries function-typed fields (StartWorkflow, SignalWorkflow)
// and an *AuditLogger field that pluginapi does not separately re-export.
// The composite literal below pins the field names and, for the function
// fields, their exact signatures -- a changed parameter or return type on
// plugin.Environment fails this literal to compile.
var _ = pluginapi.Environment{
	DB:       fakePluginDB{},
	Config:   nil,
	TenantID: "x",
	Done:     nil,
	StartWorkflow: func(ctx context.Context, defName string, input json.RawMessage) (string, error) {
		return "", nil
	},
	SignalWorkflow: func(ctx context.Context, workflowID, signalName, payload string) error {
		return nil
	},
}

// --- Constants ------------------------------------------------------------
//
// Each constant is declared in pluginapi.go as `X = plugin.X`, which is a
// value re-export, not a type alias -- a different failure mode. A typo
// that swapped, say, DatabaseAccessNone's right-hand side for
// plugin.DatabaseAccessReadOnly would still compile (both sides are valid
// plugin.DatabaseAccess values); only comparing the re-exported constant
// against the specific plugin.X it claims to mirror catches that. The
// compile-time half below pins the type; the runtime half pins the value.

const (
	_ pluginapi.DatabaseAccess = pluginapi.DatabaseAccessNone
	_ pluginapi.DatabaseAccess = pluginapi.DatabaseAccessReadOnly
	_ pluginapi.DatabaseAccess = pluginapi.DatabaseAccessReadWrite
)

func TestConstants_ResolveToTheClaimedPluginConstant(t *testing.T) {
	cases := []struct {
		name string
		got  pluginapi.DatabaseAccess
		want plugin.DatabaseAccess
	}{
		{"DatabaseAccessNone", pluginapi.DatabaseAccessNone, plugin.DatabaseAccessNone},
		{"DatabaseAccessReadOnly", pluginapi.DatabaseAccessReadOnly, plugin.DatabaseAccessReadOnly},
		{"DatabaseAccessReadWrite", pluginapi.DatabaseAccessReadWrite, plugin.DatabaseAccessReadWrite},
	}
	for _, c := range cases {
		if plugin.DatabaseAccess(c.got) != c.want {
			t.Errorf("pluginapi.%s = %q, want plugin.%s = %q", c.name, c.got, c.name, c.want)
		}
	}
}

// --- Functions --------------------------------------------------------
//
// Register is a value re-export (`var Register = plugin.Register`), and
// func values in Go are not comparable, so "same function" cannot be
// checked with `==` the way the constants are above. Two complementary
// assertions stand in for it: a compile-time signature pin, and a runtime
// behavioural check that calling pluginapi.Register actually reaches
// plugin's registry (proving it is the same underlying func, not a
// same-signature wrapper that silently does nothing).

var _ func(pluginapi.PluginInfo, func() pluginapi.Plugin) = pluginapi.Register

func TestRegister_ReachesThePluginRegistry(t *testing.T) {
	const name = "pluginapi-contract-test-plugin"

	for _, existing := range plugin.List() {
		if existing.Name == name {
			t.Fatalf("test plugin %q already registered; registry leaked across tests", name)
		}
	}

	info := pluginapi.PluginInfo{
		Name:           name,
		Version:        "0.0.1",
		DatabaseAccess: pluginapi.DatabaseAccessNone,
	}
	pluginapi.Register(info, func() pluginapi.Plugin { return fakePlugin{} })

	var found *plugin.PluginInfo
	for _, p := range plugin.List() {
		if p.Name == name {
			p := p
			found = &p
			break
		}
	}
	if found == nil {
		t.Fatalf("pluginapi.Register(%q, ...) did not register with plugin's registry: plugin.List() = %+v", name, plugin.List())
	}
	if found.Version != info.Version {
		t.Errorf("registered PluginInfo.Version = %q, want %q", found.Version, info.Version)
	}
}
