#!/usr/bin/env python3
"""Build a Python workflow into a WASM component using componentize-py.

Usage:
    python build_wasm.py --entry my_workflow.py:function_name [--output my_workflow.wasm]

This script wraps the componentize-py tool to compile a Python workflow
function (decorated with @cleat_entry) into a WASM component that can
be loaded by the cleat worker runtime.
"""

from __future__ import annotations

import argparse
import atexit
import os
import re
import shutil
import subprocess
import sys
from pathlib import Path

# The componentize-py version CI tests and the standard-library findings in python-sdk/README.md were measured on (cleat#4355). It is
# stated in python-sdk/componentize-py-requirements.txt; scripts/check-componentize-py-pin.py fails if this constant disagrees with it.
PINNED_COMPONENTIZE_PY = "0.25.1"

# A build refused because the installed componentize-py is not the measured version (cleat#4383) exits with this status and not with 1. 1 means a build that
# RAN and failed, and a caller that sees it tells the reader to look at their own source; a refusal is a problem with the environment, and says so. cmd/cleat
# and wasm/build.go read it as PythonBuildExitUnmeasuredComponentizePy, and a Go test fails if the two stop being equal.
EXIT_UNMEASURED_COMPONENTIZE_PY = 3
ALLOW_UNMEASURED_FLAG = "--allow-unmeasured-componentize-py"
ALLOW_UNMEASURED_ENV = "CLEAT_ALLOW_UNMEASURED_COMPONENTIZE_PY"


def find_sdk_root() -> Path:
    """Find the python-sdk root directory (a checkout's layout only)."""
    script_dir = Path(__file__).resolve().parent
    return script_dir.parent


class SdkLayout:
    """Where the SDK's pieces are, in a checkout or in an installed wheel.

    This script ships in two places and the pieces sit differently in each:

        checkout   python-sdk/scripts/build_wasm.py    python-sdk/wit/cleat.wit
                   python-sdk/cleat_sdk/               <- put python-sdk/ on the path
        installed  <site-packages>/cleat_sdk/_build/build_wasm.py
                   <site-packages>/cleat_sdk/wit/cleat.wit
                   <site-packages>/cleat_sdk/          <- NOT site-packages itself

    The installed case is why ``python_path`` is a field and not just the SDK
    root. componentize-py bundles whatever is on ``-p`` into the component, and
    site-packages holds every other package in the environment, componentize-py
    itself among them. So an installed build stages a copy of ``cleat_sdk``
    alone in a temporary directory and points ``-p`` there. Call ``close()``
    when the build is done to remove it.

    cleat#3793: until this, ``pip install cleat-sdk`` shipped neither the WIT
    nor this script, and ``cleat build --target python`` needed a 305 MB
    checkout of the repository to find them.
    """

    def __init__(self, script: Path | None = None):
        script_dir = (script or Path(__file__)).resolve().parent
        self._staged: Path | None = None
        if script_dir.name == "_build":
            package = script_dir.parent
            self.installed = True
            self.wit_dir = package / "wit"
            self.stamp_script = script_dir / "stamp_metadata.py"
            self._package = package
            self.python_path = package.parent  # replaced by stage()
        else:
            root = script_dir.parent
            self.installed = False
            self.wit_dir = root / "wit"
            self.stamp_script = script_dir / "stamp_metadata.py"
            self._package = root / "cleat_sdk"
            self.python_path = root

    def stage(self) -> None:
        """Make ``python_path`` safe to hand to componentize-py. A no-op in a checkout."""
        if not self.installed or self._staged is not None:
            return
        import tempfile

        self._staged = Path(tempfile.mkdtemp(prefix="cleat-sdk-stage-"))
        shutil.copytree(
            self._package,
            self._staged / "cleat_sdk",
            ignore=shutil.ignore_patterns("__pycache__", "_build", "wit"),
        )
        self.python_path = self._staged

    def close(self) -> None:
        if self._staged is not None:
            shutil.rmtree(self._staged, ignore_errors=True)
            self._staged = None


def parse_entry(entry: str) -> tuple[str, str]:
    """Parse --entry argument 'file.py:func_name' into (file_path, func_name)."""
    if ":" not in entry:
        raise ValueError(f"Invalid entry format: {entry}. Expected 'file.py:func_name'")
    file_path, func_name = entry.rsplit(":", 1)
    file_path = file_path.strip()
    func_name = func_name.strip()
    if not file_path:
        raise ValueError(
            f"Invalid entry format: {entry!r}. File path is empty. "
            f"Expected 'file.py:func_name'"
        )
    if not func_name:
        raise ValueError(
            f"Invalid entry format: {entry!r}. Function name is empty. "
            f"Expected 'file.py:func_name'"
        )
    return file_path, func_name


def validate_entry(entry_file: str, func_name: str) -> dict:
    """Validate that the entry file contains a @cleat_entry function."""
    entry_path = Path(entry_file)
    if not entry_path.exists():
        raise FileNotFoundError(f"Entry file not found: {entry_file}")

    with open(entry_path) as f:
        source = f.read()

    # Simple AST-free check for @cleat_entry decorated function
    if "@cleat_entry" not in source:
        raise ValueError(
            f"No @cleat_entry decorator found in {entry_file}"
        )

    if f"def {func_name}" not in source:
        raise ValueError(
            f"Function '{func_name}' not found in {entry_file}"
        )

    return {"entry_file": str(entry_path.absolute()), "func_name": func_name}


def generate_wit_export(wit_dir: Path, func_name: str, module_name: str) -> Path:
    """Generate or update the WIT export for the specific workflow function.

    The base cleat.wit declares a general export. This generates a
    workflow-specific WIT file that exports only the target function.
    """
    # For MVP, we use the existing cleat.wit and the function name is
    # passed to componentize-py as metadata. The actual export is the
    # generic "run" function that dispatches to the decorated function.
    return wit_dir / "cleat.wit"


# ---- Dense data segments: give wasmtime a copy-on-write memory image for the Python module (cleat#4382) ----------------------------------
#
# WHY. wasmtime builds a copy-on-write image of a module's initial memory (instances then share it and pay only for the pages they dirty) only
# if the data segments cover at least HALF of the span they sit in, or the span is under memory_guaranteed_dense_image_size, 16 MiB by default
# (crates/environ/src/compile/module_environ.rs, try_static_init). The CPython module componentize-py produces has 2,539 segments covering 10.6 MiB
# of a 21.7 MiB span: 49%, a 0.45 MiB miss, with a span over 16 MiB. It therefore got NO image, and every Python workflow instance copied the
# whole 10.6 MiB snapshot: 12.05 MiB of private memory per instance, measured as a fit over 1 to 64 instances. cleat cannot raise the 16 MiB
# knob (the C API has no setter, in wasmtime 48 or 51), so this rewrites the module instead.
#
# WHAT. The module's active data segments are merged into ONE segment spanning the same range, the gaps zero-filled. Linear memory after
# instantiation is byte-for-byte the same (memory starts zero, and a gap is a range no segment wrote). The result gets an image: 1.31 MiB per
# instance, instantiation 5.9 ms to 0.03 ms. The price is size on disk: the zero gaps become bytes (18.5 MB to 30.1 MB for CPython).
#
# WHAT IT REFUSES. It rewrites only a module wasmtime would otherwise leave without an image, and only one it can parse completely: every segment
# active, in memory 0, at a constant i32 offset. It then re-parses its own output and compares the whole memory image with the original's, and
# compares every other section byte for byte; any difference is an error, and anything it cannot parse leaves the module UNCHANGED with a warning
# (the original is valid, only larger in memory). A build that silently kept the 12 MiB would be the failure this exists to remove, so
# python-sdk/tests/test_dense_data_segments.py pins the outcome on the real component.

WASMTIME_DENSE_IMAGE_BYTES = 16 << 20  # wasmtime's default Config::memory_guaranteed_dense_image_size


class NotDensifiable(Exception):
    """The module's data section has a shape this rewrite does not model."""


def _uleb(n: int) -> bytes:
    out = bytearray()
    while True:
        b, n = n & 0x7F, n >> 7
        out.append(b | (0x80 if n else 0))
        if not n:
            return bytes(out)


def _sleb(n: int) -> bytes:
    out = bytearray()
    while True:
        b, n = n & 0x7F, n >> 7
        if (n == 0 and not b & 0x40) or (n == -1 and b & 0x40):
            out.append(b)
            return bytes(out)
        out.append(b | 0x80)


def _read_uleb(b: bytes, i: int) -> tuple[int, int]:
    result = shift = 0
    while True:
        x = b[i]
        i += 1
        result |= (x & 0x7F) << shift
        shift += 7
        if not x & 0x80:
            return result, i


def _read_sleb(b: bytes, i: int) -> tuple[int, int]:
    result = shift = 0
    while True:
        x = b[i]
        i += 1
        result |= (x & 0x7F) << shift
        shift += 7
        if not x & 0x80:
            if x & 0x40:
                result -= 1 << shift
            return result, i


def _wasm_sections(b: bytes) -> list[tuple[int, int, int]]:
    """``(section id, payload start, payload end)`` for every top-level section of a module or component."""
    if b[:4] != b"\0asm":
        raise NotDensifiable("not a WebAssembly binary")
    i, out = 8, []
    while i < len(b):
        sid = b[i]
        size, j = _read_uleb(b, i + 1)
        if j + size > len(b):
            raise NotDensifiable("a section runs past the end of the binary")
        out.append((sid, j, j + size))
        i = j + size
    return out


def _parse_data_segments(payload: bytes) -> list[tuple[int, bytes]]:
    count, i = _read_uleb(payload, 0)
    segs = []
    for _ in range(count):
        flag, i = _read_uleb(payload, i)
        if flag != 0:
            raise NotDensifiable(f"a data segment has flag {flag}: not an active segment of memory 0")
        if payload[i] != 0x41:
            raise NotDensifiable("a data segment's offset is not an i32.const")
        off, i = _read_sleb(payload, i + 1)
        if payload[i] != 0x0B:
            raise NotDensifiable("a data segment's offset expression is not a single i32.const")
        n, i = _read_uleb(payload, i + 1)
        if i + n > len(payload):
            raise NotDensifiable("a data segment runs past its section")
        segs.append((off & 0xFFFFFFFF, payload[i : i + n]))
        i += n
    if i != len(payload):
        raise NotDensifiable("bytes follow the last data segment")
    return segs


def _memory_image(segs: list[tuple[int, bytes]], lo: int, hi: int) -> bytes:
    """The bytes of linear memory over ``[lo, hi)`` after instantiation: zero, then each segment applied in file order (later wins)."""
    image = bytearray(hi - lo)
    for off, data in segs:
        image[off - lo : off - lo + len(data)] = data
    return bytes(image)


def densify_module(module: bytes) -> tuple[bytes, dict] | None:
    """Merge a core module's data segments into one dense segment, or ``None`` if wasmtime would already give it an image.

    Raises ``NotDensifiable`` for a shape it does not model, and ``AssertionError`` if its own output fails the equivalence check.
    """
    sections = _wasm_sections(module)
    data = [(sid, a, b) for sid, a, b in sections if sid == 11]
    if not data:
        return None
    if len(data) != 1:
        raise NotDensifiable(f"{len(data)} data sections")
    segs = _parse_data_segments(module[data[0][1] : data[0][2]])
    if not segs:
        return None
    lo = min(off for off, _ in segs)
    hi = max(off + len(d) for off, d in segs)
    data_bytes = sum(len(d) for _, d in segs)
    span = hi - lo
    if span < WASMTIME_DENSE_IMAGE_BYTES or span < 2 * data_bytes:
        return None  # wasmtime builds an image for this one already
    image = _memory_image(segs, lo, hi)
    merged = _uleb(1) + b"\x00\x41" + _sleb(lo) + b"\x0b" + _uleb(len(image)) + image

    out = bytearray(module[:8])
    for sid, a, b in sections:
        payload = module[a:b]
        if sid == 11:
            payload = merged
        elif sid == 12:  # data count: one segment now
            payload = _uleb(1)
        out += bytes([sid]) + _uleb(len(payload)) + payload
    result = bytes(out)

    # The equivalence check: the new module's memory image is the old one's, over the same range, and nothing else changed.
    new_sections = _wasm_sections(result)
    assert len(new_sections) == len(sections), "densify changed the number of sections"
    for (sid, a, b), (nsid, na, nb) in zip(sections, new_sections, strict=True):
        assert sid == nsid, "densify reordered the sections"
        if sid not in (11, 12):
            assert module[a:b] == result[na:nb], f"densify changed section {sid}"
    new_segs = _parse_data_segments(result[new_sections[[s[0] for s in sections].index(11)][1] : new_sections[[s[0] for s in sections].index(11)][2]])
    assert len(new_segs) == 1 and new_segs[0][0] == lo and len(new_segs[0][1]) == span, "densify did not produce one segment over the same range"
    assert new_segs[0][1] == image, "densify changed the memory image"
    return result, {"segments": len(segs), "low": lo, "high": hi, "data_bytes": data_bytes, "span": span, "added_bytes": len(result) - len(module)}


def densify_component(component: bytes) -> tuple[bytes, list[dict]]:
    """Densify every core module of a component that needs it. Returns the component and a report per module rewritten (empty: none needed)."""
    out = bytearray(component[:8])
    report: list[dict] = []
    for sid, a, b in _wasm_sections(component):
        payload = component[a:b]
        if sid == 1:  # a core module
            dense = densify_module(payload)
            if dense is not None:
                payload, info = dense
                report.append(info)
        out += bytes([sid]) + _uleb(len(payload)) + payload
    return bytes(out), report


def densify_output(path: str) -> int:
    """Densify the component at ``path`` in place. Returns the bytes added; never fails the build for a shape it does not model."""
    try:
        with open(path, "rb") as f:
            original = f.read()
        dense, report = densify_component(original)
    except (NotDensifiable, IndexError) as e:
        print(f"  Warning: the module's data layout was not rewritten ({e}); every Python workflow instance will copy the whole snapshot "
              "(about 12 MiB of memory each) instead of sharing it (cleat#4382).", file=sys.stderr)
        return 0
    if not report:
        return 0
    if shutil.which("wasm-tools"):
        check = subprocess.run(["wasm-tools", "validate", "-"], input=dense, capture_output=True)
        if check.returncode != 0:
            print("  Warning: the rewritten component does not validate, so the original was kept: "
                  f"{check.stderr.decode(errors='replace').strip()[:300]}", file=sys.stderr)
            return 0
    tmp = path + ".dense"
    with open(tmp, "wb") as f:
        f.write(dense)
    os.replace(tmp, path)
    for r in report:
        print(f"  Dense data: {r['segments']:,} segments over {r['span'] / 1048576:.1f} MiB covered {r['data_bytes'] / 1048576:.1f} MiB "
              f"({100 * r['data_bytes'] // r['span']}%), so wasmtime would build no shared memory image; merged into one segment "
              f"(+{r['added_bytes'] / 1048576:.1f} MiB on disk, same memory contents).")
    return sum(r["added_bytes"] for r in report)


def componentize_py_version_problem(version_output: str) -> str | None:
    """What is wrong when the installed componentize-py is not the version this SDK was measured on, else ``None`` (cleat#4355, #4383).

    componentize-py decides which standard-library modules a component contains, and the README's compatibility tables and
    ``cleat vet``'s PY015 table were measured on ``PINNED_COMPONENTIZE_PY`` and on no other version. A different version is not known
    to be broken, only not measured, and that is enough to stop: a module that is missing raises ModuleNotFoundError only when the
    workflow RUNS. An output it cannot read is reported as unreadable and is a problem too, because a check that could not look must
    not say it matched.
    """
    match = re.match(r"\s*componentize-py\s+(\S+)", version_output)
    got = match.group(1) if match else None
    if got == PINNED_COMPONENTIZE_PY:
        return None
    installed = f"componentize-py {got}" if got else f"a componentize-py whose version could not be read ({version_output.strip()[:60]!r})"
    return (
        f"{installed} is installed, but this SDK was tested and measured with componentize-py {PINNED_COMPONENTIZE_PY}. "
        "componentize-py decides which standard-library modules a component contains, so a build with another version may contain "
        "different ones, and a module can raise ModuleNotFoundError only when the workflow runs."
    )


def componentize_py_refusal(problem: str) -> str:
    """The message of a build refused for ``problem``: what is wrong, the fix, and the explicit way to go on."""
    return (
        f"Error: {problem}\n"
        "Refusing to build. Install the measured version:\n"
        f"    pip install componentize-py=={PINNED_COMPONENTIZE_PY}\n"
        "or, if you know what you are doing, build with the version you have anyway:\n"
        f"    cleat build --target python {ALLOW_UNMEASURED_FLAG} ...\n"
        f"(this script takes the same {ALLOW_UNMEASURED_FLAG}, and both read {ALLOW_UNMEASURED_ENV}=1)."
    )


def componentize_py_override_warning(problem: str) -> str:
    """The warning printed when the build goes on past ``problem`` because the override was given."""
    return f"Warning: {problem} Building anyway, because {ALLOW_UNMEASURED_FLAG} (or {ALLOW_UNMEASURED_ENV}=1) was given."


def unmeasured_componentize_py_allowed(flag: bool, environ) -> bool:
    """Whether the override was asked for, by the flag or by ``CLEAT_ALLOW_UNMEASURED_COMPONENTIZE_PY=1``. Only the value ``1`` counts: ``0``, ``false`` and the empty string do not."""
    return bool(flag) or environ.get(ALLOW_UNMEASURED_ENV) == "1"


def run_componentize_py(
    entry_file: str,
    func_name: str,
    output: str,
    wit_dir: Path,
    sdk_root: Path,
    verbose: bool = False,
    allow_unmeasured: bool = False,
) -> bool:
    """Run componentize-py to compile the workflow to WASM.

    Returns True on success, False on failure. A componentize-py that is not the measured version is neither: it exits the process
    with ``EXIT_UNMEASURED_COMPONENTIZE_PY`` before compiling anything, unless ``allow_unmeasured`` (cleat#4383).
    """
    # Check if componentize-py is available
    try:
        result = subprocess.run(
            ["componentize-py", "--version"],
            capture_output=True, text=True
        )
        if result.returncode != 0:
            print("Error: componentize-py not found or not working", file=sys.stderr)
            print(f"Install with: pip install componentize-py=={PINNED_COMPONENTIZE_PY}", file=sys.stderr)
            return False
        problem = componentize_py_version_problem(result.stdout)
        if problem:
            if not allow_unmeasured:
                print(componentize_py_refusal(problem), file=sys.stderr)
                sys.exit(EXIT_UNMEASURED_COMPONENTIZE_PY)
            print(componentize_py_override_warning(problem), file=sys.stderr)
    except FileNotFoundError:
        print("Error: componentize-py not found", file=sys.stderr)
        print(f"Install with: pip install componentize-py=={PINNED_COMPONENTIZE_PY}", file=sys.stderr)
        return False

    entry_dir = Path(entry_file).resolve().parent
    entry_name = Path(entry_file).stem

    # Build the componentize-py command.
    # componentize-py 0.13+ moved --wit/--world to top-level flags (-d/-w)
    # and expects a module name (not file path) after the componentize subcommand.
    cmd = [
        "componentize-py",
        "-d", str(wit_dir),
        "-w", "cleat-workflow",
        "componentize", entry_name,
        "-p", str(sdk_root),
        "-p", str(entry_dir),
        "-o", output,
    ]

    if verbose:
        print(f"Running: {' '.join(cmd)}")

    # Set PYTHONPATH to include the SDK
    env = os.environ.copy()
    pythonpath = str(sdk_root)
    if "PYTHONPATH" in env:
        pythonpath = f"{sdk_root}:{env['PYTHONPATH']}"
    env["PYTHONPATH"] = pythonpath

    try:
        result = subprocess.run(
            cmd,
            capture_output=True,
            text=True,
            cwd=str(entry_dir),
            env=env,
            timeout=300,  # 5-minute timeout for compilation
        )

        if verbose:
            if result.stdout:
                print(result.stdout)
            if result.stderr:
                print(result.stderr, file=sys.stderr)

        if result.returncode != 0:
            stderr = result.stderr.strip() if result.stderr else ""
            stdout = result.stdout.strip() if result.stdout else ""
            details = f" (stderr: {stderr})" if stderr else ""
            details += f" (stdout: {stdout})" if stdout and not stderr else ""
            print(f"Error: componentize-py failed with exit code {result.returncode}{details}",
                  file=sys.stderr)
            return False

        return True

    except subprocess.TimeoutExpired:
        print("Error: componentize-py compilation timed out (5 minutes). "
              "Try simplifying the workflow or increasing the timeout.",
              file=sys.stderr)
        return False
    # Deliberate: this is a build script's top-level reporter. Any failure of
    # the toolchain becomes a readable message and a non-zero exit, not a
    # traceback the user has to interpret.
    except Exception as e:  # noqa: BLE001
        print(f"Error running componentize-py: {e}", file=sys.stderr)
        return False


def get_wasm_info(output: str) -> dict:
    """Get info about the compiled WASM file."""
    output_path = Path(output)
    if not output_path.exists():
        return {"exists": False}

    size = output_path.stat().st_size
    return {
        "exists": True,
        "path": str(output_path.absolute()),
        "size_bytes": size,
        "size_mb": round(size / (1024 * 1024), 2),
    }


def main():
    parser = argparse.ArgumentParser(
        description="Build a Python workflow into a WASM component, with optional metadata stamping"
    )
    parser.add_argument(
        "--entry", "-e",
        required=True,
        help="Entry point in 'file.py:func_name' format",
    )
    parser.add_argument(
        "--output", "-o",
        default=None,
        help="Output .wasm file path (default: <func_name>.wasm)",
    )
    parser.add_argument(
        "--verbose", "-v",
        action="store_true",
        help="Verbose output from componentize-py",
    )
    parser.add_argument(
        "--validate-only",
        action="store_true",
        help="Only validate the entry point, don't compile",
    )
    # Metadata stamping options.
    parser.add_argument(
        "--name",
        default=None,
        help="Workflow name (or CLEAT_WORKFLOW_NAME env var)",
    )
    parser.add_argument(
        "--version",
        type=int,
        default=None,
        help="Workflow version (or CLEAT_WORKFLOW_VERSION env var)",
    )
    parser.add_argument(
        "--min-version",
        type=int,
        default=None,
        dest="min_version",
        help="Min compatible version (or CLEAT_MIN_COMPATIBLE_VERSION env var)",
    )
    parser.add_argument(
        ALLOW_UNMEASURED_FLAG,
        action="store_true",
        dest="allow_unmeasured_componentize_py",
        help=f"Build even when the installed componentize-py is not {PINNED_COMPONENTIZE_PY}, the version this SDK was measured on "
             f"(or set {ALLOW_UNMEASURED_ENV}=1). Without it such a build is refused, exit {EXIT_UNMEASURED_COMPONENTIZE_PY}, "
             "because a standard-library module can be missing only when the workflow runs.",
    )
    parser.add_argument(
        "--abi-version",
        type=int,
        default=None,
        dest="abi_version",
        help="ABI version (or CLEAT_ABI_VERSION env var)",
    )
    parser.add_argument(
        "--plugin-deps",
        default=None,
        dest="plugin_deps",
        help="Plugin dependencies JSON (or CLEAT_PLUGIN_DEPS env var)",
    )
    parser.add_argument(
        "--child-binding-policy",
        default=None,
        dest="child_binding_policy",
        help="Child binding policy (or CLEAT_CHILD_BINDING_POLICY env var)",
    )
    parser.add_argument(
        "--entry-points",
        default=None,
        dest="entry_points",
        help="Comma-separated entry point names (or CLEAT_ENTRY_POINTS env var); cleat#2914",
    )
    parser.add_argument(
        "--skip-decompose",
        action="store_true",
        help="Skip the wasm-tools component decompose step. "
             "The output will be a WASM Component Model binary.",
    )
    parser.add_argument(
        "--keep-component",
        action="store_true",
        help="Keep the original Component Model binary alongside the decomposed core module. "
             "Saved as <output>.component.wasm.",
    )
    parser.add_argument(
        "--output-core",
        default=None,
        help="Path for the decomposed core WASM output file. "
             "Defaults to the --output path (overwriting the Component Model binary).",
    )
    parser.add_argument(
        "--runtime",
        default=None,
        help="Target WASM runtime: 'wasmtime' or 'wazero'. "
             "When 'wasmtime', or when unset (the default), the output is the WASM Component "
             "Model binary componentize-py produced, which is what the worker runs; no other "
             "tool is needed. "
             "When 'wazero', the output is a decomposed core WASM module, which needs a wasm-tools "
             "with `component decompose`; none of the releases checked (1.200.0 to 1.261.0) has it, "
             "so this exits 1 (wazero backs developer tooling only, not the worker).",
    )

    args = parser.parse_args()

    try:
        entry_file, func_name = parse_entry(args.entry)
    except ValueError as e:
        print(f"Error: {e}", file=sys.stderr)
        sys.exit(1)

    layout = SdkLayout()
    atexit.register(layout.close)
    wit_dir = layout.wit_dir

    if not wit_dir.exists():
        print(f"Error: WIT directory not found: {wit_dir}. "
              "This build script ships with cleat-sdk: reinstall it with "
              "'pip install --force-reinstall cleat-sdk'.",
              file=sys.stderr)
        sys.exit(1)

    # Validate entry
    try:
        info = validate_entry(entry_file, func_name)
        print(f"Validated entry: {info['entry_file']} -> {info['func_name']}()")
    except (FileNotFoundError, ValueError) as e:
        print(f"Error: {e}", file=sys.stderr)
        sys.exit(1)

    if args.validate_only:
        print("Validation passed. Entry point is ready for compilation.")
        sys.exit(0)

    # Determine output path
    output = args.output or f"{func_name}.wasm"

    layout.stage()

    print("Building WASM component...")
    print(f"  Entry:  {entry_file}:{func_name}")
    print(f"  Output: {output}")
    print(f"  WIT:    {wit_dir}")
    print()

    success = run_componentize_py(
        entry_file, func_name, output, wit_dir, layout.python_path, args.verbose,
        allow_unmeasured=unmeasured_componentize_py_allowed(args.allow_unmeasured_componentize_py, os.environ),
    )

    if not success:
        print("\nBuild FAILED", file=sys.stderr)
        print("\nTroubleshooting tips:", file=sys.stderr)
        print(f"  1. Ensure componentize-py is installed: pip install componentize-py=={PINNED_COMPONENTIZE_PY}",
              file=sys.stderr)
        print("  2. Ensure PYTHONPATH includes the cleat SDK", file=sys.stderr)
        print("  3. Try with --verbose for detailed error output", file=sys.stderr)
        sys.exit(1)

    # Resolve output path to absolute. The componentize-py subprocess runs
    # with entry_dir as CWD, so a relative output path is relative to
    # entry_dir, not the script's CWD.
    if not os.path.isabs(output):
        entry_dir_resolved = Path(entry_file).resolve().parent
        output = str(entry_dir_resolved / output)

    # Give the module a shareable memory image (cleat#4382). Before the size report, so the report is of the file that ships.
    dense_padding = densify_output(output) if os.path.isfile(output) else 0

    info = get_wasm_info(output)
    if not info.get("exists"):
        print("\nBuild SUCCESS", file=sys.stderr)
        print(f"  Warning: WASM output not found at {output}", file=sys.stderr)
    else:
        print("\nBuild SUCCESS")
        print(f"  Output: {info['path']}")
        print(f"  Size:   {info['size_bytes']:,} bytes ({info['size_mb']} MB)")

    # The zero gaps the dense rewrite adds are not CPython growing: the warning is about the component as componentize-py made it.
    if info["size_mb"] - dense_padding / 1048576 > 25:
        print(f"  Warning: WASM binary is large ({info['size_mb']} MB).")
        print("  This is expected for CPython-in-WASM but may affect load times.")

    # ---- Apply --runtime to control decomposition behavior ----
    # --runtime wasmtime  -> no decomposition; wasmtime runs the component itself
    # --runtime wazero    -> decompose, keep only core WASM (no component)
    # No --runtime        -> the same as wasmtime: no decomposition (cleat#3818)
    #
    # The wasmtime branch used to set skip_decompose = False, with a comment
    # reading "(wasmtime backend uses core WASM)". That stopped being true on
    # 2026-08-05: engine/component_cgo.go hands the component straight to
    # wasmtime's own Component Model runtime, which is how Python runs at all
    # (IMPROVEMENT-PLAN 2.72, 1.5/2.28). Decomposing for wasmtime now produces
    # an artifact nothing consumes, and made the build depend on a tool it does
    # not need.
    #
    # The error path below still tells people to "use --runtime wasmtime to
    # skip this step", which was false in the other direction -- that flag was
    # the one branch guaranteed *not* to skip it. Now it is true.
    if args.runtime == "wasmtime":
        args.skip_decompose = True
        args.keep_component = True
    elif args.runtime == "wazero":
        args.skip_decompose = False
        args.keep_component = False
    elif args.runtime is None:
        # Default: the component, as for wasmtime (cleat#3818).
        #
        # This used to "produce both formats", with decomposition best-effort, and it
        # warned on every default build on every machine: wasm-tools absent printed "no
        # core WASM module was produced", and a wasm-tools new enough to have dropped
        # `component decompose` (1.261.0 has) printed "not available in this version.
        # Install an older wasm-tools", which is advice to downgrade a tool for an output
        # nothing consumes. The component is what the worker runs Python from, and the
        # core module is only for wazero, which backs developer tooling and is chosen
        # with --runtime wazero. Measured 2026-11-14 on a hello-world build: both of
        # those configurations wrote a component (magic 00 61 73 6d 0d 00 01 00) and
        # exited 0 with the warning. By reading, not run (the installed 1.261.0 cannot
        # decompose): a wasm-tools OLD enough to decompose replaced the output with the
        # core module (the os.replace below), so the default output depended on which
        # tool version happened to be installed.
        args.skip_decompose = True
        args.keep_component = True

    # ---- Decompose component model to core WASM ----
    # componentize-py produces a WASM Component Model binary, but wazero
    # only runs core WASM modules. Use wasm-tools to decompose the component.
    if args.skip_decompose:
        print("  Decompose skipped: output is a WASM Component Model binary (not core WASM)")
    else:
        # Preserve the component model binary if requested
        if args.keep_component:
            component_backup = output + ".component.wasm"
            shutil.copy2(output, component_backup)
            print(f"  Preserved component model: {component_backup}")

        # Determine core WASM output path
        core_output = args.output_core or output

        # When the core output overwrites the component binary, use a temp
        # intermediate file so a partial write doesn't corrupt the original.
        use_temp = core_output == output
        decompose_output = output + ".core" if use_temp else core_output

        try:
            decompose_result = subprocess.run(
                ["wasm-tools", "component", "decompose", output, "-o", decompose_output],
                capture_output=True, text=True, timeout=60,
            )
            if decompose_result.returncode == 0:
                if use_temp:
                    os.replace(decompose_output, core_output)
                print("  Decomposed component model to core WASM module")
            else:
                stderr = decompose_result.stderr.strip() if decompose_result.stderr else ""
                # If wasm-tools removed the decompose subcommand (newer versions),
                # fall back gracefully rather than failing the build.
                if "unrecognized subcommand" in stderr or "unrecognized" in stderr:
                    # --runtime wazero asked for core WASM, and this is the one thing it cannot get here, so
                    # it is an error and not a warning (cleat#3870). The warning below is for a caller that
                    # did not ask for core WASM; since cleat#3818 the only way to reach this step is
                    # --runtime wazero, so that is the only branch a build normally takes.
                    #
                    # WHAT TO TELL THE READER. This used to say "Install an older wasm-tools", and that advice
                    # is not one that was found to work: `wasm-tools component decompose` is absent from every
                    # release that was checked on 2026-11-14 (1.200.0, 1.210.0, 1.220.0, 1.225.0, 1.228.0,
                    # 1.229.0 and 1.261.0; the engine tests record it as removed in 1.230, but 1.229.0 and
                    # everything older that was tried lacks it too). So no version is named, and the way out
                    # that is known to work is the one that does not need the tool.
                    if args.runtime == "wazero":
                        print("Error: --runtime wazero needs core WASM, and the wasm-tools on PATH cannot produce it",
                              file=sys.stderr)
                        print("", file=sys.stderr)
                        print("  This build decomposes the component with `wasm-tools component decompose`, and",
                              file=sys.stderr)
                        print("  that subcommand does not exist in this wasm-tools (see `wasm-tools --version`).",
                              file=sys.stderr)
                        print("  wazero cannot load a WASM Component Model binary, so there is no output this",
                              file=sys.stderr)
                        print("  build can give it. Build with --runtime wasmtime (the default) instead: the",
                              file=sys.stderr)
                        print("  worker runs the component, and wazero backs developer tooling only.",
                              file=sys.stderr)
                        sys.exit(1)
                    print("  Warning: wasm-tools component decompose not available in this version.",
                          file=sys.stderr)
                    print("  The output is a WASM Component Model binary (usable with wasmtime).",
                          file=sys.stderr)
                else:
                    print(f"Error: wasm-tools decompose failed (exit code {decompose_result.returncode})",
                          file=sys.stderr)
                    if stderr:
                        print(f"  stderr: {stderr}", file=sys.stderr)
                    sys.exit(1)
        except FileNotFoundError:
            # Missing wasm-tools is fatal only when core WASM is the point --
            # that is, --runtime wazero, which cannot load a component.
            #
            # Otherwise it is not. The component has already been built at this
            # point, and since 2026-08-05 the component *is* the artifact the
            # engine runs: wasmtime serves Python through its own Component
            # Model runtime. So exiting 1 here failed a build that had already
            # produced the thing the user asked for, on a machine missing a tool
            # needed for an output nothing consumes.
            #
            # Observed rather than theorised. `cleat build --target python` in a
            # container without wasm-tools printed "Build SUCCESS", the 18.33 MB
            # component path, and then exited 1 -- and
            # TestPluginCalls_Wasm_Python read that exit code as "componentize-py
            # pipeline may need setup" and skipped. A build failure that reports
            # success first is the worst of both.
            if args.runtime == "wazero":
                print("Error: wasm-tools not found, and --runtime wazero needs core WASM",
                      file=sys.stderr)
                print("", file=sys.stderr)
                print("wazero cannot load a WASM Component Model binary, so decomposition",
                      file=sys.stderr)
                print("is required for this runtime. Install wasm-tools:", file=sys.stderr)
                print("  cargo install wasm-tools", file=sys.stderr)
                print("  # or on macOS:", file=sys.stderr)
                print("  brew install wasm-tools", file=sys.stderr)
                sys.exit(1)
            print("  Warning: wasm-tools not found, so no core WASM module was produced.",
                  file=sys.stderr)
            print("  The build succeeded: the output is a WASM Component Model binary,",
                  file=sys.stderr)
            print("  which is what the cleat engine runs Python from. Install wasm-tools",
                  file=sys.stderr)
            print("  (cargo install wasm-tools) only if you need core WASM as well.",
                  file=sys.stderr)
        # Deliberate: same reasoning as the compile step above.
        except Exception as e:  # noqa: BLE001
            print(f"Error: component decomposition failed: {e}", file=sys.stderr)
            sys.exit(1)

    # ---- Post-compile metadata stamping ----
    stamp_name = args.name or os.environ.get("CLEAT_WORKFLOW_NAME")
    stamp_version = args.version
    if stamp_version is None and os.environ.get("CLEAT_WORKFLOW_VERSION"):
        stamp_version = int(os.environ["CLEAT_WORKFLOW_VERSION"])
    stamp_min = args.min_version
    if stamp_min is None and os.environ.get("CLEAT_MIN_COMPATIBLE_VERSION"):
        stamp_min = int(os.environ["CLEAT_MIN_COMPATIBLE_VERSION"])
    stamp_abi = args.abi_version
    if stamp_abi is None and os.environ.get("CLEAT_ABI_VERSION"):
        stamp_abi = int(os.environ["CLEAT_ABI_VERSION"])
    stamp_deps = args.plugin_deps or os.environ.get("CLEAT_PLUGIN_DEPS")
    stamp_child_binding_policy = args.child_binding_policy or os.environ.get("CLEAT_CHILD_BINDING_POLICY")
    stamp_entry_points = args.entry_points or os.environ.get("CLEAT_ENTRY_POINTS")

    # Always stamp at least the language so consumers can identify the source.
    stamp_args = [
        sys.executable,
        str(layout.stamp_script),
        output,
        "--language", "python",
    ]
    if stamp_name is not None:
        stamp_args.extend(["--name", stamp_name])
    if stamp_version is not None:
        stamp_args.extend(["--version", str(stamp_version)])
    if stamp_min is not None:
        stamp_args.extend(["--min-version", str(stamp_min)])
    if stamp_abi is not None:
        stamp_args.extend(["--abi-version", str(stamp_abi)])
    if stamp_deps is not None:
        stamp_args.extend(["--plugin-deps", stamp_deps])
    if stamp_child_binding_policy is not None:
        stamp_args.extend(["--child-binding-policy", stamp_child_binding_policy])
    if stamp_entry_points is not None:
        stamp_args.extend(["--entry-points", stamp_entry_points])
    if args.verbose:
        stamp_args.append("--verbose")

    try:
        subprocess.run(stamp_args, capture_output=True, text=True, check=True)
        print("  Metadata stamped successfully.")
    except subprocess.CalledProcessError as e:
        err = e.stderr.strip() if e.stderr else ""
        reason = f": {err}" if err else ""
        print(f"  Warning: metadata stamping failed (non-fatal){reason}", file=sys.stderr)
        if args.verbose and e.stdout:
            print(e.stdout, file=sys.stderr)


if __name__ == "__main__":
    main()
