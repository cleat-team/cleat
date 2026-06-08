# STATUS — cleat-230-race-fix1e

**Phase:** plan
**Created:** 2026-06-06T12:00:00Z
**Budget:** $2
**Spent:** $0

## Summary

Extending the WASM stdout/stderr buffer race fix to the `executeComponent` path. Currently in planning phase.

## Changes planned

1. **engine/engine.go**: In `executeComponent()`, add per-execution `stdout`/`stderr` `bytes.Buffer` and use `instantiateModuleNamedWithWriters` instead of `InstantiateModuleNamed`
2. **engine/engine_race_test.go** (new): `TestComponentStdoutStderrRace` — concurrent `Engine.Execute()` calls with a hand-crafted component binary, verified race-free with `-race -count=10`
