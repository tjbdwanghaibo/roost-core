#!/usr/bin/env python3
"""数 TestAllianceE2ERPCConcurrency 输出里的“全体超时簇”。

口径（沿用 alliance-join-1000-20261105“单实例全体同时超时”）：1 秒内 ≥32 个尝试以长耗时结束。本夹具只打印 ≥3s 的
慢样本（历史探针是 ≥2.5s），所以这里按“同一 1 秒窗口内结束的 ≥3s 样本 ≥32 个”计，比历史口径略严；另报每阶段失败数、
慢样本数、错误文本分类与栈采集次数。

用法：clusters.py <console.log>...
"""
import re, sys
from collections import Counter, defaultdict
from datetime import datetime, timedelta

slow_re = re.compile(r'PERF rpc slow name=(\S+) worker=(\d+) start=(\S+) duration=(\S+) error=(.*)$')
phase_re = re.compile(r'PERF rpc (\S+) conc=(\d+) n=(\d+) failed=(\d+) slow_ge_3s=(\d+) in (\S+) ([\d.]+)/s (.*)$')
stack_re = re.compile(r'PERF stack sample name=(\S+)')
cfg_re = re.compile(r'PERF rpc config (.*)$')


def dur(s):
    total = 0.0
    for num, unit in re.findall(r'([\d.]+)(ms|µs|us|ns|h|m|s)', s):
        total += float(num) * {'h': 3600, 'm': 60, 's': 1, 'ms': 1e-3, 'µs': 1e-6, 'us': 1e-6, 'ns': 1e-9}[unit]
    return total


for path in sys.argv[1:]:
    slows, phases, stacks, cfg = [], [], Counter(), ''
    for line in open(path, errors='replace'):
        if m := cfg_re.search(line):
            cfg = m.group(1)
        if m := slow_re.search(line):
            start = datetime.fromisoformat(m.group(3).replace('Z', '+00:00'))
            d = dur(m.group(4))
            slows.append((m.group(1), start + timedelta(seconds=d), d, m.group(5).strip()))
        elif m := phase_re.search(line):
            phases.append(m.groups())
        elif m := stack_re.search(line):
            stacks[m.group(1)] += 1
    buckets = defaultdict(list)
    for name, end, d, err in slows:
        buckets[(name, end.replace(microsecond=0))].append((d, err))
    clusters = [(k, v) for k, v in buckets.items() if len(v) >= 32]
    print(f'== {path}\n  config: {cfg}')
    for name, conc, n, failed, slow, elapsed, rate, rest in phases:
        print(f'  phase {name:14s} conc={conc:3s} n={n:5s} failed={failed:4s} slow>=3s={slow:4s} {rate:>9s}/s {rest}')
    errs = Counter((name, err if err == '<nil>' else err[:80]) for name, _, _, err in slows)
    for (name, err), c in errs.most_common(8):
        print(f'  slow sample error  {name:12s} x{c}: {err}')
    maxbucket = max((len(v) for v in buckets.values()), default=0)
    print(f'  slow samples {len(slows)}; max slow completions in one 1s window: {maxbucket}; clusters (>=32 in 1s): {len(clusters)}; stack samples: {dict(stacks)}')
    for (name, sec), v in sorted(clusters, key=lambda kv: kv[0][1]):
        print(f'    CLUSTER {name} at {sec.isoformat()} count={len(v)} errors={sum(1 for d, e in v if e != "<nil>")}')
