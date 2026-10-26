#!/usr/bin/env python3
"""汇总 saga-scan.sh 的一个结果目录：每档成功吞吐、延迟、终态与关键路径阶段拆分。

原生步骤（native-*）用逐跳计时 Hops 的“每笔累计耗时”（PerSagaMS = 该跳总耗时 / 完成笔数）按关键路径分组；
Mongo 步骤（mongo-*）优先用 saga-timeline.py 的逐笔时间线（timeline-summary.json），没有时退回 hops.json。
分组只取关键路径上的跳，不把后台的回执等待（h12）、outbox 删除（ack）这类旁路时间加进去。
"""
import argparse
import glob
import json
import os
import re

STATUS = {"4": "Completed", "5": "Compensated", "6": "Failed", "7": "ManualRequired"}

# 原生步骤关键路径分组（每笔两个步骤合计）。
NATIVE_GROUPS = [
    ("mongo_coord_txn", "协调 Mongo 事务（启动/派发/完成推进）", ["h00.start.create", "h02.dispatch.apply", "h11.completion.handle"]),
    ("coord_wake_claim", "协调到期唤醒 + 认领", ["h01.coord.wake"]),
    ("mongo_step_reserve", "步骤预留 Mongo 事务", ["h05.step.reserve"]),
    ("nest_wal", "Nest 业务 + 本地 WAL 提交", ["h06.step.handler"]),
    ("projection_wait", "WAL→投影排队（写屏障前段）", ["h09.de.project_wait"]),
    ("mongo_projection_txn", "投影 Mongo 事务（回执/effect）", ["h09.de.project_txn"]),
    ("outbox_wait", "outbox 排队（saga 命令 + DE effect）", ["h03.outbox.wait", "h10.de.outbox_wait"]),
    ("nats_puback", "NATS 发布等 PubAck", ["h03.outbox.publish", "h10.de.outbox_publish"]),
    ("nats_deliver", "NATS 投递 + 消费排队", ["h04.step.deliver", "h11.completion.deliver"]),
]
# Redis 权威方案能直接替换掉的组（按设计稿 §1/§4/§6：协调状态、认领、步骤预留、原生步骤的投影裁决与写屏障）。
NATIVE_REDIS = {"mongo_coord_txn", "coord_wake_claim", "mongo_step_reserve", "projection_wait", "mongo_projection_txn"}

# Mongo 步骤时间线分组（saga-timeline.py 的 Stages）。
MONGO_GROUPS = [
    ("mongo_coord_txn", "协调 Mongo 事务（启动/派发/完成推进）", ["start_commit", "dispatch_commit", "completion_apply"]),
    ("coord_wake_claim", "协调等待 + 认领", ["coordinator_wait_and_claim", "handoff_overlap"]),
    ("mongo_step_txn", "步骤 Mongo 事务（预留 + 业务 + 回执提交）", ["step_reserve", "business_writes", "receipt_and_commit"]),
    ("outbox_wait", "outbox 排队", ["outbox_wait"]),
    ("nats", "NATS 命令/结果交付（含 PubAck）", ["command_transport", "result_transport", "command_transport_to_parser", "command_transport_client_queue", "result_transport_to_parser", "result_transport_client_queue"]),
    ("consumer_slot", "消费槽排队", ["command_consumer_slot", "result_consumer_slot"]),
    ("other", "其余（解码/准备）", ["step_decode", "step_prepare", "transaction_prepare", "result_prepare"]),
]
# Mongo 步骤的业务事务在设计稿 §6 明确不迁移，Redis 只替换协调部分。
MONGO_REDIS = {"mongo_coord_txn", "coord_wake_claim"}


def load(path):
    with open(path) as f:
        return json.load(f)


def redis_info(path):
    """读 INFO 文本（saga-scan.sh 的 redis-before / redis-after），返回 {字段: 值}；commandstats 拆成 {命令: calls}。"""
    fields, calls = {}, {}
    with open(path) as f:
        for line in f:
            line = line.strip()
            if line.startswith("cmdstat_"):
                name, rest = line[len("cmdstat_"):].split(":", 1)
                stats = dict(kv.split("=", 1) for kv in rest.split(","))
                calls[name] = int(stats.get("calls", 0))
            elif ":" in line and not line.startswith("#"):
                key, value = line.split(":", 1)
                fields[key] = value
    return fields, calls


def per_saga_counts(r, path):
    """每笔往返：Mongo 用夹具窗口内的 metrics.commands 差值减夹具自己的读；Redis 用 commandstats 的 EVALSHA + EVAL 调用差值。
    分母是本档到达终态的笔数（计数窗口覆盖整档，含爬升与排空）。"""
    out = {}
    completed = r.get("Completed") or 0
    commands = r.get("MongoCommands")
    if commands is not None and completed:
        total = sum(commands.values())
        harness = r.get("HarnessMongoCommands") or 0
        out["MongoCommandsPerSaga"] = round((total - harness) / completed, 2)
        out["MongoCommandBreakdownPerSaga"] = {k: round(v / completed, 2) for k, v in sorted(commands.items()) if v}
        out["HarnessMongoCommands"] = harness
    before, after = path[:-5] + ".redis-before.txt", path[:-5] + ".redis-after.txt"
    if os.path.exists(before) and os.path.exists(after) and completed:
        fb, cb = redis_info(before)
        fa, ca = redis_info(after)
        scripts = sum(ca.get(k, 0) - cb.get(k, 0) for k in ("evalsha", "eval"))
        out["RedisScriptsPerSaga"] = round(scripts / completed, 2)
        out["RedisCommandsPerSaga"] = {k: round((ca[k] - cb.get(k, 0)) / completed, 2) for k in sorted(ca) if ca[k] - cb.get(k, 0) > 0}
        out["RedisUsedMemoryPeak"] = fa.get("used_memory_peak_human")
        try:
            out["RedisAOFGrowthBytes"] = int(fa.get("aof_current_size", 0)) - int(fb.get("aof_current_size", 0))
        except ValueError:
            pass
        out["RedisKeysAfter"] = fa.get("dbsize")
    return out


def summarize(directory):
    rows = []
    for path in sorted(glob.glob(os.path.join(directory, "*-c*.json"))):
        name = os.path.basename(path)[:-5]
        m = re.fullmatch(r"(native|mongo)-(forward|compensate)-c(\d+)", name)
        if not m:
            continue
        kind, mode, level = m.group(1), m.group(2), int(m.group(3))
        r = load(path)
        server = r.get("Server", {})
        if kind == "mongo":
            server = server.get("Completion", {})
        statuses = {STATUS.get(k, k): v for k, v in (r.get("Statuses") or {}).items()}
        row = {
            "Scenario": f"{kind}-{mode}", "Concurrency": level,
            "SteadyTPS": r.get("SteadyTPS"), "CompletionTPS": r.get("CompletionTPS"),
            "Started": r.get("Started"), "Completed": r.get("Completed"), "WrongTerminal": r.get("WrongTerminal"),
            "Statuses": statuses, "BusinessVerified": r.get("BusinessVerified"),
            "ServerP50MS": server.get("P50MS"), "ServerP99MS": server.get("P99MS"), "ServerMaxMS": server.get("MaxMS"),
            "ProcessCPUCores": r.get("ProcessCPUCores"),
            "FinalStatuses": {STATUS.get(k, k): v for k, v in (r.get("FinalStatuses") or {}).items()},
            "FinalWrongTerminal": r.get("FinalWrongTerminal"),
        }
        row.update(per_saga_counts(r, path))
        groups = {}
        if kind == "native" and r.get("Hops"):
            hops = r["Hops"]
            for key, _, members in NATIVE_GROUPS:
                groups[key] = sum(hops.get(h, {}).get("PerSagaMS", 0.0) for h in members)
            row["MongoCommandsPerSaga"] = {k[6:]: round(v["PerSaga"], 2) for k, v in hops.items() if k.startswith("mongo.")}
        elif kind == "mongo":
            summary = path[:-5] + ".timeline-summary.json"
            if os.path.exists(summary):
                stages = load(summary)["Stages"]
                for key, _, members in MONGO_GROUPS:
                    groups[key] = sum(stages.get(s, {}).get("MeanMS", 0.0) for s in members)
                row["TimelineSource"] = "timeline"
            else:
                row["TimelineSource"] = "unavailable (retries/failed terminals or no trace)"
        if groups:
            total = sum(groups.values())
            redis = NATIVE_REDIS if kind == "native" else MONGO_REDIS
            row["StagesMS"] = {k: round(v, 2) for k, v in groups.items()}
            row["StageSumMS"] = round(total, 2)
            row["RedisReplaceableMS"] = round(sum(v for k, v in groups.items() if k in redis), 2)
            row["RedisReplaceableShare"] = round(row["RedisReplaceableMS"] / total, 3) if total else None
        for side in ("before", "after"):
            p = path[:-5] + f".server-{side}.json"
            if os.path.exists(p):
                row.setdefault("_server", {})[side] = load(p)
        sb, sa = row.pop("_server", {}).get("before"), None
        if sb is not None:
            sa = load(path[:-5] + ".server-after.json")
        if sb and sa and not sb.get("Unavailable") and not sa.get("Unavailable"):
            ops = sa["WriteOps"] - sb["WriteOps"]
            syncs = sa["WTLogSyncs"] - sb["WTLogSyncs"]
            row["MongoServer"] = {
                "WriteOps": ops, "TxnCommitted": sa["TxnCommitted"] - sb["TxnCommitted"],
                "WriteMeanMS": round((sa["WriteLatency"] - sb["WriteLatency"]) / ops / 1000, 3) if ops else None,
                "WTLogSyncs": syncs,
                "WTLogSyncMeanMS": round((sa["WTLogSyncMicros"] - sb["WTLogSyncMicros"]) / syncs / 1000, 3) if syncs else None,
            }
        rows.append(row)
    rows.sort(key=lambda x: (x["Scenario"], x["Concurrency"]))
    return rows


def markdown(rows):
    out = ["| 场景 | 并发 | 稳态成功 TPS | 服务端 P50 / P99 / max ms | 终态（复读） | 核对 | 每笔 Mongo 往返 | 每笔 Redis 脚本 | CPU 核 |",
           "| --- | ---: | ---: | --- | --- | --- | ---: | ---: | ---: |"]
    for r in rows:
        fmt = lambda v: "—" if v is None else f"{v:.0f}"
        num = lambda v: "—" if v is None else f"{v:.1f}"
        final = r.get("FinalStatuses") or r["Statuses"]
        out.append(f"| {r['Scenario']} | {r['Concurrency']} | {r['SteadyTPS']:.2f} | {fmt(r['ServerP50MS'])} / {fmt(r['ServerP99MS'])} / {fmt(r['ServerMaxMS'])} | "
                   f"{', '.join(f'{k} {v}' for k, v in sorted(final.items()))} | {'通过' if r['BusinessVerified'] else '未通过'} | "
                   f"{num(r.get('MongoCommandsPerSaga'))} | {num(r.get('RedisScriptsPerSaga'))} | {r['ProcessCPUCores']:.2f} |")
    return "\n".join(out)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("directory")
    parser.add_argument("--output", required=True)
    args = parser.parse_args()
    rows = summarize(args.directory)
    with open(args.output, "w") as f:
        json.dump(rows, f, ensure_ascii=False, indent=2)
        f.write("\n")
    print(markdown(rows))


if __name__ == "__main__":
    main()
