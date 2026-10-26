#!/usr/bin/env bash
# 不依赖外部服务：正式生成实体/组件接线、Sender，经 Nest 验证回滚与提交后动作回执。
set -euo pipefail
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
review_root="$(mktemp -d "${TMPDIR:-/tmp}/roost-level.XXXXXX")"
succeeded=0
cleanup() {
 if [[ "$succeeded" == 1 ]]; then rm -rf "$review_root"; else echo "Level consumer retained: $review_root" >&2; fi
}
trap cleanup EXIT
export GOWORK=off
cd "$repo_root"
go build -o "$review_root/entity" ./codegen/cmd/entity
go build -o "$review_root/nest" ./codegen/cmd/nest
mkdir "$review_root/project"
cp scripts/testdata/level/room.go.txt "$review_root/project/room.go"
cp scripts/testdata/level/room_test.go.txt "$review_root/project/room_test.go"
mkdir "$review_root/project/dto"
cp scripts/testdata/level/dto.go.txt "$review_root/project/dto/dto.go"
cd "$review_root/project"
go mod init example.com/level-practice
go mod edit -replace "github.com/tjbdwanghaibo/roost-core=$repo_root"
"$review_root/entity" -dir .
"$review_root/nest" -dir .
go mod tidy
go test -race -count=1 ./...
succeeded=1
