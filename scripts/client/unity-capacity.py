#!/usr/bin/env python3
"""Create a deterministic 20Hz/1%-5% DAO corpus and run a Unity Player."""
import argparse, json, pathlib, struct, subprocess

p = argparse.ArgumentParser()
p.add_argument("--player", required=True)
p.add_argument("--output", required=True)
a = p.parse_args()
root = pathlib.Path(a.output).resolve()
if root.exists():
    p.error("output exists")
root.mkdir(parents=True)


def varint(n):
    b = bytearray()
    while n >= 128:
        b.append((n & 127) | 128)
        n >>= 7
    b.append(n)
    return bytes(b)


def frame(tick, ids, versions, create):
    out = struct.pack(
        "!4sHBBQIIIHH",
        b"CRP1",
        1,
        1 if tick == 1 else 2,
        0,
        1,
        1,
        tick,
        0 if tick == 1 else tick - 1,
        1,
        len(ids),
    )
    for identity in ids:
        before = versions[identity]
        versions[identity] += 1
        level = b"\x08" + varint(versions[identity])
        payload = b"\x08" + varint(identity) + b"\x1a" + varint(len(level)) + level
        body = (
            struct.pack(
                "!4sHHQ I QQQ I HBB I HHI",
                b"RSSU",
                2,
                int(create),
                identity,
                1,
                versions[identity],
                0 if create else before,
                0,
                0,
                2,
                0,
                0,
                1,
                4,
                7,
                len(payload),
            )
            + b"herodefault"
            + payload
        )
        out += (
            struct.pack(
                "!BHHHHBHHI",
                1 if create else 2,
                identity,
                1,
                int(create),
                1,
                1,
                1,
                1,
                len(body),
            )
            + body
        )
    return out


rows = []
for count in (50, 500, 2000, 10000):
    for percent in (1, 5):
        corpus = root / f"{count}-{percent}.bin"
        versions = [0] * (count + 1)
        tick = 0
        with corpus.open("wb") as f:

            def integer(n):
                f.write(struct.pack("<I", n))

            def write(data):
                integer(len(data))
                f.write(data)

            f.write(b"RCB1")
            integer(count)
            integer(percent)
            integer((count + 199) // 200)
            for start in range(1, count + 1, 200):
                tick += 1
                write(
                    frame(
                        tick, range(start, min(count + 1, start + 200)), versions, True
                    )
                )
            integer(200)
            for t in range(200):
                offset = t * count * percent // 100
                dirty = (t + 1) * count * percent // 100 - offset
                integer((dirty + 199) // 200)
                for start in range(0, dirty, 200):
                    tick += 1
                    write(
                        frame(
                            tick,
                            [
                                (offset + j) % count + 1
                                for j in range(start, min(dirty, start + 200))
                            ],
                            versions,
                            False,
                        )
                    )
        for repeat in range(1, 4):
            report = root / f"{count}-{percent}-{repeat}.json"
            log = root / f"{count}-{percent}-{repeat}.log"
            subprocess.run(
                [
                    a.player,
                    "-batchmode",
                    "-nographics",
                    "-logFile",
                    str(log),
                    "-roostCapacity",
                    str(corpus),
                    "-roostReport",
                    str(report),
                ],
                check=True,
                timeout=60,
            )
            row = json.loads(report.read_text())
            assert row["verified"]
            row["repeat"] = repeat
            rows.append(row)
(root / "summary.json").write_text(
    json.dumps(
        {
            "kind": "Unity Player burst apply benchmark, not rendered frame or WAN latency",
            "rows": rows,
        },
        indent=2,
    )
    + "\n"
)
print(root / "summary.json")
