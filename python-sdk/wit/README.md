# WIT Interface Definitions

This directory contains WIT (Wasm Interface Types) definitions for the
Cleat host call interface. These are used by `componentize-py` to generate
Python WASM bindings.

## Building

```bash
componentize-py bindings --world cleat-workflow python-sdk/wit/ --output python-sdk/cleat_sdk/_wit/
```

## Structure

- `cleat.wit` — All 31 host call imports organized by category

## Prerequisites

- `componentize-py` from the Bytecode Alliance
- Python 3.10+
