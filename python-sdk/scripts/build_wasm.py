#!/usr/bin/env python3
"""Build a Python workflow into a WASM component using componentize-py.

Usage:
    python build_wasm.py --entry my_workflow.py:function_name [--output my_workflow.wasm]

This script wraps the componentize-py tool to compile a Python workflow
function (decorated with @cleat_entry) into a WASM component that can
be loaded by the cleat worker runtime.
"""

import argparse
import os
import subprocess
import sys
from pathlib import Path


def find_sdk_root() -> Path:
    """Find the python-sdk root directory."""
    script_dir = Path(__file__).resolve().parent
    return script_dir.parent


def parse_entry(entry: str) -> tuple[str, str]:
    """Parse --entry argument 'file.py:func_name' into (file_path, func_name)."""
    if ":" not in entry:
        raise ValueError(f"Invalid entry format: {entry}. Expected 'file.py:func_name'")
    file_path, func_name = entry.rsplit(":", 1)
    file_path = file_path.strip()
    func_name = func_name.strip()
    if not file_path:
        raise ValueError(
            f"Invalid entry format: {entry!r}. File path is empty. Expected 'file.py:func_name'"
        )
    if not func_name:
        raise ValueError(
            f"Invalid entry format: {entry!r}. Function name is empty. Expected 'file.py:func_name'"
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
        raise ValueError(f"No @cleat_entry decorator found in {entry_file}")

    if f"def {func_name}" not in source:
        raise ValueError(f"Function '{func_name}' not found in {entry_file}")

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


def run_componentize_py(
    entry_file: str,
    func_name: str,
    output: str,
    wit_dir: Path,
    sdk_root: Path,
    verbose: bool = False,
) -> bool:
    """Run componentize-py to compile the workflow to WASM.

    Returns True on success, False on failure.
    """
    # Check if componentize-py is available
    try:
        result = subprocess.run(["componentize-py", "--version"], capture_output=True, text=True)
        if result.returncode != 0:
            print("Error: componentize-py not found or not working", file=sys.stderr)
            print("Install with: pip install componentize-py", file=sys.stderr)
            return False
    except FileNotFoundError:
        print("Error: componentize-py not found", file=sys.stderr)
        print("Install with: pip install componentize-py", file=sys.stderr)
        return False

    entry_dir = Path(entry_file).resolve().parent
    module_name = Path(entry_file).stem  # componentize-py expects a module name, not a file path

    # Build the componentize-py command using the modern CLI syntax
    # (compatible with componentize-py >= 0.23.0).
    cmd = [
        "componentize-py",
        "-d",
        str(wit_dir),
        "-w",
        "cleat-workflow",
        "componentize",
        module_name,
        "-p",
        str(sdk_root),
        "-p",
        str(entry_dir),
        "-o",
        output,
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
            print(
                f"Error: componentize-py failed with exit code {result.returncode}{details}",
                file=sys.stderr,
            )
            return False

        return True

    except subprocess.TimeoutExpired:
        print(
            "Error: componentize-py compilation timed out (5 minutes). "
            "Try simplifying the workflow or increasing the timeout.",
            file=sys.stderr,
        )
        return False
    except Exception as e:
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
        "--entry",
        "-e",
        required=True,
        help="Entry point in 'file.py:func_name' format",
    )
    parser.add_argument(
        "--output",
        "-o",
        default=None,
        help="Output .wasm file path (default: <func_name>.wasm)",
    )
    parser.add_argument(
        "--verbose",
        "-v",
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

    args = parser.parse_args()

    try:
        entry_file, func_name = parse_entry(args.entry)
    except ValueError as e:
        print(f"Error: {e}", file=sys.stderr)
        sys.exit(1)

    sdk_root = find_sdk_root()
    wit_dir = sdk_root / "wit"

    if not wit_dir.exists():
        print(
            f"Error: WIT directory not found: {wit_dir}. "
            "Run 'componentize-py init' to initialize the WIT directory.",
            file=sys.stderr,
        )
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

    print("Building WASM component...")
    print(f"  Entry:  {entry_file}:{func_name}")
    print(f"  Output: {output}")
    print(f"  WIT:    {wit_dir}")
    print()

    success = run_componentize_py(entry_file, func_name, output, wit_dir, sdk_root, args.verbose)

    if not success:
        print("\nBuild FAILED", file=sys.stderr)
        print("\nTroubleshooting tips:", file=sys.stderr)
        print(
            "  1. Ensure componentize-py is installed: pip install componentize-py", file=sys.stderr
        )
        print("  2. Ensure PYTHONPATH includes the cleat SDK", file=sys.stderr)
        print("  3. Try with --verbose for detailed error output", file=sys.stderr)
        sys.exit(1)

    info = get_wasm_info(output)
    print("\nBuild SUCCESS")
    print(f"  Output: {info['path']}")
    print(f"  Size:   {info['size_bytes']:,} bytes ({info['size_mb']} MB)")

    if info["size_mb"] > 25:
        print(f"  Warning: WASM binary is large ({info['size_mb']} MB).")
        print("  This is expected for CPython-in-WASM but may affect load times.")

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

    # If any stamping arg is set, run stamp_metadata.py.
    if stamp_name is not None or stamp_version is not None:
        stamp_args = [
            sys.executable,
            str(sdk_root / "scripts" / "stamp_metadata.py"),
            output,
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
