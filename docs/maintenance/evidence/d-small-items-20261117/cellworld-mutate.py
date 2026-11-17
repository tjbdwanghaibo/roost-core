#!/usr/bin/env python3
# 变异：每条替换 runtime.go 里 AddIngress 失败分支的一处，跑新场景，恢复原文件。
import subprocess, sys, os

proj = sys.argv[1]
target = os.path.join(proj, "game/modules/cellworldhost/runtime.go")
orig = open(target).read()

branch = '''	if err := app.AddIngress(r.registry, "cellworld", r.Node); err != nil {
		cancel()
		<-r.done
		return fail(err)
	}'''
assert branch in orig

mutations = {
    "M1 忽略 AddIngress 的错误（Start 照常成功）": (branch, '''	if err := app.AddIngress(r.registry, "cellworld", r.Node); false && err != nil {
		cancel()
		<-r.done
		return fail(err)
	}'''),
    "M2 失败时不停 Node（去掉 cancel 与等待 done）": (branch, '''	if err := app.AddIngress(r.registry, "cellworld", r.Node); err != nil {
		return fail(err)
	}'''),
    "M3 失败时直接返回错误、不复位（不调 fail）": (branch, '''	if err := app.AddIngress(r.registry, "cellworld", r.Node); err != nil {
		cancel()
		<-r.done
		return err
	}'''),
    "M4 fail 不把 Runtime 从 Host 拿下（去掉 host.detach）": ("\t\thost.detach(r)\n", "\t\t_ = host\n"),
}

env = dict(os.environ, GOWORK="off")
killed = 0
try:
    for name, (old, new) in mutations.items():
        assert orig.count(old) == 1, name
        open(target, "w").write(orig.replace(old, new))
        r = subprocess.run(["go", "test", "-mod=mod", "-count=1", "-run",
                            "^TestCellworldStartAfterInputsClosedTakesTheNodeDown$", "./game/modules/cellworldhost"],
                           cwd=proj, env=env, capture_output=True, text=True, timeout=600)
        out = r.stdout + r.stderr
        lines = [l for l in out.splitlines() if "runtime_test.go" in l or l.startswith(("--- FAIL", "FAIL", "ok"))]
        status = "KILLED" if r.returncode != 0 else "SURVIVED"
        killed += r.returncode != 0
        print(f"{status}: {name}")
        for l in lines[:4]:
            print("    " + l.strip())
finally:
    open(target, "w").write(orig)
print(f"{killed}/{len(mutations)} killed")
