---
section: upgrade
---
- **Known issue: a held C# run is charged 59 MiB, so the default budget of a 2Gi worker holds 6 of them against 37 Python runs (cleat#4440).** An idle C# instance costs about 17 MiB, but one that has handled large objects stays near 57 MiB because the runtime's large-object heap grows and wasm memory never shrinks. For a worker that serves C# workflows raise the container's memory limit, lower `--resident-memory-percent`, or run C# on its own workers; reducing the cost is planned for 0.7.0. The per-language figures and where each comes from are in the new "Sizing a worker by language" section of the worker configuration reference.
