# DRAFT, NOT POSTED. Needs the project owner's say-so before it goes to bytecodealliance/wasmtime.

**Title:** C API: expose `Config::memory_guaranteed_dense_image_size` (embedders using the C API cannot get copy-on-write images for modules whose data is moderately sparse)

`Config::memory_guaranteed_dense_image_size` (default 16 MiB; `crates/wasmtime/src/config.rs`) and the CLI flag `-O memory-guaranteed-dense-image-size=N` exist, but `crates/c-api/include/wasmtime/config.h` has no setter at v48.0.0 or v51.0.0 (it has `wasmtime_config_memory_init_cow_set`), so embedders that use the C API (and the Go, .NET and other bindings built on it) cannot raise it.

**Why it matters.** In `ModuleTranslation::try_static_init`, a copy-on-write image is built only if `image_size < 2 * data_size` (at least 50% dense) or `image_size < memory_guaranteed_dense_image_size`. A CPython component produced by componentize-py has 2,539 active data segments, 10.62 MiB of data over a 21.69 MiB span: **49% dense, over 16 MiB**, so it gets **no image**, and every instance copies 10.6 MiB. The function's own comment says "an embedder desiring fast instantiation should ensure that this setting is as large as the maximum module initial memory content size", which the C API does not allow.

**Evidence (wasmtime 48.0.0 CLI, aarch64 Linux, release asset):** `wasmtime compile` of the main module: 11,724,024 bytes by default (data kept as segments), **23,227,648 bytes with `-O memory-guaranteed-dense-image-size=67108864`** (static dense image embedded). Rewriting the module so its data is a single dense segment and compiling with defaults gives the same 23,227,648 bytes. With the image, an instance's private memory goes from 11.49 MiB to 0.14 MiB and instantiation from 5.9 ms to 0.03 ms (plain wasmtime-go, 8 instantiations).

**Ask:** `wasmtime_config_memory_guaranteed_dense_image_size_set(wasm_config_t*, uint64_t)`, next to `memory_init_cow`. (Separately, the sparse-list idea in #3820 would remove the cliff.)

*(A reproduction module and the commands are in the project's measurement notes; attach a generated module of the same layout, not the CPython component.)*
