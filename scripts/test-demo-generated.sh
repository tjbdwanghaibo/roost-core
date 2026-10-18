#!/usr/bin/env bash
# game-demo 生成工程里需要真实依赖的用例。
#
# 这些用例只在新生成的 game-demo 里存在（模板在 demo/ 下），或由 codegen/internal/roost 写进新生成的 game-demo
# 再运行；门变量没人设置时它们到处跳过，go test ./... 与 framework-compat / demo-publish 的生成工程测试都看不到：
#   - game/controllers/player 的 TestGuildIDsRealMongo*（ROOST_GUILD_IT_MONGO_URI：三次进程共享 Mongo 与 WAL 的
#     发号恢复、首次发号并发 upsert）；
#   - db/migrations 的 TestPlayerMigrationRealMongo（ROOST_MIGRATION_IT_MONGO_URI：Player schema 离线迁移的
#     plan / apply / verify / rollback 在真实副本集上跑一遍，唯一库名、结束删库）；
#   - internal/service/platform 的 TestPurchaseDeliveryDurableWinner（ROOST_REVIEW_REDIS；ROOST_REVIEW_CLUSTER
#     由调用方给，mirror-local.sh test-core 传入私有 3 主 3 从，没给时集群子用例跳过）；
#   - internal/service/game 的 TestActivityBoardKeepsTheLatestCumulativeScore（ROOST_REVIEW_REDIS：真实 Redis 上
#     延后旧贡献回复，验证结算榜不被较低的累计分快照覆盖；协调器使用内存版本存储）；
#   - game/controllers/player 的 TestDungeonClearsBoardCountsEachRunOnceRealRedis（ROOST_REVIEW_REDIS：正式端点 +
#     真实 Redis 上的 rank RedisStore，超过去重环 8 个槽位后的重放、并发重放与排行提交失败后的补报都只计一次）；
#   - codegen/internal/roost 的 TestGateGeneratedBusinessConsumer（ROOST_DATAENGINE_IT_NATS_URL）。
# 门变量全部对到隔离环境地址。用例各用唯一库名 / 键前缀并在结束时删除；本脚本只删自己的临时工程。是功能验收，不是负载。
# 调用方：wiring/scripts/integration/dataengine-env.sh test、scripts/mirror-local.sh test-core；
# 根包 integration_coverage_test.go 的 TestGeneratedProjectRealDependencyTestsHaveAnEntry 钉住挂载与 -run。
set -euo pipefail
: "${ROOST_DATAENGINE_IT_MONGO_URI:?isolated Mongo replica-set URI required}"
: "${ROOST_DATAENGINE_IT_REDIS_ADDR:?isolated Redis required}"
: "${ROOST_DATAENGINE_IT_NATS_URL:?isolated NATS required}"
repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
work_dir="$(mktemp -d "${TMPDIR:-/tmp}/roost-demo-generated.XXXXXX")"
trap 'rm -rf "$work_dir"' EXIT
export GOWORK=off
cd "$repo_dir"
# 当前生成器、当前 checkout：--skip-deps 不解析已发布版本，replace 指回本仓。
go run ./codegen/cmd/roost project new planet -module example.com/planet -out "$work_dir/planet" -template game-demo --skip-deps
printf '\nreplace github.com/tjbdwanghaibo/roost-core => %s\n' "$repo_dir" >> "$work_dir/planet/go.mod"
(
	cd "$work_dir/planet"
	ROOST_GUILD_IT_MONGO_URI="$ROOST_DATAENGINE_IT_MONGO_URI" go test -mod=mod -race -count=1 -timeout 5m -v -run '^TestGuildIDsRealMongo' ./game/controllers/player
	ROOST_REVIEW_REDIS="$ROOST_DATAENGINE_IT_REDIS_ADDR" go test -mod=mod -race -count=1 -timeout 5m -v -run '^TestPurchaseDeliveryDurableWinner$' ./internal/service/platform
	ROOST_REVIEW_REDIS="$ROOST_DATAENGINE_IT_REDIS_ADDR" go test -mod=mod -race -count=1 -timeout 5m -v -run '^TestActivityBoardKeepsTheLatestCumulativeScore$' ./internal/service/game
	ROOST_REVIEW_REDIS="$ROOST_DATAENGINE_IT_REDIS_ADDR" go test -mod=mod -race -count=1 -timeout 5m -v -run '^TestDungeonClearsBoardCountsEachRunOnceRealRedis$' ./game/controllers/player
	ROOST_MIGRATION_IT_MONGO_URI="$ROOST_DATAENGINE_IT_MONGO_URI" go test -mod=mod -race -count=1 -timeout 5m -v -run '^TestPlayerMigrationRealMongo$' ./db/migrations
)
go test -count=1 -timeout 10m -v -run '^TestGateGeneratedBusinessConsumer$' ./codegen/internal/roost
