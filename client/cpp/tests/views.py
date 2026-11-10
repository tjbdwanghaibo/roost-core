#!/usr/bin/env python3
"""Independent frame protocol 2 / subject envelope v3 wrapper over Go generated DAO protobuf goldens."""
import json
import pathlib
import struct
import subprocess
import sys
import tempfile

repo = pathlib.Path(__file__).resolve().parents[3]
rows = json.loads((repo / "client/spec/dao-sync.json").read_text())


def varint(n):
    out = bytearray()
    while n >= 0x80:
        out.append((n & 0x7F) | 0x80)
        n >>= 7
    out.append(n)
    return bytes(out)


def packet(tick, payload=None):
    # Frame protocol 2: magic "RF", protocol 2, kind; varints stream, epoch, tick, [tick-base], schema, objects.
    header = b"RF" + bytes([2, 1 if tick == 1 else 2]) + varint(1) + varint(1) + varint(tick)
    if tick != 1:
        header += varint(1)
    header += varint(1) + varint(1)
    if tick == 3:
        return header + bytes([3]) + varint(1) + varint(1)
    if tick == 1:
        # Full envelope: flags 1; id, kind, version, base, encoding 2, lod 0, schema 1, "hero", "" (default).
        body = (
            bytes([1])
            + varint(9007199254740993)
            + varint(1)
            + varint(tick)
            + varint(0)
            + varint(2)
            + bytes([0])
            + varint(1)
            + varint(4)
            + b"hero"
            + varint(0)
            + payload
        )
    else:
        # Delta envelope: flags 0; base, version - base; identity comes from the baseline.
        body = bytes([0]) + varint(tick - 1) + varint(1) + payload
    obj = bytes([1 if tick == 1 else 2]) + varint(1) + varint(1)
    if tick == 1:
        obj += varint(1)
    obj += varint(1) + bytes([1]) + varint(1) + varint(1) + varint(len(body)) + body
    return header + obj


if __name__ == "__main__":
    with tempfile.TemporaryDirectory() as directory:
        files = []
        for tick in range(1, 4):
            path = pathlib.Path(directory) / str(tick)
            path.write_bytes(
                packet(tick, bytes.fromhex(rows[tick - 1]["hex"]) if tick < 3 else None)
            )
            files.append(str(path))
        subprocess.run([sys.argv[1], *files], check=True, timeout=10)
