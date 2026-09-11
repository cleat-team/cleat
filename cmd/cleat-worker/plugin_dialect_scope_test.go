package main

import (
	"testing"

	"github.com/cleat-team/cleat/plugin"
)

// cleat#1157: a plugin declares its dialect scope by omitting UpMySQL/UpMSSQL,
// the migration runner reads that declaration correctly, records the version as
// done, logs one line -- and then nothing acts on it. The plugin initialises
// normally and every query fails against tables that were never created.
//
// THIS GUARD MAKES THE DECLARATION CONSULTED, at build time, for the set that
// actually ships. It does not change what happens at runtime, because on the
// measured evidence the runtime case cannot currently arise:
//
//   - Exactly ONE of 18 plugins with migrations declares a restriction --
//     pgvector, which provides neither dialect arm. The other 17 provide both.
//   - pgvector is deliberately NOT linked into this binary. The import block in
//     main.go has it commented out, for a stronger reason than #1157's: its
//     migration creates `embedding vector(1536)`, which is fatal on any
//     PostgreSQL without the vector extension.
//
// So the defect is latent rather than live, and the cheapest honest response is
// to make it fail LOUDLY the moment it stops being latent, rather than to add
// runtime machinery for a state no shipped configuration reaches.
//
// Linking a dialect-restricted plugin -- pgvector, or any future one that omits
// an arm -- turns that silent runtime failure into this test failing by name.
func TestEveryLinkedPluginSupportsEveryDialectTheWorkerRunsOn(t *testing.T) {
	loaded, err := plugin.Discover()
	if err != nil {
		t.Fatalf("discovering registered plugins: %v", err)
	}

	// A floor on the INPUT, not the answer. If Discover returns nothing -- a
	// registry that did not populate, an import block that moved -- every
	// assertion below passes vacuously and this test reports a clean bill of
	// health for a set it never examined.
	if len(loaded) < 10 {
		t.Fatalf("only %d plugins registered; the import block in main.go is the "+
			"source of this set, so this guard is reporting on nothing (17 linked "+
			"on 2026-09-11)", len(loaded))
	}

	type gap struct {
		plugin  string
		version int
		missing string
	}
	var gaps []gap

	withMigrations := 0
	for _, lp := range loaded {
		// HasMigrations is optional: a plugin with no tables declares no dialect
		// scope and is not this test's subject. Counted so the floor below can
		// tell "no plugin has migrations" -- which would make every assertion
		// vacuous -- from "none of them has a gap".
		p, ok := lp.Plugin.(plugin.HasMigrations)
		if !ok {
			continue
		}
		withMigrations++

		name := lp.Plugin.Info().Name
		for _, m := range p.Migrations() {
			// Up is the PostgreSQL arm and the fallback; a migration with no Up
			// at all is a different defect and not this test's subject.
			if m.UpMySQL == "" {
				gaps = append(gaps, gap{name, m.Version, "UpMySQL"})
			}
			if m.UpMSSQL == "" {
				gaps = append(gaps, gap{name, m.Version, "UpMSSQL"})
			}
		}
	}

	if withMigrations < 10 {
		t.Fatalf("only %d of %d linked plugins implement HasMigrations; this guard "+
			"examines migrations, so that is a population it cannot report on "+
			"(18 on 2026-09-11)", withMigrations, len(loaded))
	}

	for _, g := range gaps {
		t.Errorf("plugin %q migration v%d has no %s, so on that dialect the "+
			"migration is SKIPPED, the version is recorded as done, and the plugin "+
			"initialises anyway against tables that were never created (cleat#1157).\n\n"+
			"  If this plugin genuinely cannot run on that dialect, it must not be "+
			"linked into cleat-worker -- the import block in main.go is where that "+
			"is decided, and pgvector is the worked example.\n"+
			"  If it can, supply the dialect arm.",
			g.plugin, g.version, g.missing)
	}
}
