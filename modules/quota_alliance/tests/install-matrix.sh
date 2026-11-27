#!/usr/bin/env bash
# 从源码树用刚构建的 CLI 检查真实生成消费者；输出都在调用方给的空目录。
set -euo pipefail
cli=${1:?absolute path to roost CLI}; dest=${2:?empty consumer directory}
[[ $cli = /* && -x $cli ]] || { echo 'need an absolute executable CLI path' >&2; exit 2; }
root=$(cd "$(dirname "$0")/../../.." && pwd -P)
mkdir -p "$dest"; dest=$(cd "$dest" && pwd -P)
[[ "$dest/" != "$root/"* && -z $(ls -A "$dest") ]] || { echo 'need an empty directory outside the source tree' >&2; exit 2; }
export PATH="$(dirname "$cli"):$PATH"
for order in solo forward reverse direct; do
    out="$dest/$order"; cd "$dest"
    "$cli" project new quotaprobe -module example.com/quotaprobe -out "$out" -template game --skip-deps
    cd "$out"; go mod edit -replace="github.com/tjbdwanghaibo/roost-core=$root"
    case "$order" in
        solo) "$cli" module add quota ;;
        forward) "$cli" module add quota; "$cli" module add alliance ;;
        reverse) "$cli" module add alliance; "$cli" module add quota ;;
        direct) "$cli" module add quota_alliance ;;
    esac
    if [[ $order = forward || $order = reverse ]]; then
        # 两个前置模块并存≠联盟宿主；显式追加后才出现第二个账本。
        ! grep -q 'QuotaAlliance' db/def/alliance.go || exit 1
        "$cli" module add quota_alliance
    fi
    "$cli" generate --check
    if [[ $order = solo ]]; then
        test ! -d game/entities/alliance
    else
        grep -q 'GetQuotaAlliance()' game/entities/alliance/*_gen_wire.go
        grep -q '^config_data:' configs/service/config.alliance.yaml
        awk '/a\.RegisterServer\(app\.ServiceName\(/{a=($0 ~ /ServiceName\("alliance"\)/)} a && /kitconfigdata\.NewConfigDataMod\(\)/{found=1} END{exit !found}' internal/bootstrap/generated.go
        # 检查权威服务本身的 mods，不能把 game 的 configdata 当成联盟的。
        awk '/^services:/{srv=1;next} srv && /^[^ ]/{exit} /^    alliance:/{a=1;next} /^    [^ ]/{a=0} a && /- configdata$/{found=1} END{exit !found}' roost.yaml
    fi
    go test -mod=mod -count=1 -race -run '^TestQuota(Player|Alliance)' ./game/handler
    "$cli" module verify --json > "$dest/$order.verify.json"
    python3 - "$dest/$order.verify.json" "$order" <<'PY'
import json, sys
report = json.load(open(sys.argv[1]))
counts = {m['name']: m['passed'] for m in report['modules'] if m['ok'] and m['skipped'] == 0}
assert report['ok'] and counts.get('quota') == 6, report
assert counts.get('quota_alliance') == (None if sys.argv[2] == 'solo' else 2), report
PY
    echo "quota matrix: $order passed"
done
