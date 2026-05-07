"""Hello World workflow for Cleat Python/WASM.

This is the simplest possible Cleat workflow demonstrating:
1. @cleat_entry decorator
2. HostCalls.cleat_call()
3. WASM compilation and execution

Usage:
    durable build --target python --entry hello_workflow.py:hello
    durable run hello '{"name": "World"}'
"""

from cleat_sdk import HostCalls, cleat_entry


@cleat_entry("Hello")
def hello(h: HostCalls, name: str = "World") -> str:
    """A simple hello workflow that calls a greeter service.

    Parameters
    ----------
    h : HostCalls
        The Cleat host calls interface (injected automatically).
    name : str
        Name to greet. Defaults to "World".

    Returns
    -------
    str
        A greeting message from the greeter service.
    """
    greeting = h.cleat_call("greeter", "greet", {"name": name})
    return greeting
