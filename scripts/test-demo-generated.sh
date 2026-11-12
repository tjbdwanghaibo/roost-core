#!/usr/bin/env bash
# game-demo 生成工程里需要真实依赖的用例。
#
# 这些用例只在新生成的 game-demo 里存在（模板在 demo/ 下），或由 codegen/internal/roost 写进新生成的 game-demo
# 再运行；门变量没人设置时它们到处跳过，go test ./... 与 framework-compat / demo-publish 的生成工程测试都看不到：
#   - internal/service/alliance 的 TestAllianceStopFoundingSendsNewAlliancesToTheOtherInstances（ROOST_DATAENGINE_IT_NATS_URL：
#     两个真实 Bus 上的 alliance 实例，运维命令 alliance.stop_founding 让一个退出 Found 队列组后，新建联盟全部落到另一个，
#     路由到它的已有联盟照常服务）；
#   - internal/service/game 的 TestTerrainRestartRealMongo（ROOST_TERRAIN_IT_MONGO_URI：可编辑地形改块重启后一致——真实
#     DataEngine（文件 WAL、投影、冷加载、新建准入门）上改块、正常停机、同一 WAL 目录重启后按 _id 读回覆盖块，整张图逐点
#     与重启前一致；唯一库名、结束删库）与 TestTerrainEditLedgerRealMongo（同一门变量：玩家编辑账本落库，重启冷加载后
#     窗口外的旧请求重放答过期、窗口内答重复、都不写，新序号照常生效；T01/T02）与 terrain_building_test 的
#     TestBuildingRestartRealMongo（同一门变量：建筑与块上的占地落库，重启后对账找回、原始地形换版后地基重新整平；P4）；
#   - db/migrations 的 TestPlayerMigrationRealMongo（ROOST_MIGRATION_IT_MONGO_URI：Player schema 离线迁移的
#     plan / apply / verify / rollback 在真实副本集上跑一遍，唯一库名、结束删库）；
#   - game/purchase 的 TestPurchaseRedis*（ROOST_PURCHASE_REDIS_ADDR：实际执行序号分配、并发去重、分批清理 Lua；
#     显式 integration 构建标签，未设置地址直接失败；原 demo 发货实现已迁入 purchase 模块）；
#   - internal/service/game 的 TestActivityBoardKeepsTheLatestCumulativeScore（ROOST_REVIEW_REDIS：真实 Redis 上
#     延后旧贡献回复，验证结算榜不被较低的累计分快照覆盖；协调器使用内存版本存储）；
#   - game/controllers/player 的 TestDungeonClearsBoardCountsEachRunOnceRealRedis（ROOST_REVIEW_REDIS：正式端点 +
#     真实 Redis 上的 rank RedisStore，超过去重环 8 个槽位后的重放、并发重放与排行提交失败后的补报都只计一次）；
#   - codegen/internal/roost 的 TestGateGeneratedBusinessConsumer（ROOST_DATAENGINE_IT_NATS_URL）；
#   - codegen/internal/roost 的 TestGeneratedSagaNativeStepsAndEffectConsumer（ROOST_DATAENGINE_IT_MONGO_URI / NATS_URL：新手工程
#     add saga -native 与 add effect 填最小业务后，真实 Mongo / NATS / DataEngine / Nest 上一笔正向、一笔补偿与一条 effect
#     消费恰好一次；唯一 DAO 库名、库与流用后删除）。
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
	go test -mod=mod -race -count=1 -timeout 5m -v -run '^TestAllianceStopFoundingSendsNewAlliancesToTheOtherInstances$' ./internal/service/alliance
	ROOST_PURCHASE_REDIS_ADDR="$ROOST_DATAENGINE_IT_REDIS_ADDR" go test -tags=integration -mod=mod -race -count=1 -timeout 5m -v -run '^TestPurchaseRedis' ./game/purchase
	ROOST_REVIEW_REDIS="$ROOST_DATAENGINE_IT_REDIS_ADDR" go test -mod=mod -race -count=1 -timeout 5m -v -run '^TestActivityBoardKeepsTheLatestCumulativeScore$' ./internal/service/game
	ROOST_REVIEW_REDIS="$ROOST_DATAENGINE_IT_REDIS_ADDR" go test -mod=mod -race -count=1 -timeout 5m -v -run '^TestDungeonClearsBoardCountsEachRunOnceRealRedis$' ./game/controllers/player
	ROOST_MIGRATION_IT_MONGO_URI="$ROOST_DATAENGINE_IT_MONGO_URI" go test -mod=mod -race -count=1 -timeout 5m -v -run '^TestPlayerMigrationRealMongo$' ./db/migrations
	ROOST_TERRAIN_IT_MONGO_URI="$ROOST_DATAENGINE_IT_MONGO_URI" go test -mod=mod -race -count=1 -timeout 5m -v -run '^(TestTerrainRestartRealMongo|TestTerrainEditLedgerRealMongo|TestBuildingRestartRealMongo)$' ./internal/service/game
)
ROOST_GATE_REAL=1 go test -count=1 -timeout 10m -v -run '^TestGateGeneratedBusinessConsumer$' ./codegen/internal/roost
go test -count=1 -timeout 20m -v -run '^TestGeneratedSagaNativeStepsAndEffectConsumer$' ./codegen/internal/roost
