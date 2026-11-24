;; A component whose core module needs 44 MiB of linear memory at instantiation (704 pages), and returns {"ok":1}. Used to test
;; the per-language memory default (cleat C# PR 3): it cannot instantiate under the 32 MiB default and can under a csharp stamp.
(module
  (memory (export "memory") 704)
  (data (i32.const 1024) "{\"ok\":1}")
  (func (export "cabi_realloc") (param i32 i32 i32 i32) (result i32) (i32.const 8192))
  (func (export "run") (param i32 i32) (result i32)
    (i32.store8 (i32.const 32) (i32.const 0))
    (i32.store (i32.const 36) (i32.const 1024))
    (i32.store (i32.const 40) (i32.const 8))
    (i32.const 32))
  (func (export "run-deferred") (result i32) (i32.const 0)))
