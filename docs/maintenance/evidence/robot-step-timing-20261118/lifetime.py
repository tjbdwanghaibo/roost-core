#!/usr/bin/env python3
"""第 5 轮诊断（未提交补丁）的日志拆分：联盟 saga 从“发起应答”到“机器人轮询看到终态”分成三段。

answered_ms：found/donate/claim 发起请求应答机器人的时刻（Nest 事务已提交、启动 effect 已在 WAL 里）
created_ms ：协调器分区里建出 saga 记录的时刻（record.CreatedAt）
updated_ms ：saga 进入终态的时刻（终态记录的 UpdatedAt）
seen_ms    ：机器人第一次轮询读到终态的时刻
"""
import json
import statistics
import sys
from pathlib import Path

log = Path(sys.argv[1]) / "logs" / "game.log"
answered, lifetime = {}, {}
for line in log.read_text(errors="replace").splitlines():
    try:
        r = json.loads(line)
    except json.JSONDecodeError:
        continue
    if r.get("msg") == "diag saga start answered":
        answered[r["saga_key"]] = (r["op"], r["answered_ms"])
    elif r.get("msg") == "diag saga lifetime" and r["saga_key"] not in lifetime:
        lifetime[r["saga_key"]] = r


def describe(values):
    values = sorted(values)
    if not values:
        return "n=0"
    return f"n={len(values)} mean={statistics.mean(values):.0f} p50={values[len(values) // 2]} p95={values[int(len(values) * .95) - 1]} max={values[-1]}"


for op in ("found", "donate", "reward"):
    start, run, see, total = [], [], [], []
    for key, (kind, answered_ms) in answered.items():
        r = lifetime.get(key)
        if kind != op or r is None:
            continue
        start.append(r["created_ms"] - answered_ms)
        run.append(r["updated_ms"] - r["created_ms"])
        see.append(r["seen_ms"] - r["updated_ms"])
        total.append(r["seen_ms"] - answered_ms)
    print(op)
    print("  start path (answered -> record created):", describe(start))
    print("  coordinator lifetime (created -> terminal):", describe(run))
    print("  poll lag (terminal -> robot saw it):", describe(see))
    print("  total (answered -> robot saw it):", describe(total))
