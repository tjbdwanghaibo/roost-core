#!/usr/bin/env bash
set -euo pipefail

# 显式运行已登记缺陷的行为回归；修复前应以非零状态报告“9 局变 10 分”。
# 只创建隔离生成工程，不修改当前 checkout，不连接真实 Redis / Mongo / NATS。
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
review_root="$(mktemp -d "${TMPDIR:-/tmp}/roost-dungeon-ranking.XXXXXX")"
project_root="$review_root/project"
export GOWORK=off
export GOMAXPROCS="${GOMAXPROCS:-4}"

echo "Generated consumer and logs: $review_root"
cd "$repo_root"
if go run ./codegen/cmd/roost project new dungeon-rank-review \
    -module example.com/dungeon-rank-review -out "$project_root" \
    -template game-demo --skip-deps > "$review_root/generate.log" 2>&1; then
    :
else
    result=$?
    cat "$review_root/generate.log"
    exit "$result"
fi

# 复用 rank 自身的存储替身，让正式 RedisStore 执行去重、CAS 与计分路径；不维护第二份计分逻辑。
sed -e 's/^package rank$/package player/' \
    -e 's/newFakeRedis/newReviewRankRedis/g' \
    -e 's/fakeRedis/reviewRankRedis/g' \
    -e 's/toStr/reviewRankString/g' \
    "$repo_root/service/rank/fake_redis_test.go" \
    > "$project_root/game/controllers/player/review_rank_redis_test.go"
cp "$repo_root/scripts/testdata/dungeon-ranking/replay_test.go.txt" \
    "$project_root/game/controllers/player/review_dungeon_ranking_test.go"

cd "$project_root"
go mod edit -replace "github.com/tjbdwanghaibo/roost-core=$repo_root"
go test -mod=mod -count=1 -run '^TestDungeonRankingReplayAfterNineClears$' ./game/controllers/player
