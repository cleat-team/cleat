"""
Static analysis tool for Cleat Python workflows.

Uses Python's ``ast`` module for proper AST-based analysis::

  - Parse Python source files and build a call graph
  - Identify durable closure: functions that transitively reach SDK leaves
  - Detect forbidden APIs (open(), time.sleep(), requests, os, etc.)
  - Verify HostCalls threading through durable closure
  - Detect entry functions and validate their signatures

Usage::

    python -m cleat_sdk.vet my_workflow.py
    python -m cleat_sdk.vet my_workflow.py --json
    python -m cleat_sdk.vet --detect-entry my_workflow.py
"""

from __future__ import annotations

import ast
import json
import sys
from typing import Any

from ._stdlib_preload import PRELOADED_MODULES

# ---------------------------------------------------------------------------
# Constants
# ---------------------------------------------------------------------------

#: Error code categories
ERROR_FORBIDDEN_API = "PY001"  # Reserved for general "not allowed in workflow code"
ERROR_FILE_IO = "PY002"  # open() etc
ERROR_DIRECT_HTTP = "PY003"  # requests, urllib, http.client
ERROR_TIME_SLEEP = "PY004"  # time.sleep
ERROR_TIME_TIME = "PY005"  # time.time
ERROR_RANDOM = "PY006"  # random.*
ERROR_OS_OPS = "PY007"  # os.*, subprocess.*
ERROR_CONCURRENCY = "PY008"  # threading, asyncio, multiprocessing
ERROR_SOCKET = "PY009"  # socket.*
ERROR_PRINT = "PY010"  # print()
ERROR_NO_THREADING = "PY011"  # HostCalls param missing
ERROR_ASYNC_FUNC = "PY012"  # async def decorated with @cleat_entry
ERROR_MULTIPLE_ENTRIES = "PY013"  # multiple @cleat_entry functions

#: WARNING codes (never errors, so they never refuse a build). Both are about the
#: same documented limit of componentize-py (cleat#4266): the component contains
#: only the standard-library modules that were imported while it was BUILT, so a
#: module first imported at run time can be missing (ModuleNotFoundError).
WARN_FUNCTION_IMPORT = "PY014"  # stdlib module imported inside a function
WARN_LAZY_IMPORT_CALL = "PY015"  # call that imports a module the first time it runs

#: A real-time clock read or sleep in workflow code (cleat#4396). A WARNING, never an error: it must not newly refuse a build that runs today.
#: ``time.sleep`` written plainly is the PY004 error and is not reported twice; the same call through an alias is reported as PY004 too (an error), and
#: this covers the clock reads that PY004 and PY005 never named.
WARN_REAL_TIME_CLOCK = "PY016"

#: A wait that blocks the worker in code the workflow executes (cleat#4403): ``select.select`` and a ``queue.Queue.get`` that can block. A WARNING, never an
#: error. Measured 2026-11-26 (develop 4f2addca, PostgreSQL, macOS arm64): ``select.select([], [], [], t)`` blocks in the host for the whole timeout like
#: ``time.sleep`` does (0.44 CPU-s over 5 s, one --concurrency slot, one OS thread, not interrupted by --wasm-instance-timeout), and
#: ``queue.Queue().get(timeout=t)`` busy-loops in the guest (5.17 CPU-s over 5 s, one slot, stopped by --wasm-instance-timeout).
WARN_BLOCKING_WAIT = "PY017"

#: The ``time`` functions that read the host's real monotonic clock. Measured 2026-11-23 (componentize-py 0.25.1): two runs of one workflow id read
#: different values, so anything computed from them differs between a run and its replay. ``time.time`` is PY005's and ``time.sleep`` is PY004's.
MONOTONIC_CLOCK_CALLS: tuple[str, ...] = (
    "monotonic",
    "monotonic_ns",
    "perf_counter",
    "perf_counter_ns",
)

#: The ``time`` functions that read the process's CPU-time clock. Measured 2026-11-23: two runs of one workflow id read -2.0412 and -2.0397 seconds
#: (``process_time_ns``: -2041199250 and -2039663875), negative on both, so the value means nothing and differs between a run and its replay. Same
#: warning as the monotonic reads, with a message that says what these actually read.
PROCESS_TIME_CALLS: tuple[str, ...] = (
    "process_time",
    "process_time_ns",
)

#: SDK durable leaf function names --- HostCalls methods that the runtime
#: journalises (i.e. they create events in the workflow history).
DURABLE_LEAVES: set[str] = {
    # Core durable calls
    "call",
    "call_typed",
    "call_with_retry",
    "call_with_heartbeat",
    # Sleep
    "sleep",
    # Logging
    "log",
    "log_kv",
    # HTTP fetch
    "fetch",
    "fetch_json",
    "fetch_get",
    "fetch_get_json",
    # Signals
    "await_signals",
    "poll_signal",
    "poll_cancellation",
    "signal_workflow",
    "send_signal_and_wait",
    "reply_to_signal",
    "await_signals_with_quorum",
    # Child workflows
    "child_workflow",
    "child_workflow_with_options",
    "await_child",
    "await_all_children",
    # State
    "set_query_state",
    "get_query_state",
    # Promises
    "create_promise",
    "await_promise",
    "resolve_promise",
    "reject_promise",
    # Handlers
    "register_update_handler",
    # There is no register_query_handler here (removed 2026-08-09; see
    # docs/determinism.md, "Why there is no RegisterQueryHandler").
    # Lifecycle
    "defer",
    "continue_as_new",
    "extend_timeout",
    "run_detached",
    # Fire-and-forget / scheduling
    "send",
    "schedule_invoke",
    # Identity
    "current_workflow_id",
    "current_run_id",
    # Scope
    "set_scope",
    "get_scope",
    "clear_scope",
    # UUID
    "uuid",
    # Plugin
    "plugin_call",
    "plugin_call_streaming",
    # Clock / random (deterministic versions)
    "now",
    "random",
    "version",
    "min_version",
}

#: Callable expression prefixes that identify forbidden APIs.
#: Maps ``(module, name)`` to ``(error_code, message, suggestion)``.
#: When *module* is ``None``, the name refers to a builtin or unqualified call.
FORBIDDEN_API_RULES: dict[tuple[str | None, str], tuple[str, str, str]] = {
    (None, "open"): (
        "PY002",
        "File I/O is not allowed in workflow code: file system operations produce non-deterministic side effects across replays.",
        "Use fetch() or HostCalls instead",
    ),
    (None, "print"): (
        "PY010",
        "print() is not allowed in workflow code: output is lost on replay and not recorded in workflow history.",
        "Use h.log() instead",
    ),
    ("time", "sleep"): (
        "PY004",
        "time.sleep() is not allowed in durable functions: real-time sleep advances differently on each replay, breaking determinism. It also blocks its execution "
        "for its whole duration, and the instance timeout does not interrupt it (cleat#4403).",
        "Use h.sleep() instead",
    ),
    ("time", "time"): (
        "PY005",
        "time.time() is not allowed in durable functions: wall-clock time differs across replays, causing divergent behavior.",
        "Use h.now() for the replayable workflow clock instead",
    ),
    ("random", "random"): (
        "PY006",
        "random.random() is not allowed in durable functions: default random seeding depends on system time, which differs across replays.",
        "Use h.Random() instead",
    ),
    ("random", "randint"): (
        "PY006",
        "random.randint() is not allowed in durable functions: default random seeding depends on system time, which differs across replays.",
        "Use h.Random() instead",
    ),
    ("random", "uniform"): (
        "PY006",
        "random.uniform() is not allowed in durable functions: default random seeding depends on system time, which differs across replays.",
        "Use h.Random() instead",
    ),
    ("random", "choice"): (
        "PY006",
        "random.choice() is not allowed in durable functions: default random seeding depends on system time, which differs across replays.",
        "Use h.Random() instead",
    ),
    ("random", "shuffle"): (
        "PY006",
        "random.shuffle() is not allowed in durable functions: default random seeding depends on system time, which differs across replays.",
        "Use h.Random() instead",
    ),
    ("os", "open"): (
        "PY007",
        "os.open() is not allowed in workflow code: OS operations produce side effects that cannot be replayed.",
        "Use fetch() or HostCalls instead",
    ),
    ("os", "popen"): (
        "PY007",
        "os.popen() is not allowed in workflow code",
        "Subprocess execution is not permitted in workflow code",
    ),
    ("os", "system"): (
        "PY007",
        "os.system() is not allowed in workflow code",
        "Subprocess execution is not permitted in workflow code",
    ),
    ("os", "getenv"): (
        "PY007",
        "os.getenv() is not allowed in durable functions",
        "Environment may differ across replays. Pass config as input instead.",
    ),
    ("os", "environ"): (
        "PY007",
        "os.environ is not allowed in durable functions",
        "Environment may differ across replays. Pass config as input instead.",
    ),
    ("os", "listdir"): (
        "PY007",
        "os.listdir() is not allowed in workflow code",
        "File system access is not permitted in workflow code",
    ),
    ("os", "remove"): (
        "PY007",
        "os.remove() is not allowed in workflow code",
        "File system access is not permitted in workflow code",
    ),
    ("os", "rename"): (
        "PY007",
        "os.rename() is not allowed in workflow code",
        "File system access is not permitted in workflow code",
    ),
    ("os", "mkdir"): (
        "PY007",
        "os.mkdir() is not allowed in workflow code",
        "File system access is not permitted in workflow code",
    ),
    ("os", "walk"): (
        "PY007",
        "os.walk() is not allowed in workflow code",
        "File system access is not permitted in workflow code",
    ),
    ("os", "exit"): (
        "PY007",
        "os.exit() is not allowed in workflow code",
        "Use return values to signal completion instead",
    ),
    ("subprocess", "call"): (
        "PY007",
        "subprocess.call() is not allowed in workflow code",
        "Subprocess execution is not permitted in workflow code",
    ),
    ("subprocess", "run"): (
        "PY007",
        "subprocess.run() is not allowed in workflow code",
        "Subprocess execution is not permitted in workflow code",
    ),
    ("subprocess", "Popen"): (
        "PY007",
        "subprocess.Popen() is not allowed in workflow code",
        "Subprocess execution is not permitted in workflow code",
    ),
    ("threading", "Thread"): (
        "PY008",
        "threading.Thread is not allowed in workflow code",
        "Cleat workflows are single-threaded. Use child workflows for parallelism.",
    ),
    ("threading", "Lock"): (
        "PY008",
        "threading.Lock is not allowed in workflow code",
        "Cleat workflows are single-threaded. Use child workflows for parallelism.",
    ),
    ("asyncio", "run"): (
        "PY008",
        "asyncio.run() is not allowed in workflow code",
        "Use cleat's synchronous durable execution model instead",
    ),
    ("asyncio", "gather"): (
        "PY008",
        "asyncio.gather() is not allowed in workflow code",
        "Use child workflows for parallel execution",
    ),
    ("multiprocessing", "Process"): (
        "PY008",
        "multiprocessing.Process is not allowed in workflow code",
        "Use child workflows for parallel execution",
    ),
    ("socket", "socket"): (
        "PY009",
        "socket.socket() is not allowed in workflow code: raw sockets produce non-replayable network side effects.",
        "Use fetch() or call() instead.",
    ),
    ("socket", "create_connection"): (
        "PY009",
        "socket.create_connection() is not allowed in workflow code: raw sockets produce non-replayable network side effects.",
        "Use fetch() or call() instead.",
    ),
}

#: Builtins that are forbidden in workflow code
FORBIDDEN_BUILTINS: dict[str, tuple[str, str, str]] = {
    # Already covered by FORBIDDEN_API_RULES with module=None
}

#: Parameter names that are accepted as HostCalls threading indicators
HOSTCALLS_PARAM_NAMES: set[str] = {"h", "host_calls", "hc", "host"}


# ---------------------------------------------------------------------------
# Analysis result
# ---------------------------------------------------------------------------


class AnalysisResult:
    """Result of analyzing a single Python file."""

    def __init__(self, filepath: str) -> None:
        self.filepath: str = filepath
        self.errors: list[dict[str, Any]] = []
        self.warnings: list[dict[str, Any]] = []
        self.entry_functions: list[str] = []
        self.call_graph: dict[str, set[str]] = {}
        self.durable_closure: set[str] = set()
        self.durable_leaf_callers: set[str] = set()
        self.function_defs: dict[str, ast.FunctionDef] = {}

    def add_error(
        self,
        code: str,
        line: int,
        message: str,
        suggestion: str = "",
        col: int = 0,
    ) -> None:
        self.errors.append(
            {
                "code": code,
                "file": self.filepath,
                "line": line,
                "column": col,
                "message": message,
                "suggestion": suggestion,
            }
        )

    def add_warning(
        self,
        code: str,
        line: int,
        message: str,
        suggestion: str = "",
        col: int = 0,
    ) -> None:
        self.warnings.append(
            {
                "code": code,
                "file": self.filepath,
                "line": line,
                "column": col,
                "message": message,
                "suggestion": suggestion,
            }
        )

    @property
    def summary(self) -> dict[str, Any]:
        return {
            "functions": len(self.function_defs),
            "durable_leaves": len(self.durable_leaf_callers),
            "durable_closure": len(self.durable_closure),
            "pure": len(self.function_defs) - len(self.durable_closure),
        }


# ---------------------------------------------------------------------------
# AST analysis
# ---------------------------------------------------------------------------


class CallGraphBuilder(ast.NodeVisitor):
    """Builds a call graph from an AST module.

    For each function definition at module level, records all calls made
    within its body (including nested function definitions within the
    function, though only the top-level function is the caller node).
    """

    def __init__(self) -> None:
        # caller_name -> set of callee names (user-defined functions only)
        self.graph: dict[str, set[str]] = {}
        # caller_name -> set of callee attribute names (e.g., "call", "sleep")
        self.leaf_calls: dict[str, set[str]] = {}
        # Module-level function defs
        self.function_defs: dict[str, ast.FunctionDef] = {}
        # Current function being analyzed
        self._current_func: str | None = None

    def _process_function(self, node: ast.FunctionDef) -> None:
        """Record module-level function definitions and their calls."""
        name = node.name
        # Only track module-level functions (nesting depth 0 in the class means
        # we're at module level). Track the function definition.
        if self._current_func is None:
            self.function_defs[name] = node
            self.graph.setdefault(name, set())
            self.leaf_calls.setdefault(name, set())

            # Walk the body to collect calls
            self._current_func = name
            self.generic_visit(node)
            self._current_func = None
        else:
            # Nested function: walk its body but attribute calls to the parent
            # function, not the nested one. We don't track nested functions
            # as separate call graph nodes.
            self.generic_visit(node)

    def visit_FunctionDef(self, node: ast.FunctionDef) -> None:
        self._process_function(node)

    def visit_AsyncFunctionDef(self, node: ast.AsyncFunctionDef) -> None:
        self._process_function(node)

    def visit_Call(self, node: ast.Call) -> None:
        """Record calls made inside the current function."""
        if self._current_func is None:
            return

        func = node.func
        if isinstance(func, ast.Name):
            callee = func.id
            # Check if it's a known user function
            if callee in self.function_defs or callee in self.graph:
                self.graph[self._current_func].add(callee)
            # Check if it's a durable leaf (standalone function like call)
            elif callee in DURABLE_LEAVES:
                self.leaf_calls[self._current_func].add(callee)
            # Check forbidden builtins
            elif callee in FORBIDDEN_BUILTINS:
                self.leaf_calls[self._current_func].add(f"__forbidden__.{callee}")

        elif isinstance(func, ast.Attribute):
            # Attribute call like obj.method() or module.func()
            attr_name = func.attr
            # Check if the attribute is a durable leaf
            if attr_name in DURABLE_LEAVES:
                self.leaf_calls[self._current_func].add(attr_name)

            # Check module-qualified calls like time.sleep(), os.open(), etc.
            if isinstance(func.value, ast.Name):
                module_name = func.value.id
                key = (module_name, attr_name)
                if key in FORBIDDEN_API_RULES:
                    self.leaf_calls[self._current_func].add(
                        f"__forbidden__.{module_name}.{attr_name}"
                    )

        # Continue walking arguments and nested calls
        self.generic_visit(node)


class ForbiddenAPIChecker(ast.NodeVisitor):
    """Checks for forbidden API usage in a set of functions.

    Visits function bodies and reports forbidden calls found using
    ``FORBIDDEN_API_RULES``.
    """

    def __init__(self, filepath: str, target_funcs: set[str]) -> None:
        self.filepath = filepath
        self.target_funcs = target_funcs
        self.errors: list[dict[str, Any]] = []
        self._current_func: str | None = None

    def _check_function(self, node: ast.FunctionDef) -> None:
        """Check calls in a function if it is in the target set."""
        if node.name in self.target_funcs:
            old = self._current_func
            self._current_func = node.name
            self.generic_visit(node)
            self._current_func = old

    def visit_FunctionDef(self, node: ast.FunctionDef) -> None:
        self._check_function(node)

    def visit_AsyncFunctionDef(self, node: ast.AsyncFunctionDef) -> None:
        self._check_function(node)

    def visit_Call(self, node: ast.Call) -> None:
        """Check each call against forbidden API rules."""
        func = node.func

        # Builtin/unqualified calls: open(), print(), eval(), exec()
        if isinstance(func, ast.Name):
            name = func.id
            key = (None, name)
            if key in FORBIDDEN_API_RULES:
                code, msg, suggestion = FORBIDDEN_API_RULES[key]
                self.errors.append(
                    {
                        "code": code,
                        "file": self.filepath,
                        "line": node.lineno,
                        "column": getattr(node, "col_offset", 0),
                        "message": msg,
                        "suggestion": suggestion,
                    }
                )
                return

        # Qualified calls: module.func() or obj.method()
        if isinstance(func, ast.Attribute):
            attr = func.attr
            if isinstance(func.value, ast.Name):
                module = func.value.id
                key = (module, attr)
                if key in FORBIDDEN_API_RULES:
                    code, msg, suggestion = FORBIDDEN_API_RULES[key]
                    self.errors.append(
                        {
                            "code": code,
                            "file": self.filepath,
                            "line": node.lineno,
                            "column": getattr(node, "col_offset", 0),
                            "message": msg,
                            "suggestion": suggestion,
                        }
                    )
                    return

        # Walk arguments and sub-calls
        self.generic_visit(node)

    def visit_Subscript(self, node: ast.Subscript) -> None:
        """Check for os.environ subscript access (os.environ["VAR"])."""
        if isinstance(node.value, ast.Attribute):
            attr = node.value
            if isinstance(attr.value, ast.Name):
                module = attr.value.id
                if module == "os" and attr.attr == "environ":
                    self.errors.append(
                        {
                            "code": "PY007",
                            "file": self.filepath,
                            "line": node.lineno,
                            "column": getattr(node, "col_offset", 0),
                            "message": "os.environ access is not allowed in durable functions",
                            "suggestion": (
                                "Environment may differ across replays. "
                                "Pass config as input instead."
                            ),
                        }
                    )
        self.generic_visit(node)

    def visit_Import(self, node: ast.Import) -> None:
        """Flag imports of forbidden modules."""
        for alias in node.names:
            name = alias.name
            if name == "requests" or name.startswith("requests."):
                self.errors.append(
                    {
                        "code": "PY003",
                        "file": self.filepath,
                        "line": node.lineno,
                        "column": getattr(node, "col_offset", 0),
                        "message": "requests library is not allowed in workflow code: direct HTTP calls produce non-replayable network effects.",
                        "suggestion": "Use fetch() or call() instead",
                    }
                )
            elif name == "urllib" or name.startswith("urllib."):
                self.errors.append(
                    {
                        "code": "PY003",
                        "file": self.filepath,
                        "line": node.lineno,
                        "column": getattr(node, "col_offset", 0),
                        "message": "urllib is not allowed in workflow code: direct HTTP calls produce non-replayable network effects.",
                        "suggestion": "Use fetch() or call() instead",
                    }
                )
            elif name == "http" or name.startswith("http."):
                self.errors.append(
                    {
                        "code": "PY003",
                        "file": self.filepath,
                        "line": node.lineno,
                        "column": getattr(node, "col_offset", 0),
                        "message": "http.client is not allowed in workflow code: direct HTTP calls produce non-replayable network effects.",
                        "suggestion": "Use fetch() or call() instead",
                    }
                )
            elif name == "socket" or name.startswith("socket."):
                self.errors.append(
                    {
                        "code": "PY009",
                        "file": self.filepath,
                        "line": node.lineno,
                        "column": getattr(node, "col_offset", 0),
                        "message": "socket module is not allowed in workflow code: raw sockets produce non-replayable network side effects.",
                        "suggestion": "Use fetch() or call() instead",
                    }
                )
            elif name in ("threading", "asyncio", "multiprocessing"):
                code = "PY008"
                msg = f"{name} module is not allowed in workflow code"
                suggestion = (
                    "Cleat workflows are single-threaded. Use child workflows for parallelism."
                )
                self.errors.append(
                    {
                        "code": code,
                        "file": self.filepath,
                        "line": node.lineno,
                        "column": getattr(node, "col_offset", 0),
                        "message": msg,
                        "suggestion": suggestion,
                    }
                )
            elif name == "subprocess" or name.startswith("subprocess."):
                self.errors.append(
                    {
                        "code": "PY007",
                        "file": self.filepath,
                        "line": node.lineno,
                        "column": getattr(node, "col_offset", 0),
                        "message": "subprocess module is not allowed in workflow code",
                        "suggestion": "Subprocess execution is not permitted in workflow code",
                    }
                )

    def visit_ImportFrom(self, node: ast.ImportFrom) -> None:
        """Flag imports from forbidden modules."""
        module = node.module or ""
        if module == "requests":
            self.errors.append(
                {
                    "code": "PY003",
                    "file": self.filepath,
                    "line": node.lineno,
                    "column": getattr(node, "col_offset", 0),
                    "message": "requests library is not allowed in workflow code: direct HTTP calls produce non-replayable network effects.",
                    "suggestion": "Use fetch() or call() instead",
                }
            )
        elif module == "urllib" or module.startswith("urllib."):
            self.errors.append(
                {
                    "code": "PY003",
                    "file": self.filepath,
                    "line": node.lineno,
                    "column": getattr(node, "col_offset", 0),
                    "message": "urllib is not allowed in workflow code: direct HTTP calls produce non-replayable network effects.",
                    "suggestion": "Use fetch() or call() instead",
                }
            )
        elif module == "socket":
            self.errors.append(
                {
                    "code": "PY009",
                    "file": self.filepath,
                    "line": node.lineno,
                    "column": getattr(node, "col_offset", 0),
                    "message": "socket module is not allowed in workflow code: raw sockets produce non-replayable network side effects.",
                    "suggestion": "Use fetch() or call() instead",
                }
            )
        elif module in ("threading", "asyncio", "multiprocessing"):
            code = "PY008"
            msg = f"from {module} import ... is not allowed in workflow code"
            suggestion = "Cleat workflows are single-threaded. Use child workflows for parallelism."
            self.errors.append(
                {
                    "code": code,
                    "file": self.filepath,
                    "line": node.lineno,
                    "column": getattr(node, "col_offset", 0),
                    "message": msg,
                    "suggestion": suggestion,
                }
            )


#: Standard-library modules compiled INTO the component's interpreter. They are
#: present whenever the component runs, so importing one inside a function is
#: harmless and PY014 stays quiet about it. Measured 2026-11-22 as the
#: non-underscore names in the guest's ``sys.builtin_module_names`` (CPython
#: 3.14.0 for wasi, componentize-py 0.25.1), each also imported at run time in a
#: minimal guest. It describes one interpreter build and can change with
#: componentize-py; the cost of it going stale is a missed warning, not a wrong
#: one, because every other stdlib import still warns.
INTERPRETER_BUILTIN_MODULES: frozenset[str] = frozenset(
    {
        "__future__",
        "array",
        "atexit",
        "binascii",
        "builtins",
        "cmath",
        "errno",
        "faulthandler",
        "gc",
        "itertools",
        "marshal",
        "math",
        "posix",
        "pyexpat",
        "select",
        "sys",
        "time",
        "unicodedata",
        "zlib",
    }
)

#: THE PY015 TABLES BELOW NOW WARN ABOUT NOTHING that they list, because ``cleat_sdk._stdlib_preload`` imports every module they name and every
#: workflow imports the SDK (cleat#4266). They stay as the record of what was measured, and the machinery stays for the next row: a call whose
#: module is NOT preloaded still warns, so a row added here without adding the module to ``PRELOADED_MODULES`` is a live warning again.
#:
#: PY015 table, calls that import a module on first use, by METHOD NAME. Each row
#: was measured (2026-11-22, same guest as above): in a guest that does not
#: import the module at top level the call raises ModuleNotFoundError, and a
#: guest whose ONLY extra import is the one named here runs it correctly.
#: A row is a measured remedy, not a guess; add a row only after measuring it,
#: and add its test. Name: (modules to import at top level, what the call does).
LAZY_IMPORT_METHODS: dict[str, tuple[tuple[str, ...], str]] = {
    "strptime": (
        ("_strptime",),
        "datetime.strptime() and time.strptime() import _strptime the first time they run",
    ),
    "most_common": (
        ("heapq",),
        "Counter.most_common() imports heapq the first time it runs",
    ),
}

#: PY015 table, codecs that load a codec module on first use, keyed by the codec's
#: MODULE name (what ``encodings.aliases`` resolves any spelling to). Codecs not
#: listed are NOT checked: they are probably the same, but advice that was never
#: measured is how ``encodings.idna`` alone nearly shipped as the remedy for
#: "idna" (it also needs ``encodings.punycode``). utf-8, ascii, latin-1, utf-16
#: and utf-32 need nothing (measured).
LAZY_IMPORT_CODECS: dict[str, tuple[str, ...]] = {
    "cp1252": ("encodings.cp1252",),
    "shift_jis": ("encodings.shift_jis",),
    "iso8859_15": ("encodings.iso8859_15",),
    "utf_8_sig": ("encodings.utf_8_sig",),
    "idna": ("encodings.idna", "encodings.punycode"),
}


def _module_level_imports(tree: ast.AST) -> set[str]:
    """Every dotted module name imported while the module itself runs.

    That is, at build time: statements at module level, in module-level ``if``/
    ``try``/``with`` blocks and in class bodies, but NOT inside a function body,
    which only runs when the workflow does. ``import a.b`` counts for ``a`` and
    ``a.b``; ``from a import b`` counts for ``a`` and ``a.b``.
    """
    found: set[str] = set()

    def add_path(dotted: str) -> None:
        parts = dotted.split(".")
        for i in range(1, len(parts) + 1):
            found.add(".".join(parts[:i]))

    def visit(child: ast.AST) -> None:
        if isinstance(child, (ast.FunctionDef, ast.AsyncFunctionDef, ast.Lambda)):
            return
        if isinstance(child, ast.If):
            # ``if TYPE_CHECKING:`` is False when the module runs, so its body is never
            # executed at build time (only the ``else`` is), and ``if not TYPE_CHECKING:``
            # is the reverse. Without this a type-only import silenced the warning.
            kind = _type_checking_test(child.test)
            if kind is not None:
                for stmt in child.orelse if kind else child.body:
                    visit(stmt)
                return
        if isinstance(child, ast.Import):
            for alias in child.names:
                add_path(alias.name)
        elif isinstance(child, ast.ImportFrom) and child.level == 0 and child.module:
            add_path(child.module)
            for alias in child.names:
                found.add(f"{child.module}.{alias.name}")
        walk(child)

    def walk(node: ast.AST) -> None:
        for child in ast.iter_child_nodes(node):
            visit(child)

    walk(tree)
    return found


def _type_checking_test(test: ast.expr) -> bool | None:
    """``True`` for ``TYPE_CHECKING`` (``typing.TYPE_CHECKING``), ``False`` for ``not`` it, else ``None``."""
    if isinstance(test, ast.UnaryOp) and isinstance(test.op, ast.Not):
        inner = _type_checking_test(test.operand)
        return None if inner is None else not inner
    if isinstance(test, ast.Name) and test.id == "TYPE_CHECKING":
        return True
    if isinstance(test, ast.Attribute) and test.attr == "TYPE_CHECKING":
        return True
    return None


def _codec_module(name: str) -> str:
    """The ``encodings`` module a codec spelling resolves to (``sjis`` -> ``shift_jis``)."""
    import encodings
    import encodings.aliases

    normalised = encodings.normalize_encoding(name).lower()
    return encodings.aliases.aliases.get(normalised, normalised)


class RealTimeClockChecker(ast.NodeVisitor):
    """Warns about a real-time clock read or sleep in the code a workflow executes (cleat#4396).

    The calls are ``MONOTONIC_CLOCK_CALLS`` and ``PROCESS_TIME_CALLS`` (``process_time`` reads a CPU-time clock that returned negative values and
    differed between two runs), plus ``time.sleep`` through an alias.

    The host's monotonic clock is real elapsed time and replay does not reproduce it: ``wasi:clocks/monotonic-clock`` is not shadowed, because a
    shadow replaces the whole interface and its ``subscribe-*`` functions return pollables the host cannot supply. So ``time.monotonic()``,
    ``monotonic_ns``, ``perf_counter`` and ``perf_counter_ns`` return a different value on a replay, and so does anything computed from one (an
    elapsed time, a loop that runs until a deadline). The owner decided to keep the clock real and say so, which is what this is.

    One warning code, PY016, over the same scope as the forbidden-API checker (the functions the entry points reach). A reference counts as well as
    a call, because ``clock = time.perf_counter`` reads the clock later. Names are resolved through the file's own imports (``import time as t``,
    ``from time import monotonic as m``); a bare ``time`` is taken to be the module as the existing rules do. A ``time.sleep`` written plainly is the
    PY004 error and is skipped here; the same call through an alias is reported as the same PY004 ERROR (cleat#4403): it blocks the same way, and an
    alias must not be the way round the rule.
    """

    def __init__(self, filepath: str, tree: ast.AST, target_funcs: set[str]) -> None:
        self.filepath = filepath
        self.target_funcs = target_funcs
        self.warnings: list[dict[str, Any]] = []
        self.errors: list[dict[str, Any]] = []  # a sleep through an alias is the PY004 error, as the plain spelling is
        self._depth = 0  # >0 while inside a function whose code the workflow executes
        # Names that mean the ``time`` module, and local names bound to one of its functions (``from time import x as y``).
        self._modules: set[str] = {"time"}
        self._functions: dict[str, str] = {}
        interesting = set(MONOTONIC_CLOCK_CALLS) | set(PROCESS_TIME_CALLS) | {"sleep"}
        for node in ast.walk(tree):
            if isinstance(node, ast.Import):
                for alias in node.names:
                    if alias.name == "time" and alias.asname:
                        self._modules.add(alias.asname)
            elif isinstance(node, ast.ImportFrom) and node.module == "time" and node.level == 0:
                for alias in node.names:
                    if alias.name in interesting:
                        self._functions[alias.asname or alias.name] = alias.name
        # A module-level REFERENCE (``clock = time.perf_counter``) reads the clock when it is CALLED, which can be in the entry, at run time. A
        # module-level CALL (``T0 = time.monotonic()``) is a build-time constant: componentize-py runs module code while it builds, so both runs of a
        # workflow read the same value (measured in review, cleat#4428). So the reference is recorded here as an alias, and the call is not flagged.
        for stmt in getattr(tree, "body", []):
            value = stmt.value if isinstance(stmt, (ast.Assign, ast.AnnAssign)) else None
            if not (isinstance(value, ast.Attribute) and isinstance(value.value, ast.Name)):
                continue
            if value.value.id not in self._modules or value.attr not in interesting:
                continue
            targets = stmt.targets if isinstance(stmt, ast.Assign) else [stmt.target]
            for target in targets:
                if isinstance(target, ast.Name):
                    self._functions[target.id] = value.attr

    def _warn(self, node: ast.AST, function: str) -> None:
        if function == "sleep":
            # The same call as the plain spelling, so the same code and the same severity: an alias must not be the way round PY004.
            self.errors.append(
                {
                    "code": ERROR_TIME_SLEEP,
                    "file": self.filepath,
                    "line": getattr(node, "lineno", 0),
                    "column": getattr(node, "col_offset", 0),
                    "message": (
                        "time.sleep() is not allowed in durable functions, written through an alias as well: real-time sleep advances differently on "
                        "each replay, breaking determinism. It also blocks its execution for its whole duration, holds a --concurrency slot and one OS "
                        "thread, and the instance timeout does not interrupt it (cleat#4403)."
                    ),
                    "suggestion": "Use h.sleep() instead",
                }
            )
            return
        elif function in PROCESS_TIME_CALLS:
            message = (
                f"time.{function}() reads the host's process-time clock, which replay does not reproduce: its value differs between a run and its replay "
                "(measured: two runs of one workflow id read -2.0412 and -2.0397 seconds, negative on both, so it does not even measure CPU time here) (cleat#4396)."
            )
            suggestion = (
                "Use h.now() for a replayable clock (milliseconds since the epoch; subtract two readings for an elapsed time) "
                "and h.sleep(seconds) for a durable sleep"
            )
        else:
            message = (
                f"time.{function}() reads the host's real monotonic clock, which replay does not reproduce: its value, and anything computed from it "
                "(an elapsed time, a loop that runs until a deadline), differs between a run and its replay (cleat#4396)."
            )
            suggestion = (
                "Use h.now() for a replayable clock (milliseconds since the epoch; subtract two readings for an elapsed time) "
                "and h.sleep(seconds) for a durable sleep"
            )
        self.warnings.append(
            {
                "code": WARN_REAL_TIME_CLOCK,
                "file": self.filepath,
                "line": getattr(node, "lineno", 0),
                "column": getattr(node, "col_offset", 0),
                "message": message,
                "suggestion": suggestion,
            }
        )

    def _check_function(self, node: ast.FunctionDef | ast.AsyncFunctionDef) -> None:
        if node.name in self.target_funcs:
            self._depth += 1
            self.generic_visit(node)
            self._depth -= 1

    def visit_FunctionDef(self, node: ast.FunctionDef) -> None:
        self._check_function(node)

    def visit_AsyncFunctionDef(self, node: ast.AsyncFunctionDef) -> None:
        self._check_function(node)

    def visit_Attribute(self, node: ast.Attribute) -> None:
        if self._depth and isinstance(node.value, ast.Name) and node.value.id in self._modules:
            if node.attr in MONOTONIC_CLOCK_CALLS or node.attr in PROCESS_TIME_CALLS:
                self._warn(node, node.attr)
            elif node.attr == "sleep" and node.value.id != "time":
                self._warn(node, "sleep")
        self.generic_visit(node)

    def visit_Name(self, node: ast.Name) -> None:
        if self._depth and isinstance(node.ctx, ast.Load) and node.id in self._functions:
            self._warn(node, self._functions[node.id])


_QUEUE_CLASSES = {"Queue", "LifoQueue", "PriorityQueue", "SimpleQueue"}


class BlockingWaitChecker(ast.NodeVisitor):
    """Warns about a wait that blocks the worker: ``select.select`` and a ``queue.Queue.get`` that can block (cleat#4403).

    A workflow is single-threaded and nothing else can wake it or fill a queue, so the only thing either call can do is wait for its timeout, in a way
    that is neither durable nor replayable. ``select.select(...)`` blocks in the host for the whole timeout, as ``time.sleep`` does: a
    ``--concurrency`` slot and one OS thread for the duration, and ``--wasm-instance-timeout`` does not interrupt it. ``Queue.get`` with a timeout (or
    none) busy-loops in the guest, which burns a CPU core and holds a slot until the instance timeout stops it. Both measured, cleat#4403.

    One warning code, PY017, over the scope RealTimeClockChecker uses (the functions the entry points reach). Names are resolved through the file's own
    imports (``import select as s``, ``from select import select as sel``, ``from queue import Queue as Q``), and a queue is a name bound with
    ``q = queue.Queue()`` (or one of the other queue classes) anywhere in the file, or the constructor call itself. That is a name-level reading, not a type
    inference: an object that comes from a parameter, or is held as an attribute (``self.q``), is not recognised, and a name rebound to something else is still
    read as a queue. ``get(False)``, ``get(block=False)``, ``get(timeout=0)`` and ``get_nowait()`` do not block and are not reported. ``select.poll``,
    ``select.epoll`` and ``selectors`` (``DefaultSelector().select()``) are not looked at.
    """

    def __init__(self, filepath: str, tree: ast.AST, target_funcs: set[str]) -> None:
        self.filepath = filepath
        self.target_funcs = target_funcs
        self.warnings: list[dict[str, Any]] = []
        self._depth = 0
        self._select_modules: set[str] = {"select"}
        self._queue_modules: set[str] = {"queue"}
        self._select_functions: set[str] = set()  # local names that mean select.select
        self._queue_classes: set[str] = set()  # local names that mean queue.Queue and its siblings
        self._queues: set[str] = set()  # names bound to a queue instance
        for node in ast.walk(tree):
            if isinstance(node, ast.Import):
                for alias in node.names:
                    if alias.name == "select" and alias.asname:
                        self._select_modules.add(alias.asname)
                    elif alias.name == "queue" and alias.asname:
                        self._queue_modules.add(alias.asname)
            elif isinstance(node, ast.ImportFrom) and node.level == 0:
                for alias in node.names:
                    if node.module == "select" and alias.name == "select":
                        self._select_functions.add(alias.asname or alias.name)
                    elif node.module == "queue" and alias.name in _QUEUE_CLASSES:
                        self._queue_classes.add(alias.asname or alias.name)
        for node in ast.walk(tree):
            value = node.value if isinstance(node, (ast.Assign, ast.AnnAssign)) else None
            if isinstance(value, ast.Call) and self._is_queue_constructor(value):
                targets = node.targets if isinstance(node, ast.Assign) else [node.target]
                for target in targets:
                    if isinstance(target, ast.Name):
                        self._queues.add(target.id)

    def _is_queue_constructor(self, call: ast.Call) -> bool:
        func = call.func
        if isinstance(func, ast.Name):
            return func.id in self._queue_classes
        return (
            isinstance(func, ast.Attribute)
            and func.attr in _QUEUE_CLASSES
            and isinstance(func.value, ast.Name)
            and func.value.id in self._queue_modules
        )

    def _is_select(self, call: ast.Call) -> bool:
        func = call.func
        if isinstance(func, ast.Name):
            return func.id in self._select_functions
        return (
            isinstance(func, ast.Attribute)
            and func.attr == "select"
            and isinstance(func.value, ast.Name)
            and func.value.id in self._select_modules
        )

    def _is_blocking_get(self, call: ast.Call) -> bool:
        func = call.func
        if not (isinstance(func, ast.Attribute) and func.attr == "get"):
            return False
        receiver = func.value
        is_queue = (isinstance(receiver, ast.Name) and receiver.id in self._queues) or (
            isinstance(receiver, ast.Call) and self._is_queue_constructor(receiver)
        )
        if not is_queue:
            return False
        # get(False), get(block=False), get(0) and get(timeout=0) do not wait: a falsy constant block, or a zero timeout, raises Empty at once.
        if call.args and isinstance(call.args[0], ast.Constant) and not call.args[0].value:
            return False
        for keyword in call.keywords:
            if isinstance(keyword.value, ast.Constant) and keyword.arg == "block" and not keyword.value.value:
                return False
            if isinstance(keyword.value, ast.Constant) and keyword.arg == "timeout" and keyword.value.value == 0:
                return False
        return not (len(call.args) > 1 and isinstance(call.args[1], ast.Constant) and call.args[1].value == 0)

    def _warn(self, node: ast.AST, what: str) -> None:
        if what == "select":
            message = (
                "select.select() waits in the host: it blocks its execution for its whole timeout, holds a --concurrency slot and one OS thread, "
                "and the instance timeout does not interrupt it (measured, cleat#4403). A workflow is single-threaded, so nothing can make a "
                "descriptor ready; the call can only wait, and it is not durable."
            )
        else:
            message = (
                "queue.Queue.get() that can block waits for an item nothing can put (a workflow is single-threaded): it busy-loops, burning a CPU "
                "core and holding a --concurrency slot until the timeout or the instance timeout ends it (measured: 5.17 CPU-seconds over 5 s, "
                "cleat#4403). It is not durable."
            )
        self.warnings.append(
            {
                "code": WARN_BLOCKING_WAIT,
                "file": self.filepath,
                "line": getattr(node, "lineno", 0),
                "column": getattr(node, "col_offset", 0),
                "message": message,
                "suggestion": "Use h.sleep(seconds) to wait durably; use get_nowait() or get(block=False) for a queue that is only read",
            }
        )

    def _check_function(self, node: ast.FunctionDef | ast.AsyncFunctionDef) -> None:
        if node.name in self.target_funcs:
            self._depth += 1
            self.generic_visit(node)
            self._depth -= 1

    def visit_FunctionDef(self, node: ast.FunctionDef) -> None:
        self._check_function(node)

    def visit_AsyncFunctionDef(self, node: ast.AsyncFunctionDef) -> None:
        self._check_function(node)

    def visit_Call(self, node: ast.Call) -> None:
        if self._depth:
            if self._is_select(node):
                self._warn(node, "select")
            elif self._is_blocking_get(node):
                self._warn(node, "queue")
        self.generic_visit(node)


class RuntimeImportChecker(ast.NodeVisitor):
    """Warns about imports that happen only when the workflow RUNS (cleat#4266).

    componentize-py keeps only the standard-library modules that were imported
    while the component was being built, so anything first imported inside a
    function can raise ModuleNotFoundError at run time, in production and not
    at build. Two warnings, both about the file as written:

    - PY014: a standard-library ``import`` inside a function.
    - PY015: a call that is known to import a module on first use, when that
      module is not imported at module level (``LAZY_IMPORT_METHODS`` and
      ``LAZY_IMPORT_CODECS``).

    These are warnings and not errors on purpose. An in-function ``import json``
    works today because json is already in the component, so refusing the build
    would break workflows that run; the warning says "may be missing", which is
    the strongest claim the file alone supports.
    """

    def __init__(self, filepath: str, tree: ast.AST) -> None:
        self.filepath = filepath
        self.warnings: list[dict[str, Any]] = []
        self._at_module_level = _module_level_imports(tree)
        self._functions: list[str] = []
        # Python 3.10+, which is all the SDK imports on (requires-python is >=3.10).
        self._stdlib: frozenset[str] = sys.stdlib_module_names

    def _warn(self, code: str, node: ast.AST, message: str, suggestion: str) -> None:
        self.warnings.append(
            {
                "code": code,
                "file": self.filepath,
                "line": getattr(node, "lineno", 0),
                "column": getattr(node, "col_offset", 0),
                "message": message,
                "suggestion": suggestion,
            }
        )

    def visit_FunctionDef(self, node: ast.FunctionDef) -> None:
        self._functions.append(node.name)
        self.generic_visit(node)
        self._functions.pop()

    def visit_AsyncFunctionDef(self, node: ast.AsyncFunctionDef) -> None:
        self.visit_FunctionDef(node)  # type: ignore[arg-type]

    def _check_function_import(self, node: ast.AST, dotted: str, shown: str) -> None:
        if not self._functions:
            return
        top = dotted.split(".")[0]
        if top not in self._stdlib or top in INTERPRETER_BUILTIN_MODULES:
            return
        if dotted in self._at_module_level:
            return
        self._warn(
            WARN_FUNCTION_IMPORT,
            node,
            f"`{shown}` inside function `{self._functions[-1]}()`: the component only contains "
            "the standard-library modules that were imported when it was built, so a module "
            "first imported at run time may be missing (ModuleNotFoundError).",
            f"Move `{shown}` to the top level of the workflow module.",
        )

    def visit_Import(self, node: ast.Import) -> None:
        for alias in node.names:
            self._check_function_import(node, alias.name, f"import {alias.name}")

    def visit_ImportFrom(self, node: ast.ImportFrom) -> None:
        if node.level == 0 and node.module:
            self._check_function_import(node, node.module, f"from {node.module} import ...")

    def _missing(self, modules: tuple[str, ...]) -> list[str]:
        # A module the SDK preloads is in every component (cleat#4266), so a workflow that does not import it is fine.
        return [m for m in modules if m not in self._at_module_level and m not in PRELOADED_MODULES]

    def visit_Call(self, node: ast.Call) -> None:
        if self._functions:
            func = node.func
            name = func.attr if isinstance(func, ast.Attribute) else getattr(func, "id", None)
            if name in LAZY_IMPORT_METHODS:
                modules, why = LAZY_IMPORT_METHODS[name]
                missing = self._missing(modules)
                if missing:
                    self._warn(
                        WARN_LAZY_IMPORT_CALL,
                        node,
                        f"`{name}()`: {why}, and `{', '.join(missing)}` is only in the "
                        "component if it was imported when the component was built.",
                        "Add "
                        + " and ".join(f"`import {m}`" for m in missing)
                        + " at the top level of the workflow module.",
                    )
            elif isinstance(func, ast.Attribute) and func.attr in ("encode", "decode"):
                codec = None
                if (
                    node.args
                    and isinstance(node.args[0], ast.Constant)
                    and isinstance(node.args[0].value, str)
                ):
                    codec = node.args[0].value
                else:
                    for kw in node.keywords:
                        if (
                            kw.arg == "encoding"
                            and isinstance(kw.value, ast.Constant)
                            and isinstance(kw.value.value, str)
                        ):
                            codec = kw.value.value
                if codec is not None:
                    modules = LAZY_IMPORT_CODECS.get(_codec_module(codec))
                    missing = self._missing(modules) if modules else []
                    if missing:
                        self._warn(
                            WARN_LAZY_IMPORT_CALL,
                            node,
                            f"`.{func.attr}({codec!r})` loads its codec module the first time it is used, "
                            f"and `{', '.join(missing)}` is only in the component if it was imported "
                            "when the component was built.",
                            "Add "
                            + " and ".join(f"`import {m}`" for m in missing)
                            + " at the top level of the workflow module.",
                        )
        self.generic_visit(node)


class ThreadingChecker(ast.NodeVisitor):
    """Verifies HostCalls threading through the durable closure.

    Checks that every function in the durable closure either:
    - Has a parameter named ``h``, ``host_calls``, ``hc``, or ``host``, OR
    - Is called by a function that does (transitive threading).
    """

    def __init__(
        self, filepath: str, func_defs: dict[str, ast.FunctionDef], closure_funcs: set[str]
    ) -> None:
        self.filepath = filepath
        self.func_defs = func_defs
        self.closure_funcs = closure_funcs
        self.errors: list[dict[str, Any]] = []

    def _has_host_calls_param(self, func_node: ast.FunctionDef) -> bool:
        """Check if a function has a HostCalls-like parameter."""
        return any(arg.arg in HOSTCALLS_PARAM_NAMES for arg in func_node.args.args)

    def check(self) -> list[dict[str, Any]]:
        """Run the threading check and return errors."""
        for func_name in sorted(self.closure_funcs):
            if func_name in self.func_defs:
                func_node = self.func_defs[func_name]
                if not self._has_host_calls_param(func_node):
                    self.errors.append(
                        {
                            "code": "PY011",
                            "file": self.filepath,
                            "line": func_node.lineno,
                            "column": getattr(func_node, "col_offset", 0),
                            "message": (
                                f"'{func_name}' is in the durable closure but "
                                f"does not have a HostCalls parameter"
                            ),
                            "suggestion": (
                                "Add 'h' as a parameter. Functions calling SDK "
                                "methods need a HostCalls instance."
                            ),
                        }
                    )
        return self.errors


# ---------------------------------------------------------------------------
# Main analysis
# ---------------------------------------------------------------------------


def decorate_name(decorator: ast.expr) -> str | None:
    """Extract the decorator name from a decorator node.

    Handles ``@cleat_entry``, ``@cleat_entry("name")``, ``@cleat_entry()``,
    and ``@module.decorator``.
    """
    if isinstance(decorator, ast.Name):
        return decorator.id
    if isinstance(decorator, ast.Attribute):
        return decorator.attr
    if isinstance(decorator, ast.Call):
        if isinstance(decorator.func, ast.Name):
            return decorator.func.id
        if isinstance(decorator.func, ast.Attribute):
            return decorator.func.attr
    return None


def _is_function_def(node: ast.AST) -> bool:
    """Check if *node* is a function definition (sync or async).

    ``AsyncFunctionDef`` is a separate class from ``FunctionDef`` in
    Python 3.13+ (they are no longer in an inheritance relationship).
    """
    return isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef))


def find_cleat_entry_functions(
    tree: ast.Module,
    filepath: str,
) -> list[tuple[str, Any]]:
    """Find all ``@cleat_entry`` decorated functions in the AST."""
    results: list[tuple[str, ast.FunctionDef]] = []
    for node in ast.iter_child_nodes(tree):
        if _is_function_def(node):
            for dec in node.decorator_list:
                dname = decorate_name(dec)
                if dname == "cleat_entry":
                    results.append((node.name, node))
                    break
    return results


def build_call_graph_and_leaf_callers(
    tree: ast.Module,
) -> tuple[dict[str, set[str]], dict[str, set[str]], dict[str, ast.FunctionDef]]:
    """Build call graph and identify leaf callers from AST."""
    builder = CallGraphBuilder()
    builder.visit(tree)
    return builder.graph, builder.leaf_calls, builder.function_defs


def compute_reachable(
    call_graph: dict[str, set[str]],
    roots: set[str],
) -> set[str]:
    """Functions reachable FORWARD from roots: everything the roots can call.

    This is the scope the determinism checks need. A workflow's non-determinism
    is whatever it can reach, so the question is "what does the entry call?",
    not "who calls into this?".

    Answering it the other way round is cleat#1813: a mock-driven test harness
    that calls a workflow once became "workflow code", and 22 print() calls in
    examples/python-langchain's run_test() were reported as PY010. The harness
    is a caller, never a callee, so a forward walk cannot reach it.
    """
    reachable: set[str] = set()
    queue = list(roots)
    visited: set[str] = set()

    while queue:
        func = queue.pop(0)
        if func in visited:
            continue
        visited.add(func)
        reachable.add(func)
        queue.extend(c for c in call_graph.get(func, set()) if c not in visited)

    return reachable


def compute_closure(
    call_graph: dict[str, set[str]],
    leaf_callers: set[str],
) -> set[str]:
    """Compute transitive closure of functions that reach durable leaves.

    Uses BFS from leaf callers following reverse edges in the call graph.

    This is the scope the THREADING check needs, and it is the right shape for
    it: a function that can reach a host call must be able to supply `h`, so
    the question there really is "who calls into this?".

    Two checks, two directions, one graph -- see compute_reachable. They were
    sharing this one result, which is why cleat#1813 reported a test harness as
    non-deterministic. The AssemblyScript transform has the same defect in the
    mirror direction (cleat#1799), where a callers-only closure means a helper
    the workflow calls is never checked at all.
    """
    # Build reverse call graph (callee -> set of callers)
    reverse_graph: dict[str, set[str]] = {}
    for caller, callees in call_graph.items():
        for callee in callees:
            reverse_graph.setdefault(callee, set()).add(caller)

    # BFS from leaf callers
    closure: set[str] = set()
    queue = list(leaf_callers)
    visited: set[str] = set()

    while queue:
        func = queue.pop(0)
        if func in visited:
            continue
        visited.add(func)
        closure.add(func)
        # Add all callers of this function (reverse edges)
        queue.extend(c for c in reverse_graph.get(func, set()) if c not in visited)

    return closure


def analyze_file(filepath: str) -> AnalysisResult:
    """Analyze a single Python file for workflow code issues.

    Performs:
    1. AST parsing
    2. Entry function detection
    3. Call graph construction
    4. Durable leaf identification
    5. Transitive closure computation
    6. Forbidden API detection in closure
    7. HostCalls threading verification

    Returns an ``AnalysisResult`` with errors, warnings, and summary data.
    """
    result = AnalysisResult(filepath)

    try:
        with open(filepath) as f:
            source = f.read()
    except FileNotFoundError:
        result.add_error(
            "E001",
            0,
            f"File not found: {filepath}",
            suggestion="Check that the file path is correct.",
        )
        return result
    except OSError as e:
        result.add_error(
            "E001",
            0,
            f"Error reading file: {e}",
            suggestion="Check file permissions and that the file is readable.",
        )
        return result

    try:
        tree = ast.parse(source, filename=filepath)
    except SyntaxError as e:
        result.add_error(
            "E001",
            getattr(e, "lineno", 0),
            f"Syntax error: {e.msg}",
            suggestion="Fix the Python syntax error and re-run.",
        )
        return result

    # --- Entry function detection ---
    entries = find_cleat_entry_functions(tree, filepath)
    result.entry_functions = [name for name, _ in entries]

    # Validate entry functions
    for name, func_node in entries:
        # Check for async def
        if isinstance(func_node, ast.AsyncFunctionDef):
            result.add_error(
                "PY012",
                func_node.lineno,
                (f"'{name}' is an async function. Async functions cannot be compiled to WASM."),
                "Remove the 'async' keyword. Cleat workflows run synchronously "
                "use h.call(), h.sleep(), etc. for durable operations.",
                getattr(func_node, "col_offset", 0),
            )

    # --- Call graph construction ---
    call_graph, leaf_calls_by_func, func_defs = build_call_graph_and_leaf_callers(tree)
    result.call_graph = call_graph
    result.function_defs = func_defs

    # --- Durable leaf callers ---
    leaf_callers: set[str] = set()
    for func_name, leaves in leaf_calls_by_func.items():
        if leaves:
            leaf_callers.add(func_name)
    result.durable_leaf_callers = leaf_callers

    # --- Transitive closure (callers) : the threading scope ---
    closure = compute_closure(call_graph, leaf_callers)
    result.durable_closure = closure

    # --- Determinism scope: what the workflow can REACH ---
    #
    # cleat#1813. These two scopes were one set, and the determinism checks were
    # reading the callers closure. A function that calls a workflow -- a
    # mock-driven test harness, a __main__ driver -- is a caller, so it landed
    # in the scope and everything in it was checked for determinism. 22 print()
    # calls in examples/python-langchain's run_test() were reported as PY010,
    # and the gate built on top of this refused the build that example's own
    # README documents.
    #
    # Determinism is a property of what the workflow EXECUTES, so the scope is
    # forward reachability from the entry points.
    #
    # Leaf callers are unioned in because a function that calls h.* is workflow
    # code whether or not this file contains the entry that reaches it -- a
    # helper module with no @cleat_entry of its own would otherwise be checked
    # for nothing.
    #
    # If a file declares no entry points at all, the forward walk has no roots.
    # Falling back to the callers closure keeps such files behaving as they did
    # rather than silently checking nothing, which would trade this false
    # positive for a false negative.
    entry_names = {name for name, _ in entries}
    if entry_names:
        determinism_scope = compute_reachable(call_graph, entry_names | leaf_callers)
    else:
        determinism_scope = set(closure) | leaf_callers

    # --- Forbidden API detection over the determinism scope ---
    # Entry points are always checked even if they don't call SDK methods directly.
    to_check = set(determinism_scope) | entry_names
    checker = ForbiddenAPIChecker(filepath, to_check)
    checker.visit(tree)
    result.errors.extend(checker.errors)

    # --- Imports that only happen at run time (warnings; cleat#4266) ---
    # Over the WHOLE file, not just the determinism scope: any function that
    # runs hits the limit, whether or not it reaches a host call.
    runtime_imports = RuntimeImportChecker(filepath, tree)
    runtime_imports.visit(tree)
    for warning in runtime_imports.warnings:
        result.add_warning(
            warning["code"],
            warning["line"],
            warning["message"],
            warning["suggestion"],
            warning["column"],
        )

    # --- A real-time clock read or sleep (warning; cleat#4396) ---
    # Same scope as the forbidden-API checker: what the workflow executes.
    real_time = RealTimeClockChecker(filepath, tree, to_check)
    real_time.visit(tree)
    for error in real_time.errors:
        result.add_error(error["code"], error["line"], error["message"], error["suggestion"], error["column"])
    for warning in real_time.warnings:
        result.add_warning(
            warning["code"],
            warning["line"],
            warning["message"],
            warning["suggestion"],
            warning["column"],
        )

    # --- A wait that blocks the worker: select.select, a blocking queue get (warning; cleat#4403) ---
    blocking_wait = BlockingWaitChecker(filepath, tree, to_check)
    blocking_wait.visit(tree)
    for warning in blocking_wait.warnings:
        result.add_warning(
            warning["code"],
            warning["line"],
            warning["message"],
            warning["suggestion"],
            warning["column"],
        )

    # Also check imports at module level that might affect closure functions
    # (module-level imports are already caught by the checker visiting the root)

    # --- HostCalls threading verification ---
    #
    # NEITHER closure alone is the right scope here, and both of the obvious
    # answers are wrong in a way that only shows up on real code.
    #
    # The CALLERS closure alone flags a test harness: run_test in
    # examples/python-langchain CONSTRUCTS a HostCalls and passes it down, so
    # it is the boundary where `h` comes from -- every workflow has one, the
    # runtime in production and a mock in a test -- and a boundary does not
    # need to receive `h`.
    #
    # The DETERMINISM scope alone flags pure helpers. `def is_digit(c: str)`,
    # reached from an entry and touching no host call, has no reason to take
    # `h`, and demanding it would be noise on every string or arithmetic helper
    # a workflow uses. Measured while fixing the AssemblyScript twin
    # (cleat#1799), where the same substitution made SIX pure helpers in
    # examples/as-workflow (removed with the AssemblyScript SDK, cleat#4023) -- extractStringField,
    # isDigit, parseI64 and friends -- fail E005 on an unmodified example.
    #
    # The question the check actually asks is "can this function obtain the `h`
    # it needs?", and that is only meaningful for a function that BOTH
    # participates in the workflow and reaches a host call. So: the
    # intersection.
    #
    #   reachable from an entry   and   able to reach a host call
    #
    #   is_digit   forward yes, backward no   -> excluded, it needs no h
    #   run_test   forward no,  backward yes  -> excluded, it supplies h
    #   a durable helper missing h            -> in both, reported
    threading_scope = (set(closure) | leaf_callers) & set(determinism_scope)
    threading_scope |= entry_names
    threading_checker = ThreadingChecker(filepath, func_defs, threading_scope)
    threading_errors = threading_checker.check()
    result.errors.extend(threading_errors)

    return result


# ---------------------------------------------------------------------------
# Entry point detection (for build integration)
# ---------------------------------------------------------------------------


def detect_entry(filepath: str) -> tuple[str | None, str | None]:
    """Detect the single ``@cleat_entry`` function in a file.

    Returns ``(function_name, None)`` on success, or
    ``(None, error_message)`` on failure.

    Also validates that the function is not ``async def``.
    """
    try:
        with open(filepath) as f:
            source = f.read()
    except FileNotFoundError:
        return None, f"File not found: {filepath}"
    except OSError as e:
        return None, f"Error reading file: {e}"

    try:
        tree = ast.parse(source, filename=filepath)
    except SyntaxError as e:
        return None, f"Syntax error in {filepath}: {e.msg} (line {e.lineno})"

    entries = find_cleat_entry_functions(tree, filepath)

    if len(entries) == 0:
        return None, f"no @cleat_entry decorated function found in {filepath}"

    if len(entries) > 1:
        names = ", ".join(name for name, _ in entries)
        return None, (
            f"Multiple @cleat_entry functions found in {filepath}: {names}. "
            f"Use --entry <file.py>:<func_name> to specify which to build."
        )

    name, func_node = entries[0]

    # Check for async def
    if isinstance(func_node, ast.AsyncFunctionDef):
        return None, (
            f"'{name}' in {filepath} is an async function (line {func_node.lineno}). "
            "Async functions cannot be compiled to WASM. "
            "Remove the 'async' keyword. "
            "Cleat workflows run synchronously -- use h.call(), h.sleep(), etc."
        )

    return name, None


# ---------------------------------------------------------------------------
# CLI
# ---------------------------------------------------------------------------


def format_line(error: dict[str, Any]) -> str:
    """Format a single error for human-readable display."""
    parts = [
        f"{error['file']}:{error['line']}",
        error["code"],
        error["message"],
    ]
    if error.get("suggestion"):
        parts.append(f"[suggestion: {error['suggestion']}]")
    return ": ".join(parts)


def print_text_output(result: AnalysisResult) -> None:
    """Print analysis results in human-readable text format."""
    if result.entry_functions:
        print(f"Entry functions: {', '.join(result.entry_functions)}")
    if result.durable_closure:
        closure_list = sorted(result.durable_closure)
        print(f"Durable closure ({len(closure_list)} functions): {', '.join(closure_list)}")
    print()

    for error in result.errors:
        print(format_line(error))

    for warning in result.warnings:
        print(format_line(warning))

    print()
    s = result.summary
    print(
        f"Summary: {s['functions']} functions, "
        f"{s['durable_closure']} in durable closure, "
        f"{len(result.errors)} errors, {len(result.warnings)} warnings"
    )


def print_json_output(result: AnalysisResult) -> None:
    """Print analysis results as JSON."""
    output = {
        "errors": result.errors,
        "warnings": result.warnings,
        "summary": result.summary,
    }
    print(json.dumps(output, indent=2))


def main() -> int:
    """Run the CLI and return exit code."""
    args = sys.argv[1:]

    if not args:
        print("Usage: python -m cleat_sdk.vet [--json] [--detect-entry] <file.py>", file=sys.stderr)
        return 1

    use_json = "--json" in args
    detect_entry_mode = "--detect-entry" in args

    # Filter out flags to get file paths
    files = [a for a in args if not a.startswith("--")]

    if not files:
        print("Error: no Python file specified", file=sys.stderr)
        return 1

    if detect_entry_mode:
        # Detect entry mode: output single entry name or error to stderr
        filepath = files[0]
        func_name, error = detect_entry(filepath)
        if func_name:
            print(func_name)
            return 0
        else:
            print(error, file=sys.stderr)
            return 1

    # Full analysis mode
    all_results: list[AnalysisResult] = []
    exit_code = 0

    for filepath in files:
        result = analyze_file(filepath)
        all_results.append(result)
        if result.errors:
            exit_code = 1

    if use_json:
        if len(all_results) == 1:
            print_json_output(all_results[0])
        else:
            combined = {
                "errors": [],
                "warnings": [],
                "summary": {"total_files": len(all_results)},
            }
            for r in all_results:
                combined["errors"].extend(r.errors)
                combined["warnings"].extend(r.warnings)
            print(json.dumps(combined, indent=2))
    else:
        for result in all_results:
            print_text_output(result)

    return exit_code


if __name__ == "__main__":
    sys.exit(main())
