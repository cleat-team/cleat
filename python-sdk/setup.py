"""Packaging hook: put the WIT and the build scripts into the wheel (cleat#3793).

Everything else about the package is in pyproject.toml. This file exists for one
thing, and it is not expressible there.

WHY A HOOK AND NOT PACKAGE DATA. The WIT (``wit/``) and the two build scripts
(``scripts/build_wasm.py``, ``scripts/stamp_metadata.py``) live BESIDE the
``cleat_sdk`` package, not inside it, and a wheel contains only what
package discovery finds under ``cleat_sdk``. ``package-data`` cannot name a file
outside its package, and moving the directories would break every reader of
``python-sdk/wit/cleat.wit`` and ``python-sdk/scripts/`` -- the engine tests, the
Java SDK's metadata script, the release smoke job and the Makefile (counted at
cleat#3793). So the files stay where they are and are copied into the BUILT tree:

    wit/*.wit                      -> cleat_sdk/wit/
    scripts/build_wasm.py          -> cleat_sdk/_build/
    scripts/stamp_metadata.py      -> cleat_sdk/_build/

``cleat build --target python`` finds the installed copy when it is not run from
inside a checkout; scripts/build_wasm.py's ``SdkLayout`` is what knows both
places. tests/test_wheel_ships_the_build_inputs.py builds a wheel and lists it.

Nothing is copied into the SOURCE tree, so a checkout stays free of a second copy
of the WIT that could drift from the first.
"""

import shutil
from pathlib import Path

from setuptools import setup
from setuptools.command.build_py import build_py

HERE = Path(__file__).resolve().parent

# (source relative to python-sdk/, glob, destination relative to the package)
SHIPPED = [
    ("wit", "*.wit", "wit"),
    ("scripts", "build_wasm.py", "_build"),
    ("scripts", "stamp_metadata.py", "_build"),
]


class build_py_with_build_inputs(build_py):
    def run(self):
        super().run()
        if self.dry_run:
            return
        package = Path(self.build_lib) / "cleat_sdk"
        for src_dir, pattern, dest in SHIPPED:
            matched = sorted((HERE / src_dir).glob(pattern))
            # An empty match is a broken wheel that installs fine and fails at
            # build time on the user's machine, which is the defect this hook
            # exists to remove. Fail the build instead.
            if not matched:
                raise RuntimeError(
                    f"{src_dir}/{pattern} matched nothing; the wheel would ship without it"
                )
            (package / dest).mkdir(parents=True, exist_ok=True)
            for f in matched:
                shutil.copy2(f, package / dest / f.name)


setup(cmdclass={"build_py": build_py_with_build_inputs})
