#!/usr/bin/env python3
"""把 robot.json 的 steps 排成 Markdown 表：按场景顺序，附占整场景平均耗时的比例。"""
import json
import sys

report = json.load(open(sys.argv[1]))
steps = report.get("steps") or []
total = sum(s["mean_ms"] * s["samples"] for s in steps) / max(1, report["stats"]["success"] + report["stats"]["failure"])
print(f"quantiles_ms={report.get('quantiles_ms')} stats={report['stats']['success']}/{report['stats']['started']} sum_of_step_means={total:.0f}ms")
order = sorted(steps, key=lambda s: s["name"]) if "--order" in sys.argv else sorted(steps, key=lambda s: -s["mean_ms"])
print("| 步骤 | 样本 | ok/失败 | 平均 ms | p50 | p95 | max | 占比 | wait 平均 ms | retry 平均/最大 |")
print("|---|---|---|---|---|---|---|---|---|---|")
for s in order:
    share = s["mean_ms"] / total * 100 if total else 0
    print(f"| `{s['name']}` | {s['samples']} | {s['ok']}/{s['failed']} | {s['mean_ms']:.0f} | {s['p50_ms']:.0f} | {s['p95_ms']:.0f} | {s['max_ms']:.0f} | {share:.1f}% | {s['wait_mean_ms']:.0f} | {s['retry_attempts_avg']:.2f}/{s['retry_attempts_max']} |")
