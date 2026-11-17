# 两项修复的独立复验

结论及限制见 [报告](../../FIX-VERIFICATION-20261117.md)。时间为 2026-11-17，Linux x86_64 / Go 1.27.2；被测 main 为 `00d6eade0c22deddc409247e231cbfa4f268e3fc`，修复代码与收尾 main `adffd62c95b7ce3ced3e0bc984b122662b484bfc` 相同。初始旧快照测试没有混入这些日志。

## 文件

| 文件 | 内容 |
| --- | --- |
| `latest-generate.log`、`latest-module-add.log`、`latest-add-edge.log` | 同版本 CLI 创建工程、添加 login / enter / portal / room、添加 edge |
| `latest-module-verify.json` | 120 场景零跳过、build / vet / ID / 生成漂移四项通过 |
| `latest-doctor.log` | 实际生成工程 doctor 检查，保留原 WARN |
| `room-probes-fixed.log` | 原三类故障与三组对照，各三轮 race |
| `gate-names-fixed.log` | 正式真实 TCP / NATS 测试，四种名称组合各三轮 race |
| `generated-edge-fixed.log` | 实际生成 edge 包装的握手与转发，三轮 race |
| `gate-unit.log`、`room-packages.log` | 完整相关包的 race 检查 |
| `review_presence_*_test.go.txt` | 原 room 审核探针，仅改包名与放置目录 |
| `review_gate_names_test.go.txt` | 原 Gate 探针按本工程 edge/game 声明适配 |

## 复跑

在上述 main 的独立干净检出中，用同一检出的 CLI、模板和 core 依赖；不要在固定审核分支的旧运行时上运行。以下用 `CORE_REVIEW_ROOT` 表示该检出绝对路径，`REVIEW_TMP` 为新建临时目录：

```bash
export GOWORK=off
export GOMAXPROCS=4
cd "$CORE_REVIEW_ROOT"
go build -o "$REVIEW_TMP/roost" ./codegen/cmd/roost
"$REVIEW_TMP/roost" project new audit -module example.com/mainaudit \
  -out "$REVIEW_TMP/project" -template game --skip-deps
cd "$REVIEW_TMP/project"
go mod edit -replace "github.com/tjbdwanghaibo/roost-core=$CORE_REVIEW_ROOT"
git init -q
"$REVIEW_TMP/roost" module add login enter portal room
"$REVIEW_TMP/roost" add gate edge
"$REVIEW_TMP/roost" module verify --json
"$REVIEW_TMP/roost" project doctor
```

把本目录两个 `review_presence_*_test.go.txt` 去掉 `.txt` 后放到生成工程的 `game/controllers/`；将 `review_gate_names_test.go.txt` 去掉 `.txt` 放到 `internal/access/player/tcp/`。先启动自己的私有 NATS（本轮为 `127.0.0.1:14222`），再执行：

```bash
cd "$REVIEW_TMP/project"
go test -race -count=3 -v -timeout 3m \
  -run '^TestReviewRoomPresence(PostCommitOrder|CanceledWait)$' ./game/controllers
REVIEW_NATS_URL=nats://127.0.0.1:14222 \
  go test -race -count=3 -v -timeout 3m \
  -run '^TestReviewGeneratedGateServiceNames$' ./internal/access/player/tcp
go test -race -count=1 -timeout 3m \
  ./game/controllers ./game/modules/room ./game/handler
cd "$CORE_REVIEW_ROOT"
ROOST_DATAENGINE_IT_NATS_URL=nats://127.0.0.1:14222 \
  go test -race -tags integration -count=3 -v -timeout 3m \
  -run '^TestRealGateHandshakeUsesActualServiceNames$' ./wiring/gate
go test -race -count=1 -timeout 3m ./wiring/gate
```

所有上述本轮检查成功结束；三份定向日志无 SKIP。Room 使用真实 Nest、替身提交器和连接事件源；Gate 使用真实 TCP / NATS、替身 account / identity / discovery，不等于完整外部依赖验收。用完清理自行创建的临时工程并停止私有 NATS。
