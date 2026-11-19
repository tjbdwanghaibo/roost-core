"""Reconcile the archived strict baseline only; no inference from elapsed time."""
import csv
import gzip
import json
import re
from collections import Counter
from pathlib import Path

base = Path(__file__).resolve().parents[1] / 'category3-batch2-20261118/item1-200burst-baseline'
run = json.loads((base / 'robot.json').read_text())
steps = {row['name']: row for row in run['steps']}
first = steps['10 add_exp']
# scene_alive sends exactly one further AddExp(1); a successful scene_alive checked its response.
follow = steps['57 scene_alive']
assert first['retry_attempts_max'] == follow['retry_attempts_max'] == follow['failed'] == 0
with gzip.open(base / 'game-dispatch.csv.gz', 'rt') as f:
    by_targets = Counter(row['targets'] for row in csv.DictReader(f) if row['handler'] == 'handlerAddExp')
assert Counter(by_targets.values()) == {1: first['failed'], 2: follow['ok']}
metrics = (base / 'game-metrics-after.txt').read_text()

def count(line):
    values = re.findall('^' + re.escape(line) + r' (\d+)$', metrics, re.M)
    assert len(values) == 1, line
    return int(values[0])

ok = count('nest_dispatch_total{handler="handlerAddExp",result="ok",type="Multi"}')
error = count('nest_dispatch_total{handler="handlerAddExp",result="error",type="Multi"}')
held = count('nest_handler_lock_hold_count{handler="handlerAddExp"}')
assert ok + error == sum(by_targets.values()) and held == ok
late_success = ok - first['ok'] - follow['ok']
assert late_success + error == first['failed']
print(f"AddExp calls={ok+error}, players={len(by_targets)}, first ack={first['ok']}, later ack={follow['ok']}")
print(f'server ok={ok}, error={error}, lock hold count={held}')
print(f"among {first['failed']} first-step failures: committed={late_success}, no handler write={error}")
