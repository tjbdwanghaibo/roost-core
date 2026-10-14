#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""把 nightly-gapmap 各组的 gapmap-report.md 合成一份。

全量取样串行约 120 分钟，逼近单 job 180 分钟上限，所以 workflow 按组（scripts/gapmap.sh
--group）拆成 matrix 并行。对外仍只发布一份 gapmap-report.md：表头一行、各包小节按包路径
排序（与串行时 go list 的顺序一致）、末尾一行总计（各组总计相加）。

Usage: merge.py --groups "framework infra codegen rest" --out gapmap-report.md DIR
DIR 下每组一个子目录 gapmap-part-<group>/，里面是该组的 gapmap-report.md（actions/
download-artifact 不合并时的布局）。

缺组或某组报告没有总计行（该组 job 中途失败、报告只写了一半）时，照样合并其余内容，
在总计前写明哪些组不完整，并以退出码 1 结束：不完整的地图不能看起来像完整的。
"""
import os, re, sys

HEADER = re.compile(r'^# Gap map — (\S+) — (\S+)$')
SAMPLED = re.compile(r'^Guards whose neutralization .* Sampled up to (\d+) per package\.$')
SECTION = re.compile(r'^### `([^`]+)`')
TOTAL = re.compile(r'^\*\*Total: (\d+) / (\d+) sampled guards have no test\.\*\*'
                   r'(?: Generated files: (\d+) guards in \*_gen\.go not sampled)?')


def parse(path):
    """返回 (sha, date, max, sections{pkg: text}, total(green, guards, gen) 或 None)。"""
    sha = date = maxn = None
    sections, total = {}, None
    current, buf = None, []

    def flush():
        if current is not None:
            while buf and not buf[-1].strip():
                buf.pop()
            sections[current] = '\n'.join(buf)

    for line in open(path, encoding='utf-8').read().split('\n'):
        m = HEADER.match(line)
        if m:
            sha, date = m.group(1), m.group(2)
            continue
        m = SAMPLED.match(line)
        if m:
            maxn = m.group(1)
            continue
        m = TOTAL.match(line)
        if m:
            flush()
            current, buf = None, []
            total = (int(m.group(1)), int(m.group(2)), int(m.group(3) or 0))
            continue
        m = SECTION.match(line)
        if m:
            flush()
            current, buf = m.group(1), [line]
            continue
        if current is not None:
            buf.append(line)
    flush()
    return sha, date, maxn, sections, total


def main(argv):
    groups, out, root = [], 'gapmap-report.md', None
    it = iter(argv)
    for a in it:
        if a == '--groups':
            groups = next(it).split()
        elif a == '--out':
            out = next(it)
        else:
            root = a
    if not groups or root is None:
        print(__doc__, file=sys.stderr)
        return 2

    shas, dates, maxes, sections = set(), [], set(), {}
    green = guards = gen = 0
    incomplete = []
    for group in groups:
        path = os.path.join(root, f'gapmap-part-{group}', 'gapmap-report.md')
        if not os.path.isfile(path):
            incomplete.append(f'{group} (no report)')
            continue
        sha, date, maxn, secs, total = parse(path)
        if sha:
            shas.add(sha)
        if date:
            dates.append(date)
        if maxn:
            maxes.add(maxn)
        for pkg, text in secs.items():
            if pkg in sections:
                # 同一个包出现在两组里说明分组规则重叠，合并结果会重复计数。
                incomplete.append(f'{group} (package {pkg} also sampled by another group)')
            sections[pkg] = text
        if total is None:
            incomplete.append(f'{group} (report has no total line: the job stopped early)')
            continue
        green += total[0]; guards += total[1]; gen += total[2]
    if len(shas) > 1:
        incomplete.append('groups sampled different commits: ' + ', '.join(sorted(shas)))

    sha = ','.join(sorted(shas)) or 'unknown'
    date = min(dates) if dates else 'unknown'
    maxn = ','.join(sorted(maxes)) or '?'
    lines = [f'# Gap map — {sha} — {date}', '',
             f'Guards whose neutralization leaves the package tests green. Sampled up to {maxn} per package.']
    for pkg in sorted(sections):
        lines += ['', sections[pkg]]
    if incomplete:
        lines += ['', '**Incomplete: ' + '; '.join(incomplete) + '. The total below covers the complete groups only.**']
    total_line = f'**Total: {green} / {guards} sampled guards have no test.**'
    if gen:
        total_line += f' Generated files: {gen} guards in *_gen.go not sampled (pinned once at the template).'
    lines += ['', total_line, '']
    with open(out, 'w', encoding='utf-8') as f:
        f.write('\n'.join(lines))
    if incomplete:
        print('gapmap merge: incomplete — ' + '; '.join(incomplete), file=sys.stderr)
        return 1
    return 0


if __name__ == '__main__':
    sys.exit(main(sys.argv[1:]))
