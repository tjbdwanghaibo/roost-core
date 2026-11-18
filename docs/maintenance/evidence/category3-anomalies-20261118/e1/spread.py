"""每个二进制 / 每个 workers 档的逐轮 sec/op 离散度。用法：python3 -I spread.py <dir>..."""
import re
import statistics as st
import sys

for d in sys.argv[1:]:
    for side in ('base', 'head', 'nobr'):
        for w in ('1', '4'):
            xs = [float(m.group(1)) for line in open(f'{d}/raw-{side}.txt')
                  if (m := re.search(rf'workers={w}-\d+\s+\d+\s+([\d.]+) ns/op', line))]
            m = st.mean(xs)
            print(f'{d.rsplit("/", 1)[-1]} {side} w={w} n={len(xs)} median={st.median(xs):.0f} '
                  f'min={min(xs):.0f} max={max(xs):.0f} cv={100 * st.stdev(xs) / m:.1f}%')
