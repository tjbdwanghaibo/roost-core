# Roost 维护与收敛清单

Roost 当前进入稳定化阶段。后续主线只接受三类工作：已复现 bug 修复、可靠性/工程性验证、删除或迁出非核心能力。未经明确产品决策，不再向 core/kit 增加新玩法 feature、控制面或基础设施抽象。

## 当前状态

- **研发基线已闭环**：四仓用未提交的 `go.work` 做 source-head 联调。持久 Entity 删除已经进入唯一 Data Engine admission；Remote 删除使用显式 delete intent 并复用 ownership marker、lock fence、route epoch。Saga reservation token 已作为控制 receipt 进入同一 Mongo 事务，只有 owner、token、`pending` 状态和未过期租约全部匹配才应用业务 mutation。
- **正式发布仍有一个门禁**：按 core → kit → skill → service → codegen 发布正式 tag，并让生成工程在 `GOWORK=off` 下只依赖这些 tag。研发 workspace 通过不能代替 pure-tag 闭包。

## Feature 逻辑修改包（M1–M9）状态

来源：[FEATURE_LOGIC_REASSESSMENT_AND_FIX_PLAN_2026-09-01](FEATURE_LOGIC_REASSESSMENT_AND_FIX_PLAN_2026-09-01.md) §5。
状态按 2026-09-04 的代码检索判定，**不是测试证据**：标"已实现"的项仍欠一次按
[收敛工作单元协议](history/ledger.md) 执行的回退验证，已逐项登记为账本待开单元。

| 包 | 状态 | 依据（代码位置） | 剩余 |
| --- | --- | --- | --- |
| M1 Sync 消息身份 | 已实现（U-0009 回退验证：去掉 MessageID 两条新测试变红） | `core/syncbus.DeliveryIDs` 是唯一身份规则；`PatchSyncer`、`core/mirror.Replicator`、`kit/syncstream.Publisher` 三条发布路径都经它取 `MessageID`；`kit/room.syncMsgID` 优先取 MessageID，元组仅为旧消息兼容 | — |
| M2 Replica 信封与 Context | 已实现 | `core/mirror` 内外层字段一致性校验、Start 配置不全报错、context-aware publisher | 回退验证 |
| M3 ConfigData 条件回滚 | 已实现 | `core/configdata` 在 `publishMu` 临界区内判定全局槽位所有权并计数冲突 | 回退验证 |
| M4 Remote 锁代际证明 | 已实现（U-0011 核对：09-04 的"未开始"是关键词误判） | 每次 `TryLock` 新 token、每次 `UnlockWithRetry` 独立 operation ID、Lua 以 `last_unlock` 收据判定幂等重试、旧 unlock 只能读自己的收据（§4.2 第 1–5 项，含测试）；`batch.go` 改用 `redis.IFencedVersionedLock` 公开契约，无 fence 工厂在构造与 `Provide` 时 fail-closed（第 6、7 项，U-0011）；缺失的两条确定性测试已补 | §4.2 要求的第五条测试（高 fence 与低 fence 的 Mongo 提交竞争最多一个成功）需要真实 Mongo，登记为账本 B-10 |
| M5 Saga/JetStream 终态 | 已实现 | `kit/saga` Stop 先 drain 并等待 `Closed()`；`kit/nats` permanent/Term 分类与 MaxDeliver 指标 | 回退验证 |
| M6 Etcd election/watch 终态 | 已实现 | `LeaderChan` 复用、`ErrElectionNoLeader` 可 `errors.Is`、watcher `Done()/Err()`、compaction 处理 | 回退验证 |
| M7 Redis best-effort 锁 | 已实现 | `kit/redis/lock.go` TTL < 1ms 报 `ErrDistLockConfig`、uncertain 状态、watchdog 代际 | 回退验证；确认 `Extend(newTTL)` 语义与文档一致 |
| M8 适配层输入与 panic 边界 | 已实现 | `kit/nats` 拒绝 nil handler / 空 subject 并 recover；`kit/mongo` 未知 WriteModel 带 index 报错，`stringifyID(nil)` 返回空串 | 回退验证 |
| M9 Lifecycle cleanup | 已实现 | `core/lifecycle` `EmitAll` + `errors.Join` | 回退验证 |

## P0：发布前必须关闭

1. **跨仓版本闭包**：生成工程必须只依赖正式 tag；release gate 同时验证 standalone/pure-tag 与 source-head，不允许 pseudo-version、local replace 或不存在的 Mod。具体流程见 [多仓研发与发布](DEVELOPMENT_WORKSPACE.md)。

## P1：正确性收敛

以下项目已完成，继续作为不可回退的回归门禁：

1. 聚合加载把全缺失/全 tombstone 识别为 NotFound，把 live/missing/tombstone 混合态识别为 Corrupt，并禁止发布半聚合 Entity。
2. 新 Data Engine 默认 WAL writer v2；v1 仅作为显式 reader-first 兼容开关，不得承载 Patch/Receipt。
3. Mongo 绝对过期字段使用 `expireAfterSeconds=0`，相对 TTL 字段使用正数，索引变更通过显式迁移策略完成。
4. 持久删除失败、延迟 admission、indeterminate、Remote 明确 delete intent 和 rollback 都有测试。
5. Saga 旧 worker 的 owner/token/status/expiry 围栏与 stale transaction no-op marker 有测试。
6. 可靠命令消费者的 ACK/NAK、permanent error、drain 与 redelivery 语义由故障测试覆盖；`ISyncBus` 保持明确“不因 handler error 重试”的版本化状态同步语义。

## 工程性门禁

- 每个修复必须包含最小回归测试；涉及并发、锁、WAL、Remote、Saga 时补 `-race` 或真实依赖故障验证。
- 每次发布运行 `go test ./...`、`go vet ./...`、source-head 四仓组合测试、生成工程 smoke test 与依赖闭包检查。
- 文档、Grafana、CI 和 Codegen catalog 必须与当前唯一 Data Engine 架构一致；删除代码时同提交删除失效配置、指标、脚本与文档。
- 性能工作只允许建立或收紧现有关键路径基线，不以性能名义引入第二套机制。

## 精简原则

- `checkpoint` Mod 与 standalone `nestwal` Mod 已删除；`nestwal` 只保留为 Data Engine 内部 WAL 库。
- 没有仓内引用不等于可直接删除。公开包先标记迁出/废弃并验证下游引用，公共 API 的物理删除放在主版本边界。
- 游戏类型相关扩展优先迁出 core/kit；Entity、Nest、Data Engine、Remote Entity、Saga、同步契约和必要基础设施适配保留在框架主线。
- 不维护平行实现、兼容空壳或只为未来设想存在的目录。
