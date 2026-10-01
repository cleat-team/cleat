"""Parse a GitHub Actions workflow file's `on:` trigger block.

cleat#2737/cleat-review on #2893: check-workflow-pr-triggers.sh,
check-workflow-concurrency.sh and check-workflow-guards.py's
workflow_triggers() each independently parsed a workflow's `on:` block --
`on:` is nested mapping with two spellings for every list, plus a YAML 1.1
quirk where a bare `on:` key parses as the boolean True rather than the
string "on". Three copies of the same half-dozen lines, free to drift if
the parsing was ever corrected in one and not the others. This module is
the one place it is read.
"""
import yaml


def triggers_from_doc(doc):
    """Return an already-parsed workflow document's `on:` block as
    {trigger_name: trigger_config_or_None}.

    Normalizes all three shapes a workflow's `on:` can take:
      - a mapping     (`on: {push: {...}, pull_request: {...}}`)
      - a list        (`on: [push, pull_request]`)
      - a bare string (`on: push`)
    and the YAML 1.1 parse of a bare `on` key as the boolean True. The
    result is always a dict, so callers never need their own isinstance
    check before calling .get() on it.
    """
    on = doc.get("on", doc.get(True))
    if isinstance(on, dict):
        return on
    if isinstance(on, list):
        return {str(t): None for t in on}
    if on is not None:
        return {str(on): None}
    return {}


def load_triggers(path):
    """Return a workflow FILE's `on:` block as {trigger_name: trigger_config_or_None}.

    See triggers_from_doc() for the normalization. This is the entry point
    for callers that have a path rather than an already-parsed document.
    """
    with open(path) as fh:
        doc = yaml.safe_load(fh) or {}
    return triggers_from_doc(doc)
