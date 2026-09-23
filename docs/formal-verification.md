# Formal verification — see specs/README.md

This document was a May 2026 pre-implementation assessment: which components would benefit
from TLA+, and sketches of what the models might look like. It described a workflow status
set (`completed`, `timed_out`, `suspended`) that does not match the implementation even at
the time it was written, and its pseudo-TLA+ sketches predate the four real specifications
that now live in `specs/`.

**Retired as of cleat#1996.** For the actual state of formal verification in this repo —
which specs exist, which are parsed by SANY and checked by TLC, at what bounds, run in CI or
not — read [`specs/README.md`](../specs/README.md), which is checked against the real specs
rather than describing a plan.

The five-component assessment this file used to contain (claim protocol, state machine,
signal delivery, concurrency keys, history compaction) is preserved in git history if it is
useful as a record of the original motivation; nothing in it should be read as current status.
