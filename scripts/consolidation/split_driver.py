#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""Move a client implementation out of a core contract package into <pkg>/driver.

  split_driver.py <pkg> <alias> <file> [<file> ...]

Files are `git mv`ed to <pkg>/driver (an `_impl` infix is dropped), their package
clause becomes `driver`, and references to the contract package's top-level
identifiers are qualified with <alias> (an import is added). Unexported contract
identifiers the moved files still use are reported for the operator to export.
"""
import os, re, subprocess, sys, glob

def sh(cmd, check=True):
    r = subprocess.run(cmd, capture_output=True, text=True)
    if check and r.returncode:
        sys.stderr.write(r.stdout + r.stderr); sys.exit(f"failed: {cmd}")
    return r

def top_level_names(files):
    names = set()
    for f in files:
        s = open(f, encoding='utf-8').read()
        for m in re.finditer(r'^(?:func |type |var |const )([A-Za-z_]\w*)', s, re.M):
            names.add(m.group(1))
        for m in re.finditer(r'^(?:var|const|type) \(\n(.*?)^\)', s, re.M | re.S):
            for line in m.group(1).split('\n'):
                mm = re.match(r'\s*([A-Za-z_]\w*)\s*[=\s]', line)
                if mm and not line.strip().startswith('//'):
                    names.add(mm.group(1))
    return names

def qualify(body, names, alias):
    """Prefix bare references to contract identifiers with alias.

    Skips selectors (x.Name) but not variadics (...Name), skips struct-literal
    keys (Name: value) but not switch case labels (case Name:)."""
    if not names:
        return body
    alt = '|'.join(sorted(map(re.escape, names), key=len, reverse=True))
    body = re.sub(r'\bcase (' + alt + r')\b', lambda m: f'case {alias}.{m.group(1)}', body)
    body = re.sub(r'(?<![\w])(?<!(?<!\.)\.)(' + alt + r')\b(?!\s*:(?!=))', lambda m: f'{alias}.{m.group(1)}', body)
    return body

def main(argv):
    pkg, alias, files = argv[0], argv[1], argv[2:]
    target = f'{pkg}/driver'
    os.makedirs(target, exist_ok=True)
    moved = []
    for name in files:
        src = f'{pkg}/{name}'
        dst = f'{target}/{name.replace("_impl", "")}'
        sh(['git', 'mv', src, dst]); moved.append(dst)
    contract = [f for f in glob.glob(f'{pkg}/*.go')]
    names = top_level_names([f for f in contract if not f.endswith('_test.go')])
    test_names = top_level_names([f for f in contract if f.endswith('_test.go')])
    exported = {n for n in names if n[0].isupper()}
    unexported = {n for n in names if not n[0].isupper()}
    corepath = f'github.com/tjbdwanghaibo/roost-core/{pkg}'
    used_unexported = set()
    for f in moved:
        s = open(f, encoding='utf-8').read()
        s = re.sub(rf'^package {pkg}(_test)?$', lambda m: 'package driver' + (m.group(1) or ''), s, flags=re.M)
        # qualify exported contract identifiers: not after '.', not followed by ':' (struct keys)
        body_start = s.find('\n)\n', s.find('import (')) if 'import (' in s else 0
        head, body = s[:body_start], s[body_start:]
        body = qualify(body, exported, alias)
        s = head + body
        for n in unexported | test_names:
            if re.search(rf'(?<![\.\w]){re.escape(n)}\b', body):
                used_unexported.add(n)
        if f'"{corepath}"' not in s and alias + '.' in body:
            if 'import (' in s:
                s = s.replace('import (\n', f'import (\n\t{alias} "{corepath}"\n', 1)
            else:
                s = re.sub(r'^(package [^\n]+\n)', rf'\1\nimport {alias} "{corepath}"\n', s, count=1, flags=re.M)
        open(f, 'w', encoding='utf-8').write(s)
    sh(['gofmt', '-w'] + moved)
    print(f'moved {len(moved)} files to {target}; qualified {len(exported)} exported contract names with {alias}.')
    if used_unexported:
        print('UNEXPORTED contract identifiers still referenced:', sorted(used_unexported))
    # Iterate with the compiler: names the regex scan missed (declared in
    # grouped blocks, aliases, ...) show up as `undefined: X`; qualify them if
    # the contract package declares them, drop unused aliases.
    for _ in range(8):
        r = sh(['go', 'build', '-gcflags=-e', f'./{target}/'], check=False)
        r2 = sh(['go', 'vet', f'./{target}/'], check=False)
        out = r.stdout + r.stderr + r2.stdout + r2.stderr
        if r.returncode == 0 and r2.returncode == 0:
            break
        und = set(re.findall(r'undefined: ([A-Z]\w*)', out))
        contract_src = '\n'.join(open(f, encoding='utf-8').read() for f in glob.glob(f'{pkg}/*.go'))
        fixable = {n for n in und if re.search(rf'^(?:func |type |var |const |\t)({re.escape(n)})\b', contract_src, re.M)}
        unused = re.findall(r'([^\s:]+\.go):\d+:\d+: "' + re.escape(corepath) + r'" imported as ' + alias + ' and not used', out)
        for f in moved:
            s = open(f, encoding='utf-8').read(); o = s
            if fixable:
                s = qualify(s, fixable, alias)
            if any(f.endswith(os.path.basename(u)) for u in unused):
                s = re.sub(rf'^\t{alias} "{re.escape(corepath)}"\n', '', s, flags=re.M)
            if s != o: open(f, 'w', encoding='utf-8').write(s)
        if not fixable and not unused:
            print('remaining errors:'); print(out[:3000]); break
    sh(['gofmt', '-w'] + moved)
    r = sh(['go', 'build', './...'], check=False)
    print('go build ./...:', 'OK' if r.returncode == 0 else 'FAIL')
    if r.returncode: print((r.stdout + r.stderr)[:3000])
    return 0

if __name__ == '__main__':
    sys.exit(main(sys.argv[1:]))
