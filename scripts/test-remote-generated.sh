#!/usr/bin/env bash
# 生成正式 DAO/Entity，验收 Nest -> WAL/Mongo -> Remote -> 真实 NATS 接收。
set -euo pipefail
: "${ROOST_DATAENGINE_IT_MONGO_URI:?isolated Mongo URI required}"
: "${ROOST_DATAENGINE_IT_REDIS_ADDR:?isolated Redis required}"
: "${ROOST_DATAENGINE_IT_NATS_URL:?isolated NATS required}"
repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
work_dir="$(mktemp -d "${TMPDIR:-/tmp}/roost-remote-generated.XXXXXX")"
trap 'rm -rf "$work_dir"' EXIT
export GOWORK=off
export ROOST_REMOTE_FAULT_SCRIPT="$repo_dir/scripts/remote-fault.sh"
mkdir -p "$work_dir/def"
cp "$repo_dir/codegen/internal/entity/testdata/remoteflow/"*.go "$work_dir/"
database="roost_remote_generated_${$}_$(date +%s)"
sed "s/roost_remote_generated_placeholder/$database/g" "$repo_dir/codegen/internal/entity/testdata/remoteflow/def/state.go" > "$work_dir/def/state.go"
cat > "$work_dir/go.mod" <<MOD
module remoteflow

go 1.27.0

require github.com/tjbdwanghaibo/roost-core v0.0.0
replace github.com/tjbdwanghaibo/roost-core => $repo_dir
MOD
cd "$repo_dir"
go run ./codegen/cmd/dao -def "$work_dir/def" -out "$work_dir" -pkg remoteflow -force
go run ./codegen/cmd/entity -dir "$work_dir" -force
cd "$work_dir"
race=(-race)
if [[ "${ROOST_REMOTE_LOAD:-0}" == 1 && "${ROOST_REMOTE_RACE:-0}" != 1 ]]; then race=(); fi
profile=()
if [[ -n "${ROOST_REMOTE_TRACE:-}" ]]; then profile+=("-trace=$ROOST_REMOTE_TRACE"); fi
if [[ -n "${ROOST_REMOTE_CPU_PROFILE:-}" ]]; then profile+=("-cpuprofile=$ROOST_REMOTE_CPU_PROFILE"); fi
go test -mod=mod ${race[@]+"${race[@]}"} ${profile[@]+"${profile[@]}"} -count=1 -timeout="${ROOST_REMOTE_TIMEOUT:-120s}" -run '^TestGeneratedRemoteNestFlow$' -v .
