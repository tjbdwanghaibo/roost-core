# 用法：m15-summarize.py <ROOST_PERF_OUTPUT_ROOT> <run>...（目录旁要有 run-s11 包装写的 <run>.load）；每轮一行摘要（负载、机器人失败、进入路径、Base 指标）。
import sys, re, json, pathlib, collections, subprocess
root = pathlib.Path(sys.argv[1])
ET = str(pathlib.Path(__file__).resolve().parent.parent / 'login-burst-20261115' / 'enter-timing.py')
names = ['entitysync_interest_budget_stop_total', 'entitysync_interest_log_backlog', 'entitysync_sessions_lost_total',
         'player_tcp_push_total', 'player_tcp_dispatch_timeout_total',
         'nest_handler_lock_hold_count{handler="handlerRecordEnter"}', 'nest_handler_lock_hold_sum_nanos{handler="handlerRecordEnter"}']
for run in sys.argv[2:]:
    load = (root / f'{run}.load').read_text().split('\n')
    loads = [re.search(r'\{ ([\d.]+)', l).group(1) for l in load if 'loadavg' in l]
    r = root / run / 'robot'
    log = (r / 'robot.log').read_text(errors='replace') if (r / 'robot.log').exists() else ''
    m = re.search(r'error_rate = ([\d.]+)', log)
    err = m.group(1) if m else '0'
    cats = collections.Counter()
    for line in re.findall(r'robot action [a-z_]+: [^"\n]*', log):
        k = re.sub(r'player \d+', 'player N', line)
        k = re.sub(r'\(.*', '', k)[:90]
        cats[k] += 1
    entered = collections.Counter(re.findall(r'entered/left the box (\d+)', log))
    vals = collections.defaultdict(float)
    for f in r.glob('metrics-9*-after.txt'):
        for l in f.read_text().splitlines():
            for n in names:
                if l.startswith(n + ' '):
                    vals[n] += float(l.split()[-1])
    hold = vals[names[6]] / vals[names[5]] / 1e6 if vals[names[5]] else 0
    et = subprocess.run(['python3', ET, str(r / 'logs/log/game-1000.20261115.log')], capture_output=True, text=True).stdout.splitlines()
    print(f'## {run}  load {"→".join(loads)}  error_rate {err}')
    print('  enter:', et[0] if et else '-', '|', ' '.join(et[1].split()[-6:]) if len(et) > 1 else '')
    for k, v in cats.most_common(4): print(f'  {v:4d} {k}')
    if entered: print('  spread failures by entered-the-box count:', dict(sorted(entered.items(), key=lambda x: int(x[0]))))
    print(f'  budget_stop {vals[names[0]]:.0f} backlog {vals[names[1]]:.0f} lost {vals[names[2]]:.0f} push {vals[names[3]]:.0f} dispatch_timeout {vals[names[4]]:.0f} RecordEnter n={vals[names[5]]:.0f} hold_avg={hold:.3f}ms')
