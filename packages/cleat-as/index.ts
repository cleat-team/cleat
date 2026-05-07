// Root-level re-export for @cleat/sdk package resolution.
//
// AssemblyScript 0.27.32's module resolver looks for `index.ts` at the root
// of scoped packages (@scope/name) when resolving bare imports like:
//   import { HostCalls } from "@cleat/sdk"
//
// The `ascMain` field in package.json is not actually used by the resolver.
// This file provides the entry point that the resolver expects.
//
// Without this file, imports of "@cleat/sdk" fail with:
//   ERROR TS6054: File '~lib/@cleat/sdk.ts' not found.
//
// See Issue #6 in fork_open_projects_issues.md.

export * from "./assembly/index";
