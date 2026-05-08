# Example Third-Party Plugin: hello-world

This directory contains a minimal, fully-documented example of a third-party
cleat plugin. It demonstrates the complete plugin authoring workflow: writing
the manifest, implementing host functions, building to WASM, and publishing.

## What this plugin does

The `example/hello-world` plugin provides two host functions:

- **`greet(name)`** -- returns `{"message": "Hello, <name>!"}`
- **`reverse(text)`** -- returns `{"reversed": "<text in reverse>"}`

Both functions are pure and idempotent -- they have no side effects and can be
safely re-invoked during workflow replay.

## Files

```
third-party-plugin/
├── plugin.json    # Plugin manifest — the source of truth
├── main.go        # Plugin implementation — WASM module with host functions
├── Makefile       # Build commands
└── README.md      # This file
```

## Prerequisites

- Go 1.21+ (for `GOOS=wasip1` support)
- cleat CLI 1.0+ (for `cleat plugin validate`)

## Build

```bash
make build
```

This runs:

```bash
GOOS=wasip1 GOARCH=wasm go build -o plugin.wasm .
```

The output is `plugin.wasm` — a ~2 MB WASM binary.

## Validate

```bash
make validate
```

Or manually:

```bash
cleat plugin validate --manifest plugin.json
```

Expected output:

```
$ cleat plugin validate --manifest plugin.json
OK
```

If there are validation errors, the CLI prints them with guidance on how to
fix each one.

## Generate SDK wrappers

Generate typed wrappers for your plugin in each supported SDK language:

```bash
make generate-sdk
```

This generates:

```bash
cleat-gen-plugin --manifest plugin.json --lang typescript --out generated/plugins.ts
cleat-gen-plugin --manifest plugin.json --lang python --out generated/plugins.py
```

The generated wrappers provide IDE autocomplete and type checking for workflow
authors.

## Test locally

1. Install the plugin into your local cleat instance:

```bash
cleat plugin install --manifest plugin.json --wasm plugin.wasm
```

2. Write a quick workflow that calls the plugin:

```typescript
// test-workflow.ts
import { HelloWorldPlugin } from "./generated/plugins";

export default async function (ctx: WorkflowContext) {
  const hw = new HelloWorldPlugin(ctx.hostCalls);

  const greeting = await hw.greet({ name: "World" });
  console.log(greeting.message); // "Hello, World!"

  const reversed = await hw.reverse({ text: "hello" });
  console.log(reversed.reversed); // "olleh"
}
```

3. Run the workflow:

```bash
cleat workflow run test-workflow.ts
```

## Clean

```bash
make clean
```

Removes the WASM binary and generated SDK wrappers.

## Publishing checklist

Before publishing:

- [ ] `plugin.json` passes `cleat plugin validate`
- [ ] WASM binary compiles without errors (`make build`)
- [ ] Plugin name uses `org/name` format for community plugins
- [ ] Capabilities are set to the minimum needed
- [ ] `repository` field points to public source code
- [ ] `min_cleat_version` is set
- [ ] Checksum is recorded: `sha256sum plugin.wasm`

## See also

- [Third-party plugin authoring guide](../../docs/third-party-plugin-guide.md)
- [Plugin manifest JSON schema](../../schemas/plugin-manifest.schema.json)
- [Plugin security guide for operators](../../docs/plugin-security.md)
- [Plugin migration guide](../../docs/plugin-migration-guide.md)
