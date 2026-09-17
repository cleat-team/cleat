# KNOWN LIMIT -- this workflow is non-deterministic and the Python checker does
# not see it. That is the point of the fixture; it is not a bug to be fixed here.
#
# cleat_sdk/vet.py is a real AST call-graph and closure analyser, the strongest
# of the four non-Go checkers. Its forbidden-API table is keyed on exact
# (module, function) pairs, and matched only against a ONE-LEVEL `module.func()`
# call. So the limit is not a missing module name -- it is the SHAPE of the call
# site.
#
# This file calls the very function the checker knows about. `("time", "time")`
# is in the table as PY005. Writing it the other way round makes it invisible:
#
#   import time;            time.time()   ->  PY005, caught
#   from time import time;  time()        ->  0 errors, this file
#
# Same function, same non-determinism, one import statement apart. That is what
# makes this a better fixture than a module the table has simply never heard of:
# it cannot be dismissed as an omission from a list, because the list has the
# entry and the call still escapes.
#
# Measured 2026-09-17, same shape in three more places, so this is a class:
#
#   import datetime;  datetime.datetime.now()  0 errors   (two-level chain)
#   import os.path;   os.path.exists("x")      0 errors   (two-level chain)
#   import os;        os.getpid()              0 errors   (not in the table)
#   import pathlib;   pathlib.Path(..).read_text()  0 errors
#   import secrets;   secrets.token_hex()      0 errors
#
# The companion fixture py002_open uses open() and IS caught. The pair is
# asserted together in python_build_refuses_nondeterminism_test.go so that
# "the gate is wired" and "the gate is weak" are both stated, and neither can
# be mistaken for the other.
from time import time

from cleat_sdk import cleat_entry, HostCalls


@cleat_entry
def workflow(h: HostCalls, input: str) -> str:
    # Non-deterministic: wall-clock time differs between the original run and
    # every replay. h.now() is the deterministic equivalent. The checker knows
    # this function; it does not recognise it spelled this way.
    stamp = time()
    return '{"stamp": %f}' % stamp
