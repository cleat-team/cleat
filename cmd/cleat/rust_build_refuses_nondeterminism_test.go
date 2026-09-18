package main

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestRustBuildRefusesNondeterminism asserts that `cleat build --target rust`
// runs the determinism checker and refuses to emit an artifact when it finds
// something -- and, in the same run, states what the checker does not find.
//
// # Why both arms are in one test
//
// Either arm alone is satisfied by a tree where the gate does not exist.
//
//   - The known-limit arm asserts a build is NOT refused. An unwired gate
//     never refuses anything, so it passes that arm perfectly.
//   - The known-positive arm asserts a build IS refused -- and a refusal is
//     not evidence on its own, because a build can fail for a hundred reasons
//     that have nothing to do with determinism.
//
// That second one is not hypothetical. Measured on develop before this gate
// existed, `cleat build --target rust` on the e001_fs_access fixture already
// exited 1:
//
//	$ cleat build --target rust testdata/vet-checks/rust/e001_fs_access
//	error: failed to parse manifest ... no targets specified in the manifest
//	Error: cargo build failed: exit status 101
//	exit=1     mentions of R001 or "non-deterministic": 0
//
// The fixture keeps its sources at the crate root rather than under src/, so
// cargo rejects the manifest. A test asserting only "the build fails on a
// non-deterministic fixture" therefore PASSED against a tree with no gate at
// all -- the issue's own acceptance criterion, satisfied by an unrelated
// error. Hence: assert on the TEXT, and keep the arm that can only pass when
// something declines to refuse.
//
// # Why the known-limit arm does not assert an exit status
//
// The known-limit fixture passes the gate and then reaches cargo, which fails
// on the same crate-root layout. That exit status says nothing about the
// checker, and making the fixture cargo-buildable would couple a test about a
// SUBSTRING MATCHER to a working Rust toolchain. The property under test is
// "the gate did not refuse this", which is textual and needs no toolchain.
func TestRustBuildRefusesNondeterminism(t *testing.T) {
	if testing.Short() {
		t.Skip("needs the cleat binary, which TestMain does not build in short mode")
	}

	build := func(t *testing.T, fixture string) string {
		t.Helper()
		dir := filepath.Join("..", "..", "testdata", "vet-checks", "rust", fixture)
		out, _ := exec.Command(cleatBinary, "build", "--target", "rust", "-o", t.TempDir(), dir).CombinedOutput()
		return string(out)
	}

	// ARM 1 -- KNOWN POSITIVE. A plain single-module import of the standard
	// filesystem module resolves to a path on forbiddenRustPaths, so the gate must refuse
	// the build and must do so BEFORE cargo runs: an artifact that was never
	// compiled cannot be deployed by accident.
	t.Run("known positive is refused, and for the determinism reason", func(t *testing.T) {
		out := build(t, "e001_fs_access")

		if !strings.Contains(out, "R001") {
			t.Errorf("the build did not report the determinism finding.\n\n"+
				"A non-zero exit is NOT what this asserts -- this fixture's crate\n"+
				"layout makes cargo fail on its own, so the build already exited 1\n"+
				"before any gate existed. The R001 code is what distinguishes\n"+
				"'refused by the determinism check' from 'failed for some other\n"+
				"reason'.\n\noutput:\n%s", out)
		}
		if !strings.Contains(out, "no artifact was emitted") {
			t.Errorf("the build reported a determinism finding but did not say it "+
				"declined to emit an artifact.\n\noutput:\n%s", out)
		}
		// The gate runs before the cargo lookup, so a refused build never
		// reaches the compiler. If this string appears, the gate is running
		// too late and a partial artifact may exist.
		if strings.Contains(out, "Compiling Rust WASM module") {
			t.Errorf("the build reached the cargo stage despite a determinism "+
				"finding; the gate is running too late.\n\noutput:\n%s", out)
		}
	})

	// ARM 1b -- THE FIXTURE THAT USED TO BE THE KNOWN LIMIT. A grouped import
	// spells the same reach in a way no literal in the old table matched;
	// cleat#1811 replaced that table with a `use` resolver and it is now
	// caught. Promoted here from the arm below, following that arm's own
	// instructions.
	//
	// Asserted on R001 rather than on the exit status, for the same reason as
	// the arm above: this fixture's crate layout makes cargo fail anyway.
	t.Run("a grouped import is refused, now that paths are resolved", func(t *testing.T) {
		out := build(t, "r001_grouped_import")

		if !strings.Contains(out, "R001") {
			t.Errorf("the grouped-import fixture was not refused. It reaches the filesystem "+
				"through `use std::{fs, io}` and `fs::read_to_string`, which the resolver "+
				"must see (cleat#1811).\n\noutput:\n%s", out)
		}
		// The resolution is part of the finding, not decoration: "R001 at
		// fs::read_to_string" sends the reader to a name that is in no table.
		if !strings.Contains(out, "std::fs") {
			t.Errorf("the finding does not name the RESOLVED path, so the reader cannot tell "+
				"which module it reached.\n\noutput:\n%s", out)
		}
	})

	// ARM 1c -- AN ALIASED IMPORT. `use std::time::SystemTime as ST; ST::now()`
	// contains the forbidden spelling nowhere at all.
	t.Run("an aliased clock is refused, and the finding names both forms", func(t *testing.T) {
		out := build(t, "r005_aliased_now")

		if !strings.Contains(out, "R005") {
			t.Errorf("an aliased SystemTime::now was not refused.\n\noutput:\n%s", out)
		}
		if !strings.Contains(out, "ST::now") || !strings.Contains(out, "std::time::SystemTime::now") {
			t.Errorf("the finding must name what was WRITTEN and what it RESOLVES TO; one "+
				"without the other is unactionable.\n\noutput:\n%s", out)
		}
		// Duration is imported here on purpose. The old table needed an
		// explicit allow row for it; the prefix rule excludes it by
		// construction, and a regression to substring matching would flag it.
		if strings.Contains(out, "R00") && strings.Contains(out, "Duration") {
			t.Errorf("std::time::Duration was reported. It is what h.DurableSleep() takes "+
				"and is under neither forbidden prefix.\n\noutput:\n%s", out)
		}
	})

	// ARM 1d -- R008 KNOWN POSITIVE. The issue's own measured example: a
	// parameter typed &HashMap, iterated directly. cleat#1864.
	t.Run("a HashMap parameter iterated with a for-loop is refused", func(t *testing.T) {
		out := build(t, "r008_hashmap_iteration")

		if !strings.Contains(out, "R008") {
			t.Errorf("iterating a HashMap parameter was not refused.\n\noutput:\n%s", out)
		}
		if !strings.Contains(out, "no artifact was emitted") {
			t.Errorf("the build reported R008 but did not say it declined to emit an "+
				"artifact.\n\noutput:\n%s", out)
		}
	})

	// ARM 1e -- R008 NEGATIVE CONTROL. Construction, insertion, lookup and
	// removal on a HashMap are all deterministic; only enumeration order is
	// not. A rule that fired on the type rather than the usage would refuse
	// this crate too.
	t.Run("constructing, inserting into and looking up in a HashMap is not refused", func(t *testing.T) {
		out := build(t, "r008_safe_map_usage")

		if strings.Contains(out, "R008") {
			t.Errorf("a crate that never enumerates a HashMap's contents was refused for "+
				"HashMap iteration order.\n\noutput:\n%s", out)
		}
		if !strings.Contains(out, "Vetting Rust crate") {
			t.Fatalf("the vet stage did not run, so finding no R008 says nothing.\n\noutput:\n%s", out)
		}
	})

	// ARM 2 -- KNOWN LIMIT, now a method call rather than a grouped import.
	// The resolver matches PATH expressions; `t.elapsed()` names no module, so
	// a crate exactly as non-deterministic as r005_aliased_now next door goes
	// unreported. Seeing it needs a receiver's type, which the design note
	// declines and which tree-sitter would not have supplied either.
	//
	// This arm exists so that "5 of 5 languages refuse a bad fixture" cannot be
	// read as parity of ENFORCEMENT between a whole-program analysis and a
	// path resolver.
	//
	// It is also a tripwire: if the Rust checker is ever strengthened, this
	// goes red. That is good news, not a regression -- see the failure text.
	t.Run("known limit escapes the checker, and says so out loud", func(t *testing.T) {
		out := build(t, "known_limit_trait_method")

		if strings.Contains(out, "determinism check failed") || strings.Contains(out, "Error [R0") {
			t.Errorf("the Rust checker now CATCHES the trait-method fixture.\n\n"+
				"This is an improvement, not a regression. Three things to do:\n"+
				"  1. move this fixture to a known-positive arm above;\n"+
				"  2. write a new known-limit fixture for whatever still escapes\n"+
				"     -- do not leave this arm empty, or the next reader has no\n"+
				"     way to tell a strong checker from an unwired one;\n"+
				"  3. update LANGUAGE_SUPPORT.md and\n"+
				"     docs/contributor/design/rust-determinism-checker.md, which\n"+
				"     both say the checker has no type resolution.\n\noutput:\n%s", out)
		}
	})

	// ARM 2b -- R008 KNOWN LIMIT: a map reached through a struct field.
	// The receiver of an order-producing method call must be a bare tracked
	// identifier; `self.counts` is a field access, not a binding this
	// checker resolved. See the fixture's own header for why this is the
	// right limit to accept rather than close.
	t.Run("a HashMap behind a struct field escapes the checker, and says so out loud", func(t *testing.T) {
		out := build(t, "known_limit_map_via_struct_field")

		if strings.Contains(out, "R008") {
			t.Errorf("the Rust checker now CATCHES a HashMap reached through a struct field.\n\n"+
				"This is an improvement, not a regression. Three things to do:\n"+
				"  1. move this fixture to r008_hashmap_iteration;\n"+
				"  2. write a new known-limit fixture for whatever still escapes;\n"+
				"  3. update LANGUAGE_SUPPORT.md and\n"+
				"     docs/contributor/design/rust-determinism-checker.md.\n\noutput:\n%s", out)
		}
	})

	// ARM 2c -- R008 KNOWN LIMIT: a HashMap that is never bound to a name,
	// iterated immediately off a `.collect::<HashMap<_, _>>()` chain. R008
	// tracks bindings; a value with no name has nothing to track.
	t.Run("collecting straight into a HashMap and iterating inline escapes the checker", func(t *testing.T) {
		out := build(t, "known_limit_collect_into_map")

		if strings.Contains(out, "R008") {
			t.Errorf("the Rust checker now CATCHES an inline .collect::<HashMap<_, _>>() chain.\n\n"+
				"This is an improvement, not a regression. Three things to do:\n"+
				"  1. move this fixture to r008_hashmap_iteration;\n"+
				"  2. write a new known-limit fixture for whatever still escapes;\n"+
				"  3. update LANGUAGE_SUPPORT.md and\n"+
				"     docs/contributor/design/rust-determinism-checker.md.\n\noutput:\n%s", out)
		}
	})
}
