#!/usr/bin/env python3
"""保留原始日志，并明确区分可观察的日志次数与全量 Saga 状态。"""
import json
from pathlib import Path
import re
import shutil
import sys

action, directory = sys.argv[1:3]
out = Path(directory)
if action == "copy-dependency-logs":
    root = out / "mirror" / "roost-dataengine-it"
    for source in root.rglob("*.log"):
        dest = out / "dependency-logs" / source.relative_to(root)
        dest.parent.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(source, dest)
    sys.exit(0)

patterns = {
    "gift_saga_still": re.compile(r"gift saga still "),
    "alliance_saga_still": re.compile(r"alliance saga still "),
    "saga_unknown_or_waiting": re.compile(r"\bsaga\b.*(?:\b(?:unknown|waiting|pending)\b|未知|等待)", re.I),
    "nest_sync_timeout": re.compile(r"nest: sync timeout"),
    "world_stats_sync_timeout": re.compile(r"world_stats failed.*nest: sync timeout"),
    "world_record_enter_sync_timeout": re.compile(r"enter_game: world did not record the login.*nest: sync timeout"),
    "player_world_add_exp_sync_timeout": re.compile(r"add_exp failed.*nest: sync timeout"),
}
counts = dict.fromkeys(patterns, 0)
matches = []
replay = []
sources = [out / "robot.log", *sorted((out / "logs").glob("*.log"))]
for source in sources:
    if not source.exists():
        continue
    for number, line in enumerate(source.read_text(errors="replace").splitlines(), 1):
        # 启动日志中的 pending_inputs:0 是空分区证据，不是“仍在等待”。
        try:
            record = json.loads(line)
        except json.JSONDecodeError:
            record = None
        if isinstance(record, dict) and record.get("msg") == "saga: partition replayed":
            replay.append({"file": str(source.relative_to(out)), "line": number,
                           **{k: record.get(k) for k in ("partition", "sagas", "messages", "pending_inputs", "last_event")}})
        matched = [name for name, pattern in patterns.items() if pattern.search(line)]
        for name in matched:
            counts[name] += 1
        if matched:
            matches.append({"file": str(source.relative_to(out)), "line": number, "matches": matched, "text": line})
(out / "diagnostic-matches.json").write_text(json.dumps(matches, ensure_ascii=False, indent=2) + "\n")
report = json.loads((out / "robot.json").read_text()) if (out / "robot.json").exists() else {}
summary = {
    "exit_code": int(sys.argv[3]),
    "unit": "whole demo scenarios; count and scenario latency are not message TPS or request latency",
    "state": report.get("state", "not_reported"),
    "stop_reason": report.get("stop_reason"),
    "final_error": report.get("error"),
    "final_error_matches": [name for name, pattern in patterns.items() if pattern.search(report.get("error", ""))],
    "count": report.get("count"),
    "stats": report.get("stats"),
    "quantiles_ms": report.get("quantiles_ms"),
    "thresholds": report.get("thresholds"),
    "logged_occurrences": counts,
    "startup_saga_replay": replay,
    "log_files_scanned": [str(p.relative_to(out)) for p in sources if p.exists()],
    "observation_limit": "Robot JSON supplies aggregate stats and a run-level final error, not individual robot error details. Retry nodes do not log every intermediate Saga status. Logged counts are occurrences, not total polls, outstanding Sagas, or unique affected robots. WorldStats and RecordEnter address World; AddExp locks Player and World together, so its timeout does not isolate which lock caused delay. Other nest timeouts are not attributed to World without source/context evidence.",
}
(out / "summary.json").write_text(json.dumps(summary, ensure_ascii=False, indent=2) + "\n")
print(json.dumps(summary, ensure_ascii=False))
