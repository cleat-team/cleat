"""Shared fixtures for the Python SDK test suite."""

from __future__ import annotations

import pytest

from cleat_sdk.test_harness import CleatTestHarness


@pytest.fixture
def test_env() -> CleatTestHarness:
    """Create a clean CleatTestHarness for behavioural testing.

    Each call returns a fresh harness with all stubs cleared, the
    simulated clock reset to 2024-01-01T00:00:00Z, and an empty
    call history.
    """
    h = CleatTestHarness()
    h.reset()
    return h
