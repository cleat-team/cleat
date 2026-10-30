package engine

// cleat#3600 (owner D52): a deploy or a library install refuses a definition whose plugin
// dependencies are unmet, through one check.
//
// "Met" here is the owner's definition and a deliberately weak one: some worker has registered
// a plugin of that NAME at least once (PluginSource). It catches a typo and a plugin that was
// never installed anywhere, and it does not fail across a rolling restart or a maintenance
// window. It says nothing about versions or about whether a worker carrying the plugin is up
// now: the claiming worker still checks its own loaded set and versions at claim time
// (checkPluginDeps in cmd/cleat-worker), and that stays the backstop. So a pass here is advisory
// at write time, and the claim-time release is what actually decides whether a run can proceed.
//
// Two declarations are read. The worker enforces the WASM binary's embedded plugin_deps, and
// the definition row carries a plugin_deps value that four different writers fill in four
// different ways (a REST body, the previous version's row, the binary's, or nothing), so the
// two can disagree. A dep in either must be met, and the error says which one declared it.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/cleat-team/cleat/wasm"
)

// ErrUnmetPluginDeps is what CheckPluginDeps returns, wrapped in an UnmetPluginDepsError,
// when a declared plugin has never been registered by any worker.
var ErrUnmetPluginDeps = errors.New("plugin dependencies are not met")

// ErrPluginSourceUnavailable is returned when there are dependencies to check and nothing to
// check them against. It fails closed: a store that cannot say which plugins are known must
// not be read as "all of them".
var ErrPluginSourceUnavailable = errors.New("cannot check plugin dependencies: this store does not record which plugins workers have registered")

// Which declaration named a missing plugin.
const (
	DeclaredByBinary       = "the WASM binary"
	DeclaredByRow          = "the definition"
	DeclaredByBinaryAndRow = "the WASM binary and the definition"
)

// MissingPlugin is one unmet dependency.
type MissingPlugin struct {
	Name string
	// Constraint is the version constraint as declared. When the binary and the row both
	// declare the plugin with different constraints, both are shown ("a; b").
	Constraint string
	DeclaredBy string
}

// UnmetPluginDepsError lists every unmet dependency, not just the first.
type UnmetPluginDepsError struct {
	Missing []MissingPlugin
}

func (e *UnmetPluginDepsError) Error() string {
	parts := make([]string, len(e.Missing))
	for i, m := range e.Missing {
		c := m.Constraint
		if c == "" {
			c = "any version"
		}
		parts[i] = fmt.Sprintf("%q (%s), declared by %s", m.Name, c, m.DeclaredBy)
	}
	return "plugin dependencies are not met; no worker has ever registered a plugin named: " + strings.Join(parts, "; ")
}

func (e *UnmetPluginDepsError) Is(target error) bool { return target == ErrUnmetPluginDeps }

// MissingNames is the names alone, sorted, for a log line.
func (e *UnmetPluginDepsError) MissingNames() []string {
	out := make([]string, len(e.Missing))
	for i, m := range e.Missing {
		out[i] = m.Name
	}
	return out
}

// BinaryPluginDeps returns the plugin_deps embedded in a WASM binary's cleat metadata, or nil
// when it carries none or cannot be read. An unreadable binary is not an error here: the
// deploy paths already treat a binary with no readable metadata as one that declares nothing,
// and refusing it is a different rule from this one.
func BinaryPluginDeps(wasmBytes []byte) map[string]string {
	meta, err := wasm.ReadMetadata(wasmBytes)
	if err != nil || meta == nil {
		return nil
	}
	return meta.PluginDeps
}

// CheckPluginDeps returns an *UnmetPluginDepsError naming every plugin declared in binary or
// row that src does not know, or nil. With nothing declared it returns nil without touching
// src. With something declared and a nil src it returns ErrPluginSourceUnavailable.
func CheckPluginDeps(ctx context.Context, src PluginSource, binary, row map[string]string) error {
	declared := make(map[string]struct{ binary, row bool })
	for n := range binary {
		d := declared[n]
		d.binary = true
		declared[n] = d
	}
	for n := range row {
		d := declared[n]
		d.row = true
		declared[n] = d
	}
	if len(declared) == 0 {
		return nil
	}
	if src == nil {
		return ErrPluginSourceUnavailable
	}
	names := make([]string, 0, len(declared))
	for n := range declared {
		names = append(names, n)
	}
	sort.Strings(names)
	known, err := src.KnownPlugins(ctx, names)
	if err != nil {
		return fmt.Errorf("check plugin dependencies: %w", err)
	}
	var missing []MissingPlugin
	for _, n := range names {
		if known[n] {
			continue
		}
		d := declared[n]
		m := MissingPlugin{Name: n}
		switch {
		case d.binary && d.row:
			m.DeclaredBy = DeclaredByBinaryAndRow
			m.Constraint = binary[n]
			if binary[n] != row[n] {
				m.Constraint = binary[n] + "; " + row[n]
			}
		case d.binary:
			m.DeclaredBy = DeclaredByBinary
			m.Constraint = binary[n]
		default:
			m.DeclaredBy = DeclaredByRow
			m.Constraint = row[n]
		}
		missing = append(missing, m)
	}
	if len(missing) == 0 {
		return nil
	}
	return &UnmetPluginDepsError{Missing: missing}
}

// EnforcePluginDeps is the one call every deploy path makes. allowMissing is the explicit
// override (--allow-missing-plugins, or the REST field): the check still runs, so the names
// are known, and when something is missing it logs them at WARN and returns nil. It does NOT
// suppress an error from reading the registry or ErrPluginSourceUnavailable, because those
// say the check could not be made, which is not the same as "a plugin is missing".
// There is no override on library install: that path calls CheckPluginDeps directly.
func EnforcePluginDeps(ctx context.Context, src PluginSource, binary, row map[string]string, allowMissing bool, log *slog.Logger) error {
	err := CheckPluginDeps(ctx, src, binary, row)
	var unmet *UnmetPluginDepsError
	if allowMissing && errors.As(err, &unmet) {
		if log == nil {
			log = slog.Default()
		}
		log.WarnContext(ctx, "deploying with unmet plugin dependencies because of --allow-missing-plugins; a run that needs them will be released until a worker carrying them registers",
			"missing_plugins", unmet.MissingNames(), "detail", unmet.Error())
		return nil
	}
	return err
}

// UnmetPluginDepsCLIAdvice is what a command-line deploy path prints after an
// *UnmetPluginDepsError, so every CLI says the same thing about the way out.
const UnmetPluginDepsCLIAdvice = "deploy the plugin to a worker first (a worker records its plugins when it starts), " +
	"or pass --allow-missing-plugins to deploy anyway; that is logged, and a run that needs the plugin is released until a worker carrying it registers."

// MemberPluginDeps is one library member's declared plugin_deps, as stored (JSON text).
type MemberPluginDeps struct {
	Name    string
	Version int
	// Raw is the member's plugin_deps column. Empty, NULL and "null" all mean none.
	Raw []byte
}

// CheckMemberPluginDeps is the install's form of the check: it reads every member's declared
// plugin_deps, asks src once, and returns an *UnmetPluginDepsError naming every unmet plugin
// with the member that declared it, so a bundle refused for three missing plugins says so in
// one answer. It reads the member's ROW declaration only: a member's bytes are not read here,
// so a plugin that only the member's binary embeds is not seen until a publisher records it
// (cleat#3597). There is no allow-missing form, by owner decision D52: an install goes into a
// running system and has no bootstrap-order excuse.
//
// A declaration that cannot be parsed refuses the install and says which member, because the
// alternative is to read an unreadable declaration as "no dependencies".
func CheckMemberPluginDeps(ctx context.Context, src PluginSource, library string, members []MemberPluginDeps) error {
	type decl struct {
		by         string
		constraint string
	}
	byPlugin := make(map[string][]decl)
	for _, m := range members {
		raw := strings.TrimSpace(string(m.Raw))
		if raw == "" || raw == "null" {
			continue
		}
		var deps map[string]string
		if err := json.Unmarshal([]byte(raw), &deps); err != nil {
			return fmt.Errorf("library %q member %q version %d: plugin_deps is not a JSON object of name to constraint: %w", library, m.Name, m.Version, err)
		}
		for n, c := range deps {
			byPlugin[n] = append(byPlugin[n], decl{
				by:         fmt.Sprintf("member %q version %d of library %q", m.Name, m.Version, library),
				constraint: c,
			})
		}
	}
	if len(byPlugin) == 0 {
		return nil
	}
	if src == nil {
		return ErrPluginSourceUnavailable
	}
	names := make([]string, 0, len(byPlugin))
	for n := range byPlugin {
		names = append(names, n)
	}
	sort.Strings(names)
	known, err := src.KnownPlugins(ctx, names)
	if err != nil {
		return fmt.Errorf("check plugin dependencies: %w", err)
	}
	var missing []MissingPlugin
	for _, n := range names {
		if known[n] {
			continue
		}
		decls := byPlugin[n]
		sort.Slice(decls, func(i, j int) bool { return decls[i].by < decls[j].by })
		for _, d := range decls {
			missing = append(missing, MissingPlugin{Name: n, Constraint: d.constraint, DeclaredBy: d.by})
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return &UnmetPluginDepsError{Missing: missing}
}

// querierPluginSource reads the registry on a transaction or connection the caller already
// holds, which is how the install checks inside its own transaction.
type querierPluginSource struct {
	q querier
	d Dialect
}

func (s querierPluginSource) KnownPlugins(ctx context.Context, names []string) (map[string]bool, error) {
	return knownPluginsOn(ctx, s.q, s.d, names)
}
