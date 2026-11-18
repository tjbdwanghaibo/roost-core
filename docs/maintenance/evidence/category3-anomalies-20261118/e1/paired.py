"""同一轮内配对：workers=4 下 head 比 base、head 比 nobr 慢的轮数（第 i 行即第 i 轮，三者同轮相邻运行）。"""
import re
import statistics as st
import sys

d = sys.argv[1]


def series(side, w='4'):
    return [float(m.group(1)) for line in open(f'{d}/raw-{side}.txt')
            if (m := re.search(rf'workers={w}-\d+\s+\d+\s+([\d.]+) ns/op', line))]


for w in ('1', '4'):
    b, h, n = series('base', w), series('head', w), series('nobr', w)
    for name, x, y in (('head/base', h, b), ('head/nobr', h, n), ('nobr/base', n, b)):
        r = [a / c for a, c in zip(x, y)]
        print(f'w={w} {name}: slower in {sum(v > 1 for v in r)}/{len(r)} rounds, median ratio {st.median(r):.3f}')
