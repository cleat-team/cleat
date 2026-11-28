"""cleat#3793: ``pip install cleat-sdk`` has to be enough to build a workflow.

The wheel shipped neither the WIT nor ``build_wasm.py``, so ``cleat build
--target python`` needed a 305 MB checkout of the repository to find them. These
build a REAL wheel and read it, because the failure they guard is a wheel that
installs fine and builds nothing -- which no import test can see.

Building needs ``pip`` and, with build isolation, network access for setuptools.
A machine that cannot do that fails these rather than skipping them: a skip here
would be a green that measured nothing, which is what the missing files were.
"""

from __future__ import annotations

import importlib.util
import shutil
import subprocess
import sys
import zipfile
from pathlib import Path

import pytest

SDK = Path(__file__).resolve().parent.parent

# What the wheel must carry, as (path inside the wheel, source in python-sdk/).
SHIPPED = {
    "cleat_sdk/wit/cleat.wit": "wit/cleat.wit",
    "cleat_sdk/_build/build_wasm.py": "scripts/build_wasm.py",
    "cleat_sdk/_build/stamp_metadata.py": "scripts/stamp_metadata.py",
}


@pytest.fixture(scope="module")
def wheel(tmp_path_factory) -> Path:
    work = tmp_path_factory.mktemp("wheel-src")
    src = work / "python-sdk"
    # A copy, so building leaves no build/ or *.egg-info in the checkout.
    shutil.copytree(
        SDK,
        src,
        ignore=shutil.ignore_patterns(
            "tests", "dist", "build", "*.egg-info", "__pycache__", ".pytest_cache", ".ruff_cache"
        ),
    )
    out = work / "out"
    proc = subprocess.run(
        [sys.executable, "-m", "pip", "wheel", "--no-deps", "-q", "-w", str(out), str(src)],
        capture_output=True,
        text=True,
    )
    assert proc.returncode == 0, (
        f"could not build a wheel, so nothing below was measured:\n{proc.stdout}\n{proc.stderr}"
    )
    wheels = sorted(out.glob("cleat_sdk-*.whl"))
    assert len(wheels) == 1, f"expected exactly one wheel, found {wheels}"
    return wheels[0]


@pytest.fixture(scope="module")
def unpacked(wheel, tmp_path_factory) -> Path:
    dest = tmp_path_factory.mktemp("site-packages")
    with zipfile.ZipFile(wheel) as z:
        z.extractall(dest)
    return dest


def test_the_wheel_carries_the_wit_and_both_build_scripts_byte_for_byte(wheel):
    with zipfile.ZipFile(wheel) as z:
        names = set(z.namelist())
        for inside, source in SHIPPED.items():
            assert inside in names, f"{inside} is not in the wheel"
            assert z.read(inside) == (SDK / source).read_bytes(), (
                f"{inside} differs from python-sdk/{source}: the wheel ships a stale copy"
            )


def test_the_wheel_has_no_top_level_name_but_the_package(wheel):
    """The allowlist in pyproject.toml exists because a wheel once carried a top-level
    ``tests`` module into every consumer's site-packages. Adding files to the wheel must
    not reopen that."""
    with zipfile.ZipFile(wheel) as z:
        top = {n.split("/", 1)[0] for n in z.namelist()}
    stray = {t for t in top if t != "cleat_sdk" and not t.endswith(".dist-info")}
    assert not stray, f"top-level names beside cleat_sdk in the wheel: {sorted(stray)}"


def _load_build_wasm(path: Path):
    spec = importlib.util.spec_from_file_location(f"build_wasm_{abs(hash(path))}", path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def test_an_installed_build_script_finds_its_wit_beside_the_package(unpacked):
    mod = _load_build_wasm(unpacked / "cleat_sdk" / "_build" / "build_wasm.py")
    layout = mod.SdkLayout(unpacked / "cleat_sdk" / "_build" / "build_wasm.py")
    assert layout.installed
    assert layout.wit_dir == unpacked / "cleat_sdk" / "wit"
    assert (layout.wit_dir / "cleat.wit").is_file()
    assert layout.stamp_script == unpacked / "cleat_sdk" / "_build" / "stamp_metadata.py"


def test_an_installed_build_stages_cleat_sdk_alone_not_all_of_site_packages(unpacked):
    """componentize-py bundles whatever ``-p`` names. In an installed layout the
    parent of cleat_sdk is site-packages, which holds every other package in the
    environment, so the script must stage a copy of cleat_sdk and point at that."""
    script = unpacked / "cleat_sdk" / "_build" / "build_wasm.py"
    # A bystander in "site-packages" that must never reach the component.
    (unpacked / "bystander_pkg").mkdir()
    (unpacked / "bystander_pkg" / "__init__.py").write_text("")

    layout = _load_build_wasm(script).SdkLayout(script)
    layout.stage()
    try:
        staged = Path(layout.python_path)
        assert staged != unpacked, "-p would be site-packages itself"
        assert (staged / "cleat_sdk" / "__init__.py").is_file()
        assert not (staged / "bystander_pkg").exists()
        # What the component does not need: the build tooling and the WIT source.
        assert not (staged / "cleat_sdk" / "_build").exists()
        assert not (staged / "cleat_sdk" / "wit").exists()
    finally:
        layout.close()
    assert not staged.exists(), "close() left the staged copy behind"


def test_a_checkout_layout_stages_nothing(tmp_path):
    layout = _load_build_wasm(SDK / "scripts" / "build_wasm.py").SdkLayout(
        SDK / "scripts" / "build_wasm.py"
    )
    assert not layout.installed
    assert layout.python_path == SDK
    assert layout.wit_dir == SDK / "wit"
    layout.stage()
    assert layout.python_path == SDK
    layout.close()


def test_the_installed_script_validates_an_entry_with_no_checkout_on_the_path(unpacked, tmp_path):
    """The script, run from the unpacked wheel with nothing of this repository on
    ``PYTHONPATH``, gets as far as validating an entry. ``--validate-only`` stops before
    componentize-py, so this needs no toolchain -- but it is past the WIT lookup, which
    is the part that used to need a checkout."""
    entry = tmp_path / "hello.py"
    entry.write_text(
        "from cleat_sdk import HostCalls, cleat_entry\n\n\n"
        '@cleat_entry("hello")\ndef hello(h: HostCalls, name: str) -> dict:\n'
        '    return {"greeting": name}\n'
    )
    proc = subprocess.run(
        [
            sys.executable,
            str(unpacked / "cleat_sdk" / "_build" / "build_wasm.py"),
            "--entry",
            f"{entry}:hello",
            "--validate-only",
        ],
        capture_output=True,
        text=True,
        cwd=tmp_path,
        env={"PATH": "/usr/bin:/bin"},
    )
    assert proc.returncode == 0, proc.stdout + proc.stderr
    assert "Validation passed" in proc.stdout
