#!/usr/bin/env python3
"""scripts/pretag-scope.py 的自检（根包 TestPretagScopeSelfTest 运行它）。

不碰 git 与 go：把脚本里的 run 换成按参数作答的假函数，在空的临时目录里跑 main()，只看它给 pretag 的变量。
用例是“只改了某一类文件时选中了什么”：SDK 验收脚本自身（RV-20261118-12）、SDK 源码与文档对照、整跑触发。
"""
import contextlib
import importlib.util
import io
import json
import os
import sys
import tempfile

sys.dont_write_bytecode = True  # 载入 pretag-scope.py 时不在 scripts/ 下留 __pycache__
HERE = os.path.dirname(os.path.abspath(__file__))
MODULE = "example.test/scope"
GENRES = ["scripts/test-slg-generated.sh"]


def load():
    spec = importlib.util.spec_from_file_location("pretag_scope", os.path.join(HERE, "pretag-scope.py"))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def select(changed):
    """假设上一个 tag 以来只改了 changed，返回 pretag-scope.py 输出的变量（字符串）。只有根包一个 Go 包。"""
    scope = load()
    workdir = os.getcwd()

    def fake_run(*args):
        if args[:3] == ("go", "list", "-m"):
            return MODULE + "\n"
        if args[:2] == ("git", "diff"):
            return "\n".join(changed) + "\n"
        if args[:3] == ("go", "list", "-f"):
            return workdir + "\t" + MODULE + "\n"
        if args[:3] == ("go", "list", "-test"):
            return json.dumps({"ImportPath": MODULE, "Deps": []})
        if args[:3] == ("go", "list", "-e"):
            return ""
        raise AssertionError("unexpected command " + " ".join(args))

    scope.run = fake_run
    out = io.StringIO()
    argv = sys.argv
    sys.argv = ["pretag-scope.py", "v0.0.0"] + GENRES
    try:
        with contextlib.redirect_stdout(out), contextlib.redirect_stderr(io.StringIO()):
            try:
                scope.main()
            except SystemExit as exit_:
                if exit_.code not in (0, None):
                    raise
    finally:
        sys.argv = argv
    values = {}
    for line in out.getvalue().splitlines():
        name, _, value = line.partition("=")
        values[name] = value
    return values


def main():
    failures = []

    def expect(changed, **want):
        got = select(changed)
        for name, value in want.items():
            if got.get(name) != value:
                failures.append(f"{changed}: {name}={got.get(name)!r}, want {value!r} (all: {got})")

    os.environ.pop("ROOST_PRETAG_FULL", None)
    with tempfile.TemporaryDirectory() as workdir:
        os.chdir(workdir)  # 空目录：生成工程 import 的扫描什么也找不到，结果只取决于 changed
        # RV-20261118-12：只改 SDK 的验收脚本自身时，对应的 SDK 门要跑（另一边不跑）。
        expect(["scripts/test-client-ts.sh"], scope="affected", run_ts="1", run_dotnet="0")
        expect(["scripts/test-client-dotnet.sh"], scope="affected", run_ts="0", run_dotnet="1")
        # 对照：SDK 源码照旧触发；只改文档两边都不跑；共同对端两边都跑；整跑触发项照旧整跑。
        expect(["client/ts/src/gate/connection.ts"], scope="affected", run_ts="1", run_dotnet="0")
        expect(["client/dotnet/Roost.Client/Connection.cs"], scope="affected", run_ts="0", run_dotnet="1")
        expect(["docs/maintenance/README.md"], scope="affected", run_ts="0", run_dotnet="0")
        expect(["client/spec/packets.json"], scope="affected", run_ts="1", run_dotnet="1")
        expect(["scripts/pretag-scope.py"], scope="full", run_ts="1", run_dotnet="1")
    if failures:
        for failure in failures:
            print("FAIL " + failure)
        sys.exit(1)
    print("PASS pretag-scope selection")


if __name__ == "__main__":
    main()
