# Fixtures for `scripts/soak-failure-report.py --self-test`

Each file is a real `go test -json` report from a real run of `.github/workflows/soak.yml`, taken from
the run's job log with the runner's timestamp prefix removed. Every kept line is verbatim. What was
dropped, and nothing else: most `[sample N]` progress lines (the first two and last two are kept) and,
in `timeout-panic.jsonl`, the goroutine dump after the panic.

| file | run | what it is |
|---|---|---|
| `failing-workload-errors.jsonl` | 42572418016, 2026-11-09, schedule | the cleat#3733 failure: ten `workload error:` lines, `Failed: 10`, `--- FAIL: TestSoakWorkflowMix` |
| `timeout-panic.jsonl` | 32696695343, 2026-08-24, schedule | the 90-minute package timeout: `panic: test timed out after 1h30m0s`, package `FAIL`, no end-of-run summary |
| `green.jsonl` | 40280191846, 2026-10-26, schedule | a passing run: `Failed: 0`, `--- PASS`. The control: it must produce no failing lines |

They are real because a matcher tested on a log its author imagined passes against the author's
imagination (cleat#3644). Re-derive one with
`gh run view <run> --repo cleat-team/cleat --log`, then keep the `{"Time":...}` part of each line.
