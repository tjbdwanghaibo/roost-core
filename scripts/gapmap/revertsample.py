#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""Promise-revert sampler ("承诺回退法" as a script).

For every guard of the shape

    if <cond> {
        return ..., <error>
    }

in a package's non-test files, neutralize the condition — `if (<cond>) && false {`
(the parentheses matter: `a || b && false` only disables `b`) — run the package
tests, and report the guards that stay GREEN: nobody tests that promise.

Usage: revertsample.py [--max N] [--timeout SEC] <package dir>...
Output: a Markdown table per package on stdout; the exit status is always 0 —
this is a map, not a gate. Every file is restored after each run; check
`git status` afterwards anyway (a neutralized "empty directory" guard once
created a stray directory).

The canonical copy lives in roost-core/scripts/gapmap; the other repositories
carry byte-identical copies.
"""
import os, re, shutil, subprocess, sys

GUARD = re.compile(r'^(\s*)if (.+) \{\s*$')
ERRORISH = re.compile(r'fmt\.Errorf|errors\.New|\bErr[A-Z]\w*|errors\.Join|, err$|\berr\b')

# Generated files (*_gen.go) are one template instantiated per package; their
# guards are pinned once where the template lives, so sampling them in every
# package only repeats the same rows. They are counted and reported, not sampled.
GEN_SKIPPED = {}

def guards_in(pkgdir):
    out = []
    for f in sorted(os.listdir(pkgdir)):
        if not f.endswith('.go') or f.endswith('_test.go'):
            continue
        p = os.path.join(pkgdir, f)
        if f.endswith('_gen.go'):
            GEN_SKIPPED[pkgdir] = GEN_SKIPPED.get(pkgdir, 0) + len(_guards_in_file(p))
            continue
        out.extend(_guards_in_file(p))
    return out

def _guards_in_file(p):
        out = []
        lines = open(p, encoding='utf-8').read().split('\n')
        for i, l in enumerate(lines):
            m = GUARD.match(l)
            if not m or i + 1 >= len(lines):
                continue
            nxt = lines[i + 1].strip()
            if not nxt.startswith('return') or not ERRORISH.search(nxt):
                continue
            cond = m.group(2).strip()
            if cond == 'err != nil' or (cond.endswith('err != nil') and ':=' in cond):
                continue  # plain error propagation, not a promise of this package
            out.append((p, i, m.group(1), m.group(2), nxt))
        return out

def neutralize(indent, cond):
    if '; ' in cond and (':=' in cond.split('; ')[0] or '=' in cond.split('; ')[0]):
        init, rest = cond.split('; ', 1)
        return f"{indent}if {init}; ({rest}) && false {{"
    return f"{indent}if ({cond}) && false {{"

# 中和一条资源上限守卫（例如 actionflow 延后队列的 runaway / 步数上限）会让被测代码无界增长：
# 测试进程每秒涨数百 MB，几十秒吃满 CI runner 的内存，内核 OOM 连带 runner 进程，job 以
# “The runner has received a shutdown signal”、exit 143 结束，报告也没上传（2026-10-06 起每晚如此）。
# 所以测试二进制在地址空间上限下运行（go test -exec prlimit --as=…）：失控的那一次以
# “out of memory” 失败、按 red 记（中和后测试变红，说明这条守卫有测试），其余取样照常继续。
# 上限只套在测试二进制上，不限制编译与链接；GAPMAP_TEST_AS_BYTES 可覆盖，0 表示不设。
# 没有 prlimit 的机器（macOS）不设上限并在 stderr 提示一次——那里 RLIMIT_AS 本来也不生效。
TEST_AS_BYTES = int(os.environ.get('GAPMAP_TEST_AS_BYTES', str(6 << 30)))
_warned_no_limit = False

def exec_wrapper():
    global _warned_no_limit
    if TEST_AS_BYTES <= 0:
        return []
    prlimit = shutil.which('prlimit')
    if prlimit is None:
        if not _warned_no_limit:
            print('revertsample: prlimit not found; test binaries run without an address-space limit', file=sys.stderr)
            _warned_no_limit = True
        return []
    return ['-exec', f'{prlimit} --as={TEST_AS_BYTES} --']

def run_tests(pkgdir, timeout):
    try:
        r = subprocess.run(['go', 'test', '-count=1', *exec_wrapper(), './' + os.path.relpath(pkgdir)],
                           capture_output=True, text=True, timeout=timeout)
    except subprocess.TimeoutExpired:
        return 'HANG'
    out = r.stdout + r.stderr
    if 'build failed' in out or 'vet:' in out or out.startswith('# '):
        return 'skip'
    return 'red' if r.returncode != 0 else 'GREEN'

def sample(pkgdir, maxn, timeout):
    guards = guards_in(pkgdir)[:maxn]
    rows, green = [], 0
    for p, i, indent, cond, nxt in guards:
        orig = open(p, encoding='utf-8').read()
        lines = orig.split('\n')
        lines[i] = neutralize(indent, cond)
        open(p, 'w', encoding='utf-8').write('\n'.join(lines))
        try:
            res = run_tests(pkgdir, timeout)
        finally:
            open(p, 'w', encoding='utf-8').write(orig)
        if res == 'GREEN':
            green += 1
        rows.append((res, f"{os.path.relpath(p)}:{i + 1}", cond[:80], nxt[:60]))
    return guards, rows, green

def main(argv):
    maxn, timeout, pkgs = 40, 600, []
    it = iter(argv)
    for a in it:
        if a == '--max':
            maxn = int(next(it))
        elif a == '--timeout':
            timeout = int(next(it))
        else:
            pkgs.append(a)
    total_g, total_green = 0, 0
    for pkg in pkgs:
        if not os.path.isdir(pkg):
            continue
        guards, rows, green = sample(pkg, maxn, timeout)
        gen = GEN_SKIPPED.get(pkg, 0)
        if not guards:
            if gen:
                print(f"\n### `{pkg}` — 0 / 0 guards have no test (generated: {gen} guards in *_gen.go not sampled)\n")
            continue
        total_g += len(guards); total_green += green
        gen_note = f" (generated: {gen} guards in *_gen.go not sampled)" if gen else ""
        print(f"\n### `{pkg}` — {green} / {len(guards)} guards have no test{gen_note}\n")
        print("| result | guard | condition | returns |\n| --- | --- | --- | --- |")
        for res, where, cond, nxt in rows:
            if res == 'GREEN':
                print(f"| **{res}** | `{where}` | `{cond}` | `{nxt}` |")
        sys.stdout.flush()
    total_gen = sum(GEN_SKIPPED.values())
    print(f"\n**Total: {total_green} / {total_g} sampled guards have no test.**" + (f" Generated files: {total_gen} guards in *_gen.go not sampled (pinned once at the template)." if total_gen else ""))
    return 0

if __name__ == '__main__':
    sys.exit(main(sys.argv[1:]))
