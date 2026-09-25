#!/usr/bin/env bash
# 正式 DAO + Entity 生成器 → Nest → 双模式 Sync，不依赖示例项目。
set -euo pipefail
repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
work_dir="$(mktemp -d "${TMPDIR:-/tmp}/roost-sync-modes.XXXXXX")"
trap 'rm -rf "$work_dir"' EXIT
export GOWORK=off
cp "$repo_dir/codegen/internal/entity/testdata/syncmodes/"*.go "$work_dir/"
cat > "$work_dir/go.mod" <<MOD
module syncmodes

go 1.27.0

require github.com/tjbdwanghaibo/roost-core v0.0.0
replace github.com/tjbdwanghaibo/roost-core => $repo_dir
MOD
cd "$repo_dir"
go run ./codegen/cmd/dao -def ./codegen/internal/entity/testdata/syncmodes/def -out "$work_dir" -pkg syncmodes -force
go run ./codegen/cmd/entity -dir "$work_dir" -force
cd "$work_dir"
go test -mod=mod -race ./... "$@"
