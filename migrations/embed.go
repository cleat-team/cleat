// Package migrations embeds this repository's SQL migration tree, so a
// binary applying them does not depend on its own working directory being a
// cleat source checkout -- cmd/cleat-worker did, before cleat#1968: it read
// a bare relative "migrations" path, which only resolved by coincidence when
// the worker happened to be started from the repo root. Started from a
// project scaffolded by `cleat init` -- the only layout a real user has --
// it refused to boot.
//
// This file is NOT a migration itself: migration.Runner only recognizes
// NNN_name.sql files (readMigrations in migration/runner.go), so a .go file
// here is invisible to it and the per-dialect numbered series is untouched.
package migrations

import "embed"

//go:embed postgres mysql mssql
var FS embed.FS
