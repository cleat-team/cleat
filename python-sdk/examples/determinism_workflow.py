"""Reads every non-deterministic source a Python guest has, and returns them.

For cleat#1410. componentize-py's CPython satisfies these through WASI Preview 2
interfaces -- ``wasi:clocks/wall-clock`` and ``wasi:random/random`` -- not
through cleat's host calls, so without shadowing on the component linker they
reach the real clock and the real entropy source and a replay diverges.

Three sources rather than one, because they do not share a route:

``time.time()``        -> ``wasi:clocks/wall-clock#now()``
``random.random()``    -> the Mersenne Twister, SEEDED from ``os.urandom`` at
                          import, so it is a test of ``get-random-bytes`` one
                          step removed
``os.urandom(8)``      -> ``wasi:random/random#get-random-bytes`` directly

The middle one is why shadowing ``get-random-u64`` alone is not a shippable
slice: CPython seeds from bytes, so a guest whose ``random.random()`` is still
live reads as fixed everywhere the test looks and is not.
"""

import os
import random
import time

from cleat_sdk import HostCalls, cleat_entry


@cleat_entry("determinism_probe")
def determinism_probe(h: HostCalls) -> dict:
    return {
        "wall_clock_seconds": time.time(),
        "random_float": random.random(),
        "urandom_hex": os.urandom(8).hex(),
        # The durable host calls, for contrast. These are already
        # deterministic, so they are the positive control: if these two move
        # between runs the harness is wrong, not the guest.
        "durable_now_ms": h.now(),
        "durable_random": h.random(),
    }
