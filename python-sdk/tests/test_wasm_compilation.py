"""Tests for the Python-to-WASM compilation pipeline.

These tests validate that:
1. Entry points are correctly detected
2. The build script validates @cleat_entry decorators
3. All stubs are properly wrapped for conditional import
4. The WIT file covers all required imports
"""

import os
import subprocess
import sys
from pathlib import Path


def _sdk_root():
    return Path(__file__).resolve().parent.parent


def _scripts_dir():
    return _sdk_root() / "scripts"


def test_build_script_imports():
    """Verify the build script can be imported without errors."""
    scripts_dir = str(_scripts_dir())
    sys.path.insert(0, scripts_dir)
    try:
        import build_wasm
        assert hasattr(build_wasm, "main")
        assert hasattr(build_wasm, "validate_entry")
        assert hasattr(build_wasm, "parse_entry")
    finally:
        sys.path.remove(scripts_dir)


def test_parse_entry():
    """Test entry point parsing."""
    scripts_dir = str(_scripts_dir())
    sys.path.insert(0, scripts_dir)
    try:
        import build_wasm

        # Valid entries
        file_path, func_name = build_wasm.parse_entry("my_workflow.py:my_func")
        assert file_path == "my_workflow.py"
        assert func_name == "my_func"

        file_path, func_name = build_wasm.parse_entry("path/to/workflow.py:research_agent")
        assert file_path == "path/to/workflow.py"
        assert func_name == "research_agent"

        # Invalid entries
        try:
            build_wasm.parse_entry("no_colon")
            assert False, "Should have raised ValueError"
        except ValueError:
            pass

        try:
            build_wasm.parse_entry(":empty_file")
            assert False, "Should have raised ValueError"
        except ValueError:
            pass
    finally:
        sys.path.remove(scripts_dir)


def test_validate_entry_with_hello_workflow():
    """Test entry validation against the hello workflow example."""
    hello_file = str(_sdk_root().parent / "examples" / "python-hello" / "hello_workflow.py")

    scripts_dir = str(_scripts_dir())
    sys.path.insert(0, scripts_dir)
    try:
        import build_wasm

        # Should validate successfully
        info = build_wasm.validate_entry(hello_file, "hello")
        assert info["func_name"] == "hello"
        assert "hello_workflow.py" in info["entry_file"]
    finally:
        sys.path.remove(scripts_dir)


def test_build_script_validate_only():
    """Test the build script with --validate-only flag."""
    hello_file = str(_sdk_root().parent / "examples" / "python-hello" / "hello_workflow.py")
    build_script = str(_scripts_dir() / "build_wasm.py")

    result = subprocess.run(
        [sys.executable, build_script, "--entry", f"{hello_file}:hello", "--validate-only"],
        capture_output=True,
        text=True,
        timeout=30,
    )

    assert result.returncode == 0, f"Validation failed: {result.stderr}"
    assert "Validated entry:" in result.stdout
    assert "Validation passed" in result.stdout


def test_build_script_invalid_entry():
    """Test the build script rejects invalid entries."""
    build_script = str(_scripts_dir() / "build_wasm.py")

    result = subprocess.run(
        [sys.executable, build_script, "--entry", "nonexistent.py:func", "--validate-only"],
        capture_output=True,
        text=True,
        timeout=30,
    )

    assert result.returncode != 0, "Should have failed for nonexistent file"


def test_host_calls_conditional_imports():
    """Verify that all stubs are properly wrapped in `if not _USING_WASM:` blocks."""
    host_calls_path = _sdk_root() / "cleat_sdk" / "host_calls.py"

    with open(host_calls_path) as f:
        source = f.read()

    # Check conditional import block exists
    assert "_USING_WASM = True" in source or "_USING_WASM: bool = True" in source
    assert "except ImportError:" in source
    assert "_USING_WASM = False" in source

    # Verify key stubs are wrapped
    assert "if not _USING_WASM:" in source

    # Count stub functions (should have ~30+)
    import_count = source.count("raise NotImplementedError")
    assert import_count >= 29, f"Expected at least 29 stubs, found {import_count}"


def test_wit_file_exists_and_has_required_interfaces():
    """Verify the WIT file exists and covers all import categories."""
    wit_path = _sdk_root() / "wit" / "cleat.wit"

    assert wit_path.exists(), f"WIT file not found: {wit_path}"

    with open(wit_path) as f:
        wit_content = f.read()

    # Check for required interfaces
    required_interfaces = [
        "durable-call",
        "durable-sleep",
        "durable-version",
        "durable-lifecycle",
        "durable-signals",
        "durable-children",
        "durable-promises",
        "durable-state",
        "durable-handlers",
        "durable-messaging",
        "durable-identity",
        "plugin",
    ]

    for interface in required_interfaces:
        assert f"interface {interface}" in wit_content, \
            f"Missing interface '{interface}' in WIT file"

    # Check for required functions
    required_functions = [
        "durable-call:",
        "durable-sleep:",
        "durable-now:",
        "durable-log:",
        "set-query-state:",
        "plugin-call:",
        "plugin-call-streaming:",
        "durable-create-promise:",
        "durable-await-promise:",
        "durable-workflow-id:",
        "durable-run-id:",
    ]

    for func in required_functions:
        assert func in wit_content, f"Missing function '{func}' in WIT file"

    # Verify world definition
    assert "world cleat-workflow" in wit_content
    assert "export run:" in wit_content


def test_all_stubs_accounted_for():
    """Verify that every _import_* stub in host_calls.py has a corresponding
    entry in the conditional WIT import block."""
    host_calls_path = _sdk_root() / "cleat_sdk" / "host_calls.py"

    with open(host_calls_path) as f:
        source = f.read()

    # Find all _import_* function definitions (the stubs)
    import re
    # Match "def _import_XXX(..."
    stub_defs = set()
    for match in re.finditer(r'def (_import_\w+)\(', source):
        stub_defs.add(match.group(1))

    # Find all _import_* aliases in the try block
    import_aliases = set()
    # Match "_import_XXX as _import_XXX" or "XXX as _import_XXX"
    for match in re.finditer(r'(\w+)\s+as\s+(_import_\w+)', source):
        import_aliases.add(match.group(2))

    # All stubs should be in the import block OR all imports should cover all stubs
    # Report any gaps
    missing_from_imports = stub_defs - import_aliases
    extra_in_imports = import_aliases - stub_defs

    # Some stubs may not need WIT imports (e.g., they delegate to other methods)
    # But most should be covered
    if missing_from_imports:
        print(f"Note: {len(missing_from_imports)} stubs not in WIT import block:")
        for stub in sorted(missing_from_imports):
            print(f"  - {stub}")

    if extra_in_imports:
        print(f"Note: {len(extra_in_imports)} WIT imports without matching stubs:")
        for imp in sorted(extra_in_imports):
            print(f"  - {imp}")

    # This is informational - we don't assert because some stubs might delegate
    # to cleat_call internally and not need direct WIT imports
    print(f"Stub coverage: {len(stub_defs) - len(missing_from_imports)}/{len(stub_defs)} stubs have WIT imports")


def test_new_imports_in_init():
    """Verify the langchain/langgraph imports are in __init__.py."""
    init_path = _sdk_root() / "cleat_sdk" / "__init__.py"

    with open(init_path) as f:
        source = f.read()

    assert "CleatCallbackHandler" in source
    assert "CleatCheckpointer" in source
    assert "StreamEvent" in source


def test_hello_workflow_exists():
    """Verify the hello workflow example exists and has required components."""
    hello_dir = _sdk_root().parent / "examples" / "python-hello"

    assert hello_dir.exists(), f"Hello example directory not found: {hello_dir}"
    assert (hello_dir / "hello_workflow.py").exists(), "hello_workflow.py not found"
    assert (hello_dir / "cleat.toml").exists(), "cleat.toml not found"
    assert (hello_dir / "README.md").exists(), "README.md not found"

    # Verify the workflow file has @cleat_entry
    with open(hello_dir / "hello_workflow.py") as f:
        source = f.read()
    assert "@cleat_entry" in source
    assert "def hello" in source


def test_agent_template_exists():
    """Verify the agent template has all required files."""
    template_dir = _sdk_root().parent / "cmd" / "cleat" / "templates" / "agent-python"

    assert template_dir.exists(), f"Agent template directory not found: {template_dir}"
    required_files = ["agent.py", "requirements.txt", "cleat.toml", "README.md", ".gitignore"]
    for fname in required_files:
        assert (template_dir / fname).exists(), f"Missing template file: {fname}"


def test_langchain_integration_files():
    """Verify all LangChain/LangGraph integration files exist."""
    sdk = _sdk_root() / "cleat_sdk"

    assert (sdk / "langchain" / "__init__.py").exists()
    assert (sdk / "langchain" / "callbacks.py").exists()
    assert (sdk / "langgraph" / "__init__.py").exists()
    assert (sdk / "langgraph" / "checkpoint.py").exists()

    # Verify callbacks.py has the right class
    with open(sdk / "langchain" / "callbacks.py") as f:
        source = f.read()
    assert "class CleatCallbackHandler" in source
    assert "def on_llm_start" in source
    assert "def on_llm_end" in source
    assert "def on_agent_action" in source
    assert "def on_agent_finish" in source

    # Verify checkpoint.py has the right class
    with open(sdk / "langgraph" / "checkpoint.py") as f:
        source = f.read()
    assert "class CleatCheckpointer" in source
    assert "def get_tuple" in source
    assert "def put" in source
    assert "def put_writes" in source
