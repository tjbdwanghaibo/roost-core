#!/usr/bin/env python3
"""重建单进程、无重试正向 Saga 的关键路径；不把各跳 P99 相加。"""
import argparse
from collections import defaultdict
import json
from pathlib import Path

STAGES = [
    ("dispatch_commit", "dispatch.begin", "dispatch.end"),
    ("outbox_wait", "dispatch.end", "publish.begin"),
    ("command_transport", "publish.begin", "step.received"),
    ("command_consumer_slot", "step.received", "step.begin"),
    ("step_decode", "step.begin", "reserve.begin"),
    ("step_reserve", "reserve.begin", "reserve.end"),
    ("step_prepare", "reserve.end", "execute.begin"),
    ("transaction_prepare", "execute.begin", "handler.begin"),
    ("business_writes", "handler.begin", "handler.end"),
    ("receipt_and_commit", "handler.end", "execute.end"),
    ("result_prepare", "execute.end", "result.publish.begin"),
    ("result_transport", "result.publish.begin", "result.received"),
    ("result_consumer_slot", "result.received", "complete.begin"),
    ("completion_apply", "complete.begin", "complete.end"),
]


def percentile(rows, fraction):
    return sorted(rows)[min(len(rows) - 1, int((len(rows) - 1) * fraction))]


def analyze(result, timeline):
    if result["Mode"] != "forward" or result["WrongTerminal"] or timeline["Dropped"]:
        raise ValueError("only fully successful forward runs without dropped events are supported")
    times = defaultdict(dict)
    links = defaultdict(list)
    duplicates = []
    for event in timeline["Events"]:
        parts = event["Key"].split("/")
        if parts[1] == "link":
            if len(parts) != 4:
                raise ValueError("invalid link event")
            links[parts[2]].append(parts[3])
        else:
            _, name, identity = parts
            if name in times[identity]:
                duplicates.append(event["Key"])
            times[identity][name] = event["AtNS"]
    if duplicates:
        raise ValueError(f"retries / duplicate events require separate analysis: {duplicates[:5]}")
    rows = []
    for saga_id, commands in links.items():
        if len(commands) != len(set(commands)):
            raise ValueError("duplicate command links")
        start = times[saga_id]
        parts = defaultdict(float)
        parts["handoff_overlap"] = 0.0
        def add(name, begin, end):
            if end < begin:
                # Mongo 写入可在前一请求收到提交确认前被别的 worker 看见。
                # 仅允许跨 worker 的交接重叠，单 goroutine 内逆序仍视为坏数据。
                if name not in {"coordinator_wait_and_claim", "outbox_wait"}:
                    raise ValueError(f"noncausal interval {saga_id}: {name}")
                parts["handoff_overlap"] += (end - begin) / 1e6
                parts[name] += 0.0
                return
            parts[name] += (end - begin) / 1e6
        add("start_commit", start["start.begin"], start["start.end"])
        previous = start["start.end"]
        ordered = sorted(commands, key=lambda cmd: times[cmd]["dispatch.begin"])
        for command in ordered:
            t = times[command]
            add("coordinator_wait_and_claim", previous, t["dispatch.begin"])
            for stage, begin, end in STAGES:
                add(stage, t[begin], t[end])
            previous = t["complete.end"]
        total = (previous - start["start.begin"]) / 1e6
        if abs(sum(parts.values()) - total) > 1e-6:
            raise ValueError("partition does not cover the entire transaction")
        rows.append({"SagaID": saga_id, "TotalMS": total, "Steps": len(ordered), "PartsMS": dict(parts)})
    if len(rows) != result["Started"] or len(rows) != result["Completed"]:
        raise ValueError(f"incomplete coverage: timelines={len(rows)} started={result['Started']} completed={result['Completed']}")
    if not rows:
        raise ValueError("empty run")
    rows.sort(key=lambda row: row["TotalMS"])
    p99 = rows[int((len(rows) - 1) * .99)]
    slow = [row for row in rows if row["TotalMS"] >= p99["TotalMS"]]
    names = rows[0]["PartsMS"]
    stages = {}
    for name in names:
        samples = [row["PartsMS"][name] for row in rows]
        stages[name] = {"MeanMS": sum(samples) / len(rows), "P99MS": percentile(samples, .99),
                        "Slowest1PercentMeanMS": sum(row["PartsMS"][name] for row in slow) / len(slow)}
    # PubAck 与交付可能重叠，只作嵌套诊断，不加入上面的分段总和。
    nested = {}
    for label, begin, end in [("command_puback", "publish.begin", "publish.end"),
                               ("result_puback", "result.publish.begin", "result.publish.end")]:
        samples = [(t[end] - t[begin]) / 1e6 for t in times.values() if begin in t and end in t]
        if len(samples) != sum(len(commands) for commands in links.values()) or any(value < 0 for value in samples):
            raise ValueError("incomplete or reversed publication intervals")
        nested[label] = {"Samples": len(samples), "MeanPerCommandMS": sum(samples) / len(samples),
                         "P99PerCommandMS": percentile(samples, .99)}
    return {"NestedPublish": nested, "Transactions": len(rows), "P50MS": percentile([r["TotalMS"] for r in rows], .5),
            "P99MS": p99["TotalMS"], "MaxMS": rows[-1]["TotalMS"], "Stages": stages,
            "P99Transaction": p99, "SlowestTransaction": rows[-1], "SlowCohortCount": len(slow)}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("result", type=Path, help="forward.json; timeline is read from forward.json.timeline.json")
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    summary = analyze(json.loads(args.result.read_text()), json.loads(Path(str(args.result) + ".timeline.json").read_text()))
    args.output.write_text(json.dumps(summary, ensure_ascii=False, indent=2) + "\n")
    print(f"transactions={summary['Transactions']} P50={summary['P50MS']:.2f}ms P99={summary['P99MS']:.2f}ms max={summary['MaxMS']:.2f}ms")
    for name, stage in sorted(summary["Stages"].items(), key=lambda item: item[1]["MeanMS"], reverse=True):
        print(f"{name:30} mean={stage['MeanMS']:8.2f}ms slowest1% mean={stage['Slowest1PercentMeanMS']:8.2f}ms")


if __name__ == "__main__":
    main()
