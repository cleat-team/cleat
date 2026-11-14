"""cleat#3793: the README and the Python tutorial documented methods that do not exist.

Nine of the 31 methods the README's "HostCalls overview" listed were not on
``HostCalls`` (``cleat_call``, ``cleat_log``, ``cleat_sleep``, ...), and its first two
snippets used them, so a workflow written as shown built, deployed, and failed in the
guest with ``AttributeError: 'HostCalls' object has no attribute 'cleat_log'``. The real
names are ``call``, ``log``, ``sleep``.

This reads the documents, not the code: it pulls every ``h.<name>`` out of the fenced
Python blocks, and every bullet in the overview, and requires each to be a member.
"""

from __future__ import annotations

import re
from pathlib import Path

from cleat_sdk import HostCalls

SDK = Path(__file__).resolve().parent.parent
DOCS = [
    SDK / "README.md",
    SDK.parent / "docs" / "tutorials" / "python-quick-start.md",
]

MEMBERS = {n for n in dir(HostCalls) if not n.startswith("_")}

# The README's "Deriving the surface" section quotes names to say they do NOT exist.
FENCE = re.compile(r"```python\n(.*?)```", re.S)


def python_blocks(text: str) -> list[str]:
    return FENCE.findall(text)


def host_call_uses(text: str) -> set[str]:
    return {m for block in python_blocks(text) for m in re.findall(r"\bh\.([A-Za-z_]\w*)", block)}


def overview_bullets(text: str) -> set[str]:
    start = text.index("## HostCalls overview")
    end = text.index("\n## ", start + 1)
    return set(re.findall(r"^- `([a-z_]\w*)\(", text[start:end], re.M))


def test_the_checker_reports_a_name_that_does_not_exist():
    """A known-positive: the defect, written the way the README used to write it."""
    bad = '```python\nh.cleat_log("x")\nh.call("a", "b", {})\n```\n'
    assert host_call_uses(bad) - MEMBERS == {"cleat_log"}
    assert "- `cleat_call(service)`".startswith("- `") and (
        overview_bullets("## HostCalls overview\n- `cleat_call(service)`\n## X") - MEMBERS
        == {"cleat_call"}
    )


def test_every_h_method_in_a_documented_snippet_is_on_hostcalls():
    for doc in DOCS:
        used = host_call_uses(doc.read_text())
        assert used, f"{doc.name}: found no h.<method> uses, so this checked nothing"
        assert not used - MEMBERS, f"{doc.name} documents methods HostCalls lacks: {sorted(used - MEMBERS)}"


def test_every_bullet_in_the_hostcalls_overview_is_on_hostcalls():
    bullets = overview_bullets((SDK / "README.md").read_text())
    assert len(bullets) > 20, f"read only {len(bullets)} bullets; the section moved"
    assert not bullets - MEMBERS, f"overview lists methods HostCalls lacks: {sorted(bullets - MEMBERS)}"


def test_the_tutorial_workflow_runs_under_the_harness_and_returns_an_object():
    """The hello.py a reader copies out of the tutorial is the file this runs."""
    from cleat_sdk.test_harness import CleatTestHarness

    text = (SDK.parent / "docs" / "tutorials" / "python-quick-start.md").read_text()
    m = re.search(r"Save this as `hello\.py`:\n\n```python\n(.*?)```", text, re.S)
    assert m, "the tutorial no longer has a 'Save this as `hello.py`' block"
    code = m.group(1)
    assert '@cleat_entry("hello")' in code
    # The decorator turns the function into a WASM export wrapper that takes the raw
    # argument JSON, so run the function it wraps: the body and its shapes are what a
    # snippet check can say, and the decorator line is asserted above.
    bare = code.replace('@cleat_entry("hello")\n', "")
    ns: dict = {}
    exec(compile(bare, "hello.py", "exec"), ns)
    result = ns["hello"](CleatTestHarness(), "Ada")
    assert result == {"greeting": "Hello, Ada!"}
