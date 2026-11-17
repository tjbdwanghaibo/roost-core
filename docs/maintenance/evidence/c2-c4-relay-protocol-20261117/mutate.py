"""变异：把 file 里的 old 换成 new，跑一条命令，打印结果尾部，再恢复原文件。
用法：python3 mutate.py <repo> <file> <old-file> <new-file> <cmd...>
old / new 从文件读（避免 shell 转义）。"""
import subprocess, sys, os
repo, path, old_f, new_f = sys.argv[1:5]
cmd = sys.argv[5:]
old = open(old_f).read(); new = open(new_f).read()
os.chdir(repo)
orig = open(path).read()
assert orig.count(old) == 1, f"old text found {orig.count(old)} times"
try:
    open(path, 'w').write(orig.replace(old, new))
    r = subprocess.run(cmd, capture_output=True, text=True, env={**os.environ, "GOWORK": "off"})
    out = (r.stdout + r.stderr).strip().splitlines()
    print("\n".join(out[-12:]))
    print("exit", r.returncode)
finally:
    open(path, 'w').write(orig)
