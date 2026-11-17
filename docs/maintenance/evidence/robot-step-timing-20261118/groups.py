#!/usr/bin/env python3
"""把 robot.json 的步骤平均耗时按“在等什么”归组求和（步骤名前缀即 demo 场景的序号）。"""
import json
import sys

groups = [
    ("saga 状态轮询（15/20 礼物、30/34/37 联盟）", ("15 ", "20 ", "30 ", "34 ", "37 ")),
    ("10 add_exp（Player+World 事务）", ("10 ",)),
    ("53 battle（一整场帧同步对战）", ("53 ",)),
    ("44 move_arrive（服务端寻路 30 格）", ("44 ",)),
    ("11/17 邮件轮询（升级奖励效果链 / 礼物邮件）", ("11 ", "17 ")),
    ("07 聊天推送等待（500ms 选择器）", ("07 ",)),
    ("51/55 匹配推送等待", ("51 ", "55 ")),
    ("02 enter_game", ("02 ",)),
]
for path in sys.argv[1:]:
    report = json.load(open(path))
    steps = {s["name"]: s["mean_ms"] for s in report["steps"]}
    total = sum(steps.values())
    used = 0.0
    print(f"{path}: 步骤平均耗时之和 {total:.0f} ms，整场景 p50 {report['quantiles_ms']['p50']:.0f} / p95 {report['quantiles_ms']['p95']:.0f}")
    for label, prefixes in groups:
        value = sum(v for n, v in steps.items() if n.startswith(prefixes))
        used += value
        print(f"  {label}: {value:.0f} ms ({value / total * 100:.1f}%)")
    print(f"  其余 {len(steps) - sum(1 for n in steps if any(n.startswith(p) for _, ps in groups for p in ps))} 步合计: {total - used:.0f} ms ({(total - used) / total * 100:.1f}%)")
