"""Standard-library modules that a call imports the FIRST TIME IT RUNS, imported here so that they are in the component (cleat#4266).

componentize-py keeps only the standard-library modules that were imported while the component was being built, and it builds by importing the
workflow's module. A function that imports another module on first use then fails at run time with ModuleNotFoundError, though the workflow's own
imports are all at the top of its file. Every workflow imports ``cleat_sdk``, so importing them here puts them in every component.

This is the short list that was MEASURED to fail and to be fixed by exactly this import, not a preload of the standard library: following every
lazy import converges on the whole stdlib (cleat#4266). Add a name only after measuring the call that needs it in a built component, and add its
case to engine/testdata/pystdlib/late_calls_workflow.py.

    datetime.strptime, time.strptime   _strptime
    collections.Counter.most_common    heapq
    the cp1252, shift_jis, iso-8859-15, utf-8-sig and idna codecs    their encodings modules (idna also needs encodings.punycode)
"""

import importlib

#: The modules this file imports. ``cleat vet`` reads it too, so the warning it gives and the imports done here cannot disagree.
PRELOADED_MODULES: tuple[str, ...] = (
    "_strptime",
    "heapq",
    "encodings.cp1252",
    "encodings.shift_jis",
    "encodings.iso8859_15",
    "encodings.utf_8_sig",
    "encodings.idna",
    "encodings.punycode",
)

for _name in PRELOADED_MODULES:
    importlib.import_module(_name)
