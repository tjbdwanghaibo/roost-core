#!/usr/bin/env python3
"""对比两次逐区域 vmmap：按（区域类型, 共享模式, 映射文件）汇总常驻 / 脏页，输出增量。

usage: regions.py <vmmap-A> <vmmap-B>
只统计非共享缓存的区域（SM=COW/PRV/NUL 等都算；路径落在 dyld 共享缓存里的 __TEXT 等计入
'shared-cache' 组，便于和本进程私有映射分开）。单位 KiB。
"""
import re
import sys
from collections import defaultdict

UNIT = {"K": 1, "M": 1024, "G": 1024 * 1024}
LINE = re.compile(r"^(.+?)\s+([0-9a-f]+)-([0-9a-f]+)\s+\[\s*([0-9.]+[KMG])\s+([0-9.]+[KMG])\s+([0-9.]+[KMG])\s+([0-9.]+[KMG])\]\s+\S+\s+SM=(\S+)\s*(.*)$")


def kib(s):
    return float(s[:-1]) * UNIT[s[-1]]


def load(path):
    groups = defaultdict(lambda: [0.0, 0.0])
    for line in open(path, errors="replace"):
        m = LINE.match(line.rstrip("\n"))
        if not m:
            continue
        rtype, _, _, _, res, dirty, _, sm, detail = m.groups()
        rtype = rtype.strip()
        detail = detail.strip()
        if "dyld_shared_cache" in detail or "/System/Library/" in detail or "/usr/lib/" in detail:
            where = "system-lib/shared-cache"
        elif detail.startswith("/"):
            where = detail.rsplit("/", 1)[-1]
        else:
            where = "-"
        key = (rtype, sm, where)
        groups[key][0] += kib(res)
        groups[key][1] += kib(dirty)
    return groups


a, b = load(sys.argv[1]), load(sys.argv[2])
rows = []
for key in set(a) | set(b):
    ra, da = a.get(key, [0, 0])
    rb, db = b.get(key, [0, 0])
    rows.append((rb - ra, db - da, ra, rb, key))
rows.sort(key=lambda r: -abs(r[0]))
tot = [0.0, 0.0, 0.0, 0.0]
print(f"{'d_resident':>10} {'d_dirty':>9} {'res_A':>9} {'res_B':>9}  type / share / mapping")
for dr, dd, ra, rb, key in rows:
    tot[0] += dr; tot[1] += dd; tot[2] += ra; tot[3] += rb
    if abs(dr) >= 16 or abs(dd) >= 16:
        print(f"{dr:10.0f} {dd:9.0f} {ra:9.0f} {rb:9.0f}  {' / '.join(key)}")
print(f"{tot[0]:10.0f} {tot[1]:9.0f} {tot[2]:9.0f} {tot[3]:9.0f}  TOTAL (all regions parsed)")
