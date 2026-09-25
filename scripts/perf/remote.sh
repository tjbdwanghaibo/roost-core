#!/usr/bin/env bash
# 正式生成业务链路；持续时间可以设为 30m / 24h，数据和日志保留在独立目录。
set -euo pipefail
repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
label="${ROOST_REMOTE_LABEL:-remote-$(date +%Y%m%d-%H%M%S)}"
[[ "$label" =~ ^[a-zA-Z0-9_-]+$ ]] || { echo 'Invalid label' >&2; exit 2; }
output="$repo_dir/artifacts/perf/remote/$label"
[[ ! -e "$output" ]] || { echo "Output already exists: $output" >&2; exit 2; }
: "${ROOST_DATAENGINE_IT_ROOT:?source the isolated environment env.sh}"
lock="$ROOST_DATAENGINE_IT_ROOT/remote-acceptance.lock"
mkdir "$lock" || { echo 'Another Remote acceptance run owns the isolated environment' >&2; exit 2; }
trap 'rmdir "$lock"' EXIT
mkdir -p "$output"
export GOWORK=off GOMAXPROCS="${ROOST_REMOTE_CPU:-4}"
export ROOST_REMOTE_LOAD=1 ROOST_REMOTE_OUTPUT="$output/result.json"
export ROOST_REMOTE_DURATION="${ROOST_REMOTE_DURATION:-30m}" ROOST_REMOTE_TIMEOUT="${ROOST_REMOTE_TIMEOUT:-25h}"
export ROOST_REMOTE_POLICY="${ROOST_REMOTE_POLICY:-strict}" ROOST_REMOTE_RATE="${ROOST_REMOTE_RATE:-20}"
export ROOST_REMOTE_ENTITIES="${ROOST_REMOTE_ENTITIES:-10000}" ROOST_REMOTE_SESSIONS="${ROOST_REMOTE_SESSIONS:-1000}"
case "$ROOST_REMOTE_POLICY" in async|strict|pipelined) ;; *) echo 'Invalid policy' >&2; exit 2;; esac
cd "$repo_dir"
# 故障测试或上一次执行器退出后，先恢复并确认隔离集群就绪，再生成业务工程。
bash kit/scripts/integration/dataengine-env.sh heal > "$output/preflight.log" 2>&1
{ go version; git rev-parse HEAD; git status --short; shasum -a 256 codegen/internal/entity/testdata/remoteflow/*.go remoteentity/*.go nest/*.go dataengine/engine/*.go redis/driver/cluster_recovery.go; env | sort | sed -n '/^ROOST_REMOTE_/p'; printf 'GOMAXPROCS=%s\n' "$GOMAXPROCS"; } > "$output/env.txt"
# 慢请求原始堆栈保留，但压缩以免长稳产生巨量文本文件。
bash scripts/test-remote-generated.sh 2>&1 | gzip > "$output/run.log.gz"
[[ -f "$output/result.json.verified" ]] || { echo 'Final data verification missing' >&2; exit 1; }
printf 'Verified results: %s\n' "$output"
