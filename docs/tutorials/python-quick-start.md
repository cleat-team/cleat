# Python quick start: your first Python workflow

This guide takes you from an empty directory to a Python workflow that has run on
a worker. It is the [Go quick start](quick-start.md) for Python, and it shares
that guide's database and worker steps rather than repeating them.

**No checkout is required.** `pip install cleat-sdk` ships the interface
definition (the WIT) and the build script that `cleat build --target python`
needs, so the CLI finds them in your virtual environment.

> **`cleat-sdk` 0.5.0 on PyPI does not ship them.** Its wheel has no `.wit`
> files and no build script, so `cleat build --target python` outside a checkout
> fails with `finding repo root: go.mod not found` (cleat#3793). Use the next
> release, or build from a clone of this repository, where the same command
> finds them in `python-sdk/`.

---

## 1. Prerequisites

- **Python 3.10 or newer**, with `venv`. On Debian and Ubuntu `python3 -m venv`
  needs its own package: `sudo apt install python3-venv`. On macOS,
  `/usr/bin/python3` is 3.9, which is too old: `brew install python@3.12` and
  put it first on `PATH`.
- **The `cleat` CLI and `cleat-worker`, Postgres, and the schema.** Do steps 1, 2
  and 3 of the [Go quick start](quick-start.md#1-prerequisites). You do **not**
  need Go: it compiles Go workflows, and Python workflows are compiled by
  `componentize-py`, which you install below.

## 2. Create a virtual environment and install the SDK

```bash
mkdir hello-py && cd hello-py
python3 -m venv .venv
. .venv/bin/activate
pip install cleat-sdk 'componentize-py>=0.12.0'
```

Both things below are deliberate:

- **The virtual environment is not optional on a current Linux distribution.**
  Current Debian and Ubuntu releases refuse a global `pip install` with
  `error: externally-managed-environment` (PEP 668).
- **The quotes around `componentize-py>=0.12.0` are required.** Unquoted, the
  shell reads `>=0.12.0` as a redirect: it creates a file named `=0.12.0` and
  installs `componentize-py` with no version constraint.

Keep this virtual environment active for every command below: `cleat build` runs
the `python3` it finds on `PATH`, and looks there for `cleat-sdk`.

## 3. Write the workflow

Save this as `hello.py`:

```python
from cleat_sdk import HostCalls, cleat_entry


@cleat_entry("hello")
def hello(h: HostCalls, name: str) -> dict:
    h.log(f"hello: greeting {name}")
    h.sleep(1)
    return {"greeting": f"Hello, {name}!"}
```

Four things in it are worth knowing:

- `@cleat_entry("hello")` marks the entry point and names it.
- `h` is a `HostCalls`, the workflow's only door to the outside world. Its methods
  are named plainly: `h.log`, `h.sleep`, `h.call`, `h.fetch`. (Older copies of the
  SDK README spelled them `cleat_log` and `cleat_call`; those never existed.)
- `h.sleep(1)` is **one second**. Python timeouts are in seconds; `h.sleep_ms` is
  the milliseconds form.
- The **input is a JSON object whose keys are your parameter names**, so this
  takes `{"name": "Ada"}`. The **result is returned as a JSON object**, which is
  why this returns a `dict` and not a bare string.

## 4. Build the workflow

```bash
mkdir -p out
cleat build --target python -o out ./hello.py
```

Flags go before the path: `cleat build ./hello.py -o out` is refused, because
`-o` would be ignored.

Expected output, abridged:

```
Entry functions: hello
Durable closure (1 functions): hello
  Compiling Python to WASM via componentize-py...
  ...
Build SUCCESS
  Size:   18,449,262 bytes (17.59 MB)
  Decompose skipped: output is a WASM Component Model binary (not core WASM)
  Wrote out/hello.wasm (17.6 MB)
  Wrote out/hello.wasm.schema.json
```

- **The build needs no tool beyond `componentize-py`.** The output is a WASM
  Component Model binary, which is what the worker runs Python from. (`Decompose
  skipped` is information, not a warning. **Python does not currently build for
  wazero** (the developer tooling): `--runtime wazero` needs core WASM, and none of
  the `wasm-tools` releases checked has the `component decompose` subcommand that
  produces it, so it exits 1. Use the default.)
- **18 MB for a hello world is normal.** The component carries a whole CPython.
  The worker loads it once and caches it.

## 5. Deploy the workflow

```bash
cleat deploy \
    --db "postgres://postgres:postgres@localhost:5432/cleat?sslmode=disable" \
    --name hello \
    out/hello.wasm
```

Expected output includes `Deployed workflow "hello" version 1`.

## 6. Start the worker

In a second terminal, as in
[step 7 of the Go quick start](quick-start.md#7-start-the-worker):

```bash
docker exec cleat-postgres psql -U postgres -d cleat \
    -c "ALTER ROLE cleat_app LOGIN PASSWORD 'cleat_app'"
export CLEAT_APP_DSN="postgres://cleat_app:cleat_app@localhost:5432/cleat?sslmode=disable"
cleat-worker --db "$CLEAT_APP_DSN" --api-addr :8080
```

Then mint an API key as in
[step 8](quick-start.md#8-run-the-workflow) and `export CLEAT_API_KEY=...`.

## 7. Run the workflow

```bash
curl -X POST http://localhost:8080/api/workflows/hello/start \
    -H "Content-Type: application/json" \
    -H "Authorization: Bearer $CLEAT_API_KEY" \
    -d '{"input": {"name": "Ada"}}'
```

Expected response:

```json
{"id": "b2bb6d67-16fb-4ec5-adb0-7cacb9f81f25", "idempotent_replay": false}
```

`{"input": "Ada"}` is refused with `type: want object, got string`: the input
must be an object, because the build derived a schema from `name: str`.

## 8. See the result

Wait a couple of seconds for the `h.sleep(1)`, then, with the `id` from above:

```bash
curl -H "Authorization: Bearer $CLEAT_API_KEY" \
    http://localhost:8080/api/workflows/<id>
```

```json
{
    "id": "b2bb6d67-16fb-4ec5-adb0-7cacb9f81f25",
    "status": "done",
    "result": "{\"greeting\":\"Hello, Ada!\"}"
}
```

Read `status` as the [Go quick start](quick-start.md#9-see-the-result) explains:
a workflow that is sleeping is `ready`, not `running`.

## Troubleshooting

| You see | Cause | Fix |
|---|---|---|
| `error: externally-managed-environment` | `pip install` outside a virtual environment | Step 2: `python3 -m venv .venv && . .venv/bin/activate` |
| `The virtual environment was not created successfully` | Debian/Ubuntu ship `venv` separately | `sudo apt install python3-venv` |
| A file named `=0.12.0` appeared | `>=` was unquoted | Delete it and quote the requirement |
| `finding repo root: go.mod not found` | `cleat-sdk` 0.5.0, which ships no build script | Upgrade `cleat-sdk`, or build from a clone |
| `the cleat-sdk installed at … has no build script` | An older `cleat-sdk` is active | `pip install --upgrade cleat-sdk` |
| `the cleat-sdk package is not installed for the python3 on PATH` | The virtual environment is not active in this shell | `. .venv/bin/activate` |
| `componentize-py not found` | Not installed in the active environment | `pip install 'componentize-py>=0.12.0'` |
| `AttributeError: 'HostCalls' object has no attribute 'cleat_log'` | A workflow written from an old README | Use `h.log`, `h.call`, `h.sleep`, `h.fetch` |
| `type: want object, got string` | Input sent as a string | `{"input": {"name": "Ada"}}` |

## Next steps

- [The Python SDK README](../../python-sdk/README.md) -- every `HostCalls` method,
  signals, update handlers, and the standard-library limits inside WASM
- [Signals and the human loop](signals-and-human-loop.md)
- [Common patterns](../how-to/common-patterns.md)
