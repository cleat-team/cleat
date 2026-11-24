;; The mirror of bigmem.wat: a component whose core module needs 260 MiB of linear memory at instantiation (4160 pages), which is MORE than the
;; engine's 256 MiB default (4096 pages), and returns {"ok":1}. bigmem.wat proves the default is high enough for a runtime to start in; this one
;; proves the default is a limit at all. Without it a backend whose constructor leaves the limit at 0 (an unbounded store) passes every test
;; that reads the default (cleat#4200, found in review): it cannot instantiate under the default, and runs under an explicit larger limit.
(module
  (memory (export "memory") 4160)
  (data (i32.const 1024) "{\"ok\":1}")
  (func (export "cabi_realloc") (param i32 i32 i32 i32) (result i32) (i32.const 8192))
  (func (export "run") (param i32 i32) (result i32)
    (i32.store8 (i32.const 32) (i32.const 0))
    (i32.store (i32.const 36) (i32.const 1024))
    (i32.store (i32.const 40) (i32.const 8))
    (i32.const 32))
  (func (export "run-deferred") (result i32) (i32.const 0)))
