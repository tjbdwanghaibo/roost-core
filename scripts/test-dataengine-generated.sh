#!/usr/bin/env bash
# 正式 DAO / Entity 生成 → Nest 事务 → 文件 WAL / Mongo → 三次独立进程。
set -euo pipefail
: "${ROOST_DATAENGINE_IT_MONGO_URI:?set the isolated Mongo replica-set URI}"
repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
work_dir="$(mktemp -d "${TMPDIR:-/tmp}/roost-dataengine-generated.XXXXXX")"
export GOWORK=off
export ROOST_GENERATED_WAL_DIR="$work_dir/wal"
cleanup() {
  local result=$?
  if [[ -x "$work_dir/persist.test" ]]; then
    if ! ROOST_GENERATED_PHASE=cleanup "$work_dir/persist.test" -test.run '^TestGeneratedDataEngineProcessLifecycle$' -test.timeout 45s; then
      result=1
    fi
  fi
  rm -rf "$work_dir"
  exit "$result"
}
trap cleanup EXIT
mkdir -p "$work_dir/def"
cp "$repo_dir/codegen/internal/entity/testdata/dataengine/"*.go "$work_dir/"
database="roost_generated_it_${$}_$(date +%s)"
for definition in "$repo_dir/codegen/internal/entity/testdata/dataengine/def/"*.go; do
  sed "s/roost_generated_it_placeholder/$database/g" "$definition" > "$work_dir/def/$(basename "$definition")"
done
cat > "$work_dir/go.mod" <<MOD
module persistflow

go 1.27.0

require github.com/tjbdwanghaibo/roost-core v0.0.0
replace github.com/tjbdwanghaibo/roost-core => $repo_dir
MOD
cd "$repo_dir"
go run ./codegen/cmd/dao -def "$work_dir/def" -out "$work_dir" -pkg persistflow -force
go run ./codegen/cmd/entity -dir "$work_dir" -force
cd "$work_dir"
if [[ "${ROOST_DATAENGINE_PERF:-0}" == 1 ]]; then
  # 性能样本不启用 race；可单独用 PERF_RACE=1 检查测量器并发正确性。
  if [[ "${ROOST_DATAENGINE_PERF_RACE:-0}" == 1 ]]; then
    go test -mod=mod -race -c -o persist.test .
  else
    go test -mod=mod -c -o persist.test .
  fi
  ./persist.test -test.run '^TestGeneratedDataEnginePressure$' -test.timeout 600s -test.v
else
  go test -mod=mod -race -c -o persist.test .
  for phase in 1 2 3; do
    ROOST_GENERATED_PHASE="$phase" ./persist.test -test.run '^TestGeneratedDataEngine' -test.timeout 240s -test.v
  done
fi
