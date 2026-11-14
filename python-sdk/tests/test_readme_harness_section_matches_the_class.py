"""cleat#3846: the README's CleatTestHarness section documented methods the class does not have.

``run``, ``register_call_stub``, ``register_call_handler``, ``deliver_signal`` and
``call_records`` were all in the section, and its example, the first thing a new Python
user copies, failed with ``AttributeError`` on its second line. ``assert_called`` was
documented with a ``times=`` parameter it does not take, and as an assertion when it
returns a bool.

This is the same shape as ``test_docs_name_only_real_host_calls.py`` for the harness: it
reads the document, requires every method it names to be a member, and RUNS the example.
"""

from __future__ import annotations

import re
from pathlib import Path

from cleat_sdk.test_harness import CleatTestHarness

README = Path(__file__).resolve().parent.parent / "README.md"
MEMBERS = {n for n in dir(CleatTestHarness) if not n.startswith("_")}
ATTRIBUTES = {"now_ms", "call_history"}  # instance attributes, not in dir(class)


def harness_section(text: str) -> str:
    start = text.index("### `CleatTestHarness()`")
    end = text.index("\n### ", start + 1)
    return text[start:end]


def python_blocks(section: str) -> list[str]:
    return re.findall(r"```python\n(.*?)```", section, re.S)


def names_used_on_a_harness(section: str) -> set[str]:
    return {
        m
        for block in python_blocks(section)
        for m in re.findall(r"\bharness\.([A-Za-z_]\w*)", block)
    }


def bullet_method_names(section: str) -> set[str]:
    key = section[section.index("Key members:") :]
    return set(re.findall(r"`([a-z_]\w*)\(", key))


def test_the_checker_reports_the_names_the_section_used_to_document():
    """A known-positive: the section as it was before cleat#3846."""
    old = (
        "### `CleatTestHarness()`\n```python\n"
        'harness.register_call_stub("greeter", "Greet", "{}")\n'
        'result = harness.run(GreetWorkflow, "Alice")\n'
        'harness.assert_called("greeter", "Greet", times=1)\n```\n'
        "Key members:\n- `run(entry_fn, input)` -- execute\n- `deliver_signal(n, p)` -- signal\n"
        "- `advance_time(ms)` -- clock\n### next\n"
    )
    section = harness_section(old + "\n### `Plugins(host)`")
    assert names_used_on_a_harness(section) - MEMBERS == {"register_call_stub", "run"}
    assert bullet_method_names(section) - MEMBERS == {"run", "deliver_signal"}


def test_every_harness_method_the_example_calls_exists():
    section = harness_section(README.read_text())
    used = names_used_on_a_harness(section)
    assert used, "found no harness.<method> uses, so this checked nothing"
    assert not used - MEMBERS, (
        f"the example calls methods the class lacks: {sorted(used - MEMBERS)}"
    )


def test_every_method_in_the_key_members_list_exists():
    section = harness_section(README.read_text())
    names = bullet_method_names(section)
    assert len(names) >= 8, f"found only {sorted(names)}, so this checked little"
    assert not names - MEMBERS, (
        f"the list documents methods the class lacks: {sorted(names - MEMBERS)}"
    )
    for attr in ATTRIBUTES:
        assert attr in section, f"{attr} is no longer documented"
        assert hasattr(CleatTestHarness(), attr), f"documented attribute {attr} does not exist"


def test_the_example_runs():
    """The document's own example, executed: the check that would have failed on its second line."""
    section = harness_section(README.read_text())
    blocks = python_blocks(section)
    assert len(blocks) == 1, "the section is expected to carry one runnable example"
    namespace: dict = {}
    exec(compile(blocks[0], "README.md:CleatTestHarness", "exec"), namespace)
    tests = [v for k, v in namespace.items() if k.startswith("test_") and callable(v)]
    assert tests, "the example defines no test_ function, so running it checked nothing"
    for t in tests:
        t()
