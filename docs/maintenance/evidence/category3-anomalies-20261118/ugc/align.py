"""把 soak TSV 的每个 15 秒窗口与 `pmset -g log` 的显示器开 / 关事件对齐，按显示器状态分段求均值。
用法：python3 -I align.py <soak.tsv> <display-events.txt>
display-events.txt 是 `pmset -g log | grep 'Display is turned'` 的原样输出（本机时区 +0800）。"""
import csv
import datetime as dt
import statistics as st
import sys

rows = list(csv.DictReader(open(sys.argv[1]), delimiter='\t'))
events = []
for line in open(sys.argv[2]):
    stamp = dt.datetime.strptime(line[:25], '%Y-%m-%d %H:%M:%S %z').timestamp()
    events.append((stamp, 'on' if 'turned on' in line else 'off'))
events.sort()


def state(at):
    s = 'unknown'
    for stamp, kind in events:
        if stamp <= at:
            s = kind
    return s


cols = ['delivered_hz', 'loop_mean', 'submit_mean', 'poll_mean', 'timer_late_mean', 'tick_interval_p99_ms', 'step_p99_ms', 'loadavg1']
print('elapsed_s\tdisplay(窗口起点→终点)\t' + '\t'.join(cols))
groups = {}
for r in rows:
    end = float(r['unix_s'])
    begin = end - 15
    a, b = state(begin), state(end)
    label = a if a == b else f'{a}->{b}'
    print(r['elapsed_s'] + '\t' + label + '\t' + '\t'.join(r[c] for c in cols))
    if a == b and float(r['elapsed_s']) > 15:
        groups.setdefault(a, []).append(r)
print()
for name, sel in groups.items():
    print(name, f'n={len(sel)}', {c: round(st.mean(float(r[c]) for r in sel), 3) for c in cols})
