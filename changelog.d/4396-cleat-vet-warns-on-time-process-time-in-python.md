---
section: changed
---
- **`cleat vet` for Python also warns about `time.process_time()` and `time.process_time_ns()` (PY016, cleat#4396).** They are not monotonic clocks, but they differ between a run and its replay like the monotonic reads do: measured on componentize-py 0.25.1, two runs of one workflow id read `-2.0412` and `-2.0397` seconds, negative on both, so the value does not even measure CPU time in a workflow. Same code and same remedy as the monotonic reads (`h.now()` and `h.sleep()`), with a message that says what these actually read. Still a warning, never an error.
