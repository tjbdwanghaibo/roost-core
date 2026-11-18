#!/usr/bin/env python3
"""数历史探针 TestJoinScaleRetry 输出里的“全体超时簇”：LONG 行给出耗时 ≥2.5s 的尝试按结束时刻（100ms 桶）的计数，
按秒合并后 ≥32 记一簇（alliance-join-1000-20261105 的口径）；另报 STALL 次数与每阶段 PERF 行。

用法：probe_clusters.py <probe.log>...
"""
import re, sys
from collections import Counter

for path in sys.argv[1:]:
    text = open(path, errors='replace').read()
    print(f'== {path}')
    for line in text.splitlines():
        if 'PERF rpc' in line and ('conc=' in line or 'first error' in line):
            print('  ' + line.split('PERF rpc ', 1)[1][:200])
    stalls = re.findall(r'STALL (\S+) (\S+): (\d+) attempts', text)
    clusters, longs = [], 0
    for name, body in re.findall(r'LONG (\S+) attempts>=2.5s by end time:(.*)', text):
        per_sec = Counter()
        for stamp, n in re.findall(r'(\d\d:\d\d:\d\d)\.\d=(\d+)', body):
            per_sec[stamp] += int(n)
            longs += int(n)
        clusters += [(name, s, n) for s, n in per_sec.items() if n >= 32]
    print(f'  attempts>=2.5s: {longs}; STALL dumps: {len(stalls)} {stalls}; clusters (>=32 ending >=2.5s within 1s): {len(clusters)} {clusters}')
