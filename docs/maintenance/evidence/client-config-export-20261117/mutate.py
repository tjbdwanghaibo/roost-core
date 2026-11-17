#!/usr/bin/env python3
# usage: mutate.py LABEL FILE OLD NEW RUNREGEX PKG  (OLD/NEW are python string literals, evaluated)
import sys, subprocess, os
label, path, old, new, run, pkg = sys.argv[1:7]
old = eval(old); new = eval(new)
root = '/Users/whb/roost/roost-core'
full = os.path.join(root, path)
src = open(full).read()
if src.count(old) != 1:
    print(f"{label}: SETUP-ERROR old found {src.count(old)} times"); sys.exit(2)
open(full, 'w').write(src.replace(old, new))
try:
    env = dict(os.environ, GOWORK='off')
    r = subprocess.run(['go', 'test', '-count=1', '-run', run, './' + pkg], cwd=root, env=env, capture_output=True, text=True, timeout=900)
    out = r.stdout + r.stderr
    status = 'GREEN(mutation survived!)' if r.returncode == 0 else 'RED'
    if 'build failed' in out or '[setup failed]' in out:
        status = 'BUILD-FAIL (not counted)'
    print(f"{label}: {status} -run '{run}'")
    lines = [l for l in out.splitlines() if ('--- FAIL' in l or '.go:' in l) and 'ok ' not in l]
    for l in lines[:6]:
        print('    ' + l.strip()[:400])
finally:
    open(full, 'w').write(src)
