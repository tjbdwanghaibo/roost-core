"""把 soak TSV 按时间段求均值（排除 vmmap 挂起窗口：物理均频 < 49.5Hz）。
用法：python3 -I segments.py <go.tsv> a-b [a-b ...]"""
import csv
import statistics as st
import sys

rows = list(csv.DictReader(open(sys.argv[1]), delimiter='\t'))
keys = [k for k in rows[0] if k.endswith(('_hz', '_ms', '_mean')) and k != 'min_room_hz']


def seg(a, b):
    sel = [r for r in rows if a < float(r['elapsed_s']) <= b and float(r['physics_hz']) >= 49.5]
    out = {k: round(st.mean(float(r[k]) for r in sel), 3) for k in keys}
    out['n'] = len(sel)
    return out


for spec in sys.argv[2:]:
    a, b = map(float, spec.split('-'))
    print(spec, seg(a, b))
