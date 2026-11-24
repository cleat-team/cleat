"""A workflow that calls the standard-library functions that import a module the FIRST TIME THEY RUN (cleat#4266).

componentize-py keeps only the modules imported while the component is built, so a call that imports another module on first use fails at run
time with ModuleNotFoundError even though the user's own import is at the top of the file. The imports below are exactly what a user writes.
The workflow does NOT import _strptime, heapq or any encodings.* module, which is the point: the SDK is what has to carry them.

Each call reports its own outcome, so one run shows every call rather than the first failure.
"""

import collections
import datetime
import time

from cleat_sdk import HostCalls, cleat_entry


def _try(fn):
    try:
        return {"ok": fn()}
    except Exception as e:  # the type and message are what the test reads
        return {"error": f"{type(e).__name__}: {e}"}


@cleat_entry("late_calls")
def late_calls(h: HostCalls) -> dict:
    return {
        "datetime.strptime": _try(lambda: datetime.datetime.strptime("2026-11-23 05:06:07", "%Y-%m-%d %H:%M:%S").isoformat()),
        "time.strptime": _try(lambda: time.strptime("2026-11-23", "%Y-%m-%d").tm_yday),
        "Counter.most_common": _try(lambda: collections.Counter("abracadabra").most_common(2)),
        "cp1252": _try(lambda: b"\x80 caf\xe9".decode("cp1252")),
        "shift_jis": _try(lambda: b"\x93\xfa\x96\x7b".decode("shift_jis")),
        "iso-8859-15": _try(lambda: b"\xa4 5".decode("iso-8859-15")),
        "utf-8-sig": _try(lambda: b"\xef\xbb\xbfhello".decode("utf-8-sig")),
        "idna": _try(lambda: "bücher.example".encode("idna").decode("ascii")),
    }
