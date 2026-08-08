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
import shutil
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
             "When 'wasmtime', the output is decomposed core WASM with component backup. "
             "When 'wazero', the output is a decomposed core WASM module. "
             "When unset, both formats are produced (default).",
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
        print(f"Error: WIT directory not found: {wit_dir}. "
              "Run 'componentize-py init' to initialize the WIT directory.",
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

    print("Building WASM component...")
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

    # Resolve output path to absolute. The componentize-py subprocess runs
    # with entry_dir as CWD, so a relative output path is relative to
    # entry_dir, not the script's CWD.
    if not os.path.isabs(output):
        entry_dir_resolved = Path(entry_file).resolve().parent
        output = str(entry_dir_resolved / output)

    info = get_wasm_info(output)
    if not info.get("exists"):
        print("\nBuild SUCCESS", file=sys.stderr)
        print(f"  Warning: WASM output not found at {output}", file=sys.stderr)
    else:
        print("\nBuild SUCCESS")
        print(f"  Output: {info['path']}")
        print(f"  Size:   {info['size_bytes']:,} bytes ({info['size_mb']} MB)")

    if info["size_mb"] > 25:
        print(f"  Warning: WASM binary is large ({info['size_mb']} MB).")
        print("  This is expected for CPython-in-WASM but may affect load times.")

    # ---- Apply --runtime to control decomposition behavior ----
    # --runtime wasmtime  -> no decomposition; wasmtime runs the component itself
    # --runtime wazero    -> decompose, keep only core WASM (no component)
    # No --runtime        -> produce both, but decomposition is best-effort
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
        # Default: produce both formats.
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
                    print("  Warning: wasm-tools component decompose not available in this version.",
                          file=sys.stderr)
                    print("  The output is a WASM Component Model binary (usable with wasmtime).",
                          file=sys.stderr)
                    print("  Install an older wasm-tools or use --runtime wasmtime to skip this step.",
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

    # Always stamp at least the language so consumers can identify the source.
    stamp_args = [
        sys.executable,
        str(sdk_root / "scripts" / "stamp_metadata.py"),
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
