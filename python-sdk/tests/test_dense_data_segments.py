"""The dense-data rewrite in scripts/build_wasm.py (cleat#4382).

wasmtime builds a shared copy-on-write memory image for a module only if its data segments cover at least half of their span, or the span is
under 16 MiB. CPython's module covers 49% of 21.7 MiB, so it got none and every Python workflow instance paid about 12 MiB. The build rewrites
such a module's data into one segment. These tests pin what that rewrite must and must not do, on small hand-built modules so they run
anywhere, without componentize-py. The real component is pinned by engine/a_python_instance_shares_a_memory_image_linux_test.go.
"""

from __future__ import annotations

import importlib.util
import shutil
import subprocess
from pathlib import Path

import pytest

SCRIPT = Path(__file__).resolve().parent.parent / "scripts" / "build_wasm.py"
_spec = importlib.util.spec_from_file_location("build_wasm_under_test", SCRIPT)
bw = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(bw)

MIB = 1 << 20


def _section(sid: int, payload: bytes) -> bytes:
    return bytes([sid]) + bw._uleb(len(payload)) + payload


def _segment(offset: int, data: bytes) -> bytes:
    return bw._uleb(0) + b"\x41" + bw._sleb(offset) + b"\x0b" + bw._uleb(len(data)) + data


def _module(segments: list[tuple[int, bytes]], pages: int = 400, datacount: bool = True) -> bytes:
    """A valid core module: one memory of ``pages`` pages, one function type, and these active data segments."""
    out = b"\x00asm\x01\x00\x00\x00"
    out += _section(1, b"\x01\x60\x00\x00")  # type section: () -> ()
    out += _section(5, b"\x01\x00" + bw._uleb(pages))  # memory section: one memory, minimum only
    if datacount:
        out += _section(12, bw._uleb(len(segments)))
    out += _section(11, bw._uleb(len(segments)) + b"".join(_segment(o, d) for o, d in segments))
    return out


def _segments(module: bytes) -> list[tuple[int, bytes]]:
    for sid, a, b in bw._wasm_sections(module):
        if sid == 11:
            return bw._parse_data_segments(module[a:b])
    return []


def _memory(module: bytes, size: int) -> bytes:
    """The linear memory a module starts with, as the spec defines it: zero, then each active segment in order."""
    mem = bytearray(size)
    for off, data in _segments(module):
        mem[off : off + len(data)] = data
    return bytes(mem)


def _validates(module: bytes) -> None:
    if shutil.which("wasm-tools"):
        r = subprocess.run(["wasm-tools", "validate", "-"], input=module, capture_output=True)
        assert r.returncode == 0, r.stderr.decode()


# A layout shaped like CPython's: many small segments with gaps, more than half empty, over a span above wasmtime's 16 MiB.
SPARSE = [(1024 + i * 40_000, bytes([i % 251 + 1]) * 12_000) for i in range(0, 600)] + [(22 * MIB, b"\xaa" * 28_000)]


def test_a_sparse_module_becomes_one_dense_segment_with_the_same_memory():
    original = _module(SPARSE)
    _validates(original)
    result = bw.densify_module(original)
    assert result is not None, "a module covering under half of a span over 16 MiB must be rewritten"
    dense, info = result
    _validates(dense)
    segs = _segments(dense)
    assert len(segs) == 1
    assert info["added_bytes"] == len(dense) - len(original) > 0
    size = 23 * MIB
    assert _memory(dense, size) == _memory(original, size), "the rewrite changed what memory holds after instantiation"


def test_the_rewrite_is_idempotent():
    dense, _ = bw.densify_module(_module(SPARSE))
    assert bw.densify_module(dense) is None, "a module that already has one dense segment must be left alone"


def test_overlapping_segments_keep_the_spec_order_later_wins():
    segs = [(1024, b"\x01" * 100), (1050, b"\x02" * 100), (20 * MIB, b"\x03" * 4096), (20 * MIB + 8192, b"\x04" * 10)]
    original = _module(segs)
    dense, _ = bw.densify_module(original)
    assert _memory(dense, 21 * MIB) == _memory(original, 21 * MIB)
    assert _memory(dense, 21 * MIB)[1050:1060] == b"\x02" * 10


def test_a_module_wasmtime_already_gives_an_image_is_not_touched():
    # Dense: the data covers more than half of the span, however large.
    dense_already = _module([(0, b"\x07" * (20 * MIB))], pages=400)
    assert bw.densify_module(dense_already) is None
    # Small: sparse, but the span is under 16 MiB, where wasmtime builds an image whatever the density.
    small = _module([(0, b"\x01" * 100), (8 * MIB, b"\x02" * 100)], pages=200)
    assert bw.densify_module(small) is None
    # No data at all.
    assert bw.densify_module(b"\x00asm\x01\x00\x00\x00" + _section(1, b"\x01\x60\x00\x00")) is None


def test_the_boundary_is_wasmtimes_rule_not_a_nearby_one():
    # data == span / 2 exactly: "image_size < 2 * data_size" is false, so wasmtime builds no image, so the module is rewritten.
    half = _module([(0, b"\x01" * (9 * MIB - 1)), (18 * MIB - 1, b"\x02")], pages=400)  # data 9 MiB, span 18 MiB
    assert bw.densify_module(half) is not None
    # one byte of data more than half: wasmtime builds the image already.
    more = _module([(0, b"\x01" * (9 * MIB)), (18 * MIB - 1, b"\x02")], pages=400)  # data 9 MiB + 1
    assert bw.densify_module(more) is None


def test_every_other_section_is_byte_for_byte_unchanged():
    original = _module(SPARSE)
    dense, _ = bw.densify_module(original)
    old, new = bw._wasm_sections(original), bw._wasm_sections(dense)
    assert [s[0] for s in old] == [s[0] for s in new]
    for (sid, a, b), (_, na, nb) in zip(old, new):
        if sid not in (11, 12):
            assert original[a:b] == dense[na:nb]
    # the data count follows the segment count
    count = [dense[a:b] for sid, a, b in new if sid == 12][0]
    assert bw._read_uleb(count, 0)[0] == 1


@pytest.mark.parametrize(
    "bad",
    [
        pytest.param(bw._uleb(1) + bw._uleb(1) + bw._uleb(4) + b"abcd", id="passive-segment"),
        pytest.param(bw._uleb(1) + bw._uleb(2) + bw._uleb(0) + b"\x41\x00\x0b" + bw._uleb(1) + b"x", id="other-memory"),
        pytest.param(bw._uleb(1) + bw._uleb(0) + b"\x23\x00\x0b" + bw._uleb(1) + b"x", id="offset-is-a-global"),
    ],
)
def test_a_shape_it_does_not_model_is_refused_not_guessed(bad):
    module = b"\x00asm\x01\x00\x00\x00" + _section(11, bad)
    with pytest.raises(bw.NotDensifiable):
        bw.densify_module(module)


def test_a_component_is_rewritten_in_its_core_modules_and_nowhere_else(tmp_path):
    prefix = b"\x00asm\x0d\x00\x01\x00"  # a component header
    custom = _section(0, b"\x04name" + b"payload")
    small_core = _module([(0, b"\x01" * 100)], pages=2)
    component = prefix + custom + _section(1, _module(SPARSE)) + _section(1, small_core) + custom
    dense, report = bw.densify_component(component)
    assert len(report) == 1, "only the sparse module needed rewriting"
    secs = bw._wasm_sections(dense)
    assert [s[0] for s in secs] == [0, 1, 1, 0]
    assert dense[secs[0][1] : secs[0][2]] == custom[2:]
    assert dense[secs[2][1] : secs[2][2]] == small_core
    assert dense[secs[3][1] : secs[3][2]] == custom[2:]
    again, report2 = bw.densify_component(dense)
    assert again == dense and report2 == []


def test_the_build_step_keeps_the_original_and_says_so_when_it_cannot_parse(tmp_path, capsys):
    core = b"\x00asm\x01\x00\x00\x00" + _section(11, bw._uleb(1) + bw._uleb(1) + bw._uleb(4) + b"abcd")
    module = b"\x00asm\x0d\x00\x01\x00" + _section(1, core)
    path = tmp_path / "w.wasm"
    path.write_bytes(module)
    assert bw.densify_output(str(path)) == 0
    assert path.read_bytes() == module, "an unparseable module must be left exactly as it was"
    assert "was not rewritten" in capsys.readouterr().err, "the cost of keeping the original must be said, not silent"


def test_the_build_step_rewrites_a_file_in_place_and_reports_it(tmp_path, capsys):
    path = tmp_path / "w.wasm"
    path.write_bytes(b"\x00asm\x0d\x00\x01\x00" + _section(1, _module(SPARSE)))
    added = bw.densify_output(str(path))
    assert added > 0
    written = path.read_bytes()
    (core,) = [written[a:b] for sid, a, b in bw._wasm_sections(written) if sid == 1]
    assert len(_segments(core)) == 1
    assert "Dense data:" in capsys.readouterr().out
    assert not Path(str(path) + ".dense").exists(), "the temporary file must not be left behind"
