---
section: changed
---
- **A C# workflow instance costs about 13 MiB of memory instead of 49 MiB, which also lets residency hold about four times as many of them (cleat#4378).** A .NET guest on WASI started its GC with three 16 MiB heap segments, and the WASI reservation writes every byte it reserves, so an empty workflow occupied 48.7 MiB per instance (a fit over 1 to 64 instances, measured 2026-11-22). The C# SDK now sets the GC segment size to 4 MiB, its minimum, which makes the same workflow cost 12.7 MiB. A workflow that holds many large objects pays about 28% over its data for segment rounding; set `CleatGcSegmentSizeHex` in the project to change the size. Rebuild C# workflows with `cleat build --target csharp` to get it: a component built before this change keeps its old cost.
