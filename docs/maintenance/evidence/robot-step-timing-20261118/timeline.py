#!/usr/bin/env python3
"""服务端确认日志按“运行开始后第几秒”分桶：看各类异步效果每秒完成多少、是否排成一条长尾。"""
import collections
import json
import sys
from pathlib import Path

d = Path(sys.argv[1])
t0 = json.load(open(d / "robot.json"))["started_at_ms"]
series = [
    ("logs/game.log", "mailbox: level reward mail confirmed", "lvl_mail"),
    ("logs/game.log", "mailbox: send confirmed", "mail_send"),
    ("logs/alliance.log", "alliance: founding confirmed", "found"),
    ("logs/alliance.log", "alliance: donate confirmed", "donate"),
    ("logs/alliance.log", "alliance: reward claim confirmed", "claim"),
    ("logs/game.log", "matchmaker: match formed", "match"),
]
rows = {}
for file, msg, label in series:
    counter = collections.Counter()
    for line in (d / file).read_text(errors="replace").splitlines():
        try:
            record = json.loads(line)
        except json.JSONDecodeError:
            continue
        if record.get("msg") == msg:
            counter[int((record["server_time_ms"] - t0) / 1000)] += 1
    rows[label] = counter
print("sec " + " ".join(f"{label:>9}" for label in rows))
last = max((max(c) for c in rows.values() if c), default=0)
for second in range(last + 1):
    print(f"{second:3d} " + " ".join(f"{rows[label].get(second, 0):9d}" for label in rows))
