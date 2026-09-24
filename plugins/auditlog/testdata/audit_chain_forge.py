#!/usr/bin/env python3
"""Test-only: forge what an editor who controls a whole export file can (cleat#2047).

The chain is UNKEYED: anyone can recompute a row's hash, so deleting or editing a record and
then re-hashing everything after it yields a file that is internally consistent. Only a head
anchor recorded elsewhere disagrees with it. chain_export_matrix_test.go uses this to put that
case in the table using the reference implementation's OWN row_hash, so the forger and the
verifier cannot share a mistake with the Go code.

    audit_chain_forge.py [--drop SEQ] [--edit-path SEQ TEXT] [--renumber] < export.jsonl

Reads an export from stdin and writes the forged one to stdout. After the edits it recomputes
`prev_hash` and `hash` for every chained record in order (a record whose predecessor in the file
is not seq-1 keeps its own prev_hash, as in a range), optionally renumbers `seq` so a dropped
record leaves no gap, and rewrites the checkpoint's head to the new last record and its `events`
to the new count. The floor, the kind and every other checkpoint field are left alone.
"""
import json
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from audit_chain_reference import FIELDS, _Num, _enc, row_hash  # noqa: E402


def dump(v):
    """JSON text that keeps every number exactly as it was read (a _Num is its own text)."""
    if v is None:
        return "null"
    if v is True:
        return "true"
    if v is False:
        return "false"
    if isinstance(v, _Num):
        return str(v)
    if isinstance(v, str):
        return json.dumps(v, ensure_ascii=False)
    if isinstance(v, list):
        return "[" + ",".join(dump(x) for x in v) + "]"
    return "{" + ",".join(json.dumps(k, ensure_ascii=False) + ":" + dump(x) for k, x in v.items()) + "}"


def main():
    drop = None
    edit = None
    renumber = False
    argv = sys.argv[1:]
    while argv:
        flag = argv.pop(0)
        if flag == "--drop":
            drop = int(argv.pop(0))
        elif flag == "--edit-path":
            edit = (int(argv.pop(0)), argv.pop(0))
        elif flag == "--renumber":
            renumber = True
        else:
            sys.exit("unknown argument %r" % flag)

    records = [json.loads(l, parse_int=_Num, parse_float=_Num) for l in sys.stdin if l.strip()]
    cp = records.pop()
    assert cp["type"] == "checkpoint"
    kept = []
    for r in records:
        if r["type"] == "event" and r["seq"] is not None and drop is not None and int(r["seq"]) == drop:
            continue
        if edit and r["seq"] is not None and int(r["seq"]) == edit[0]:
            r["path"] = edit[1]
        kept.append(r)

    prev_seq, prev_hash, next_seq = None, None, None
    for r in kept:
        if r["seq"] is None:
            continue
        seq = int(r["seq"])
        if renumber:
            seq = next_seq if next_seq is not None else seq
        if prev_seq is not None and seq == prev_seq + 1:
            r["prev_hash"] = prev_hash
        rc = {k: (None if r[k] is None else (r[k] if k in ("timestamp", "metadata") else str(r[k])))
              for k in FIELDS if k != "metadata"}
        rc["metadata"] = None if r["metadata"] is None else _enc(r["metadata"])
        rc["seq"] = seq
        r["hash"] = row_hash(bytes.fromhex(r["prev_hash"]), rc).hex()
        if renumber:
            r["seq"] = _Num(str(seq))
        prev_seq, prev_hash, next_seq = seq, r["hash"], seq + 1

    chained = [r for r in kept if r["seq"] is not None]
    if chained:
        cp["head_seq"], cp["head_hash"] = int(chained[-1]["seq"]), chained[-1]["hash"]
    cp["events"] = _Num(str(len(kept)))
    if chained:
        cp["head_seq"] = _Num(str(cp["head_seq"]))
    for r in kept + [cp]:
        print(dump(r))


if __name__ == "__main__":
    main()
