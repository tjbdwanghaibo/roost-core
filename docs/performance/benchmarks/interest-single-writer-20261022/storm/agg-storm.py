# 汇总登录风暴：压测端每条消息的往返（直方图估 p50/p95/p99，桶上界）、game 进程 handler 均值 / 最大、Interest 应用与结算延迟。
import re, sys, glob, os, json
G = sys.argv[1]
def parse(path):
    out = []
    for l in open(path):
        if l.startswith('#') or not l.strip(): continue
        m = re.match(r'([a-zA-Z_:]+)(\{[^}]*\})?\s+(\S+)', l)
        if not m: continue
        labels = dict(re.findall(r'(\w+)="([^"]*)"', m.group(2) or ''))
        out.append((m.group(1), labels, float(m.group(3))))
    return out
def quant(rows, name, sel, q):
    b = sorted(((float(l['le']) if l['le'] != '+Inf' else float('inf')), v) for n, l, v in rows if n == name + '_bucket' and all(l.get(k) == x for k, x in sel.items()))
    if not b: return None
    total = b[-1][1]
    for le, v in b:
        if v >= q * total: return le * 1000
def get(rows, name, sel):
    for n, l, v in rows:
        if n == name and all(l.get(k) == x for k, x in sel.items()): return v
res = []
for f in sorted(glob.glob(os.path.join(G, 'storm', '*-r*.robot.metrics'))):
    run = os.path.basename(f).split('.')[0]
    rob = parse(f); game = parse(f.replace('.robot.metrics', '.game.metrics'))
    r = {'run': run}
    for msg, name in (('10002', 'enter_game'), ('10024', 'scene_ready'), ('10025', 'scene_view')):
        for q in (0.5, 0.95, 0.99):
            r[f'{name}_p{int(q*100)}_ms'] = quant(rob, 'robot_session_call', {'msg': msg, 'result': 'ok'}, q)
        r[f'{name}_n'] = get(rob, 'robot_session_call_count', {'msg': msg, 'result': 'ok'})
    for h in ('handlerEnterScene', 'handlerRecordEnter'):
        c = get(game, 'nest_dispatch_cost_count', {'handler': h, 'result': 'ok'}) or 0
        s = get(game, 'nest_dispatch_cost_sum_nanos', {'handler': h, 'result': 'ok'}) or 0
        mx = get(game, 'nest_dispatch_cost_max_nanos', {'handler': h, 'result': 'ok'}) or 0
        r[h] = {'n': c, 'mean_ms': s / c / 1e6 if c else None, 'max_ms': mx / 1e6}
    c = get(game, 'player_tcp_dispatch_duration_count', {}) or 0; s = get(game, 'player_tcp_dispatch_duration_sum_nanos', {}) or 0
    r['player_tcp_dispatch'] = {'n': c, 'mean_ms': s / c / 1e6 if c else None, 'max_ms': (get(game, 'player_tcp_dispatch_duration_max_nanos', {}) or 0) / 1e6}
    for m in ('entitysync_interest_apply_duration', 'entitysync_interest_settle_delay', 'entitysync_flush_duration'):
        c = get(game, m + '_count', {}) or 0; s = get(game, m + '_sum_nanos', {}) or 0
        r[m] = {'n': c, 'mean_ms': s / c / 1e6 if c else None, 'p99_ms': quant(game, m, {}, 0.99)}
    res.append(r)
with open(os.path.join(G, 'storm', 'storm-runs.jsonl'), 'w') as f:
    for r in res: f.write(json.dumps(r) + '\n')
for r in res:
    print(r['run'], 'enter_game p50/p95/p99', r['enter_game_p50_ms'], r['enter_game_p95_ms'], r['enter_game_p99_ms'], '| scene_view p99', r['scene_view_p99_ms'],
          '| EnterScene mean/max %.2f/%.2f' % (r['handlerEnterScene']['mean_ms'] or 0, r['handlerEnterScene']['max_ms']),
          '| RecordEnter mean/max %.2f/%.2f' % (r['handlerRecordEnter']['mean_ms'] or 0, r['handlerRecordEnter']['max_ms']),
          '| tcp dispatch mean/max %.2f/%.2f' % (r['player_tcp_dispatch']['mean_ms'] or 0, r['player_tcp_dispatch']['max_ms']),
          '| apply mean/p99', r['entitysync_interest_apply_duration']['mean_ms'], r['entitysync_interest_apply_duration']['p99_ms'],
          '| flush p99', r['entitysync_flush_duration']['p99_ms'])
