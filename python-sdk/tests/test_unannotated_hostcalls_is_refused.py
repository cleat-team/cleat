"""An unannotated HostCalls parameter is refused where it is written.

cleat#1637. ``cleat_entry`` identifies the injected runtime parameter by its
TYPE HINT. Without the hint it is not skipped, so it becomes a workflow
parameter with no default -- and the presence check then refuses every payload,
because no caller ever sends a key called ``h``: the framework injects it.

The workflow never runs, on any input, and the old failure named the wrong
thing: ``Missing required parameters: h`` reads as a caller problem. Adding
``"h"`` to the start payload DOES make it go away, and hands the workflow a JSON
value where it expects a HostCalls -- so the fix that suggests itself is worse
than the defect.
"""

import pytest

from cleat_sdk.entry import cleat_entry
from cleat_sdk.host_calls import HostCalls


def test_an_unannotated_hostcalls_parameter_is_refused():
    with pytest.raises(TypeError) as excinfo:

        @cleat_entry
        def bad(h, note: str) -> str:
            return note

    msg = str(excinfo.value)
    # The message must name the ANNOTATION, not the payload. That is the whole
    # point: the old runtime error named the payload and sent the reader to add
    # a key.
    assert "HostCalls" in msg, msg
    assert "h: HostCalls" in msg, msg


def test_the_annotated_form_is_accepted_and_binds(capsys):
    """The control.

    Without it, "unannotated is refused" is also satisfied by a decorator that
    refuses everything -- which would be a far worse defect and would look
    identical from the test above.
    """

    @cleat_entry
    def good(h: HostCalls, note: str) -> str:
        return note

    wrapper = getattr(good, "export_wrapper", good)
    assert wrapper('{"note":"hi"}') == '"hi"'


def test_a_first_parameter_that_is_plain_data_is_refused():
    """The same rule, reached the other way.

    A workflow whose first parameter is genuinely data has no way to receive the
    runtime at all. Java's annotation processor refuses this shape outright
    ("@CleatEntry method first parameter must be cleat.HostCalls, got ..."); this
    brings Python to the same place.
    """
    with pytest.raises(TypeError):

        @cleat_entry
        def also_bad(note: str, h: HostCalls) -> str:
            return note


def test_a_zero_parameter_entry_is_left_alone():
    """Deliberately NOT refused, and pinned so the scope is not widened by accident.

    testdata/vet-checks/python/* declares several zero-parameter entries
    (``def workflow() -> None``) as fixtures for other rules. Whether a
    zero-parameter entry should be legal is a separate question, and cleat#1637
    must not decide it as a side effect.
    """

    @cleat_entry
    def no_params() -> None:
        return None

    assert no_params is not None
