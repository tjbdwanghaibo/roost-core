#!/usr/bin/env bash
# 玩法夹具公用的“装模块 + 核对模块场景”两步（R9：玩法 = 装模块 + 只写模块没覆盖的部分）。
# 在生成工程的根目录里执行（cwd = 工程根）。
#
#   modules.sh add    <roost> <模块…>          装清单里的模块（依赖由安装器连带装上），并核对它们都已登记进 roost.yaml
#   modules.sh verify <roost> <repo> <json> [<模块…>]
#                                              跑 roost module verify --json（结果写到 <json>），逐个核对：
#                                              每个已装模块（及装上的联动 <a>+<b>）通过数 == 本检出 modules/<a>/module.yaml
#                                              的 verify.pass（联动取 with.<b>.verify.pass）、0 skip；build / vet / id check /
#                                              generate --check 全部通过；命令行列出的模块必须都在结果里。
#
# 只核对条数不够：安装器自己读的也是工程里的清单，所以这里再按本检出的 module.yaml 独立比一次，
# 模块场景被删、被跳过或清单计数漂移都会在玩法脚本里失败。
set -euo pipefail

usage() {
  echo "usage: modules.sh add <roost> <module...> | modules.sh verify <roost> <repo> <json> [<module...>]" >&2
  exit 2
}

[[ $# -ge 2 ]] || usage
action=$1
roost=$2
shift 2
case "$action" in
  add)
    [[ $# -ge 1 ]] || usage
    "$roost" module add "$@"
    # 安装成功 = 每个模块都记在 roost.yaml 的 modules.<name>（归属清单）；list --json 标出已安装的。
    "$roost" module list --json | python3 -c '
import json, sys
wanted = sys.argv[1:]
data = json.load(sys.stdin)
items = data["modules"] if isinstance(data, dict) else data
installed = {item["name"] for item in items if item.get("installed")}
missing = [name for name in wanted if name not in installed]
if missing:
    sys.exit("modules.sh add: not installed after roost module add: " + " ".join(missing))
print("modules.sh add: installed " + " ".join(sorted(installed)))
' "$@"
    ;;
  verify)
    [[ $# -ge 2 ]] || usage
    repo=$1
    out=$2
    shift 2
    # verify 自己失败（条数不符、跳过、build 失败）时退出码非零；JSON 仍写出来，下面的核对给出具体是哪一项。
    status=0
    "$roost" module verify --json > "$out" || status=$?
    python3 - "$repo" "$out" "$status" "$@" <<'PY'
import json, re, sys
from pathlib import Path

repo, out, status, wanted = Path(sys.argv[1]), Path(sys.argv[2]), int(sys.argv[3]), sys.argv[4:]
report = json.loads(out.read_text())
errors = []


def yaml_scalar(path, keys):
    # module.yaml 只用块状映射；按缩进找 keys 这条路径上的标量（不引入 PyYAML 依赖）。
    stack = []
    for raw in path.read_text().splitlines():
        match = re.match(r"^( *)([A-Za-z0-9_]+):\s*(.*?)\s*$", raw)
        if not match or raw.lstrip().startswith("#"):
            continue
        indent, key, value = len(match.group(1)), match.group(2), match.group(3)
        while stack and stack[-1][0] >= indent:
            stack.pop()
        stack.append((indent, key))
        if [k for _, k in stack] == keys:
            return value
    return None


def manifest_pass(name):
    # name 是模块名，或联动 <a>+<b>（a 的 module.yaml 里 with.<b>.verify.pass）。
    base, _, other = name.partition("+")
    keys = ["with", other, "verify", "pass"] if other else ["verify", "pass"]
    value = yaml_scalar(repo / "modules" / base / "module.yaml", keys)
    return int(value) if value and value.isdigit() else 0


seen = set()
for item in report.get("modules") or []:
    name = item["name"]
    seen.add(name)
    want = manifest_pass(name)
    line = f"module verify: {name} {item.get('passed')}/{want} skipped={item.get('skipped')}"
    print(line)
    if want <= 0:
        errors.append(f"{name}: modules/{name.partition('+')[0]}/module.yaml has no verify.pass")
    if item.get("want") != want:
        errors.append(f"{name}: CLI expected {item.get('want')}, module.yaml says {want}")
    if item.get("passed") != want or item.get("skipped") != 0 or not item.get("ok"):
        errors.append(f"{name}: passed={item.get('passed')} skipped={item.get('skipped')} want={want}")
for check in report.get("checks") or []:
    if not check.get("ok"):
        errors.append(f"check failed: {check.get('name')}")
missing = [name for name in wanted if name not in seen]
if missing:
    errors.append("declared modules missing from verify: " + " ".join(missing))
if not seen:
    errors.append("roost module verify reported no installed module")
if not report.get("ok") or status != 0:
    errors.append(f"roost module verify ok={report.get('ok')} exit={status}")
if errors:
    sys.exit("modules.sh verify:\n  " + "\n  ".join(errors))
PY
    ;;
  *)
    usage
    ;;
esac
