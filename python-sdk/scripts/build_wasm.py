#!/usr/bin/env python3
"""Build a Python workflow into a WASM component using componentize-py.

Usage:
    python build_wasm.py --entry my_workflow.py:function_name [--output my_workflow.wasm]

This script wraps the componentize-py tool to compile a Python workflow
function (decorated with @cleat_entry) into a WASM component that can
be loaded by the cleat worker runtime.
"""

import argparse
import json
import os
import subprocess
import sys
import tempfile
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
        result = subprocess.run(
            ["componentize-py", "--version"],
            capture_output=True, text=True
        )
        if result.returncode != 0:
            print("Error: componentize-py not found or not working", file=sys.stderr)
            print("Install with: pip install componentize-py", file=sys.stderr)
            return False
    except FileNotFoundError:
        print("Error: componentize-py not found", file=sys.stderr)
        print("Install with: pip install componentize-py", file=sys.stderr)
        return False

    entry_dir = Path(entry_file).resolve().parent
    entry_name = Path(entry_file).stem

    # Build the componentize-py command
    cmd = [
        "componentize-py",
        "componentize",
        str(entry_file),
        "--wit-path", str(wit_dir),
        "--world", "cleat-workflow",
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
            print(f"Error: componentize-py failed with exit code {result.returncode}",
                  file=sys.stderr)
            if result.stderr:
                print(result.stderr, file=sys.stderr)
            return False

        return True

    except subprocess.TimeoutExpired:
        print("Error: componentize-py compilation timed out (5 minutes)", file=sys.stderr)
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
        description="Build a Python workflow into a WASM component"
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

    args = parser.parse_args()

    try:
        entry_file, func_name = parse_entry(args.entry)
    except ValueError as e:
        print(f"Error: {e}", file=sys.stderr)
        sys.exit(1)

    sdk_root = find_sdk_root()
    wit_dir = sdk_root / "wit"

    if not wit_dir.exists():
        print(f"Error: WIT directory not found: {wit_dir}", file=sys.stderr)
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

    print(f"Building WASM component...")
    print(f"  Entry:  {entry_file}:{func_name}")
    print(f"  Output: {output}")
    print(f"  WIT:    {wit_dir}")
    print()

    success = run_componentize_py(
        entry_file, func_name, output, wit_dir, sdk_root, args.verbose
    )

    if not success:
        print("\nBuild FAILED", file=sys.stderr)
        print("\nTroubleshooting tips:", file=sys.stderr)
        print("  1. Ensure componentize-py is installed: pip install componentize-py",
              file=sys.stderr)
        print("  2. Ensure PYTHONPATH includes the cleat SDK", file=sys.stderr)
        print("  3. Try with --verbose for detailed error output", file=sys.stderr)
        sys.exit(1)

    info = get_wasm_info(output)
    print(f"\nBuild SUCCESS")
    print(f"  Output: {info['path']}")
    print(f"  Size:   {info['size_bytes']:,} bytes ({info['size_mb']} MB)")

    if info["size_mb"] > 25:
        print(f"  Warning: WASM binary is large ({info['size_mb']} MB).")
        print(f"  This is expected for CPython-in-WASM but may affect load times.")


if __name__ == "__main__":
    main()
