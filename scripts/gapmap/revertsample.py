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
import os, re, subprocess, sys

GUARD = re.compile(r'^(\s*)if (.+) \{\s*$')
ERRORISH = re.compile(r'fmt\.Errorf|errors\.New|\bErr[A-Z]\w*|errors\.Join|, err$|\berr\b')

def guards_in(pkgdir):
    out = []
    for f in sorted(os.listdir(pkgdir)):
        if not f.endswith('.go') or f.endswith('_test.go'):
            continue
        p = os.path.join(pkgdir, f)
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

def run_tests(pkgdir, timeout):
    try:
        r = subprocess.run(['go', 'test', '-count=1', './' + os.path.relpath(pkgdir)],
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
        if not guards:
            continue
        total_g += len(guards); total_green += green
        print(f"\n### `{pkg}` — {green} / {len(guards)} guards have no test\n")
        print("| result | guard | condition | returns |\n| --- | --- | --- | --- |")
        for res, where, cond, nxt in rows:
            if res == 'GREEN':
                print(f"| **{res}** | `{where}` | `{cond}` | `{nxt}` |")
        sys.stdout.flush()
    print(f"\n**Total: {total_green} / {total_g} sampled guards have no test.**")
    return 0

if __name__ == '__main__':
    sys.exit(main(sys.argv[1:]))
