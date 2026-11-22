---
section: added
---
- **`cleat build --target python` now warns when the installed `componentize-py` is not the version the Python SDK was measured on, 0.25.1 (cleat#4355).** `componentize-py` decides which standard-library modules a component contains, so a different version may contain different ones, and a missing module raises `ModuleNotFoundError` only when the workflow runs. The warning names both versions and the install command, and the build goes on: a different version is not known to be broken, only not measured. The pin itself, in CI and the docs, shipped in the previous change.
