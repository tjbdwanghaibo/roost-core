#!/usr/bin/env python3
"""汇总诊断补丁写出的 dispatch-<pid>.csv：AddExp 等在哪把锁上、World 链的占用。

用法：analyze_dispatch.py <dispatch-*.csv 所在目录>
"""
import csv, glob, os, statistics, sys
from collections import Counter, defaultdict

d = sys.argv[1]
target = None
for path in glob.glob(os.path.join(d, 'dispatch-*.csv')):
    with open(path) as f:
        if 'handlerAddExp' in f.read():
            target = path
            break
if target is None:
    raise SystemExit('no file with handlerAddExp')
rows = []
with open(target) as f:
    for r in csv.DictReader(f):
        if r['queue'] == '' or r['ended_us'] == '':
            continue
        for k in ('admitted_us', 'ready_us', 'started_us', 'ended_us', 'blocker_id'):
            r[k] = int(r[k])
        r['ids'] = [int(x.split(':')[1]) for x in r['targets'].split()]
        r['kinds'] = [int(x.split(':')[0]) for x in r['targets'].split()]
        rows.append(r)
print(f'file {os.path.basename(target)} rows {len(rows)}')
addexp = [r for r in rows if r['handler'] == 'handlerAddExp']
common = Counter(i for r in addexp for i in r['ids'])
world_id, n = common.most_common(1)[0]
assert n == len(addexp), (n, len(addexp))
kind_of = {}
for r in rows:
    for k, i in zip(r['kinds'], r['ids']):
        kind_of[i] = k
print(f'World id {world_id} kind {kind_of[world_id]}; AddExp jobs {len(addexp)}')

def q(v, p):
    v = sorted(v)
    return v[min(len(v) - 1, int(p * len(v)))] if v else 0

def ms(us):
    return us / 1000.0

def summary(name, vals):
    if not vals:
        return f'{name}: n=0'
    return f'{name}: n={len(vals)} mean={ms(statistics.mean(vals)):.1f}ms p50={ms(q(vals,.5)):.1f} p95={ms(q(vals,.95)):.1f} max={ms(max(vals)):.1f}'

# 1. AddExp 等待归因
blk = Counter()
for r in addexp:
    if not r['waited_pred'] == 'true':
        blk['no predecessor'] += 1
    elif r['blocker_id'] == world_id:
        blk['World (last releaser shared World id)'] += 1
    else:
        blk[f'Player/other (blocker kind {kind_of.get(r["blocker_id"], "?")}, handler {r["blocker_handler"]})'] += 1
print('\n## AddExp: who released it last')
for k, v in blk.most_common():
    print(f'  {v:5d}  {k}')
print(summary('AddExp dependency wait (ready-admitted)', [r['ready_us'] - r['admitted_us'] for r in addexp]))
print(summary('AddExp worker wait (started-ready)', [r['started_us'] - r['ready_us'] for r in addexp]))
print(summary('AddExp run (ended-started)', [r['ended_us'] - r['started_us'] for r in addexp]))
print(summary('AddExp admitted->ended', [r['ended_us'] - r['admitted_us'] for r in addexp]))
over5 = [r for r in addexp if r['ended_us'] - r['admitted_us'] > 5_000_000]
print(f'AddExp admitted->ended > 5s: {len(over5)}; started-admitted > 5s: {sum(1 for r in addexp if r["started_us"]-r["admitted_us"] > 5_000_000)}')

# 2. World 链
world = sorted([r for r in rows if world_id in r['ids']], key=lambda r: r['started_us'])
per = defaultdict(list)
for r in world:
    per[r['handler']].append(r['ended_us'] - r['started_us'])
print(f'\n## World chain: {len(world)} jobs')
for h, v in sorted(per.items(), key=lambda kv: -sum(kv[1])):
    print('  ' + summary(f'{h} run', v) + f' total={ms(sum(v)):.0f}ms')
# 忙碌区间：AddExp 第一次准入到最后一次结束
t0 = min(r['admitted_us'] for r in addexp)
t1 = max(r['ended_us'] for r in addexp)
inwin = [r for r in world if r['started_us'] >= t0 and r['ended_us'] <= t1]
busy = sum(r['ended_us'] - r['started_us'] for r in inwin)
print(f'AddExp window {ms(t1-t0):.0f}ms: World jobs {len(inwin)}, World run total {ms(busy):.0f}ms ({100*busy/(t1-t0):.0f}% busy)')
gaps = [b['started_us'] - a['ended_us'] for a, b in zip(inwin, inwin[1:])]
print(summary('gap between consecutive World jobs in window', gaps))
cnt = Counter(r['handler'] for r in inwin)
print('World jobs in window by handler:', dict(cnt))
# 3. 每秒：World 完成数与排队深度
print('\n## per second (relative to first AddExp admission): World completions / AddExp admitted / AddExp started')
sec = defaultdict(lambda: [0, 0, 0])
for r in world:
    if r['ended_us'] >= t0:
        sec[(r['ended_us'] - t0) // 1_000_000][0] += 1
for r in addexp:
    sec[(r['admitted_us'] - t0) // 1_000_000][1] += 1
    sec[(r['started_us'] - t0) // 1_000_000][2] += 1
for s in sorted(sec):
    if s > 40:
        break
    print(f'  t+{s:2d}s  world_done={sec[s][0]:4d}  addexp_admitted={sec[s][1]:4d}  addexp_started={sec[s][2]:4d}')
# 4. Player 侧：AddExp 的 Player 目标上还有谁
players = {i for r in addexp for i in r['ids'] if i != world_id}
pj = [r for r in rows if r['handler'] != 'handlerAddExp' and players.intersection(r['ids'])]
print(f'\n## other jobs on the AddExp players: {len(pj)}')
ov = 0
for r in addexp:
    pid = [i for i in r['ids'] if i != world_id][0]
    for o in pj:
        if pid in o['ids'] and o['started_us'] < r['started_us'] and o['ended_us'] > r['admitted_us']:
            ov += 1
            break
print(f'AddExp whose Player had another job overlapping its wait: {ov}')
# 5. 全队列：同时段所有作业 run 时间（判断是否 fsync 主导）
allrun = [r['ended_us'] - r['started_us'] for r in rows if t0 <= r['started_us'] <= t1]
print(summary('all jobs run in window', allrun))
