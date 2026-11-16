# 2026-11-16 main 审查证据

被测 main：`f2c1ac4434334630569f93ecda49191673965e1a`，基线 `603dd1394dcf892a25bfaa142454804a723de554`。Linux amd64、Go 1.27.2、GOWORK=off、GOMAXPROCS=4、GOFLAGS=-mod=readonly。报告分支只保存文档，不是被测运行时。

## 结果清单

| 检查 | 结果与日志 |
| --- | --- |
| nestwal、nest、dataengine/engine、remoteentity、account、chat、manager，race count=1 | 七包 PASS，`core-baseline.log` |
| cellworld、gateway、wiring/dataengine、wiring/remoteentity，race count=1 | 四包各自 PASS，`secondary-baseline.log`；该命令还误带了不存在的 gameplay/cellworldhost，因此命令整体退出 1，详见下文 |
| codegen/internal/nest，race count=1 | PASS，`nest-codegen.log` |
| codegen/internal/roost 的七项 manager 定向测试，race count=1 | 七项 PASS，`manager-codegen.log`，不是整包全量 |
| 初始 rehome + room module verify | 79 场景、零 skip、四项工程检查 PASS，`module-verify.json` |
| 扩展模块组合 module verify | 248 场景、零 skip、四项工程检查 PASS，`module-verify-more.json`，与初始 79 有重叠，不相加 |
| 上次原探针、core 新增用例、rehome 三项新增回归，race count=3 | 全部 PASS，`old-probe-fix.log`、`copy-fix.log`、`rehome-fix.log` |
| 房间两个乱序场景，race count=3 | 实验六次 FAIL、对照六次 PASS，`room-presence-order.log` |
| 房间取消等待后仍提交，race count=3 | 实验三次 FAIL、对照三次 PASS，`room-presence-cancel.log` |

扩展组合的逐项计数：

| 模块 / 联动 | PASS | SKIP |
| --- | ---: | ---: |
| activity | 21 | 0 |
| activity_alliance | 7 | 0 |
| activity_world | 6 | 0 |
| alliance | 55 | 0 |
| alliance+rehome | 2 | 0 |
| bag | 20 | 0 |
| cellworld | 9 | 0 |
| login | 37 | 0 |
| portal | 12 | 0 |
| rehome | 50 | 0 |
| room | 29 | 0 |

四项工程检查是 `go build ./...`、`go vet ./...`、`roost id check`、`roost generate --check`。模块校验自带 race 的离线测试；没有真实依赖、真实玩家网络或性能承诺。

过程中的两项验证噪声完整保留，均不计为产品缺陷：

- `secondary-baseline.log` 的 `gameplay/cellworldhost` 是审查命令误写的路径；该宿主属于生成模块 `game/modules/cellworldhost`，不是 core 包。其余四个包各自有 PASS 行。宿主实际通过后续生成工程 cellworld 的 9 个正式场景校验。
- 第一轮扩展 verify 执行中，审查者写入尚未编译通过的取消等待探针，干扰了 room 和 vet；原始结果保存在 `module-verify-more-probe-interference.*`。探针修正、定向复现并移出生成工程后，重新完整 verify 得到 `module-verify-more.json` 全绿。不能把审查探针的临时编译错误当作 main 缺陷。

## 生成工程与探针复现

必须从被测 SHA 构建 CLI，并用同一检出作为运行依赖；不要在固定报告分支的旧代码上运行。以下 `CORE` 是该 SHA 的独立检出，`EVIDENCE` 是本证据目录，`WORK` 是新的临时目录：

```bash
export GOWORK=off
mkdir -p "$WORK"
(cd "$CORE" && go build -o "$WORK/roost" ./codegen/cmd/roost)
"$WORK/roost" project new audit -module example.com/mainaudit   -out "$WORK/project" -template game --skip-deps
cd "$WORK/project"
git init -q
go mod edit -replace "github.com/tjbdwanghaibo/roost-core=$CORE"
"$WORK/roost" module add rehome room
"$WORK/roost" module verify --json
# 本轮另执行了扩展组合（运行探针不强制需要）：
"$WORK/roost" module add login portal activity_world activity_alliance cellworld
"$WORK/roost" module verify --json

cp "$EVIDENCE/review_presence_order_test.go.txt" game/controllers/room/review_presence_order_test.go
cp "$EVIDENCE/review_presence_cancel_test.go.txt" game/controllers/room/review_presence_cancel_test.go
go test -race -count=3 -v -run '^TestReviewRoomPresence(PostCommitOrder|CanceledWait)$' ./game/controllers/room
# 预期非零退出；三个实验失败，各自控制组通过。

cp "$EVIDENCE/review_driver_copy_unknown_test.go.txt" game/modules/rehome/review_driver_copy_unknown_test.go
go test -race -count=3 -v -run '^TestReviewCopyCommittedButReplyLost$' ./game/modules/rehome
# 预期原失败探针在本轮 main 通过。
```

基础回归的复现命令（在被测 core 检出）：

```bash
go test -race -count=1 -timeout 6m ./framework/nestwal ./framework/nest   ./framework/dataengine/engine ./framework/remoteentity ./service/account   ./service/chat ./wiring/manager
go test -race -count=1 -timeout 6m ./gameplay/cellworld ./infra/network/gateway   ./wiring/dataengine ./wiring/remoteentity
go test -race -count=1 -timeout 6m ./codegen/internal/nest
go test -race -count=3 -v -run '^TestCopyHandedOffEntityLostCommitReplyListsWhatMayHaveBeenWritten$' ./framework/dataengine/engine
```

rehome 新回归（生成工程）：

```bash
go test -race -count=3 -v -run 'TestRehome(CopyWithALostCommitReply|CopyThatNeverSettles)' ./game/modules/rehome
```

`review_presence_order_test.go.txt` 延迟真实 Nest 已完成的回复，构造控制器完成顺序反转；`review_presence_cancel_test.go.txt` 在真实 Nest 的 strict 提交器内设屏障，让请求先取消等待，再成功完成提交。均使用正式 room 业务逻辑，连接与持久化为替身，无 TCP、Mongo 或故障网络。证据以 `.txt` 保存，避免意外加入仓库默认测试。
