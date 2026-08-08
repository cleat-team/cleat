"""Comprehensive tests for ``cleat_sdk.vet`` — static analysis of Cleat workflow code.

Covers all 13 error codes:

- **E001**  — file not found / syntax error
- **PY002** — File I/O (open)
- **PY003** — Direct HTTP (requests, urllib, http.client)
- **PY004** — time.sleep()
- **PY005** — time.time()
- **PY006** — random.*
- **PY007** — os.*, subprocess.*
- **PY008** — threading, asyncio, multiprocessing
- **PY009** — socket.*
- **PY010** — print()
- **PY011** — HostCalls param missing in durable closure
- **PY012** — async def decorated with @cleat_entry
- **PY013** — multiple @cleat_entry functions
"""

from __future__ import annotations

import textwrap

from cleat_sdk.vet import analyze_file, detect_entry

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------


def _assert_analyze(tmp_path, source: str, expected_code: str, filename: str = "test.py") -> None:
    """Assert that ``analyze_file`` produces *expected_code* for *source*."""
    f = tmp_path / filename
    f.write_text(textwrap.dedent(source))
    result = analyze_file(str(f))
    codes = {e["code"] for e in result.errors}
    assert expected_code in codes, f"Expected {expected_code!r} in errors, got {codes}"


def _assert_no_error(tmp_path, source: str, code: str, filename: str = "test.py") -> None:
    """Assert that ``analyze_file`` does NOT produce *code* for *source*."""
    f = tmp_path / filename
    f.write_text(textwrap.dedent(source))
    result = analyze_file(str(f))
    codes = {e["code"] for e in result.errors}
    assert code not in codes, f"Did not expect {code!r} in errors, got {codes}"


def _write(tmp_path, source: str, filename: str = "test.py") -> str:
    """Write *source* to a temp file and return its string path."""
    f = tmp_path / filename
    f.write_text(textwrap.dedent(source))
    return str(f)


# ===================================================================
# E001 -- File / syntax errors
# ===================================================================


def test_e001_file_not_found(tmp_path) -> None:
    """analyze_file returns E001 when the file does not exist."""
    result = analyze_file(str(tmp_path / "nonexistent.py"))
    codes = {e["code"] for e in result.errors}
    assert "E001" in codes


def test_e001_syntax_error(tmp_path) -> None:
    """analyze_file returns E001 for files with Python syntax errors."""
    f = tmp_path / "bad_syntax.py"
    f.write_text("def foo(:\n")
    result = analyze_file(str(f))
    codes = {e["code"] for e in result.errors}
    assert "E001" in codes


# ===================================================================
# PY002 -- File I/O (open)
# ===================================================================


def test_py002_open_not_allowed(tmp_path) -> None:
    """open() inside a durable-closure function triggers PY002."""
    _assert_analyze(
        tmp_path,
        """\
        def my_func(h):
            call("foo")
            open("data.txt")
    """,
        "PY002",
    )


def test_py002_open_module_level(tmp_path) -> None:
    """open() at module level also triggers PY002."""
    _assert_analyze(tmp_path, "open('file.txt')\n", "PY002")


# ===================================================================
# PY003 -- Direct HTTP (requests, urllib, http.client)
# ===================================================================


def test_py003_import_requests(tmp_path) -> None:
    """import requests triggers PY003."""
    _assert_analyze(tmp_path, "import requests\n", "PY003")


def test_py003_from_requests_import(tmp_path) -> None:
    """from requests import ... triggers PY003."""
    _assert_analyze(tmp_path, "from requests import get\n", "PY003")


def test_py003_import_urllib(tmp_path) -> None:
    """import urllib triggers PY003."""
    _assert_analyze(tmp_path, "import urllib\n", "PY003")


def test_py003_from_urllib_import(tmp_path) -> None:
    """from urllib import ... triggers PY003."""
    _assert_analyze(tmp_path, "from urllib import request\n", "PY003")


def test_py003_import_http_client(tmp_path) -> None:
    """import http.client triggers PY003."""
    _assert_analyze(tmp_path, "import http.client\n", "PY003")


# ===================================================================
# PY004 -- time.sleep
# ===================================================================


def test_py004_time_sleep(tmp_path) -> None:
    """time.sleep() inside a durable-closure function triggers PY004."""
    _assert_analyze(
        tmp_path,
        """\
        def my_func(h):
            call("foo")
            time.sleep(1)
    """,
        "PY004",
    )


# ===================================================================
# PY005 -- time.time
# ===================================================================


def test_py005_time_time(tmp_path) -> None:
    """time.time() inside a durable-closure function triggers PY005."""
    _assert_analyze(
        tmp_path,
        """\
        def my_func(h):
            call("foo")
            _ = time.time()
    """,
        "PY005",
    )


# ===================================================================
# PY006 -- random.*
# ===================================================================


def test_py006_random_random(tmp_path) -> None:
    """random.random() inside a durable-closure function triggers PY006."""
    _assert_analyze(
        tmp_path,
        """\
        def my_func(h):
            call("foo")
            random.random()
    """,
        "PY006",
    )


def test_py006_random_randint(tmp_path) -> None:
    """random.randint() inside a durable-closure function triggers PY006."""
    _assert_analyze(
        tmp_path,
        """\
        def my_func(h):
            call("foo")
            random.randint(1, 10)
    """,
        "PY006",
    )


def test_py006_random_uniform(tmp_path) -> None:
    """random.uniform() inside a durable-closure function triggers PY006."""
    _assert_analyze(
        tmp_path,
        """\
        def my_func(h):
            call("foo")
            random.uniform(0, 1)
    """,
        "PY006",
    )


def test_py006_random_choice(tmp_path) -> None:
    """random.choice() inside a durable-closure function triggers PY006."""
    _assert_analyze(
        tmp_path,
        """\
        def my_func(h):
            call("foo")
            random.choice([1, 2, 3])
    """,
        "PY006",
    )


def test_py006_random_shuffle(tmp_path) -> None:
    """random.shuffle() inside a durable-closure function triggers PY006."""
    _assert_analyze(
        tmp_path,
        """\
        def my_func(h):
            call("foo")
            lst = [1, 2, 3]
            random.shuffle(lst)
    """,
        "PY006",
    )


# ===================================================================
# PY007 -- OS operations / subprocess
# ===================================================================


def test_py007_os_system(tmp_path) -> None:
    """os.system() inside a durable-closure function triggers PY007."""
    _assert_analyze(
        tmp_path,
        """\
        def my_func(h):
            call("foo")
            os.system("cmd")
    """,
        "PY007",
    )


def test_py007_os_popen(tmp_path) -> None:
    """os.popen() inside a durable-closure function triggers PY007."""
    _assert_analyze(
        tmp_path,
        """\
        def my_func(h):
            call("foo")
            os.popen("cmd")
    """,
        "PY007",
    )


def test_py007_os_getenv(tmp_path) -> None:
    """os.getenv() inside a durable-closure function triggers PY007."""
    _assert_analyze(
        tmp_path,
        """\
        def my_func(h):
            call("foo")
            os.getenv("HOME")
    """,
        "PY007",
    )


def test_py007_os_environ_subscript(tmp_path) -> None:
    """os.environ[KEY] inside a durable-closure function triggers PY007."""
    _assert_analyze(
        tmp_path,
        """\
        def my_func(h):
            call("foo")
            _ = os.environ["HOME"]
    """,
        "PY007",
    )


def test_py007_os_listdir(tmp_path) -> None:
    """os.listdir() inside a durable-closure function triggers PY007."""
    _assert_analyze(
        tmp_path,
        """\
        def my_func(h):
            call("foo")
            os.listdir(".")
    """,
        "PY007",
    )


def test_py007_os_remove(tmp_path) -> None:
    """os.remove() inside a durable-closure function triggers PY007."""
    _assert_analyze(
        tmp_path,
        """\
        def my_func(h):
            call("foo")
            os.remove("f.txt")
    """,
        "PY007",
    )


def test_py007_os_rename(tmp_path) -> None:
    """os.rename() inside a durable-closure function triggers PY007."""
    _assert_analyze(
        tmp_path,
        """\
        def my_func(h):
            call("foo")
            os.rename("a", "b")
    """,
        "PY007",
    )


def test_py007_os_mkdir(tmp_path) -> None:
    """os.mkdir() inside a durable-closure function triggers PY007."""
    _assert_analyze(
        tmp_path,
        """\
        def my_func(h):
            call("foo")
            os.mkdir("d")
    """,
        "PY007",
    )


def test_py007_os_walk(tmp_path) -> None:
    """os.walk() inside a durable-closure function triggers PY007."""
    _assert_analyze(
        tmp_path,
        """\
        def my_func(h):
            call("foo")
            os.walk(".")
    """,
        "PY007",
    )


def test_py007_os_exit(tmp_path) -> None:
    """os.exit() inside a durable-closure function triggers PY007."""
    _assert_analyze(
        tmp_path,
        """\
        def my_func(h):
            call("foo")
            os.exit(1)
    """,
        "PY007",
    )


def test_py007_subprocess_call(tmp_path) -> None:
    """subprocess.call() inside a durable-closure function triggers PY007."""
    _assert_analyze(
        tmp_path,
        """\
        def my_func(h):
            call("foo")
            subprocess.call(["ls"])
    """,
        "PY007",
    )


def test_py007_subprocess_run(tmp_path) -> None:
    """subprocess.run() inside a durable-closure function triggers PY007."""
    _assert_analyze(
        tmp_path,
        """\
        def my_func(h):
            call("foo")
            subprocess.run(["ls"])
    """,
        "PY007",
    )


def test_py007_subprocess_popen(tmp_path) -> None:
    """subprocess.Popen() inside a durable-closure function triggers PY007."""
    _assert_analyze(
        tmp_path,
        """\
        def my_func(h):
            call("foo")
            subprocess.Popen(["ls"])
    """,
        "PY007",
    )


def test_py007_import_subprocess(tmp_path) -> None:
    """import subprocess at module level triggers PY007."""
    _assert_analyze(tmp_path, "import subprocess\n", "PY007")


# ===================================================================
# PY008 -- Threading / asyncio / multiprocessing
# ===================================================================


def test_py008_import_threading(tmp_path) -> None:
    """import threading at module level triggers PY008."""
    _assert_analyze(tmp_path, "import threading\n", "PY008")


def test_py008_import_asyncio(tmp_path) -> None:
    """import asyncio at module level triggers PY008."""
    _assert_analyze(tmp_path, "import asyncio\n", "PY008")


def test_py008_import_multiprocessing(tmp_path) -> None:
    """import multiprocessing at module level triggers PY008."""
    _assert_analyze(tmp_path, "import multiprocessing\n", "PY008")


def test_py008_from_threading_import(tmp_path) -> None:
    """from threading import ... triggers PY008."""
    _assert_analyze(tmp_path, "from threading import Thread\n", "PY008")


def test_py008_from_asyncio_import(tmp_path) -> None:
    """from asyncio import ... triggers PY008."""
    _assert_analyze(tmp_path, "from asyncio import run\n", "PY008")


def test_py008_from_multiprocessing_import(tmp_path) -> None:
    """from multiprocessing import ... triggers PY008."""
    _assert_analyze(tmp_path, "from multiprocessing import Process\n", "PY008")


def test_py008_module_level_call(tmp_path) -> None:
    """threading.Thread() call at module level triggers PY008.

    The ForbiddenAPIChecker detects qualified calls at any level, but only
    inside functions that are in the durable closure or entry set.
    """
    _assert_analyze(
        tmp_path,
        """\
        def my_func(h):
            call("foo")
            threading.Thread()
    """,
        "PY008",
    )


# ===================================================================
# PY009 -- Socket
# ===================================================================


def test_py009_socket_socket(tmp_path) -> None:
    """socket.socket() inside a durable-closure function triggers PY009."""
    _assert_analyze(
        tmp_path,
        """\
        def my_func(h):
            call("foo")
            socket.socket()
    """,
        "PY009",
    )


def test_py009_socket_create_connection(tmp_path) -> None:
    """socket.create_connection() inside a durable-closure function triggers PY009."""
    _assert_analyze(
        tmp_path,
        """\
        def my_func(h):
            call("foo")
            socket.create_connection(("localhost", 80))
    """,
        "PY009",
    )


def test_py009_import_socket(tmp_path) -> None:
    """import socket at module level triggers PY009."""
    _assert_analyze(tmp_path, "import socket\n", "PY009")


def test_py009_from_socket_import(tmp_path) -> None:
    """from socket import ... triggers PY009."""
    _assert_analyze(tmp_path, "from socket import socket\n", "PY009")


# ===================================================================
# PY010 -- print()
# ===================================================================


def test_py010_print_not_allowed(tmp_path) -> None:
    """print() inside a durable-closure function triggers PY010."""
    _assert_analyze(
        tmp_path,
        """\
        def my_func(h):
            call("foo")
            print("hello")
    """,
        "PY010",
    )


def test_py010_print_module_level(tmp_path) -> None:
    """print() at module level also triggers PY010."""
    _assert_analyze(tmp_path, "print('hello')\n", "PY010")


# ===================================================================
# PY011 -- Missing HostCalls param in durable closure
# ===================================================================


def test_py011_missing_host_calls_param(tmp_path) -> None:
    """Function in durable closure without a HostCalls parameter triggers PY011."""
    _assert_analyze(
        tmp_path,
        """\
        def worker():
            call("some_func")
    """,
        "PY011",
    )


def test_py011_host_calls_param_h(tmp_path) -> None:
    """h parameter is accepted as a HostCalls name, so no PY011."""
    _assert_no_error(
        tmp_path,
        """\
        def worker(h):
            call("some_func")
    """,
        "PY011",
    )


def test_py011_host_calls_param_host_calls(tmp_path) -> None:
    """host_calls parameter is accepted as a HostCalls name, so no PY011."""
    _assert_no_error(
        tmp_path,
        """\
        def worker(host_calls):
            call("some_func")
    """,
        "PY011",
    )


def test_py011_host_calls_param_hc(tmp_path) -> None:
    """hc parameter is accepted as a HostCalls name, so no PY011."""
    _assert_no_error(
        tmp_path,
        """\
        def worker(hc):
            call("some_func")
    """,
        "PY011",
    )


def test_py011_host_calls_param_host(tmp_path) -> None:
    """host parameter is accepted as a HostCalls name, so no PY011."""
    _assert_no_error(
        tmp_path,
        """\
        def worker(host):
            call("some_func")
    """,
        "PY011",
    )


# ===================================================================
# PY012 -- async def with @cleat_entry
# ===================================================================


def test_py012_async_entry_function(tmp_path) -> None:
    """@cleat_entry on an async def triggers PY012 via analyze_file."""
    _assert_analyze(
        tmp_path,
        """\
        @cleat_entry
        async def my_func(h):
            call("foo")
    """,
        "PY012",
    )


def test_py012_async_entry_detect(tmp_path) -> None:
    """detect_entry returns an error for an async @cleat_entry function."""
    fp = _write(
        tmp_path,
        """\
        @cleat_entry
        async def my_func(h):
            pass
    """,
    )
    name, error = detect_entry(fp)
    assert name is None
    assert error is not None


# ===================================================================
# PY013 -- Multiple @cleat_entry functions
# ===================================================================


def test_py013_multiple_entries_detect(tmp_path) -> None:
    """detect_entry returns an error when multiple @cleat_entry functions exist."""
    fp = _write(
        tmp_path,
        """\
        @cleat_entry
        def func1(h):
            pass

        @cleat_entry
        def func2(h):
            pass
    """,
    )
    name, error = detect_entry(fp)
    assert name is None
    assert error is not None


# ===================================================================
# Positive / edge cases
# ===================================================================


def test_clean_file_no_errors(tmp_path) -> None:
    """A valid workflow file with proper HostCalls usage produces no errors."""
    fp = _write(
        tmp_path,
        """\
        def my_func(h):
            h.call("foo")
    """,
    )
    result = analyze_file(fp)
    assert len(result.errors) == 0


def test_detect_entry_success(tmp_path) -> None:
    """detect_entry returns the function name for a single valid @cleat_entry."""
    fp = _write(
        tmp_path,
        """\
        @cleat_entry
        def my_func(h):
            pass
    """,
    )
    name, error = detect_entry(fp)
    assert name == "my_func"
    assert error is None


def test_detect_entry_no_entry(tmp_path) -> None:
    """detect_entry returns an error when no @cleat_entry function exists."""
    fp = _write(tmp_path, "def my_func(h):\n    pass\n")
    name, error = detect_entry(fp)
    assert name is None
    assert error is not None


def test_entry_function_detected_in_analyze(tmp_path) -> None:
    """analyze_file populates entry_functions when @cleat_entry is present."""
    fp = _write(
        tmp_path,
        """\
        @cleat_entry
        def my_func(h):
            h.call("foo")
    """,
    )
    result = analyze_file(fp)
    assert "my_func" in result.entry_functions


def test_analyze_result_properties(tmp_path) -> None:
    """analyze_file populates call_graph, function_defs, and summary."""
    fp = _write(
        tmp_path,
        """\
        def inner(h):
            cleat_call("bar")

        def outer(h):
            inner(h)
    """,
    )
    result = analyze_file(fp)
    assert "inner" in result.call_graph
    assert "outer" in result.call_graph
    assert "inner" in result.function_defs
    assert "outer" in result.function_defs
    assert result.summary["functions"] == 2


def test_no_false_positive_innocent_imports(tmp_path) -> None:
    """Common harmless imports do not trigger errors."""
    fp = _write(
        tmp_path,
        """\
        import json
        import sys
        import os
        import typing
        from typing import Any, Dict
    """,
    )
    result = analyze_file(fp)
    assert len(result.errors) == 0
