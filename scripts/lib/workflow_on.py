"""Parse a GitHub Actions workflow file's `on:` trigger block.

cleat#2737: check-workflow-pr-triggers.sh and check-workflow-concurrency.sh
each need to know what events trigger a workflow, and `on:` is nested
mapping with two spellings for every list -- plus a YAML 1.1 quirk where a
bare `on:` key parses as the boolean True rather than the string "on". Both
guards had independently copied the same half-dozen lines to handle this;
if the parsing was ever corrected, the fix could land in one and not the
other with nothing to notice. This module is the one place it is read.
"""
import yaml


def load_triggers(path):
    """Return the `on:` block as {trigger_name: trigger_config_or_None}.

    Normalizes all three shapes a workflow's `on:` can take:
      - a mapping     (`on: {push: {...}, pull_request: {...}}`)
      - a list        (`on: [push, pull_request]`)
      - a bare string (`on: push`)
    and the YAML 1.1 parse of a bare `on` key as the boolean True. The
    result is always a dict, so callers never need their own isinstance
    check before calling .get() on it.
    """
    with open(path) as fh:
        doc = yaml.safe_load(fh) or {}
    on = doc.get("on", doc.get(True))
    if isinstance(on, dict):
        return on
    if isinstance(on, list):
        return {str(t): None for t in on}
    if on is not None:
        return {str(on): None}
    return {}
