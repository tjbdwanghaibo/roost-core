# v1.23.0 实现 · SAGA / DRV / DAO / REM

v1.19.2 → v1.23.0 双文档的“实现”部分，覆盖四个主题：saga 最终性（SAGA）、Redis / Mongo 驱动契约（DRV）、回滚统一走 DAO（DAO）、remoteentity 快照缓存与 Mirror（REM）。
写给 review agent，也让人能读懂。对应的说明文档是 [guide-saga-drv-dao-rem.md](guide-saga-drv-dao-rem.md)，两份文档用同一编号作锚点（`#saga-3` 这样的小写编号）。

- **源码基准**：发版提交 `02c8a10d`。文中所有 `path:line` 都按这个提交的源码现查；历史记录（feature / bugfix）里的行号、符号名与源码冲突时以源码为准，并在条目里注明。
- **范围**：`git log v1.19.2..02c8a10d` 里属于这四个主题的改动。每条写明首发版本；“v1.23.0（本版）”指 v1.22.0 之后、本次发版的提交。
- **不在本部分**：App 单实例锁、停机契约 A3、readyz（APP）；业务时钟（CLK）；严格配置 A4 / B10（CFG）；skill 编译器（SKILL）；N01～N15 非核心 review 的修复（NONCORE，本部分只在背景里引用 NC-35/36、NC-37～42、NC-61/65/130/131/140 等）；Ops 与指标（OPS）；pretag、门禁脚本（TOOL）。

## 怎么用这份文档 review

### 建议顺序

1. **DRV 先读**。DRV-3（A2 驱动重放契约）定义了“结果未知交给调用方”的口径，后面 REM 的 L2 写入、SAGA 的 Mongo 收件箱、DRV-4 的墓碑 `WAIT` 都按这个口径分类错误。先确认 `driver.IsDefinitelyNotExecuted` 与 `mongo.ErrCommitResultUnknown` 的判定边界，再看调用方。
2. **SAGA 按时间读**：SAGA-1 / 2（U-0281、U-0280 原生步骤收件箱）→ SAGA-6（B1 代际）→ SAGA-7（NC-250）→ SAGA-8（方向① stepTransition）→ SAGA-9（方向② Mongo 收件箱）→ SAGA-10～13（发版前收尾）。后一条常常改写前一条的代码位置，按时间读能看清每一步收窄了什么。
3. **DAO**：DAO-1 是框架契约变化（组件不再持有可回滚状态），DAO-2 是它的明确例外（skill Runtime），DAO-3 是例外边界上的投影入口。
4. **REM 按层读**：REM-1（B2 水位权威与 `admitLocked`）是后面所有 Mirror 步骤的不变量基础；REM-2～6 是 Mirror 第 1～5 步；REM-7～13 是第 6 步本机替代、观察与发版前实测。

### 先读的规范

- `docs/agent-skills/roost-coding/SKILL.md`：“DataEngine 与 Remote”（超时 / 未知结果不等于未提交；Remote 持久写权限由 Mongo ownership / grant / fence 与版本共同校验，Redis 只是竞争协调）、“Nest 与 Entity”里的**回滚统一走 DAO** 与 **B4 例外**、“生命周期与装配的复审要点”（三步停机、Close 统一口径）。
- `docs/agent-skills/roost-coding/references/fix-contract-review.md`：修复改变错误分类、TTL / 版本有效性、取消或关闭所有权时的组合契约复核。本部分的 SAGA-2 / 6 / 9、DRV-3 / 5、REM-1 / 4 都属于这一类。
- 驱动契约表：`redis/driver/README.md`、`mongo/driver/README.md`（§5 Close）。

### 本地复跑

- 单元与 race（不需要外部依赖）：`GOWORK=off go test -race -count=1 ./saga/... ./remoteentity/... ./entity/... ./redis/... ./mongo/... ./skill/... ./internal/operation/...`；改了 nest / entity / dataengine / sync 的补 `go run ./cmd/glsvet ./nest ./entity ./dataengine/engine ./sync/entitysync`。
- 根包门禁：`GOWORK=off go test -count=1 .`（含文档链接、冲突标记、示例实跑）。
- 真实依赖（`-tags integration`）：隔离环境 `~/.roost-it/roost-dataengine-it`，`source ~/.roost-it/roost-dataengine-it/env.sh`（含凭据，只 source、不打印）。**一律加 `-run`** 只跑自己的用例；各条目的“测试”一节给出用例名。
- Mirror / Cluster 的私有依赖进程：`scripts/mirror-local.sh`（自建 Redis 单机 / Cluster、NATS、Mongo 副本集，不碰共享环境），见 REM-8 / REM-12。

### 不能碰的共享资源

- 共享隔离环境的 toxiproxy：不 `/reset`、不删别人的毒；故障注入一律自建代理或进程（维护者决定 A5）。
- 全局运维命令（`dataengine-env.sh up/down/heal/reset/fault`、故障矩阵 `scripts/test-remote-matrix.sh`）必须持有 `remote-acceptance.lock`；锁存在时不跑真实依赖用例。
- 主检出 `/Users/whb/roost/roost-core` 与其 `artifacts/`：只读；review 在独立 worktree 里做。
- 生成工程的 DAO 库名是编译期常量，复跑生成工程用例时改成唯一名字，用完删除。

## 条目总表

| 编号 | 一句话 | 首发 | 行为变化 / 兼容破坏 | 需业务改动 |
| --- | --- | --- | --- | --- |
| [SAGA-1](#saga-1) | 过期且无回执的原生步骤命令直接 ack，不再无限 nak 占满共享 durable（U-0281） | v1.20.1 | 是（nak → ack） | 否 |
| [SAGA-2](#saga-2) | 原生步骤操作实例收件箱：同一操作最多生效一次、租约封顶到命令截止、放弃后迟到成功告警，含两处复核修复（U-0280） | v1.20.1 | 是（跨尝试回放 / 等待 / 接替；投影积压时步骤停住；持久格式只增） | 否（运维按 T-226 处置告警） |
| [SAGA-3](#saga-3) | 步骤超时与重试预算由配置 `saga.step_defaults` / `saga.steps` 提供 | v1.20.1 | 是（零值预算由 `Register` 补齐；写错配置 `Init` 失败） | 否（可选配置） |
| [SAGA-4](#saga-4) | 步骤覆盖原样查不到时按小写回退（RR-20261005-NC-194） | v1.20.2 | 是（无定义时大小写混写的覆盖开始生效） | 否 |
| [SAGA-5](#saga-5) | 步骤预算拒绝只差大小写的类型 / 步骤名（RR-20261006-06，收尾 A12） | v1.23.0（本版） | 是，收紧（这类命名启动失败） | 仅有这类命名的工程要改名 |
| [SAGA-6](#saga-6) | 协调器接收 completion 时核对代际、迟到告警去重、补偿方向人工 Compensate 换代（B1） | v1.20.2 | 是，收紧 | 否（运维：补偿方向 `ManualRequired` 用 `Resume`） |
| [SAGA-7](#saga-7) | 定义缺失 fence 时退避中的步骤同样记为放弃（RR-20261005-NC-250） | v1.21.0 | 是（迟到成功由丢弃改为 ack + 告警） | 否 |
| [SAGA-8](#saga-8) | 离开当前步骤收成一个转移 `stepTransition` + 守卫测试与补漏（saga 方向 ①） | v1.21.0 | 否（唯一差异在正常不可达路径） | 否 |
| [SAGA-9](#saga-9) | Mongo 步骤纳入操作实例收件箱（两事务、`_claims` 集合），结果消费者终态分类统一（saga 方向 ②、O-S5-1、O-S5-3） | v1.21.0 | 是（Mongo 步骤延迟约翻倍；新集合；普通结果流 Term 4 种错误） | 业务：事务外副作用仍需按 `IdempotencyKey` 幂等；运维：建议 `transactionLifetimeLimitSeconds=20` |
| [SAGA-10](#saga-10) | `ErrDefinitionMissing` 改为可重试 nak；回放不交还 claim 列为观察 | v1.21.0 | 是（Term → nak 退避） | 否 |
| [SAGA-11](#saga-11) | Mongo 步骤延迟分析，维护者选 A（接受两次落盘提交） | v1.23.0（本版） | 否（无生产代码改动） | 否 |
| [SAGA-12](#saga-12) | saga Mod 启动时校验 `saga.completion_receipt_ttl > dataengine.effects.max_age`（O-S5-2） | v1.23.0（本版） | 是，收紧（不满足拒绝启动） | 仅调过这两个键的部署 |
| [SAGA-13](#saga-13) | 真实 NATS 上 nak 退避 / `MaxDeliver` 实测，`TestAssemblyConsumesNativeNestCompletionEffects` 偶发失败根因 | v1.23.0（本版） | 否（只改测试） | 否 |
| [DRV-1](#drv-1) | Redis 脚本（Eval / EvalSha / EvalBatchDurable）回复丢失不再被驱动重放，一次调用至多执行一次（RR-20261005-NC-100） | v1.20.1 | 是（收紧）：脚本回复丢失返回传输错误 | 否 |
| [DRV-2](#drv-2) | `mongo.transaction_timeout` 端到端约束事务含提交；EndSession 补发的 abort 有 5s 上限；退避中到期保留最后一次事务错误（RR-20261005-NC-101） | v1.20.1 | 是：分区时更早返回错误，该错误可能已提交 | 否 |
| [DRV-3](#drv-3) | A2：Redis 写 / 含写 pipeline / EvalBatchDurable / DistLock 不经驱动重放，只在 `IsDefinitelyNotExecuted` 时重发；Mongo 提交后失败包 `ErrCommitResultUnknown`；契约表进仓 | v1.20.2 | 是（收紧）：写命令回复丢失返回结果未知；Mongo 错误文本多前缀 | 否（新增调用点按契约表 §6 核对） |
| [DRV-4](#drv-4) | O-M6-3：L2 墓碑写入后同连接 WAIT 副本（驱动能力 `EvalReplicated`），只计数 / Warn 不回滚；新键 `snapshot_l2_tombstone_wait_replicas` / `_timeout` | v1.23.0（本版） | 是：删除 Remote 实体最多多等 50ms（缺省） | 否（运维可调键） |
| [DRV-5](#drv-5) | 驱动与 Mod 的 Close 统一口径：重复 Close 返回 nil、并发后到者等第一个、关闭后返回已关闭错误；`operation.Serial`（第十二轮 + RR-20261006-10） | v1.23.0（本版） | 是：单机 Redis 重复 Close 改为 nil；etcd 关闭后立即 `ErrClosed`；DistLock 遇 `ErrClosed` 不再记未知 | 依赖“第二次 Close 报错”的调用方需改判断 |
| [DRV-6](#drv-6) | O-M6-5：启动建索引遇 Mongo 换主错误码有界重试（每个索引 10 次 × 1s） | v1.23.0（本版） | 是：撞上选举时启动变慢而非失败 | 否 |
| [DAO-1](#dao-1) | A1：回滚统一走 DAO，组件不再持有可回滚状态、不再登记 undo；`nopersist,nosync` 字段有 mutator；glsvet A1 提示 | v1.20.2 | 是（规范）：组件写法改变；生成 DAO 只增方法；持久格式不变 | 新组件按规范；已生成工程不迁移 |
| [DAO-2](#dao-2) | B4：skill Runtime 状态不进事务，写成约束 + 守卫测试 | v1.21.0 | 否（代码行为不变） | 是（设计约束）：先校验后推进 Runtime，扣费交给 Runtime commit |
| [DAO-3](#dao-3) | combatcomponent 属性投影入口 `ProjectAttributes`：投影写 DAO vitals，随 DAO 回滚 | v1.23.0（本版） | 只新增 API；装了投影后被投影字段以投影为准 | 想让 buff 影响伤害的业务写投影函数 |
| [REM-1](#rem-1) | 共享 L2 为快照水位权威，L1 只缓存 L2 确认过的版本；`remote_entity.cached_max_staleness`；复制消息带 `published_at`，过老快照不再接受（O5） | v1.20.2 | 是（收紧：未确认 / 超上限条目先重新确认；L2 `DeleteAtVersion` 被拒返回 `ErrStaleWrite`） | 否 |
| [REM-2](#rem-2) | Mirror 第 1～3 步：`RemoteSnapshotReadOnly` / `RemoteObservation` / `RemoteMirrorReader`、唯一读出口 `Read` + `Covers`、共享 `SnapshotClient` | v1.21.0 | 是（收紧：Monotonic 只回源一次；Cached 最低版本不满足返回 `ErrRemoteSnapshotStale`；Linearizable 需声明；停止后 `ErrSnapshotClientStopped`） | 否（只增 API） |
| [REM-3](#rem-3) | nest `allow_stale` 的 Cached Remote 访问照旧接受低于 `min_version` 的快照 | v1.21.0 | 否（回到 Mirror 之前） | 否 |
| [REM-4](#rem-4) | Mirror 第 4 步：可确认订阅（JetStream DeliverNew，普通 NATS 退化按需）、首载缓冲（64，溢出再回源）、兴趣代际锁内分配与撤销水位 | v1.21.0 | 是（普通 NATS 不再推送；JetStream 换新 durable） | 运维：可删旧 DeliverAll durable |
| [REM-5](#rem-5) | O4 兴趣容量按 consumer 配额：`snapshot_interest_per_consumer`、`ErrInterestQuotaExceeded` / `ErrInterestRegistryFull`、指标 | v1.21.0 | 是（按 consumer 拒绝，可识别、计数） | 否 |
| [REM-6](#rem-6) | Mirror 第 5 步：kit `RemoteMirrorMod`、`remote_entity.mirror.shutdown_timeout`、codegen `//roost:mirror`、`remote=mirror` 迁移诊断、公会摘要两进程样例 | v1.21.0 | 是（生成器对 `remote=mirror` 报错；只读产物需 core ≥ v1.21.0） | 是：`remote=mirror` 改 `//roost:mirror`（仓内无使用方）；装只读 Mod 要调大停机总预算 |
| [REM-7](#rem-7) | RR-20261006-01：删除提交确认时实例已被清空，确认视为完成、照常发布墓碑 | v1.22.0 | 是（strict 删除从“结果未知”变成功并发布） | 否 |
| [REM-8](#rem-8) | Mirror 第 6 步本机替代：`scripts/mirror-local.sh` 私有依赖进程，两进程 7 类故障 0 违例；v1.20.2 对照 n=6 无显著差别 | v1.22.0 | 否（测试设施） | 否 |
| [REM-9](#rem-9) | O-M6-1：owner 启动在 `remote_entity_interest_refresh` 广播“请重新续租”，合并、间隔 ≥ 1s | v1.23.0（本版） | 是（改善；wire 只新增主题） | 否 |
| [REM-10](#rem-10) | O-M6-6：同 sid 重启按进程代际令牌立即接管上一代留下的 Remote 实体锁 | v1.23.0（本版） | 是（开单实例锁时 token 格式变长；只接管同持有者上一代） | 否（需 `singleton.enabled=true` 才受益） |
| [REM-11](#rem-11) | L2 落后权威的上界：`snapshot_l2_ttl + cached_max_staleness`（core 缺省约 5m30s），保持不加后台补写 | v1.23.0（本版，文档） | 否 | 否 |
| [REM-12](#rem-12) | Redis Cluster 迁槽 ASK / MOVED 下 L2 读写与墓碑 WAIT 实测；mirror-local Cluster 就绪判定补“每个主节点有 online 副本” | v1.23.0（本版） | 否 | 否 |
| [REM-13](#rem-13) | 生成配置写出 `remote_entity` 五个新键；生产化不再把墓碑 WAIT 副本数改成 3 | v1.23.0（本版） | 否（只影响新生成工程） | 否 |

各编号链接到本文的实现条目；说明见 [guide-saga-drv-dao-rem.md](guide-saga-drv-dao-rem.md) 同编号。

## SAGA：saga 最终性

<a id="saga-1"></a>
### SAGA-1 过期且无回执的原生步骤命令直接 ack（U-0281）

> 首发 v1.20.1 · [说明](guide-saga-drv-dao-rem.md#saga-1)

**1. 提交**

| 提交 | 版本 | 内容 |
| --- | --- | --- |
| `96720a05` | v1.20.1 | 过期分支未命中回执时计数、Info、返回 nil（ack）；回归 `saga/step_expired_promises_test.go`；SAGA.md、T-225、bugfix 记录、CHANGELOG |
| `877bb66c` | v1.20.1 | U-0280 复核 2：过期分支 ack 前先重发同一操作实例已生效的成功（见 [SAGA-2](#saga-2)） |
| `9669d181` | v1.21.0 | Mongo 步骤过期分支同样先重发（`ackUnexecutedAttempt`，见 [SAGA-9](#saga-9)） |

**2. 改动文件与关键符号**（行号以 `02c8a10d` 为准）

| `path:line` | 符号 | 职责 |
| --- | --- | --- |
| `saga/command_consumer.go:384` | `SubscribeDataEngineStep` | 原生步骤消费者 |
| `saga/command_consumer.go:419` | 过期分支（`!time.Now().Before(command.DeadlineAt)`） | 在 `Admit` / `Reserve` / handler 之前判断过期 |
| `saga/command_consumer.go:441` | `metrics.IncCounter("saga.step.expired_unexecuted_total", ...)` | 无回执、无可重发成功时计数 |
| `saga/command_consumer.go:442` | `slog.Info("saga: step command expired before it ran; ...")` | 带 `command_id` / `saga_id` / `deadline_at` |
| `saga/dataengine_step_inbox.go:135` | `(*DataEngineStepInbox).Replay` | 只读回执，命中时 `markCompleted` |
| `saga/command_consumer.go:302` | `SubscribeMongoStep` 过期分支 | Mongo 路径（本来就 ack；现在先 `ackUnexecutedAttempt`） |

**3. 不变量与强制点**

- 过期命令不开始任何业务：过期判断在 `Admit` 之前（`saga/command_consumer.go:419` 早于 `:449` 的 `config.Admit`），分支内只调 `Replay` 与 `replayOperationSuccess`。
- 读回执出错不 ack：`:430-432` `replayErr != nil` 直接返回错误（nak）。
- 守卫测试：`TestExpiredStepCommandWithoutReceiptIsAcknowledgedNotRedelivered`（`saga/step_expired_promises_test.go`）把 `Admit` 与 handler 设成“一调用就失败”。

**4. 控制流**

1. 解码命令（`decodeStepCommand`，`saga/command_consumer.go:522`）。
2. `now >= DeadlineAt`：`inbox.Replay`；出错 → 返回错误（nak）。
3. 命中回执 → 返回 nil（ack；completion 随投影的 effect 送达）。
4. 未命中 → `replayOperationSuccess` 查同一操作实例另一次尝试已生效的成功：有 → 经 `transport.PublishCompletion` 重发（计 `saga.step.attempt_replayed_total`）后 ack；无 → 计 `saga.step.expired_unexecuted_total`、Info、ack。

**5. 失败与不确定结果**

| 情形 | 处理 | 对调用方的样子 |
| --- | --- | --- |
| 已提交 WAL、尚未投影（进程崩溃待重放 / 投影积压） | ack 这条过期消息；记录随后投影或因 fence 被跳过 | 结果经 WAL → 投影 → completion effect 送达协调器，不依赖这条消息 |
| 读回执暂时性错误 | 返回错误 | nak 重投，与修前相同 |
| 重发同一操作的成功时发布失败 | 返回错误 | nak 重投 |
| Mongo 路径 `AckWait < Timeout` | 第一次投递提交前被重投并 ack；第一次投递发布 completion 失败后无法再重投 | 协调器按超时重试；修前相同（记录“未验证项”） |

**6. 测试**

| 用例 | 文件 | 覆盖 |
| --- | --- | --- |
| `TestExpiredStepCommandWithoutReceiptIsAcknowledgedNotRedelivered`（5 个子用例：dataengine without receipt / with receipt / receipt unreadable、mongo without receipt / with receipt） | `saga/step_expired_promises_test.go` | 过期分支的 ack / 重投分类，`Admit` 与 handler 不被调用 |
| `TestNativeStepExpiredDeliveryStillReplaysTheOperationsSuccess` | `saga/step_operation_review_test.go` | 过期投递 ack 前重发同一操作的成功（U-0280 复核 2） |

修前红（原样，出自 [U-0281 记录](../../bugfix/U-0281-saga-expired-command-nak-forever.md)，基线 `12726715` + 新测试）：

```text
--- FAIL: TestExpiredStepCommandWithoutReceiptIsAcknowledgedNotRedelivered/dataengine_without_receipt (0.00s)
    step_expired_promises_test.go:63: expired command without a receipt = context deadline exceeded, want nil (ack): any error is nak'd and redelivered until MaxDeliver, holding a MaxAckPending slot all the while
```

其余四个子用例修前即通过（控制组 / 守卫）。修后命令与结果（记录“验证”表，原样摘录）：`go test -race -count=3 ./saga/... ./kit/saga/...` → `ok / ok`；`go test ./saga/ -run TestExpiredStep -race -count=300` → `ok`；`bash scripts/test-dataengine-generated.sh` → `rc=0，三个进程阶段 + cleanup 全部 PASS`；生成 game-demo `go build ./... && go vet ./... && go test ./...` → `通过，18 个包 ok`。负对照：记录未保留“退回修复变红”的单独记录（修前红即为对照）。

**7. 性能证据**：无（不涉及热路径，过期分支只多一次计数与日志）。

**8. 未验证项与已知风险**：真实 NATS 上 durable 被占满后的恢复没有复现；`saga.step.expired_unexecuted_total` 没有 `saga_type` 标签，运维只能从 Info 日志区分步骤。

**9. review 检查点**

- [ ] `saga/command_consumer.go:419-448`：确认过期分支里没有任何路径调用 `config.Admit`、`inbox.Reserve` 或 `handler`；`TestExpiredStepCommandWithoutReceiptIsAcknowledgedNotRedelivered` 的 `Admit` / handler “一调用就失败”是否覆盖了全部五个子用例。
- [ ] `:430-432` 与 `:436-438`：`Replay` 或 `replayOperationSuccess` 持续失败（例如 claim 集合权限错误）时会一直 nak 到 `MaxDeliver`——与修前“读回执出错仍重投”同口径，确认这是接受的行为并有日志可查。
- [ ] `saga/command_consumer.go:302-322`（Mongo）：`replayCtx` 只有 3s，`PublishCompletion` 超时返回错误 → nak；确认不会因此在 Mongo 路径重新引入长期 nak。
- [ ] 确认 U-0281 记录“ack 不会丢掉已提交未投影的尝试”的前提仍成立：原生尝试的 completion 只经 effect 送达（`SubscribeDataEngineStep` 注释“never publishes the completion directly”，`saga/command_consumer.go:376-383`），唯一例外是 U-0280 之后的回放重发。

<a id="saga-2"></a>
### SAGA-2 原生步骤操作实例收件箱：同一操作最多生效一次（U-0280）

> 首发 v1.20.1 · [说明](guide-saga-drv-dao-rem.md#saga-2)

**1. 提交**

| 提交 | 版本 | 内容 |
| --- | --- | --- |
| `50e9a4e8` | v1.20.1 | 方案（未实施）：定位、确定性复现（evidence `u0280_repro_test.go.txt` / `repro-red.txt`）、候选 A～F、方向判断 |
| `054fdd66` | v1.20.1 | 实施 A + B + C'：收件箱按操作实例、租约封顶、tombstone `closure`、`CompletionHistoryStore`、迟到告警；步骤预算配置化（[SAGA-3](#saga-3)）；codegen / demo 同步 |
| `6be347db` | v1.20.1 | SAGA.md「原生步骤执行契约」、USER_GUIDE、T-226、kill -9 修前修后证据与 harness |
| `23b97942` | v1.20.1 | 复核 1：以失败关闭的 operation 记为放弃关闭（`mongo_store.go` closure 判定） |
| `877bb66c` | v1.20.1 | 复核 2：过期投递 ack 前重发同一操作已生效的成功（`operationSuccess` / `replayOperationSuccess`） |
| `69016a3e` | v1.20.1 | U-0280 记录补独立复核两处修复 |
| `23f82dbc` | v1.20.1 | 复核记录改用 rebase 后的提交号 |
| `3fabe34d` | v1.20.2 | B1：`commandIncarnation` 改调协调器的 `commandIDIncarnation`（[SAGA-6](#saga-6)） |
| `9669d181` | v1.21.0 | 操作实例代码原样移到 `saga/step_operation_inbox.go` 的 `stepOperationInbox`，原生与 Mongo 收件箱共用（[SAGA-9](#saga-9)） |

**2. 改动文件与关键符号**

| `path:line` | 符号 | 职责 |
| --- | --- | --- |
| `saga/step_operation_inbox.go:63` | `stepOperationInbox` | 两种收件箱共用的 claim / 守卫 / 判定 |
| `saga/step_operation_inbox.go:92` | `stepClaim` | claim 文档：`operation_key`、`incarnation`、`superseded_by`、`lease_until`、`lease_token`、`status`、`expires_at` |
| `saga/step_operation_inbox.go:112` | `ensureClaimIndexes` | 索引 `claim_expired`、`uniq_command`（唯一）、`ttl_expires_at`、`by_operation` |
| `saga/step_operation_inbox.go:124` | `reserve` | 一个 Mongo 事务里跑 `reserveInTransaction`；撞唯一键重试一次 |
| `saga/step_operation_inbox.go:162` | `reserveInTransaction` | 回执 → 截止 → 自己的 claim → 守卫 → 其他尝试 → 新建 / 接管 |
| `saga/step_operation_inbox.go:180` | 租约封顶 | `leaseUntil = min(now+leaseDuration, DeadlineAt)` |
| `saga/step_operation_inbox.go:247` | `guardOperation` | upsert `saga-step-op/<IdempotencyKey>`，`$inc seq` 制造写冲突 |
| `saga/step_operation_inbox.go:262` | `resolveOtherAttempts` | 成功任何一生回放、本生拒绝回放、可重试失败放行、在途 → 等、过期 → 接替 |
| `saga/step_operation_inbox.go:322` | `operationSuccess` | 只读查同一操作已生效的成功（复核 2） |
| `saga/step_operation_inbox.go:369` | `supersede` | `status=superseded`、`lease_token+1`，计 `saga.step_inbox.superseded_total` |
| `saga/step_operation_inbox.go:413` | `commandIncarnation` | 从 `CommandID` 取代际（B1 起调 `commandIDIncarnation`） |
| `saga/step_operation_inbox.go:422` | `releaseLease` | 交还未用上的租约（`lease_until = now`） |
| `saga/dataengine_step_inbox.go:94` | `Bind` | 把 lease fence（owner / token / digest）绑进 Nest 事务 |
| `dataengine/lease_fence.go:75` | `LeaseFence.Predicate` | 投影时的条件：owner、token、digest、`pending`、`lease_until > now` |
| `saga/command_consumer.go:452-507` | `SubscribeDataEngineStep` Reserve 分支 | `ErrCommandExpired` / `errAttemptSuperseded` ack；回放他人结果经结果流重发；执行后 `waitReplay` |
| `saga/command_consumer.go:513` | `replayOperationSuccess` | 不执行的投递在 ack 前重发同一操作的成功 |
| `saga/engine.go:474` | `completeNotWaiting` | 回执 / tombstone 判重复或“放弃后迟到” |
| `saga/engine.go:530` | `reportLateAfterAbandon` | ERROR + `saga.completion.late_after_abandon_total{saga_type,phase}` |
| `saga/mongo_store.go:161` | `CompletionHistory` | 先查回执，再查 tombstone 与 `closure` |
| `saga/mongo_store.go:328` | `Apply` 的 closure 判定 | 只有 `Receipt.Success` 才记 `result`，其余 `abandoned`（复核 1） |
| `saga/store.go:65-93` | `OperationClosure` / `CompletionHistory` / `CompletionHistoryStore` | 可选扩展，`Store` 接口不变 |

**3. 不变量与强制点**

| 不变量 | 强制点 | 守卫测试 |
| --- | --- | --- |
| 同一操作实例的并发 Reserve 串行化 | 守卫文档写在同一事务里（`saga/step_operation_inbox.go:210`），Mongo 写冲突让后者重跑并看到前者的 claim | `TestRealMongoConcurrentAttemptsOfOneOperationReserveOnce`（真实 Mongo；mongotest 按集合检测写冲突，证明不了守卫） |
| 尝试只在截止前生效 | 租约封顶 `:180-183`（新建 `:217-227` 与接管 `:229-233` 都用 `leaseUntil`）；投影条件 `lease_until > now`（`dataengine/lease_fence.go:82`） | `TestNativeStepLeaseNeverOutlivesTheCommandDeadline`；`TestNativeStepTakesEffectAtMostOncePerOperation/c` |
| 接替与生效只能有一个提交 | `supersede` 与投影的 `Confirmation` 写同一个 claim 文档（`dataengine/lease_fence.go:86-94`） | `TestRealMongoSupersedeAndProjectionOfTheSameAttemptSerialize`（40 轮，记录本次 39 / 1） |
| 已生效的成功不被重做、不被丢失 | `resolveOtherAttempts` 成功回放 `:280-282`；不执行的投递经 `replayOperationSuccess` 重发 | `TestNativeStepTakesEffectAtMostOncePerOperation/a`、`/b`；`TestNativeStepExpiredDeliveryStillReplaysTheOperationsSuccess` |
| 放弃之后才到的成功可见 | tombstone `closure`（`saga/mongo_store.go:328-341`）+ `completeNotWaiting` | `.../c'`、`TestNativeStepSuccessAfterAFailureClosedOperationIsAlarmed`、`TestMongoStoreTombstoneTellsAbandonedFromResolved` |

**4. 控制流 / 状态机**

claim 文档状态机（每次尝试一份，`saga-step/<CommandID>`；守卫文档另算）：

```mermaid
stateDiagram-v2
    [*] --> pending: Reserve 新建 claim，lease_token=1，租约封顶到 DeadlineAt
    pending --> pending: 同一命令重投且租约有效，返回 Duplicate
    pending --> pending: 自己的租约已过期，接管并 lease_token+1
    pending --> pending: releaseLease，lease_until 设为 now
    pending --> completed: 生效点条件写成功，原生投影或 Mongo settleOwnClaim
    pending --> completed: Reserve 或 Replay 读到回执后 markCompleted
    pending --> superseded: 同一操作另一尝试 Reserve 时租约已过期，supersede 并 lease_token+1
    completed --> [*]: expires_at 到期 TTL 删除
    superseded --> [*]: expires_at 到期 TTL 删除
```

Reserve 判定（`reserveInTransaction`，同一事务内）：

```mermaid
flowchart TD
    A[读本命令回执] -->|有| R1[回放，Duplicate 加 Completion]
    A -->|无| B{now 不早于 DeadlineAt}
    B -->|是| E1[ErrCommandExpired]
    B -->|否| C{本命令已有 claim}
    C -->|completed| R2[回放 claim 里的 completion]
    C -->|superseded| E2[errAttemptSuperseded]
    C -->|pending 且租约有效| R3[Duplicate，等本尝试结果]
    C -->|无或 pending 已过期| G[写守卫 saga-step-op]
    G --> H{同一操作其他尝试}
    H -->|有成功，任何一生| R4[回放那次成功]
    H -->|有本生拒绝| R5[回放那次拒绝]
    H -->|有 pending 且租约有效| E3[errOperationAttemptInFlight]
    H -->|pending 已过期| S[supersede 后继续]
    H -->|只有可重试失败或没有| N[新建或接管自己的 claim，执行]
    S --> N
```

原生执行：消费者把 `Reservation` 放进 ctx（`withReservation`），handler 在 Nest 事务里 `Bind` + `EmitCompletion`；回执、lease fence 与 completion effect 同一条 WAL 记录；消费者 `waitReplay`（25ms 轮询）等回执投影后 ack。

**5. 失败与不确定结果**

| 情形 | 处理 | 对调用方的样子 |
| --- | --- | --- |
| kill -9 后 WAL 重放，截止已过 | 投影条件 `lease_until > now` 不匹配，记录被跳过，受影响实体驱逐重载（RR-20260926-30） | 不留回执、不发 completion；协调器按超时重试 |
| 投影积压超过 `Timeout` | 每次尝试都在截止后投影、被跳过 | 步骤停住直到积压消退或重试用尽；`dataengine.fence.skipped.total{resource="_dataengine_inbox_claims"}` 与 `saga.step_inbox.superseded_total` 增长 |
| handler 以 `ErrFencedEntityPending` 失败 | 交还租约（`saga/command_consumer.go:496-501`） | nak，屏障解除后重投立即重新 Reserve |
| 另一尝试租约有效 | `errOperationAttemptInFlight` | nak，重投时再判断 |
| 尝试 k 的成功在退避期间被协调器丢弃（`ErrNotWaiting`） | k+1 的 Reserve 回放 k；若 k+1 已过期，过期分支重发 k 的成功 | 协调器收到 k 的成功（`CommandID` 是 k 的），不重复执行 |
| 成功在协调器放弃后到达 | `completeNotWaiting` 告警，返回 `record, nil` | 消费者 ack；ERROR + 计数；运维按 T-226 处置 |
| 回执 / tombstone TTL 之后的迟到成功 | 协调器无记录 | `ErrNotWaiting` → Term；不告警、不生效（守卫用例） |
| `markCompleted` 失败（Reserve 第 1 步） | 只记 Warn + `saga.step_inbox.mark_completed_error_total`，回执仍是权威 | Reserve 结论不变 |

**6. 测试**

| 用例 | 文件 | 覆盖 |
| --- | --- | --- |
| `TestNativeStepTakesEffectAtMostOncePerOperation`（a / b / c / c' / d） | `saga/step_operation_promises_test.go` | 三种交错 + 截止后重放跳过 + 投影积压接替；走真实 `SubscribeDataEngineStep` 与 dataengine `MongoStore.Project` |
| `TestNativeStepOperationInterleavingsWithCoordinatorDecisions`（Resume 回放、saga 截止告警、人工 Compensate 退避中放弃告警、可重试失败不挡、本生拒绝回放、TTL 后不告警） | 同上 | 协调器决定 × 收件箱 |
| `TestNativeStepLeaseNeverOutlivesTheCommandDeadline` | 同上 | 新建 / 接管封顶、过期 `ErrCommandExpired` |
| `TestNativeStepConsumerHandlesOperationOutcomes`（被接替 ack、在途 nak、零值 Completion 不 nak） | 同上 | 消费者分支 |
| `TestMongoStoreTombstoneTellsAbandonedFromResolved` | 同上 | abandoned / result / 旧 tombstone / Resume 升级 |
| `TestNativeStepSuccessAfterAFailureClosedOperationIsAlarmed`、`TestMongoStoreTombstoneOfAFailureCloseIsAbandoned` | `saga/step_operation_review_test.go` | 复核 1 |
| `TestNativeStepExpiredDeliveryStillReplaysTheOperationsSuccess` | 同上 | 复核 2 |
| `TestRealMongoConcurrentAttemptsOfOneOperationReserveOnce`、`TestRealMongoSupersedeAndProjectionOfTheSameAttemptSerialize` | `saga/step_operation_real_mongo_integration_test.go`（`-tags integration`） | 真实服务端写冲突与接替 / 投影串行化 |
| `TestDataEngineStepInboxReservesCommandIdentityAndAllowsNewAttempt`、`TestDataEngineStepInboxUsesAbsoluteClaimExpiry`（按新契约改写） | `saga/dataengine_step_inbox_test.go` | 在途 / 截止后接替；索引数 3 → 4 |

修前红（原样，基线 `50e9a4e8`，全文 [formal-red-before.txt](../../bugfix/evidence/U-0280/formal-red-before.txt)，节选）：

```text
    step_operation_promises_test.go:56: operation gift-1:1:0 took effect 2 time(s) with 2 success receipt(s), want 1: debits by [gift-1:1:0:1 gift-1:1:0:2] (handler ran 2 times)
    step_operation_promises_test.go:93: attempt gift-3:1:0:1 was replayed 55s after its deadline and still took effect: the claim lease outlived the command deadline, so the saga stays failed with an uncompensated debit
    step_operation_promises_test.go:117: success of gift-4:1:0:1 arrived after the coordinator abandoned the step; saga.completion.late_after_abandon_total grew by 0, want 1 (duplicates grew by 1): an effective but uncompensated step must be visible
    step_operation_promises_test.go:133: attempt gift-5:1:0:1 was projected after attempt gift-5:1:0:2 superseded it: the step took effect twice
    step_operation_promises_test.go:169: operation gift-6:1:0 took effect 2 time(s) with 2 success receipt(s), want 1: debits by [gift-6:1:0:1 gift-6:1:0:r1:1] (handler ran 2 times)
    step_operation_promises_test.go:253: a refused step ran 2 times in one life, want 1
```

复核两处的红（原样，出自 [U-0280 记录](../../bugfix/U-0280-saga-step-reexecuted-after-crash.md)“复核”，记录里本身带省略号）：

```text
attempt gift-1:1:0:2 took effect after the coordinator closed the operation on a stale retryable failure; saga.completion.late_after_abandon_total grew by 0, want 1
... the expired delivery of gift-N:1:0:2 was acknowledged without replaying it: the saga ended failed with CompletedSteps=0 and no late_after_abandon alarm
```

负对照（原样，[real-mongo-guard-disabled-red.txt](../../bugfix/evidence/U-0280/real-mongo-guard-disabled-red.txt)，临时让 `guardOperation` 直接返回）：

```text
    step_operation_real_mongo_integration_test.go:95: round 1: 6 attempts of one operation reserved a live lease at the same time, want exactly 1 (results=[<nil> <nil> <nil> <nil> <nil> <nil>])
--- FAIL: TestRealMongoConcurrentAttemptsOfOneOperationReserveOnce (0.28s)
```

kill -9 复现（生成 game-demo，单 sid，三轮 kill -9；记录表格）：修前 u0280c 154 个 saga，debit 回执 >1 共 7、退款 >1 共 3，背包 30 / 27 / 29；修后 u0280b 164 个 saga，0 / 0，背包 30 / 30 / 30。修后原文（[u0280b/analysis.txt](../../bugfix/evidence/U-0280/kill9/u0280b/analysis.txt)）：

```text
tag=u0280b sagas=164 status={"5":164} (4=completed 5=compensated 6=failed 7=manual)
success receipts: debit=164 refund=164; sagas with debit>1=0 refund>1=0 failed_with_debit=0 compensated_debit!=refund=0
```

修后命令（记录“验证”）：`go test -race -count=3 ./saga/... ./kit/saga/...` 通过；`-race -count=200 -run 'TestNativeStep|TestDataEngineStepInbox|TestStepBudget'` 通过；`go test -tags integration -run 'TestRealMongo(ConcurrentAttempts|SupersedeAndProjection)' ./saga/` 通过；`bash scripts/test-dataengine-generated.sh` 通过；生成 game-demo build / vet / test 通过。

**7. 性能证据**：每次新建 / 接管 claim 多一次守卫 upsert 与一次 `by_operation` 索引查询。mongotest `BenchmarkDataEngineStepReservation/new_command` 0.52 → 1.33 ms/op、`duplicate_active_claim` 4.8 → 5.5 µs/op（记录原文；替身按集合快照，不代表真实 Mongo；样本数记录未写）。真实 Mongo 上的 Reserve 开销在 U-0280 时未测，[SAGA-11](#saga-11) 的分解给出 Reserve 事务约 5 条命令 + 1 次落盘提交。

**8. 未验证项与已知风险**：混跑未实跑；时钟偏差未注入；真实进程下投影积压超过 `Timeout` 未实跑（只有单元用例 d）；“截止前已投影、放弃后才送达”只在单测构造。

**9. review 检查点**

- [ ] `saga/step_operation_inbox.go:209-215`：确认守卫写（`guardOperation`）发生在 `resolveOtherAttempts` 读同一操作 claim **之前**、且在同一事务里；看 `TestRealMongoConcurrentAttemptsOfOneOperationReserveOnce` 的负对照证据是否仍能复现（守卫被跳过时 6 个并发尝试都拿到租约）。
- [ ] `:180-183` 与 `:229-233`：接管自己过期 claim 时 `$set lease_until` 用的也是封顶后的 `leaseUntil`；过滤条件 `lease_until $lte now` + `lease_token` 保证只接管一次。
- [ ] `:115` 唯一索引 `uniq_command` 是 `(namespace, command_id)`；守卫文档的 `command_id` 写的是 `operationKey`、`namespace` 是 `saga-step-op`（`:251`），确认它与 claim（`namespace=saga-step`）不会互撞。
- [x] `:45` `maxOperationAttempts = 4096`、`:311-316` 超过即 `ErrConflict`：每一生最多 1000 次尝试，多次 Resume 之后同一操作的 claim 数在 30 天 TTL 内是否可能超过 4096 导致这一步永远 Reserve 失败？
  **已闭环（fixs，RR-20261006-15）**：会，五生约 4100 次尝试后新一生 Reserve 每次报 `ErrConflict`（越界是拒绝、不溢出、claim 停在 4097），修好原因再 Resume 也执行不了，直到旧 claim 过 TTL。已修：claim 写 `outcome`，按操作只取 pending / 成功 / 本生拒绝 / 旧 claim，结果集有界（证明见 `operationClaimsFilter` 注释），上限改名 `maxDecisiveOperationClaims`；`operationSuccess` 同一查询、超限报错。[问题](../../bug/RR-20261006-15.md)、[修复](../../bugfix/RR-20261006-15.md)。
- [ ] `:164-174` 与 `:359-361`：Reserve 第 1 步 `markCompleted` 失败只告警，`attemptResult` 里 `markCompleted` 失败却让整个 Reserve 失败——确认这种不对称是有意的（后者在事务里，失败会中止重跑）。
- [ ] `saga/command_consumer.go:470-475`（原生 `errAttemptSuperseded`）不重发同一操作的成功，而 Mongo 路径 `:331`（含 `errAttemptSuperseded`）会重发：确认原生侧“接替者负责回放或执行”足以覆盖被接替投递恰好是最后一次的情形。
- [ ] `saga/mongo_store.go:328-341`：只有 `Receipt.Success` 记 `result`；已存在 `abandoned` 的 tombstone 在新一生带结果关闭时升级为 `result`；确认反方向（`result` 不会被降级为 `abandoned`）。
- [ ] 时钟：租约封顶读步骤进程 `o.now()`（`:176`），`DeadlineAt` 由协调器按自己的 `now` 算（`saga/engine.go:765`）；确认文档里“依赖时钟偏差远小于 `Timeout`”是唯一的假设，没有别处再依赖跨进程时钟。

<a id="saga-3"></a>
### SAGA-3 步骤超时与重试预算由配置提供

> 首发 v1.20.1 · [说明](guide-saga-drv-dao-rem.md#saga-3)

**1. 提交**

| 提交 | 版本 | 内容 |
| --- | --- | --- |
| `054fdd66` | v1.20.1 | core `StepBudget` / `StepBudgets` / `Resolve` / `Validate`、`Options.StepBudgets`；kit `kit/saga/step_budgets.go`；codegen `add saga` 不写预算、生成配置带 `step_defaults`；demo debit 覆盖改走配置 |
| `f9367785` | v1.20.2 | RR-20261005-NC-190：预算时长改 `app.ConfigDuration` 严格读取（见 CFG 部分） |
| `3e3350d5` | v1.20.2 | A4：`max_attempts` 改 `app.ConfigInt` 严格读取（见 CFG 部分） |
| `48b3311a` | v1.20.2 | NC-194 小写回退（[SAGA-4](#saga-4)） |
| `611d5d72` | v1.23.0（本版） | RR-20261006-06 歧义报错（[SAGA-5](#saga-5)） |

**2. 改动文件与关键符号**

| `path:line` | 符号 | 职责 |
| --- | --- | --- |
| `saga/record.go:83` | `StepBudget` | 四个字段，零值 = 未指定 |
| `saga/record.go:91` | `DefaultStepBudget` | 5s / 5 次 / 100ms..5s |
| `saga/record.go:110` | `StepBudgets{Defaults, Overrides}` | 配置默认值与按步骤覆盖 |
| `saga/record.go:120` | `StepBudgets.Resolve` | 逐字段取值：覆盖 > 定义 > Defaults > 内置 |
| `saga/record.go:139` | `StepBudgets.Validate` | 负数、超过 1000 次、退避上限小于下限 → `ErrInvalidDefinition` |
| `saga/engine.go:160` | `NewEngine` 校验 `options.StepBudgets` | 构造时拒绝 |
| `saga/engine.go:191` | `Engine.Register` | 先 `Resolve` 再 `Definition.Validate` |
| `saga/record.go:185` | `Definition.Validate` | 补齐后仍要 `Timeout>0`、`1≤MaxAttempts≤1000`、`BackoffMin>0`、`BackoffMax≥BackoffMin` |
| `kit/saga/step_budgets.go:35` | `StepBudgetsFromConfig` | 读 `saga.step_defaults` 与 `saga.steps.<type>.<step>` |
| `kit/saga/step_budgets.go:116` | `readStepBudget` | 未知字段拒绝；时长严格读取且 >0；`max_attempts` 1..1000 |
| `kit/saga/mod.go:126` | `Mod.Init` 调用 | 结果写入 `m.config.Engine.StepBudgets` |
| `codegen/internal/roost/catalog.go:85` | saga Mod 生成配置 | 带 `step_defaults` 与 `steps: {}` |
| `codegen/internal/roost/demo.go:235` | game-demo 补丁 | 写 `saga.steps.gift_item.debit.max_attempts: 15` |

**3. 不变量与强制点**：协调器实际使用的定义 = `Register` 时补齐后的定义（`saga/engine.go:192`）；取值上限 1000 在 kit（`kit/saga/step_budgets.go:150`）与 core（`saga/record.go:142`、`saga/record.go:192`）各查一次。守卫：`TestStepBudgetsComeFromConfigWithPerStepOverrides`、`TestStepBudgetConfigRejectsTyposAndImpossibleValues`（`kit/saga/step_budgets_test.go`）；demo 的 `gift_saga_budget_test.go`（经 `kitsaga.StepBudgetsFromConfig` 读两份配置核对“退款重试窗口 ≥ startup_wait + ttl + 45s”）。

**4. 控制流**

1. saga Mod `Init`：严格读取其余键 → `StepBudgetsFromConfig(cfg, m.definitions...)`。
2. 读 `saga.step_defaults`，与内置默认逐字段合并（`mergeStepBudget`）。
3. 有定义时建已知表（[SAGA-5](#saga-5) 起冲突报错），`saga.steps` 下每个 `<type>.<step>` 必须在表里，键还原成定义里的写法；无定义时以小写键保存。
4. `budgets.Validate()`。
5. `Assemble` → `Engine.Register(definition)` → `Resolve`（原样 → 小写回退，[SAGA-4](#saga-4)）→ `Definition.Validate`。

**5. 失败与不确定结果**

| 情形 | 处理 | 对调用方的样子 |
| --- | --- | --- |
| `saga.steps` 写错类型 / 步骤（有定义） | `Init` 返回错误 | 进程启动失败，报错点名键 |
| 未知字段、`max_attempts` 越界、时长非正或不带单位 | `Init` 返回错误 | 同上 |
| 无定义（`NewMod()`）时写错名字 | 无法核对，保存为小写覆盖，`Resolve` 找不到即不生效 | 静默不生效（按源码推断） |
| 覆盖只给 `backoff_min`，大于实际生效的 `backoff_max` | kit `Validate` 只看单个覆盖内的上下限，放行；`Register` 时 `Definition.Validate` 失败 | `ErrInvalidDefinition: step N`，不点名配置键（按源码推断，未验证） |

**6. 测试**

| 用例 | 文件 | 覆盖 |
| --- | --- | --- |
| `TestStepBudgetsComeFromConfigWithPerStepOverrides` | `kit/saga/step_budgets_test.go` | 默认值、按步骤覆盖、优先级 |
| `TestStepBudgetConfigRejectsTyposAndImpossibleValues`（unknown saga / step / field、zero attempts、too many attempts、negative timeout、inverted backoff） | 同上 | `Init` 拒绝 |
| `TestStepBudgetDurationsRefuseValuesWithoutAUnit` | `kit/saga/config_types_promises_test.go` | 见 CFG 部分 |

修前红：U-0280 记录写 demo 预算用例改回 5 次时照原文红（原样，出自 [U-0280 记录](../../bugfix/U-0280-saga-step-reexecuted-after-crash.md)）：`a refund gets at least 25.75s of retries (max_attempts 5 x timeout 5s ...) ... = 1m30s`。其余新用例的修前红记录未保留原文。修后：随 U-0280 整体验证通过（`-race -count=200 -run '...|TestStepBudget'`）。

**7. 性能证据**：无（只在启动时执行）。

**8. 未验证项与已知风险**：见第 5 节两条推断；已生成工程 `definition.go` 里写死的预算仍生效，运维只能用按步骤覆盖压过它。

**9. review 检查点**

- [ ] `kit/saga/step_budgets.go:44` 与 `saga/record.go:141-147`：`step_defaults` 只写 `backoff_min: 10s`（大于内置 `backoff_max` 5s）时，合并后的 Defaults 会被 `Validate` 拒绝——确认这是期望（运维必须同时写 `backoff_max`），且报错能看懂。
- [ ] 同一情形放在 `saga.steps.<type>.<step>` 下：覆盖本身通过 `Validate`，`Resolve` 后 `BackoffMin > BackoffMax`，在 `saga/record.go:192` 以 `ErrInvalidDefinition: step N` 失败；是否应在 kit 层把覆盖与定义 / 默认合并后再校验并点名配置键？
- [ ] `saga/record.go:120-135`：同一覆盖作用于该类型的所有定义版本，确认新旧版本步骤名相同但语义不同时这是可接受的。
- [ ] `codegen/internal/roost/catalog.go:85` 生成的 `step_defaults` 与 `DefaultStepBudget()` 数值一致（5s / 5 / 100ms / 5s），后续改其中一处时是否有测试钉住两者一致？
- [ ] `kit/saga/mod.go:126` 在 `read.Err()` 之后调用；`StepBudgetsFromConfig` 内部用 `app.ConfigDuration` / `app.ConfigInt` 单独报错，确认与 `ValidateServiceConfig` 的 `frameworkDurationKeys` 不会对同一键给出两种不同报错（见 CFG 部分）。

<a id="saga-4"></a>
### SAGA-4 步骤覆盖原样查不到时按小写回退（RR-20261005-NC-194）

> 首发 v1.20.2 · [说明](guide-saga-drv-dao-rem.md#saga-4)

**1. 提交**

| 提交 | 版本 | 内容 |
| --- | --- | --- |
| `efe219c1` | v1.20.2 | N14 审查登记 NC-190～194 |
| `48b3311a` | v1.20.2 | `Resolve` 小写回退；`StepBudgetsFromConfig` 注释；回归用例 |
| `0aa2e1b9` | v1.20.2 | N14 收口：修复状态、索引、CHANGELOG |

**2. 改动文件与关键符号**

| `path:line` | 符号 | 职责 |
| --- | --- | --- |
| `saga/record.go:120` | `StepBudgets.Resolve` | `:124` 原样查找，`:125-127` 查不到再按 `strings.ToLower` 查 |
| `kit/saga/step_budgets.go:33-34` | `StepBudgetsFromConfig` 注释 | 写明无定义时以小写键保存、由 `Resolve` 回退 |
| `kit/saga/step_override_case_promises_test.go` | 回归用例 | 有 / 无定义两条路径 |

**3. 不变量与强制点**：原样写法优先于小写回退（`saga/record.go:124-127`），守卫 `TestExactOverrideWinsOverTheLowercaseFallback`。

**4. 控制流**：`Resolve` 对每个步骤：`Overrides[{Type, Step}]` → 未命中则 `Overrides[{lower(Type), lower(Step)}]` → 逐字段 `firstDuration` / `firstAttempts`。

**5. 失败与不确定结果**

| 情形 | 处理 | 对调用方的样子 |
| --- | --- | --- |
| 无定义、类型 `GiftItem` 与 `giftitem` 同时登记 | 两者原样都查不到时都命中同一条小写覆盖 | 两个类型拿到同一份覆盖，不报错（按源码推断） |
| 代码里同时给原样与小写两份覆盖 | 原样优先 | 守卫用例钉住 |

**6. 测试**

| 用例 | 文件 | 覆盖 |
| --- | --- | --- |
| `TestPerStepOverrideAppliesToMixedCaseNamesWithoutDefinitions` | `kit/saga/step_override_case_promises_test.go` | 有 / 无定义两条路径都得到 15 次 |
| `TestExactOverrideWinsOverTheLowercaseFallback` | 同上 | 原样优先 |

修前红（原样，[nc194-red.txt](../../review/evidence/noncore-review-20261005-n14/nc194-red.txt)）：

```text
# 基线 f6245613，修复前；GOWORK=off go test -count=1 -run TestPerStepOverrideAppliesToMixedCaseNamesWithoutDefinitions ./kit/saga
--- FAIL: TestPerStepOverrideAppliesToMixedCaseNamesWithoutDefinitions (0.00s)
    step_override_case_promises_test.go:29: withDefinitions=false: GiftItem/Debit max_attempts = 5, want the configured 15 (overrides map[{giftitem debit}:{0s 15 0s 0s}])
FAIL
```

修后（[修复记录](../../bugfix/RR-20261005-NC-194.md)）：两条用例通过；`saga`、`kit/saga` `-race -count=3` 通过。负对照：记录未保留。

**7. 性能证据**：无（启动时一次 map 查找）。

**8. 未验证项与已知风险**：只差大小写的两个类型同时登记时无法区分（审查 O2）；有定义路径在 [SAGA-5](#saga-5) 改为报错，无定义路径仍是第 5 节的推断行为。

**9. review 检查点**

- [ ] `saga/record.go:124-127`：回退只在“整对 `{Type, Step}` 原样未命中”时发生；确认不存在“类型原样、步骤小写”这种混合键的写入路径（`StepBudgetsFromConfig` 无定义时两者都小写，有定义时都还原）。
- [ ] `kit/saga/step_budgets.go:66-81`：无定义时 `key` 就是 viper 的小写键；确认 `NewMod()` + `Engine.Register` 的生成路径（项目没有登记 saga 时）是唯一会走到回退的路径。
- [ ] 无定义时两个只差大小写的类型同时命中同一覆盖：是否需要在 `Engine.Register` 层检测（`Engine` 知道全部已注册类型）？
- [ ] `TestPerStepOverrideAppliesToMixedCaseNamesWithoutDefinitions` 直接调 `budgets.Resolve` 而不是经 `Engine.Register`：确认 `Register`（`saga/engine.go:191-193`）没有在 `Resolve` 之外再做大小写处理。

<a id="saga-5"></a>
### SAGA-5 步骤预算拒绝只差大小写的类型 / 步骤名（RR-20261006-06，收尾 A12）

> 首发 v1.23.0（本版） · [说明](guide-saga-drv-dao-rem.md#saga-5)

**1. 提交**

| 提交 | 版本 | 内容 |
| --- | --- | --- |
| `611d5d72` | v1.23.0（本版） | 收尾第 4 批：`knownSagaSteps` 歧义报错与用例（A12）；同提交里的 A14（RR-20261006-08，mongotest `$in` 具名切片）让 saga 用例去掉绕行、改走完整 `ClaimDue`，mongotest 部分不属于本主题 |
| `53fd9e9c` | v1.23.0（本版） | DECISIONS-PENDING 收尾第 4 批标为已实施 |

**2. 改动文件与关键符号**

| `path:line` | 符号 | 职责 |
| --- | --- | --- |
| `kit/saga/step_budgets.go:93` | `knownSagaSteps` | 按小写键建已知表；同一小写键对应两个不同原名即报错 |
| `kit/saga/step_budgets.go:99` | 类型冲突报错 | `saga definitions: types %q and %q differ only in case; ...` |
| `kit/saga/step_budgets.go:108` | 步骤冲突报错 | `saga definition %q: steps %q and %q differ only in case; ...` |
| `kit/saga/step_budgets.go:45` | `StepBudgetsFromConfig` 调用点 | 在读 `saga.steps` 之前建表，所以与配置内容无关 |

**3. 不变量与强制点**：一个小写键最多对应一个原名（`kit/saga/step_budgets.go:98`、`:107`）；同名重复（同一定义两次、同类型多版本）不算冲突（比较的是原名是否不同）。守卫 `TestStepBudgetConfigRejectsNamesThatDifferOnlyInCase`。

**4. 控制流**：`StepBudgetsFromConfig` → 读 `step_defaults` → `knownSagaSteps(definitions)`（冲突即返回错误）→ 遍历 `saga.steps`。

**5. 失败与不确定结果**

| 情形 | 处理 | 对调用方的样子 |
| --- | --- | --- |
| 定义里有 `gift_item` 与 `Gift_Item` | 报错，不论配置是否写了 `saga.steps` | saga Mod `Init` 失败 |
| 同一定义传两次 | 不报错 | 正常启动 |
| `NewMod()` 不带定义 | 不建表，检测不到 | 见 [SAGA-4](#saga-4) 第 5 节 |

**6. 测试**

| 用例 | 文件 | 覆盖 |
| --- | --- | --- |
| `TestStepBudgetConfigRejectsNamesThatDifferOnlyInCase`（types / types_swapped / steps，加“同一定义两次不报错”） | `kit/saga/step_budgets_test.go` | 两种注册顺序、步骤冲突、非冲突 |

修前红（原样，[问题记录](../../bug/RR-20261006-06.md)）：

```text
step_budgets_test.go:116: types: err=<nil> overrides=map[{Gift_Item debit}:{0s 15 0s 0s}], want an ambiguity error
```

修后（[修复记录](../../bugfix/RR-20261006-06.md)）：`go test -race -count=3 ./kit/saga` 通过；整批 `gofmt`、`go vet`、`-race -count=3 ./nest ./app ./kit/service/global ./kit/saga ./mongo/mongotest ./saga`、根包、`go build ./... && go vet ./...` 通过（[收尾第 4 批](../../bugfix/CLOSING-BATCH-4-2026-10-06.md)）。负对照：记录未保留。

**7. 性能证据**：无（启动时一次）。

**8. 未验证项与已知风险**：无定义路径不覆盖；`map` 遍历顺序不影响结果（任何顺序下冲突都会被发现，`types_swapped` 子用例钉住）。

**9. review 检查点**

- [ ] `kit/saga/step_budgets.go:96-112`：冲突判断是“同小写键、原名不同”；确认 Unicode 大小写（非 ASCII 类型名）下 `strings.ToLower` 的折叠与 viper 键的折叠一致，否则会出现 viper 能区分而这里报冲突（或反之）。
- [ ] 报错发生在不写任何 `saga.steps` 的部署上：确认 CHANGELOG / 兼容说明已提示“注册了只差大小写名字的工程启动失败”，且生成器 `add saga` 不可能产出这种名字。
- [ ] 同一提交 `611d5d72` 改了 `saga/coordinator_takeover_review_test.go`、`saga/step_operation_promises_test.go`、`saga/cross_process_real_integration_test.go`（A14 去绕行）：确认只删了领取函数参数，断言未改（记录写 integration 用例本批未在真实 Mongo 重跑）。
- [ ] 与 [SAGA-4](#saga-4) 的组合：有定义时冲突先报错，回退不会被走到；无定义时回退仍让两类型共用覆盖——两条路径的行为不一致是否接受。

<a id="saga-6"></a>
### SAGA-6 协调器接收 completion 时核对代际（B1）

> 首发 v1.20.2 · [说明](guide-saga-drv-dao-rem.md#saga-6)

**1. 提交**

| 提交 | 版本 | 内容 |
| --- | --- | --- |
| `3fabe34d` | v1.20.2 | `Complete` 判定顺序、`commandIDIncarnation`、stale 计数、告警去重（`LateSuccessAlarmStore` / `MarkLateSuccessAlarm` / `late_alarms`）、人工 Compensate 换代；方案文档、SAGA.md、U-0280 记录追加 |
| `76088da2` | v1.20.2 | DECISIONS-PENDING B1 标为已实施 |
| `a95cf4dc` | v1.21.0 | 换代判断并入 `stepTransition`（[SAGA-8](#saga-8)） |

**2. 改动文件与关键符号**

| `path:line` | 符号 | 职责 |
| --- | --- | --- |
| `saga/engine.go:406` | `Engine.Complete` | B1 判定顺序 |
| `saga/engine.go:416` | `commandIDIncarnation(...)` 调用 | 从 `CommandID` 解析 completion 的代际 |
| `saga/engine.go:424-433` | 接收判定 | 旧一生失败 / 更新代际 → stale；旧一生成功 → `positionedAt`；同一生 → 等待中 |
| `saga/engine.go:461` | `positionedAt` | 在等它，或 `Pending` / `Compensating` 且当前方向 + 步骤就是它 |
| `saga/engine.go:474` | `completeNotWaiting` | 放弃后迟到成功按（操作，代际）只告警一次 |
| `saga/engine.go:507` | `reportStaleIncarnation` | WARN + `saga.completion.stale_incarnation_total{saga_type,phase}` |
| `saga/engine.go:920` / `:930` | `commandID` / `commandIDIncarnation` | 铸造与解析同处：第 0 代 `key:attempt`，第 N 代 `key:rN:attempt` |
| `saga/step_transition.go:54` | 换代规则 | Resume 总是；人工 Compensate 只在 `before.Phase == PhaseCompensate` |
| `saga/store.go:99` | `LateSuccessAlarmStore` | 可选接口 |
| `saga/mongo_store.go:196` | `MongoStore.MarkLateSuccessAlarm` | 条件更新 `late_alarms.r<N>` `$exists:false`，`MatchedCount==1` 才是第一次 |
| `saga/mongo_store.go:528` | `operationDoc.LateAlarms` | tombstone 子文档 |

**3. 不变量与强制点**

- 协调器与收件箱对“同一生”的判断不分叉：两边都调 `commandIDIncarnation`（`saga/engine.go:930`、`saga/step_operation_inbox.go:413-415`），守卫 `TestCommandIDIncarnationInvertsCommandID`。
- 同一迟到成功只告警一次：单文档条件更新的原子性（`saga/mongo_store.go:200-206`），守卫 `TestRealMongoLateSuccessAlarmIsMarkedOnce`（20 轮 × 8 并发，每轮恰好 1 个 first）。
- 新一生的 `CommandID` 与上一生不相交：只在 `stepTransition` 里改 `Incarnation`（[SAGA-8](#saga-8) 的守卫钉住）。

**4. 控制流 / 状态机**

saga 记录状态机（含代际；`Incarnation` 只在 `stepTransition` 里 +1）：

```mermaid
stateDiagram-v2
    [*] --> Pending: StartSaga，Incarnation=0
    Pending --> Waiting: 派发尝试，Attempt+1，CommandID 为 key:Attempt 或 key:rN:Attempt
    Waiting --> Pending: 成功且还有下一步，或可重试失败与超时未用尽
    Waiting --> Completed: 最后一步成功
    Waiting --> Compensating: 拒绝、重试用尽、saga 截止，且已有完成步骤
    Waiting --> Failed: 同上但没有完成步骤
    Pending --> Compensating: saga 截止或人工 Compensate
    Compensating --> Waiting: 派发补偿尝试
    Waiting --> Compensated: 最后一个补偿成功
    Waiting --> ManualRequired: 补偿拒绝或用尽
    Pending --> ManualRequired: 定义缺失或步骤越界
    Waiting --> ManualRequired: 定义缺失
    Failed --> Pending: Resume，Incarnation+1，无完成步骤
    Failed --> Compensating: Resume，Incarnation+1，有完成步骤
    ManualRequired --> Compensating: Resume，Incarnation+1
    ManualRequired --> Compensating: 补偿方向上人工 Compensate，Incarnation+1
    Completed --> [*]
    Compensated --> [*]
```

`Complete` 接收判定：

```mermaid
flowchart TD
    A[completion 到达，解析代际 n] --> B{n 不等于记录代际}
    B -->|是，且为失败或 n 更新| S[不接收，计 StaleIncarnation，返回 nil 即 ack]
    B -->|是，且为成功| P{记录正停在这个操作上}
    B -->|否| W{Waiting 且 OperationKey 相同}
    P -->|是| ACC[接收，applyCompletion 与 stepTransition 带回执]
    P -->|否| NW[completeNotWaiting]
    W -->|是| ACC
    W -->|否| NW
    NW --> H{有回执或 tombstone}
    H -->|都没有| E[ErrNotWaiting]
    H -->|成功且无回执且放弃关闭| M{MarkLateSuccessAlarm 第一次}
    M -->|是| L[ERROR 加 late_after_abandon 计数]
    M -->|否| D[计 Duplicates]
    H -->|其余| D
```

**5. 失败与不确定结果**

| 情形 | 处理 | 对调用方的样子 |
| --- | --- | --- |
| Resume 后旧一生的拒绝晚到（E1） | 不接收，计 stale | 消费者 ack；新一生照常执行 |
| 同一迟到成功送达多次（E2） | 第一次告警，之后 `Duplicates` | 消费者 ack |
| Resume 后、派发前旧一生成功到达（E3） | 接收为结果，tombstone 升级为 `result` | saga 前进，不告警 |
| 补偿方向 `ManualRequired` 上人工 Compensate（E4） | 进入新一生，新 `CommandID` 带 `:rN:` | 补偿真正重新执行 |
| Store 没实现 `LateSuccessAlarmStore` | 每次送达都告警 | 宁可重复，不丢告警 |
| `MarkLateSuccessAlarm` 出错 | `Complete` 返回错误 | 消费者 nak 重投 |

**6. 测试**

| 用例 | 文件 | 覆盖 |
| --- | --- | --- |
| `TestCoordinatorChecksTheIncarnationOfACompletion`（E1～E4 + “补偿方向 ManualRequired 用 Resume 重新执行补偿”守卫） | `saga/step_operation_incarnation_promises_test.go` | `nativeWorld`：真实 `SubscribeDataEngineStep` + mongotest 上真实收件箱 + 真实投影器，手动推进时钟 |
| `TestMongoStoreMarksALateSuccessAlarmOncePerLife` | 同上 | MongoStore 上 3 次送达 = 1 告警 + 2 重复；按代际标记；旧 tombstone 补标记；旧一生拒绝不接收 |
| `TestCommandIDIncarnationInvertsCommandID` | 同上 | 解析是铸造的逆 |
| `TestRealMongoLateSuccessAlarmIsMarkedOnce` | `saga/late_alarm_real_mongo_integration_test.go`（`-tags integration`） | 真实 Mongo 并发标记 |

修前红：见说明条目引的四条首行，全文 [B1 red-before.txt](../../bugfix/evidence/B1/red-before.txt)（基线 `3a71321c`，`go test ./saga/ -count=1 -run TestCoordinatorChecksTheIncarnationOfACompletion`）。修后（方案第 8 节）：`go test -race -count=3 ./saga/... ./kit/saga/...` 通过；新增与相关用例 `-race -count=200` 通过；`go test -tags integration -run 'TestRealMongo(LateSuccessAlarmIsMarkedOnce|ConcurrentAttempts|SupersedeAndProjection)' ./saga/` 通过。负对照：没有做真实 Mongo 上去掉 `$exists` 条件的负对照（方案“未验证”）。

**7. 性能证据**：无（只在迟到成功路径多一次条件更新）。

**8. 未验证项与已知风险**：新旧协调器混跑未实跑；B1 方案的方向判断写明：若之后在“Mongo 步骤仍按命令”或“迟到成功只告警”两处再出缺陷，应走 Mongo 步骤纳入收件箱（已在 [SAGA-9](#saga-9) 做）或 C，而不是继续在协调器加特判。

**9. review 检查点**

- [ ] `saga/engine.go:424-426`：`incarnation > record.Incarnation` 的成功也被当作 stale 丢弃（返回 `record, nil`，消费者 ack）——确认“比记录还新”确实不可能由协调器产生（`Incarnation` 只增、只在 `stepTransition` 改），丢弃不会吞掉真实结果。
- [ ] `saga/engine.go:930-943`：`commandIDIncarnation` 用 `operationKey+":r"` 前缀切分；`operationKey` 本身是 `<sagaID>:<phase>:<step>`，确认 sagaID 的字符集（`validSubjectToken`）不可能让第 0 代的 `key:attempt` 被误解析成 `key:rN:...`。
- [ ] `saga/engine.go:461-472` `positionedAt` 对 `Pending` / `Compensating` 用 `operationKey(record.ID, record.Phase, record.Step)`：确认 Resume 进入补偿方向时（`Step = CompletedSteps-1`）旧一生**正向**成功不会被当作“停在这个操作上”（方向不同、键不同）。
- [ ] `saga/step_transition.go:54`：人工 Compensate 只在 `before.Phase == PhaseCompensate` 时换代；从正向退避中发起的补偿保持 `CommandID`——确认这时补偿方向在这一生里确实从未派发过（否则会复用 ID，E4 同形）。
- [ ] `saga/mongo_store.go:196-207`：过滤条件包含 `saga_id`；不同 saga 不会因为相同 `IdempotencyKey` 误标（`IdempotencyKey` 含 sagaID，通常不会）；没有 tombstone 时返回 false，`completeNotWaiting` 这时走 `Duplicates`——确认与“没有 tombstone 就 `ErrNotWaiting`”的前序判断不矛盾（`history.Recorded` 已保证 tombstone 存在）。

<a id="saga-7"></a>
### SAGA-7 定义缺失 fence 时退避中的步骤同样记为放弃（RR-20261005-NC-250）

> 首发 v1.21.0 · [说明](guide-saga-drv-dao-rem.md#saga-7)

**1. 提交**

| 提交 | 版本 | 内容 |
| --- | --- | --- |
| `31b48bc0` | v1.21.0 | 新增 `abandonedOperation`，截止 / 人工 Compensate / 定义缺失三个出口共用；回归用例；N06 S5 剩余项审查用例（`TestCoordinatorLeaseTakeoverFencesTheLateApply`、`TestOutboxSupersedeAndUnknownAckOnMongoStore`、真实跨进程 `TestRealSagaCrossProcessKillRecovers` / `TestRealMongoCoordinatorLeaseTakeover`）；SAGA.md O-S5-3 说明 |
| `a95cf4dc` | v1.21.0 | `abandonedOperation` / `closedOperation` 删除，由 `stepTransition` + `openOperation` 取代（[SAGA-8](#saga-8)） |

**2. 改动文件与关键符号**（当前源码）

| `path:line` | 符号 | 职责 |
| --- | --- | --- |
| `saga/engine.go:697-714` | `processClaimed` 定义缺失分支 | fence 到 `ManualRequired`，`stepTransition(..., causeDefinitionMissing, fenced)` |
| `saga/step_transition.go:73` | `openOperation` | `Waiting` → `OperationKey`；`Pending` / `Compensating` 且 `Attempt>0` → 当前方向 + 步骤的操作 |
| `saga/step_transition.go:61-63` | 关闭规则 | `before` 开着的操作在 `after` 不再开着就关闭 |
| `saga/mongo_store.go:323-346` | `Apply` 的 `CloseOperation` | 写 tombstone（放弃关闭）、删排队命令 |
| `saga/definition_fence_abandon_promises_test.go` | 回归 | 正向、补偿两个方向 |

**3. 不变量与强制点**：协调器离开一个已派发过的操作（不论在等还是在退避）都写 tombstone；强制点在 `stepTransition`（`saga/step_transition.go:61`），守卫是 [SAGA-8](#saga-8) 的 `TestEveryCoordinatorWriteGoesThroughStepTransition`。

**4. 控制流**：协调器领到退避到期的记录 → 找不到定义 → `after.Status = ManualRequired`、清 `OperationKey` / `CommandID` → `stepTransition`：`openOperation(before)` = `<saga>:<phase>:<step>`（`Attempt>0`），`openOperation(after)` = 空 → `CloseOperation` = 该操作 → `MongoStore.Apply` 同一事务写放弃 tombstone、删排队命令。之后的迟到成功走 `completeNotWaiting` → 告警一次。

**5. 失败与不确定结果**

| 情形 | 处理 | 对调用方的样子 |
| --- | --- | --- |
| fence 时步骤在等结果 | 关闭 `OperationKey`（修前也对） | 同修前 |
| fence 时步骤在退避 | 关闭当前操作（修前漏） | 迟到成功 ack + ERROR + 计数 |
| 步骤越界（`invalid saga step`）且 `Attempt>0` | `stepTransition` 后也会放弃关闭（[SAGA-8](#saga-8) 唯一行为差异） | 正常不可达 |
| 混跑：旧协调器 fence | 没有 tombstone | 迟到成功仍 `ErrNotWaiting` |

**6. 测试**

| 用例 | 文件 | 覆盖 |
| --- | --- | --- |
| `TestDefinitionFenceDuringBackoffAbandonsTheOperation`（forward / compensate） | `saga/definition_fence_abandon_promises_test.go` | 排队命令删除、迟到成功 ack、告警 +1 |
| `TestCoordinatorLeaseTakeoverFencesTheLateApply`（3 种顺序） | `saga/coordinator_takeover_review_test.go` | 审查用例：去掉 `applyFilter` 的租约条件做负对照，第一种顺序变红（修复记录） |
| `TestOutboxSupersedeAndUnknownAckOnMongoStore` | 同上 | 审查用例 |
| `TestRealMongoCoordinatorLeaseTakeover`、`TestRealSagaCrossProcessKillRecovers` | `saga/cross_process_real_integration_test.go`（`-tags integration`） | 真实依赖 |

修前红：说明条目引了正向三条，全文 [red-before.txt](../../bugfix/evidence/NC-250/red-before.txt)。修后（原样，[green-after.txt](../../bugfix/evidence/NC-250/green-after.txt)）：

```text
2026/10/06 07:58:57 ERROR saga: step succeeded after the coordinator abandoned it; the effect is not compensated saga_id=nc250-compensate saga_type=rally status=manual_required phase=compensate step=0 command_id=nc250-compensate:2:0:1 operation=nc250-compensate:2:0
--- PASS: TestDefinitionFenceDuringBackoffAbandonsTheOperation (0.00s)
    --- PASS: TestDefinitionFenceDuringBackoffAbandonsTheOperation/forward (0.00s)
    --- PASS: TestDefinitionFenceDuringBackoffAbandonsTheOperation/compensate (0.00s)
```

真实依赖（[real-run.txt](../../bugfix/evidence/NC-250/real-run.txt)，第 3 次即修后）：`recovered in 1m26.705s: 60 sagas completed, 120 operations, 121 committed step executions (102 by coordinator-b), 1 operations re-run by a later attempt (allowed for Mongo steps: idempotent by IdempotencyKey), 120 coordinator receipts`——这时 Mongo 步骤还不在收件箱契约内，[SAGA-9](#saga-9) 之后同一用例为 0 个重复提交。

**7. 性能证据**：无（只多写一份 tombstone）。

**8. 未验证项与已知风险**：混跑未实跑；真实 Mongo 上没有单独复跑 NC-250 的触发。

**9. review 检查点**

- [ ] `saga/step_transition.go:73-83`：`openOperation` 对 `Pending` / `Compensating` 只在 `Attempt > 0` 时返回操作；确认 `retryState`（`saga/engine.go:841-869`）在退避时保留 `Attempt`（不清零），否则退避中的操作不会被识别。
- [ ] `saga/engine.go:697-714`：定义缺失分支 `after` 没有改 `Phase` / `Step`；确认 `openOperation(after)` 因 `Status = ManualRequired` 返回空，从而一定关闭。
- [ ] `saga/mongo_store.go:323-346`：关闭时 `DeleteMany` 删的是 `command.idempotency_key == CloseOperation` 的排队命令；确认已被 publisher 领取（租约中）的命令也会被删，或其后续 Ack / Nack 对已删文档的处理是安全的（`TestOutboxSupersedeAndUnknownAckOnMongoStore` 是否覆盖）。
- [ ] 修复记录与源码的对应：记录里的 `abandonedOperation` 已不存在（`a95cf4dc` 删除），确认 `openOperation` 对截止、人工 Compensate、定义缺失三个出口给出与 `abandonedOperation` 相同的答案（方案“行为对照”逐出口）。

<a id="saga-8"></a>
### SAGA-8 离开当前步骤收成一个转移 stepTransition（saga 方向 ①）

> 首发 v1.21.0 · [说明](guide-saga-drv-dao-rem.md#saga-8)

**1. 提交**

| 提交 | 版本 | 内容 |
| --- | --- | --- |
| `b3538251` | v1.21.0 | 记录维护者第六轮决定（含 saga 方向 ①②） |
| `a95cf4dc` | v1.21.0 | 新增 `saga/step_transition.go`，`engine.go` 8 个出口改调，删除 `abandonedOperation` / `closedOperation`；守卫测试；负对照证据 |
| `8d4bec52` | v1.21.0 | 方案与 DECISIONS-PENDING 写入提交号 |
| `42419890` | v1.21.0 | 发版前复审：守卫补上不写字面量的两种绕过，负对照固定在 `saga/testdata/stepguard` |

**2. 改动文件与关键符号**

| `path:line` | 符号 | 职责 |
| --- | --- | --- |
| `saga/step_transition.go:12-31` | `transitionCause` | dispatch / result / timeout / deadline / definitionMissing / invalidStep / manualCompensate / resume |
| `saga/step_transition.go:34` | `transition` | `cause`、`fenced`、`receipt`、`outbox` |
| `saga/step_transition.go:53` | `stepTransition` | 唯一构造 `ApplyRequest`、唯一改 `Incarnation` |
| `saga/step_transition.go:73` | `openOperation` | “当前开着哪个操作”的唯一回答 |
| `saga/engine.go:343` / `:381` / `:440` / `:710` / `:718` / `:727` / `:742` / `:767` | 8 个出口 | Resume / Compensate / Complete / 定义缺失 / 截止 / 超时 / 步骤越界 / 派发 |
| `saga/step_transition_guard_test.go:17` | `TestEveryCoordinatorWriteGoesThroughStepTransition` | 解析包内非测试源码 |
| `saga/step_transition_guard_test.go:26` | `TestStepTransitionGuardSeesBypassesWithoutALiteral` | 守卫自己的负对照 |
| `saga/testdata/stepguard/bypass.go` | `mutatedTransitionExit`、`aliasedStoreExit` | 两种不写字面量的绕过 |

**3. 不变量与强制点**

| 不变量 | 强制点 |
| --- | --- |
| 所有协调器写记录经 `stepTransition` | 守卫：`ApplyRequest{}` 复合字面量、`var x ApplyRequest`、`new(ApplyRequest)` 只能出现在 `stepTransition` 里（`saga/step_transition_guard_test.go:72-102`） |
| `Incarnation` 只在 `stepTransition` 里改 | 赋值 / 自增检查（`:83-94`） |
| `stepTransition` 的结果不被改写 | 对结果变量字段赋值、取地址报出（`:86-98`） |
| `Engine` 方法里的 `store.Apply` 参数必须是 `stepTransition(...)` 或只被它赋值过的变量 | `:103-116`；对 `e.store` 以外的 `Apply` 调用报出（`:106-111`） |
| 守卫没有失明 | `applyCalls == 0` 时 `t.Fatal`（`:122-124`） |

**4. 控制流**：每个出口算出 `after` → `stepTransition(before, after, transition{...})`：(1) `Resume`，或 `ManualCompensate` 且 `before.Phase == PhaseCompensate` → `after.Incarnation = before.Incarnation + 1`；(2) `ExpectedVersion = before.Version`，`fenced` 时带 `before.Lease`；(3) `openOperation(before)` 非空且不等于 `openOperation(after)` → `CloseOperation`；(4) 接收了成功 → `CloseOperation = receipt.IdempotencyKey`（覆盖第 3 步）。

**5. 失败与不确定结果**

| 情形 | 处理 | 对调用方的样子 |
| --- | --- | --- |
| 新出口手拼请求 | 守卫测试失败并报文件:行号 | CI 红 |
| 步骤越界且已派发（`Attempt > 0`） | 新规则放弃关闭该操作，旧代码不关闭 | 正常不可达；方案列为唯一行为差异 |
| `ManualRequired` 上人工 Compensate（`Attempt>0`） | 旧代码会对已关闭的操作再关一次；新规则不重复关闭 | 可观察结果相同（tombstone 不改关闭方式，排队命令已删） |

**6. 测试**

| 用例 | 文件 | 覆盖 |
| --- | --- | --- |
| `TestEveryCoordinatorWriteGoesThroughStepTransition` | `saga/step_transition_guard_test.go` | 生产源码无违例 |
| `TestStepTransitionGuardSeesBypassesWithoutALiteral` | 同上 + `saga/testdata/stepguard/bypass.go` | 两种绕过必须逐个报出 |
| U-0280、U-0281、B1、NC-250 与 `step_operation_*` 全部既有用例 | `saga/` | 不改断言通过（`-race -count=3` 全包；关键用例 `-race -count=50`） |

负对照（原样，[guard-negative.txt](../../feature/evidence/sagadir/guard-negative.txt)）：

```text
# 负对照：在 saga 包里临时加一个手拼请求的 Engine 出口（handBuiltExit：Incarnation++ 后 store.Apply(ctx, ApplyRequest{...})），
# GOWORK=off go test -count=1 -run TestEveryCoordinatorWriteGoesThroughStepTransition ./saga/   （验证后删除该文件）
--- FAIL: TestEveryCoordinatorWriteGoesThroughStepTransition (0.00s)
    step_transition_guard_test.go:20: ./zz_guard_negative.go:8:2: Incarnation changed outside stepTransition in handBuiltExit
    step_transition_guard_test.go:20: ./zz_guard_negative.go:9:12: store.Apply in handBuiltExit does not take stepTransition(...)
    step_transition_guard_test.go:20: ./zz_guard_negative.go:9:31: ApplyRequest built outside stepTransition in handBuiltExit
FAIL
```

`42419890` 的绕过负对照现在是常驻用例（`TestStepTransitionGuardSeesBypassesWithoutALiteral` 每次运行），修前（原守卫看不到这两种）的红文本记录未保留。

**7. 性能证据**：无（纯重构，不涉及热路径）。

**8. 未验证项与已知风险**：守卫是语法层（`go/ast`）检查，不做类型推断；见第 9 节列出的盲区。

**9. review 检查点**

- [x] 确认所有步骤状态转移只经 `stepTransition`：看守卫是否覆盖不写字面量的两种绕过——`saga/testdata/stepguard/bypass.go` 的 `mutatedTransitionExit`（改 `request.CloseOperation`）与 `aliasedStoreExit`（`var request ApplyRequest` + `store := e.store`），对应 `TestStepTransitionGuardSeesBypassesWithoutALiteral`。
- [x] 盲区 1（按源码推断）：`engineMethod` 只看接收者为 `Engine` 的方法（`saga/step_transition_guard_test.go:65`、`:103-105`）；包级函数（例如 `func apply(s Store, r ApplyRequest)`）里的 `s.Apply(ctx, r)` 不被检查。
- [x] 盲区 2（按源码推断）：`ApplyRequest` 作为**函数参数**传入时既不是 `ValueSpec` 也不是复合字面量，`transitionResultNames` 也不认它，参数上的字段赋值不会报出；配合盲区 1 可构造“经 helper 改写 `CloseOperation` 再写入”的绕过。
- [x] `isRequestFromTransition`（`:213-235`）按标识符**名字**而不是 `ast.Object` 统计赋值；同一函数里不同作用域的同名变量会被合并计数——确认不会产生漏报（只会更严格还是可能放过？）。
  **以上四条已闭环（fixs，RR-20261006-14）**：盲区 1 + 2 组合的 helper 绕过已在修前复现（原守卫两个用例都通过）。现在 `stepTransition` 是 Engine 方法、自己调 `Store.Apply` 并重写代际，出口拿不到请求；守卫改为 `go/types` 全包检查（请求只能在 stepTransition 里产生、不能改写、`Store.Apply` 只在那里调用），语法守卫、`isRequestFromTransition` 与 `saga/testdata/stepguard` 一并删除，负对照按维护者要求不留仓库，验证输出见[修复记录](../../bugfix/RR-20261006-14.md)。本节第 2、3、6 小节的符号与用例名是 v1.21.0 时的样子。
- [ ] `saga/step_transition.go:61-66`：成功回执覆盖 `CloseOperation` 为 `receipt.IdempotencyKey`；确认可重试失败 / 拒绝的回执不会走这一分支，以失败关闭时仍按第 3 步关闭 `before` 开着的操作、由 Store 记为 `abandoned`。
- [ ] `fenced` 只在协调循环出口为 true（`saga/engine.go:710-767`），`Complete` / `Compensate` / `Resume` 只按版本 fence；确认这与 `ClaimDue` 的租约语义一致（方案第 4 条）。

<a id="saga-9"></a>
### SAGA-9 Mongo 步骤纳入操作实例收件箱，结果消费者终态分类统一（saga 方向 ②、O-S5-1、O-S5-3）

> 首发 v1.21.0 · [说明](guide-saga-drv-dao-rem.md#saga-9)

**1. 提交**

| 提交 | 版本 | 内容 |
| --- | --- | --- |
| `9669d181` | v1.21.0 | `saga/step_operation_inbox.go`（共用核心 + `settleOwnClaim` / `errAttemptFenced`）；`MongoCommandInbox` 两事务；`SubscribeMongoStep` 分支；O-S5-1 共用 `isTerminalCompletionError` 并加 `ErrIdentityConflict`；O-S5-3 文档；codegen / demo 注释；证据 `bench.txt` / `cross-process-kill.txt` / `mongo-step-red-green.txt` / `o-s5-1-red.txt` |
| `8d4bec52` | v1.21.0 | 方案与 DECISIONS-PENDING 写入提交号 |
| `5a3c4a60` | v1.21.0 | `ErrDefinitionMissing` 移出终态；`ErrDuplicateKey` 回放分支加观察注释（[SAGA-10](#saga-10)） |
| `ff08c941` | v1.23.0（本版） | 延迟分析与基准文件（[SAGA-11](#saga-11)） |

**2. 改动文件与关键符号**

| `path:line` | 符号 | 职责 |
| --- | --- | --- |
| `saga/command_consumer.go:35` | `MongoCommandInbox` | 嵌入 `stepOperationInbox`，`collection` 存回执 |
| `saga/command_consumer.go:41-48` | `CommandInboxOptions` | `ReceiptTTL`、`Owner`、`LeaseDuration` |
| `saga/command_consumer.go:50` | `mongoInboxClaimSuffix = "_claims"` | claim / 守卫集合 = 收件箱集合 + 后缀 |
| `saga/command_consumer.go:83` | `EnsureInfrastructure` | 回执 TTL 索引 + `ensureClaimIndexes` |
| `saga/command_consumer.go:103` | `Handle` | Reserve 事务 → 执行事务；失败交还租约 |
| `saga/command_consumer.go:150` | `execute` | handler → `settleOwnClaim` → 插回执，同一事务 |
| `saga/step_operation_inbox.go:387` | `settleOwnClaim` | 条件写 owner / token / `pending` / `lease_until > now`，不匹配 → `errAttemptFenced` |
| `saga/command_consumer.go:256` | `SubscribeMongoStep` | 过期、fence、接替、在途分支 |
| `saga/command_consumer.go:358` | `ackUnexecutedAttempt` | 不执行的投递 ack 前重发同一操作的成功 |
| `saga/nest_completion_consumer.go:150` | `isTerminalCompletionError` | 两条结果流共用 |
| `saga/jetstream.go:144` | 普通结果流调用点 | O-S5-1 |
| `saga/nest_completion_consumer.go:137` | 原生结果流调用点 | O-S5-1 |

**3. 不变量与强制点**

| 不变量 | 强制点 | 守卫 |
| --- | --- | --- |
| 同一操作实例至多一次业务写生效 | Reserve 事务（`saga/step_operation_inbox.go:124-146`）+ 执行事务里的条件写（`saga/command_consumer.go:179`） | `TestMongoStepAttemptsOfOneOperationTakeEffectOnce`（mongotest）与 `TestRealMongoStepAttemptsOfOneOperationTakeEffectOnce`（真实副本集，同一份用例） |
| 接替与生效只能一个提交 | `supersede`（`:369`）与 `settleOwnClaim`（`:387`）写同一 claim 文档 | 同上“in-flight attempt past its deadline” |
| 回执格式不变 | 执行事务最后 `InsertOne(commandReceiptDoc{...})`（`saga/command_consumer.go:182`） | 混跑语义依赖它 |
| 两条结果流终态分类一致 | 共用 `isTerminalCompletionError` | `TestCompletionConsumersTermTheSameTerminalErrors` |
| 跨进程强杀下每个操作恰好一次提交 | 上述全部 | `TestRealSagaCrossProcessKillRecovers` |

**4. 控制流 / 状态机**

Reserve 事务与执行事务（`MongoCommandInbox.Handle`）：

```mermaid
sequenceDiagram
    participant C as SubscribeMongoStep
    participant I as MongoCommandInbox
    participant R as Reserve 事务
    participant X as 执行事务
    participant M as Mongo 副本集
    C->>C: 过期则走 ackUnexecutedAttempt，否则 Admit
    C->>I: Handle(ctx 截止为 DeadlineAt)
    I->>R: StartSession 与 WithTransaction
    R->>M: findOne 回执集合 _id=CommandID
    R->>M: findOne claims 本命令 claim
    R->>M: findAndModify upsert 守卫 saga-step-op/IdempotencyKey
    R->>M: find claims by operation_key
    R->>M: insert 或接管本命令 claim，租约封顶到 DeadlineAt
    R->>M: commitTransaction，w majority 且 j true
    alt 回放或在途
        R-->>I: Duplicate 带 Completion 或 errOperationAttemptInFlight
        I-->>C: 回放发布或 nak
    else 新租约
        I->>X: StartSession 与 WithTransaction
        X->>M: handler 用事务 ctx 写业务文档
        X->>M: update claims，条件为 owner token pending 且 lease_until 大于 now
        X->>M: insert 回执，_id=CommandID
        X->>M: commitTransaction
        alt 条件写未匹配
            X-->>I: errAttemptFenced，整笔回滚
            I->>M: releaseLease，条件不匹配时不改
            I-->>C: ackUnexecutedAttempt 后 ack
        else 提交成功
            X-->>I: completion
            I-->>C: PublishCompletion
        end
    end
```

`SubscribeMongoStep` 对 `Handle` 结果的分支（`saga/command_consumer.go:329-353`）：`ErrCommandExpired` / `errAttemptFenced` / `errAttemptSuperseded` → `ackUnexecutedAttempt` 后 ack；其他错误（含 `errOperationAttemptInFlight`、被截止打断的执行事务）→ nak；成功或回放 → `PublishCompletion`（回放的 `CommandID` 是生效那次的）。

**5. 失败与不确定结果**

| 情形 | 处理 | 对调用方的样子 |
| --- | --- | --- |
| handler 返回错误 / ctx 取消 | 执行事务回滚，`releaseLease`（`WithoutCancel`） | nak，重投立即重新 Reserve |
| 执行事务提交结果未知 | 若已提交，claim 是 completed，交还条件不匹配不改；重投读到回执回放 | 至多一次仍成立 |
| 执行事务撞回执唯一键（混跑，旧进程先写回执） | 回放那份回执，不交还 claim（观察，[SAGA-10](#saga-10)） | 返回 duplicate |
| k 卡在执行事务里拖过截止，k+1 接替 | k 的条件写不匹配 → `errAttemptFenced`，不留回执 | k 不生效，k+1 执行 |
| 被 kill -9 的进程遗留事务持锁 | 服务端到 `transactionLifetimeLimitSeconds` 才中止 | 后续尝试写同一批文档时等待；O-S5-3 建议调到 20s |
| 事务外副作用（如发邮件）在被 fence 的尝试里已发出 | 框架不保证 | 业务仍需按 `IdempotencyKey` 幂等 |
| 普通结果流收到 `ErrNotWaiting` / `ErrNotFound` / `ErrInvalidRecord` / `ErrIdentityConflict` | `nats.Permanent` → Term | 不再 nak 到 `MaxDeliver` |

**6. 测试**

| 用例 | 文件 | 覆盖 |
| --- | --- | --- |
| `TestMongoStepAttemptsOfOneOperationTakeEffectOnce`（committed attempt is replayed / in-flight attempt past its deadline is taken over） | `saga/mongo_step_operation_promises_test.go` | mongotest |
| `TestRealMongoStepAttemptsOfOneOperationTakeEffectOnce` | `saga/mongo_step_operation_real_mongo_integration_test.go`（`-tags integration`） | 同一份用例跑真实副本集 |
| `TestMongoStepConsumerFollowsTheOperationInbox`（回放发布、在途 nak、过期重发、被接替 ack） | `saga/mongo_step_consumer_promises_test.go` | 消费者分支 |
| `TestCompletionConsumersTermTheSameTerminalErrors` | `saga/completion_consumer_terminal_promises_test.go` | O-S5-1：`ErrNotFound` / `ErrNotWaiting` / `ErrIdentityConflict` 两条流都 Term |
| `TestRealSagaCrossProcessKillRecovers` | `saga/cross_process_real_integration_test.go`（`-tags integration`） | 每个操作实例业务事务恰好提交一次 |
| `BenchmarkMongoCommandInboxHandle` / `BenchmarkRealMongoCommandInboxHandle` | `saga/mongo_step_benchmark_test.go` / `saga/mongo_step_benchmark_real_mongo_integration_test.go` | 前后对照 |

修前红：说明条目引了真实副本集三行，全文 [mongo-step-red-green.txt](../../feature/evidence/sagadir/mongo-step-red-green.txt)（基线 `a5e7b070` = `b3538251` + ①）。O-S5-1 修前红（原样，[o-s5-1-red.txt](../../feature/evidence/sagadir/o-s5-1-red.txt)，节选）：

```text
        completion_consumer_terminal_promises_test.go:74: plain result stream = saga: not found (permanent=false), want saga: not found terminated: a redelivery cannot change it, and naking it holds a MaxAckPending slot until MaxDeliver
        completion_consumer_terminal_promises_test.go:74: plain result stream = saga: step is not waiting for a result (permanent=false), want saga: step is not waiting for a result terminated: a redelivery cannot change it, and naking it holds a MaxAckPending slot until MaxDeliver
        completion_consumer_terminal_promises_test.go:74: plain result stream = saga: idempotency identity conflict (permanent=false), want saga: idempotency identity conflict terminated: a redelivery cannot change it, and naking it holds a MaxAckPending slot until MaxDeliver
```

修后跨进程强杀（原样，[cross-process-kill.txt](../../feature/evidence/sagadir/cross-process-kill.txt)）：

```text
    cross_process_real_integration_test.go:384: recovered in 1m22.732s: 60 sagas completed, 120 operations, 120 committed step executions (104 by coordinator-b), 0 operations committed by more than one attempt (want 0), 120 coordinator receipts
--- PASS: TestRealSagaCrossProcessKillRecovers (86.01s)
```

存活进程 232 次 `saga: step command is past its deadline`、41 次 WriteConflict，期间出现 MaxTimeMSExpired、连接中断导致的提交结果未知，仍然恰好一次（同文件统计）。修后验证命令（方案“验证”）：`go test -race -count=3 ./saga/... ./kit/saga/...`；新增与 `TestNativeStep*`、`TestDataEngineStepInbox*`、`TestExpiredStepCommand*`、`TestMongoCommandInbox*` `-race -count=20`；`-run '^TestRealMongo'` 全部 saga 真实 Mongo 用例；`scripts/test-dataengine-generated.sh`；生成 game-demo build / vet / test。负对照：合并单事务的 spike 让本用例在真实副本集变红（[SAGA-11](#saga-11)）。

**7. 性能证据**（原样数值，[bench.txt](../../feature/evidence/sagadir/bench.txt)，同机 Apple M5，顺序单协程，每次新命令）

| 基准 | 样本 | 修前 | 修后 |
| --- | --- | --- | --- |
| `BenchmarkMongoCommandInboxHandle`（mongotest） | 2000 次 × 5 | 433704～450968 ns/op，8169 allocs/op | 3557160～3626163 ns/op，63276～63278 allocs/op |
| `BenchmarkRealMongoCommandInboxHandle`（真实三节点副本集） | 500 次 × 6，交替 | 8807913～9246100 ns/op | 16148137～18714654 ns/op |

结论：延迟代价来自多出的一次事务提交（多数派写）；mongotest 按集合快照、按操作查询是扫描，只作同口径对照。并发吞吐与时间分解见 [SAGA-11](#saga-11)。

**8. 未验证项与已知风险**：混跑没有实跑；`CommandInboxOptions.LeaseDuration` 不要求大于 `AckWait`（方案原意：租约总被截止封顶，截止后的投递走过期分支）；依赖步骤进程与协调器时钟偏差远小于 `Timeout`。

**9. review 检查点**

- [ ] 确认 Reserve 与执行在**两个**事务里的边界：`saga/command_consumer.go:114`（`i.reserve`，自带 `WithTransaction`）与 `:150-187`（`execute`）之间没有共享 session；`settleOwnClaim`（`saga/command_consumer.go:179`）在 handler 之后、插回执之前，确认 handler 若用了非事务 ctx 写业务，框架无法发现（契约要求业务写经事务 ctx）。
- [ ] 确认 claim 唯一索引：`<收件箱集合>_claims` 上 `uniq_command`（`saga/step_operation_inbox.go:115`）由 `EnsureInfrastructure`（`saga/command_consumer.go:83-92`）在订阅前建好；收件箱集合本身只有 `ttl_created_at`，回执排他靠 `_id`。
- [ ] `saga/command_consumer.go:126-136` 撞 `ErrDuplicateKey`：只在读回执成功时回放；读回执失败时落到 `:137` 的交还租约 + 返回原错误——确认这条路径的 claim 状态与重投行为。
- [ ] `saga/command_consumer.go:331` 把 `errAttemptSuperseded` 与过期 / fence 同样处理（先重发同一操作的成功再 ack），而原生 `:470-475` 不重发——两侧不对称是否有意。
- [ ] `SubscribeMongoStep` 没有 `LeaseDuration > AckWait` 校验（对照原生 `:403`）：`AckWait` 小于实际执行时长时，同一命令的重投会读到“自己的 claim 租约有效”→ `Reservation.Duplicate` 无 completion → `errOperationAttemptInFlight` → nak（`:118-124`），确认这条路径不会在截止前让第二个投递执行。
- [ ] `saga/nest_completion_consumer.go:150-159`：确认 `TestCompletionConsumersTermTheSameTerminalErrors` 覆盖的三种错误之外，`ErrInvalidRecord` 在两条流上同样是 Term（用例没有单列它）。
- [ ] O-S5-3：`transactionLifetimeLimitSeconds=20` 只是文档建议（SAGA.md、USER_GUIDE §7），生成的 compose 模板没改（方案原意）；确认 `roost doctor` 或部署文档是否需要检查它。

<a id="saga-10"></a>
### SAGA-10 结果先于定义到达时 nak 退避；回放不交还 claim 列为观察

> 首发 v1.21.0 · [说明](guide-saga-drv-dao-rem.md#saga-10)

**1. 提交**

| 提交 | 版本 | 内容 |
| --- | --- | --- |
| `5a3c4a60` | v1.21.0 | 发版前审查观察收尾（5 条，saga 占第 3、4 条）：`isTerminalCompletionError` 删 `ErrDefinitionMissing`；`Handle` 的 `ErrDuplicateKey` 分支加注释；回归 `saga/completion_definition_rollout_promises_test.go`；SAGA.md、方案 O-S5-1 段更正 |
| `3d3b0c09` | v1.21.0 | 记录写入提交号 |
| `8016580b` | v1.21.0 | DECISIONS-PENDING 登记发版前审查跟进 |
| `ba13cb05` | v1.23.0（本版） | 真实 NATS 上的 nak 退避 / `MaxDeliver` 实测（[SAGA-13](#saga-13)） |

**2. 改动文件与关键符号**

| `path:line` | 符号 | 职责 |
| --- | --- | --- |
| `saga/nest_completion_consumer.go:150` | `isTerminalCompletionError` | 终态只剩 `ErrNotWaiting` / `ErrNotFound` / `ErrInvalidRecord` / `ErrIdentityConflict`（`:154`） |
| `saga/engine.go:435-438` | `Complete` 里的定义查找 | 只在 `accept` 为真（记录正等着这个操作）时查，缺失返回 `ErrDefinitionMissing` |
| `saga/command_consumer.go:126-136` | `Handle` 的 `ErrDuplicateKey` 分支 | 观察：不交还 claim，注释写明理由 |

**3. 不变量与强制点**：可恢复的暂时状态不能被当成终态 Term；强制点是共用分类函数（两条流同一处），守卫 `TestACompletionBeforeItsDefinitionIsRegisteredIsRetriedNotTerminated`、`TestADefinitionThatNeverArrivesEndsInTheCoordinatorFence`，以及 O-S5-1 的 `TestCompletionConsumersTermTheSameTerminalErrors` 不改断言通过。

**4. 控制流**：结果到达 → `Complete` → 记录在等这个操作 → `e.definition(...)` 缺失 → `ErrDefinitionMissing` → 不是终态 → 消费者 nak（`NakBackoffMin` 起翻倍）→ (a) 新进程上线、定义注册 → 重投被接收；(b) 一直不来 → 步骤超时后没有定义的协调器在 `processClaimed`（`saga/engine.go:697-714`）fence 到 `ManualRequired` 并放弃关闭 → 之后重投走 `completeNotWaiting` → 迟到成功 ack 并告警一次 → `MaxDeliver` 兜底。

**5. 失败与不确定结果**

| 情形 | 处理 | 对调用方的样子 |
| --- | --- | --- |
| 结果先到、定义随后 | nak 退避，重投接收 | 记录推进 |
| 定义永不注册（配置错误） | 步骤超时前持续 nak，占一个 `MaxAckPending` 位 | 步骤超时时长与之前相同 |
| 混跑撞回执唯一键 | 回放回执，claim 留 pending、租约有效 | 读 claim 的每条路径先看回执，不会因此等待（观察结论） |
| 回执 30 天 TTL 后 claim 仍 pending | 会被接替 | 已在任何重试窗口之外 |

**6. 测试**

| 用例 | 文件 | 覆盖 |
| --- | --- | --- |
| `TestACompletionBeforeItsDefinitionIsRegisteredIsRetriedNotTerminated`（plain / native result stream） | `saga/completion_definition_rollout_promises_test.go` | 两个 Engine 共用 mongotest 存储模拟滚动发布 |
| `TestADefinitionThatNeverArrivesEndsInTheCoordinatorFence` | 同上 | fence 后重投被 ack，`LateAfterAbandon = 1` |
| `TestRealNatsCompletionNakBackoffAndMaxDeliver` | `saga/consumer_nak_maxdeliver_real_integration_test.go` | 真实 JetStream（[SAGA-13](#saga-13)） |

修前红：说明条目已原样引（出自 [发版前审查观察收尾](../../bugfix/PRERELEASE-AUDIT-FOLLOWUP-2026-10-06.md) 第 3 节）。修后（同记录）：两条流第一次投递返回可重试的 `ErrDefinitionMissing`、记录不动；注册定义后同一条消息重投被接收，记录推进到第 1 步；定义不来时 fence 之后的重投被 ack，`LateAfterAbandon` = 1。整批验证：`go test -race -count=3 ./kit/service/global/activity/ ./saga/... ./kit/saga/... ./configdata/... ./codegen/internal/tablegen/` 通过。观察第 4 条没有用例（写不出在承诺上变红的用例，记录原意）。

**7. 性能证据**：无。

**8. 未验证项与已知风险**：“滚动发布”只在替身上模拟；真实 NATS 的退避节奏在 [SAGA-13](#saga-13) 补测。

**9. review 检查点**

- [ ] `saga/engine.go:424-438`：确认 `ErrDefinitionMissing` 只可能在 `accept` 为真之后返回；`completeNotWaiting` 路径（`:433`）不查定义——否则“定义永不来”的结果会在不等待的记录上无限 nak。
- [ ] `StartSaga`（`saga/engine.go:211`）与 `Resume`（`:316`）也返回 `ErrDefinitionMissing`：确认 start 效果消费者（`saga/nest_start_consumer.go`）对它的分类与本条一致或有意不同。
- [ ] 观察第 4 条：核对 `saga/step_operation_inbox.go:164-174`（第 1 步先读回执）与 `:346-363`（`attemptResult` 先看回执）确实覆盖读 claim 的全部路径，包括 `operationSuccess`（`:322`）。
- [ ] 共用分类函数之后，任何新增“暂时性”错误都要显式不进 `isTerminalCompletionError`；是否需要一条用例枚举 `saga/errors.go` 的全部哨兵并断言其分类，防止下次误加。

<a id="saga-11"></a>
### SAGA-11 Mongo 步骤延迟分析与维护者选 A

> 首发 v1.23.0（本版） · [说明](guide-saga-drv-dao-rem.md#saga-11)

**1. 提交**

| 提交 | 版本 | 内容 |
| --- | --- | --- |
| `78e26853` | v1.23.0（本版） | 记录第十二轮决定（含“Mongo 步骤延迟待分析”） |
| `ff08c941` | v1.23.0（本版） | 分析文档、证据 `evidence/mongolat/*`、基准文件 `saga/mongo_step_latency_real_mongo_integration_test.go`；无生产代码改动 |
| `47f04413` | v1.23.0（本版） | DECISIONS-PENDING 标为已分析，待维护者选 A / B / C |
| `4c557678` | v1.23.0（本版） | 记录维护者决定（选 A），分析文档追加“维护者决定”一节 |

**2. 改动文件与关键符号**

| `path:line` | 符号 | 职责 |
| --- | --- | --- |
| `saga/mongo_step_latency_real_mongo_integration_test.go:222` | `BenchmarkRealMongoStepLatencyBreakdown` | 包住 `fmongo.IMongo` 统计每次尝试的事务 / 命令 / 提交耗时 |
| `saga/mongo_step_latency_real_mongo_integration_test.go:285` | `BenchmarkRealMongoStepThroughput` | g = 1 / 8 / 32 协程，报 ops/s、p50、p99 |
| `saga/mongo_step_latency_real_mongo_integration_test.go:340` | `BenchmarkRealMongoCommitWriteConcern` | 裸驱动单文档事务，只换写关注 |

文件带 `integration` tag，只有带 `-bench` 才运行，不会被 `-run '^TestRealMongo'` 选中；库名 `roost_mongolat_<pid>_<ns>`，跑完删除。

**3. 不变量与强制点**：契约不变（[SAGA-9](#saga-9)）。分析的“必要性”论证：(i) 在途尝试租约有效时别人要等，需要在途状态在业务事务提交前可见；(ii) 截止后即使旧事务还开着也要能接替——两者合起来要求在开业务事务之前已提交一份“我在途”的记录。守住它的是 ② 的用例 2（`in-flight attempt past its deadline is taken over and cannot commit`）。

**4. 控制流**：每次尝试的命令构成（分析文档表，原意）：修前 1 个事务 4 条命令 + 1 次提交；修后 Reserve 事务 5 条命令（读回执、读自己的 claim、守卫 upsert、按操作查 claim、写 claim）+ 1 次提交，执行事务 3 条命令（业务写、claim 条件写、插回执）+ 1 次提交。修前 5 次往返，修后 10 次。

**5. 失败与不确定结果**（各候选的正确性，分析文档原意）

| 候选 | 结论 | 失败点 |
| --- | --- | --- |
| 1 合并事务（选项 B） | 不采用 | 接替丢失；spike 在真实副本集上用例 2 变红 |
| 2 首次尝试快路径 | 不采用 | 不写守卫 → 写偏斜违反最多一次；否则退化为 B 或 C |
| 3 合并守卫与 claim 写 | 不采用 | 收益约 0.1～0.3 ms，小于噪声；要改原生投影 fence 格式 |
| 4 Reserve 降写关注 | 不采用 | 读侧反例：回放一份最终被回滚的成功 |
| 5 跨投递批量预约 | 暂不做 | 有效但引入调度层；无业务量证据；吞吐成瓶颈时首选 |

**6. 测试**

合并事务 spike 的负对照（原样，出自 [SAGA-MONGO-STEP-LATENCY](../../feature/SAGA-MONGO-STEP-LATENCY-2026-10-06.md) 候选 1，补丁全文见 [spike-merged-tx-contract-red.txt](../../feature/evidence/mongolat/spike-merged-tx-contract-red.txt)）：

```text
in-flight_attempt_past_its_deadline_is_taken_over_and_cannot_commit:
  attempt gift-1:1:1:2 after gift-1:1:1:1's deadline: {...} duplicate=false err=saga: another attempt of this step operation holds a live lease, want it to take over and execute
```

分析期间验证（文档“实施状态与验证”）：`gofmt -l saga kit/saga` 为空；`go vet ./saga/`、`go vet -tags integration ./saga/`、`go build ./... && go vet ./...`；`go test -race -count=3 ./saga/`；私有副本集 `go test -tags integration -count=1 -run '^TestRealMongo' ./saga/` 全部通过。

**7. 性能证据**

| 基准 | 样本 | 修前 | 修后 | 结论 |
| --- | --- | --- | --- | --- |
| `RealMongoStepLatencyBreakdown` sec/op | n=6（每轮 1000x，交替） | 8.965m ±3% | 18.321m ±12% | +104.35%（p=0.002）；延迟分析那一轮（benchstat 中位数）。② 实施那一轮的 9.0 → 17.4 ms/op 是 `BenchmarkRealMongoCommandInboxHandle` 500x × 6 的均值，两轮口径见 [SAGA-MONGO-STEP-LATENCY](../../feature/SAGA-MONGO-STEP-LATENCY-2026-10-06.md) 开头 |
| `RealMongoStepThroughput/g=1` ops/s | n=6 | 111.05 ±5% | 54.45 ±3% | −50.96% |
| `RealMongoStepThroughput/g=8` ops/s | n=6 | 597.0 ±3% | 296.1 ±2% | −50.40% |
| `RealMongoStepThroughput/g=32` ops/s | n=6 | 1.792k ±24% | 1.022k ±7% | −43.01% |
| p99（g=1 / 8 / 32） | n=6 | 14.06 / 22.67 / 46.27 ms | 28.11 / 39.12 / 65.08 ms | |
| commit-ms/op | n=6 | 8.061 ±5% | 15.900 ±14% | +97.25% |
| allocs/op（Breakdown） | n=6 | 598.0 | 1336.5 | +123.49% |
| spike 合并事务 | 3 轮 | — | 9.20 ms/op；108 / 624 / 1431 ops/s | 与修前无显著差别，但契约用例红 |

数值原样取自 [throughput-benchstat.txt](../../feature/evidence/mongolat/throughput-benchstat.txt)（base = `9669d181^`（`a95cf4dc`），after = `78e26853`）。服务端口径（141 次 Handle，诊断日志）：`commitTransaction` 修前平均 8.34 ms（等写关注 7.67 ms），修后 8.44 ms（8.04 ms），次数翻倍。结论：瓶颈是副本集每秒能做的落盘提交数（8 协程修前 597 次提交/s，修后 592 次/s）。

**8. 未验证项与已知风险**：64 个以上协程；选项 C 实测；生产 Linux + NVMe + 跨主机的提交耗时（文档写 1～5 ms 是推断）——[E12](../../review/EXTERNAL-VERIFICATION-2026-10-06.md)。测量期间机器 load average 3.6～6.7（文档记录）。

**9. review 检查点**

- [ ] 核对“55tps”与证据：维护者原话的 55tps 对应 `RealMongoStepThroughput/g=1` 修后 54.45 ops/s（单协程）；8 / 32 协程是 296 / 1022 ops/s。确认 E12 的通过标准（8 与 32 协程 ≥ 55）与原话口径一致。
- [ ] 确认 `saga/mongo_step_latency_real_mongo_integration_test.go` 只在 `-bench` 下运行（`-run '^TestRealMongo'` 不会选中），不会拖慢常规 integration 跑。
- [ ] 必要性论证依赖“Mongo 事务未提交的写只能通过写冲突被感知”：核对 `BenchmarkRealMongoCommitWriteConcern` 与 spike 证据是否足以支撑“没有不放松契约的单事务做法”，尤其选项 C 只做了推断。
- [ ] 候选 4 的反例（`w:1` Reserve 读到未达多数派的回执并回放）：确认当前 Reserve 事务确实以 `w:majority` 提交、snapshot 读（`mongo/driver` 事务选项），否则反例在现状下也成立。
- [ ] 影响面声明“原生步骤不经过这条路径”：原生 `DataEngineStepInbox.Reserve` 同样调 `stepOperationInbox.reserve`（分析文档“原生步骤为什么没有这笔代价”一节），确认原生步骤的 Reserve 落盘提交成本也被计入容量评估。

<a id="saga-12"></a>
### SAGA-12 saga Mod 启动时校验效果流保留期与完成回执 TTL（O-S5-2）

> 首发 v1.23.0（本版） · [说明](guide-saga-drv-dao-rem.md#saga-12)

**1. 提交**

| 提交 | 版本 | 内容 |
| --- | --- | --- |
| `7b73aabc` | v1.23.0（本版） | 第十二轮 kit 批第 1 项：`kit/saga/mod.go` `checkEffectRetention`；`kit/dataengine/mod.go` `EffectStreamRetention` / `DefaultEffectStream` / `DefaultEffectMaxAge`；回归 `kit/saga/effect_retention_promises_test.go`；T-281（同提交另有 8 项不属于本主题） |
| `d6a677e0` | v1.23.0（本版） | DECISIONS-PENDING 第十二轮 kit 批标为已实施 |

**2. 改动文件与关键符号**

| `path:line` | 符号 | 职责 |
| --- | --- | --- |
| `kit/saga/mod.go:131` | 既有校验 | `CompletionReceiptTTL <= Stream.MaxAge` 报错 |
| `kit/saga/mod.go:134` | `Init` 末尾 | `return m.checkEffectRetention(cfg)` |
| `kit/saga/mod.go:141` | `checkEffectRetention` | 结果效果流名 = DataEngine 效果流名时比较 |
| `kit/dataengine/mod.go:479-480` | `DefaultEffectStream` / `DefaultEffectMaxAge` | `ROOST_EFFECTS` / `7 * 24 * time.Hour` |
| `kit/dataengine/mod.go:492` | `EffectStreamRetention` | 按 DataEngine Mod 的读法返回流名与保留期（严格读取） |
| `kit/dataengine/mod.go:209` | DataEngine Mod 自己的读法 | 与上面同一个缺省常量 |

**3. 不变量与强制点**：结果效果流 = DataEngine 效果流时 `saga.completion_receipt_ttl > dataengine.effects.max_age`；强制点 `kit/saga/mod.go:149`（`ttl <= effectMaxAge` 报错），两边读同一份缺省与读法（`EffectStreamRetention`）。守卫 `TestModRefusesAnEffectStreamThatOutlivesTheCompletionReceipts`、`TestModChecksEffectRetentionOnlyAgainstTheStreamItReadsResultsFrom`。

**4. 控制流**：`Init` 读完全部 saga 键 → 步骤预算 → 回执 TTL > saga 流保留期 → `EffectStreamRetention(cfg)`（读 `dataengine.effects.stream`、`dataengine.effects.max_age`）→ `TrimSpace(NestResults.Stream)` 与之不同 → 返回 nil；相同 → 比较。

**5. 失败与不确定结果**

| 情形 | 处理 | 对调用方的样子 |
| --- | --- | --- |
| 同流且 `ttl <= max_age` | `Init` 返回错误，点名两个键、两个取值与流名 | 启动失败，T-281 |
| 结果在别的流上 | 不比较 | 正常启动 |
| `dataengine.effects.max_age` 写法不合法 | `EffectStreamRetention` 严格读取报错，包成 `saga: ...` | 启动失败（DataEngine Mod 也会报同一键） |
| DataEngine Mod 没装（可选依赖） | 仍按缺省 `ROOST_EFFECTS` / 168h 比较 | 缺省 720h > 168h，通过 |

**6. 测试**

| 用例 | 文件 | 覆盖 |
| --- | --- | --- |
| `TestModRefusesAnEffectStreamThatOutlivesTheCompletionReceipts` | `kit/saga/effect_retention_promises_test.go` | 800h vs 720h 拒绝，错误含两个键与两个值 |
| `TestModChecksEffectRetentionOnlyAgainstTheStreamItReadsResultsFrom` | 同上 | 缺省、同流回执更长、结果在别的流、两边一起改名（含改名后 800h 拒绝） |

修前红：说明条目已原样引（[DECISIONS-R12-KIT §1](../../feature/DECISIONS-R12-KIT-2026-10-06.md)）。修后：“修后通过；缺省值、同流且回执更长、结果在别的流、两边一起改名四种都照常启动”（同记录原文）。负对照：修前红即对照，未另做。

**7. 性能证据**：无（启动时一次）。

**8. 未验证项与已知风险**：只比较同名流；如果运维让两个名字不同的流其实是同一份数据（不可能由配置表达），不在本校验范围。

**9. review 检查点**

- [ ] `kit/saga/mod.go:146`：saga 侧 `TrimSpace(NestResults.Stream)`，`kit/dataengine/mod.go:483-488` `effectStreamName` 也 `TrimSpace`；确认 `NestResults.Stream` 的回退链（`result_effect_stream` → `start_effect_stream` → `ROOST_EFFECTS`，`kit/saga/mod.go:101`）与 `Assemble` 实际订阅的流名一致，否则校验比较的是错的流。
- [ ] `kit/saga/mod.go:149`：`<=` 使相等也被拒绝；确认这是期望（相等时最后一刻的重投与回执过期同时发生）。
- [ ] `EffectStreamRetention` 与 DataEngine Mod `Init`（`kit/dataengine/mod.go:209`）读同一个键两次：确认严格读取的报错不会因为两处都报而让运维看到两条不同措辞。
- [ ] 跨 Mod 校验放在 saga Mod 里而不是 App 层：确认在 DataEngine Mod 不存在、或两个 Mod 分属不同进程（结果流由别的进程的 DataEngine 写）时，这条校验读的 `dataengine.*` 配置确实是写流那一方的配置。

<a id="saga-13"></a>
### SAGA-13 真实 NATS 上的 nak 退避 / MaxDeliver 实测与 saga 偶发失败根因

> 首发 v1.23.0（本版） · [说明](guide-saga-drv-dao-rem.md#saga-13)

**1. 提交**

| 提交 | 版本 | 内容 |
| --- | --- | --- |
| `ba13cb05` | v1.23.0（本版） | 发版前补充验证：`saga/nest_completion_promises_test.go` 断言修正；新增 `saga/consumer_nak_maxdeliver_real_integration_test.go`；另含示例实跑门禁、global / Redis Cluster 用例（不属于本主题） |

**2. 改动文件与关键符号**

| `path:line` | 符号 | 职责 |
| --- | --- | --- |
| `saga/nest_completion_promises_test.go:99` | `TestAssemblyConsumesNativeNestCompletionEffects` | 断言改为与协调器时序无关的事实 |
| `saga/nest_completion_promises_test.go:140-160` | 新断言 | 不再等第 0 步、`CompletedSteps==1 && Step==1`、`Store.CompletionRecorded` 为真 |
| `saga/consumer_nak_maxdeliver_real_integration_test.go:79` | `TestRealNatsCompletionNakBackoffAndMaxDeliver` | 两个子用例，各自流 / 前缀 |
| `saga/engine.go:453` | `Complete` 末尾 `e.signal(e.dueKick)` | 偶发失败的产品侧原因：收下结果后立即唤醒协调器派发下一步 |

**3. 不变量与强制点**：测试只断言产品承诺（第 0 步被收下、回执已写），不断言协调器 goroutine 的调度顺序。nak 退避与 `MaxDeliver` 由 broker 与 `nats/driver` 的 settle 决定，用例在真实 JetStream 上核对。

**4. 控制流**：真实 NATS 用例：协调器存储用 mongotest，一个“还没有该定义版本”的 Engine 作 `Completer`，`SubscribeNestCompletions` 消费一条原生 completion effect；`recordingCompleter` 记每次投递时间与错误；子用例 2 在第一次投递返回后注册定义。

**5. 失败与不确定结果**

| 情形 | 实测结果 |
| --- | --- |
| 定义一直不来，`MaxDeliver=3`、`NakBackoffMin=200ms` | 投递 3 次，间隔 202 ms / 402 ms；再等 AckWait（3s）+1s 无第 4 次；`NumAckPending=0`、`NumPending=0`；记录仍在等第 0 步 |
| 定义在第 1 次投递后上线 | 第 2 次投递被接收并 ack，记录推进到第 1 步，之后不再投递 |
| 两子用例共用 effect 前缀（首次运行） | 第二个消费者（DeliverAll）读到第一个子用例的消息，`saga: not found` 按终态 Term——用例隔离问题，改为各自前缀 |

**6. 测试**

偶发失败复现条件（[发版前补充验证](../../bugfix/PRERELEASE-VERIFICATION-2026-10-06.md) 第 1 节）：`go test -race -c` 编出 `saga.test`，后台并行跑 nest / dataengine / sync / entity / skill 三轮加压；A：8 实例 × 3 轮 × `-test.count=20 -test.cpu 1,2,8`（1440 次）。修前 A 失败 8 次，全部在 `-cpu 1`（8/480），B、C 各 0 次；修后同一脚本 A 1440 次 0 失败（其中 `-cpu 1` 480 次），B、C 0 失败（记录：修前失败率下 480 次全过的概率约 e⁻⁸）。修前红原文见说明条目。

真实 NATS 用例：

```text
go test -tags integration -count=1 -race -v -run '^TestRealNatsCompletionNakBackoffAndMaxDeliver$' ./saga/ → PASS（item4-saga-nats.out）
```

（命令与结果原样摘自记录第 4a 节；原始输出在主检出被忽略的 `artifacts/perf/relprep-20261006/`，未入库。）整批验证：`go vet -tags integration ./saga/ ./remoteentity/ ./kit/service/integration/` 通过；`go test -race -count=3 ./saga/` ok；根包与 `go build ./... && go vet ./...` 通过。

**7. 性能证据**：无。

**8. 未验证项与已知风险**：同包其余 `Status == StatusWaiting` 断言逐个核对后未改（`saga/completion_definition_rollout_promises_test.go:74` 的引擎没有 `Run`；`saga/step_operation_incarnation_promises_test.go:309` 断言的是 `Complete` 返回值）——记录原意；原始压测输出不在仓库里。

**9. review 检查点**

- [ ] `saga/nest_completion_promises_test.go:140-160`：新断言 `!(Status==Waiting && OperationKey==第 0 步)` 加 `CompletedSteps==1 && Step==1` 加 `CompletionRecorded`，确认它仍能抓住“结果没被收下”的产品回退（例如把 `Complete` 改成不写回执时会红）。
- [ ] `saga/consumer_nak_maxdeliver_real_integration_test.go`：间隔断言只要求 `≥ want*9/10`，没有上界；确认“按次翻倍”的结论（202 / 402 ms）来自日志而非断言，退避上限 `NakBackoffMax` 未被覆盖。
- [ ] “第 3 次失败后 Term”：确认是 broker 达到 `MaxDeliver` 停止投递，还是 `nats/driver` 在最后一次主动 Term；两者对 `nats.jetstream.terminal.total` 计数的影响不同。
- [ ] 同包其他用例里断言协调器中间状态（`Status == StatusWaiting` 等）且引擎在 `Run` 的，是否还有同类时序假设（记录只核对了两处）。

## DRV：Redis / Mongo 驱动契约

<a id="drv-1"></a>
### DRV-1 Redis 脚本不经驱动重放（RR-20261005-NC-100）

> 首发 v1.20.1 · [说明](guide-saga-drv-dao-rem.md#drv-1)

**1. 提交**

| 提交 | 版本 | 内容 |
| --- | --- | --- |
| `81659082` | v1.20.1 | NC-100～102：脚本包成 `scriptCmd`（`NoRetry`）经 `rdb.Process` 发送；`EvalBatchDurable` 同一命令；`versionstore.Store.Update` 注释写明结果未知；故障矩阵加 `./versionstore`；回归 `script_no_retry_promises_test.go`、`versionstore/lost_reply_integration_test.go`。同提交含 NC-101（[DRV-2](#drv-2)）与 NC-102（mongotest 唯一索引遇数组返回 `ErrUnsupported`，NONCORE） |
| `998857ab` | v1.20.1 | revn04 证据补共享 toxiproxy 事故影响核对与 `-run` 复跑结果（只改证据 README） |
| `cf5721c9` | v1.20.2 | A2：`scriptCmd` 并入通用 `noReplay`（`redis/driver/replay.go`），脚本在确定没执行时恢复重发（见 [DRV-3](#drv-3)） |

**2. 改动文件与关键符号**（当前源码 `02c8a10d`）

| `path:line` | 符号 | 职责 |
| --- | --- | --- |
| `redis/driver/client.go:336` | `newScriptCmd` | 与 go-redis `cmdable.eval` 相同的参数与首键位置（`SetFirstKeyPos(3)`），集群槽位计算不变 |
| `redis/driver/client.go:353` | `runScript` | 组装脚本命令后经 `sendOnce` 发一次（A2 之后的形状；NC-100 当时是 `scriptCmd` + `rdb.Process`） |
| `redis/driver/client.go:359` / `:363` | `Client.Eval` / `EvalSha` | 走 `runScript` |
| `redis/driver/client.go:384` | `Client.EvalBatchDurable` | 脚本批次 + WAITAOF 同一独占连接；整批不经驱动重放 |
| `redis/driver/client.go:408` | `evalBatchOnConn` | 每条脚本与 WAITAOF 都以 `noReplay{…}` 入流水线，含 NoRetry 的流水线整体不重发 |
| `redis/driver/replay.go:64` | `noReplay` | `NoRetry() == true`，`Clone` 保留标记（`:69`） |
| `versionstore/versionstore.go:92` | `Store.Update` 注释 | 传输错误是结果未知；重试需要幂等 mutate；store 不重放 |
| `versionstore/redis_store.go:330` | `RedisStore.Update` | CAS 脚本调用方；只在“比较输了”时重读，不在错误上重试 |
| `kit/scripts/integration/dataengine-env.sh:164` | 故障矩阵 `test` | 加入 `./versionstore`（根包 `TestFaultMatrixScriptNamesEveryFullEnvironmentSuite` 要求列入，`integration_coverage_promises_test.go:216`） |

**3. 不变量与强制点**

- 不变量：一次脚本调用在服务端至多执行一次；回复丢失返回错误。
- 强制点：所有脚本经 `runScript` → `sendOnce`（`redis/driver/replay.go:110`），后者把命令包成 `noReplay` 再交给 `rdb.Process`；go-redis 的 `processWithRetry` / `generalProcessPipeline` / Cluster `process` 看到 `NoRetry` 都跳过。MOVED / ASK 重定向分支在 NoRetry 检查之前，照常跟随（脚本在错误节点上没有执行）。
- 守卫测试：`TestAScriptWhoseReplyIsLostIsNotReplayedByTheDriver`（`redis/driver/script_no_retry_promises_test.go:127`，RESP 替身：eval / evalsha 各执行恰好 1 次且返回错误）；`TestNoReplayMarkSurvivesCloneAndUnmarkedCommandsKeepTheDriverRetry`（原名 `TestOnlyScriptCommandsOptOutOfTheDriverRetry`，`:166`，标记克隆后仍在、普通 GET 不带标记）；`TestRealRedisUpdateWhoseReplyIsLostNeverWritesTwice`（`versionstore/lost_reply_integration_test.go:103`，`-tags integration`，真实 Redis + 自建随机端口 toxiproxy 代理）。

**4. 控制流**

1. `versionstore.RedisStore.Update` 调 `IRedis.Eval(CAS 脚本)`。
2. `Client.Eval` → `runScript` → `newScriptCmd` → `sendOnce(ctx, rdb, resends, cmd)`。
3. `sendOnce` 发 `noReplay{cmd}`；成功返回结果；错误时只有 `shouldResend`（确定没执行且不是 `ErrClosed`）才退避重发（A2 起；v1.20.1 时一律不重发）。
4. 回复丢失 → EOF / 超时原样返回 → `Update` 原样返回错误（不重读、不重试）。

**5. 失败与不确定结果**

| 情形 | 处理 | 调用方看到 |
| --- | --- | --- |
| 脚本执行后回复丢失（EOF / 部分回复 / 读超时） | 不重发 | 传输错误（结果未知）；`IsDefinitelyNotExecuted` 为假 |
| 取连接时拨号失败、LOADING 等执行前拒绝 | v1.20.1：不重发；v1.20.2 起：重发至多 `resendsFor(MaxRetries)` 次 | 成功，或最后一次的确定未执行错误 |
| Cluster MOVED / ASK | go-redis 跟随重定向 | 正常结果 |
| 脚本自身报错（`ERR … user_script`） | 不重发 | 已执行并报错 |

**6. 测试**

| 用例 | 文件 | 覆盖 |
| --- | --- | --- |
| `TestAScriptWhoseReplyIsLostIsNotReplayedByTheDriver` | `redis/driver/script_no_retry_promises_test.go` | eval / evalsha 回复丢失只执行一次、返回错误 |
| `TestNoReplayMarkSurvivesCloneAndUnmarkedCommandsKeepTheDriverRetry`（原名 `TestOnlyScriptCommandsOptOutOfTheDriverRetry`） | 同上 | `noReplay` 标记与克隆；普通命令仍可重试 |
| `TestRealRedisUpdateWhoseReplyIsLostNeverWritesTwice` | `versionstore/lost_reply_integration_test.go` | 真实 Redis：一次 Update 在 Redis 中至多一次写入、调用方拿到错误 |

修前红文本（原样，出处 `docs/bugfix/evidence/noncore-bugfix-20261005-revn04/nc100-driver-red.txt`）：

```text
=== RUN   TestAScriptWhoseReplyIsLostIsNotReplayedByTheDriver
=== RUN   TestAScriptWhoseReplyIsLostIsNotReplayedByTheDriver/eval
    script_no_retry_promises_test.go:153: one eval call executed the script 2 times on the server (reply=1 err=<nil>); want exactly once
=== RUN   TestAScriptWhoseReplyIsLostIsNotReplayedByTheDriver/evalsha
    script_no_retry_promises_test.go:153: one evalsha call executed the script 2 times on the server (reply=1 err=<nil>); want exactly once
--- FAIL: TestAScriptWhoseReplyIsLostIsNotReplayedByTheDriver (0.04s)
    --- FAIL: TestAScriptWhoseReplyIsLostIsNotReplayedByTheDriver/eval (0.03s)
    --- FAIL: TestAScriptWhoseReplyIsLostIsNotReplayedByTheDriver/evalsha (0.01s)
FAIL
FAIL	github.com/tjbdwanghaibo/roost-core/redis/driver	0.383s
FAIL
```

真实 Redis 修前 / 修后（原样，出处 `nc100-versionstore-real-red.txt` / `nc100-versionstore-real-green.txt`，同一证据目录）：

```text
=== RUN   TestRealRedisUpdateWhoseReplyIsLostNeverWritesTwice
    lost_reply_integration_test.go:154: one Update wrote its mutation 2 times: stored [hello msg-1 msg-1] / v3 (update returned [hello msg-1 msg-1] / v3 applied=true err=<nil>, mutate ran 2 times)
--- FAIL: TestRealRedisUpdateWhoseReplyIsLostNeverWritesTwice (0.12s)
FAIL
FAIL	github.com/tjbdwanghaibo/roost-core/versionstore	0.668s
FAIL
=== RUN   TestRealRedisUpdateWhoseReplyIsLostNeverWritesTwice
    lost_reply_integration_test.go:163: lost reply surfaced as EOF; Redis holds [hello msg-1] / v2
--- PASS: TestRealRedisUpdateWhoseReplyIsLostNeverWritesTwice (0.03s)
PASS
ok  	github.com/tjbdwanghaibo/roost-core/versionstore	0.578s
```

修后其他命令与结果（原样，出处 `docs/bugfix/RR-20261005-NC-100.md`“回归命令与结果”）：

```text
GOWORK=off go test -race -count=3 ./redis/... ./versionstore/ ./cache/...                      ok
REDIS_ADDR=127.0.0.1:17379 GOWORK=off go test -tags integration -count=1 -p 1 ./kit/service/... ./service/... ./versionstore/ ./cache/...
  1008 pass / 30 环境 skip / 0 fail（真实 Redis，各用例独立前缀）
GOWORK=off go test -tags integration -count=1 -run TestRealVersionedLock ./remoteentity/     ok（真实 Redis）
GOWORK=off go test -tags integration -count=1 -run 'TestMGet|TestIntegrationPipeline|TestDistLockExpiry' ./redis/driver/   ok
```

负对照：问题记录写明，同一真实 Redis 用例把 `MaxRetries` 设为 -1 时 Update 返回 `EOF`、Redis 中 `[hello msg-1] / v2`（探针已删，结论写入正式回归）。

**7. 性能证据**：无（不涉及热路径；只改命令的重试标记）。

**8. 未验证项与已知风险**

- Redis Cluster 下脚本的 MOVED / ASK 与节点故障（本机无 Cluster），外部验证 E08。
- `redis/driver/lock_toxic_integration_test.go` 会 `/reset` 共享 toxiproxy，本轮没在共享环境上跑。
- remoteentity marker / snapshot L2 在回复丢失时现在看到错误而不是驱动重放，只经既有单元与真实锁用例验证，未上故障矩阵。
- 测试名 `TestOnlyScriptCommandsOptOutOfTheDriverRetry` 在 A2 之后已不准确（写命令也带 `NoRetry`），注释已改，名字未改。**已改名（fixs）**：`TestNoReplayMarkSurvivesCloneAndUnmarkedCommandsKeepTheDriverRetry`。

**9. review 检查点**

- [ ] 确认全部脚本入口都经 `runScript` / `noReplay`：`grep -n 'rdb.Eval\|rdb.EvalSha\|\.Eval(ctx' redis/driver/*.go`，生产代码里不应有直接 `c.rdb.Eval(...)`；`EvalReplicated`（`redis/driver/replicated.go:72-75`）与 `evalBatchOnConn`（`redis/driver/client.go:408`）都包了 `noReplay`。
- [ ] 确认 `noReplay.Clone`（`redis/driver/replay.go:69`）保留标记：Cluster 路由层复制命令时若退回原命令，就会重新可重放。
- [ ] 确认 `newScriptCmd`（`redis/driver/client.go:336`）的首键位置与 go-redis `cmdable.eval` 一致（无键时不设 `FirstKeyPos`），否则 Cluster 槽位计算会变。
- [ ] 确认 `versionstore/redis_store.go:330` 的 `Update` 在传输错误上直接返回、不重读重试；只在 CAS 返回“比较输了”时重读。
- [ ] 确认仓内生产代码没有经 `Client.Raw()` 发脚本或写命令（当前 `grep -rn 'Raw()' --include='*.go' . | grep -v _test.go` 只有定义处）。

<a id="drv-2"></a>
### DRV-2 Mongo 事务提交受 transaction_timeout 约束（RR-20261005-NC-101 及两处复审）

> 首发 v1.20.1 · [说明](guide-saga-drv-dao-rem.md#drv-2)

**1. 提交**

| 提交 | 版本 | 内容 |
| --- | --- | --- |
| `81659082` | v1.20.1 | `session.WithTransaction` 自实现与驱动便捷 API 相同的重试规则，提交用带 `transaction_timeout` 截止的 ctx；回调失败的 abort 有 5s 上限；`mongo/config.go` 注释；回归 `transaction_deadline_integration_test.go` |
| `edbe290b` | v1.20.1 | 复审 1：`EndSession` 用 `WithoutCancel` + 5s 上限；回归 `TestRealMongoEndSessionAfterCommitTimeoutIsBounded` |
| `f608503b` | v1.20.1 | 复审 2：窗口在退避里到期时返回 `errors.Join(ctx.Err(), lastErr)`；回归 `transaction_retry_chain_test.go` |
| `cf5721c9` | v1.20.2 | A2：提交发出后失败包 `fmongo.ErrCommitResultUnknown`（见 [DRV-3](#drv-3)） |

**2. 改动文件与关键符号**

| `path:line` | 符号 | 职责 |
| --- | --- | --- |
| `mongo/driver/session.go:19` | `defaultTransactionTimeout` | `TransactionTimeout <= 0` 时用 120s（同驱动） |
| `mongo/driver/session.go:22` | `transactionAbortTimeout` | abort 与 EndSession 的 5s 上限 |
| `mongo/driver/session.go:24-25` | `transactionBackoffInitial` / `Max` | 5ms 起、封顶 500ms |
| `mongo/driver/session.go:56` | `session.WithTransaction` | 自实现重试循环；`ctx` 套 `TransactionTimeout` 截止（`:64`） |
| `mongo/driver/session.go:69` / `:78` | `lastErr` / 退避分支 | 记住触发重跑的错误；退避中到期返回 `errors.Join(ctx.Err(), lastErr)` |
| `mongo/driver/session.go:98` | 提交前截止检查 | 已到期不发提交，abort 后返回 ctx 错误 |
| `mongo/driver/session.go:119` | `session.commit` | UnknownTransactionCommitResult（非 MaxTimeMS）只重试提交；Transient 返回 `retryCallback` |
| `mongo/driver/session.go:142` | `session.abort` | `WithoutCancel` + 5s |
| `mongo/driver/session.go:160` | `session.EndSession` | `WithoutCancel` + 5s；无进行中事务时驱动不发命令 |
| `mongo/config.go:12` | `Config.TransactionTimeout` 注释 | 端到端约束回调重试与提交 |

**3. 不变量与强制点**

- 不变量 1：整次 `WithTransaction`（含提交）不晚于 `TransactionTimeout` 返回；回调失败时另加至多 5s abort。强制点：`mongo/driver/session.go:64` 的 `context.WithTimeout` 交给回调与 `CommitTransaction`。
- 不变量 2：调用方 `defer EndSession(ctx)` 不因无截止的 ctx 而无界阻塞。强制点：`mongo/driver/session.go:164`。
- 不变量 3：窗口到期时最后一次事务错误的链不丢。强制点：`mongo/driver/session.go:78`。
- 守卫测试：`TestRealMongoCommitIsBoundedByTransactionTimeout`（`mongo/driver/transaction_deadline_integration_test.go:103`）、`TestRealMongoEndSessionAfterCommitTimeoutIsBounded`（`:191`）、`TestWithTransactionKeepsTheLastCallbackErrorWhenTheWindowClosesInBackoff`（`mongo/driver/transaction_retry_chain_test.go:23`，不需要服务端、不靠 sleep）。

**4. 控制流**

```mermaid
flowchart TD
    A[WithTransaction ctx 套 TransactionTimeout] --> B{"backoff > 0"}
    B -- 是 --> C{退避期间 ctx 到期}
    C -- 是 --> R1["返回 errors.Join ctx.Err, lastErr<br/>确定未提交"]
    C -- 否 --> D
    B -- 否 --> D[StartTransaction]
    D --> E[回调 fn]
    E -- 错误 --> F[abort WithoutCancel+5s]
    F --> G{Transient 且 ctx 未到期}
    G -- 是 --> H["lastErr = err"] --> B
    G -- 否 --> R2["返回回调错误<br/>确定未提交"]
    E -- 成功 --> I{ctx 已到期}
    I -- 是 --> J[abort] --> R3["返回 ctx 错误<br/>确定未提交"]
    I -- 否 --> K[commit 带截止 ctx]
    K -- UnknownCommitResult 非 MaxTimeMS 且 ctx 未到期 --> K
    K -- Transient --> L["lastErr = 提交错误"] --> B
    K -- 其他错误 / ctx 到期 --> R4["v1.20.1: 原样返回<br/>v1.20.2 起: 包 ErrCommitResultUnknown"]
    K -- 成功 --> R5[nil]
```

调用方随后 `defer EndSession(ctx)`：提交因截止失败时驱动事务状态仍是 InProgress，驱动在 `EndSession` 里补发 abort；`mongo/driver/session.go:164` 给它 `WithoutCancel` + 5s 上限。

**5. 失败与不确定结果**

| 情形 | 处理 | 调用方看到 |
| --- | --- | --- |
| 回调返回非 Transient 错误 | abort（5s 上限）后返回 | 回调错误，确定未提交 |
| 回调 / 提交 Transient，窗口在退避里到期 | 不再重跑 | `errors.Join(ctx.Err(), lastErr)`，确定未提交；`errors.Is` DeadlineExceeded 与 lastErr 链都成立 |
| 回调完成时截止已到 | 不发提交，abort | ctx 错误，确定未提交 |
| 提交发出后网络黑洞 / 截止落在提交中途 | 不 abort（与驱动一致） | 驱动错误；v1.20.2 起包 `ErrCommitResultUnknown`，可能已提交 |
| 提交后 `EndSession` 补发 abort 遇黑洞 | 5s 后放弃 | 无错误；服务端事务持锁到 `transactionLifetimeLimitSeconds`（缺省 60s） |

**6. 测试**

| 用例 | 文件 | 覆盖 |
| --- | --- | --- |
| `TestRealMongoCommitIsBoundedByTransactionTimeout` | `mongo/driver/transaction_deadline_integration_test.go` | 真实副本集 + 自建 toxiproxy，upstream / downstream 黑洞，2s 窗口不晚于 4s 返回 |
| `TestRealMongoEndSessionAfterCommitTimeoutIsBounded` | 同上 | 提交超时后 `EndSession` 不晚于 9s |
| `TestWithTransactionKeepsTheLastCallbackErrorWhenTheWindowClosesInBackoff` | `mongo/driver/transaction_retry_chain_test.go` | 退避中到期保留 `ErrDuplicateKey` 与 Transient 标签 |
| `TestRealMongoWriteErrorAbortsTransactionAndKeepsDriverChain` | `mongo/driver/real_mongo_transaction_test.go` | 既有：transient 重跑一次、写错误中止不重跑 |

修前红文本（原样，出处 `docs/bugfix/evidence/noncore-bugfix-20261005-revn04/nc101-real-mongo-red.txt`）：

```text
=== RUN   TestRealMongoCommitIsBoundedByTransactionTimeout
=== RUN   TestRealMongoCommitIsBoundedByTransactionTimeout/upstream
    transaction_deadline_integration_test.go:150: upstream: elapsed=30.2s calls=1 committed=1 err=<nil>
    transaction_deadline_integration_test.go:152: WithTransaction with transaction_timeout=2s returned after 30.2s (bound 4s); the commit is not bounded
=== RUN   TestRealMongoCommitIsBoundedByTransactionTimeout/downstream
    transaction_deadline_integration_test.go:150: downstream: elapsed=29.79s calls=1 committed=1 err=server selection error: context deadline exceeded, current topology: { Type: Single, Servers: [{ Addr: 127.0.0.1:56845, Type: Unknown, Last error:  connection(127.0.0.1:56845[-201]) incomplete read of message header: context deadline exceeded: client timed out waiting for server response: read tcp 127.0.0.1:56949->127.0.0.1:56845: i/o timeout: connection(127.0.0.1:56845[-201]) incomplete read of message header: context deadline exceeded: client timed out waiting for server response: read tcp 127.0.0.1:56949->127.0.0.1:56845: i/o timeout }, ] }
    transaction_deadline_integration_test.go:152: WithTransaction with transaction_timeout=2s returned after 29.79s (bound 4s); the commit is not bounded
--- FAIL: TestRealMongoCommitIsBoundedByTransactionTimeout (61.25s)
    --- FAIL: TestRealMongoCommitIsBoundedByTransactionTimeout/upstream (30.59s)
    --- FAIL: TestRealMongoCommitIsBoundedByTransactionTimeout/downstream (30.65s)
FAIL
FAIL	github.com/tjbdwanghaibo/roost-core/mongo/driver	61.781s
FAIL
```

修后（原样节选，出处 `nc101-real-mongo-green.txt`，整包 integration 输出）：

```text
    transaction_deadline_integration_test.go:150: upstream: elapsed=2s calls=1 committed=0 err= connection(127.0.0.1:57151[-144]) incomplete read of message header: context deadline exceeded: client timed out waiting for server response: read tcp 127.0.0.1:57154->127.0.0.1:57151: i/o timeout: connection(127.0.0.1:57151[-144]) incomplete read of message header: context deadline exceeded: client timed out waiting for server response: read tcp 127.0.0.1:57154->127.0.0.1:57151: i/o timeout
--- PASS: TestRealMongoCommitIsBoundedByTransactionTimeout (5.37s)
    --- PASS: TestRealMongoCommitIsBoundedByTransactionTimeout/upstream (2.79s)
    --- PASS: TestRealMongoCommitIsBoundedByTransactionTimeout/downstream (2.58s)
```

两处复审的红绿（原样，出处 `docs/bugfix/RR-20261005-NC-101.md`“复审追加”）：

```text
GOWORK=off go test -tags integration -count=1 -run TestRealMongoEndSessionAfterCommitTimeoutIsBounded -v ./mongo/driver/
# 修前（81659082 的 session.go）：
#   upstream: WithTransaction=2s EndSession done at 30.01s（看门狗恢复网络的时刻）  FAIL bound 9s
#   downstream: WithTransaction=2s EndSession done at 24.97s                         FAIL bound 9s
# 修后：upstream / downstream 均 EndSession done at 7s（2s 窗口 + 5s abort 上限）     PASS
```

```text
GOWORK=off go test -count=5 -run TestWithTransactionKeepsTheLastCallbackErrorWhenTheWindowClosesInBackoff ./mongo/driver/
# 修前：5/5 FAIL  err=context deadline exceeded after 10 callbacks lost the last callback error (errors.Is ErrDuplicateKey=false, transient label=false)
# 修后：-count=60 与 -race -count=30 全部 PASS（60 次里 1 次窗口落在回调与检查之间，那条分支本来就返回回调错误）
```

其他修后验证（`docs/bugfix/RR-20261005-NC-101.md`）：`go test -race -count=3 ./mongo/...` ok；mongotest 消费包 `./dataengine/engine ./kit/dataengine ./kit/saga ./nestwal ./remoteentity ./saga` ok；第 22 轮 28 个正式链路消费叶子在真实副本集上两次全部通过。

**7. 性能证据**：未测（只改超时与错误包装，正常路径无额外往返；`EndSession` 在无进行中事务时不发命令）。

**8. 未验证项与已知风险**

- mongos 上提交失败后 abort 的并发问题（本实现同样不 abort，未实测）、主从切换期间的提交、`transactionLifetimeLimitSeconds` 与短窗口的组合：E11。
- 驱动升级时便捷 API 规则若变，需同步本循环（`mongo/driver/README.md` 文首写明）。
- 上限内没送达的 abort 留下的服务端事务持锁到 60s，其间同文档写入得到 WriteConflict 并重跑。
- kit/dataengine 的真实 Mongo 集成套件（含主节点切换）本轮没在共享环境上跑。

**9. review 检查点**

- [ ] 确认 `CommitTransaction` 只经 `session.commit`（`mongo/driver/session.go:121`）调用，且传入的是 `:64` 带截止的 ctx，不是 background。
- [ ] 确认 `session.commit` 在 `ctx.Err() != nil` 时（`:125`）直接返回、不再重试提交；这使“Transient 标签 + ctx 已到期”的提交错误被当作结果未知返回——确认这一保守归类可接受（它确实已发出提交）。
- [ ] 确认 `lastErr` 只在两处赋值（`:92` 回调 Transient、`:104` 提交 Transient），两处都一定未提交，因此退避分支 `:78` 不带 `ErrCommitResultUnknown` 是正确的。
- [ ] 确认全部正式调用方都 `defer session.EndSession(...)` 且不依赖 EndSession 的阻塞语义：`grep -rn 'EndSession(' --include='*.go' dataengine remoteentity saga nestwal`。
- [ ] 确认 `abort` / `EndSession` 用的是 `context.WithoutCancel(ctx)` 加上限（`:143`、`:164`），而不是 `context.Background()` 无上限。

<a id="drv-3"></a>
### DRV-3 A2 驱动重放契约：写不重放、确定没执行才重发、提交结果未知带哨兵

> 首发 v1.20.2 · [说明](guide-saga-drv-dao-rem.md#drv-3)

**1. 提交**

| 提交 | 版本 | 内容 |
| --- | --- | --- |
| `d346b45c` | v1.20.2 | 记录维护者第二轮决定（A2 按推荐） |
| `cf5721c9` | v1.20.2 | `redis/driver/replay.go`（新）；19 个写方法、pipeline、`EvalBatchDurable`、DistLock 改走 `sendOnce`；`mongo.ErrCommitResultUnknown`；cache 补发 EXPIRE；两份 README 契约表；USER_GUIDE / TROUBLESHOOTING T-259 / NC-100、NC-101 bugfix 追记 |
| `bb3aa647` | v1.20.2 | DECISIONS-PENDING A2 标为已实施 |
| `5df60765` | v1.20.2 | A4 留项：kit/redis 的 `redis.db` / `pool_size` / `min_idle_conns` 严格读取（A4 方案写明这三项“留给 A2 之后”，属 CFG 主题，此处只记关联） |
| `88f33776` | v1.23.0 | A2 方案“未完成 / 观察”补第十二轮后续（bus SETNX 去重保持、Close 契约），README §5（见 [DRV-5](#drv-5)） |
| `db67b8ee` | v1.23.0 | 契约表 §1 / §2 增 `EvalReplicated` 与 WAIT 分类（见 [DRV-4](#drv-4)） |
| `d05a04a1` | v1.23.0 | 契约表 §5 改为统一后的 Close 口径（见 [DRV-5](#drv-5)） |

**2. 改动文件与关键符号**

| `path:line` | 符号 | 职责 |
| --- | --- | --- |
| `redis/driver/replay.go:36` | `IsDefinitelyNotExecuted` | 唯一的错误分类函数（拨号 / 池 / `ErrClosed` / 七种执行前拒绝） |
| `redis/driver/replay.go:57` | `shouldResend` | 确定没执行且不是 `ErrClosed` |
| `redis/driver/replay.go:64` | `noReplay` | 给任意命令加 `NoRetry` |
| `redis/driver/replay.go:73` | `resendsFor` | -1 → 0，0 → 3，其他照用 |
| `redis/driver/replay.go:92` | `waitBeforeResend` | 10ms 起、封顶 1s，与 go-redis 缺省同公式 |
| `redis/driver/replay.go:110` | `sendOnce` | 发一次；确定没执行才重发；退避中 ctx 结束返回 `errors.Join(err, ctx.Err())` |
| `redis/driver/replay.go:127` / `:132` | `buildWrite` / `write` | 用 go-redis 类型化方法组装（入队到用完即弃的 Pipeliner、不经它发送），再 `sendOnce` |
| `redis/driver/replay.go:139` | `allNotExecuted` | pipeline 每条都确定没执行 |
| `redis/driver/client.go:22` / `:91` | `Client.resends` | 由 `Config.MaxRetries` 换算 |
| `redis/driver/client.go:150`–`:464` | `Set` `SetNX` `Del` `Expire` `Incr` `IncrBy` `HSet` `HDel` `LPush` `RPush` `LPop` `RPop` `LTrim` `LRem` `ZAdd` `ZRem` `SAdd` `SRem` `Publish` | 19 个写方法全部 `write(ctx, c, …)` |
| `redis/driver/pipeline.go:45` | `queueWrite` | 写命令以 `noReplay` 入队，置 `hasWrites` |
| `redis/driver/pipeline.go:115` | `pipeline.Exec` | 含写的整条只在 `allNotExecuted` 时重发（`:124`）；只读 pipeline 交给驱动 |
| `redis/driver/client.go:384` | `EvalBatchDurable` | 整批确定没执行时换新连接重发（`:398`） |
| `redis/driver/lock.go:94` / `:133` / `:161` | `distLock.Acquire` / `Release` / `Extend` | SETNX 经 `sendOnce`，释放 / 续期脚本经 `runScript` |
| `redis/driver/assembly.go:30` | `Assemble` | 把客户端的 `resends` 传给锁工厂 |
| `cache/redis_hash.go:59`（补发在 `:92-96`） | `RedisJSONHashStore.Set` | HSET 结果未知仍补发 EXPIRE，返回 HSET 的错误 |
| `cache/redis_raw.go:93`（补发在 `:98-101`） | `RedisRawSortedSetStore.SetScore` | ZADD 同上 |
| `mongo/errors.go:16` | `ErrCommitResultUnknown` | 提交已发出、结论未知的哨兵 |
| `mongo/session.go:10-11` | `ISession.WithTransaction` 注释 | 写明哨兵语义 |
| `mongo/driver/session.go:111` | `WithTransaction` | `fmt.Errorf("%w: %w", fmongo.ErrCommitResultUnknown, err)` |
| `redis/driver/README.md`、`mongo/driver/README.md` | 契约表 | §1 重放、§2 分类、§3 ctx 替换、§4 默认值、§5 Close、§6 新增调用点核对清单 |

**3. 不变量与强制点**

- 不变量 1：经 `fredis.IRedis` 的写命令不会被 go-redis 在任何错误上自动重发。强制点：每个写方法唯一经 `write` → `sendOnce` → `noReplay`；pipeline 写方法唯一经 `queueWrite`。
- 不变量 2：重发只发生在 `shouldResend` 为真时，且次数 ≤ `resends`。强制点：`sendOnce`（`redis/driver/replay.go:114`）、`pipeline.Exec`（`redis/driver/pipeline.go:124`）、`EvalBatchDurable`（`redis/driver/client.go:398`）、`EvalReplicated`（`redis/driver/replicated.go:57`）四处循环。
- 不变量 3：`ErrCommitResultUnknown` 只在提交命令已发出后返回。强制点：`mongo/driver/session.go:111` 是唯一包装处，位于 `s.commit(ctx)` 之后、`retryCallback` 判断之后。
- 守卫测试：`TestAWriteWhoseReplyIsLostIsNotReplayedByTheDriver`（`redis/driver/write_no_replay_promises_test.go:204`，21 个调用点）、`TestNotExecutedErrorsAreStillResent`（`:226`，7 种拒绝 × 调用点）、`TestResendsStopAtTheConfiguredBudget`（`:265`）、`TestAPipelineWithAnExecutedCommandIsNotResent`（`:285`）、`TestReadsKeepTheDriverRetry`（`:300`）、`TestDurableBatchIsResentWhenNothingExecuted`（`:313`）、`TestIsDefinitelyNotExecuted`（`:326`，分类表）、`TestRealRedisAWriteWhoseReplyIsLostRunsOnce`（`redis/driver/write_lost_reply_integration_test.go:71`，真实 Redis）、`TestAWriteWhoseReplyIsLostStillGetsItsTTL`（`cache/redis_lost_write_ttl_promises_test.go:53`）、`TestWithTransactionFailuresBeforeTheCommitAreNotResultUnknown`（`mongo/driver/transaction_retry_chain_test.go:60`）、`TestRealMongoCommitIsBoundedByTransactionTimeout`（断言提交被截断时 `errors.Is ErrCommitResultUnknown`）。

**4. 控制流**

```mermaid
flowchart TD
    W[写方法 / Eval / DistLock] --> B[buildWrite 或 newScriptCmd]
    B --> S["sendOnce: rdb.Process noReplay cmd"]
    S -- nil --> OK[返回结果]
    S -- 错误 --> C{"shouldResend<br/>确定没执行且非 ErrClosed"}
    C -- 否 --> U["原样返回<br/>EOF / 超时 = 结果未知"]
    C -- 是 --> D{"attempt < resends"}
    D -- 否 --> N["返回最后一次<br/>确定未执行错误"]
    D -- 是 --> E[waitBeforeResend 10ms..1s]
    E -- ctx 结束 --> J["errors.Join err, ctx.Err<br/>仍属确定未执行"]
    E -- 到时 --> S
```

pipeline：`Exec` 先整体发一次；`hasWrites` 为真且每条命令错误都 `shouldResend` 时，清错误、整条重新入队再发（`redis/driver/pipeline.go:124-134`）；只要有一条可能已执行就不重发。

**5. 失败与不确定结果**

| 情形 | 处理 | 调用方看到 |
| --- | --- | --- |
| 写命令回复丢失（EOF / 重置 / 读超时 / ctx 到期） | 不重发 | 传输错误，结果未知；返回值不可信 |
| 拨号失败 / 池超时 / 池耗尽 | 退避后重发 ≤ resends | 成功或最后的确定未执行错误 |
| LOADING / MASTERDOWN / TRYAGAIN / CLUSTERDOWN / max clients / READONLY / NOREPLICAS | 同上 | 同上 |
| `ErrClosed`（客户端已关） | 不重发 | `ErrClosed`（确定未执行） |
| pipeline 中部分已执行 | 整条不重发 | `Exec` 返回错误；各 future 带各自结果 |
| WRONGTYPE、脚本 `ERR … user_script` | 不重发 | 已执行并报错 |
| HSET / ZADD 结果未知（cache） | 仍补发一次 EXPIRE，吞掉 EXPIRE 的错误 | 返回 HSET / ZADD 的错误 |
| Mongo 提交发出后失败 | 不 abort | `ErrCommitResultUnknown` 包原错误 |
| Mongo 回调失败 / 提交前到期 / 退避中到期 / StartTransaction 失败 | — | 不带哨兵，确定未提交 |

**6. 测试**

| 用例 | 文件 | 覆盖 |
| --- | --- | --- |
| `TestAWriteWhoseReplyIsLostIsNotReplayedByTheDriver` | `redis/driver/write_no_replay_promises_test.go` | 21 个调用点（`writeCalls`：incr、incrby、rpush、lpush、lpop、rpop、set（经 SetNX）、del、hset、hdel、expire、zadd、zrem、sadd、srem、ltrim、lrem、publish、eval、pipeline、distlock）回复丢失只执行一次、返回错误且不被判为确定未执行 |
| `TestNotExecutedErrorsAreStillResent` | 同上 | 7 种拒绝下各调用点重发 |
| `TestResendsStopAtTheConfiguredBudget` | 同上 | 1 次发送 + 3 次重发后交回 LOADING |
| `TestAPipelineWithAnExecutedCommandIsNotResent` | 同上 | 有一条已执行就不整条重发 |
| `TestReadsKeepTheDriverRetry` | 同上 | 读命令仍由驱动重试（服务端执行 2 次） |
| `TestDurableBatchIsResentWhenNothingExecuted` | 同上 | `EvalBatchDurable` 整批重发 |
| `TestIsDefinitelyNotExecuted` | 同上 | 分类表（拨号、池、ErrClosed、EOF、超时、ctx、脚本错误…） |
| `TestRealRedisAWriteWhoseReplyIsLostRunsOnce` | `redis/driver/write_lost_reply_integration_test.go` | 真实 Redis + 自建 toxiproxy：incr / rpush / pipeline / setnx / distlock |
| `TestAWriteWhoseReplyIsLostStillGetsItsTTL` | `cache/redis_lost_write_ttl_promises_test.go` | HSET / ZADD 结果未知后键仍有 TTL |
| `TestWithTransactionFailuresBeforeTheCommitAreNotResultUnknown` | `mongo/driver/transaction_retry_chain_test.go` | 回调错误、提交前到期都不带哨兵 |

修前红文本（原样，出处 `docs/feature/A2-DRIVER-REPLAY-CONTRACT-2026-10-05.md`“先红后绿”）：

```text
# 修前（d346b45c 的 client.go / pipeline.go / lock.go / assembly.go；新用例与分类函数临时放在一起）
source ~/.roost-it/roost-dataengine-it/env.sh
GOWORK=off go test -tags integration -count=1 -run TestRealRedisAWriteWhoseReplyIsLostRunsOnce -v ./redis/driver/
  incr: err=<nil>; Redis holds counter=2                 one incr call was applied 2 times
  rpush: err=<nil>; Redis holds list=[msg-1 msg-1]       one rpush call was applied 2 times
  pipeline: err=<nil>; Redis holds counter=2 list=[msg-1 msg-1]
  setnx: the replayed SET NX saw this call's own key and reported it as taken (value=mine)
  distlock: the replayed SET NX saw this call's own key and reported it as taken (held-after-reconcile=1)
  （5/5 FAIL；真实 Redis + 本用例自建、端口随机的 toxiproxy 代理，limit_data 1 字节）
GOWORK=off go test -count=1 -run 'TestAWriteWhoseReplyIsLost|TestNotExecuted|TestDurableBatch' ./redis/driver/
  one {incr,incrby,rpush,lpush,lpop,rpop,set,del,hset,hdel,expire,zadd,zrem,sadd,srem,ltrim,lrem,publish,pipeline,distlock} call executed … 2 times on the server
  eval rejected once with "{LOADING,MASTERDOWN,TRYAGAIN,CLUSTERDOWN,ERR max number of clients reached,READONLY,NOREPLICAS} …" (never executed) failed instead of being resent
  EvalBatchDurable = [], local=0, LOADING Redis is loading the dataset in memory; want one result after one resend
  （28 个用例 / 子用例 FAIL）
GOWORK=off go test -tags integration -count=1 -run TestRealMongoCommitIsBoundedByTransactionTimeout -v ./mongo/driver/
  upstream committed=0 / downstream committed=1: commit was sent and cut by the deadline but err=… errors.Is ErrCommitResultUnknown=false DeadlineExceeded=true
GOWORK=off go test -count=1 -run TestAWriteWhoseReplyIsLostStillGetsItsTTL ./cache/
  HSET reached Redis but the key carries TTL 0s (want 1m0s): it never expires（ZADD 同）

# 修后：上述全部 PASS
  incr/rpush/pipeline/setnx/distlock: err=EOF; Redis holds counter=1 / [msg-1] / value=mine / held-after-reconcile=0
  mongo upstream / downstream: err=mongo: transaction commit result unknown: … context deadline exceeded（errors.Is 两者均成立）
```

修后验证（原样，同一出处“验证”）：

```text
GOWORK=off go test -race -count=3 ./redis/... ./cache/ ./mongo/...                         ok
source env.sh; GOWORK=off go test -tags integration -count=1 -p 1 ./redis/...              ok（含 NC-208 的自建代理 toxic 锁用例）
GOWORK=off go test -tags integration -count=1 -run 'TestRealMongo(Commit|EndSession|WriteError)' ./mongo/driver/   ok
REDIS_ADDR=<隔离 Redis> GOWORK=off go test -tags integration -count=1 -p 1 ./kit/service/... ./service/... ./versionstore/ ./cache/... ./failurelog/ ./kit/redis/ ./remoteentity/   20 个包 ok
GOWORK=off go build ./... && go vet ./...                                                  ok
GOWORK=off go test -count=1 .                                                              ok（根包门禁）
GOWORK=off go test -race -count=1 ./kit/redis/ ./versionstore/ ./failurelog/ ./service/mail/ ./remoteentity/ ./kit/service/rank/ ./app/   ok
```

负对照：A2 方案列出的“修前修后都应通过”的边界用例（`TestReadsKeepTheDriverRetry`、`TestNotExecutedErrorsAreStillResent` 的写命令与 pipeline 部分、`TestResendsStopAtTheConfiguredBudget`、`TestAPipelineWithAnExecutedCommandIsNotResent`、`TestIsDefinitelyNotExecuted`、`TestWithTransactionFailuresBeforeTheCommitAreNotResultUnknown`）守住“没把读命令或确定未执行的写一起关掉”。

调用方核对（A2 方案第 3 项）：全仓 `rg` 排除测试、codegen 模板与生成 DAO、attribute、bus / syncbus、saga 协调器后，versionstore、kit 单实例锁、remoteentity versioned lock / marker / L2、cache、rank、mail、failurelog、DistLock 逐一核对，“没有发现把传输错误当成失败、然后再写一遍非幂等写的生产调用方”；mail `redisEnvelopes.Create` 的 SETNX 误报 `ErrConflict` 在本次被修好。

**7. 性能证据**：未测（写命令多一次 Pipeliner 组装 + 包装；没有基准）。

**8. 未验证项与已知风险**

- Cluster 下 `noReplay` 的 MOVED / ASK 跟随与换节点只按源码核对（E08）。
- 握手阶段失败（新建连接时 HELLO 上的 EOF）保守归为结果未知，不重发。
- `writeCalls` 表里名为 `set` 的条目实际调用的是 `SetNX`；`Client.Set`（`redis/driver/client.go:150`）与 pipeline 的 `Set` / `Del` / `HSet` / `Expire` / `ZAdd` / `LPop` 没有逐个进“回复丢失只执行一次”的表，靠同一 `write` / `queueWrite` 路径与 `TestAPipelineWithAnExecutedCommandIsNotResent` 间接覆盖（推断，未见专门用例）。
- bus `BeginConsume` 的 SETNX 去重在回复丢失时进死信（第十二轮决定保持，契约在 `bus/reliable.go` 的 `ReliableStore` 注释）。
- ③ versionstore 一次性写令牌留到下个大版本。

**9. review 检查点**

- [ ] grep 全部 Redis 写路径，确认没有走驱动自动重放：`grep -n 'c.rdb\.' redis/driver/client.go` 只应出现读命令（Get、MGet、Exists、TTL、HGet、HGetAll、HExists、LLen、LRange、ZScore、ZRank、ZRevRank、ZRange*、ZCard、SMembers、SIsMember、Ping）与 `Close`；写方法都经 `write(`。
- [ ] 确认 `redis/driver` 之外的生产代码不直接 import go-redis：`grep -rln 'redis/go-redis' --include='*.go' . | grep -v _test.go` 只应列出 `redis/driver/*.go`。
- [ ] 确认 `pipeline.go` 的每个写方法都走 `queueWrite`（`:59-112`），读方法（Get、HGet、HGetAll）不置 `hasWrites`；`Exec` 重发时重新入队的是 `Exec` 返回的（已包 `noReplay` 的）命令。
- [ ] 确认 `ErrCommitResultUnknown` 只在 commit 已发出后返回：`grep -n 'ErrCommitResultUnknown' mongo/driver/*.go` 只有 `mongo/driver/session.go:111` 一处包装，且位于 `s.commit(ctx)` 之后。
- [ ] 确认 `IsDefinitelyNotExecuted`（`redis/driver/replay.go:36`）没有把 EOF、`io.ErrUnexpectedEOF`、i/o timeout、`context.*` 归为“没执行”；对照 `TestIsDefinitelyNotExecuted` 的表。
- [ ] 确认 cache 两处补发 EXPIRE（`cache/redis_hash.go:92-96`、`cache/redis_raw.go:98-101`）返回的是写命令的错误而不是 EXPIRE 的错误。
- [ ] 确认 `kit/redis/singleton.go:74` 的单实例锁客户端仍是 `MaxRetries = -1`（`resendsFor(-1) == 0`，写与脚本都不重发）。

<a id="drv-4"></a>
### DRV-4 L2 墓碑写入后 WAIT 副本（O-M6-3，驱动能力 EvalReplicated）

> 首发 v1.23.0（本版） · [说明](guide-saga-drv-dao-rem.md#drv-4)

**1. 提交**

| 提交 | 版本 | 内容 |
| --- | --- | --- |
| `fa98f53f` | v1.23.0 | 记录第十轮决定（O-M6-1、O-M6-3） |
| `db67b8ee` | v1.23.0 | O-M6-3：`fredis.ReplicatedEvaler`、`driver.Client.EvalReplicated`；`DeleteAtVersion` 走 WAIT；`NewSnapshotL2StoreFromConfig`、`ValidateSnapshotL2TombstoneWait`；kit 严格读取两键、`app/config_validation.go` 登记；`scripts/mirror-local.sh test-core` 等。同提交含 O-M6-1（兴趣续租请求，属 REM 主题） |
| `d1d6d967` | v1.23.0 | DECISIONS-PENDING 第十轮标为已实施 |
| `fcc78ad0` | v1.23.0 | 收尾第 2 批 A8：生成配置写出 `snapshot_l2_tombstone_wait_replicas` / `_timeout`（1 / 50ms），`productionizeConfig` 只改独占一行的流 `replicas`，不再把墓碑 WAIT 的副本数一起改成 3 |
| `ba13cb05` | v1.23.0 | 发版前补充验证：Cluster 槽位迁移（ASK / MOVED）上的 L2 读写与墓碑 WAIT（`TestMirrorLocalClusterSlotMigrationSnapshotReadWriteAndTombstone`），见 [REM-12](impl-saga-drv-dao-rem.md#rem-12) |

**2. 改动文件与关键符号**

| `path:line` | 符号 | 职责 |
| --- | --- | --- |
| `redis/client.go:106` | `ReplicatedEvaler` | 可选能力接口 `EvalReplicated(ctx, script, keys, numReplicas, timeout, args...)` |
| `redis/client.go:111` | `ReplicatedEvalResult` | `Result`、`Waited`、`Replicas`、`WaitErr`、`Skipped` |
| `redis/client.go:125-132` | `ReplicatedSkip*` | `no_replicas` / `redirected` / `unsupported` / `disabled` |
| `redis/driver/replicated.go:25` | `Client.EvalReplicated` | 选节点（单机用客户端本身；Cluster `MasterForKey`），重发循环（`:52-63`） |
| `redis/driver/replicated.go:68` | `evalReplicatedOnConn` | 独占连接上 EVAL + ROLE 流水线；副本 > 0 才同连接 `WAIT`（`:98-99`） |
| `redis/driver/replicated.go:110` | `connectedReplicas` | 解析 ROLE，非 master 报错 |
| `redis/driver/replicated.go:129` | `isRedirect` | MOVED / ASK 前缀 |
| `remoteentity/snapshot_l2.go:176` | `NewSnapshotL2StoreFromConfig` | 按 Config 建 store 并校验 WAIT 设置 |
| `remoteentity/snapshot_l2.go:194` / `:198` | `MaxSnapshotL2TombstoneWaitTimeout` / `ValidateSnapshotL2TombstoneWait` | 上限 1s；副本数非负、要等时超时在 (0, 1s] |
| `remoteentity/snapshot_l2.go:324` | `remoteSnapshotL2Store.DeleteAtVersion` | 配置了副本数且客户端实现 `ReplicatedEvaler` 时走 `EvalReplicated`；返回值只由脚本决定 |
| `remoteentity/snapshot_l2.go:357` | `recordTombstoneWait` | 计数、指标标签、限频 Warn / 首次 Info |
| `remoteentity/config.go:21` / `:24` / `:88-89` | `Config.SnapshotL2TombstoneWaitReplicas` / `Timeout`；`DefaultConfig` | 零值关闭；缺省 1 / 50ms |
| `remoteentity/assemble.go:108` | `Assemble` | 改用 `NewSnapshotL2StoreFromConfig` |
| `kit/remoteentity/remote_entity_mod.go:328-347` | `readSnapshotConfig` | 两键严格读取与范围校验 |
| `kit/remoteentity/remote_mirror_mod.go:140` | `RemoteMirrorMod` | 同一构造 |
| `app/config_validation.go:288` / `:325` | `frameworkDurationKeys` / `frameworkIntKeys` | 登记两键（启动前类型校验） |

**3. 不变量与强制点**

- 不变量 1：`WAIT` 与写墓碑的脚本在同一条物理连接上，且只发往执行脚本的主节点。强制点：`evalReplicatedOnConn` 用 `node.Conn()` 独占连接（`redis/driver/replicated.go:69`），`WAIT` 经 `conn.Process`（`:99`）。
- 不变量 2：`WAIT` 从不重放；脚本只在流水线每条命令都确定没执行时整条重发。强制点：`:81` 只在脚本出错时计算 `retryable`；脚本成功后的任何 ROLE / WAIT 错误都返回 `retryable=false`。
- 不变量 3：`WAIT` 结果不改变 `DeleteAtVersion` 的返回值。强制点：`remoteentity/snapshot_l2.go:332-337` 只在 `EvalReplicated` 返回 err（脚本错误）时返回错误，`outcome` 只进 `recordTombstoneWait`。
- 不变量 4：配置非法时启动失败。强制点：kit `readSnapshotConfig`（`kit/remoteentity/remote_entity_mod.go:333-345`）与 core `ValidateSnapshotL2TombstoneWait`（`remoteentity/snapshot_l2.go:198`）。
- 守卫测试：`TestEvalReplicatedWaitsOnTheScriptsConnection`（`redis/driver/replicated_promises_test.go:104`）、`TestEvalReplicatedScriptErrorsAndDisabledWait`（`:147`）、`TestSnapshotL2TombstoneWaitIsObservedButNeverFailsTheDelete`（`remoteentity/snapshot_l2_tombstone_wait_promises_test.go:56`）、`TestSnapshotL2TombstoneWaitDisabledAndUnsupported`（`:102`）、`TestSnapshotL2TombstoneWaitSettingsAreValidated`（`:129`）、`TestTombstoneWaitConfiguration`（`kit/remoteentity/tombstone_wait_config_test.go:14`）。

**4. 控制流**

```mermaid
sequenceDiagram
    participant S as L2 store DeleteAtVersion
    participant D as driver.EvalReplicated
    participant P as 主节点（独占连接）
    S->>D: 墓碑脚本 + keys[0], replicas, timeout
    alt replicas<=0 或 timeout<=0
        D->>P: 普通 Eval（Skipped=disabled）
    else Cluster
        D->>D: MasterForKey(keys[0])，失败则普通发送（redirected）
    end
    D->>P: 流水线 EVAL + ROLE（noReplay）
    alt 脚本回 MOVED / ASK
        D->>P: 经集群客户端普通发送一次（Skipped=redirected）
    else 脚本错误且两条都确定没执行
        D->>D: 退避后换新连接整条重发
    else 脚本成功
        D->>D: ROLE 数副本
        alt 副本数 = 0
            D-->>S: Skipped=no_replicas
        else
            D->>P: WAIT n timeout（同连接，不重放）
            P-->>D: 确认数 或 错误
            D-->>S: Waited/Replicas 或 WaitErr
        end
    end
    S->>S: recordTombstoneWait（计数 / 指标 / 限频日志）
    S-->>S: 返回值只看脚本结果（"0" → ErrStaleWrite）
```

**5. 失败与不确定结果**

| 情形 | 处理 | 调用方（`DeleteAtVersion`）看到 |
| --- | --- | --- |
| 副本确认数 ≥ 配置 | `confirmed` 计数 | nil |
| `WAIT` 超时、确认不足 | `short` 计数 + 限频 Warn | nil |
| 主节点无副本 | 不发 WAIT，`no_replicas` 计数 + 首次 Info | nil |
| ROLE / WAIT 出错（EOF、超时） | `error` 计数 + 限频 Warn；复制结果未知 | nil（墓碑已在主上） |
| Cluster MOVED / ASK、取不到槽主、客户端不支持、未启用 | 普通发送，`skipped` 计数 | 脚本结果 |
| 脚本回复丢失 | 不重发 | 传输错误（结果未知；墓碑脚本幂等，可重发） |
| 脚本返回 0（L2 已有更新快照） | — | `cache.ErrStaleWrite` |

**6. 测试**

| 用例 | 文件 | 覆盖 |
| --- | --- | --- |
| `TestEvalReplicatedWaitsOnTheScriptsConnection` | `redis/driver/replicated_promises_test.go` | 假服务端按连接记录：WAIT 与 EVAL 同连接；没有副本不发 WAIT |
| `TestEvalReplicatedScriptErrorsAndDisabledWait` | 同上 | WAIT 出错只进 WaitErr；脚本错误不发 WAIT；关闭时只发 EVAL |
| `TestSnapshotL2TombstoneWaitIsObservedButNeverFailsTheDelete` | `remoteentity/snapshot_l2_tombstone_wait_promises_test.go` | 各结果计数；返回值只由脚本决定；快照写入不走 WAIT |
| `TestSnapshotL2TombstoneWaitDisabledAndUnsupported` | 同上 | 副本数 0 / 旧构造不调用新能力 |
| `TestSnapshotL2TombstoneWaitSettingsAreValidated` | 同上 | 设置校验 |
| `TestTombstoneWaitConfiguration` | `kit/remoteentity/tombstone_wait_config_test.go` | 两个 Mod 的严格读取 |
| `TestMirrorLocalTombstoneSurvivesAFailoverOnlyWithWait` | `remoteentity/snapshot_l2_tombstone_wait_integration_test.go:144` | 私有环境：复制滞后 + 立刻提升副本 |
| `TestMirrorLocalTombstoneWaitOnClusterGoesToTheKeysPrimary` | 同上 `:277` | Cluster：WAIT 只打该键主节点；副本暂停时按超时返回 `short` |

修前红文本（原样，出处 `docs/feature/MIRROR-M6-OBSERVATIONS-2026-10-06.md` §6.2；在基线 worktree 上用修前构造 `NewSnapshotL2StoreWithKeyPrefix`）：

```
--- FAIL: TestMirrorLocalTombstoneSurvivesAFailoverOnlyWithWait/pre_fix (0.03s)
    snapshot_l2_tombstone_red_integration_test.go:213: the tombstone must survive the failover: delete took 0s; fresh reader found=true version=1, tombstone=""
```

修后（原样，同一出处）：

```
MIRROR O-M6-3 without_wait: delete took 0s; after promoting the replica tombstone="" fresh reader found=true version=1; wait stats {Confirmed:0 ...}
MIRROR O-M6-3 with_wait: delete took 502ms; after promoting the replica tombstone="2" fresh reader found=false version=0; wait stats {Confirmed:1 ...}
MIRROR O-M6-3 cluster: stopped replica, delete took 241ms, wait stats {Confirmed:1 Short:1 NoReplicas:0 Failed:0 Skipped:0}
--- PASS: TestMirrorLocalTombstoneSurvivesAFailoverOnlyWithWait (9.56s)
--- PASS: TestMirrorLocalTombstoneWaitOnClusterGoesToTheKeysPrimary (0.43s)
```

负对照：`without_wait` 子用例保留为边界记录（不 WAIT 时现象可确定复现）。Cluster：`INFO commandstats` 的 `cmdstat_wait` 只在该键所在主节点 +1。其他验证（同一出处 §6.4）：`go test -race -count=3 ./remoteentity ./redis/... ./kit/remoteentity ./sync/syncbus/...`、根包、`./codegen/... ./kit/...` 通过；`scripts/mirror-local.sh test-core` 两次通过、基线上修前红一次。

**7. 性能证据**：删除路径最多多等 `timeout`（缺省 50ms）；私有环境 `with_wait` 子用例因人为 500ms 复制延迟耗时 502ms，Cluster 副本暂停时 241ms（200ms 超时）。普通快照写入不变。无吞吐基准。

**8. 未验证项与已知风险**

- 副本在确认前断开时 WAIT 挡不住（S4a′“未复制即切主”），计 `short` / `no_replicas`；真实多机复制延迟见 E10。
- 记录写 `WAIT` 超时要低于 Redis 客户端读超时 3s，否则客户端先超时计为 error；上限 1s 满足，但若运维把 `ReadTimeout` 调到 1s 以下（kit 不暴露该项）会出现此情形（推断）。
- 指标标签：MIRROR-M6 方案 §3 表把未执行 WAIT 的情形写成 `redirected` / `unsupported`，源码标签统一为 `skipped`（`remoteentity/snapshot_l2.go:373-375`；T-277 与发版前验证记录写的也是 `skipped`）——以源码为准。
- `kit` 读取：只设 `_timeout` 不设 `_replicas` 时，timeout 仍按 (0, 1s] 校验（即使副本数为 0）；core 校验只在 replicas > 0 时要求 timeout 合法——两层口径略有差别（源码观察，未见文档说明）。

**9. review 检查点**

- [ ] 确认 `evalReplicatedOnConn`（`redis/driver/replicated.go:68`）里 EVAL、ROLE、WAIT 用的是同一个 `conn`，且三条都包了 `noReplay`。
- [ ] 确认脚本成功后任何 ROLE / WAIT 错误都只进 `WaitErr`、返回 `retryable=false`（`:84-103`），外层循环（`:57`）不会因此重发脚本。
- [ ] 确认 Cluster 分支 `MasterForKey` 失败与脚本 MOVED / ASK 都退回 `plain(...)`（普通 `Eval`，仍经 `runScript` 不重放），且不发 WAIT。
- [ ] 确认 `DeleteAtVersion`（`remoteentity/snapshot_l2.go:324`）的返回值与修前一致（只由脚本结果决定），`recordTombstoneWait` 不返回错误。
- [ ] 确认 `app/config_validation.go:288`、`:325` 登记了两个键，`kit/remoteentity/remote_entity_mod.go:328-347` 用 `read.Int` / `read.Duration`（严格读取）。
- [ ] 确认只有墓碑删除走 WAIT：`grep -n 'EvalReplicated' --include='*.go' -r . | grep -v _test.go` 只应有驱动实现与 `remoteentity/snapshot_l2.go:332`。

<a id="drv-5"></a>
### DRV-5 驱动与 Mod 的 Close 统一口径（第十二轮“驱动 Close 契约” + RR-20261006-10）

> 首发 v1.23.0（本版） · [说明](guide-saga-drv-dao-rem.md#drv-5)

**1. 提交**

| 提交 | 版本 | 内容 |
| --- | --- | --- |
| `78e26853` | v1.23.0 | 记录第十二轮决定（含“驱动 Close 契约写进 A2 驱动契约表”） |
| `88f33776` | v1.23.0 | 收尾第 1 批：按当时源码实测写 redis / mongo README §5（单机与 Cluster 不一致等），登记 WANTED W-2026-10-06-02，代码未改 |
| `8059b877` | v1.23.0 | DECISIONS-PENDING 收尾第 1 批与第十二轮文档类决定标为已实施 |
| `d05a04a1` | v1.23.0 | RR-20261006-10：redis / mongo / etcd / nats 驱动与 kit Redis / Mongo / Nats Mod、单实例锁 store 的 Close 统一；新增 `internal/operation.Serial`；两份 README §5 改成统一后的表。同提交的文档链接门禁属 TOOL |
| `02c8a10d` | v1.23.0 | roost-coding 加 `operation.Serial` 与 Close 统一口径 |

前史（不属本条、只作背景）：NC-233（`2c1c7be7`，v1.21.0）`RedisMod` 第一次 Close 就交出连接；NC-260（`36220f34`，v1.21.0）mongo `Client.Close` 把 `ErrClientDisconnected` 当已关闭；NC-173 复核补修（`50f2ac2a`，v1.20.2）。

**2. 改动文件与关键符号**

| `path:line` | 符号 | 职责 |
| --- | --- | --- |
| `internal/operation/serial.go:14` / `:20` / `:39` | `Serial` / `Lock(ctx)` / `Unlock` | 容量 1 的 channel 做停止入口串行器；后到者等待受自己 ctx 约束 |
| `redis/driver/client.go:25` / `:483` | `Client.closeOnce` / `Client.Close` | `sync.Once` 只关一次，错误只报给第一次；单机与 Cluster 一致 |
| `redis/driver/assembly.go:44` | `Assembly.Close` | 转发 `Client.Close` |
| `redis/driver/pubsub.go:50` | `pubSub.Close` | `sync.Once`（修前已是） |
| `redis/driver/lock.go:113` / `:146` / `:174` | `distLock` Acquire / Release / Extend 的 `ErrClosed` 分支 | 直接返回、锁状态不变，不记成未知 |
| `mongo/driver/client.go:27-30` / `:119` | `closeSerial` / `closed` / `disconnect`；`Client.Close` | 串行化；断开成功（或驱动报 `ErrClientDisconnected`）才置 `closed`；FLE 断开失败不置位、下次重做 |
| `etcd/driver/client.go:23` / `:212` / `:224` | `closed`；`Client.Close`；`checkOpen` | Close 开始即置位；Get / Put / Txn / Grant / KeepAlive / Revoke 等先查，返回 `fetcd.ErrClosed` |
| `nats/driver/assembly.go:35-36` / `:78` | `closeSerial` / `closed`；`Assembly.Close` | 串行；连接关闭后置位，之后返回 nil；`ErrClosedUndrained` 只报一次 |
| `nats/driver/client.go:53` | `Client.Publish` | 连接已关 / 正在排空时立即返回，可 `errors.Is` 到 `fnats.ErrClosed` 与 gonats 原错误 |
| `kit/redis/singleton.go:95` / `:145` | `singletonStore.closeOnce` / `Close` | 错误只报给第一次 |
| `kit/redis/redis_mod.go:23` / `:29` / `:131` | `mu` / `assembly()` / `StopWithContext` | `mu` 保护 `asm`，Stop 持锁串行（go-redis Close 不等在途命令，持锁短） |
| `kit/mongo/mongo_mod.go:25-26` / `:128` / `:181` | `stopSerial` / `mu`；`StopWithContext`；`Client()` | Stop 用 `Serial` 串行（会阻塞到 ctx），`mu` 保护 `client` |
| `kit/nats/nats_mod.go:32` / `:176` | `stopSerial`；`StopWithContext` | 同上 |
| `redis/driver/README.md` §5、`mongo/driver/README.md` §5 | 契约表 | 统一后的口径 |
| `docs/agent-skills/roost-coding/SKILL.md:73` | A3 停机条目追加 | 停止入口用 `operation.Serial`；Close 统一口径；唯一例外 nats `ErrClosedUndrained` |

**3. 不变量与强制点**

- 不变量 1：重复 Close 返回 nil，第一次错误只报一次。强制点：redis `closeOnce`、单实例锁 `closeOnce`、etcd `Close` 的一次性逻辑、mongo / nats 的 `closed` 标记。
- 不变量 2：并发 Close 后到者等第一个做完；会阻塞的在自己的 ctx 内等。强制点：`sync.Once`（redis、pubsub、单实例锁、etcd）、`operation.Serial`（mongo、nats、MongoMod、NatsMod）、`sync.Mutex`（RedisMod，持锁短）。
- 不变量 3：Close 之后的调用返回可 `errors.Is` 的已关闭错误。强制点：go-redis 自身（`ErrClosed`）、mongo 驱动（`ErrClientDisconnected`）、etcd `checkOpen`、nats `Publish` 的前置检查。
- 不变量 4：DistLock 在 `ErrClosed` 上不改变锁状态。强制点：`redis/driver/lock.go:113`、`:146`、`:174`。
- 守卫测试：各包 `close_contract_promises_test.go`（下表）与 `internal/operation/serial_test.go`。无全仓“每个 Close 都符合口径”的静态守卫，靠 roost-coding 规则与各包用例。

**4. 控制流 / 状态机**

`operation.Serial` 的并发 Close（mongo / nats / Mod）：

```mermaid
stateDiagram-v2
    [*] --> Open
    Open --> Closing: 第一个 Close 取得 Serial
    Closing --> Closed: 断开 / 排空成功，置 closed
    Closing --> Open: 断开失败（mongo FLE）或 ctx 到期，状态不变，可再调用
    Closed --> Closed: 之后的 Close 返回 nil
    note right of Closing
        后到者 Lock(ctx) 等待
        自己的 ctx 先结束 → 返回 ctx 错误
        取得后看 closed：已关 → nil
    end note
```

redis / etcd / 单实例锁用 `sync.Once`：第一个调用者执行关闭并拿到错误，并发后到者在 `Once.Do` 内等它完成后拿到 nil。

**5. 失败与不确定结果**

| 情形 | 处理 | 调用方看到 |
| --- | --- | --- |
| redis 第一次 Close 出错 | go-redis 遇错照样关完全部池 | 第一次：错误；之后：nil |
| mongo 第一次 Close ctx 已过期 | 驱动断开各 server 时忽略 ctx 错误，照样释放 | nil |
| mongo FLE 客户端断开失败 | 不置 `closed` | 错误；再调用重做断开（本项目不配 FLE） |
| nats 排空失败硬关 | 置 `closed` | 第一次：`ErrClosedUndrained`；之后 nil |
| 并发后到者自己的 ctx 先结束（mongo / nats / Mod） | 不取得串行器 | ctx 错误；对象状态不变，可再调 |
| Close 之后的 redis 命令 | `IsDefinitelyNotExecuted` 为真但 `shouldResend` 为假 | `goredis.ErrClosed`，不重发 |
| Close 之后 DistLock.Acquire | 锁状态不变 | 每次 `goredis.ErrClosed`（不再变成 `ErrDistLockStateUncertain`） |
| Close 之后 etcd Get / Put … | `checkOpen` | 立即 `fetcd.ErrClosed`；Watch 立即结束（`fetcd.ErrWatchClosed`） |
| 与 Close 并发的 etcd 在途调用 | 不打断 | 仍受调用方 ctx 约束（停机顺序由调用方负责） |

**6. 测试**

| 用例 | 文件 | 覆盖 |
| --- | --- | --- |
| `TestClientRepeatedCloseReturnsNilOnEveryDeployment` | `redis/driver/close_contract_promises_test.go:46` | 单机 / Cluster 第二次 Close 为 nil；Close 后 Set 返回 `ErrClosed` |
| `TestClientCloseErrorIsReportedOnce` | 同上 `:69` | 首次错误只报一次 |
| `TestClientConcurrentCloseAllReturnNil` | 同上 `:84` | 并发 Close 全部 nil |
| `TestAssemblyRepeatedCloseReturnsNil` | 同上 `:104` | Assembly 转发 |
| `TestDistLockAfterClientCloseKeepsReportingErrClosed` | 同上 `:120` | 锁不记成未知 |
| `TestClientConcurrentCloseWaitsForTheFirstDisconnect` | `mongo/driver/close_contract_promises_test.go:21` | 后到者等第一次断开 |
| `TestClientCloseRetriesAfterAFailedDisconnect` | 同上 `:78` | 控制（修前已成立） |
| `TestClientCloseErrorIsReportedOnceThenNil` / `TestClientConcurrentCloseAllReturnNil` / `TestClientCallsAfterCloseFailFastWithErrClosed` | `etcd/driver/close_contract_promises_test.go:32` / `:46` / `:62` | 首次错误只报一次；并发（控制）；关闭后快速失败 |
| `TestAssemblyTerminalCloseErrorIsReportedOnce` / `TestAssemblyConcurrentCloseReportsTheTerminalErrorOnce` / `TestClientPublishAfterCloseReportsErrClosed` | `nats/driver/close_contract_promises_test.go:36` / `:49` / `:75` | 终态错误只报一次；并发只报一次；Publish 关闭后 `fnats.ErrClosed` |
| `TestSingletonStoreCloseErrorIsReportedOnce` / `TestRedisModConcurrentStopIsSafe` | `kit/redis/close_contract_promises_test.go:19` / `:36` | 单实例锁 store；RedisMod 并发 Stop（`-race`） |
| `TestMongoModConcurrentStopIsSafe` | `kit/mongo/close_contract_promises_test.go:16` | `-race` |
| `TestNatsModConcurrentStopIsSafe` | `kit/nats/close_contract_promises_test.go:15` | `-race` |
| `TestSerialLaterCallerWaitsWithinItsOwnContext` | `internal/operation/serial_test.go:11` | 后到者在自己的 ctx 内等 |

修前红文本（原样，出处 `docs/bug/RR-20261006-10.md`“复现”）：

```
--- FAIL: TestClientRepeatedCloseReturnsNilOnEveryDeployment/single
    close_contract_promises_test.go:54: Close #2 = redis: client is closed, want nil: repeated Close is idempotent on every deployment
--- FAIL: TestClientConcurrentCloseAllReturnNil/single
    close_contract_promises_test.go:97: round 0: concurrent Close #1 = redis: client is closed, want nil (all = [<nil> redis: client is closed redis: client is closed redis: client is closed])
--- FAIL: TestAssemblyRepeatedCloseReturnsNil/single
    close_contract_promises_test.go:114: second Assembly.Close = redis: client is closed, want nil
--- FAIL: TestDistLockAfterClientCloseKeepsReportingErrClosed/single（cluster 同）
    close_contract_promises_test.go:135: Acquire #2 after Close = (false, redis: distributed lock ownership is uncertain; call Release to reconcile), want (false, goredis.ErrClosed): the SetNX was never sent

--- FAIL: TestClientConcurrentCloseWaitsForTheFirstDisconnect            (mongo/driver)
    close_contract_promises_test.go:56: concurrent Close returned <nil> while the first Close was still disconnecting; want it to wait

--- FAIL: TestClientCloseErrorIsReportedOnceThenNil                       (etcd/driver)
    close_contract_promises_test.go:41: Close #2 = context canceled, want nil: the first error is reported once
--- FAIL: TestClientCallsAfterCloseFailFastWithErrClosed (18.01s)
    close_contract_promises_test.go:94: Get after Close still blocked after 2s, want an immediate fetcd.ErrClosed
    （GetWithPrefix / Put / PutWithLease / Delete / DeleteWithPrefix / Txn / Grant / Revoke 同）
    close_contract_promises_test.go:91: KeepAlive after Close = etcdclient: leases keep alive halted, want fetcd.ErrClosed

--- FAIL: TestAssemblyTerminalCloseErrorIsReportedOnce                    (nats/driver)
    close_contract_promises_test.go:44: Close #2 = nats: connection closed before drain finished: nats: connection closed, want nil: the connection is already closed, the terminal error is reported once
--- FAIL: TestAssemblyConcurrentCloseReportsTheTerminalErrorOnce
    close_contract_promises_test.go:70: round 0: 4 concurrent Close calls reported the terminal error, want exactly 1
--- FAIL: TestClientPublishAfterCloseReportsErrClosed (0.06s)
    close_contract_promises_test.go:79: Publish after Close = nats: publish to roost.close failed after 3 retries: nats: connection closed, want fnats.ErrClosed

--- FAIL: TestSingletonStoreCloseErrorIsReportedOnce                      (kit/redis)
    close_contract_promises_test.go:31: Close #2 = redis: client is closed, want nil: the first error is reported once
--- FAIL: TestRedisModConcurrentStopIsSafe / TestMongoModConcurrentStopIsSafe / TestNatsModConcurrentStopIsSafe
    WARNING: DATA RACE（redis_mod.go:116/120、mongo_mod.go:117/129、nats_mod.go:183/197）
    testing.go:1865: race detected during execution of test
```

（红文本里的 `kit/redis/redis_mod.go:116/120` 等是基线 `ba13cb05` 的行号。）修后（`docs/bugfix/RR-20261006-10.md`“验证”）：各包 `close_contract_promises_test.go` 全部通过；`go test -race -count=3 ./internal/operation ./redis/driver ./mongo/driver ./etcd/driver ./nats/driver ./kit/redis ./kit/mongo ./kit/nats` 通过；私有 redis-server 上 `-tags integration -race -count=3 -p 1 ./redis/driver ./kit/redis` 通过；根包、build / vet 通过。负对照：mongo“重复 Close 返回 nil”“断开失败后重试”与 etcd 并发 Close 修前已成立，作为控制用例。`kit/redis/stop_retry_promises_test.go` 的 NC-233 用例原来靠“再调一次驱动 Close 得到 ErrClosed”制造 Close 错误，驱动幂等后改为先关底层 `Raw()` 连接池。

**7. 性能证据**：无（不涉及热路径）。

**8. 未验证项与已知风险**

- 用不可达地址与私有 redis-server 验证；未用真实 Mongo / etcd / NATS；三个 toxiproxy 用例、`TestSingletonStoreGetAcrossClusterSlots`（需 Cluster）未跑。
- etcd 与 Close 并发的在途调用不被打断；`Discovery` / 选举经 `Raw()` 直接用 clientv3，不在此列。
- 没有静态守卫约束新写的 Close；新驱动 / Mod 要按 roost-coding 规则自觉套用。
- 文档状态：`docs/review/DECISIONS-PENDING-2026-10-05.md` 文首“当前总状态”仍把 W-2026-10-06-02 列为“待 review 判断”，第十二轮表“驱动 Close 契约”行仍写“代码未改”；`docs/bug/WANTED.md` 已标“已转 RR-20261006-10，已修复”——以源码与 WANTED 为准。

**9. review 检查点**

- [ ] 确认 `operation.Serial.Lock`（`internal/operation/serial.go:20`）在 ctx 已结束但槽位空闲时仍能取得（先走非阻塞分支），以及每条成功路径都恰好 `Unlock` 一次（`mongo/driver/client.go:126`、`kit/mongo/mongo_mod.go:138`、nats Assembly / NatsMod）。
- [ ] 确认 mongo `Client.Close` 只在断开成功或 `ErrClientDisconnected` 时置 `closed`（`mongo/driver/client.go:130-133`），FLE 失败可重试。
- [ ] 确认 etcd 所有对外数据方法都先调 `checkOpen`（`etcd/driver/client.go:224`）：逐个核对 Get / GetPrefixSnapshot / Put / PutWithLease / Delete / DeleteWithPrefix / Txn / Grant / KeepAlive / Revoke。
- [ ] 确认 DistLock 三处 `ErrClosed` 分支（`redis/driver/lock.go:113`、`:146`、`:174`）都在修改 `l.state` 之前返回。
- [ ] 确认 `RedisMod.StopWithContext`（`kit/redis/redis_mod.go:131`）持 `mu` 期间只做 go-redis Close（不等在途命令），不会长时间阻塞健康检查的 `assembly()`。
- [ ] grep 全仓其他 Close / StopWithContext，看是否还有“每次报错”或“粘滞”口径：`grep -rn 'func (.*) Close() error\|func (.*) Close(ctx' --include='*.go' . | grep -v _test.go`，对照 RR-20261006-10 修复记录里列为本来就幂等的对象（`serviceWatcher` / `watcher` / `localMirror` / `mirrorSubscription`）与保留例外（etcd 内部 `campaignSession.Close`）。

<a id="drv-6"></a>
### DRV-6 启动建索引遇 Mongo 选举有界重试（O-M6-5）

> 首发 v1.23.0（本版） · [说明](guide-saga-drv-dao-rem.md#drv-6)

**1. 提交**

| 提交 | 版本 | 内容 |
| --- | --- | --- |
| `78e26853` | v1.23.0 | 记录第十二轮决定 |
| `7b73aabc` | v1.23.0 | 第十二轮 kit 批（九项之一 O-M6-5）：`mongo/driver/collection.go` 的 `retryDuringElection`；单元与私有副本集回归。同提交其余八项（O-S5-2、metrics 按标签删除、readyz 期限、Ops Bearer、CAS 口径、RR-20261006-09、高水位计数、回滚面板）属 OPS / APP / SAGA 等主题 |
| `d6a677e0` | v1.23.0 | DECISIONS-PENDING 第十二轮 kit 批标为已实施 |

**2. 改动文件与关键符号**

| `path:line` | 符号 | 职责 |
| --- | --- | --- |
| `mongo/driver/collection.go:247` | `collection.EnsureIndexes` | 每个索引经 `retryDuringElection(ctx, startupElectionRetry, …)`（`:252`） |
| `mongo/driver/collection.go:268` / `:275` | `electionRetry` / `startupElectionRetry` | `{attempts: 10, interval: 1s}` |
| `mongo/driver/collection.go:282` | `electionRetryMetric` | `mongo.ensure_index.election_retries.total` |
| `mongo/driver/collection.go:284` | `electionErrorCodes` | 11602、10107、13435、13436、189、91、11600 |
| `mongo/driver/collection.go:286` | `isElectionError` | `errors.As(err, &mongo.ServerError)` + `HasErrorCode` |
| `mongo/driver/collection.go:299` | `retryDuringElection` | 计数、用完 / ctx 到期时点名选举，原错误 `%w` 在链里 |

调用方（全部启动 DDL）：`dataengine/engine/mongo_store.go:104/109/114`、`remoteentity/mongo_committer.go:228`、`saga/mongo_store.go:74/83/90/93`、`saga/command_consumer.go:88`、`saga/step_operation_inbox.go:113`、`nestwal/effect_inbox.go:73`。

**3. 不变量与强制点**

- 不变量：只对换主错误码重试，其他错误立即原样返回；每个索引至多 10 次尝试；ctx 先到期以 ctx 为准。强制点：`retryDuringElection` 是 `EnsureIndexes` 唯一调用的重试入口。
- 守卫测试：`TestIndexCreationRetriesThroughAnElectionWithinBounds`（`mongo/driver/election_retry_promises_test.go:17`：选举结束后成功并计 2 次重试；不结束时恰好 attempts 次后失败并点名、原错误在链里；ctx 先到期点名期限；非选举错误不重试）；`TestMirrorLocalOwnerStorageInitSurvivesAMongoElection`（`remoteentity/owner_startup_election_integration_test.go:24`，私有副本集）。

**4. 控制流**

```mermaid
flowchart TD
    A[EnsureIndexes 遍历索引] --> B["ensureIndex CreateOne<br/>冲突且允许时 Drop + Create"]
    B -- nil --> N[下一个索引]
    B -- 非选举错误 --> R1[立即返回原错误]
    B -- 选举错误码 --> C[计 election_retries.total]
    C --> D{"attempt >= 10"}
    D -- 是 --> R2["did not settle after 10 attempts 1s apart: 最后错误"]
    D -- 否 --> E{等 1s 期间 ctx 结束}
    E -- 是 --> R3["did not settle before the deadline attempt n of 10: ctx 错误; last error"]
    E -- 否 --> B
```

**5. 失败与不确定结果**

| 情形 | 处理 | 调用方看到 |
| --- | --- | --- |
| 选举在 10 次内结束 | 重试成功 | nil；启动慢若干秒 |
| 选举 10 次仍未结束 | 放弃 | 点名选举的错误，启动失败 |
| 启动期限先到 | 放弃 | 点名期限的错误（`errors.Is` ctx 错误与原错误都成立） |
| 非选举错误（索引定义冲突、权限…） | 不重试 | 原错误 |
| 重试期间某次 CreateOne 其实已在旧主执行 | 再建同一索引幂等 | 无影响 |

**6. 测试**

| 用例 | 文件 | 覆盖 |
| --- | --- | --- |
| `TestIndexCreationRetriesThroughAnElectionWithinBounds` | `mongo/driver/election_retry_promises_test.go` | 四种情形（见上） |
| `TestMirrorLocalOwnerStorageInitSurvivesAMongoElection` | `remoteentity/owner_startup_election_integration_test.go` | 私有副本集 `fault mongo-stepdown` 期间连续执行 owner 存储初始化 |

修前红文本（原样，出处 `docs/feature/DECISIONS-R12-KIT-2026-10-06.md` §7；把上界临时改成 1 次，等同旧行为，未提交）：

```
owner_startup_election_integration_test.go:86: owner storage init #9 failed during the election: remote_entity: ensure transaction indexes: mongo: replica set primary election did not settle after 1 attempts 1s apart: write exception: write concern error: (InterruptedDueToReplStateChange) operation was interrupted
```

修后（原样，同一出处；同一用例连跑 6 轮）：

```
owner_startup_election_integration_test.go:104: stepDown: mongo-2; 20 owner storage inits all succeeded; slowest #4 took 1.120542541s; election retries 1
--- PASS: TestMirrorLocalOwnerStorageInitSurvivesAMongoElection (12.47s)
…（round 2～5 同形）
owner_startup_election_integration_test.go:104: stepDown: mongo-3; 20 owner storage inits all succeeded; slowest #4 took 1.126167625s; election retries 1
--- PASS: TestMirrorLocalOwnerStorageInitSurvivesAMongoElection (12.50s)
```

复跑入口（同一出处）：`ROOST_MIRROR_LOCAL_HOME=<私有目录> ROOST_MIRROR_LOCAL_OFFSET=<非 0 / 1000> ROOST_MIRROR_LOCAL_CORE_RUN='^TestMirrorLocalOwnerStorageInitSurvivesAMongoElection$' scripts/mirror-local.sh test-core`。批次验证：`go test -race -count=3 … mongo/driver …` ok、根包 ok、glsvet 退出 0。

**7. 性能证据**：无（启动路径；修后最慢一次初始化约 1.1s，即一次 1s 间隔的重试）。

**8. 未验证项与已知风险**

- 多机副本集与 mongos：E11。
- 次数按“每个索引”计：`EnsureIndexes` 带 N 个索引时，选举恰好跨越多个索引的最坏总时长可到约 N × 10s；决定表与记录写“10 次 × 1s”（字面上是每个索引的上界），总时长由调用方启动 ctx（缺省 30s）兜底（源码观察）。
- 错误码表随驱动 / 服务端版本可能变化，升级时需核对。

**9. review 检查点**

- [ ] 确认 `EnsureIndexes` 只在启动路径调用：逐个看上面列出的 10 处调用点所在函数（`EnsureInfrastructure`、`EnsureRemoteStorage`、saga store / inbox 初始化），没有业务请求路径调用它。
- [ ] 确认 `isElectionError`（`mongo/driver/collection.go:286`）对写关注错误里的错误码也生效（`mongo.ServerError.HasErrorCode` 覆盖 `WriteException.WriteConcernError`）——修前红文本就是写关注错误。
- [ ] 确认 `ensureIndex` 的“冲突时 Drop 再 Create”（`AutoRecreate` 策略）在 Drop 之后遇到选举时，重试从 `CreateOne` 重新开始是安全的（索引已被 Drop，CreateOne 会重建）。
- [ ] 确认两条放弃错误都用 `%w` 保留原错误（`mongo/driver/collection.go:307-308`、`:314-315`），T-282 的日志文字与之一致。

## DAO：回滚统一走 DAO

<a id="dao-1"></a>
### DAO-1 回滚统一走 DAO（A1）

> 首发 v1.20.2 · [说明](guide-saga-drv-dao-rem.md#dao-1)

**1. 提交**

| 提交 | 版本 | 内容 |
| --- | --- | --- |
| `3d4fe9f3` | v1.20.1 | 背景：NC-60～63（含 NC-61 属性层随事务回滚，组件 `captureRollback` 做法） |
| `197f7bb9` | v1.20.1 | 背景：NC-64/65（NC-65 玩家加载时重建 Gear 与 `attr_final`） |
| `23a10f42` | v1.20.1 | 背景：NC-140～147（NC-140 World 定时器堆随事务回滚，组件 `captureRollback` 做法） |
| `d346b45c` | v1.20.2 | 记录维护者第二轮决定（A1 不采用推荐） |
| `5407f127` | v1.20.2 | A1 实施：codegen `mutableFields`；demo 属性 / 定时器组件无状态；`CombatDao.beginChange` / `markChanged`；glsvet A1 提示；roost-coding、codegen README、生成工程文档、组件骨架注释；先红后绿证据与基准 |
| `aa415fa6` | v1.20.2 | DECISIONS-PENDING A1 标为已实施 |
| `62cec54e` | v1.21.0 | B4：roost-coding A1 条加 skill Runtime 例外，glsvet 守卫（见 [DAO-2](#dao-2)） |
| `88f33776` | v1.23.0 | 收尾第 1 批 A16：CombatComponent 注释按源码更正（事务里 DAO 加入字段、事务外 panic、`ProjectAttributes` 例外） |

**2. 改动文件与关键符号**

| `path:line` | 符号 | 职责 |
| --- | --- | --- |
| `codegen/internal/dao/gen.go:299` / `:512` | 模板函数 `mutableFields` / `filterMutable` | 所有非 `dao:"-"` 字段都生成 mutator（原 `dirtyFields` / `filterDirty` 只收 persist 或 sync） |
| `codegen/internal/dao/template_dao.go:222` | `{{range mutableFields .Dao.Fields}}` | mutator 生成范围 |
| `codegen/internal/dao/template_dao.go:226`、`:261`、`:332`、`:361` | 生成 setter 里的 `nest.CurrentRollbackTx()` + `RollbackUndo` | DAO 自己“记逆操作 → 改 → 标脏”；`nopersist,nosync` 的 mark 为空 |
| `demo/db/def/player.go.tmpl:40` / `:47` / `:55` | `AttrBase` / `AttrFinal` / `AttrGear`（新） | `persist,sync` / `nopersist,sync` / `nopersist,nosync` |
| `demo/db/def/world.go.tmpl:22` / `:25` / `:33` | `Timers` / `TimerSeed` / `TimerNextDue`（新） | `persist` / `persist` / `nopersist,nosync` |
| `demo/game/entities/player/attribute_component.go.tmpl:89` | `AttributeComponent.OnInitFinish` | 加载时 `derive()`，只写 nopersist 字段 |
| `…/attribute_component.go.tmpl:97` / `:108` | `Container()` / `Final()` | 每次从 DAO 构造副本 |
| `…/attribute_component.go.tmpl:120` / `:137` | `LevelUp` / `RefreshGear` | 改源字段并在同一事务 `recompose()` |
| `…/attribute_component.go.tmpl:149` / `:155` / `:194` | `derive` / `writeGear` / `recompose` | 唯一写派生层的函数 |
| `demo/game/entities/world/timer_component.go.tmpl:113` | `TimerComponent.OnInitFinish` | 从 DAO 节点算 `timer_next_due` |
| `…/timer_component.go.tmpl:130` / `:153` / `:165` | `scheduler` / `settle` / `persist` | 一次调用内从 DAO 建调度器；变更钩子写穿 DAO；写回最早到期 |
| `…/timer_component.go.tmpl:233` | `Tick` | `timer_next_due` 为 0 或未到直接返回（不建堆） |
| `skill/combatcomponent/component.go:334` / `:374` | `CombatDao.beginChange` / `markChanged` | 按字段掩码登记逆操作（`nest.RecordUndo(dao, Field…)`）/ 标持久与同步脏 |
| `cmd/glsvet/main.go:672-676` / `:691` | `componentUndoCalls` / `componentUndoHints` | 组件方法里的 `RecordUndo` / `RecordUndoToken` / `DeferRollback` 打 `hint:` |
| `cmd/glsvet/main.go:162` | `vetDirectory` | 打印提示、不计 findings |
| `codegen/internal/roost/add_entity.go:325` | 组件骨架注释 | 新规范 |
| `docs/agent-skills/roost-coding/SKILL.md:39` | 执行契约“回滚统一走 DAO” | 规则正文（含 B4 例外） |

Nest 侧（未改，作为机制背景）：`nest/rollback.go:163` / `:170` `RollbackTx.RecordUndo` / `RecordUndoToken`（同一 owner + field + token 只记第一条逆操作，`:187-191`）；`:198` 包级 `RecordUndo`（非 `RollbackUndo` 策略返回 false）；`:370` `RollbackTx.Rollback`（逆序执行）；`:980` `captureDao`（state 策略下经 `RollbackSnapshotter` 快照）；`nest/execution.go:277` `rejectCommit`（明确拒绝 → Rollback）。

**3. 不变量与强制点**

- 不变量 1：组件不持有事务会改的内存状态；事务会改的状态都在 DAO，回滚由 Nest 的 DAO 回滚（undo 逆操作或 state 快照）完成。强制点：没有硬门禁（规范 + 生成形状）；glsvet A1 提示只覆盖“组件方法里直接调 undo”这一种形状。
- 不变量 2：`nopersist,nosync` 字段参与回滚，不进提交记录 / WAL / Mongo / 同步。强制点：生成 DAO 的 mark 函数为空；`persistFields` / `syncFields` 不含它。
- 不变量 3：派生值只由唯一的 derive 写；回滚不触发重算。强制点：模板结构（`derive` / `settle` / `deriveProjection` 各一处）。
- 守卫测试：`TestTransientFieldsHaveMutatorsAndStayOutOfStorageAndSync`（`codegen/internal/dao/transient_field_promises_test.go:14`）；daoruntime `TestATransientFieldRollsBackWithTheTransaction` / `TestATransientFieldNeverReachesTheCommitRecordOrSync` / `TestATransientFieldIsInTheStateSnapshot`（`codegen/internal/dao/testdata/runtime/transient_test.go:21` / `:39` / `:94`）；`TestComponentRecordingItsOwnUndoIsHinted`（`cmd/glsvet/main_test.go:166`）；demo 模板 `TestAttributeRollbackIsTheDaoRollback`（`demo/game/entities/player/attribute_component_test.go.tmpl:340`）、`TestNonPersistentAttributeLayersStayOutOfTheWAL`（`:430`）、`TestTimerRollbackIsTheDaoRollback`（`demo/game/entities/world/timer_component_test.go.tmpl:319`）、`TestTimerBookkeepingStaysOutOfTheCommitRecord`（`:419`）；`TestCombatRollbackIsTheDaoRollback`（`skill/combatcomponent/dao_rollback_promises_test.go:29`）。

**4. 控制流**

```mermaid
sequenceDiagram
    participant H as handler（Nest 事务内）
    participant C as 组件（无状态）
    participant D as DAO（生成 setter / CombatDao）
    participant T as RollbackTx
    participant K as committer（WAL / Remote）
    Note over T: 事务开始：state 策略下 captureDao 对每个 DAO 拍快照
    H->>C: LevelUp / RefreshGear / Arm / ApplyBuff …
    C->>D: 改源字段（SetAttrBase、SetTimers…）
    D->>T: undo 策略：RecordUndo(dao, field[, key]) 每键首次一条
    D->>D: 改值；mark…Dirty（nopersist,nosync 为空）
    C->>D: derive：写派生字段（attr_gear / attr_final / timer_next_due / vitals 投影）
    D->>T: 同样登记逆操作（同一笔事务）
    alt handler 返回 error
        H-->>T: 失败
        T->>D: Rollback：逆序执行逆操作 / 恢复快照（源字段与派生字段一起）
    else 提交被明确拒绝（rejectCommit）
        T->>K: prepare / admit
        K-->>T: 拒绝
        T->>D: Rollback（同上）
    else 提交成功
        T->>K: 只含 persist 字段的提交记录
        Note over D: nopersist 字段不进 WAL / Mongo；nosync 不进同步
    else 提交结果未知（ErrCommitIndeterminate）
        Note over T: 不回滚：abandon，fence 引擎，交给 recovery（nest/execution.go:271）
    end
```

**5. 失败与不确定结果**

| 情形 | 处理 | 结果 |
| --- | --- | --- |
| handler 返回 error（undo 策略） | `Rollback` 逆序执行 DAO 登记的逆操作 | 源字段与派生字段都回到事务开始时的值 |
| handler 返回 error（state 策略） | 恢复事务开始时的 DAO 快照（`CaptureRollbackState` 覆盖所有字段，含 nopersist） | 同上 |
| 提交被明确拒绝（WAL 准入失败、committer 拒绝、Remote 批次拒绝） | `rejectCommit` → `Rollback`，错误带 `ErrCommitRejected` | 同上 |
| 提交结果未知 | 不回滚，fence 引擎 | 调用方得到 `ErrCommitIndeterminate`；之后按 recovery 从存储重载 |
| 加载（`OnInitFinish`，无事务） | 只写 nopersist 字段：生成 setter 无事务时不登记 undo、不 `MarkPersist` | 不产生持久写 |
| 旧工程组件仍在登记 undo | 照常工作 | glsvet 打 `hint:`，退出码不变 |

**6. 测试**

| 用例 | 文件 | 覆盖 |
| --- | --- | --- |
| `TestTransientFieldsHaveMutatorsAndStayOutOfStorageAndSync` | `codegen/internal/dao/transient_field_promises_test.go` | 生成断言：有 mutator、快照覆盖、存储 / 提交 / 同步函数不碰它 |
| `TestATransientFieldRollsBackWithTheTransaction` 等 3 条 | `codegen/internal/dao/testdata/runtime/transient_test.go` | daoruntime：undo 回滚、state 快照、提交记录（Put 与 patch）与同步载荷不含 |
| `TestComponentRecordingItsOwnUndoIsHinted` | `cmd/glsvet/main_test.go` | 组件方法登记 undo 被提示 |
| `TestAttributeLayersRollBackWithTheTransaction`（NC-61） / `TestAttributeRollbackIsTheDaoRollback` | `demo/game/entities/player/attribute_component_test.go.tmpl` | 真实 Nest 派发 × undo / state × handler 失败 / 提交被拒 |
| `TestFinalIsRecomputedFromWornGearWhenThePlayerLoads`（NC-65） | 同上 | 加载时重建 |
| `TestNonPersistentAttributeLayersStayOutOfTheWAL` | 同上 | 真实 `nestwal` 写入 + 重放：有 `attr_base`，无 `attr_gear` / `attr_final` |
| `TestTheTimerHeapRollsBackWithTheTransaction`（NC-140） / `TestTimerRollbackIsTheDaoRollback` | `demo/game/entities/world/timer_component_test.go.tmpl` | 武装 / 触发 × 两种策略 × 两条失败路径 |
| `TestTimerBookkeepingStaysOutOfTheCommitRecord` | 同上 | 提交记录不含 `timer_next_due` |
| `TestCombatRollbackIsTheDaoRollback` / `TestNestUndoRollbackRestoresCombatStateExactly` | `skill/combatcomponent/dao_rollback_promises_test.go` / `skill/combatcomponent/component_test.go:106` | 战斗状态回滚字节一致 |

修前红文本（原样，出处 `docs/feature/evidence/dao-unified-rollback-20261005/red-demo.txt`；做法：新用例 + 旧组件模板，把两个 `captureRollback` 置为立即返回）：

```text
--- FAIL: TestAttributeLayersRollBackWithTheTransaction (0.00s)
    --- FAIL: TestAttributeLayersRollBackWithTheTransaction/a_later_step_fails (0.00s)
        attribute_component_test.go:241: rolled back to level 0 but the base layer kept the level-up: HP 110 attack 12, want HP 100 attack 10
    --- FAIL: TestAttributeLayersRollBackWithTheTransaction/the_commit_is_rejected (0.00s)
        attribute_component_test.go:241: rolled back to level 0 but the base layer kept the level-up: HP 110 attack 12, want HP 100 attack 10
--- FAIL: TestAttributeRollbackIsTheDaoRollback (0.00s)
    --- FAIL: TestAttributeRollbackIsTheDaoRollback/undo/handler_fails (0.00s)
        attribute_component_test.go:399: base after rollback = &{HP:110 Attack:12 Power:35 dirtyMask:7} (present true), want HP 100 attack 10
    --- FAIL: TestAttributeRollbackIsTheDaoRollback/undo/commit_rejected (0.00s)
        attribute_component_test.go:399: base after rollback = &{HP:110 Attack:12 Power:35 dirtyMask:7} (present true), want HP 100 attack 10
    --- FAIL: TestAttributeRollbackIsTheDaoRollback/state/handler_fails (0.00s)
        attribute_component_test.go:399: base after rollback = &{HP:110 Attack:12 Power:35 dirtyMask:7} (present true), want HP 100 attack 10
    --- FAIL: TestAttributeRollbackIsTheDaoRollback/state/commit_rejected (0.00s)
        attribute_component_test.go:399: base after rollback = &{HP:110 Attack:12 Power:35 dirtyMask:7} (present true), want HP 100 attack 10
FAIL
FAIL	example.com/a1demo/game/entities/player	0.969s
--- FAIL: TestTimerRollbackIsTheDaoRollback (0.00s)
    --- FAIL: TestTimerRollbackIsTheDaoRollback/arm/undo/handler_fails (0.00s)
        timer_component_test.go:350: rolled back, but node 1 is still pending
    --- FAIL: TestTimerRollbackIsTheDaoRollback/tick/undo/handler_fails (0.00s)
        timer_component_test.go:391: the rolled-back tick took the deadline away
    --- FAIL: TestTimerRollbackIsTheDaoRollback/arm/undo/commit_rejected (0.00s)
        timer_component_test.go:350: rolled back, but node 1 is still pending
    --- FAIL: TestTimerRollbackIsTheDaoRollback/tick/undo/commit_rejected (0.00s)
        timer_component_test.go:391: the rolled-back tick took the deadline away
    --- FAIL: TestTimerRollbackIsTheDaoRollback/arm/state/handler_fails (0.00s)
        timer_component_test.go:350: rolled back, but node 1 is still pending
    --- FAIL: TestTimerRollbackIsTheDaoRollback/tick/state/handler_fails (0.00s)
        timer_component_test.go:391: the rolled-back tick took the deadline away
    --- FAIL: TestTimerRollbackIsTheDaoRollback/arm/state/commit_rejected (0.00s)
        timer_component_test.go:350: rolled back, but node 1 is still pending
    --- FAIL: TestTimerRollbackIsTheDaoRollback/tick/state/commit_rejected (0.00s)
        timer_component_test.go:391: the rolled-back tick took the deadline away
FAIL
FAIL	example.com/a1demo/game/entities/world	0.489s
FAIL
```

combatcomponent（原样，出处 `red-combat.txt`；把组件里的 `nest.RecordUndo` 换成空操作）：

```text
--- FAIL: TestNestUndoRollbackRestoresCombatStateExactly (0.00s)
    component_test.go:161: defender state diverged after rollback:
--- FAIL: TestCombatRollbackIsTheDaoRollback (0.00s)
    --- FAIL: TestCombatRollbackIsTheDaoRollback/undo/handler_fails (0.00s)
        dao_rollback_promises_test.go:88: defender state diverged after rollback:
    --- FAIL: TestCombatRollbackIsTheDaoRollback/undo/commit_rejected (0.00s)
        dao_rollback_promises_test.go:88: defender state diverged after rollback:
FAIL
FAIL	github.com/tjbdwanghaibo/roost-core/skill/combatcomponent	0.487s
FAIL
```

codegen（原样，出处 `red-codegen.txt`；旧生成器）：

```text
--- FAIL: TestTransientFieldsHaveMutatorsAndStayOutOfStorageAndSync (0.01s)
    transient_field_promises_test.go:46: the generated DAO has no "func (d *VarietyDao) SetNeither(v int64)": a nopersist,nosync field cannot be changed inside a transaction
    transient_field_promises_test.go:46: the generated DAO has no "func (d *VarietyDao) SetPending(key int32, val int64)": a nopersist,nosync field cannot be changed inside a transaction
    transient_field_promises_test.go:46: the generated DAO has no "func (d *VarietyDao) DelPending(key int32)": a nopersist,nosync field cannot be changed inside a transaction
    transient_field_promises_test.go:46: the generated DAO has no "func (d *VarietyDao) GetPending(key int32) (int64, bool)": a nopersist,nosync field cannot be changed inside a transaction
    transient_field_promises_test.go:46: the generated DAO has no "func (d *VarietyDao) RangePending(": a nopersist,nosync field cannot be changed inside a transaction
    transient_field_promises_test.go:46: the generated DAO has no "func (d *VarietyDao) PendingLen() int": a nopersist,nosync field cannot be changed inside a transaction
FAIL
FAIL	github.com/tjbdwanghaibo/roost-core/codegen/internal/dao	0.470s
FAIL
```

glsvet 修前提示（原样，出处 `glsvet-hints-before.txt`；修后全仓 0 条，退出码均为 0）：

```text
base/game/entities/player/attribute_component.go:197:6: hint: component AttributeComponent.captureRollback registers its own undo (RecordUndo); keep transaction state in the DAO (a nopersist field if it must not be stored) so the DAO's rollback covers it
base/game/entities/world/timer_component.go:138:6: hint: component TimerComponent.captureRollback registers its own undo (RecordUndo); keep transaction state in the DAO (a nopersist field if it must not be stored) so the DAO's rollback covers it
skill/combatcomponent/component.go:276:2: hint: component CombatComponent.undoVitals registers its own undo (RecordUndo); keep transaction state in the DAO (a nopersist field if it must not be stored) so the DAO's rollback covers it
skill/combatcomponent/component.go:286:2: hint: component CombatComponent.undoAttributes registers its own undo (RecordUndo); keep transaction state in the DAO (a nopersist field if it must not be stored) so the DAO's rollback covers it
skill/combatcomponent/component.go:296:2: hint: component CombatComponent.undoBuffs registers its own undo (RecordUndo); keep transaction state in the DAO (a nopersist field if it must not be stored) so the DAO's rollback covers it
```

负对照：旧组件**带**手写 undo 时新用例全部通过（证明用例有效）；combatcomponent 的 state 两叶在红时也通过（状态本来就在 DAO 快照里）；codegen 的存储 / 同步隔离断言在旧生成器上已通过（缺的只是 mutator）。修后：`green-demo.txt` 全部 PASS；core `go test -race -count=3 ./cmd/glsvet ./codegen/internal/dao ./skill/combatcomponent`、`go test -count=1 ./codegen/...`、`go generate ./...` 后 porcelain、glsvet、`go build ./... && go vet ./...`、根包通过；生成 game-demo `go test -race -count=3 ./game/entities/... ./game/handler/...` 与 `go test -count=1 ./...` 通过；NC-61 / NC-65 / NC-140 原用例不改断言照样通过；`git diff d346b45c -- nest` 为空（方案 §8）。

**7. 性能证据**（Apple M5，Go 1.27.0，同机同命令 `-count 8 -benchtime 300ms`，n=8；旧 = `d346b45c` 模板生成，新 = 本分支模板生成；出处 `docs/feature/evidence/dao-unified-rollback-20261005/bench-stat.txt`，基准源码 `attribute_bench_test.go.txt` / `timer_bench_test.go.txt`）

| 基准 | sec/op 旧 → 新 | B/op 旧 → 新 | allocs/op 旧 → 新 |
| --- | --- | --- | --- |
| `A1AttributeLevelUpCommit` | 5.168µ → 5.163µ（~，p=0.721） | 8.553Ki → 8.388Ki（−1.93%） | 130 → 134（+3.08%） |
| `A1AttributeLevelUpRollback` | 3.016µ → 2.440µ（−19.10%） | 4.569Ki → 4.155Ki（−9.06%） | 64 → 64 |
| `A1AttributeRefreshGearCommit` | 6.658µ → 4.951µ（−25.63%） | 11.009Ki → 8.285Ki（−24.74%） | 146 → 118（−19.18%） |
| `A1TimerIdleTick` | 416.9n → 387.9n（~，p=0.083） | 1008 → 849（−15.77%） | 8 → 5（−37.50%） |
| `A1TimerArmAndFire` | 6.351µ → 6.661µ（+4.89%） | 12.19Ki → 13.37Ki（+9.65%） | 154 → 173（+12.34%） |

结论（方案 §8）：DAO 逐键 undo 低于旧实现每次复制全部层；代价在定时器武装 / 触发（每次从 DAO 建调度器，O(n log n)），World 节点数个位到十位可接受。combatcomponent 只是把同样的逆操作从组件搬进 DAO，未单独测。

**8. 未验证项与已知风险**

- 未在真实三进程（WAL + Mongo 投影）链路上跑被拒提交（用真实 Nest + 拒绝的 committer 与真实文件 WAL 覆盖，同一 `rejectCommit` 路径）。
- glsvet A1 提示的盲区（源码观察）：只认接收者类型名以 `Component` 结尾或匿名嵌入 `ComponentBase` 的方法，只看方法体里**直接**出现的 `RecordUndo` / `RecordUndoToken` / `DeferRollback` 调用；组件调用包级辅助函数间接登记、或组件字段持有可变状态但不登记 undo（直接漏回滚）都不会提示。“组件上不再有可回滚字段”没有自动检查。**（发版文档之后的更新，未发版）**：前者由 [RR-20261006-13](../../bug/RR-20261006-13.md) 补上（跟进一层同包 helper，`b7471ae4`）；后者按维护者第十三轮“A1 盲区”决定新增字段写提示——组件方法（`OnInitFinish` / `OnDestroy` 除外，同样跟进一层 helper）写非 DAO 句柄、非函数类型、未标 `//roost:cache` 的组件字段时打印 `hint:`，全仓 / 示例 / game-demo 0 条（[记录](../../feature/A1-COMPONENT-FIELD-WRITE-HINT-2026-10-06.md)）。
- 已生成工程不迁移，旧 `captureRollback` 仍在用户工程里；glsvet 在 CI（`go run ./cmd/glsvet ./...`）里会打提示但不失败。
- 定时器节点数到数千时需换写法（方案 §8）。

**9. review 检查点**

- [ ] 确认组件上不再有可回滚字段：读 `demo/game/entities/**/*_component.go.tmpl` 与 `skill/combatcomponent/component.go:222-225` 的组件结构体，字段只应是 `owner` / `dao` / 投影函数这类非事务状态。
- [ ] 确认 glsvet A1 提示覆盖的范围：`cmd/glsvet/main.go:691` `componentUndoHints` 只识别组件方法里的直接调用（之后 RR-20261006-13 跟进一层同包 helper，第十三轮“A1 盲区”新增 `cmd/glsvet/componentfields.go` 字段写提示与 `//roost:cache` 豁免，见上条“未验证项”的更新）；`TestComponentRecordingItsOwnUndoIsHinted`（`cmd/glsvet/main_test.go:166`）覆盖了哪些形状（嵌入 `ComponentBase` / 名字后缀 / 三种调用名）；CI 的 `go run ./cmd/glsvet ./...` 输出里当前应为 0 条 A1 hint。
- [ ] 确认全仓生产代码里登记 undo 的只有 DAO：`grep -rln 'RecordUndo\|DeferRollback' --include='*.go' . | grep -v _test.go` 只应是 `nest/`、`cmd/glsvet/main.go`（字符串）、`codegen/internal/dao/template_*.go`、`skill/combatcomponent/component.go`（且只在 `CombatDao.beginChange` 里，`:340-354`）。
- [ ] 确认 `nopersist,nosync` 字段不进提交记录 / WAL / 同步：看 `template_dao.go` 里 `persistFields` / `syncFields` 的使用点与 daoruntime `TestATransientFieldNeverReachesTheCommitRecordOrSync`。
- [ ] 确认 `CombatDao.beginChange`（`skill/combatcomponent/component.go:334`）在事务外 panic、在 state 策略下 `nest.RecordUndo` 返回 false 由快照兜底，且同一字段一笔事务只记第一条逆操作（`nest/rollback.go:187-191` 的 `undoKeys`）。
- [ ] 确认 demo 的 `OnInitFinish` 只写 nopersist 字段（`demo/game/entities/player/attribute_component.go.tmpl:89`、`demo/game/entities/world/timer_component.go.tmpl:113`），加载不产生持久写。

<a id="dao-2"></a>
### DAO-2 skill Runtime 状态不进事务（B4）

> 首发 v1.21.0 · [说明](guide-saga-drv-dao-rem.md#dao-2)

**1. 提交**

| 提交 | 版本 | 内容 |
| --- | --- | --- |
| `5407f127` | v1.20.2 | A1 方案 §4.4 把 Runtime 列为后续；`docs/skill/skill-casting-and-combat.md` 写现状约束 |
| `2954c583` | v1.21.0 | 记录维护者第四轮决定（含 B4） |
| `62cec54e` | v1.21.0 | “Runtime 不在事务里（B4）”一节；skill README；roost-coding A1 例外；glsvet `componentUndoHints` 注释 + `TestSkillPackagesGetNoComponentUndoHint` |
| `86882561` | v1.21.0 | DECISIONS-PENDING B4 标为已实施 |

**2. 改动文件与关键符号**

| `path:line` | 符号 | 职责 |
| --- | --- | --- |
| `docs/skill/skill-casting-and-combat.md:145` | “Runtime 不在事务里（B4）” | 回退 / 不回退对照表与四条设计约束 |
| `docs/agent-skills/roost-coding/SKILL.md:39` | A1 条“明确例外” | 不要为 Runtime 补 undo 或 DAO 化 |
| `cmd/glsvet/main.go:678-690` | `componentUndoHints` 注释 | 说明 Runtime 不会被命中、无需豁免 |
| `cmd/glsvet/main_test.go:222` | `TestSkillPackagesGetNoComponentUndoHint` | skill 各包零 A1 提示 |
| `skill/README.md` | combatcomponent 段 | 补 B4 说明 |

**3. 不变量与强制点**

- 不变量：Runtime 状态不参与 Nest 回滚；战斗 DAO 参与。强制点：无代码强制（这是“保持现状”的决定）；Runtime 没有任何 DAO 字段，也不登记 undo。
- 守卫：`TestSkillPackagesGetNoComponentUndoHint` 扫 `skill`、`skill/combatcomponent`、`skill/combat`、`skill/skillsync` 四个目录的非测试文件，A1 提示必须为 0。它防的是“有人给 Runtime / skill 组件补 undo 登记”，同时保证 glsvet 不会因 B4 误报。

**4. 控制流**（handler 推进 Runtime 后失败）

1. handler 校验业务条件（应全部放在 Runtime 调用之前）。
2. handler 调 `Runtime.Start / Activate / Advance …`；Runtime 经 `HostAdapter` 改战斗 DAO（资源、血量、buff），commit 时 `Host.PayCosts` → ammo 扣减 → 冷却。
3. handler 之后返回 error 或提交被拒：Nest 回滚战斗 DAO；Runtime 的冷却、ammo、cast、proc 账本、revision 不回退。
4. 结果：“法力已回滚、技能已进冷却”。业务若需要严格一致：提交确认后再推进 Runtime，或失败时 `Checkpoint` / `RestoreRuntime`。

**5. 失败与不确定结果**

| 情形 | 处理 | 结果 |
| --- | --- | --- |
| Runtime 调用前业务校验失败 | handler 返回 error | 无副作用（推荐写法） |
| Runtime 调用后 handler 返回 error | DAO 回滚 | Runtime 状态与 DAO 不一致（设计接受） |
| `PayCosts` 失败 | cast 不提交 | 冷却与 ammo 不动；启动阶段失败的 cast 直接删除（NC-110） |
| Host 命令返回 error | `failCastLocked` 记 `CastFailed` | 冷却按已提交处理；DAO 与 Runtime 看法一致 |
| 提交被拒 / 结果未知 | 只能由业务处理 | 见步骤 4 |

**6. 测试**

| 用例 | 文件 | 覆盖 |
| --- | --- | --- |
| `TestSkillPackagesGetNoComponentUndoHint` | `cmd/glsvet/main_test.go:222` | skill 四个包零 A1 提示 |

修前红文本：记录未保留原文（B4 是“写成约束 + 守卫”，守卫在实施时新增即绿，没有修前红）。验证命令：记录未保留原文（`62cec54e` 提交说明只列改动）。

**7. 性能证据**：无（不涉及代码路径）。

**8. 未验证项与已知风险**

- 守卫只覆盖 4 个目录；`skill/skillcompose` 与 `skill/examples/*` 不在扫描列表（源码观察）。
- Runtime 与 DAO 的不一致由业务按约束避免，没有运行期检测。

**9. review 检查点**

- [ ] 确认 `TestSkillPackagesGetNoComponentUndoHint`（`cmd/glsvet/main_test.go:222`）的目录列表是否应补 `skill/skillcompose`（`find skill -name '*.go' ! -name '*_test.go' -exec dirname {} \; | sort -u` 列出 8 个目录）。
- [ ] 确认 `docs/skill/skill-casting-and-combat.md:145` 一节列的“不回退”项与 Runtime 源码一致：`runtime_cast_window.go` 的 `commitCast` 顺序“支付 → ammo → 冷却”，`failCastLocked` 的冷却处理。
- [ ] 确认 roost-coding A1 条（`docs/agent-skills/roost-coding/SKILL.md:39`）的例外文字与 DECISIONS-PENDING 第四轮 B4 行一致。
- [ ] 确认仓内没有生产代码在 nest handler 里推进 Runtime 后再做会失败的业务检查（当前 Runtime 无正式生产调用方，方案 §4.4）。

<a id="dao-3"></a>
### DAO-3 combatcomponent 属性投影入口（N09 O2）

> 首发 v1.23.0（本版） · [说明](guide-saga-drv-dao-rem.md#dao-3)

**1. 提交**

| 提交 | 版本 | 内容 |
| --- | --- | --- |
| `78e26853` | v1.23.0 | 记录第十二轮决定（buff 投影按推荐） |
| `229a5aa0` | v1.23.0 | `AttributeProjection`、`CombatComponent.ProjectAttributes`、`deriveProjection`；各 mutator 末尾投影；`OnInitFinish` 投影；statusbridge 示例改用并包进事务；回归 `attribute_projection_promises_test.go`；文档。同提交 O22 / O7 / O29 与 cfggen globals 规则属 SKILL / CFG |
| `6bf15516` | v1.23.0 | DECISIONS-PENDING 第十二轮三行标为已实施 |
| `88f33776` | v1.23.0 | A16：`FieldVitals` 注释写明含 `ProjectAttributes` 写的字段；组件注释更正 |
| `ba13cb05` | v1.23.0 | 根包 `TestExamplesRun` 示例实跑门禁（起因是 statusbridge 在 A1 之后运行即 panic；属 TOOL） |

**2. 改动文件与关键符号**

| `path:line` | 符号 | 职责 |
| --- | --- | --- |
| `skill/combatcomponent/component.go:232` | `AttributeProjection` | `func(attribute func(combat.AttributeID) int64, combatant *combat.Combatant)` |
| `skill/combatcomponent/component.go:224` | `CombatComponent.projection` | 投影函数（代码，不是状态） |
| `skill/combatcomponent/component.go:240` | `OnInitFinish` | 加载 / 新建后投影一次 |
| `skill/combatcomponent/component.go:251` | `ProjectAttributes` | 安装并立即投影；nil 卸下 |
| `skill/combatcomponent/component.go:262` | `deriveProjection` | 唯一写投影字段的入口：克隆 vitals → 调投影 → `reflect.DeepEqual` 相同则不动 → 无事务直接写内存 / 有事务 `beginChange(FieldVitals)` + 写 + `markChanged(FieldVitals)` |
| `skill/combatcomponent/component.go:384`–`:472` | `InitCombatant`、`SetAttributeBase`、`SetAttributeBounds`、`ApplyBuff`、`RemoveBuff`、`SetBuffStacks`、`AdoptBuff`、`DispelBuffs`、`TickBuffs` | 末尾调 `deriveProjection`；`SetBuffDueTick`（`:442`）不调 |
| `skill/examples/statusbridge/main.go` | 示例 | `ProjectAttributes(projectCombat)`；整段包进 `nest.RunDetachedTransaction` |

**3. 不变量与强制点**

- 不变量 1：伤害读到的 Combatant 投影字段总是当前属性的结果。强制点：每个改属性来源的 mutator 末尾调 `deriveProjection`。
- 不变量 2：投影与源字段在同一事务里、同一笔逆操作 / 快照，回滚一起恢复。强制点：`deriveProjection` 在事务内经 `CombatDao.beginChange(FieldVitals)`（`:334`）登记 vitals 逆操作（undo 策略同一字段一笔事务只记第一条，恢复的是事务开始时的 vitals）。
- 不变量 3：加载不产生持久写。强制点：`nest.CurrentRollbackTx() == nil` 分支（`:272`）直接写内存、不 `markChanged`。
- 守卫测试：`TestBuffAttributeModifierReachesDamage`（`skill/combatcomponent/attribute_projection_promises_test.go:33`）、`TestAttributeProjectionRollsBackWithTheDao`（`:69`）、`TestAttributeProjectionReprojectsOnLoad`（`:132`）；`TestExamplesRun`（`examples_run_test.go:46`）保证示例实跑。

**4. 控制流**

```mermaid
sequenceDiagram
    participant H as handler（事务内）
    participant C as CombatComponent
    participant D as CombatDao
    participant T as RollbackTx
    H->>C: ApplyBuff(+100 护甲)
    C->>D: beginChange(FieldBuffs)
    D->>T: RecordUndo(dao, FieldBuffs)（undo 策略）
    C->>D: buffs.Apply → attributes 修饰生效；markChanged(FieldBuffs)
    C->>C: deriveProjection()
    C->>C: projected = clone(vitals)；projection(attributes.Current, &projected)
    alt projected == vitals
        C-->>H: 不动
    else 不同
        C->>D: beginChange(FieldVitals) → RecordUndo(dao, FieldVitals)
        C->>D: vitals = projected；markChanged(FieldVitals)
    end
    alt handler 失败 / 提交被拒
        T->>D: 逆序恢复 vitals、buffs（或恢复快照）
        Note over D: 护甲回到事务开始时的值，无需重算
    end
```

**5. 失败与不确定结果**

| 情形 | 处理 | 结果 |
| --- | --- | --- |
| handler 失败 / 提交被拒（undo） | 逆操作恢复 vitals 与源字段 | 投影字段回到事务开始时的值，字节一致、无残留脏位 |
| 同上（state） | 恢复 DAO 快照 | 同上 |
| 加载时投影函数已变 | 只写内存，不标脏 | 存储里的 vitals 在下一次 vitals 提交时写回 |
| 投影结果与现值相同 | 什么都不做 | 无持久写、无逆操作 |
| `ApplyBuff` 被免疫 | 不 markChanged、不投影 | — |
| 投影函数改了 `Health` 等过程状态 | 约定禁止，无运行期检查 | 行为未定义（风险） |

**6. 测试**

| 用例 | 文件 | 覆盖 |
| --- | --- | --- |
| `TestBuffAttributeModifierReachesDamage` | `skill/combatcomponent/attribute_projection_promises_test.go` | +100 护甲 buff 前后各打 100 物理伤害：100 / 50；移除 buff 后回到 100 |
| `TestAttributeProjectionRollsBackWithTheDao` | 同上 | 两种回滚策略 × handler 失败 / 提交被拒：投影护甲随 DAO 回到 20，字节一致、无残留脏位 |
| `TestAttributeProjectionReprojectsOnLoad` | 同上 | 加载后 `OnInitFinish` 投影出存储属性 50、不标脏 |

修前红文本（原样，出处 `docs/feature/ROUND12-SKILL-CFGGEN-2026-10-06.md` §2 表 O2 行；旧 API 下同一场景）：

```text
health damage without / with the +100 armor buff = 100 / 100, want 100 / 50
```

负对照（同一出处）：把 `deriveProjection` 里的 `beginChange(FieldVitals)` 去掉，undo 策略的两个子用例失败，state 策略通过（vitals 本来就在 DAO 快照里）。修后验证（同一出处 §3）：`go test -race -count=3 ./skill/... ./codegen/internal/cfggen/ ./configdata/...` 通过；`skill/examples` 的 `combat`、`fireball`、`statusbridge` 都运行退出 0（statusbridge 修前 panic）。

**7. 性能证据**：未测（每个改属性的 mutator 多一次 Combatant 克隆 + `reflect.DeepEqual`；只在装了投影时发生）。

**8. 未验证项与已知风险**

- 投影纯度只靠约定；投影函数写了过程状态字段会被每次投影覆盖或回滚，没有检查。
- `reflect.DeepEqual` 比较整个 Combatant（含 map），成本随字段增长（推断，未测）。
- 加载时投影函数变化导致内存与存储不同、直到下一次 vitals 提交才写回：期间若只改了别的字段，存储里的 vitals 仍是旧投影（文档已写明“下一次 vitals 提交写回”）。

**9. review 检查点**

- [ ] 确认每个改 attributes / buffs 的 mutator 末尾都调了 `deriveProjection`：`grep -n 'beginChange(Field\(Attributes\|Buffs\))' skill/combatcomponent/component.go` 的每个函数都应有对应调用；`SetBuffDueTick`（`:442`）例外是有意的。
- [ ] 确认 `deriveProjection`（`:262`）在事务内先 `beginChange` 再写 `dao.combatant`，且在事务外的分支不 `markChanged`。
- [ ] 确认 `HostAdapter` / `StatusBridge` 的命令最终都经上述 mutator 落地（`skill/combatcomponent/adapter.go`、`status_bridge.go`），而不是直接改 `dao.attributes` / `dao.buffs`。
- [ ] 确认 `ApplyDamage`（`:487`）不改属性、因而不重投影的前提成立（只改 vitals 的 Health / Shield 等）。
- [ ] 确认 `skill/examples/statusbridge/main.go` 在 `nest.RunDetachedTransaction` 内调用战斗 mutator，且已登记在根包 `TestExamplesRun` 的 `exampleRuns` 里。

## REM：remoteentity 快照缓存与 Mirror

<a id="rem-1"></a>
### REM-1 共享 L2 为快照水位权威（B2）与 Cached 读最大陈旧时间

> 首发 v1.20.2 · [说明](guide-saga-drv-dao-rem.md#rem-1)

**1. 提交**

| 提交 | 版本 | 内容 |
| --- | --- | --- |
| `6f06f0da` | v1.20.2 | 背景（NONCORE，NC-130）：L2 CAS 落败报 `ErrStaleWrite`，Publish 不再把旧快照装进 L1 |
| `c3475150` | v1.20.2 | 背景（NONCORE，NC-131）：表满时 Stats 先清过期兴趣 |
| `366058a7` | v1.20.2 | 背景（NONCORE，RR-20260913-01 残余）：版本化删除在共享 L2 留墓碑 |
| `b7f98343` | v1.20.2 | 记录维护者第三轮决定（B2 L2 为水位权威） |
| `f376bba0` | v1.20.2 | B2 主体：`admitLocked` 唯一写入口、`confirmedAt`、`refresh`、`MaxStaleness`、`published_at` 与历史丢弃、L2 `DeleteAtVersion` 返回 `ErrStaleWrite`；用例、矩阵、基准 |
| `7d49e54d` | v1.20.2 | `remote_entity.cached_max_staleness` 按 A4 严格读取并登记 `frameworkDurationKeys`；USER_GUIDE / CHANGELOG / 交接 / N05 / Mirror 方案状态 |
| `05633529` | v1.20.2 | DECISIONS-PENDING B2 标为已实施 |
| `88f33776` | v1.23.0（本版） | B2 §7 回填“L2 落后于权威”的上界（见 [REM-11](#rem-11)） |
| `fcc78ad0` | v1.23.0（本版） | 生成配置模板写出 `cached_max_staleness`（见 [REM-13](#rem-13)） |

**2. 改动文件与关键符号**（行号以 `02c8a10d` 为准）

| `path:line` | 符号 | 职责 |
| --- | --- | --- |
| `entity/remote_snapshot.go:105` | `RemoteSnapshotCacheConfig` | 新增 `MaxStaleness`（`:119`）、`Now`（`:121`） |
| `entity/remote_snapshot.go:127` | `RemoteSnapshotCache` 类型注释 | 写明 B2 契约：L2 唯一权威、L1 有界副本、写入经 `admitLocked`、降级不冒充已确认 |
| `entity/remote_snapshot.go:162` | `publishMu [64]sync.Mutex` | 同一 key 全部 L1 写入与它们的 L2 调用串行化 |
| `entity/remote_snapshot.go:211` | `remoteSnapshotEntry` | L1 条目：快照或删除标记 + `confirmedAt`（0 = 未确认） |
| `entity/remote_snapshot.go:271` | `NewRemoteSnapshotCache` 缺省 | `MaxStaleness` 零值取 `TTL`，`TTL` 也为零取 30s |
| `entity/remote_snapshot.go:330` | `fresh` | `confirmedAt != 0 && now-confirmedAt <= maxStaleness` |
| `entity/remote_snapshot.go:601` | `remoteSnapshotEntryStale` | L1 唯一新旧判定（快照按 epoch / 版本；删除只按版本，同版本删除胜） |
| `entity/remote_snapshot.go:684` | `readConfirmed` | 热路径一次 L1 读 + 一次读时钟；否则 `coalesce` 合并的 `refresh` |
| `entity/remote_snapshot.go:719` | `refresh` | 锁外读 L2，锁内与 L1 比较：同值改记确认、L2 更新改记 L2、L1 更新补写 L2、L2 无值回源 |
| `entity/remote_snapshot.go:807` | `loadForRefresh` | L2 担保不了时回源；权威说不存在就删掉加载开始前确认的 L1 快照 |
| `entity/remote_snapshot.go:901` | `publishLocked` | 快照写入的前置判断（同值只延有效期 / 补确认；已确认的更新条目在前则不写 L2） |
| `entity/remote_snapshot.go:937` | `admitLocked` | 唯一的“先 L2 后 L1”写入口：接受 / stale→`adoptSharedLocked` / 冲突原样返回 / 其他错误降级未确认 |
| `entity/remote_snapshot.go:959` | `writeShared` | 快照 `l2.Set`（CAS），删除 `DeleteAtVersion`（无能力时退化为 `Delete`） |
| `entity/remote_snapshot.go:983` | `adoptSharedLocked` | L2 拒绝后让 L1 跟上 L2；L2 无活值时删掉不新于被拒写的 L1 快照 |
| `entity/remote_snapshot.go:1008` | `setL1Locked` | 实际写 L1（`AtomicLocalStore.SetWithTTL`，L1 自己的 Stale / Conflict 准入） |
| `entity/remote_snapshot.go:1096` | `DeleteAtVersion` | 删除标记经 `admitLocked` |
| `remoteentity/snapshot_l2.go:43` | `remoteSnapshotL2CAS` | L2 版本 CAS 脚本（未改），成功时 `PEXPIRE`（`:70`） |
| `remoteentity/snapshot_l2.go:88` | `remoteSnapshotL2DeleteAtVersion` | 墓碑脚本（未改），`PEXPIRE`（`:98`） |
| `remoteentity/snapshot_l2.go:324` | `remoteSnapshotL2Store.DeleteAtVersion` | 脚本返回 0（L2 持有更新快照）时返回 `cache.ErrStaleWrite`（`:348`），之前 nil |
| `remoteentity/syncer.go:37` | `remoteSnapshotWire.PublishedAt` | 复制 wire 新字段 `published_at`（omitempty） |
| `remoteentity/syncer.go:126` | `SnapshotReplicaStore.ApplyReplica` 历史过滤 | 早于 `snapshot_l2_ttl / 2` 的快照更新丢弃并计数；删除在 `:113` 之前处理、不过滤 |
| `remoteentity/syncer.go:147` | `SnapshotReplicaStore.historic` | 无发布时刻或无 L2 TTL 时不过滤 |
| `remoteentity/config.go:44` | `Config.CachedMaxStaleness` | 核心配置项 |
| `remoteentity/snapshot_client.go:156` | `newSnapshotClient` | 把 `CachedMaxStaleness` 交给缓存 `MaxStaleness` |
| `kit/remoteentity/remote_entity_mod.go:307` | `readSnapshotConfig` | `cached_max_staleness` 严格读取、必须为正 |
| `app/config_validation.go:287` | `frameworkDurationKeys` | 登记 `remote_entity.cached_max_staleness` |

**3. 不变量与强制点**

- 不变量 I1：一个 key 的全部 L1 写入在该 key 的 `publishMu` 分片锁下进行（`entity/remote_snapshot.go:162`）。核对结果：`Publish`（`:886`）、`fetchAndAdmit`（`:465`）、`refresh`（`:733`）、`loadForRefresh`（`:817`）、`Delete`（`:1078`）、`DeleteAtVersion`（`:1100`）都先取这把锁。
- 不变量 I2：写进 L1 的快照值要么被 L2 以 CAS / 带版本删除接受过（`admitLocked`），要么就是刚从 L2 读到的值（`refresh` / `adoptSharedLocked`），要么带 `confirmedAt = 0` 降级。
  - **与记录的字面差异**：方案 §2.1 与类型注释（`:131`）写“全部 L1 写入都经 `admitLocked`”。源码里经 `admitLocked` 的是 `publishLocked`（`:924`）、`DeleteAtVersion`（`:1110`）、`refresh` 的“L1 比 L2 新”修复分支（`:765`）；另有直接写 L1 的点：`refresh` 的 `setL1Locked`（`:744`、`:756`、`:779`、`:795`，写入的是 L2 刚读到的值或给删除标记补确认时刻）、`adoptSharedLocked` 的 `setL1Locked`（`:1001`）与 `l1.Delete`（`:997`）、`loadForRefresh` 的 `l1.Delete`（`:820`）、无版本 `Delete` 的 `l1.Delete`（`:1088`）。这些写入都在 `publishMu` 下，值都来自 L2 或权威“不存在”，语义上不违反“L1 只缓存 L2 确认过的版本”，但不是字面上的“只经 `admitLocked`”。（已闭环，fixr：注释与 B2 §2、DECISIONS 第九轮按源码改为“新值经 `admitLocked`，其余只回填 L2 的值或删除”，逐点核对没有绕过准入，见 [RR-20261006-11 记录](../../bugfix/RR-20261006-11.md) §2.1。）
- 不变量 I3：非线性读只交出 `fresh` 的条目（`readConfirmed` `:687`、`refresh` `:736`）；`Linearizable` 每次读权威（`Read` `:658`）。
- 不变量 I4：同版本异值是一致性错误、不降级（`admitLocked` `:949`、`refresh` `:751`）。
- 不变量 I5：早于 `snapshot_l2_ttl / 2` 的复制快照不进缓存（`remoteentity/syncer.go:126`）。
- 守卫测试：`TestB2*` 六条（`remoteentity/snapshot_l2_watermark_promises_test.go`）；组合矩阵 `TestRealB2WatermarkMatrixStandalone` / `TestRealB2WatermarkMatrixCluster`（`remoteentity/snapshot_l2_watermark_matrix_integration_test.go`）；O5 `TestRealJetStreamReplayAfterL2ExpiryDoesNotResurrect`（`remoteentity/snapshot_replay_jetstream_integration_test.go`）。**没有结构性守卫**（例如 grep 新增的 L1 写入点）防止今后绕过 `publishMu` / `admitLocked` 新增写入口。（已闭环，fixr：加 AST 守卫 `TestRemoteSnapshotCacheWritesStayInTheListedFunctions`，见 [RR-20261006-11 记录](../../bugfix/RR-20261006-11.md) §2.1。）

**4. 控制流**

写入（发布 / 复制 / 加载回填 / 删除）：

1. 调用方取 `publishMu[shard(key)]`。
2. `publishLocked` 前置判断（同值且已确认且有效期不更晚 → 直接返回，不写 L2；L1 已确认持有更新条目 → 返回，不写 L2，避免 L2 过期时复活旧值）。删除走 `DeleteAtVersion` 的同类判断。
3. `admitLocked`：记 `started`，`writeShared` 调 L2（受 `loadTimeout` 限时）。
4. 按结果写 L1：接受 → `confirmedAt = started`（权威结果取更早的加载开始时刻）；`ErrStaleWrite` → `adoptSharedLocked` 读 L2 并记下；冲突 → 原样返回；其他 → `confirmedAt = authoritativeAt`（发布 / 复制为 0，未确认）。
5. 释放锁；`notify` 唤醒版本等待者。

```mermaid
flowchart TD
  W["写入：Publish / ApplyUpdate / fetchAndAdmit / DeleteAtVersion"] --> L["取 publishMu["shard(key)"]"]
  L --> P{"publishLocked / DeleteAtVersion 前置判断"}
  P -- "同值已确认 或 L1 已确认更新" --> X["返回，不写 L2"]
  P -- "需要写" --> A["admitLocked：writeShared 调 L2"]
  A -- "接受" --> C["setL1Locked，confirmedAt = started（权威取加载开始时刻）"]
  A -- "ErrStaleWrite" --> D["adoptSharedLocked：读 L2"]
  D -- "L2 有值" --> D1["setL1Locked(L2 的值, confirmedAt = started)"]
  D -- "L2 无活值" --> D2["删掉不新于被拒写的 L1 快照"]
  D -- "L2 读不到" --> D3["L1 保持原样"]
  A -- "同版本异值" --> E["返回 ErrRemoteVersionConflict，L1 不写"]
  A -- "断网 / 结果未知" --> U["setL1Locked，confirmedAt = authoritativeAt（通常 0 = 未确认）"]
```

非线性读：

```mermaid
flowchart TD
  R["Read(Cached / Monotonic)"] --> H{"L1 条目 fresh？"}
  H -- "是" --> S["serveAt 交出（删除标记 = 未找到）"]
  H -- "否 / 无条目" --> CO["coalesce(key, refresh) 合并"]
  CO --> G["锁外 L2 HGET"]
  G --> LK["取 publishMu，重看 L1"]
  LK -- "L1 已被别人确认" --> S
  LK -- "L2 有值，L1 无" --> F1["记 L2 值并交出"]
  LK -- "L2 与 L1 同值" --> F2["改记确认时刻并交出"]
  LK -- "L1 比 L2 新" --> F3["admitLocked 补写 L2；仍不 fresh 则回源"]
  LK -- "L2 更新" --> F4["记 L2 值并交出"]
  LK -- "L2 无值 / 读不到，L1 是快照" --> AU["loadForRefresh 回源权威"]
  LK -- "L1 与 L2 都没有" --> NF["未找到（Cached 不回源；Monotonic 另行回源）"]
```

**5. 失败与不确定结果**

| 情形 | 处理 | 对调用方 |
| --- | --- | --- |
| 写入时 L2 断网 / 结果未知 | L1 照记，`confirmedAt = 0`，`remoteErrors` 计数 | 写入成功；之后的非线性读不交出这份，先重新确认 |
| 写入被 L2 以 stale 拒绝 | L1 改记 L2 当前值 | 发布不报错（输给更新值是预期结果） |
| 同版本异值 | 不写 L1 | `ErrRemoteVersionConflict` |
| 读时条目未确认 / 超上限、L2 可读 | 按 `refresh` 规则确认、补写或改记 | 交出确认过的值 |
| 读时 L2 读不到且权威成功 | 权威结果按加载开始时刻确认 | 交出权威结果 |
| 读时 L2 与权威都失败 | 不交出 | 返回错误（loader 的错误或 `ErrRemoteSnapshotStale`） |
| 复制消息早于 `snapshot_l2_ttl / 2` | 丢弃，计 `remote_entity.snapshot_replica_historic_dropped_total` | 无（之后按需读取） |
| 旧发布者的消息无 `published_at` | 照旧接受 | 滚动升级期间 O5 仍可能出现 |

**6. 测试**

| 用例 | 文件 | 覆盖 |
| --- | --- | --- |
| `TestB2CachedReadPastMaxStalenessReconfirmsAgainstL2` | `remoteentity/snapshot_l2_watermark_promises_test.go:84` | 超过上限的 Cached 读先对 L2 重新确认 |
| `TestB2EntryWrittenDuringL2OutageIsNotServedUnconfirmed` | 同上 `:112` | L2 断网时写入的条目不以未确认身份交出 |
| `TestB2PublisherRepairsALostL2Write` | 同上 `:178` | owner 下一次读补写丢失的 L2 写 |
| `TestB2LostL2DeleteIsRepairedByTheNextRead` | 同上 `:205` | 丢失的带版本删除由下一次读重发 |
| `TestB2HistoricReplicaPastL2MemoryIsNotAdmitted` | 同上 `:255` | O5：过老的复制快照不准入 |
| `TestB2ConcurrentWritersAndReconfirmingReadersConvergeOnL2` | 同上 `:293` | 并发写与重新确认读收敛到 L2 |
| `TestRealB2WatermarkMatrixStandalone` / `…Cluster` | `remoteentity/snapshot_l2_watermark_matrix_integration_test.go:43` / `:47` | 真实 Redis 单机与自建 3 主 3 从 Cluster，各 20 格 |
| `TestRealJetStreamReplayAfterL2ExpiryDoesNotResurrect` | `remoteentity/snapshot_replay_jetstream_integration_test.go:33` | 真实 JetStream + Redis 复现 O5 |
| `TestCachedMaxStalenessConfiguration` | `kit/remoteentity/cached_max_staleness_test.go:13` | 严格读取、必须为正 |

修前红（原样，出处 [B2 §7“先红后绿”](../../feature/B2-REMOTE-SNAPSHOT-L2-WATERMARK-2026-10-06.md)）：

```text
--- FAIL: TestB2CachedReadPastMaxStalenessReconfirmsAgainstL2
    Cached read 11s after confirmation (max staleness 10s) returned payload="v1" version=1 found=true err=<nil>; L2 holds v2
--- FAIL: TestB2EntryWrittenDuringL2OutageIsNotServedUnconfirmed
    an entry written while L2 was unreachable was served unconfirmed: payload="v1" version=1 found=true err=<nil>; L2 holds v2
--- FAIL: TestB2PublisherRepairsALostL2Write
    L2 after the owner's next read holds version=1 held=true err=<nil>; the lost v2 write was never repaired
--- FAIL: TestB2LostL2DeleteIsRepairedByTheNextRead
    L2 still serves the deleted snapshot version=1 held=true err=<nil>; the lost delete was never repaired
--- FAIL: TestB2HistoricReplicaPastL2MemoryIsNotAdmitted
    a replica published 10m ago (L2 TTL 1m) was admitted: payload="v1-from-history" found=true err=<nil>

# 真实 JetStream + 真实 Redis（O5 复现）
--- FAIL: TestRealJetStreamReplayAfterL2ExpiryDoesNotResurrect
    replayed messages applied=1; late joiner Cached read found=true payload="v1" err=<nil>; L2 held=true version=1 err=<nil>
    the late joiner serves the replayed v1 (authority is at v2)

# 组合矩阵：单机与自建 Cluster 各 20 格，各红 8 格（两个后端相同）
--- FAIL: .../old-over-new/{publish,replica,load}/hot        the reader (hot=true) read version=1 ... found=true
--- FAIL: .../delete-resurrect/{publish,replica,load}/hot    the reader (hot=true) read version=1 ... found=true
--- FAIL: .../deliverall-replay/replica/cold                 L2 holds the stale input version=1 route=1
--- FAIL: .../deliverall-replay/replica/hot                  the reader (hot=true) read version=1 ... found=true
```

修后（原样，同出处）：

```text
ok  remoteentity  TestB2*（6 条，含并发收敛；-race -count=5）
--- PASS: TestRealB2WatermarkMatrixStandalone (4.04s)    20/20
--- PASS: TestRealB2WatermarkMatrixCluster (6.48s)       20/20（3 主 3 从，端口 17380～17385，自建自杀）
--- PASS: TestRealJetStreamReplayAfterL2ExpiryDoesNotResurrect (2.12s)
    replayed messages applied=1; late joiner Cached read found=false ...; L2 held=false
--- PASS: TestRealSnapshotL2KeyPrefixOnRedis / OnRedisCluster / StaleWrite / Tombstone（既有真实 Redis 用例）
```

改了断言的既有用例（契约变化，记录 B2 §7）：`TestRemoteSnapshotPublishDoesNotPinShardOnUnresponsiveL2`（`entity/remote_snapshot_test.go`）、`TestStaleBackfillControls/L2 outage`、`TestPublishConflictAfterPreflight`（`entity/snapshot_delete_l2_promises_test.go`，预查已删，改为直接验证 CAS 裁决）、`TestRemoteSnapshotL2DeleteAtVersionKeepsNewerSnapshot` 与 `TestRealSnapshotL2KeyPrefixOnRedis(Cluster)`（被拒的带版本删除返回 `cache.ErrStaleWrite`）、`TestRemoteSnapshotDeleteAtVersionPromiseClearedByNewerSnapshot`。负对照：记录未保留单独的“退回修复变红”运行；修前红即基线实现上的红。

**7. 性能证据**（原样，出处 B2 §7；真实 Redis 单机，同机交替 3 轮 × count 2，`-benchtime 3000x`，Apple M5，Go 1.27.0；基准在 `remoteentity/snapshot_l2_watermark_bench_integration_test.go:66`～`:112`）

```text
                         │   修前      │   修后                         │
RealB2PublishWarm-10       34.53µ ± 7%   34.42µ ± 1%        ~ (p=0.589 n=6)
RealB2ReplicaCold-10       55.99µ ± 3%   34.68µ ± 2%  -38.06% (p=0.002 n=6)
RealB2CachedHit-10         147.2n ± 2%   143.8n ± 3%   -2.31% (p=0.015 n=6)
RealB2CachedReconfirm-10                 21.79µ ± 1%   （修前无此路径）
allocs/op: PublishWarm 29 → 29，ReplicaCold 46 → 28，CachedHit 0 → 0，CachedReconfirm 20
```

结论：L1 写的 L2 往返不变（一次 CAS）；冷节点复制写入少一次 HGET（−38%）；读命中不碰 Redis、无退化；超过上限的读多一次 HGET（约 22µs），每个 key 每个陈旧窗口一次。

**8. 未验证项与已知风险**

- O5 的发布时刻用发布方 `time.Now()` 与接收方 `time.Now()` 比较（`remoteentity/syncer.go:47`、`:126`），跨主机时钟偏差只靠“留一半窗口”吸收，未在多主机验证（E02）。
- 没有共享 L2 的装配里删除标记受 L1 容量淘汰，迟到旧消息可复活（只适合单进程 / 测试）。
- 滚动升级期间旧发布者不带 `published_at`。
- L2 落后于权威的上界见 [REM-11](#rem-11)。

**9. review 检查点**

- [x] grep `entity/remote_snapshot.go` 中全部 L1 写入点（`setL1Locked(`、`admitLocked(`、`c.l1.Delete(`、`c.l1.SetWithTTL(`，当前 `:744 :756 :765 :779 :795 :820 :924 :946 :954 :997 :1001 :1013 :1088 :1110`），逐一确认都在 `publishMu[shard(key)]` 下；并判断“只经 `admitLocked`”的字面说法（`:131`、B2 §2.1、DECISIONS 第九轮）是否应改为“经 `admitLocked`，或写入刚从 L2 读到的值”。（fixr：已核对并加守卫，见 [RR-20261006-11 记录](../../bugfix/RR-20261006-11.md) §2.1。）
- [ ] 确认 `refresh`（`:719`）在锁外读 L2、锁内重看 L1 时，`hasCurrent && fresh` 的短路（`:736`）不会把一个“别人刚以 `confirmedAt = authoritativeAt` 写入、但比 L2 旧”的条目当作 fresh 交出（关注 `authoritativeAt` 早于 `started` 的情形）。
- [ ] 确认 `publishLocked` 的“已确认的更新条目在前则不写 L2”（`:918`）与 O5 一起成立：L1 有已确认 v2、L2 已过期时，迟到的 v1 不会写进 L2（看 `TestRealB2WatermarkMatrix*` 的 `deliverall-replay/replica/hot` 格）。
- [ ] 确认 `adoptSharedLocked` 在 L2 无活值时只删“不新于被拒写”的 L1 快照、不记删除标记（`:995`～`:998`），并看注释给的理由（避免挡住键过期后另一 epoch 的合法写入）是否被某个用例钉住。
- [ ] 确认 `remoteSnapshotL2Store.DeleteAtVersion` 返回 `ErrStaleWrite`（`remoteentity/snapshot_l2.go:348`）之后，`admitLocked` 走 `adoptSharedLocked` 而不是降级（`entity/remote_snapshot.go:947`）。
- [ ] 确认 `historic`（`remoteentity/syncer.go:147`）只过滤快照更新、不过滤删除（删除分支 `:113` 在过滤之前返回）。
- [ ] 确认 kit 的 `cached_max_staleness` 严格读取：写成 `30`（无单位）时 `read.Err()` 先于“必须为正”报错（`kit/remoteentity/remote_entity_mod.go:308`～`:313`），对照 `TestCachedMaxStalenessConfiguration`。

<a id="rem-2"></a>
### REM-2 Mirror 第 1～3 步：只读契约、快照唯一读出口、共享 SnapshotClient

> 首发 v1.21.0 · [说明](guide-saga-drv-dao-rem.md#rem-2)

**1. 提交**

| 提交 | 版本 | 内容 |
| --- | --- | --- |
| `2954c583` | v1.20.2（文档） | 记录维护者第四轮决定（含“之后补 Mirror”） |
| `8495c5c4` | v1.21.0 | 第 1～3 步：`entity/remote_mirror.go`（新）、`RemoteSnapshotCache.Read` 唯一读出口、`remoteentity/snapshot_client.go`（新）、Manager 委托；用例 |
| `b31d7640` | v1.21.0 | DECISIONS-PENDING Mirror 行标为第 1～3 步已实施 |
| `207163f9` | v1.21.0 | 发版前复审补修 nest allow_stale（见 [REM-3](#rem-3)） |

**2. 改动文件与关键符号**

| `path:line` | 符号 | 职责 |
| --- | --- | --- |
| `entity/remote_mirror.go:26` | `ErrRemoteObservationIncomparable` | 混合 epoch 不可比 |
| `entity/remote_mirror.go:29` | `ErrRemoteReadUnsupported` | 读者不提供请求的一致性 |
| `entity/remote_mirror.go:37` | `RemoteObservation` | 观察 token（MarkerEpoch / RouteEpoch / StateVersion） |
| `entity/remote_mirror.go:56` | `RemoteObservation.Covers` | 所有读出口共用的最低要求判定 |
| `entity/remote_mirror.go:74` | `RemoteSnapshotRead` | 一次只读请求（Key / Consistency / After） |
| `entity/remote_mirror.go:85` | `RemoteSnapshotReadOnly` | 只读方唯一能力 `ReadSnapshot` |
| `entity/remote_mirror.go:90` | `RemoteMirrorSpec` | 视图身份（Tenant / Kind / Scope / Policy / Schema / Codec） |
| `entity/remote_mirror.go:117` | `NewRemoteMirrorReader` | 校验 spec，返回 reader |
| `entity/remote_mirror.go:133` | `RemoteMirrorReader.Read` | 读侧核对 key / schema / codec，解码 `BytesCopy()`（`:149`） |
| `entity/remote_snapshot.go:643` | `RemoteSnapshotCache.Read` | 唯一读出口：Linearizable 每次读权威、Monotonic 不足合并回源、Cached 不回源且不满足返回 `ErrRemoteSnapshotStale` |
| `entity/remote_snapshot.go:629` | `Get` | 旧签名外观 |
| `entity/remote_snapshot.go:350` | `LoadAuthoritative` | 旧签名外观 |
| `entity/remote_snapshot.go:366` | `loaderMinVersion` | 只约束版本的 token 下推给 loader，带 epoch 的不下推 |
| `entity/remote_snapshot.go:375` | `covers` | 读出口最低要求检查 |
| `entity/remote_snapshot.go:226` | `remoteSnapshotLoadKey` | 合并键 `(key, after, refresh)` |
| `remoteentity/snapshot_client.go:22` | `ErrSnapshotClientStopped` | 客户端已停 |
| `remoteentity/snapshot_client.go:36` | `SnapshotClient` | 快照协议唯一实现 |
| `remoteentity/snapshot_client.go:102` | `NewSnapshotClient` | 校验后构造（`validateSnapshotClientConfig` `:112`） |
| `remoteentity/snapshot_client.go:133` | `newSnapshotClient` | 不校验的构造（Manager 内嵌） |
| `remoteentity/snapshot_client.go:165` | `ReadSnapshot` | 停止检查（`:181`）、Linearizable 能力门（`:184`）、续租兴趣（`:192`）、`cache.Read`（`:193`） |
| `remoteentity/snapshot_client.go:387` | `publishCommitted` | owner 提交后发布（包内，只读方拿不到） |
| `remoteentity/snapshot_client.go:450` | `Start` | 订阅复制主题，失败逐步回收 |
| `remoteentity/snapshot_client.go:503` | `unsubscribe` | Assembly 启动后续失败时退订 |
| `remoteentity/snapshot_client.go:522` | `Stop` | 三步停机 |
| `remoteentity/snapshot_client.go:556` | `gatedLoader` | 权威加载受 `work` 准入约束、Stop 时取消 |
| `remoteentity/snapshot_client.go:573` | `gatedSnapshotL2` | L2 调用受 `work` 准入约束 |
| `remoteentity/transaction_manager.go:725` | `Manager.ReadRemoteSnapshot` | 委托客户端（外层回退已删除） |
| `remoteentity/transaction_manager.go:734` | `Manager.ReadSnapshot` | 只读能力 |
| `remoteentity/transaction_manager.go:743` | `Manager.SnapshotClient` | 同进程只读方用 |
| `remoteentity/transaction_manager.go:815` | `afterRemoteCommit` | 发布改为 `publishCommitted`（`:822`） |

**3. 不变量与强制点**

- 唯一读出口：所有非线性 / 线性读经 `RemoteSnapshotCache.Read`（`entity/remote_snapshot.go:643`）；`SnapshotClient.ReadSnapshot` 只调它（`remoteentity/snapshot_client.go:193`）；Manager 的读委托客户端（`remoteentity/transaction_manager.go:729`、`:738`）。守卫：`TestRemoteSnapshotReadExitsShareOnePostCondition`（`entity/remote_mirror_promises_test.go`）、`TestReadRemoteSnapshotMonotonic*`（`remoteentity/snapshot_read_exit_promises_test.go`）。
- 只读方无写能力：`SnapshotClient` 没有导出的发布 / 删除方法，`publishCommitted` 包内（`:387`）。守卫：`TestSnapshotClientHasNoWriteCapability`（`remoteentity/snapshot_client_promises_test.go`）。
- Linearizable 能力门（`:184`），守卫 `TestSnapshotClientLinearizableNeedsADeclaredLoader`。
- 启动失败不留订阅（`Start` `:475`～`:485`），守卫 `TestSnapshotClientStartFailureLeavesNoSubscription`。
- 停机三步、返回 nil 前不释放依赖（`Stop` `:522`～`:550`，`work.Wait`），守卫 `TestSnapshotClientStopContract`（`stopcontract.Check` + `CallerReleases`）、`TestSnapshotClientStopCancelsLoads`。
- DTO 不污染缓存（`BytesCopy`，`entity/remote_mirror.go:149`），守卫 `TestRemoteMirrorReaderDTOMutationDoesNotPolluteCache`。
- 缓存写入不新增路径：客户端全部写入经 `Publish` / `DeleteAtVersion` / `LoadAuthoritative`（→ `admitLocked`），见 [REM-1](#rem-1)。

**4. 控制流**（`SnapshotClient.ReadSnapshot`）

1. 记读指标（defer）。
2. 已停 → `ErrSnapshotClientStopped`；Linearizable 而 loader 未声明 → `ErrRemoteReadUnsupported`。
3. 续租兴趣（失败忽略，O4 计数在 `renewInterest`）。
4. `cache.Read`：Linearizable → `loadAuthoritative`（不合并）；否则 `readConfirmed`；满足 `covers` 交出；Cached 不满足 → `ErrRemoteSnapshotStale` / 未命中 → 未找到；Monotonic 不满足或未命中 → `loadMonotonic` 按 `(key, After)` 合并回源一次。
5. `loadAuthoritative` 对返回值再做过期与 `covers` 检查（NC-36、RR-20260913-08）。

```mermaid
sequenceDiagram
  participant B as 业务 / RemoteMirrorReader
  participant C as SnapshotClient
  participant K as RemoteSnapshotCache.Read
  participant L as L2 / 权威 loader
  B->>C: ReadSnapshot(key, consistency, after)
  C->>C: stopped? 能力门
  C->>C: RenewInterest（忽略失败）
  C->>K: Read
  alt Linearizable
    K->>L: loadAuthoritative（gatedLoader）
  else Cached / Monotonic
    K->>K: readConfirmed（必要时 refresh）
    alt 满足 After
      K-->>C: 快照
    else Cached
      K-->>C: ErrRemoteSnapshotStale 或未找到
    else Monotonic
      K->>L: coalesce(key, After) 回源一次
    end
  end
  C-->>B: 快照（reader 再核对 key / schema / codec，解码副本）
```

**5. 失败与不确定结果**

| 情形 | 处理 | 对调用方 |
| --- | --- | --- |
| 客户端已停 | 不访问 L2 / 权威 / 总线 | `ErrSnapshotClientStopped` |
| Linearizable 未声明 | 不加载 | `ErrRemoteReadUnsupported` |
| Cached + After 不满足 | 不回源 | `ErrRemoteSnapshotStale` |
| token 与快照 epoch 混合 | 不交出 | `ErrRemoteObservationIncomparable` |
| Monotonic 回源后仍不满足 | 回源一次 | `ErrRemoteSnapshotStale` |
| 合并等待者超过 `snapshot_max_waiters` | 拒绝 | `ErrRemoteOverloaded` |
| Stop 时 loader 不响应取消 | 不杀 | Stop 返回 ctx 错误，客户端保持“停止中”，可再 Stop |
| reader 读到别的 key / schema / codec | 不解码 | 错误（`ErrRemoteSnapshotSchemaMismatch` 等） |

**6. 测试**

| 用例 | 文件 | 覆盖 |
| --- | --- | --- |
| `TestRemoteObservationCoversFollowsAdmissionOrder` | `entity/remote_mirror_promises_test.go` | token 排序与准入同规则 |
| `TestRemoteMirrorReaderDTOMutationDoesNotPolluteCache` | 同上 | DTO 独立副本 |
| `TestRemoteMirrorReaderNeedsNoRegistrationBesideTheOwner` | 同上 | 同进程无冲突注册 |
| `TestRemoteMirrorReaderRejectsForeignIdentityAndSchema` | 同上 | 读侧身份 / schema / codec |
| `TestRemoteSnapshotReadExitsShareOnePostCondition` | 同上 | 三个出口 × 同版本刷新 / 跨 epoch token + Cached 最低要求 |
| `TestReadRemoteSnapshotMonotonicMissLoadsAuthorityOnce` / `…BelowMinimumLoadsAuthorityOnce` | `remoteentity/snapshot_read_exit_promises_test.go` | 一次回源 |
| `TestSnapshotClientHasNoWriteCapability` 等 6 条 | `remoteentity/snapshot_client_promises_test.go` | 写能力、同进程读、线性化门、启动回收、停机契约、停机取消 |

修前红（原样，出处 [MIRROR-STEPS-1-3 §4](../../feature/MIRROR-STEPS-1-3-2026-10-06.md)）：

```text
# Manager 外层回退：一次 Monotonic 读回源两次
--- FAIL: TestReadRemoteSnapshotMonotonicMissLoadsAuthorityOnce
    one Monotonic miss loaded the authority 2 times, want 1 (the reader's fallback reloaded outside the coalesced load)
--- FAIL: TestReadRemoteSnapshotMonotonicBelowMinimumLoadsAuthorityOnce
    one Monotonic read below the minimum loaded the authority 2 times, want 1

# Cached 读交出低于最低版本的快照（基线 remote_snapshot.go 上用只走旧 API 的同场景用例）
--- FAIL: TestRedCachedReadBelowMinimum
    Cached read with minVersion=5 returned version=3 found=true err=<nil>; want ErrRemoteSnapshotStale, not a value below the minimum
```

（`TestRedCachedReadBelowMinimum` 是只在基线上跑的红用例，未入库。）负对照 N1～N8（原样节选，同出处）：

```text
N1 去掉启动失败时退订快照主题
--- FAIL: TestSnapshotClientStartFailureLeavesNoSubscription
    a failed Start left 1 snapshot subscription(s)
N2 Stop 不等依赖调用排空（去掉 c.work.Wait）
--- FAIL: TestSnapshotClientStopContract
    stopcontract: first Stop with work in flight = <nil>, want the ctx error (the stop must not report a drain that did not happen)
N3 给客户端加导出的 PublishRemoteSnapshot / DeleteRemoteSnapshot
--- FAIL: TestSnapshotClientHasNoWriteCapability
    SnapshotClient implements entity.IRemoteSnapshotPublisher: a read-only client must not carry write capability
N4 去掉 Linearizable 能力门
--- FAIL: TestSnapshotClientLinearizableNeedsADeclaredLoader
    Linearizable without a declared loader: found=true err=<nil> loads=1; want ErrRemoteReadUnsupported without loading
N5 解码器拿缓存底层切片（不复制）
--- FAIL: TestRemoteMirrorReaderDTOMutationDoesNotPolluteCache
    after the caller rewrote its DTO, the next read returned "XXXXXXXXXX" found=true err=<nil>; the cache was polluted
N7 token 只比版本
--- FAIL: TestRemoteObservationCoversFollowsAdmissionOrder（newer route / marker epoch、older route epoch、mixed 四格）
N8 带 epoch 的 token 也把版本下推给 loader
--- FAIL: TestRemoteSnapshotReadExitsShareOnePostCondition/token_across_an_epoch_change
    Read/Monotonic: a token from before the takeover returned route=0 version=0 found=false err=<nil>; the newer epoch satisfies it
```

修后：上表用例全部通过；`go test -race -count=3 ./entity ./remoteentity ./kit/remoteentity ./cache` 全过；真实依赖 `TestRealB2WatermarkMatrix*`、`TestRealJetStreamReplayAfterL2ExpiryDoesNotResurrect` 等与生成工程 12 条 `TestGeneratedRemote*` 通过（记录 §5，未保留逐行原文）。

**7. 性能证据**：未测（记录 §6：“本批没有做压测（维护者要求节省额度）”；读命中路径多一次原子读、每次 L2 调用多一次 `Lifetime` Begin / End）。第 6 步对照 v1.20.2 的结果见 [REM-8](#rem-8)。

**8. 未验证项与已知风险**

- owner 的提交后总线发布不经客户端准入（停止后仍可发布），属 K3。
- `RemoteSnapshotRead.After` 的注释（`entity/remote_mirror.go:78`）写“Cached 读……不满足就是未找到”，源码 `Read` 对 Cached 不满足返回 `ErrRemoteSnapshotStale`（`entity/remote_snapshot.go:673`～`:674`）；以源码为准，注释过时。

**9. review 检查点**

- [ ] 确认仓内不再有绕过 `RemoteSnapshotCache.Read` 的读：grep `LoadAuthoritative(`、`.Get(ctx, key, consistency` 的调用方，确认只剩 `SnapshotReplicaStore.ApplyReplica` 的缺基回填（`remoteentity/syncer.go:139`）与外观本身。
- [ ] 确认 `loadAuthoritative` 对返回值的最终检查（`entity/remote_snapshot.go:419`～`:432`）覆盖 Linearizable 出口（`Read` `:662` 直接返回它）。
- [ ] 确认 `gatedSnapshotL2` 的四个方法（`remoteentity/snapshot_client.go:578`～`:612`）都先 `work.Begin()`，且 `Stop` 中 `work.Stop()` 先于 `stopCancel()`（`:535`～`:536`）。
- [ ] 确认 `Covers` 的“两个 epoch 都 ≥ 且至少一个更新”分支（`entity/remote_mirror.go:63`）与 L2 CAS 脚本的“marker 或 route 任一更小即拒”（`remoteentity/snapshot_l2.go` CAS 脚本）对混合 epoch 的处理一致。
- [x] 修正或登记 `entity/remote_mirror.go:78` 注释与源码的不一致。（fixr：已按源码改正。）

<a id="rem-3"></a>
### REM-3 allow_stale 的 Cached Remote 访问接受低于 min_version 的快照（发版前复审修复）

> 首发 v1.21.0 · [说明](guide-saga-drv-dao-rem.md#rem-3)

**1. 提交**

| 提交 | 版本 | 内容 |
| --- | --- | --- |
| `207163f9` | v1.21.0 | `nest/remote_access.go` 对 AllowStale + Cached 不下推最低版本；回归用例；CHANGELOG；MIRROR-STEPS-1-3 §6 更正 |
| `8016580b` | v1.21.0 | DECISIONS-PENDING 登记发版前审查与跟进的实施状态 |

**2. 改动文件与关键符号**

| `path:line` | 符号 | 职责 |
| --- | --- | --- |
| `nest/remote_access.go:127` | `resolveRemoteSnapshot` 中 `minVersion` | `AllowStale && consistency == RemoteReadCached` 时置 0（`:128`～`:131`） |
| `nest/remote_access.go:33` | `RemoteAccess.AllowStale` | 生成器标签 `allow_stale` |
| `nest/remote_cached_allow_stale_promises_test.go:35` | `TestCachedRemoteAccessWithAllowStaleStillAcceptsAnOlderSnapshot` | 回归 |

**3. 不变量与强制点**：AllowStale 的 Cached 访问由 `RemoteSnapshot.Accepts` 判定版本（单点：`nest/remote_access.go:128`）；不带 AllowStale 的 Cached 访问仍由读出口拒绝；Cached 不回源。守卫：上表用例（同一用例里有“不带 allow_stale 被拒”与“权威加载次数仍为 1”的对照断言）。

**4. 控制流**：1）nest 计算 `consistency`；2）AllowStale + Cached → `minVersion = 0`；3）`manager.ReadRemoteSnapshot(..., minVersion)`；4）返回的快照交 `Accepts`（按 AllowStale 放行低版本）。

**5. 失败与不确定结果**

| 情形 | 处理 | 对调用方 |
| --- | --- | --- |
| AllowStale + Cached，L1 低于 MinVersion | 读出口交出低版本，`Accepts` 放行 | 拿到旧快照 |
| 无 AllowStale + Cached，L1 低于 MinVersion | 读出口拒绝 | `remote snapshot stale`，`required` 访问整笔被拒 |
| Monotonic / Linearizable | 不变 | 不变 |

**6. 测试**：`TestCachedRemoteAccessWithAllowStaleStillAcceptsAnOlderSnapshot`（`nest/remote_cached_allow_stale_promises_test.go:35`）：真实 `SnapshotClient`，L1 先持有 v3；AllowStale + `min_version=5` 被接受；不带 AllowStale 被拒；权威加载次数为 1（Cached 不回源）。

修前红（原样，出处提交 `207163f9` 说明）：

```text
红（5505db4b，真实 SnapshotClient，L1 持有版本 3）：
  cached access with allow_stale and min_version=5 over a cached version 3:
  nest: remote access guild: remote snapshot stale; want the older snapshot accepted
绿：同一用例通过，不带 allow_stale 的对照仍被拒、Cached 不回源。
```

**7. 性能证据**：无（不涉及热路径）。

**8. 未验证项与已知风险**：发版前审查报告未入库；只覆盖 nest 一个调用方，其他直接用 `Cached + minVersion` 的调用方（若有）仍按新契约得到 `ErrRemoteSnapshotStale`。

**9. review 检查点**

- [ ] 确认 `nest/remote_access.go:128` 的条件只在 Cached 时生效，Monotonic + AllowStale 仍把下限交给读出口（会回源）。
- [ ] grep 仓内其他 `ReadRemoteSnapshot(` / `ReadSnapshot(` 调用方，确认没有别处依赖“Cached 交出低于最低版本的值”。
- [ ] 确认 `RemoteSnapshot.Accepts` 对 AllowStale 的判定与生成器 `allow_stale` 标签一一对应（看 `nest/remote_access.go:171` 透传）。
- [ ] 确认回归用例里“不带 allow_stale”对照断言的是错误而不是未找到。

<a id="rem-4"></a>
### REM-4 Mirror 第 4 步：可确认订阅、首载缓冲、兴趣代际与撤销水位

> 首发 v1.21.0 · [说明](guide-saga-drv-dao-rem.md#rem-4)

**1. 提交**

| 提交 | 版本 | 内容 |
| --- | --- | --- |
| `23e17d81` | v1.21.0 | `ILiveSubscriber` / JetStream `SubscribeLive` / `mirror.NewLive`；`ApplyReplica` 首载缓冲；兴趣代际锁内分配与撤销水位；O4（见 [REM-5](#rem-5)）；用例与真实环境用例 |
| `c99b59f6` | v1.21.0 | DECISIONS-PENDING 第九轮标为已实施 |

**2. 改动文件与关键符号**

| `path:line` | 符号 | 职责 |
| --- | --- | --- |
| `sync/syncbus/sync.go:49` | `ILiveSubscriber` | 可确认订阅能力（返回 nil 即确认） |
| `sync/syncbus/driver/jetstream.go:195` | `jetStreamSyncBus.SubscribeLive` | DeliverNew durable |
| `sync/syncbus/driver/jetstream.go:220` | `subscribe` 的 live 分支 | fanout 键 `topic+"\x00live"`、durable 主题 `topic+".live"`、`JetStreamDeliverNew` |
| `sync/syncbus/mirror/envelope.go:39` | `ErrLiveSubscribeUnsupported` | 总线不支持可确认订阅 |
| `sync/syncbus/mirror/envelope.go:65` | `NewLive` | 用 `SubscribeLive` 的复制器 |
| `sync/syncbus/mirror/envelope.go:72` | `Replicator.Live` | 是否 live |
| `entity/remote_snapshot.go:124` | `RemoteSnapshotCacheConfig.ReplicaBuffer` | 首载缓冲条数（缺省 64，`:246`、`:280`） |
| `entity/remote_snapshot.go:183` | `RemoteSnapshotReplica` | 一条复制消息（更新或删除） |
| `entity/remote_snapshot.go:392` | `loadAuthoritative` | `beginBootstrap` → `fetchAndAdmit` → `endBootstrap` → 溢出再回源 → `replayReplicas` |
| `entity/remote_snapshot.go:479` | `ApplyReplica` | 复制消息唯一入口 |
| `entity/remote_snapshot.go:510` / `:522` | `beginBootstrap` / `endBootstrap` | 登记 / 结束一次在途加载，最后一个取走缓冲 |
| `entity/remote_snapshot.go:539` | `bufferDuringBootstrap` | 进缓冲；满则清空并标记溢出、Warn、计数 |
| `entity/remote_snapshot.go:564` | `replayReplicas` | 按到达顺序重放，失败只计数 |
| `entity/remote_snapshot.go:576` | `BootstrapStats` | 累计计数 |
| `remoteentity/config.go:37` | `Config.SnapshotReplicaBuffer` | core 配置（无 kit 键） |
| `remoteentity/syncer.go:137` | `SnapshotReplicaStore.ApplyReplica` | 改走 `cache.ApplyReplica`；不在首载时的缺基回源（`:138`～`:140`） |
| `remoteentity/snapshot_client.go:450` | `SnapshotClient.Start` | 按 `bus.(ILiveSubscriber)` 选推送 / 退化（`:466`），Warn（`:495`），gauge（`:493`） |
| `remoteentity/snapshot_client.go:425` | `bindLocked` | live 时快照复制器用 `mirror.NewLive` |
| `remoteentity/snapshot_client.go:256` | `renewInterest` | generation 在条带锁内分配（`:268`） |
| `remoteentity/snapshot_client.go:310` | `ReleaseInterest` | generation 在条带锁内分配（`:317`） |
| `remoteentity/interest.go:40` | `interestLease.released` | 撤销水位 |
| `remoteentity/interest.go:134` | `renewIfNeeded` 撤销水位判定 | 不新于水位的 renew 被忽略 |
| `remoteentity/interest.go:218` | `release` | `g>0` 留水位（`:234`）；`g==0` 或表满放不下时只撤销（`:229`） |
| `remoteentity/interest.go:238` | `drop` | 本机回滚 / 清理，不留水位 |
| `remoteentity/assembly.go:18` / `:20` | `Stats.SnapshotPush` / `InterestRefused` | Manager 统计 |

**3. 不变量与强制点**

- 推送只在可确认订阅上开：`SnapshotClient.Start` 的类型断言（`remoteentity/snapshot_client.go:466`）与 `mirror.Replicator.Start` 的 live 分支（`sync/syncbus/mirror/envelope.go:87`～`:92`）双重强制。守卫：`TestSnapshotClientWithoutConfirmedSubscriptionsReadsOnDemand`、`TestJetStreamSubscribeLiveUsesASeparateDeliverNewDurable`。
- 首载期间的复制消息不与加载结果交错写入：`bufferDuringBootstrap` 在 `bootMu` 下判定（`entity/remote_snapshot.go:539`）；`bootMu` 持有期间不调用 L2 / loader（注释 `:174`）。重放与加载结果都经 `admitLocked`。守卫：`TestSnapshotBootstrapBuffersDeltaDuringFirstLoad`、`TestSnapshotBootstrapReplayMatrix`、`TestSnapshotBootstrapOverflowDropsTheBufferAndReloads`。
- 溢出后再回源一次：`loadAuthoritative` `:411`～`:414`（条件 `overflowed && err == nil`）。守卫：`TestSnapshotBootstrapOverflowDropsTheBufferAndReloads`（负对照：去掉再回源 → `version=5 loads=1 … Reloads:0`）。
- 兴趣代际锁内分配：`renewInterest` 在 `stripe.Lock()`（`:262`）之后才 `nextInterestGeneration()`（`:268`）；`ReleaseInterest` 同（`:315`、`:317`）。守卫：`TestInterestRenewReleaseConvergesInEveryDeliveryOrder`（覆盖线上乱序投递；**没有并发 renew / release 的竞态用例**直接钉住“锁内分配”）。
- release 撤销水位：`remoteentity/interest.go:134`、`:234`。守卫：同上，以及改了断言的 `TestStaleInterestReleaseDoesNotCancelANewerRenewal`（`remoteentity/interest_generation_promises_test.go:28`）。

**4. 控制流**

首载缓冲：

```mermaid
stateDiagram-v2
  [*] --> Idle
  Idle --> Loading: loadAuthoritative 取得加载名额，beginBootstrap(loads++)
  Loading --> Loading: ApplyReplica 到达，缓冲未满，append
  Loading --> Overflowed: 缓冲已满，清空并标记 overflowed，Warn + 计数
  Overflowed --> Overflowed: 之后到达的消息直接丢弃（不计数）
  Loading --> Draining: endBootstrap，最后一个加载取走缓冲
  Overflowed --> Reload: endBootstrap，overflowed 且首次加载无错
  Reload --> Draining: fetchAndAdmit 再回源一次（reloads++）
  Draining --> Idle: replayReplicas 按到达顺序经 admitLocked 重放
  Idle --> Idle: 无在途加载时 ApplyReplica 直接 applyReplica
```

订阅模式与兴趣代际：

```mermaid
stateDiagram-v2
  state "Start(bus)" as S
  [*] --> S
  S --> Push: bus 实现 ILiveSubscriber，snapshotRep = NewLive，订阅快照 / 兴趣 / 续租请求
  S --> OnDemand: 普通总线，不订阅快照主题，Warn，PushEnabled=false，gauge=0
  state "兴趣表条目（每 consumer × key）" as I {
    [*] --> Leased: renew(g)
    Leased --> Leased: renew(g2 >= g) 延长
    Leased --> Released: release(g3 >= g)，留水位，存活 interest TTL
    Released --> Released: renew(g4 <= g3) 迟到，忽略
    Released --> Leased: renew(g5 > g3)
    Released --> [*]: 水位过期
    Leased --> [*]: 租约过期 / drop
  }
```

**5. 失败与不确定结果**

| 情形 | 处理 | 对调用方 |
| --- | --- | --- |
| 普通 NATS | 不订阅快照推送 | 读取在陈旧上限后回源；启动 Warn |
| `NewLive` 复制器在不支持的总线上 Start | 返回 `ErrLiveSubscribeUnsupported` | Start 失败（`SnapshotClient` 先做类型断言，正常不会走到） |
| 缓冲溢出 | 丢缓冲，加载装入后整体再回源一次 | 读者拿到第二次加载后的值 |
| 缓冲溢出但首次加载出错 | 不再回源（`err != nil`） | 读者拿到加载错误 |
| 重放失败（缺基、L2 冲突） | 只计数 | 新鲜度交给陈旧上限 |
| 多个并发加载（不同合并键）同一 key | 只有最后结束的加载取走缓冲 / 溢出标记 | 先结束的加载返回时缓冲尚未重放（推断，按 `endBootstrap` `:530` 源码） |
| 表满放不下撤销水位 | 只撤销，不留水位（`remoteentity/interest.go:229`） | 迟到的旧 renew 会复活租约；已修复（RR-20261006-11）：表满时改记溢出水位，不再复活 |

**6. 测试**

| 用例 | 文件 | 覆盖 |
| --- | --- | --- |
| `TestSnapshotBootstrapBuffersDeltaDuringFirstLoad` | `remoteentity/mirror_step4_promises_test.go:74` | 加载在途的增量缓冲后重放，权威只读一次 |
| `TestInterestRenewReleaseConvergesInEveryDeliveryOrder` | 同上 `:114` | renew / release 全排列收敛到最后一次操作 |
| `TestSnapshotBootstrapReplayMatrix` | 同上 `:226` | 旧 upsert / 旧删除 / 同版本删除 / 更新的删除 / 删除后重建 / 重建先于删除 / 新旧 upsert 乱序 |
| `TestSnapshotBootstrapOverflowDropsTheBufferAndReloads` | 同上 `:292` | 溢出丢缓冲并再回源 |
| `TestSnapshotClientWithoutConfirmedSubscriptionsReadsOnDemand` | 同上 `:395` | 退化：不订阅、Warn、`PushEnabled=false`，经 L2 读到 owner 的新版本 |
| `TestJetStreamSubscribeLiveUsesASeparateDeliverNewDurable` | `sync/syncbus/driver/jetstream_live_promises_test.go:17` | DeliverNew durable 与 DeliverAll 分开 |
| `TestRealJetStreamLiveSubscriptionConfirmsAndResumes` | `remoteentity/mirror_step4_jetstream_integration_test.go` | 真实 JetStream：确认前不投、确认后投、退订期间的续投 |
| `TestRealJetStreamLiveSnapshotPushReachesTheReader` | 同上 | 真实 JetStream + Redis：推送到达只读方 |

修前红（原样，出处 [MIRROR-STEP-4 §6.2](../../feature/MIRROR-STEP-4-AND-O4-2026-10-06.md)）：

```text
--- FAIL: TestSnapshotBootstrapBuffersDeltaDuringFirstLoad
    after the first load: version=5 payload="base" found=true err=<nil> authority loads=2 replica err=remote snapshot stale; want v6 "base+delta" from one load (the delta buffered and replayed on top of the load)
--- FAIL: TestInterestRenewReleaseConvergesInEveryDeliveryOrder
    renew then release delivered in order [1 0]: interested=true, want false (the last operation issued decides)
    renew renew release delivered in order [2 1 0] / [1 2 0] / [2 0 1] / [0 2 1]: interested=true, want false
    release renew release delivered in order [2 1 0] / [2 0 1] / [0 2 1]: interested=true, want false
```

负对照（记录原文）：去掉首载缓冲（`bufferDuringBootstrap` 不生效）→ 第一条与 `TestSnapshotBootstrapReplayMatrix` 的计数断言红；去掉溢出后的再回源 → `TestSnapshotBootstrapOverflowDropsTheBufferAndReloads` 红（`version=5 loads=1 … Reloads:0`）。

真实环境（原样摘要，出处同 §6.3）：`TestRealJetStreamLiveSubscriptionConfirmsAndResumes` 一次 2.05s（退订时被停掉的拉取请求已交出 3、未确认，AckWait 2s 后重投），另一次 0.05s；`TestRealB2WatermarkMatrixStandalone`（4.04s）、`…Cluster`（6.43s）等全部 PASS；`scripts/test-remote-generated.sh` 12 条 PASS。

改了断言的既有用例：`TestStaleInterestReleaseDoesNotCancelANewerRenewal`（原把“release(13) 之后迟到的 renew(12) 重建租约”当前提，改为断言不复活）、`TestRemoteInterestRegistryHasHardCapacityLimits`、四个 Assembly 生命周期用例的测试总线补 `SubscribeLive`。

**7. 性能证据**：本批未压测（记录 §6.6：首载缓冲只在权威加载在途时取一次互斥锁，每条复制消息一次 `bootMu`）。第 6 步对照见 [REM-8](#rem-8)（推送扇出无显著差别）。

**8. 未验证项与已知风险**

- 无 L2 装配里 L1 删除标记被 LRU 淘汰后旧 upsert 可复活。
- 订阅断开到重连之间漏掉的推送只影响新鲜度（上界 `cached_max_staleness`）；JetStream durable 续投依赖流保留期（MaxAge）。
- durable 名：记录（MIRROR-STEP-4 §4、USER_GUIDE）写 `sync_remote_entity_snapshot.live_<sid>_…`；源码 `durableSyncName` 经 `sanitizeSyncName` 把 `.` 换成 `_`（`sync/syncbus/driver/jetstream.go:427`、`:445`），实际可读部分应为 `sync_remote_entity_snapshot_live_<sid>_<hash>`（推断，未实跑核对服务端名字）。（已实跑核对，fixr：隔离 NATS 上服务端名字为 `sync_remote_entity_snapshot_live_<sid>_<hex16>`，用例 `TestRealJetStreamLiveDurableNameShape`；MIRROR-STEP-4 与 USER_GUIDE 已改。）
- 推送 / 退化在多节点 JetStream HA 与 Linux 网络下：E06 / E01。

**9. review 检查点**

- [ ] 确认兴趣代际在锁内分配：`renewInterest`（`remoteentity/snapshot_client.go:262`→`:268`）与 `ReleaseInterest`（`:315`→`:317`）都在 `stripe.Lock()` 之后取 generation；评估是否需要补一个并发 renew / release 的竞态用例（现有守卫只覆盖线上乱序）。
- [ ] 确认首载缓冲溢出后再回源一次：`entity/remote_snapshot.go:411`～`:414`；检查首次加载出错时不回源是否符合“溢出有明确行为”的决定，并看 `TestSnapshotBootstrapOverflowDropsTheBufferAndReloads` 是否覆盖出错分支。
- [ ] 确认缓冲重放与溢出回源都只经 `applyReplica` → `ApplyUpdate` / `DeleteAtVersion` / `Delete` 与 `fetchAndAdmit` → `publishLocked`，没有直接写 L1 的新路径（`entity/remote_snapshot.go:492`、`:564`）。
- [ ] 检查同一 key 多个并发加载（合并键 `(key, after, refresh)` 不同）时只有最后结束的加载重放缓冲（`endBootstrap` `:530`）：先返回的加载是否可能交出缺少缓冲增量的值，以及是否违反“读者看到的值不早于加载结果”。
- [x] 检查 `release` 在表满时不留水位（`remoteentity/interest.go:229`）是否会让迟到的旧 renew 复活租约，是否需要计数或日志。（fixr：会复活，登记并修复 RR-20261006-11，见 [RR-20261006-11 记录](../../bugfix/RR-20261006-11.md)。）
- [ ] 确认普通 NATS 退化时兴趣主题仍订阅、`refreshRep` 不建立（`remoteentity/snapshot_client.go:434`～`:437`、`:480`）。

<a id="rem-5"></a>
### REM-5 O4：兴趣容量按 consumer 配额

> 首发 v1.21.0 · [说明](guide-saga-drv-dao-rem.md#rem-5)

**1. 提交**

| 提交 | 版本 | 内容 |
| --- | --- | --- |
| `23e17d81` | v1.21.0 | `remoteInterestRegistry` 按 consumer 计、错误哨兵、指标与限频日志；kit 严格读取与 `frameworkIntKeys` 登记 |
| `c99b59f6` | v1.21.0 | DECISIONS-PENDING 第九轮 O4 标为已实施 |
| `fcc78ad0` | v1.23.0（本版） | 生成配置写出 `snapshot_interest_per_consumer: 0`（见 [REM-13](#rem-13)） |

**2. 改动文件与关键符号**

| `path:line` | 符号 | 职责 |
| --- | --- | --- |
| `remoteentity/interest.go:24` | `ErrInterestQuotaExceeded` | 包裹 `entity.ErrRemoteOverloaded` |
| `remoteentity/interest.go:28` | `ErrInterestRegistryFull` | 包裹 `entity.ErrRemoteOverloaded` |
| `remoteentity/interest.go:48` | `remoteInterestLimits` | PerConsumer / Total / ReleaseFence |
| `remoteentity/interest.go:58` | `interestQuotaShare = 16` | 缺省配额比例 |
| `remoteentity/interest.go:78` | `newRemoteInterestRegistry` | 缺省 `max(1, Total/16)` |
| `remoteentity/interest.go:139`～`:149` | `renewIfNeeded` 配额与表满判定 | 先按 consumer 配额，再按每节点上限；各自先 `pruneExpiredLocked` 再判 |
| `remoteentity/interest.go:156` | `rejectLocked` | 计数 + 每表每 10s 至多一条 Warn |
| `remoteentity/interest.go:168` | `setLocked` | 维护 `perConsumer`（不含撤销水位）与 `total` |
| `remoteentity/snapshot_client.go:290` | `renewInterest` 本机预判 | 本机兴趣表按同一配额判定，被拒不广播并回滚本机条目 |
| `remoteentity/snapshot_client.go:326` | `noteInterestRejected` | `interest_renew_refused_total{reason}` |
| `remoteentity/snapshot_client.go:126` | `validateSnapshotClientConfig` | 配额越界拒绝 |
| `remoteentity/config.go:34` | `Config.SnapshotInterestPerConsumer` | core 配置 |
| `kit/remoteentity/remote_entity_mod.go:358` | `readSnapshotConfig` | `snapshot_interest_per_consumer` 严格读取与范围 |
| `app/config_validation.go:324` | `frameworkIntKeys` | 登记 |

**3. 不变量与强制点**

- 一个 consumer 的拒绝只取决于它自己的租约数（`perConsumer[sid] >= quota`，`remoteentity/interest.go:142`）；撤销水位不计入配额（`setLocked` `:177`～`:182`）。
- consumer 本机与 owner 用同一份判定：本机兴趣表收到自己的全部续租（`renewInterest` 调 `c.interests.renew`，`remoteentity/snapshot_client.go:290`）。
- 读路径不因拒绝失败（`ReadSnapshot` 忽略 `RenewInterest` 错误，`remoteentity/snapshot_client.go:192`）。
- 守卫：`TestInterestCapacityIsPerConsumer`、`TestInterestQuotaRefusalIsVisibleAndReadsGoOnDemand`（`remoteentity/mirror_step4_promises_test.go:171`、`:340`）、`TestRemoteInterestRegistryHasHardCapacityLimits`（`remoteentity/interest_test.go`）、`TestInterestPerConsumerConfiguration`（`kit/remoteentity/interest_quota_config_test.go`）。

**4. 控制流**（`renewIfNeeded`）

1. 校验（consumer 非 0、key 合法、未过期）。
2. 已有撤销水位且过期 → 删掉当作不存在。
3. 已有租约：代际更旧 → 忽略；否则延长 / 更新代际。
4. 已有撤销水位且 renew 代际不新于它 → 忽略。
5. 新租约：配额满 → 先清过期再判，仍满 → `consumer_quota` 拒绝；表满（新条目）→ 先清过期再判，仍满 → `registry_full` 拒绝。
6. 写入并维护计数。

**5. 失败与不确定结果**

| 情形 | 处理 | 对调用方 |
| --- | --- | --- |
| consumer 配额满（本机） | 回滚本机条目，计 `interest_renew_refused_total{reason=registry}` | 读不失败；该 key 无推送，按需读取 |
| 本机兴趣表 key 数满 | 先清过期，仍满则拒绝，计 `{reason=local_table_full}` | 同上（`ErrRemoteOverloaded`） |
| owner 处配额满 / 表满 | `interest_rejected_total{reason}`、限频 Warn；`InterestReplicaStore.ApplyReplica` 返回错误 | 复制 handler 收到错误；JetStream 上是否 NAK 重投到 MaxDeliver 未核对（推断） |
| 配置 `per_consumer > subs` | kit `Init` 与 `NewSnapshotClient` 拒绝 | 启动失败 |

**6. 测试**

| 用例 | 文件 | 覆盖 |
| --- | --- | --- |
| `TestInterestCapacityIsPerConsumer` | `remoteentity/mirror_step4_promises_test.go:171` | A 占满不影响 B |
| `TestInterestQuotaRefusalIsVisibleAndReadsGoOnDemand` | 同上 `:340` | 超额 `ErrInterestQuotaExceeded`、计数、读取经权威 |
| `TestRemoteInterestRegistryHasHardCapacityLimits` | `remoteentity/interest_test.go` | 按 consumer 配额 + 每节点上限（改了断言） |
| `TestInterestPerConsumerConfiguration` | `kit/remoteentity/interest_quota_config_test.go` | 严格读取、范围、`ValidateServiceConfig` |

修前红（原样，出处 [MIRROR-STEP-4 §6.2](../../feature/MIRROR-STEP-4-AND-O4-2026-10-06.md)）：

```text
--- FAIL: TestInterestCapacityIsPerConsumer
    consumer B's interest was refused at the owner because consumer A filled the cluster-wide table; capacity must be counted per consumer
```

记录补充：修前 B 的读路径 `_ = c.RenewInterest(...)` 吞掉错误，没有计数。

**7. 性能证据**：未测。

**8. 未验证项与已知风险**

- 兴趣表是广播副本，两边收到的消息不同时判定可能不一致；总线丢兴趣消息由租约过期收敛。
- 记录写“缺省总上限的 1/16，即 16384”只对 core `DefaultConfig`（262144）成立；生成配置 `snapshot_interest_subs: 100000`（`codegen/internal/roost/catalog.go:71`）时缺省配额是 6250。（已补进 USER_GUIDE 与 MIRROR-STEP-4，fixr。）
- 满载容量的多主机验证：E13 / E15 / E16。

**9. review 检查点**

- [ ] 确认 `perConsumer` 计数在 `setLocked` / `removeLocked`（`remoteentity/interest.go:168`、`:190`）里对“租约 ↔ 撤销水位”互转维护正确（水位不计配额、租约变水位时减一）。
- [ ] 确认 kit 读取 `snapshot_interest_per_consumer` 写错类型时报错：`read.Int` 返回 0 不触发范围检查（`kit/remoteentity/remote_entity_mod.go:359`～`:362`），错误要靠 Init 末尾的 `read.Err()`（`:157`）；看 `TestInterestPerConsumerConfiguration` 是否有错类型用例。
- [ ] 确认 `InterestReplicaStore.ApplyReplica` 在 owner 拒绝时返回错误（`remoteentity/interest.go:315`～`:317`）对 JetStream 消费的影响（是否反复重投），以及兴趣主题当前用普通订阅（`remoteentity/snapshot_client.go:431`）。
- [ ] 确认 `rejectLocked` 的限频（`lastLogAt`）在锁内读写（`remoteentity/interest.go:158`）。

<a id="rem-6"></a>
### REM-6 Mirror 第 5 步：kit RemoteMirrorMod、codegen `//roost:mirror` DTO、公会摘要样例

> 首发 v1.21.0 · [说明](guide-saga-drv-dao-rem.md#rem-6)

**1. 提交**

| 提交 | 版本 | 内容 |
| --- | --- | --- |
| `a6985cf3` | v1.21.0 | kit `RemoteMirrorMod` / `MirrorSource` / `WithMirrorLoader`；`remoteentity.NewMongoSnapshotLoader`；`mods.ModRemoteMirror`；codegen `//roost:mirror` 与 `remote=mirror` 迁移诊断；`testdata/remoteflow` 两进程样例；`remote_entity.mirror.shutdown_timeout` 登记 |
| `43c60e82` | v1.21.0 | DECISIONS-PENDING Mirror 第 5 步标为已实施 |
| `4881f2b7` | v1.21.0 | 发版：生成器 Core 下限升到 v1.21.0（生成的只读产物用到 `entity.RemoteMirrorReader` 等） |
| `db67b8ee` | v1.23.0（本版） | `RemoteMirrorMod` 改用 `NewSnapshotL2StoreFromConfig`（O-M6-3 墓碑 WAIT，DRV 主题） |
| `fcc78ad0` | v1.23.0（本版） | 生成配置写出 `mirror.shutdown_timeout: 5s` |

**2. 改动文件与关键符号**

| `path:line` | 符号 | 职责 |
| --- | --- | --- |
| `kit/remoteentity/remote_mirror_mod.go:26` | `defaultMirrorShutdownTimeout` | 5s |
| `kit/remoteentity/remote_mirror_mod.go:36` | `RemoteMirrorMod` | 只读装配 |
| `kit/remoteentity/remote_mirror_mod.go:57` | `WithMirrorLoader` | 换权威 loader 与线性化声明 |
| `kit/remoteentity/remote_mirror_mod.go:75` | `MirrorSource` | 从注册表取 `entity.RemoteSnapshotReadOnly` |
| `kit/remoteentity/remote_mirror_mod.go:87` | `Init` | sid、快照段、`mirror.shutdown_timeout`（`:103`～`:112`）、`mongo.database` |
| `kit/remoteentity/remote_mirror_mod.go:123` | `DependsOn` | redis、syncbus；缺省 loader 时加 mongo |
| `kit/remoteentity/remote_mirror_mod.go:132` | `Provide` | L2（`:140`）、loader（`:150`）、`NewSnapshotClient`（`:152`）、登记只读接口（`:163`）、健康项（`:166`） |
| `kit/remoteentity/remote_mirror_mod.go:172` | `Start` | 取 syncbus 启动客户端，Info 日志 |
| `kit/remoteentity/remote_mirror_mod.go:191` | `StopBudget` | 停机预算 |
| `kit/remoteentity/remote_mirror_mod.go:213` | `StopWithContext` | 客户端三步停机 |
| `kit/remoteentity/remote_mirror_mod.go:231` | `checkHealth` | OK / Degraded（本机兴趣表满）/ Fail（停止后） |
| `kit/remoteentity/remote_entity_mod.go:198` | `RemoteEntityMod` 能力表 | 登记 `ModRemoteMirror = Manager.SnapshotClient()` |
| `kit/remoteentity/remote_entity_mod.go:289` | `readSnapshotConfig` | 两个 Mod 共用的快照段读取 |
| `kit/mods/name.go:38` | `ModRemoteMirror` | `"remote_entity.mirror"` |
| `remoteentity/mongo_committer.go:348` | `NewMongoSnapshotLoader` | 只读 loader，与 `MongoCommitter.LoadRemoteSnapshot` 共用 `loadMongoRemoteSnapshot`（`:358`） |
| `app/config_validation.go:287` | `frameworkDurationKeys` | 登记 `remote_entity.mirror.shutdown_timeout` |
| `codegen/internal/entity/mirror.go:35` | `mirrorMarkerRe` | `//roost:mirror` 解析 |
| `codegen/internal/entity/mirror.go:50` | `remoteMirrorMigration` | 迁移诊断文本 |
| `codegen/internal/entity/mirror.go:53` | `extractMirrors` | 找标记与 struct |
| `codegen/internal/entity/mirror.go:174` | `validateMirrorFields` | 拒绝 `dao:` / `comp:` 标签与嵌入字段 |
| `codegen/internal/entity/mirror.go:187` | `generateMirror` | 生成 `<dto>_gen_wire.go` |
| `codegen/internal/entity/mirror.go:228` | `mirrorTemplate` | spec / Decode / New…Reader 模板 |
| `codegen/internal/entity/parse.go:318` / `:321` | 解析期诊断 | `remote=mirror` / `lifetime=mirror_cache` |
| `codegen/internal/entity/gen.go:35` | 生成期诊断 | 同一文本 |
| `codegen/internal/entity/testdata/remoteflow/guild_summary.go` | `GuildSummary` DTO | 样例 |
| `codegen/internal/entity/testdata/remoteflow/mirror_test.go` | 两进程样例 | JetStream / NATS |

**3. 不变量与强制点**

- 只读服务的注册表里没有写能力：`Provide` 只登记 `entity.RemoteSnapshotReadOnly(client)`（`kit/remoteentity/remote_mirror_mod.go:163`），客户端本身无导出写方法。守卫：`TestRemoteMirrorModRegistersOnlyReadCapability`；样例子进程断言注册表无 `remote_entity` / `remote_entity.atomic_store` / `redis.versioned_lock`。
- 同一身份不开第二个客户端：`ModRemoteMirror` 由两个 Mod 之一登记，同进程冲突启动即失败。守卫：`TestRemoteMirrorModRefusesASecondClientBesideTheOwner`。
- 生成物无注册 / 写能力：模板只有 spec、Decode、reader（`codegen/internal/entity/mirror.go:228`）。守卫：`TestMirrorDTOGeneratesReadOnlyView`（与 fixture 逐字一致）。
- `remote=mirror` 不再生成 Entity：解析期（`codegen/internal/entity/parse.go:318`）与生成期（`codegen/internal/entity/gen.go:35`）两处。守卫：`TestRemoteMirrorEntityMarkerIsAMigrationError`。
- 停机：`stopcontract.Check` + `CallerReleases`。守卫：`TestRemoteMirrorModStopContract`、`TestRemoteMirrorModStopCancelsInFlightReads`。

**4. 控制流**（只读服务生命周期）

```mermaid
sequenceDiagram
  participant App
  participant M as RemoteMirrorMod
  participant C as SnapshotClient
  participant Bus as syncbus Mod
  App->>M: Init(cfg)：sid、快照段、mirror.shutdown_timeout、mongo.database
  App->>M: Provide(registry)：L2 = NewSnapshotL2StoreFromConfig，loader = 只读 Mongo 或 WithMirrorLoader
  M->>C: NewSnapshotClient(cfg, deps)
  M->>App: 登记 ModRemoteMirror（只读接口）+ 健康项 remote_mirror
  App->>M: Start()
  M->>Bus: Lookup ModSyncBus
  M->>C: Start(bus)：JetStream 开推送，普通 NATS 退化
  Note over App,C: 业务：New<DTO>Reader(MirrorSource(registry)).Read(...)
  App->>M: StopWithContext(ctx)（预算 = StopBudget）
  M->>C: Stop(ctx)：关准入、取消在途加载、退订、等排空
  C-->>M: nil 或 ctx 错误（保留，可再 Stop）
```

**5. 失败与不确定结果**

| 情形 | 处理 | 对调用方 |
| --- | --- | --- |
| sid 为 0 | Init 失败 | `remote_entity mirror mod: non-zero sid is required …` |
| `mirror.shutdown_timeout` 非正 / 错类型 | Init 失败 | 点名键 |
| 缺 redis / mongo / health / syncbus 能力 | Provide / Start 失败 | `required capability "…" not found` |
| 与 `RemoteEntityMod` 同进程 | 注册冲突 | 启动失败 |
| Linearizable 读，缺省 loader | 不加载 | `ErrRemoteReadUnsupported` |
| 停机超预算 | 返回 ctx 错误，Warn `stop incomplete` | 客户端保留，可再 Stop |
| 生成器遇 `remote=mirror` | 报迁移诊断 | `roost generate` 失败（T-274） |

**6. 测试**

| 用例 | 文件 | 覆盖 |
| --- | --- | --- |
| `TestRemoteMirrorModRegistersOnlyReadCapability` | `kit/remoteentity/remote_mirror_mod_promises_test.go` | 只读、换 loader 不依赖 mongo、未声明线性化 |
| `TestRemoteMirrorModRefusesASecondClientBesideTheOwner` | 同上 | 同进程冲突 |
| `TestRemoteMirrorModConfiguration` | 同上 | 快照段、新键、错类型 / 非正 / 越界 / hash tag 前缀、`ValidateServiceConfig` |
| `TestRemoteMirrorModStopContract` / `…StopCancelsInFlightReads` | 同上 | 停机契约、停止取消在途读 |
| `TestRemoteMirrorModHealthReportsPushMode` | 同上 | 健康信息 |
| `TestRemoteMirrorEntityMarkerIsAMigrationError` | `codegen/internal/entity/mirror_promises_test.go` | 迁移诊断 |
| `TestMirrorDTOGeneratesReadOnlyView` | 同上 | 生成物逐字、无写能力 |
| `TestMirrorDTOOnlyPackageIsDiscoveredAndRetired` / `TestMirrorMarkerValidation` | 同上 | 目录发现与孤儿清理、标记校验 |
| `TestGeneratedRemoteMirrorGuildSummary/{jetstream,nats}` + `TestGeneratedRemoteMirrorReaderProcess` | `codegen/internal/entity/testdata/remoteflow/mirror_test.go` | 两进程样例 |

新能力没有修前红；负对照（原样，出处 [MIRROR-STEP-5 §5.2](../../feature/MIRROR-STEP-5-2026-10-06.md)）：

```text
N1 去掉停止取消（gatedLoader 不再随 Stop 取消在途加载）
--- FAIL: TestRemoteMirrorModStopCancelsInFlightReads (2.00s)
    remote_mirror_mod_promises_test.go:273: Stop with a cancellable read in flight = context deadline exceeded; the in-flight load must be cancelled by the stop
N2 去掉只读限制（Mod 登记写 Manager 而不是只读客户端）
--- FAIL: TestRemoteMirrorModRegistersOnlyReadCapability
    the mirror capability *remoteentity.Manager is a remote entity manager
    the mirror capability is the write Manager
    Linearizable read through an undeclared loader = <nil>, want ErrRemoteReadUnsupported
N3 去掉 remote=mirror 迁移诊断（等于基线：旧形式被接受、生成可写 Entity）
--- FAIL: TestRemoteMirrorEntityMarkerIsAMigrationError
    parseDir accepted the source and produced 1 entities; want an error mentioning ["remote=mirror no longer generates an entity" "//roost:mirror entityKind="]
N4 生成物多出全局解码器注册
--- FAIL: TestMirrorDTOGeneratesReadOnlyView
    mirror view carries write / registration capability "MustRegisterRemoteSnapshotDecoder"
N5 样例：只读子进程调用 RegisterEntity（拿到 Guild 的 builder，能加载 / 写公会）
    mirror_test.go:493: the read-only process registered the Guild entity builder: it could load and write guilds
--- FAIL: TestGeneratedRemoteMirrorGuildSummary/jetstream
N6 样例：只读方在 JetStream 上不开推送
    mirror_test.go:336: read-only process push mode [push=false], want push=true
--- FAIL: TestGeneratedRemoteMirrorGuildSummary/jetstream
```

修后真实环境（原样，出处同 §5.4）：

```text
jetstream: MIRROR READY push=true / FIRST 1 alpha 1 / ISOLATED / SECOND 2 beta 2 35ms / STOPPED   PASS (1.67s)
nats:      MIRROR READY push=false / FIRST 1 alpha 1 / ISOLATED / SECOND 2 beta 2 3.007s / STOPPED PASS (4.14s)
```

改了断言的既有用例：`TestParseDirAcceptsEveryDocumentedMarkerForm`、`TestRemoteCapableMarkerIsRejectedWithTheCategoryReplacement`（原接受 `remote=mirror`，改为拒绝）。

**7. 性能证据**：本步未单独测；只读装配的读路径与 Manager 读同一个 `SnapshotClient`，第 6 步对照见 [REM-8](#rem-8)。

**8. 未验证项与已知风险**

- 生成的 spec 不带 `Tenant` / `Policy`（`codegen/internal/entity/mirror.go:235`～`:241` 的模板只设 Kind / Scope / Schema / Codec），多租户 / profile 视图要手写 spec（推断）。
- `RemoteMirrorMod` 的 `StopBudget` 不计入生成器 `shutdown.total_timeout`（RR-20260926-66）。
- owner 删除没有带版本的 Mongo 墓碑，防复活只靠 L2 墓碑。
- 健康语义两 Mod 不同：只读 Mod 本机兴趣表满报 Degraded（`kit/remoteentity/remote_mirror_mod.go:241`），`RemoteEntityMod` 同一条件报 Fail（`kit/remoteentity/remote_entity_mod.go:230`）。

**9. review 检查点**

- [ ] 确认 `MirrorSource` 返回的接口在 owner 进程里断言不到 `*remoteentity.Manager`（`kit/remoteentity/remote_entity_mod.go:198` 登记的是 `SnapshotClient()` 而非 Manager）。
- [ ] 确认 `readSnapshotConfig` 被两个 Mod 共用、`RemoteMirrorMod` 不读锁 / 提交 / finalizer 键（`kit/remoteentity/remote_mirror_mod.go:99`），并核对 Cluster 下无 hash tag 的 `lock_key` 不影响只读装配（`TestRemoteMirrorModConfiguration`）。
- [ ] 确认迁移诊断在解析期与生成期两处都触发（`codegen/internal/entity/parse.go:318`、`:321`，`codegen/internal/entity/gen.go:35`），`lifetime=mirror_cache` 与 `mirror-cache` 两种拼写（`codegen/internal/entity/parse.go:724`）都被拒。
- [ ] 确认生成的 `Schema` 与 owner 提交快照用同一个 `entity.RemoteSnapshotSchema(kind, RemoteSnapshotScope(coll))` 规则（`codegen/internal/entity/mirror.go:237`～`:238`），看 `TestMirrorDTOGeneratesReadOnlyView` 的“owner 提交仍用同一身份规则”断言。
- [ ] 评估两 Mod 对“本机兴趣表满”的健康状态（Degraded vs Fail）是否应统一。

<a id="rem-7"></a>
### REM-7 RR-20261006-01 删除提交确认时实例已被清空：确认视为完成、照常发布墓碑

> 首发 v1.22.0 · [说明](guide-saga-drv-dao-rem.md#rem-7)

**1. 提交**

| 提交 | 版本 | 内容 |
| --- | --- | --- |
| `4ca757aa` | v1.22.0 | `acknowledgeRemoteCommit` 身份不符摘掉并返回 nil；单元红绿用例；问题 / 修复记录；TROUBLESHOOTING T-276 |
| `b15e70c8` | v1.22.0 | 真实链路 S4a / S4b（见 [REM-8](#rem-8)） |
| `8d010147` | v1.22.0 | 文档：标为已实施 |

**2. 改动文件与关键符号**

| `path:line` | 符号 | 职责 |
| --- | --- | --- |
| `remoteentity/transaction_manager.go:996` | `Manager.acknowledgeRemoteCommit` | 推进内存实例；`live.ID() != commit.EntityID` 时 `detachEntity` 并返回 nil（`:1009`～`:1012`） |
| `remoteentity/transaction_manager.go:815` | `afterRemoteCommit` | 核对回执（`:816`）→ 确认（`:819`）→ `publishCommitted`（`:822`） |
| `remoteentity/snapshot_client.go:405` | `publishCommitted` 的 Invalidations 分支 | `DeleteAtVersion(key, NextVersion)` + 推送删除 |
| `remoteentity/remote_delete_ack_promises_test.go:57` | `TestRemoteDeleteCommitPublishesItsTombstoneAfterTheInstanceIsCleared` | 回归 |

**3. 不变量与强制点**：确认只推进“这个实体”的内存实例；实例已不是它时确认视为完成，发布不被跳过（单点：`remoteentity/transaction_manager.go:1009`）。回执校验仍在调用方 `validateRemoteReceipt`（`:816`），提交 / 发布 / 回执顺序不变。生成代码的身份核对不变。守卫：上表用例（先 `ClearBase` 再确认的顺序）。

**4. 控制流**

```mermaid
sequenceDiagram
  participant N as Nest 事务（strict）
  participant EM as 实体管理器
  participant P as WAL 投影器
  participant M as remoteentity.Manager
  participant C as SnapshotClient
  N->>EM: Destroy(..., true) → deferEntityDelete
  EM->>EM: 准入后清空实例（ID 归零）
  P->>M: afterRemoteCommit(commit, receipt)
  M->>M: validateRemoteReceipt
  M->>M: acknowledgeRemoteCommit
  alt 登记实例 ID != commit.EntityID（修后）
    M->>M: detachEntity，返回 nil
  end
  M->>C: publishCommitted：L1 / L2 DeleteAtVersion + 推送删除
  M->>M: notifyRemoteVersion
```

**5. 失败与不确定结果**

| 情形 | 处理 | 对调用方 |
| --- | --- | --- |
| 修前：实例已清空 | 生成代码身份核对拒绝，发布跳过 | `remote entity persistence outcome is indeterminate: Guild: remote acknowledgement identity mismatch` |
| 修后：实例已清空或已回收 | 摘登记，视为确认完成 | 成功；墓碑与推送删除发出 |
| 实例身份相符 | 原逻辑（版本向量、解冻） | 不变 |
| 回执过期（`remoteReceiptObsolete`） | 返回 nil | 不变 |

**6. 测试**

修前红（原样，出处 [修复记录](../../bugfix/RR-20261006-01.md)）：

```text
--- FAIL: TestRemoteDeleteCommitPublishesItsTombstoneAfterTheInstanceIsCleared (0.00s)
    remote_delete_ack_promises_test.go:93: the delete reached the authority but its acknowledgement failed: remote entity persistence outcome is indeterminate
        Guild: remote acknowledgement identity mismatch
```

真实链路探针原文（修前，出处 [问题记录](../../bug/RR-20261006-01.md)）：

```text
PROBE delete: [DELETEERR ... nest: finish remote write batch: remote entity persistence outcome is indeterminate: Guild: remote acknowledgement identity mismatch]
PROBE reader after 8s: [3]
PROBE ...:remote_entity:snapshot:0:238:4611686018525734841:1447779876:0: "... version\n3 ..."
PROBE mongo:            （两个集合都已空）
```

修后：同一用例通过——提交成功、发布一次 `DeleteRemoteSnapshot(key, NextVersion)`、缓存读到墓碑（修复记录“验证”）。真实链路 S4a / S4b：删除 26～36ms 到达只读方，切主后新起的只读方读到“不存在”，0 违例（MIRROR-STEP-6-LOCAL §3）。负对照：S4a 在修复之前“删除不发布，本场景红”（同 §3.1 表）。

**7. 性能证据**：无（不涉及热路径）。

**8. 未验证项与已知风险**：pipelined 带 Remote 批次未单独实测；Durability 0 与清空的先后未在生成链路核对；包内 `testRemoteEntity.AcknowledgeRemoteCommit` 不核对身份，所以既有删除用例当初没暴露问题（问题记录“根因”末段）。

**9. review 检查点**

- [ ] 确认 `remoteentity/transaction_manager.go:1009` 的身份判定位于 `remoteReceiptObsolete`（`:1002`）之后，且 `detachEntity(live)` 只摘这个实例、不影响同 ID 新实例的登记。
- [ ] 确认实例被回收给别的实体（ID 非 0 但不同）时同样走摘除分支，不会 `SetRemoteVersionVector` 到别人身上。
- [ ] 确认 `publishCommitted` 的删除用 `commit.NextVersion`（`remoteentity/snapshot_client.go:409`、`:413`）与 `afterRemoteCommit` 的 `notifyRemoteVersion` 一致。
- [ ] 评估是否需要生成工程上的 pipelined 删除端到端用例。

<a id="rem-8"></a>
### REM-8 Mirror 第 6 步本机替代：私有依赖进程上的两进程故障与 v1.20.2 对照基准

> 首发 v1.22.0 · [说明](guide-saga-drv-dao-rem.md#rem-8)

**1. 提交**

| 提交 | 版本 | 内容 |
| --- | --- | --- |
| `2a4c835d` | v1.22.0 | 下一轮规划（第 3 项 Mirror 第 6 步本机替代） |
| `b15e70c8` | v1.22.0 | `scripts/mirror-local.sh`、`testdata/remoteflow/mirror_local_test.go`（S1～S7）、`remoteentity/mirror_local_bench_integration_test.go`、记录 |
| `4ca757aa` | v1.22.0 | 场景中发现并修复 RR-20261006-01（[REM-7](#rem-7)） |
| `8d010147` | v1.22.0 | 补第二次完整运行结果，规划标为已实施 |
| `db67b8ee` | v1.23.0（本版） | 脚本加 `test-core`、`fault redis-cluster-stop-replica` / `redis-cluster-cont`、副本端口导出；S1 报 `commit_ms` 等 |
| `d483238e` | v1.23.0（本版） | S1 夹具传进程代际，去掉“先等旧锁过期”绕行 |
| `ba13cb05` | v1.23.0（本版） | `cluster_replicas_online` 就绪判定（[REM-12](#rem-12)） |

**2. 改动文件与关键符号**

| `path:line` | 符号 | 职责 |
| --- | --- | --- |
| `scripts/mirror-local.sh:1` | 头注释 | 用法与隔离规则 |
| `scripts/mirror-local.sh:27`～`:40` | 根目录 / 偏移校验 | 拒绝 `~/.roost-it`、偏移 0 / 1000 |
| `scripts/mirror-local.sh:283` 起 | `fault_action` | `nats-kill/stop/cont/start`、`mongo-stepdown/settle`、Redis 单机 / Cluster 切主与暂停 |
| `scripts/mirror-local.sh:436`～`:456` | 命令分派 | `up/status/down/clean/fault/test/test-core/bench` |
| `codegen/internal/entity/testdata/remoteflow/mirror_local_test.go:97` | `TestGeneratedRemoteMirrorLocal` | S1～S7 编排（`ROOST_MIRROR_LOCAL=1` 才运行） |
| `codegen/internal/entity/testdata/remoteflow/mirror_local_test.go:849` | `TestGeneratedRemoteMirrorLocalProcess` | 子进程（owner / 只读方） |
| `remoteentity/mirror_local_bench_integration_test.go:172` / `:238` | `BenchmarkMirrorLocalRead` / `…Push` | 读与推送扇出基准（integration tag） |

**3. 不变量与强制点**（测试判据，只读方进程内自查，MIRROR-STEP-6 §3.2）：不回退（版本不低于此前最大）；不复活（读到“不存在”后再读到存在即违例）；有界收敛（确认时刻 T 之后超过 `T + cached_max_staleness + 1.5s` 仍读到旧值即违例）。违例打印 `MIRROR VIOLATION …`，编排要求 0 条；S4a′ 切到只计数。环境隔离由脚本强制（拒绝共享根目录与偏移）。

**4. 控制流**：`test` = `environment_up`（NATS 3 节点、Mongo 3 节点副本集、Redis 单机 + 副本 + toxiproxy、Redis 3 主 3 从 Cluster）→ `run_generated_tests`（`scripts/test-remote-generated.sh` 用正式生成器生成工程，`-race`）→ `finish`（`clean`：SIGCONT 暂停进程、停全部进程、核对无残留、删根目录）。`bench` = 同一基准文件复制到基线 worktree，当前与基线交替各跑 `ROOST_MIRROR_LOCAL_COUNT` 次，benchstat。

**5. 失败与不确定结果**（场景结论，原样摘自 MIRROR-STEP-6 §3.1 表）

| # | 注入 | 结果与收敛时间 |
| --- | --- | --- |
| S1 | owner 第二笔写在投影前被拖住，owner SIGKILL；同 WAL、同 sid 重启 | 重启就绪 574ms；v2 在就绪后 1.9s 读到；v3 3.0s 读到（都是陈旧上限 3s 回源，见 O-M6-1）；0 违例 |
| S2a | 只读方连着的 nats-2 SIGKILL | 本次流 leader 不在 nats-2：owner 无感，提交 → 读到 31ms。开发期一次 leader 恰在 nats-2：owner 4 次尝试 6.1s 才写成（首错 `nats: no response from stream`，回复为结果未知，见 O-M6-2），只读方随即读到 |
| S2b | nats-3 SIGSTOP 8s 后 SIGCONT | 2.98s 读到（L2 重新确认）；SIGCONT 后推送 34ms。第二次完整运行流 leader 恰在 nats-3：owner 6 次尝试 10.6s 才写成（首错 `nest: sync canceled context deadline exceeded`），写成后 1.4s 只读方读到（陈旧上限回源），SIGCONT 后 55ms |
| S3 | 只读方只经 toxiproxy，陈旧上限 60s | 基线 30ms；latency 200ms → 239ms；全部代理断开 5s 期间提交 → 恢复后 269ms 读到（共 5.3s，DeliverNew durable 续投）；timeout 毒丢数据 3s → 共 4.1s；之后 33ms |
| S4a | Redis 单机 graceful 切主；提交；删除；再切主；新读者 | 切主后提交 40ms 读到；删除 24ms 读到“不存在”；第二次切主后新读者读到“不存在”；0 违例（修复 RR-20261006-01 之前删除不发布，本场景红） |
| S4a′ | 单机未复制即切主 | 新读者约 2.9s（145 次读）读到已删除的 v1，之后回源 Mongo 得“不存在”；原读者（L1 有墓碑）0 次读到（O-M6-3） |
| S4b | Cluster SIGKILL 该键主节点 | 切主后提交 33ms 读到；删除 38ms；新读者读到“不存在”；0 违例 |
| S5 | owner 每 100ms 提交 60 笔，期间 `replSetStepDown` | owner 60/60 成功、最大延迟 48ms；权威回源 164/164 成功；最后版本 v61 读到（推送先于 owner 回复到达，−8ms） |
| S6 | owner A 转交 B | B 就绪 268ms，v3 34ms 读到；A 的写 `writer fenced`；B 第二笔 27ms |
| S7 | 只读方 SIGKILL，期间 owner 提交 v3，同 sid 重启 | 首读 v3 59ms（经 L2）；重启后推送 30ms；首载缓冲 0 次 |

**6. 测试**：完整运行两次 PASS（`-race`；rebase 前 107.5s，rebase 到 `a26c9454` 之后 111.7s），每次核对“无残留进程”（记录 §6）。RR-20261006-01 的修前红见 [REM-7](#rem-7)。根包 `TestGlobalEnvironmentOperationsHoldTheAcceptanceLock` 起初因脚本注释提到隔离环境脚本名而误报，注释改写（记录 §6）。

**7. 性能证据**（原样，出处 MIRROR-STEP-6 §4；n=6 交替，`goos: darwin`、`cpu: Apple M5`）

| 基准 | 口径 | v1.20.2 | 当前（v1.21.0 + 本轮） | benchstat |
| --- | --- | --- | --- | --- |
| Read/L1Hit（已确认、陈旧上限内） | 平均 / p50 / p99 每次读 | 720.5ns / 667ns / 1041ns | 731.6ns / 708ns / 959ns | 平均 ~（p=0.18）；p50 +6.15%（p=0.002，约 41ns）；p99 ~ |
| Read/L2Fetch（L1 无、L2 有） | 平均 / p50 / p99 | 24.60µs / 23.50µs / 50.79µs | 24.97µs / 23.73µs / 52.60µs | 均 ~ |
| Read/Authority（L1 / L2 都无，Monotonic 回源 Mongo） | 平均 / p50 / p99 | 125.6µs / 120.4µs / 270.5µs | 128.4µs / 121.9µs / 286.5µs | 平均 ~（p=0.093）；p50 +1.28%（p=0.041）；p99 ~ |
| 回源次数 | 权威加载 / 每次读 | L1Hit 0、L2Fetch 0、Authority 1.000 | 同左 | 相等 |
| Push 扇出 1 读者 × 10 key | 一轮全部读到（平均 / p50 / p99） | 1.841ms / 1.777ms / 2.335ms | 1.852ms / 1.808ms / 2.721ms | 均 ~ |
| Push 扇出 10 读者 × 10 key | 同上 | 4.036ms / 4.017ms / 4.762ms | 4.063ms / 4.009ms / 5.090ms | 均 ~ |
| Push 扇出 100 读者 × 10 key | 同上 | 26.06ms / 25.89ms / 33.04ms | 25.42ms / 24.94ms / 35.26ms | 均 ~ |

结论：读延迟、回源次数、推送扇出与 v1.20.2 无可分辨差别；L1 命中 p50 多约 41ns（原因未逐项拆分）。macOS 绝对值不能外推到 Linux。

**8. 未验证项与已知风险**：E01、E02、E14、E15、E16（见说明）。原始输出不入库（记录 §7：scratchpad 的 `m6-artifacts`）。S2 的 owner 侧数字取决于流 leader 落在哪个节点，两次运行差很多。

**9. review 检查点**

- [ ] 确认 `scripts/mirror-local.sh` 的隔离校验（`:27`～`:40`）在 `ROOST_MIRROR_LOCAL_HOME` 是 `~/.roost-it` 的符号链接时也拒绝（用了 `pwd -P`）。
- [ ] 确认 `clean` 先 SIGCONT 被暂停的进程再停（否则 SIGSTOP 的进程停不掉），并核对无残留的判定覆盖 toxiproxy。
- [ ] 确认只读方“有界收敛”判据的 1.5s 余量与 `cached_max_staleness` 的关系在 `mirror_local_test.go` 里按配置值计算，而不是写死。
- [ ] 确认 S4a′ 切到只计数模式的条件只用于该边界场景，不会把其他场景的违例吞掉。
- [ ] 确认基准对照只用两个版本都有的入口（记录 §4：`Assemble` / `Assembly.Start` / `Manager.ReadRemoteSnapshot` / `afterRemoteCommit`）。

<a id="rem-9"></a>
### REM-9 O-M6-1 owner 启动广播“请重新续租兴趣”

> 首发 v1.23.0（本版） · [说明](guide-saga-drv-dao-rem.md#rem-9)

**1. 提交**

| 提交 | 版本 | 内容 |
| --- | --- | --- |
| `fa98f53f` | v1.23.0（本版） | 记录第十轮决定 |
| `db67b8ee` | v1.23.0（本版） | `remoteentity/interest_refresh.go`（新）、`renewInterest(refresh)` 唯一续租入口、`Start` 第三个订阅、`Assembly.Start` 发请求；用例。同一提交还有 O-M6-3（DRV 主题） |
| `d1d6d967` | v1.23.0（本版） | DECISIONS-PENDING 第十轮标为已实施 |

**2. 改动文件与关键符号**

| `path:line` | 符号 | 职责 |
| --- | --- | --- |
| `remoteentity/interest_refresh.go:26` | `SyncTopicInterestRefresh` | `"remote_entity_interest_refresh"` |
| `remoteentity/interest_refresh.go:30` | `interestRefreshMinGap` | 1s |
| `remoteentity/interest_refresh.go:38` | `remoteInterestRefreshWire` | `requester_sid`、`requested_at` |
| `remoteentity/interest_refresh.go:44` | `remoteInterestRefreshReplicaKey` | `int64(uint32(sid)) + 1` |
| `remoteentity/interest_refresh.go:49` | `requestInterestRefresh` | owner 发请求（只在 push 开着时），计 `interest_refresh_sent_total{result}` |
| `remoteentity/interest_refresh.go:81` | `InterestRefreshStore.ApplyReplica` | 身份核对、丢自己的（`:97`）与过期的（`:100`） |
| `remoteentity/interest_refresh.go:116` | `acceptInterestRefresh` | 单飞 / 合并 |
| `remoteentity/interest_refresh.go:135` | `runInterestRefresh` | 间隔、循环到无待办 |
| `remoteentity/interest_refresh.go:185` | `refreshInterestsOnce` | 遍历本机仍有效的 key，`renewInterest(ctx, key, true)` |
| `remoteentity/snapshot_client.go:256` | `renewInterest` | 唯一续租入口；refresh 时只续仍有效的 key（`:272`） |
| `remoteentity/snapshot_client.go:436` | `bindLocked` | push 时建 `refreshRep = mirror.NewLive(...)` |
| `remoteentity/snapshot_client.go:480` | `Start` | push 时启动第三个订阅，失败逐步回收 |
| `remoteentity/assemble.go:179` | `Assembly.Start` | `snapshots.Start` 之后、存储初始化之前调用 `requestInterestRefresh`，失败只 Warn |

**3. 不变量与强制点**

- 续租只有一个入口 `renewInterest`（`remoteentity/snapshot_client.go:256`）：代际锁内分配、本机按 O4 配额判定、撤销水位都不被绕过。守卫：`TestInterestRefreshRenewsOnlyLiveInterests`。
- 遍历不复活已 release / 已过期的 key：条带锁内重查本机表（`:272`）。守卫同上。
- 有界：同一时刻至多一个遍历，进行中的请求只置待办（`remoteentity/interest_refresh.go:118`），两次开始间隔 ≥ 1s（`:139`）。守卫：`TestInterestRefreshRequestsCoalesceAndAreValidated`。
- 停机：遍历持 `work` 准入（`:123`、`:136`），`Stop` 取消 `stopCtx`。守卫：`TestInterestRefreshGapWaitEndsOnStop`。
- 推送关着时不订阅、不发送（`requestInterestRefresh` `:50`；`Start` `:480`）。守卫：`TestInterestRefreshNeedsPushAndToleratesOldConsumers`。
- 订阅失败逐步回收。守卫：`TestSnapshotClientRefreshSubscriptionFailureLeavesNoSubscription`。

**4. 控制流**

```mermaid
sequenceDiagram
  participant O as owner Assembly.Start
  participant Bus as JetStream
  participant R as 只读方 SnapshotClient
  participant OI as owner 兴趣表
  O->>O: SnapshotClient.Start（兴趣订阅已确认）
  O->>Bus: requestInterestRefresh（requester_sid, requested_at）
  Bus->>R: InterestRefreshStore.ApplyReplica
  R->>R: 核对身份；丢 own / historic
  R->>R: acceptInterestRefresh（单飞；进行中只置 pending）
  loop 每个本机仍有效的 key
    R->>R: renewInterest(key, refresh=true)：锁内新代际、本机配额判定
    R->>Bus: 广播续租
    Bus->>OI: InterestReplicaStore.ApplyReplica → renew
  end
  Note over O,OI: owner 之后的提交按兴趣表推送
```

**5. 失败与不确定结果**

| 情形 | 处理 | 对调用方 |
| --- | --- | --- |
| owner 发送失败 | Warn，计 `interest_refresh_sent_total{result=error}` | 启动不失败；退化到原续租周期 |
| 请求身份不符 | 计 `invalid`，返回错误 | 复制 handler 收到错误 |
| 请求过期 / 自己发的 | 计 `historic` / `own`，忽略 | 无 |
| 遍历进行中再来请求 | 计 `coalesced`，结束后再做一次 | 无 |
| 客户端已停 | 计 `stopped`（源码有，记录未列） | 无 |
| 遍历中部分 key 续租失败 | Warn `interest refresh could not renew every interest …` | 这些 key 按常规续租收敛 |

**6. 测试**

| 用例 | 文件 | 覆盖 |
| --- | --- | --- |
| `TestOwnerRestartWithTheSameSidGetsInterestBackWithoutWaitingForRenewal` | `remoteentity/interest_refresh_promises_test.go` | 同 sid 重启后 `Assembly.Start` 返回时兴趣已恢复 |
| `TestInterestRefreshRenewsOnlyLiveInterests` | 同上 | 不复活已 release / 过期 key，用新代际 |
| `TestInterestRefreshRequestsCoalesceAndAreValidated` | 同上 | 合并、过期 / 自己的不触发、身份不符被拒 |
| `TestInterestRefreshGapWaitEndsOnStop` | 同上 | 间隔等待中 Stop 不挂住 |
| `TestInterestRefreshNeedsPushAndToleratesOldConsumers` | 同上 | 推送关着时不订阅不发送；无订阅者时 owner 照常启动 |
| `TestSnapshotClientRefreshSubscriptionFailureLeavesNoSubscription` | 同上 | 订阅失败回收 |

修前红（原样，出处 [MIRROR-M6-OBSERVATIONS §6.2](../../feature/MIRROR-M6-OBSERVATIONS-2026-10-06.md)；基线 `fa98f53f` 的 detached worktree，红用例文件未入库）：

```text
--- FAIL: TestOwnerRestartWithTheSameSidGetsInterestBackWithoutWaitingForRenewal (0.00s)
    interest_refresh_red_test.go:127: after the owner restarted with the same sid the reader still reads v1, want v2 pushed right away: the restarted owner's interest table is empty (interested=false) and the reader renews only past half its lease
```

修后：整条用例通过；既有 Assembly 生命周期用例的订阅数从 2 改为 3（`liveSubscriptions`）。

**7. 性能证据**（原样，出处同 §6.3；私有环境，同机，基线与本分支各起一套）

| 场景 | 指标 | 基线 `fa98f53f` | 本分支 |
| --- | --- | --- | --- |
| S1 owner 强杀、同 sid 重启 | 重放补发的 v2：重启就绪 → 只读方读到 | 1895ms（陈旧上限回源） | −24ms（推送先于就绪信号到达） |
| S1 | 重启后的写 v3：确认之后只读方还要等多久（`visible_after_confirm_ms`；写本身 45 / 46ms） | 2246ms（陈旧上限回源） | −5ms（推送先于回复） |
| S1 | 只读方计数 | errors=0、违例 0、权威回源 0 | errors=0、违例 0、权威回源 0 |
| S7 只读方强杀、同 sid 重启 | 首读 v3 / 重启后推送 | 71ms / 33ms | 75ms / 29ms |

**8. 未验证项与已知风险**

- 过期判定用接收方 `time.Now()` 减发送方 `requested_at`（`remoteentity/interest_refresh.go:100`），跨主机时钟偏差未验证（E02）。
- 记录 §2 列出的 `interest_refresh_requests_total` 结果值不含源码的 `stopped`（`remoteentity/interest_refresh.go:125`），也没提 `interest_refresh_sent_total`（`:73`）。（已补进 MIRROR-M6-OBSERVATIONS §2 与 T-278，fixr。）
- 多主机强杀重启：E13。

**9. review 检查点**

- [ ] 确认 `refreshInterestsOnce` 对每个 key 走 `renewInterest(ctx, key, true)` 而不是直接广播（`remoteentity/interest_refresh.go:201`），因而 O4 配额与撤销水位都生效。
- [ ] 确认 `renewInterest` 的 refresh 分支在条带锁内、`nextInterestGeneration()` 之后才判断“仍有效”（`remoteentity/snapshot_client.go:268`～`:274`）：被跳过的 key 也消耗了一个代际号，评估是否有副作用（推断无：代际只需单调）。
- [ ] 确认 `runInterestRefresh` 的间隔从 `refreshLastStart` 算起、首次遍历 `refreshLastStart` 为零值时不等待（`:139`）。
- [ ] 确认 `Assembly.Start` 中请求在 `SnapshotClient.Start` 之后（`remoteentity/assemble.go:167` → `:179`），且启动后续失败时 `unsubscribe` 退掉第三个订阅（`remoteentity/snapshot_client.go:506`）。
- [ ] 确认 `RemoteMirrorMod.Start` 不调 `requestInterestRefresh`（`kit/remoteentity/remote_mirror_mod.go:180`）。

<a id="rem-10"></a>
### REM-10 O-M6-6 同 sid 重启立即接管上一代进程留下的 Remote 实体锁

> 首发 v1.23.0（本版） · [说明](guide-saga-drv-dao-rem.md#rem-10)

**1. 提交**

| 提交 | 版本 | 内容 |
| --- | --- | --- |
| `e65f1cb6` | v1.23.0（本版） | 记录第十一轮决定 |
| `d483238e` | v1.23.0（本版） | `app.SingletonIncarnation` / `ModSingletonIncarnation`；`remoteentity.ProcessIncarnation`、`AssemblyDeps.Incarnation`、锁工厂带代际、取锁 Lua 接管分支、计数；kit 传入；用例；S1 夹具 |
| `8a292a5a` | v1.23.0（本版） | DECISIONS-PENDING 第十一轮标为已实施 |

**2. 改动文件与关键符号**

| `path:line` | 符号 | 职责 |
| --- | --- | --- |
| `app/singleton.go:80` | `SingletonIncarnation` | `{Key, Sid, Token}` |
| `app/singleton.go:90` | `ModSingletonIncarnation` | `"singleton_incarnation"`，只在 `singleton.enabled=true` 时登记 |
| `app/singleton.go:678` | `openSingleton` 登记 | token 取锁值第一段 |
| `kit/remoteentity/remote_entity_mod.go:182` | `Provide` | `Sid == localSid` 时传 `deps.Incarnation` |
| `remoteentity/assemble.go:28` | `AssemblyDeps.Incarnation` | 非 nil 时校验并装到锁工厂（`:113`～`:118`） |
| `remoteentity/versioned_lock.go:550` | `ProcessIncarnation` | `{Holder, Token}` |
| `remoteentity/versioned_lock.go:562` | `lockIncarnation` | `scope` / `self` 前缀 |
| `remoteentity/versioned_lock.go:568` | `newLockIncarnation` | 校验 sid、Holder、Token（1～64 位字母数字 `-` `_`）；摘要 = SHA-256 前 8 字节十六进制 |
| `remoteentity/versioned_lock.go:597` | `noteTakeover` | `remote_entity.lock_takeover_total` + 每 Assembly 一次 Info |
| `remoteentity/versioned_lock.go:633`～`:636` | `NewVersionedLock` | 代际前缀放在锁对象前缀之前 |
| `remoteentity/versioned_lock.go:138` | `TryLock` | ARGV[5] / ARGV[6] 交给 Lua；第 4 项回报为 1 时计数（`:151`～`:153`） |
| `remoteentity/versioned_lock_lua.go:16` | `versionedTryLockLua` | 接管判定（`:24`）与 owner 不存在 / 更早序号同一分支（`:27`） |

**3. 不变量与强制点**

- 只接管“同一单实例锁持有者的上一代”：owner 以 `scope` 开头且不以 `self` 开头（Lua `:24`）。原子：判定与换 owner 在一条 Lua 里。
- 接管 = TTL 过期后取锁：先 `INCR` fence 再写 owner（Lua `:29`～`:34`），fence 单调；随后 `GrantWrite(新 token)` 递增 Mongo `_grant_fence`，上一代许可作废。
- 只有持单实例锁的进程才传代际：kit 只在 `ModSingletonIncarnation` 存在且 sid 一致时传（`kit/remoteentity/remote_entity_mod.go:182`）。
- 守卫：`TestSameSidRestartTakesOverThePreviousIncarnationsSharedLock`、`TestLockTakeoverOnlyAppliesToThePreviousIncarnationOfTheSameSingletonHolder`、`TestAssembleRejectsAMalformedIncarnation`（`remoteentity/lock_takeover_promises_test.go`）、`TestSingletonIncarnationIsTheHeldLocksIdentity`（`app/singleton_incarnation_promises_test.go`）、`TestRemoteEntityModPassesTheSingletonIncarnationToTheLocks`（`kit/remoteentity/lock_incarnation_promises_test.go`）、`TestMirrorLocalSameSidRestartTakesOverTheOldIncarnationsLock`（`remoteentity/lock_takeover_integration_test.go`）。

**4. 控制流**

```mermaid
flowchart TD
  T["TryLock：token = self + 锁对象前缀 + 序号"] --> E["Eval versionedTryLockLua(ARGV3 前缀, ARGV4 序号, ARGV5 scope, ARGV6 self)"]
  E --> O{"owner"}
  O -- "不存在（TTL 过期）" --> A["INCR fence，HSET owner，PEXPIRE"]
  O -- "本锁对象更早序号" --> A
  O -- "以 scope 开头且不以 self 开头（上一代）" --> A2["同一分支，回报第 4 项 = 1"]
  O -- "别的 sid / 服务类型 / 前缀、本代、旧格式" --> N["返回 0：NotAcquired，照旧等 TTL"]
  A --> G["GrantWrite(新 token)：Mongo _grant_fence 递增"]
  A2 --> C["noteTakeover：计数 + 首次 Info"] --> G
```

**5. 失败与不确定结果**

| 情形 | 处理 | 对调用方 |
| --- | --- | --- |
| Incarnation 格式错误 | `Assemble` 失败 | `ErrVersionedLockConfig` |
| `singleton.enabled=false` | 不传代际，旧格式 token | 按 TTL 等待（不变） |
| 上一代仍有在途提交（卡住后恢复） | `CommitRemote` 要求 `_grant_fence` 等于它的 fence | 上一代被确定拒绝（`remote entity: state version conflict`） |
| 上一代 Touch / Refresh / Unlock | Lua 要求 owner 等于自己 token | 过期 / 未持有，不改动新一代 |
| 旧进程卡住恢复后反向接管 | 新进程那一笔被 Mongo fence 确定拒绝，下次取锁再接管 | 至多持续到旧进程 fail-stop |
| 取锁回复丢失 | owner 是本锁对象 token，下次按 RR-20261004-01 取回 | 不变 |

**6. 测试**：修前红（原样，出处 [MIRROR-M6-OBSERVATIONS §7.5](../../feature/MIRROR-M6-OBSERVATIONS-2026-10-06.md)；基线 worktree，红用例文件未入库）：

```text
--- FAIL: TestRedSameSidRestartFirstLockWaitsForTheOldProcesssLease (0.01s)
    lock_takeover_red_test.go:98: the restarted same-sid process's first lock = versioned lock not acquired after 7ms, want it acquired right away (the old process is dead; its lease runs for lock_ttl 3s)
```

```text
    lock_takeover_red_integration_test.go:69: the restarted same-sid process's first TryLock = versioned lock not acquired, want it to take over the dead process's lease (lock_ttl 3s)
--- FAIL: TestMirrorLocalSameSidRestartTakesOverTheOldIncarnationsLock (0.20s)
```

```text
    mirror_local_test.go:418: the first write after the restart took 2408ms (lock_ttl 3s): it waited for the killed owner's lease instead of taking it over (O-M6-6)
--- FAIL: TestGeneratedRemoteMirrorLocal/S1_owner_kill_restart_wal_replay (4.71s)
```

修后（原样，同出处）：

```text
MIRROR O-M6-6: restarted same-sid TryLock took 9ms (lock_ttl 3s); fence 1 -> 2; old grant commit: remote entity: state version conflict
--- PASS: TestMirrorLocalSameSidRestartTakesOverTheOldIncarnationsLock (0.29s)
--- PASS: TestMirrorLocalTombstoneSurvivesAFailoverOnlyWithWait (10.24s)
--- PASS: TestMirrorLocalTombstoneWaitOnClusterGoesToTheKeysPrimary (0.81s)
```

```text
MIRROR6 .../S1_owner_kill_restart_wal_replay owner_restart_ready_ms=566 replayed_v2_converge_ms=-19
MIRROR6 .../S1_owner_kill_restart_wal_replay first_write_started_after_kill_ms=566 (lock_ttl 3s)
MIRROR6 .../S1_owner_kill_restart_wal_replay after_restart_commit_v3 commit_ms=33 converge_ms=25 visible_after_confirm_ms=-8
MIRROR6 .../S1_owner_kill_restart_wal_replay reader_stats loads=0 errors=0 reads=182
--- PASS: TestGeneratedRemoteMirrorLocal/S1_owner_kill_restart_wal_replay (4.38s)
```

对照（不接管）：同一用例里别的 sid、同 sid 未启用单实例锁、旧格式 token、同一代另一个锁对象都是 NotAcquired。

**7. 性能证据**：S1 重启后第一笔写修前 2408ms（等旧租约）→ 修后 33ms，无多写（CHANGELOG / 记录）。取锁脚本多两个参数、零额外往返。

**8. 未验证项与已知风险**：旧进程卡住恢复后的反向接管边界（见说明）；core `DefaultConfig.LockTTL` 24h 且未开单实例锁的装配不受益；多主机强杀 E13、多机 Cluster 锁 Lua E08。

**9. review 检查点**

- [ ] 确认 Lua 接管判定（`remoteentity/versioned_lock_lua.go:24`）要求 `not earlier`，且本代自己留下的 token（以 `self` 开头）不被接管，对照 `TestLockTakeoverOnlyAppliesToThePreviousIncarnationOfTheSameSingletonHolder` 的“同一代另一个锁对象”子用例。
- [ ] 确认旧格式 token（`crand.Text` base32 大写开头）不可能以 `~` 开头，`Token` 字符集（`remoteentity/versioned_lock.go:578`～`:581`）排除 `~`，前缀判定无歧义。
- [ ] 确认 kit 只在 `incarnation.Sid == m.localSid` 时传（`kit/remoteentity/remote_entity_mod.go:182`），`NewRemoteEntityMod(sid)` 显式给了别的 sid 时不传。
- [ ] 确认接管后 `GrantWrite` 失败时走 `versionedAbandonLua` 只清本 token（记录 §7.5“组合契约复核”），上一代 owner 不被恢复。
- [ ] 确认 `noteTakeover` 在 `i == nil` 时仍计数但不 panic（`remoteentity/versioned_lock.go:598`～`:601`）。

<a id="rem-11"></a>
### REM-11 L2 落后于权威的上界（保持，写明上界）

> 首发 v1.23.0（本版，文档） · [说明](guide-saga-drv-dao-rem.md#rem-11)

**1. 提交**

| 提交 | 版本 | 内容 |
| --- | --- | --- |
| `78e26853` | v1.23.0（本版） | 记录第十二轮决定 |
| `88f33776` | v1.23.0（本版） | B2 §7 与 USER_GUIDE 写明上界（代码不变） |
| `8059b877` | v1.23.0（本版） | 收尾第 1 批与第十二轮文档类决定标为已实施 |

**2. 改动文件与关键符号**（上界由以下源码决定；本条无代码改动）

| `path:line` | 符号 | 职责 |
| --- | --- | --- |
| `remoteentity/snapshot_l2.go:70` | CAS 脚本 `PEXPIRE` | 每次成功写（含同版本同值重写）重设 TTL |
| `remoteentity/snapshot_l2.go:98` | 墓碑脚本 `PEXPIRE` | 墓碑同 TTL |
| `remoteentity/config.go:86` | `DefaultConfig.SnapshotL2TTL` | 5m |
| `codegen/internal/roost/catalog.go:71` | 生成模板 | `snapshot_l2_ttl: 10m`、`cached_max_staleness: 30s` |
| `entity/remote_snapshot.go:787`～`:803` | `refresh` 的 L2 无值分支 | L1 有快照而 L2 无值 → 回源权威 |
| `entity/remote_snapshot.go:762`～`:777` | `refresh` 的“L1 比 L2 新”分支 | owner 自己下一次读补写 L2 |

**3. 不变量与强制点**：上界 = `snapshot_l2_ttl`（L2 记住旧值的最长时间）+ `cached_max_staleness`（读者最后一次确认后还能交出的时间）。没有后台补写，靠 owner 下一次读（`refresh` 补写）、复制更新 / 删除在每个接收节点写 L2、下一笔提交写 L2。无专门守卫测试（文档决定）；相关行为由 `TestB2PublisherRepairsALostL2Write`、`TestB2LostL2DeleteIsRepairedByTheNextRead` 钉住。

**4. 控制流**：1）owner 提交，L2 写失败 / 结果未知 → owner L1 记新版本但未确认；2）读者重新确认读到 L2 的旧值（同值 → 改记确认）；3）owner 下一次读 → `refresh` 发现 L1 比 L2 新 → `admitLocked` 补写；或 4）L2 键在最后一次写入后 `snapshot_l2_ttl` 过期 → 读者重新确认时 L2 无值 → 回源权威。

**5. 失败与不确定结果**

| 情形 | 处理 | 对调用方 |
| --- | --- | --- |
| owner 写 L2 失败且之后不再读 / 不再提交、无推送 | L2 旧值存活到 TTL | 读者最长 `snapshot_l2_ttl + cached_max_staleness` 读到旧值 |
| 推送开着 | 接收节点写 L2 | 通常更早修好 |
| Linearizable | 每次读权威 | 不受影响 |

**6. 测试**：无新增（文档决定）。

**7. 性能证据**：无。

**8. 未验证项与已知风险**

- 记录（B2 §7、USER_GUIDE、DECISIONS 第十二轮）写“缺省约 5m30s”，按 core `DefaultConfig`（5m + 30s）成立；生成配置模板 `snapshot_l2_ttl: 10m`（`codegen/internal/roost/catalog.go:71`），按模板部署时约 10m30s（推断，按模板值计算）。（已补进 USER_GUIDE、B2 §7 与 DECISIONS，fixr。）
- 读者重新确认读到同值时只改记确认时刻、不写 L2（`entity/remote_snapshot.go:750`～`:761`），所以读者不会续命 L2 的旧值；但收到旧版本复制消息（未过 O5 窗口）且 L1 冷的节点会把旧值 CAS 进 L2 并续期（推断，CAS 对同版本同值也 `PEXPIRE`）。

**9. review 检查点**

- [ ] 确认“读者不会给 L2 旧值续期”：`refresh` 同值分支（`entity/remote_snapshot.go:750`）只 `setL1Locked`，不调 `admitLocked` / L2。
- [ ] 评估“L1 冷节点收到较旧复制消息时 CAS 同值续期 L2 旧值”是否会让上界超过 `snapshot_l2_ttl`（CAS 脚本 `remoteentity/snapshot_l2.go:66`～`:70`）。
- [x] 在 USER_GUIDE 与生成模板注释里核对“约 5m30s”是否需要注明“按 core 缺省；生成模板为 10m L2 TTL”。（fixr：USER_GUIDE 与 B2 §7 已分别写明 core 缺省约 5m30s、生成模板约 10m30s。）
- [ ] 确认 `snapshot_l2_ttl` 调小的代价（墓碑寿命同时变短）写进了说明。

<a id="rem-12"></a>
### REM-12 Redis Cluster 迁槽（ASK / MOVED）下 L2 读写与墓碑 WAIT 实测；mirror-local Cluster 就绪判定

> 首发 v1.23.0（本版） · [说明](guide-saga-drv-dao-rem.md#rem-12)

**1. 提交**

| 提交 | 版本 | 内容 |
| --- | --- | --- |
| `ba13cb05` | v1.23.0（本版） | 新增 `remoteentity/cluster_slot_migration_integration_test.go`；`scripts/mirror-local.sh` 加 `cluster_replicas_online`；记录（同一提交还有示例实跑门禁、saga 用例时序修复等，属其他主题） |

**2. 改动文件与关键符号**

| `path:line` | 符号 | 职责 |
| --- | --- | --- |
| `remoteentity/cluster_slot_migration_integration_test.go:32` | `TestMirrorLocalClusterSlotMigrationSnapshotReadWriteAndTombstone` | ASK / MOVED / 迁回 |
| `remoteentity/cluster_slot_migration_integration_test.go:256` | `existsAsking` | 在 IMPORTING 节点同连接先发 ASKING 再查键 |
| `scripts/mirror-local.sh:103` | `cluster_replicas_online` | 每个主节点有 `state=online` 的副本 |
| `scripts/mirror-local.sh:122` | `cluster_up` | 加等待 `cluster_replicas_online`（30s） |
| `remoteentity/snapshot_l2.go:357` | `recordTombstoneWait` | 结果分类（被测，属 DRV-4） |

**3. 不变量与强制点**：迁槽期间 L2 读写落到键实际所在节点、墓碑不丢、旧版本写不能复活；墓碑脚本遇重定向不 WAIT（计 `skipped`）。由测试断言（`:141`～`:142` ASK、`:161`～`:162` MOVED）。环境就绪判定由脚本强制。

**4. 控制流**：1）写 v1；2）ASK：源 `MIGRATING`、目标 `IMPORTING`、`MIGRATE` 键到目标 → HGET 跟随 ASK、写脚本跟随 ASK（源上不重建键）、墓碑脚本在源上回 ASK → 退回集群客户端普通发送一次（`skipped`），墓碑落在目标，之后旧版本写被拒；3）MOVED：`SETSLOT NODE` 完成迁移、客户端槽位表仍指向源 → 墓碑 `skipped`、写读跟随 MOVED；槽位表刷新后墓碑 WAIT 打到新主（`confirmed`）；4）槽位迁回源后读写照常。

**5. 失败与不确定结果**

| 情形 | 处理 | 结果 |
| --- | --- | --- |
| 墓碑遇 ASK / MOVED | 普通发送一次，不 WAIT | `skipped` 计数，墓碑落在目标 |
| 副本未 online（修前环境） | `ROLE` 不列副本 | WAIT 按 `no_replicas` 跳过，既有用例单独跑必红 |
| “键还在源上、槽位 MIGRATING” | 命令直接在源上执行 | 未单列 |

**6. 测试**

| 用例 | 文件 | 覆盖 |
| --- | --- | --- |
| `TestMirrorLocalClusterSlotMigrationSnapshotReadWriteAndTombstone` | `remoteentity/cluster_slot_migration_integration_test.go:32` | 新增 |
| `TestMirrorLocalTombstoneWaitOnClusterGoesToTheKeysPrimary` | `remoteentity/snapshot_l2_tombstone_wait_integration_test.go` | 既有，就绪判定修复后单独跑也绿 |

环境缺口修前红（原样，出处 [PRERELEASE-VERIFICATION §4b](../../bugfix/PRERELEASE-VERIFICATION-2026-10-06.md)）：

```text
snapshot_l2_tombstone_wait_integration_test.go:319: WAIT calls on 127.0.0.1:37401 went up by 0, want 1 (the key's primary is 127.0.0.1:37401)
```

修后：`ROOST_MIRROR_LOCAL_HOME=<scratch> ROOST_MIRROR_LOCAL_CORE_RUN='^TestMirrorLocalClusterSlotMigration|^TestMirrorLocalTombstoneWaitOnClusterGoesToTheKeysPrimary$' scripts/mirror-local.sh test-core` → 两条 PASS；默认集合（`^TestMirrorLocal`，5 条）全部 PASS；最终 stats `{Confirmed:1 Skipped:2}`（同出处，原始输出在主检出被忽略的 `artifacts/perf/relprep-20261006/item4-*.out`，不入库）。新用例本身是验证（产品行为原本正确），没有修前红。

**7. 性能证据**：无。

**8. 未验证项与已知风险**

- ASK 窗口只覆盖“键已搬到目标”。
- 记录 [MIRROR-M6-OBSERVATIONS §3](../../feature/MIRROR-M6-OBSERVATIONS-2026-10-06.md) 的结果表写 `redirected` / `unsupported` 两种结果；源码 `recordTombstoneWait` 的指标标签只有 `skipped`（`remoteentity/snapshot_l2.go:377`～`:379`，统计字段 `Skipped`），以源码为准（DRV 主题，此处登记）。（已改 MIRROR-M6-OBSERVATIONS §3，fixr。）
- `cluster_replicas_online` 只看 `slave0:` 一行（`scripts/mirror-local.sh:108`），每主一个副本时等价于“至少一个 online 副本”。
- 多机 Cluster：E08；异步复制丢写：E10。

**9. review 检查点**

- [ ] 确认 MOVED 阶段的 `skipped` 断言只在客户端槽位表确实过期（`stale`）时才检查（`remoteentity/cluster_slot_migration_integration_test.go:161`），表已刷新时用例不误报也不漏测。
- [ ] 确认测试在迁槽后、读写前没有手工刷新槽位表而掩盖 MOVED 路径（`:170` 注释写的是环境就绪等待）。
- [ ] 确认 `cluster_replicas_online` 的 30s 上限与 `cluster_ok` 顺序（`scripts/mirror-local.sh:121`～`:122`）在冷启动时足够。
- [ ] 墓碑 WAIT 的设计、`EvalReplicated` 的重定向退化与指标标签见 [DRV-4](impl-saga-drv-dao-rem.md#drv-4)，确认两处文档对 `skipped` 的叫法一致。

<a id="rem-13"></a>
### REM-13 生成配置写出 remote_entity 新键（收尾第 2 批 A8）

> 首发 v1.23.0（本版） · [说明](guide-saga-drv-dao-rem.md#rem-13)

**1. 提交**

| 提交 | 版本 | 内容 |
| --- | --- | --- |
| `fcc78ad0` | v1.23.0（本版） | `catalog.go` 模板加五个键与注释；`render.go` 的 `streamReplicasLine` 只替换独占一行的 `replicas: 1`；两条生成用例（同一提交还有 A9 / A11 / A15 / A17，属其他主题） |
| `94548913` | v1.23.0（本版） | 记录与 DECISIONS-PENDING 标为已实施 |

**2. 改动文件与关键符号**

| `path:line` | 符号 | 职责 |
| --- | --- | --- |
| `codegen/internal/roost/catalog.go:71` | `remote_entity` Mod 模板 `Config` | 五个新键与中文注释 |
| `codegen/internal/roost/render.go:647` | `streamReplicasLine` | `(?m)^([ \t]*)replicas: 1$` |
| `codegen/internal/roost/render.go:656` | 生产化替换 | 只把匹配行改成 `replicas: 3` |
| `codegen/internal/roost/remote_entity_config_keys_promises_test.go` | `TestGeneratedRemoteEntitySectionCarriesTheSnapshotKeys` | 开发 / 生产两份配置带五个键 |
| `codegen/internal/roost/generated_config_validation_promises_test.go` | `TestGeneratedConfigsPassStrictAndProductionValidation` | 生成工程里注入检查：键都设置、取值等于 `DefaultConfig` / kit 缺省、`ValidateServiceConfig` 与两个 Mod 的 `Init` 都接受 |

**3. 不变量与强制点**：生成配置的取值与 core `DefaultConfig` / kit 缺省一致（守卫：`TestGeneratedConfigsPassStrictAndProductionValidation`）；生产化不改墓碑 WAIT 副本数（守卫：同上 + `TestGeneratedRemoteEntitySectionCarriesTheSnapshotKeys` 的 production=true 分支）。

**4. 控制流**：`roost project new` / `add` 渲染 Mod 配置段 → 开发配置原样；生产示例与 k8s Secret 示例经 `productionizeConfig` / `appendModConfigSections`，`streamReplicasLine` 只替换整行恰为 `replicas: 1` 的流副本数。

**5. 失败与不确定结果**

| 情形 | 处理 | 结果 |
| --- | --- | --- |
| 已有工程 | 不回写配置 | 不配置时用缺省值 |
| v1.21.0 / v1.22.0 的 kit 读到墓碑两键 | 记录写“viper 忽略未知键” | 生成器 Core 下限不变 |

**6. 测试**：修前红（原样，出处 [CLOSING-BATCH-2 §A8](../../bugfix/CLOSING-BATCH-2-2026-10-06.md)）：

```text
--- FAIL: TestGeneratedRemoteEntitySectionCarriesTheSnapshotKeys
    production=false: config lacks "cached_max_staleness: 30s"   （其余四个键同样，开发 / 生产各一遍）
--- FAIL: TestGeneratedConfigsPassStrictAndProductionValidation
    a4_config_test.go:92: config.game.yaml does not set remote_entity.cached_max_staleness
    a4_config_test.go:103: config.game.yaml: snapshot_l2_tombstone_wait_replicas = 0, want DefaultConfig 1
    （config.game.prod.example.yaml、secret.game.example.yaml 同样）
```

中间负对照（同出处）：只改模板、不改生产化时 `production=true: config lacks "snapshot_l2_tombstone_wait_replicas: 1"`（被改成了 3）。修后两条用例通过；`go test -count=1 ./codegen/...`、`go generate ./...` 无漂移、生成 game-demo build / vet / test 通过（记录“验证”表）。

**7. 性能证据**：无（不涉及热路径）。

**8. 未验证项与已知风险**

- 记录写“v1.21.0 / v1.22.0 的 kit 不认识墓碑两键，viper 忽略未知键”；A4 严格读取只针对登记的键，未登记键是否在 `ValidateServiceConfig` 被当作未知键报错未核对（推断忽略）。
- 生成模板的 `snapshot_l2_ttl: 10m`、`snapshot_interest_subs: 100000` 与 core `DefaultConfig`（5m、262144）不同，影响 [REM-11](#rem-11) 上界与 [REM-5](#rem-5) 缺省配额的实际数值。
- framework-compat full 场景未在 GitHub 上等结果。

**9. review 检查点**

- [ ] 确认 `streamReplicasLine`（`codegen/internal/roost/render.go:647`）不会匹配 `snapshot_l2_tombstone_wait_replicas: 1`（正则要求行首空白后紧接 `replicas`）。
- [ ] 确认模板 `cached_max_staleness: 30s` 与模板 `snapshot_cache_ttl: 30s` 同值，注释说明“不能写 0”（kit 要求设置了必须为正）。
- [ ] 确认 `mirror.shutdown_timeout` 写在 `remote_entity:` 下的 `mirror:` 子段，与 kit 读取的键名 `remote_entity.mirror.shutdown_timeout` 一致（`kit/remoteentity/remote_mirror_mod.go:103`）。
- [ ] 确认 `TestGeneratedConfigsPassStrictAndProductionValidation` 对每份含 `remote_entity:` 的配置都同时跑 `RemoteEntityMod.Init` 与 `RemoteMirrorMod.Init`。

## 全局守卫测试 / 门禁清单

本部分新增或加强的守卫测试（标 integration 的要 `-tags integration` 与真实依赖，按“本地复跑”一节加 `-run`）。根包门禁 `TestTrackedMarkdownRelativeLinksResolve`、`TestNoMergeConflictMarkersInTrackedFiles`、`TestExamplesRun` 归 TOOL 部分，本部分文档本身受前两条约束。

| 测试名 | 文件 | 守什么 | 条目 |
| --- | --- | --- | --- |
| `TestExpiredStepCommandWithoutReceiptIsAcknowledgedNotRedelivered` | `saga/step_expired_promises_test.go` | 过期命令不执行、无回执 ack、读错误重投 | SAGA-1 |
| `TestNativeStepTakesEffectAtMostOncePerOperation` | `saga/step_operation_promises_test.go` | 操作实例最多生效一次（a / b / c / c' / d） | SAGA-2 |
| `TestNativeStepOperationInterleavingsWithCoordinatorDecisions` | `saga/step_operation_promises_test.go` | 协调器决定 × 收件箱交错 | SAGA-2 |
| `TestNativeStepLeaseNeverOutlivesTheCommandDeadline` | `saga/step_operation_promises_test.go` | 租约封顶 | SAGA-2 |
| `TestNativeStepConsumerHandlesOperationOutcomes` | `saga/step_operation_promises_test.go` | 消费者分支 | SAGA-2 |
| `TestMongoStoreTombstoneTellsAbandonedFromResolved` | `saga/step_operation_promises_test.go` | tombstone `closure` | SAGA-2 |
| `TestNativeStepSuccessAfterAFailureClosedOperationIsAlarmed`、`TestMongoStoreTombstoneOfAFailureCloseIsAbandoned`、`TestNativeStepExpiredDeliveryStillReplaysTheOperationsSuccess` | `saga/step_operation_review_test.go` | 两处复核修复 | SAGA-2 |
| `TestRealMongoConcurrentAttemptsOfOneOperationReserveOnce`、`TestRealMongoSupersedeAndProjectionOfTheSameAttemptSerialize` | `saga/step_operation_real_mongo_integration_test.go`（integration） | 真实服务端守卫串行化、接替 vs 投影 | SAGA-2 |
| `TestStepBudgetsComeFromConfigWithPerStepOverrides`、`TestStepBudgetConfigRejectsTyposAndImpossibleValues` | `kit/saga/step_budgets_test.go` | 预算配置与校验 | SAGA-3 |
| `TestPerStepOverrideAppliesToMixedCaseNamesWithoutDefinitions`、`TestExactOverrideWinsOverTheLowercaseFallback` | `kit/saga/step_override_case_promises_test.go` | 小写回退与原样优先 | SAGA-4 |
| `TestStepBudgetConfigRejectsNamesThatDifferOnlyInCase` | `kit/saga/step_budgets_test.go` | 大小写歧义报错 | SAGA-5 |
| `TestCoordinatorChecksTheIncarnationOfACompletion`、`TestMongoStoreMarksALateSuccessAlarmOncePerLife`、`TestCommandIDIncarnationInvertsCommandID` | `saga/step_operation_incarnation_promises_test.go` | B1 四个边角、告警去重、代际解析 | SAGA-6 |
| `TestRealMongoLateSuccessAlarmIsMarkedOnce` | `saga/late_alarm_real_mongo_integration_test.go`（integration） | 并发告警标记只有一个 first | SAGA-6 |
| `TestDefinitionFenceDuringBackoffAbandonsTheOperation` | `saga/definition_fence_abandon_promises_test.go` | 定义缺失出口放弃关闭 | SAGA-7 |
| `TestCoordinatorLeaseTakeoverFencesTheLateApply`、`TestOutboxSupersedeAndUnknownAckOnMongoStore` | `saga/coordinator_takeover_review_test.go` | N06 S5 审查用例（租约接管晚 Apply、outbox 替换与未知 ack） | SAGA-7 |
| `TestEveryCoordinatorWriteGoesThroughStepTransition` | `saga/step_transition_guard_test.go` | 协调器写记录只经 `stepTransition`（源码 AST 守卫） | SAGA-8 |
| ~~`TestStepTransitionGuardSeesBypassesWithoutALiteral`~~ | 已删除（RR-20261006-14）；守卫改为类型检查，另加 `TestStepTransitionAloneDecidesTheIncarnation` | 原为守卫自己的负对照 | SAGA-8 |
| `TestMongoStepAttemptsOfOneOperationTakeEffectOnce` / `TestRealMongoStepAttemptsOfOneOperationTakeEffectOnce` | `saga/mongo_step_operation_promises_test.go` / `saga/mongo_step_operation_real_mongo_integration_test.go` | Mongo 步骤操作实例最多一次（mongotest 与真实副本集同一份用例） | SAGA-9 |
| `TestMongoStepConsumerFollowsTheOperationInbox` | `saga/mongo_step_consumer_promises_test.go` | Mongo 步骤消费者分支 | SAGA-9 |
| `TestCompletionConsumersTermTheSameTerminalErrors` | `saga/completion_consumer_terminal_promises_test.go` | 两条结果流同一终态分类（O-S5-1） | SAGA-9 |
| `TestRealSagaCrossProcessKillRecovers`（断言加强为每个操作恰好一次提交） | `saga/cross_process_real_integration_test.go`（integration） | 跨进程强杀 | SAGA-7 / SAGA-9 |
| `TestACompletionBeforeItsDefinitionIsRegisteredIsRetriedNotTerminated`、`TestADefinitionThatNeverArrivesEndsInTheCoordinatorFence` | `saga/completion_definition_rollout_promises_test.go` | 定义缺失可重试、最终由 fence 收尾 | SAGA-10 |
| `BenchmarkRealMongoStepLatencyBreakdown`、`BenchmarkRealMongoStepThroughput`、`BenchmarkRealMongoCommitWriteConcern` | `saga/mongo_step_latency_real_mongo_integration_test.go`（integration，只在 `-bench` 下运行） | 可复跑的延迟分析基准 | SAGA-11 |
| `TestModRefusesAnEffectStreamThatOutlivesTheCompletionReceipts`、`TestModChecksEffectRetentionOnlyAgainstTheStreamItReadsResultsFrom` | `kit/saga/effect_retention_promises_test.go` | O-S5-2 跨 Mod 校验 | SAGA-12 |
| `TestRealNatsCompletionNakBackoffAndMaxDeliver` | `saga/consumer_nak_maxdeliver_real_integration_test.go`（integration） | 真实 JetStream 上 nak 退避与 `MaxDeliver` | SAGA-13 |
| `TestAScriptWhoseReplyIsLostIsNotReplayedByTheDriver` | `redis/driver/script_no_retry_promises_test.go` | eval / evalsha 回复丢失只执行一次 | DRV-1 |
| `TestNoReplayMarkSurvivesCloneAndUnmarkedCommandsKeepTheDriverRetry`（原名 `TestOnlyScriptCommandsOptOutOfTheDriverRetry`） | 同上 | `noReplay` 克隆保留标记；普通命令可重试 | DRV-1 |
| `TestRealRedisUpdateWhoseReplyIsLostNeverWritesTwice` | `versionstore/lost_reply_integration_test.go` | 真实 Redis：Update 至多写一次（integration） | DRV-1 |
| `TestRealMongoCommitIsBoundedByTransactionTimeout` | `mongo/driver/transaction_deadline_integration_test.go` | 提交受窗口约束；提交被截断时带 `ErrCommitResultUnknown`（integration） | DRV-2 / DRV-3 |
| `TestRealMongoEndSessionAfterCommitTimeoutIsBounded` | 同上 | EndSession 补发 abort 有上限（integration） | DRV-2 |
| `TestWithTransactionKeepsTheLastCallbackErrorWhenTheWindowClosesInBackoff` | `mongo/driver/transaction_retry_chain_test.go` | 退避中到期保留最后一次错误链 | DRV-2 |
| `TestWithTransactionFailuresBeforeTheCommitAreNotResultUnknown` | 同上 | 确定未提交的错误不带哨兵 | DRV-3 |
| `TestAWriteWhoseReplyIsLostIsNotReplayedByTheDriver` | `redis/driver/write_no_replay_promises_test.go` | 21 个写调用点回复丢失只执行一次 | DRV-3 |
| `TestNotExecutedErrorsAreStillResent` / `TestResendsStopAtTheConfiguredBudget` / `TestAPipelineWithAnExecutedCommandIsNotResent` / `TestReadsKeepTheDriverRetry` / `TestDurableBatchIsResentWhenNothingExecuted` / `TestIsDefinitelyNotExecuted` | 同上 | 确定没执行才重发、次数上限、pipeline 不部分重放、读保留重试、分类表 | DRV-3 |
| `TestRealRedisAWriteWhoseReplyIsLostRunsOnce` | `redis/driver/write_lost_reply_integration_test.go` | 真实 Redis：incr / rpush / pipeline / setnx / distlock（integration） | DRV-3 |
| `TestAWriteWhoseReplyIsLostStillGetsItsTTL` | `cache/redis_lost_write_ttl_promises_test.go` | HSET / ZADD 结果未知后仍有 TTL | DRV-3 |
| `TestEvalReplicatedWaitsOnTheScriptsConnection` / `TestEvalReplicatedScriptErrorsAndDisabledWait` | `redis/driver/replicated_promises_test.go` | WAIT 同连接、无副本不等、WAIT 错误只进 WaitErr | DRV-4 |
| `TestSnapshotL2TombstoneWaitIsObservedButNeverFailsTheDelete` / `…DisabledAndUnsupported` / `…SettingsAreValidated` | `remoteentity/snapshot_l2_tombstone_wait_promises_test.go` | 计数、返回值只由脚本决定、设置校验 | DRV-4 |
| `TestTombstoneWaitConfiguration` | `kit/remoteentity/tombstone_wait_config_test.go` | 两 Mod 严格读取 | DRV-4 |
| `TestMirrorLocalTombstoneSurvivesAFailoverOnlyWithWait` / `TestMirrorLocalTombstoneWaitOnClusterGoesToTheKeysPrimary` | `remoteentity/snapshot_l2_tombstone_wait_integration_test.go` | 私有环境复制滞后切主 / Cluster 只打主节点（integration） | DRV-4 |
| 各包 `close_contract_promises_test.go`（redis / mongo / etcd / nats 驱动，kit redis / mongo / nats） | 见 DRV-5 实现 §6 | Close 统一口径；Mod 并发 Stop 无数据竞争 | DRV-5 |
| `TestSerialLaterCallerWaitsWithinItsOwnContext` | `internal/operation/serial_test.go` | 串行器等待受后到者 ctx 约束 | DRV-5 |
| `TestIndexCreationRetriesThroughAnElectionWithinBounds` | `mongo/driver/election_retry_promises_test.go` | 选举重试四种情形 | DRV-6 |
| `TestMirrorLocalOwnerStorageInitSurvivesAMongoElection` | `remoteentity/owner_startup_election_integration_test.go` | 私有副本集 stepDown 期间 owner 初始化（integration） | DRV-6 |
| `TestTransientFieldsHaveMutatorsAndStayOutOfStorageAndSync` | `codegen/internal/dao/transient_field_promises_test.go` | `nopersist,nosync` 有 mutator、不进存储 / 同步 | DAO-1 |
| `TestATransientFieldRollsBackWithTheTransaction` / `…NeverReachesTheCommitRecordOrSync` / `…IsInTheStateSnapshot` | `codegen/internal/dao/testdata/runtime/transient_test.go` | daoruntime 运行验证 | DAO-1 |
| `TestComponentRecordingItsOwnUndoIsHinted` | `cmd/glsvet/main_test.go` | 组件方法登记 undo 被提示（不计失败） | DAO-1 |
| `TestAttributeRollbackIsTheDaoRollback` / `TestNonPersistentAttributeLayersStayOutOfTheWAL` / `TestTimerRollbackIsTheDaoRollback` / `TestTimerBookkeepingStaysOutOfTheCommitRecord` | `demo/game/entities/{player,world}/*_component_test.go.tmpl` | 生成 game-demo 的组合回滚、WAL 不含非持久字段 | DAO-1 |
| `TestCombatRollbackIsTheDaoRollback` | `skill/combatcomponent/dao_rollback_promises_test.go` | 两策略 × 两失败路径字节一致 | DAO-1 |
| `TestSkillPackagesGetNoComponentUndoHint` | `cmd/glsvet/main_test.go` | skill 四个包零 A1 提示 | DAO-2 |
| `TestBuffAttributeModifierReachesDamage` / `TestAttributeProjectionRollsBackWithTheDao` / `TestAttributeProjectionReprojectsOnLoad` | `skill/combatcomponent/attribute_projection_promises_test.go` | buff 进伤害、投影随 DAO 回滚、加载重投影不标脏 | DAO-3 |
| `TestB2CachedReadPastMaxStalenessReconfirmsAgainstL2` 等 `TestB2*` 6 条 | `remoteentity/snapshot_l2_watermark_promises_test.go` | 陈旧上限、未确认不交出、补写丢失的 L2 写 / 删除、O5、并发收敛 | REM-1 |
| `TestRealB2WatermarkMatrixStandalone` / `…Cluster` | `remoteentity/snapshot_l2_watermark_matrix_integration_test.go` | 真实 Redis 单机 / Cluster 各 20 格水位矩阵 | REM-1 |
| `TestRealJetStreamReplayAfterL2ExpiryDoesNotResurrect` | `remoteentity/snapshot_replay_jetstream_integration_test.go` | O5：DeliverAll 重放不复活 | REM-1 |
| `TestCachedMaxStalenessConfiguration` | `kit/remoteentity/cached_max_staleness_test.go` | 新键严格读取 | REM-1 |
| `TestRemoteObservationCoversFollowsAdmissionOrder`、`TestRemoteMirrorReaderDTOMutationDoesNotPolluteCache`、`TestRemoteMirrorReaderNeedsNoRegistrationBesideTheOwner`、`TestRemoteMirrorReaderRejectsForeignIdentityAndSchema`、`TestRemoteSnapshotReadExitsShareOnePostCondition` | `entity/remote_mirror_promises_test.go` | token 排序、DTO 副本、无注册冲突、读侧身份、读出口后置条件 | REM-2 |
| `TestReadRemoteSnapshotMonotonicMissLoadsAuthorityOnce` / `…BelowMinimumLoadsAuthorityOnce` | `remoteentity/snapshot_read_exit_promises_test.go` | 一次 Monotonic 只回源一次 | REM-2 |
| `TestSnapshotClientHasNoWriteCapability`、`…LinearizableNeedsADeclaredLoader`、`…StartFailureLeavesNoSubscription`、`…StopContract`、`…StopCancelsLoads`、`…ReadsWhatTheOwnerPublishesInTheSameProcess` | `remoteentity/snapshot_client_promises_test.go` | 只读、线性化门、启动回收、三步停机 | REM-2 |
| `TestCachedRemoteAccessWithAllowStaleStillAcceptsAnOlderSnapshot` | `nest/remote_cached_allow_stale_promises_test.go` | allow_stale 的 Cached 访问 | REM-3 |
| `TestSnapshotBootstrapBuffersDeltaDuringFirstLoad`、`TestSnapshotBootstrapReplayMatrix`、`TestSnapshotBootstrapOverflowDropsTheBufferAndReloads`、`TestInterestRenewReleaseConvergesInEveryDeliveryOrder`、`TestSnapshotClientWithoutConfirmedSubscriptionsReadsOnDemand` | `remoteentity/mirror_step4_promises_test.go` | 首载缓冲、溢出再回源、renew / release 乱序收敛、退化 | REM-4 |
| `TestJetStreamSubscribeLiveUsesASeparateDeliverNewDurable` | `sync/syncbus/driver/jetstream_live_promises_test.go` | DeliverNew durable 与 DeliverAll 分开 | REM-4 |
| `TestRealJetStreamLiveSubscriptionConfirmsAndResumes`、`TestRealJetStreamLiveSnapshotPushReachesTheReader` | `remoteentity/mirror_step4_jetstream_integration_test.go` | 真实 JetStream 确认订阅与推送 | REM-4 |
| `TestInterestCapacityIsPerConsumer`、`TestInterestQuotaRefusalIsVisibleAndReadsGoOnDemand` | `remoteentity/mirror_step4_promises_test.go` | 按 consumer 配额、拒绝可见 | REM-5 |
| `TestInterestPerConsumerConfiguration` | `kit/remoteentity/interest_quota_config_test.go` | 新键严格读取与范围 | REM-5 |
| `TestRemoteMirrorModRegistersOnlyReadCapability`、`…RefusesASecondClientBesideTheOwner`、`…Configuration`、`…StopContract`、`…StopCancelsInFlightReads`、`…HealthReportsPushMode` | `kit/remoteentity/remote_mirror_mod_promises_test.go` | 只读装配、冲突、配置、停机、健康 | REM-6 |
| `TestRemoteMirrorEntityMarkerIsAMigrationError`、`TestMirrorDTOGeneratesReadOnlyView`、`TestMirrorDTOOnlyPackageIsDiscoveredAndRetired`、`TestMirrorMarkerValidation` | `codegen/internal/entity/mirror_promises_test.go` | 迁移诊断、生成物无写能力 | REM-6 |
| `TestGeneratedRemoteMirrorGuildSummary/{jetstream,nats}`、`TestGeneratedRemoteMirrorReaderProcess` | `codegen/internal/entity/testdata/remoteflow/mirror_test.go` | 两进程样例（`scripts/test-remote-generated.sh`） | REM-6 |
| `TestRemoteDeleteCommitPublishesItsTombstoneAfterTheInstanceIsCleared` | `remoteentity/remote_delete_ack_promises_test.go` | 删除确认身份不符时照常发布 | REM-7 |
| `TestGeneratedRemoteMirrorLocal`（S1～S7） | `codegen/internal/entity/testdata/remoteflow/mirror_local_test.go` | 私有环境故障场景（`ROOST_MIRROR_LOCAL=1`） | REM-8 |
| `BenchmarkMirrorLocalRead` / `BenchmarkMirrorLocalPush` | `remoteentity/mirror_local_bench_integration_test.go` | v1.20.2 对照基准 | REM-8 |
| `TestOwnerRestartWithTheSameSidGetsInterestBackWithoutWaitingForRenewal`、`TestInterestRefreshRenewsOnlyLiveInterests`、`TestInterestRefreshRequestsCoalesceAndAreValidated`、`TestInterestRefreshGapWaitEndsOnStop`、`TestInterestRefreshNeedsPushAndToleratesOldConsumers`、`TestSnapshotClientRefreshSubscriptionFailureLeavesNoSubscription` | `remoteentity/interest_refresh_promises_test.go` | 续租请求恢复、合并、校验、停机 | REM-9 |
| `TestSameSidRestartTakesOverThePreviousIncarnationsSharedLock`、`TestLockTakeoverOnlyAppliesToThePreviousIncarnationOfTheSameSingletonHolder`、`TestAssembleRejectsAMalformedIncarnation` | `remoteentity/lock_takeover_promises_test.go` | 接管范围与校验 | REM-10 |
| `TestMirrorLocalSameSidRestartTakesOverTheOldIncarnationsLock` | `remoteentity/lock_takeover_integration_test.go` | 真实 Redis Lua + Mongo | REM-10 |
| `TestSingletonIncarnationIsTheHeldLocksIdentity` | `app/singleton_incarnation_promises_test.go` | 单实例锁身份登记 | REM-10 |
| `TestRemoteEntityModPassesTheSingletonIncarnationToTheLocks` | `kit/remoteentity/lock_incarnation_promises_test.go` | kit 传代际的条件 | REM-10 |
| `TestMirrorLocalClusterSlotMigrationSnapshotReadWriteAndTombstone` | `remoteentity/cluster_slot_migration_integration_test.go` | Cluster ASK / MOVED 下 L2 与墓碑 | REM-12 |
| `TestGeneratedRemoteEntitySectionCarriesTheSnapshotKeys` | `codegen/internal/roost/remote_entity_config_keys_promises_test.go` | 生成配置带五个键 | REM-13 |
| `TestGeneratedConfigsPassStrictAndProductionValidation` | `codegen/internal/roost/generated_config_validation_promises_test.go` | 生成配置取值等于缺省并通过严格校验 | REM-13 |

## 按包的改动索引

| 包（目录） | 条目 |
| --- | --- |
| `saga/`（`command_consumer.go`） | SAGA-1、SAGA-2、SAGA-9、SAGA-10 |
| `saga/`（`step_operation_inbox.go`、`dataengine_step_inbox.go`） | SAGA-2、SAGA-6、SAGA-9 |
| `saga/`（`engine.go`、`step_transition.go`） | SAGA-2、SAGA-6、SAGA-7、SAGA-8、SAGA-10 |
| `saga/`（`mongo_store.go`、`store.go`） | SAGA-2、SAGA-6、SAGA-7 |
| `saga/`（`record.go`） | SAGA-3、SAGA-4 |
| `saga/`（`nest_completion_consumer.go`、`jetstream.go`） | SAGA-9、SAGA-10 |
| `saga/`（仅测试） | SAGA-11、SAGA-13 |
| `kit/saga/` | SAGA-3、SAGA-4、SAGA-5、SAGA-12 |
| `kit/dataengine/`（`EffectStreamRetention`） | SAGA-12 |
| `dataengine/`（`lease_fence.go`，未改，契约依赖） | SAGA-2 |
| `codegen/internal/roost/`（`add.go`、`catalog.go`、`demo.go`）与 `demo/` 模板 | SAGA-2、SAGA-3、SAGA-9 |
| 文档：`SAGA.md`、`docs/USER_GUIDE.md`、`docs/TROUBLESHOOTING.md`（T-225 / T-226 / T-281） | SAGA-1、SAGA-2、SAGA-6、SAGA-9、SAGA-10、SAGA-12 |
| `redis/driver` | DRV-1、DRV-3、DRV-4、DRV-5 |
| `redis`（`fredis` 接口） | DRV-4 |
| `mongo`（`fmongo`） | DRV-3 |
| `mongo/driver` | DRV-2、DRV-3、DRV-5、DRV-6 |
| `etcd/driver`、`nats/driver` | DRV-5 |
| `internal/operation` | DRV-5 |
| `kit/redis`、`kit/mongo`、`kit/nats` | DRV-5（kit/redis 单实例锁的 `MaxRetries=-1` 与 DRV-3 相关） |
| `kit/remoteentity` | DRV-4 |
| `remoteentity` | DRV-4、DRV-6（调用方与集成用例） |
| `app`（`config_validation.go`） | DRV-4 |
| `versionstore` | DRV-1 |
| `cache` | DRV-3 |
| `codegen/internal/dao`、`codegen/internal/roost` | DAO-1 |
| `demo`（game-demo 模板） | DAO-1 |
| `cmd/glsvet` | DAO-1、DAO-2 |
| `skill/combatcomponent` | DAO-1、DAO-3 |
| `skill/examples/statusbridge` | DAO-3 |
| `docs/skill`、`docs/agent-skills/roost-coding` | DAO-1、DAO-2、DAO-3、DRV-5 |
| `entity/` | REM-1、REM-2、REM-4 |
| `remoteentity/` | REM-1、REM-2、REM-4、REM-5、REM-6（只读 Mongo loader）、REM-7、REM-8（基准）、REM-9、REM-10、REM-11、REM-12 |
| `sync/syncbus/`（`sync.go`、`driver/`、`mirror/`） | REM-4 |
| `kit/remoteentity/` | REM-1、REM-5、REM-6、REM-10 |
| `kit/mods/` | REM-6、REM-10 |
| `app/`（`config_validation.go`、`singleton.go`） | REM-1、REM-5、REM-6、REM-10 |
| `nest/` | REM-3 |
| `codegen/internal/entity/`（含 `testdata/remoteflow`） | REM-6、REM-8 |
| `codegen/internal/roost/` | REM-13 |
| `scripts/mirror-local.sh` | REM-8、REM-12 |
