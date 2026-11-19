#!/usr/bin/env python3
"""汇总 rounds.txt：每个（房间数，标签）各量的中位数与三轮原始值，以及相对变化。usage: summ.py rounds.txt"""
import collections
import json
import statistics
import sys

rows = collections.defaultdict(list)
for line in open(sys.argv[1]):
    label, _, raw = line.split(" ", 2)
    d = json.loads(raw)
    rows[(d["Rooms"], label)].append(d)

KEYS = [("MeanPhysicsHz", "物理均频 Hz", 2), ("DeliveredHzPerClient", "交付 Hz / 客户端", 2), ("CommitP50ms", "提交 p50 ms", 3), ("CommitP99ms", "提交 p99 ms", 3),
        ("StepP99ms", "Step p99 ms", 2), ("AllocationMBps", "Go 分配 MB/s", 2), ("AllocsPerFrame", "分配次数 / 交付帧", 1),
        ("PayloadKBpsPerClient", "载荷 KB/s / 客户端", 2), ("FactsPerSecond", "事实 / s", 0), ("RespawnsPerClientPerSecond", "重生 / 客户端 / s", 3)]


def cell(values, digits):
    return "%.*f (%s)" % (digits, statistics.median(values), ", ".join("%.*f" % (digits, v) for v in values))


def median(rooms, label, key):
    values = [d[key] for d in rows[(rooms, label)] if key in d]
    return statistics.median(values) if values else None


for rooms in sorted({k[0] for k in rows}):
    print(f"\n### {rooms} 房间 × 20 客户端\n\n| 量 | 改前 | 流程关 | 流程开 | 关 相对 改前 | 开 相对 关 |\n| --- | --- | --- | --- | ---: | ---: |")
    for key, name, digits in KEYS:
        cells, medians = [], {}
        for label in ("base", "off", "on"):
            values = [d[key] for d in rows[(rooms, label)] if key in d]
            cells.append(cell(values, digits) if values else "—")
            medians[label] = statistics.median(values) if values else None

        def change(a, b):
            if medians[a] is None or medians[b] is None or medians[b] == 0:
                return "—"
            return "%+.1f%%" % (100 * (medians[a] / medians[b] - 1))

        print(f"| {name} | {cells[0]} | {cells[1]} | {cells[2]} | {change('off', 'base')} | {change('on', 'off')} |")
