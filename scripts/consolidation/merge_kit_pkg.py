#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""Merge one roost-kit package into roost-core with history (consolidation P2).

  merge_kit_pkg.py <pkg> [--alias fnats,rediscore] [--exclude f1,f2] [--delete f3]
                   [--replace old=new ...] [--target core/subdir] [--package newname]

Steps: git subtree split in ../roost-kit -> git subtree add into core staging/<pkg>
-> move files into <target> (clashing names get an _impl suffix; excluded Mod glue
is dropped, it stays in kit) -> remove the self-import alias -> gofmt -> go mod tidy
-> build/vet/test. The commit is left to the operator after inspection.
"""
import os, re, subprocess, sys, shutil

def sh(cmd, cwd=None, check=True):
    r = subprocess.run(cmd, cwd=cwd, shell=isinstance(cmd, str), capture_output=True, text=True)
    if check and r.returncode:
        sys.stderr.write(r.stdout + r.stderr); sys.exit(f"failed: {cmd}")
    return r

def main(argv):
    pkg = argv[0]; aliases = []; exclude = []; delete = []; replace = []; target = None; newpkg = None
    it = iter(argv[1:])
    for a in it:
        if a == '--alias': aliases += next(it).split(',')
        elif a == '--exclude': exclude = next(it).split(',')
        elif a == '--delete': delete = next(it).split(',')
        elif a == '--replace': replace.append(next(it).split('=', 1))
        elif a == '--target': target = next(it)
        elif a == '--package': newpkg = next(it)
    target = target or pkg
    core = os.getcwd(); kit = os.path.abspath(os.path.join(core, '..', 'roost-kit'))
    branch = f'split/{pkg.replace("/", "-")}'
    sh(['git', 'branch', '-D', branch], cwd=kit, check=False)
    sh(['git', 'subtree', 'split', f'--prefix={pkg}', '-b', branch], cwd=kit)
    staging = f'staging/{pkg}'
    msg = f'P2：{pkg} 自 roost-kit 带历史并入（收敛方案）\n\nCo-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>'
    sh(['git', 'subtree', 'add', f'--prefix={staging}', kit, branch, '-m', msg], cwd=core)
    os.makedirs(target, exist_ok=True)
    moved = []
    for name in sorted(os.listdir(staging)):
        src = f'{staging}/{name}'
        if os.path.isdir(src):
            sh(['git', 'rm', '-rq', src], cwd=core); print(f'dropped subdir {src} (separate batch)'); continue
        if name in exclude or name in delete:
            sh(['git', 'rm', '-q', src], cwd=core); print(f'{"kept in kit" if name in exclude else "deleted"}: {name}'); continue
        dst = f'{target}/{name}'
        if os.path.exists(dst):
            stem, ext = name[:-3], '.go'
            stem = stem[:-5] + '_impl_test' if stem.endswith('_test') else stem + '_impl'
            dst = f'{target}/{stem}{ext}'; print(f'name clash: {name} -> {os.path.basename(dst)}')
        sh(['git', 'mv', src, dst], cwd=core); moved.append(dst)
    if os.path.isdir('staging'):
        shutil.rmtree('staging', ignore_errors=True)
    corepath = f'github.com/tjbdwanghaibo/roost-core/{target}'
    for f in moved:
        s = open(f, encoding='utf-8').read(); orig = s
        for alias in aliases:
            s = re.sub(rf'^\s*{alias} "{re.escape(corepath)}"\n', '', s, flags=re.M)
            s = re.sub(rf'\b{alias}\.', '', s)
        for old, new in replace:
            s = s.replace(old, new)
        if newpkg:
            s = re.sub(r'^package (\w+?)(_test)?$', lambda m: f'package {newpkg}{m.group(2) or ""}', s, count=1, flags=re.M)
        if s != orig:
            open(f, 'w', encoding='utf-8').write(s)
    sh(['gofmt', '-w'] + moved, cwd=core)
    sh(['go', 'mod', 'tidy'], cwd=core)
    print(sh(['git', 'diff', '--stat', '--', 'go.mod'], cwd=core).stdout.strip() or 'go.mod unchanged')
    for cmd in (['go', 'build', './...'], ['go', 'vet', f'./{target}/'], ['go', 'vet', '-tags', 'integration', f'./{target}/'],
                ['go', 'test', '-count=1', f'./{target}/', '.']):
        r = sh(cmd, cwd=core, check=False)
        print(('OK  ' if r.returncode == 0 else 'FAIL') + ' ' + ' '.join(cmd))
        if r.returncode:
            print((r.stdout + r.stderr)[-4000:]); return 1
    return 0

if __name__ == '__main__':
    sys.exit(main(sys.argv[1:]))
