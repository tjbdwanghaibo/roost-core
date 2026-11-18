#!/usr/bin/env python3
"""pretag 的范围：按上一个发布 tag 以来改了什么，算出这次要跑的测试（维护者 2026-11-18：改动不多时只跑有影响的）。

用法：python3 scripts/pretag-scope.py <上一个 tag 或提交> <生成工程脚本...>
标准输出是给 bash eval 的变量（scope、go_packages、genre_scripts、run_dotnet、run_ts）；标准错误是给人看的说明。

规则（任何一条判不准就往“多跑”那边靠）：
- 整跑（scope=full）：设了 ROOST_PRETAG_FULL=1；或者改了 go.mod / go.sum / 发版与 pretag 脚本本身；或者改动的 Go 包超过
  FULL_PACKAGE_LIMIT 个、改动的文件超过 FULL_FILE_LIMIT 个（“改得多”）。
- Go 测试：改动落在的包（非 .go 文件按所在目录往上找最近的 Go 包，go:embed 的模板因此算进嵌入它的包），加上依赖这些包
  的包（含测试依赖，go list -test -deps 的传递闭包）。根包总是跑（文档链接、提交引用等门禁在那里）。
- 生成工程脚本与 codegen/internal/roost：生成的工程经 replace 编译本仓库，所以它们还依赖“生成代码会 import 的包”——
  模板、modules、demo、scripts/testdata 里出现的本仓库 import 路径的依赖闭包。改动的非测试包落在这个闭包里，或者改了
  codegen/、modules/、demo/、scripts/testdata/，全部生成工程脚本都跑，codegen/internal/roost 也跑；否则只跑自己被改过的脚本。
  只改 _test.go 不影响生成的工程。
- C# SDK：改了 client/dotnet/、client/spec/、client/wire/、service/relay/ 才跑；TS SDK：改了 client/ts/、client/spec/ 才跑。
"""
import json
import os
import re
import subprocess
import sys

FULL_PACKAGE_LIMIT = 40
FULL_FILE_LIMIT = 300
GENRE_SCRIPTS = []  # 由 pretag.sh 作为参数传入：清单只写在 pretag.sh 一处（根包守卫 TestGeneratedScriptsAreInPretag 读它）
FULL_TRIGGERS = {"go.mod", "go.sum", "scripts/pretag.sh", "scripts/release.sh", "scripts/pretag-scope.py"}
GENERATOR_DIRS = ("codegen/", "modules/", "demo/", "scripts/testdata/")
CODEGEN_PKG_SUFFIX = "/codegen/internal/roost"
# 发版提交自己改的文件（release.sh 写版本号）：不算生成器改动。
RELEASE_FILES = {"codegen/ci/framework-release.yaml", "CHANGELOG.md", ".github/workflows/upgrade-compat.yml"}


def run(*args):
    env = dict(os.environ, GOWORK="off")
    return subprocess.run(args, check=True, capture_output=True, text=True, env=env).stdout


def say(msg):
    print("pretag-scope: " + msg, file=sys.stderr)


def emit(scope, go_packages, genres, dotnet, ts):
    def arr(name, items):
        print(name + "=(" + " ".join("'" + i + "'" for i in items) + ")")
    print("scope=" + scope)
    arr("go_packages", go_packages)
    arr("genre_scripts", genres)
    print("run_dotnet=" + ("1" if dotnet else "0"))
    print("run_ts=" + ("1" if ts else "0"))


def full(reason):
    say("full run: " + reason)
    emit("full", ["./..."], GENRE_SCRIPTS, True, True)
    sys.exit(0)


def main():
    if len(sys.argv) < 3:
        print("usage: pretag-scope.py <base-ref> <genre-script>...", file=sys.stderr)
        sys.exit(2)
    base = sys.argv[1]
    GENRE_SCRIPTS[:] = sys.argv[2:]
    if os.environ.get("ROOST_PRETAG_FULL") == "1":
        full("ROOST_PRETAG_FULL=1")
    module = run("go", "list", "-m").strip()
    changed = [f for f in run("git", "diff", "--name-only", base + "..HEAD").split("\n") if f]
    say(f"{len(changed)} files changed since {base}")
    hit = sorted(FULL_TRIGGERS.intersection(changed))
    if hit:
        full("changed " + ", ".join(hit))
    if len(changed) > FULL_FILE_LIMIT:
        full(f"{len(changed)} files changed (> {FULL_FILE_LIMIT})")

    # 本模块的包：目录 -> 导入路径。
    pkgs = {}
    for line in run("go", "list", "-f", "{{.Dir}}\t{{.ImportPath}}", "./...").splitlines():
        d, p = line.split("\t")
        pkgs[os.path.relpath(d)] = p
    root = os.getcwd()

    def owner(path):
        d = os.path.dirname(path) or "."
        while True:
            if d in pkgs:
                return pkgs[d]
            if d in (".", ""):
                return pkgs.get(".")
            d = os.path.dirname(d) or "."

    changed_pkgs, changed_nontest = set(), set()
    for f in changed:
        if f.endswith(".md") or f in RELEASE_FILES:
            continue  # 文档与发版提交的文件只影响根包的门禁，根包总是跑
        p = owner(f)
        if p is None or (p == pkgs.get(".") and not (f.endswith(".go") and "/" not in f)):
            continue  # 落到根包的非 Go 文件（docs/ 证据、根目录杂项）：根包总是跑，不往下传
        changed_pkgs.add(p)
        if not f.endswith("_test.go"):
            changed_nontest.add(p)
    if len(changed_pkgs) > FULL_PACKAGE_LIMIT:
        full(f"{len(changed_pkgs)} packages changed (> {FULL_PACKAGE_LIMIT})")

    # 反向依赖：go list -test 的每个条目（含测试变体）的传递依赖里有改动的包，就算受影响。
    affected = set(changed_pkgs)
    out = run("go", "list", "-test", "-e", "-json=ImportPath,Deps,ForTest", "./...")
    dec = json.JSONDecoder()
    i = 0
    while i < len(out):
        while i < len(out) and out[i].isspace():
            i += 1
        if i >= len(out):
            break
        obj, i = dec.raw_decode(out, i)
        path = obj.get("ForTest") or obj["ImportPath"].split(" ")[0]
        path = re.sub(r"(_test|\.test)$", "", path)
        if path not in pkgs.values():
            continue
        if changed_pkgs.intersection(obj.get("Deps") or []):
            affected.add(path)

    # 生成代码会 import 的本仓库包，及其依赖闭包。
    imports = set()
    pattern = re.compile(r'"(' + re.escape(module) + r'(?:/[A-Za-z0-9_.\-/]+)?)"')
    for top in ("codegen", "modules", "demo", "scripts/testdata"):
        for dirpath, _, files in os.walk(top):
            for name in files:
                if name.endswith((".tmpl", ".go")):
                    with open(os.path.join(dirpath, name), encoding="utf-8", errors="ignore") as fh:
                        imports.update(pattern.findall(fh.read()))
    imports = sorted(p for p in imports if p in pkgs.values() and p != module)  # 只写模块路径的是 go.mod 的 require，不是 import
    generated_closure = set(imports)
    if imports:
        generated_closure.update(run("go", "list", "-e", "-deps", *imports).split())
    generator_touched = any(f.startswith(GENERATOR_DIRS) and not f.endswith(".md") and f not in RELEASE_FILES for f in changed)
    generated_hit = sorted(changed_nontest.intersection(generated_closure))

    codegen_pkg = module + CODEGEN_PKG_SUFFIX
    if generator_touched or generated_hit:
        why = "generator files changed" if generator_touched else "generated code imports " + ", ".join(generated_hit[:5])
        say("generated projects affected (" + why + "): all genre scripts and " + codegen_pkg)
        genres = list(GENRE_SCRIPTS)
        affected.add(codegen_pkg)
    else:
        genres = [s for s in GENRE_SCRIPTS if s in changed]

    affected.add(pkgs["."])
    go_packages = sorted(affected)
    dotnet = any(f.startswith(("client/dotnet/", "client/spec/", "client/wire/", "service/relay/")) for f in changed)
    ts = any(f.startswith(("client/ts/", "client/spec/")) for f in changed)
    say(f"affected run: {len(go_packages)} of {len(set(pkgs.values()))} packages, {len(genres)} of {len(GENRE_SCRIPTS)} genre scripts, "
        f"C# {'yes' if dotnet else 'no'}, TS {'yes' if ts else 'no'}")
    emit("affected", go_packages, genres, dotnet, ts)


if __name__ == "__main__":
    main()
