---
section: upgrade
---
- **`cleat vet` and `cleat build` now refuse `time.sleep` through an alias, as they already refused it written plainly (PY004, cleat#4403).** `from time import sleep`, `from time import sleep as nap`, `import time as t; t.sleep(...)` and a module-level `nap = time.sleep` were only the PY016 warning, which let an in-guest sleep through. It blocks the same way whatever it is called: measured on a worker, a `time.sleep` holds a `--concurrency` slot and one OS thread for its whole duration and `--wasm-instance-timeout` does not interrupt it. **A workflow that sleeps through an alias will stop building; use `h.sleep(seconds)`, which is durable.** The PY016 warning is unchanged for the clock reads.
