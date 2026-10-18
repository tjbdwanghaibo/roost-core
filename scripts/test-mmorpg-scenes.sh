#!/usr/bin/env bash
# 同进程多地图业务接法：一个同步流、独立 Interest、正式 DAO / Sender 与真实 TCP 客户端。
set -euo pipefail
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
review_root="$(mktemp -d "${TMPDIR:-/tmp}/roost-mmo-scenes.XXXXXX")"
succeeded=0
cleanup() {
  if [[ "$succeeded" == 1 && "${ROOST_MMORPG_KEEP:-0}" != 1 ]]; then rm -rf "$review_root"; else echo "MMORPG scenes consumer kept: $review_root" >&2; fi
}
trap cleanup EXIT
export GOWORK=off GOMAXPROCS="${GOMAXPROCS:-4}"
cd "$repo_root"
echo "MMORPG scenes consumer: $review_root"
go run ./codegen/cmd/roost project new mmo-world -module example.com/mmo-world -out "$review_root/project" -template game-demo --skip-deps > "$review_root/generate.log" 2>&1
cd "$review_root/project"
go mod edit -replace "github.com/tjbdwanghaibo/roost-core=$repo_root"
for name in scene_runtime.go scene_runtime_test.go; do
  sed 's|MMO_MODULE|example.com/mmo-world|g' "$repo_root/scripts/testdata/mmorpg/$name.txt" > "internal/service/game/mmo_$name"
done
gofmt -w internal/service/game/mmo_*.go
go test -mod=mod -race -count=1 -timeout 3m -v -run '^TestMMORPGTwoScenesTCPTransferRetryAndReconnect$' ./internal/service/game | tee "$review_root/test.log"
if [[ "$(grep -c '^--- PASS: TestMMORPGTwoScenes' "$review_root/test.log" || true)" != 1 ]] || grep -q '^--- SKIP' "$review_root/test.log"; then exit 1; fi
go build -mod=mod -p 2 ./...
go vet -mod=mod -p 2 ./...
succeeded=1
