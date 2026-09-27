# WIT Interface Definitions

This directory contains WIT (Wasm Interface Types) definitions for the
Cleat host call interface. These are used by `componentize-py` to generate
Python WASM bindings.

## Building

```bash
componentize-py bindings --world cleat-workflow python-sdk/wit/ --output python-sdk/cleat_sdk/_wit/
```

## Structure

- `cleat.wit` — the host call imports, organized by category. It declares **49**
  functions across 18 interfaces, 17 of which the `cleat-workflow` world imports
  (the 18th, `outcomes`, carries types rather than functions). Derived, not counted:
  `grep -cE '^[[:space:]]*[a-z0-9-]+:[[:space:]]*func' cleat.wit`. This said 31 until
  2026-09-13.

## Prerequisites

- `componentize-py` from the Bytecode Alliance
- Python 3.10+
