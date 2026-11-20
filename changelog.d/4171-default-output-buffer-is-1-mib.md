---
section: changed
---
- **Behaviour change: the default `--wasm-output-buffer-size` is now 1 MiB (it was 32 KiB), the engine's own default and what `ABI.md` states (cleat#4171, owner decision).** For a core-module guest's direct export call, the host places an input area and an output area of this size past the end of the guest's memory, so a workflow input or result up to 1 MiB now works on a stock worker, and each such call needs twice this value of headroom under `--wasm-memory-max-mb`. Python and C# components take the value and do not use it. The worker's flag, the engine's constant and `docs/reference/worker-config.md` agreed on 32 KiB or 1 MiB at different times; a test now fails if the flag stops matching the engine.
