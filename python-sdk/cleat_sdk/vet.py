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
from typing import Any, Dict, List, Optional, Set, Tuple


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

#: SDK durable leaf function names --- HostCalls methods that the runtime
#: journalises (i.e. they create events in the workflow history).
DURABLE_LEAVES: Set[str] = {
    # Core durable calls
    "cleat_call",
    "cleat_call_typed",
    "cleat_call_with_retry",
    "cleat_call_with_heartbeat",
    # Sleep
    "cleat_sleep",
    # Logging
    "cleat_log",
    "log_kv",
    # HTTP fetch
    "cleat_fetch",
    "cleat_fetch_json",
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
    "set_state",
    "get_state",
    "set_query_state",
    "get_query_state",
    "delete_state",
    "incr_state",
    "has_state",
    "list_state",
    # Promises
    "create_promise",
    "await_promise",
    "resolve_promise",
    "reject_promise",
    # Handlers
    "register_update_handler",
    "register_query_handler",
    # Lifecycle
    "cleat_defer",
    "continue_as_new",
    "extend_timeout",
    "run_detached",
    # Fire-and-forget / scheduling
    "cleat_send",
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
FORBIDDEN_API_RULES: Dict[Tuple[Optional[str], str], Tuple[str, str, str]] = {
    (None, "open"): (
        "PY002",
        "File I/O is not allowed in workflow code: file system operations produce non-deterministic side effects across replays.",
        "Use cleat_fetch() or HostCalls instead",
    ),
    (None, "print"): (
        "PY010",
        "print() is not allowed in workflow code: output is lost on replay and not recorded in workflow history.",
        "Use h.cleat_log() instead",
    ),
    ("time", "sleep"): (
        "PY004",
        "time.sleep() is not allowed in durable functions: real-time sleep advances differently on each replay, breaking determinism.",
        "Use h.cleat_sleep() instead",
    ),
    ("time", "time"): (
        "PY005",
        "time.time() is not allowed in durable functions: wall-clock time differs across replays, causing divergent behavior.",
        "Use h.Now() or the workflow clock instead",
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
        "Use cleat_fetch() or HostCalls instead",
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
        "Use cleat_fetch() or cleat_call() instead.",
    ),
    ("socket", "create_connection"): (
        "PY009",
        "socket.create_connection() is not allowed in workflow code: raw sockets produce non-replayable network side effects.",
        "Use cleat_fetch() or cleat_call() instead.",
    ),
}

#: Builtins that are forbidden in workflow code
FORBIDDEN_BUILTINS: Dict[str, Tuple[str, str, str]] = {
    # Already covered by FORBIDDEN_API_RULES with module=None
}

#: Parameter names that are accepted as HostCalls threading indicators
HOSTCALLS_PARAM_NAMES: Set[str] = {"h", "host_calls", "hc", "host"}


# ---------------------------------------------------------------------------
# Analysis result
# ---------------------------------------------------------------------------


class AnalysisResult:
    """Result of analyzing a single Python file."""

    def __init__(self, filepath: str) -> None:
        self.filepath: str = filepath
        self.errors: List[Dict[str, Any]] = []
        self.warnings: List[Dict[str, Any]] = []
        self.entry_functions: List[str] = []
        self.call_graph: Dict[str, Set[str]] = {}
        self.durable_closure: Set[str] = set()
        self.durable_leaf_callers: Set[str] = set()
        self.function_defs: Dict[str, ast.FunctionDef] = {}

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
    def summary(self) -> Dict[str, Any]:
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
        self.graph: Dict[str, Set[str]] = {}
        # caller_name -> set of callee attribute names (e.g., "cleat_call", "sleep")
        self.leaf_calls: Dict[str, Set[str]] = {}
        # Module-level function defs
        self.function_defs: Dict[str, ast.FunctionDef] = {}
        # Current function being analyzed
        self._current_func: Optional[str] = None

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
            # Check if it's a durable leaf (standalone function like cleat_call)
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

    def __init__(self, filepath: str, target_funcs: Set[str]) -> None:
        self.filepath = filepath
        self.target_funcs = target_funcs
        self.errors: List[Dict[str, Any]] = []
        self._current_func: Optional[str] = None

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
                        "suggestion": "Use cleat_fetch() or cleat_call() instead",
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
                        "suggestion": "Use cleat_fetch() or cleat_call() instead",
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
                        "suggestion": "Use cleat_fetch() or cleat_call() instead",
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
                        "suggestion": "Use cleat_fetch() or cleat_call() instead",
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
                    "suggestion": "Use cleat_fetch() or cleat_call() instead",
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
                    "suggestion": "Use cleat_fetch() or cleat_call() instead",
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
                    "suggestion": "Use cleat_fetch() or cleat_call() instead",
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


class ThreadingChecker(ast.NodeVisitor):
    """Verifies HostCalls threading through the durable closure.

    Checks that every function in the durable closure either:
    - Has a parameter named ``h``, ``host_calls``, ``hc``, or ``host``, OR
    - Is called by a function that does (transitive threading).
    """

    def __init__(
        self, filepath: str, func_defs: Dict[str, ast.FunctionDef], closure_funcs: Set[str]
    ) -> None:
        self.filepath = filepath
        self.func_defs = func_defs
        self.closure_funcs = closure_funcs
        self.errors: List[Dict[str, Any]] = []

    def _has_host_calls_param(self, func_node: ast.FunctionDef) -> bool:
        """Check if a function has a HostCalls-like parameter."""
        for arg in func_node.args.args:
            if arg.arg in HOSTCALLS_PARAM_NAMES:
                return True
        return False

    def check(self) -> List[Dict[str, Any]]:
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


def decorate_name(decorator: ast.expr) -> Optional[str]:
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
) -> List[Tuple[str, Any]]:
    """Find all ``@cleat_entry`` decorated functions in the AST."""
    results: List[Tuple[str, ast.FunctionDef]] = []
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
) -> Tuple[Dict[str, Set[str]], Dict[str, Set[str]], Dict[str, ast.FunctionDef]]:
    """Build call graph and identify leaf callers from AST."""
    builder = CallGraphBuilder()
    builder.visit(tree)
    return builder.graph, builder.leaf_calls, builder.function_defs


def compute_closure(
    call_graph: Dict[str, Set[str]],
    leaf_callers: Set[str],
) -> Set[str]:
    """Compute transitive closure of functions that reach durable leaves.

    Uses BFS from leaf callers following reverse edges in the call graph.
    """
    # Build reverse call graph (callee -> set of callers)
    reverse_graph: Dict[str, Set[str]] = {}
    for caller, callees in call_graph.items():
        for callee in callees:
            reverse_graph.setdefault(callee, set()).add(caller)

    # BFS from leaf callers
    closure: Set[str] = set()
    queue = list(leaf_callers)
    visited: Set[str] = set()

    while queue:
        func = queue.pop(0)
        if func in visited:
            continue
        visited.add(func)
        closure.add(func)
        # Add all callers of this function (reverse edges)
        for caller in reverse_graph.get(func, set()):
            if caller not in visited:
                queue.append(caller)

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
    except IOError as e:
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
                "use h.cleat_call(), h.cleat_sleep(), etc. for durable operations.",
                getattr(func_node, "col_offset", 0),
            )

    # --- Call graph construction ---
    call_graph, leaf_calls_by_func, func_defs = build_call_graph_and_leaf_callers(tree)
    result.call_graph = call_graph
    result.function_defs = func_defs

    # --- Durable leaf callers ---
    leaf_callers: Set[str] = set()
    for func_name, leaves in leaf_calls_by_func.items():
        if leaves:
            leaf_callers.add(func_name)
    result.durable_leaf_callers = leaf_callers

    # --- Transitive closure ---
    closure = compute_closure(call_graph, leaf_callers)
    result.durable_closure = closure

    # --- Forbidden API detection in durable closure + entry points ---
    # Entry points are always checked even if they don't call SDK methods directly.
    to_check = set(closure)
    for name, _ in entries:
        to_check.add(name)
    checker = ForbiddenAPIChecker(filepath, to_check)
    checker.visit(tree)
    result.errors.extend(checker.errors)

    # Also check imports at module level that might affect closure functions
    # (module-level imports are already caught by the checker visiting the root)

    # --- HostCalls threading verification ---
    threading_checker = ThreadingChecker(filepath, func_defs, to_check)
    threading_errors = threading_checker.check()
    result.errors.extend(threading_errors)

    return result


# ---------------------------------------------------------------------------
# Entry point detection (for build integration)
# ---------------------------------------------------------------------------


def detect_entry(filepath: str) -> Tuple[Optional[str], Optional[str]]:
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
    except IOError as e:
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
            "Cleat workflows run synchronously -- use h.cleat_call(), h.cleat_sleep(), etc."
        )

    return name, None


# ---------------------------------------------------------------------------
# CLI
# ---------------------------------------------------------------------------


def format_line(error: Dict[str, Any]) -> str:
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
    all_results: List[AnalysisResult] = []
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
