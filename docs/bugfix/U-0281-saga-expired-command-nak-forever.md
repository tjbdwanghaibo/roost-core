# U-0281：原生 saga 步骤消费者把“已过截止时间、又没有回执”的旧尝试无限 nak，堵死共享 durable

**仓库 / 位置**：roost-core `saga/command_consumer.go` `SubscribeDataEngineStep` 的过期分支（修前 `:343`～`:352`）。
**来源**：roost-core 真实进程演练 drill6（2026-10-05，代码 `47a9132c`，生成 game-demo，两个 sid 1300 / 1302 + kill -9 / SIGSTOP）；维护者授权“确认是缺陷就按先红后绿修”。
**没有 RR 编号**（用户直接提出）。**定位文档**：TROUBLESHOOTING T-225。同一演练的另一缺陷见 [U-0280](U-0280-saga-step-reexecuted-after-crash.md)。
**状态**：已修复，未发版。

## 现象（演练证据）

`c6-drill/timeline.txt` 14:36～14:43：sid 1300 的进程 P9 / P10 被 kill -9 期间，赠礼 debit 步骤的各次尝试投递到 sid 1302 的进程，被生成工程的
`Admit`（`admitOwned`：发送方绑定在 1300）拒绝并 nak；每次尝试 5s 后过期，协调器发出下一次尝试。1300 恢复后，最新一次尝试正常执行，
而那些过期的旧尝试仍在 durable 里：

```text
consumer game-gift-debit  delivered=1028 ack_floor=436 num_ack_pending=256 num_redelivered=256 num_waiting=3 num_pending=512
fetch ... stream_seq=469 delivered=29 published=14:28:44.544 data={"version":1,"command":{"id":"gift-100068-demo-100068-1791-7:1:0:2",...
```

`num_ack_pending` 停在 `MaxAckPending=256` 上限，十分钟不动（重启两边进程也不动）；之后两个 sid 新发的赠礼全部超时 Failed
（14:40 的 probe1300 / probe1302 各 1 次，`failure=1`）。抽出的 3 条都是 14:28 发出的旧尝试（`…:1:0:2`，attempt 2），已投递 29 次。
演练只能切到新的 saga stream 继续（14:43）。`game-gift-debit-undo` 同形（`num_ack_pending=18`）。

## 契约与根因

`Command.DeadlineAt` 是协调器给这一次尝试的等待期限：`saga/engine.go` `processClaimed` 派发时 `DeadlineAt: after.NextRunAt`，
即该尝试的超时点；协调器到点后在 `processClaimed` 的 `StatusWaiting` 分支自己按 `retryOrCompensate` 进入下一次尝试或补偿，
**不等待、也不需要**步骤侧对这条旧消息再作任何回答。步骤侧对过期命令只剩一件事：这次尝试若已提交过结果（回执存在），让结果送达。

- Mongo 路径（`SubscribeMongoStep`，`:259`）：过期时 `Replay` 命中就重发 completion，未命中 `err == nil`，返回 nil → ack。**本来就对**；
  任务描述里“`:259` 形状相同”不准确，这里只加守卫用例。
- 原生路径（`SubscribeDataEngineStep`，修前 `:343`）：命中回执返回 nil；**未命中返回 `context.DeadlineExceeded`**。它不是 `nats.Permanent`，
  适配层按 `NakBackoffMin..Max`（250ms～30s）退避 nak，直到 `MaxDeliver=25000`（约 8.7 天）才 Term。这条消息在这期间一直占着一个
  `MaxAckPending` 名额。过期检查在 `Admit` 之前，所以每个进程都会这样 nak 它；共享 durable 的 256 个名额被这类消息占满后，新命令不再投递。

### 为什么 ack 不会丢掉“已执行、回执还没写”的尝试

原生步骤的业务修改、`saga-step/<CommandID>` 回执、lease fence 和 completion effect 是同一条 WAL 记录（SAGA.md「Native Entity step」）：
回执与执行同一事务提交，结果经 **WAL → 投影 → completion effect（Data Engine outbox → EFFECT 流 → `SubscribeNestCompletions`）** 送达协调器，
**从不经过这条命令消息**。所以：

- WAL 已提交、投影还没完成（进程崩溃待重放、投影积压）时，回执查不到；此时 ack 这条过期消息不影响那条记录随后投影（或因 fence
  失效被跳过）以及 completion 送达。修前的无限 nak 只是在等回执出现后再 ack，期间什么也不做；
- 修前修后都不在过期后执行业务，也不发布任何东西，所以不会出现“过期还执行”；
- 读回执遇到暂时性错误仍返回错误重投：那时无法判断有没有回执，保持修前行为。

“已过期未执行”结果**不上报**：协调器自己拥有超时，不需要它；而且 `Engine.Complete` 只比较 `IdempotencyKey`（同一步骤同一方向的
所有尝试共享），一条“尝试 k 未执行”的失败结果若在协调器等待尝试 k+1 时到达，会被当成 k+1 的结果，错误地消耗一次重试。

### 与 U-0280 的关系

U-0280 是“尝试 k 已提交、结果晚到，协调器又发出 k+1，k+1 再执行一次”。本修复不改变那条路径：过期消息原本就不执行业务，
ack 只是让它不再占名额；k 的结果是否晚到、晚到后协调器怎么处理，仍取决于 WAL 投影与 `Engine.Complete`，由 U-0280 的方案决定。
两者不矛盾：U-0280 的任一候选方案都不依赖过期消息继续留在 durable 里。

## 修法

`saga/command_consumer.go` 过期分支：`Replay` 未命中时计数 `saga.step.expired_unexecuted_total`、Info 日志（`command_id` / `saga_id` / `deadline_at`）后返回 nil（ack）；
命中仍返回 nil；读错误仍返回错误。不调用 `Admit`、不 `Reserve`、不执行 handler（修前也不执行）。选 ack 而不是 `nats.Permanent`（Term）：
这是协调器超时后的正常结果，不该走 ERROR 日志与 `nats.jetstream.terminal.total{reason="permanent"}` 的故障口径。

未采用：

- **只把 `MaxDeliver` 调小**：缩短占位时间但不消除；默认值是为实体屏障期间的重投预算定的（SAGA.md RR-20260926-63），不能为此改。
- **过期时 `Term`**：结果相同，但口径错误（见上）。
- **上报“已过期未执行” completion**：见上，会被 `Complete` 误记为当前尝试的结果。

兼容性：行为只在“过期且无回执”这一个分支变化；无 API / 持久格式 / wire 变化。旧版本残留在 durable 里的这类消息，升级后的进程第一次投递到即 ack。

## 先红后绿

回归：`saga/step_expired_promises_test.go` `TestExpiredStepCommandWithoutReceiptIsAcknowledgedNotRedelivered`（五个子用例：原生无回执 / 原生有回执 /
原生读回执出错 / Mongo 无回执 / Mongo 有回执；`Admit` 设为一调用就失败，handler 一调用就失败）。

修前（`12726715` + 新测试）：

```text
--- FAIL: TestExpiredStepCommandWithoutReceiptIsAcknowledgedNotRedelivered/dataengine_without_receipt (0.00s)
    step_expired_promises_test.go:63: expired command without a receipt = context deadline exceeded, want nil (ack): any error is nak'd and redelivered until MaxDeliver, holding a MaxAckPending slot all the while
```

其余四个子用例修前即通过（控制组 / 守卫）。修后五个全部通过。

## 组合契约复核（fix-contract-review）

- **错误分类改变追到调用方**：返回 nil 由 `nats` 适配层 ack；`Admit` 的 nak 语义不变（只对未过期命令调用）；生成工程的 handoff 路径
  （`runHandoff`）本来就对过期命令 `return nil`，与此一致。
- **邻近分支**：未过期但 `Reserve` 得到租约内 `Duplicate` 时，`waitReplay` 等到本命令截止时间返回 ctx 错误 → nak；下一次投递已过期 → 本分支 ack。
  因此“等别人的租约”这条路径现在也有界（修前它会落入无限 nak）。
- **恢复后状态**：被 ack 的旧尝试若 WAL 里有记录，投影 / 跳过照常；claim 行保持 pending 直到 TTL（与修前相同，claim 只是协调行）。

## 验证（`GOWORK=off`，worktree 基于 `12726715`，2026-10-05）

| 命令 | 结果 |
| --- | --- |
| `gofmt -l .` | 空 |
| `go vet ./saga/... ./kit/saga/...` | 通过 |
| `go test -race -count=3 ./saga/... ./kit/saga/...` | ok / ok |
| `go test ./saga/ -run TestExpiredStep -race -count=300` | ok |
| 根包 `go test -count=1 .` | ok |
| `go run ./cmd/glsvet ./nest ./entity ./dataengine/engine ./sync/entitysync` | 通过（无输出） |
| `go build ./... && go vet ./...` | 通过 |
| `bash scripts/test-dataengine-generated.sh`（隔离环境 `~/.roost-it/roost-dataengine-it`，脚本自建自删库） | rc=0，三个进程阶段 + cleanup 全部 PASS |
| 生成 game-demo（`roost project new … -template game-demo`，`replace` 指向本 worktree）`go build ./... && go vet ./... && go test ./...` | 通过，18 个包 ok |

（以上在 C01 结束后执行。）

## 未验证项 / 风险

- 未在真实 NATS 上复现“durable 被占满后恢复”（没有重跑 drill6 的 sid 宕机场景）：单测只证明消费者对过期无回执命令返回 nil；ack 由 `nats` 适配层执行（既有行为）。生成工程测试只证明修改不破坏正常路径。
- Mongo 路径的边角：`AckWait` 小于步骤 `Timeout` 时，同一条消息可能在第一次投递的事务提交前被重投并 ack，第一次投递发布 completion
  失败后无法再重投；结果是这次尝试的 completion 丢失、协调器按超时重试。修前（Mongo 路径本来就 ack）相同，不是本修复引入；默认
  `AckWait` 30s 大于模板步骤 `Timeout` 5s。
