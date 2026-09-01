# Core/Kit Feature 逻辑复评与整体修改方案

> 日期：2026-09-01  
> 范围：`cube-core` 与 `cube-kit` 中“core 定义契约、kit 提供实现”的框架能力  
> 不包含：停止维护的 `cube`、用户自行处理的 checkpoint/WAL 重复 snapshot（原 P1-5）、业务 handler 异步 Context 设计变更

## 1. 复评目的

上一轮审查列出了 14 项主要问题和若干低优先级问题，但其中混合了四类性质不同的结论：

1. 已有完整上层防护，但只检查了某个底层局部；
2. 真实逻辑缺陷；
3. API 或运维能力可以增强，但当前契约没有承诺；
4. 明确的框架设计取舍，被误判为缺陷。

本轮沿真实调用链重新检查每一项，判断标准是：

- 是否存在框架内或公开 API 可达的失败时序；
- 是否已有本地串行化、代际 token、CAS、事务、幂等收据或 fail-close 机制兜底；
- 最终影响是数据安全、消息语义、可用性、可观测性，还是仅 API 易用性；
- 修改是否符合现有设计，而不是为了“看起来更完整”引入新系统。

## 2. 修正后的总判断

上一轮把 Remote Entity 版本锁列为最严重 P1，是不准确的。Remote Entity 已经通过 `writeGate`、ownership 锁、每次获取递增的 `LockFence`、`MarkerEpoch`、`RouteEpoch`、`BaseVersion` CAS、Mongo 事务和 release fail-close 处理了经典的旧持有者覆盖问题。

Remote Entity 真正需要修改的是更窄的控制面问题：同一个锁对象跨 acquisition 复用 owner token，并且 unlock 的幂等证明错误地依赖“业务版本相等”。它是需要关闭的形式化缺口，但不是当前已有证据能够支持的 P1 数据损坏问题。

复评后，真正影响 8 分目标的主线收敛为：

1. Sync 消息身份、错误重试语义和输入失败策略不一致；
2. Replica 内外信封身份可以分叉；
3. ConfigData 跨 Store 回滚可能覆盖另一 Store 的成功发布；
4. Remote/Redis 锁的 acquisition 身份和状态机契约不完整；
5. Saga、Etcd watcher/election 的终止边界和终态错误不完整；
6. NATS、Mongo、lifecycle 等适配边界仍有少量 fail-open 或 cleanup 中断问题。

## 3. 上次问题逐项复评

| 原项 | 复评结论 | 修正后级别 | 是否进入必改 | 原因 |
|---|---|---:|---:|---|
| 1. Remote Entity 跨代 ABA | 部分成立，原定性过重 | P2 | 是 | 业务提交已有四维 fence + CAS；剩余的是 token 跨 acquisition 复用和 unlock receipt 证明不足 |
| 2. PatchSyncer 毫秒 Version 导致 JetStream 去重 | 成立 | P1 | 是 | 同 key、同 sid、同毫秒的两条 patch 确实生成同一 MsgID；第二条可被服务器去重 |
| 3. Sync 在 NATS/JetStream 下重试契约相反 | 成立 | P1 | 是 | core 明确声明 handler error 不重试；JetStream 实现却 Nak，切换 transport 改变业务语义 |
| 4. Sync/JetStream nil 输入 fail-open | 部分成立 | P2 | 是 | Mod 正常装配降低了触发概率，但公开构造器/API 确实一边静默成功、一边可能 panic；generic JetStream nil handler 会 ACK |
| 5. 永久错误不断 Nak、达到 MaxDeliver | 成立但原影响描述过重 | P2 | 是 | 只搁置毒消息，不会停止整个 consumer；仍缺终态分类、指标和恢复入口 |
| 6. Saga 停机未等待 drain | 成立但应降级 | P2 | 是 | `Drain()` 异步，随后立即 cancel；未 ACK 消息可重投，主要风险是并发停止和错误丢失，不是已证实静默丢消息 |
| 7. ConfigData 跨 Store 回滚覆盖新发布 | 成立 | P1 | 是 | `publishMu` 未覆盖 listener 阶段，旧提交失败后会无条件恢复全局槽位 |
| 8. Etcd LeaderChan/watcher | 部分成立 | P2 | 是 | 首次 Campaign 前取得 channel 会失联；watch channel 关闭本身可观察，原“必然永久静默陈旧”表述过重，但错误原因和重建信号不足 |
| 9. JetStream Sync durable 名称碰撞 | 成立 | P2 | 是 | 清洗和 200 字节截断均可能把不同 topic 映射为同一 durable；unsubscribe 也未从内部集合移除 |
| 10. Replica 内外信封身份分叉、忽略 context | 成立 | P1/P2 | 是 | 非零内层字段可以覆盖外层 Topic/Key/Version；Publish 的 context 参数目前没有真实作用 |
| 11. FeatureFlag Snapshot/Version 非原子 | 理论成立，但不属于当前承诺 | P3 | 否 | 当前能力明确是简单布尔开关表，仓库内没有原子版本快照消费者；可提供组合读取 API，但不是 8 分阻断项 |
| 12. Redis 普通锁生命周期 | 部分成立 | P2 | 是 | 无 fence/可双执行是明确 best-effort 契约，不应算 bug；但 `ttl<=0`、固定 token、重获与 watchdog 状态仍是可避免的生命周期缺陷 |
| 13. Admin 重名覆盖/宽松 JSON | 多数不成立 | P3 | 否 | 重名覆盖有明确回归测试，是现有 Replace 语义；审批/RBAC 明确由上层实现。严格 payload 可作为可选安全 API |
| 14. Health 串行、无单项强制超时 | 不应判为缺陷 | - | 否 | checker 应遵守 context；框架强行 goroutine 超时无法终止不合作 checker，反而会泄漏 goroutine |
| Mongo 未知 WriteModel / `<nil>` ID | 成立 | P2/P3 | 是 | 适配层应在进入 driver 前拒绝未知模型，并保持空 ID 的稳定语义 |
| NATS nil handler / callback panic | 成立 | P2 | 是 | wrapper 用非 nil 闭包绕过 nats.go 的 nil 校验，收到消息后才 panic；panic 未隔离会越过框架边界 |
| SyncMsg 无明确 wire version | 工程性缺口 | P3 | 暂缓 | 需要兼容迁移方案，不能直接改 JSON 名称破坏滚动升级 |
| Lifecycle cleanup 首错即停 | 部分成立 | P2 | 是 | init/config reload 适合 fail-fast；stopping/stopped 阶段应尽量执行全部清理并聚合错误 |

## 4. Remote Entity：真正需要修改的部分

### 4.1 已有机制保留不动

- 同进程写入由 wrapper `writeGate` 与 `ownershipMu` 串行化；
- `versionedLock.acquired` 阻止 unlock 未完成时直接重获；
- Redis 独立、不过期的 fence counter 为每次获取分配新 `LockFence`；
- Remote Commit 携带 `StateVersion + MarkerEpoch + LockFence + RouteEpoch`；
- Mongo 使用 `BaseVersion` 和上述代际条件更新，并在事务中提交 meta、数据、snapshot 与 outbox；
- 真正的 release error 会记录 fatal，后续 Remote Entity 写入 fail-close。

这些机制解决的是数据面陈旧写和提交竞争，不应重新设计，也不需要增加第五种版本维度。

### 4.2 必须关闭的控制面缺口

当前 owner token 在 `versionedLock` 对象创建时生成，后续 acquisition 复用。同一对象第一代的极端延迟 unlock，在形式上无法与第二代 owner 区分。

当前 unlock Lua 还使用：

```text
stored version == requested newVersion
```

证明“之前一次 unlock 已经落到 Redis”。但失败、无 dirty、ownership transition 和 `Close()` 都可能以原版本释放，因此业务版本不是唯一 unlock operation ID。

修改方案：

1. 每次 `TryLock` 生成新的 acquisition token；token 不再绑定锁对象生命周期。
2. 每次 `UnlockWithRetry` 生成一个稳定的 unlock operation ID；同一次内部/外部重试始终复用该 ID。
3. Redis hash 保存 `last_unlock_operation_id`；owner 不匹配时，只有 operation ID 相等才返回“前次已成功”。
4. 旧 unlock operation 即使晚于下一次 acquisition 到达，也只能读取自己的 receipt，不能删除新 owner。
5. `newVersion` 只承担业务版本职责，不再承担网络重试幂等键职责。
6. core 增加公开的 `IFencedVersionedLock` 契约，包含 `Fence() uint64`；kit Remote Entity 使用 core 契约，不再使用私有 `lockFenceProvider` 鸭子类型。
7. 当前 `IVersionedLockFactory` 的兼容接口先保留；Remote Entity 对不支持 fence 的实现启动/创建时 fail-closed。下一主版本再考虑把 factory 返回类型收紧。

必须新增的确定性测试：

- unlock 已执行但响应丢失，重试凭同一 operation ID 成功；
- 业务版本相等但 operation ID 不同，不得误报本次成功；
- 第一代 unlock 延迟到第二代 acquisition 后执行，不得删除第二代 owner；
- wrapper 淘汰/重建后旧 token 不得影响新 wrapper；
- 高 fence 与低 fence 的 Mongo 提交竞争最多一个成功，失败方得到版本冲突并进入既有隔离流程。

## 5. 合并后的必要修改包

### M1：统一 Sync 的消息身份与消费语义（P1）

修改范围：`core/sync`、`kit/sync`、`core/nats`、`kit/nats`。

1. 为 `SyncMsg` 增加独立的稳定投递 ID；业务 `Version` 不再兼任 JetStream 去重键。
2. JetStream MsgID 优先使用投递 ID；旧消息没有该字段时兼容原 tuple 算法。
3. PatchSyncer 每次 Publish 生成进程唯一 epoch + 单调序列的投递 ID；不得使用 `UnixMilli` 作为唯一身份。
4. PatchSyncer 的业务 Version 由显式 `VersionOf` 提供；没有业务版本的 transient patch 不伪装成权威数据版本。
5. JetStream Sync 捕获 handler error 后记录指标/日志并返回成功 ACK，与 core 的“不重试”契约一致。
6. generic JetStream 保留 `error => Nak` 的既有语义；不要为了 Sync 改坏 Saga/RPC。
7. Publish/Subscribe 对 nil bus/client/message/handler、空 topic、非法 sid/key 统一返回错误，不允许 no-op success 或延迟 panic。
8. durable 名称使用“可读前缀 + 原始 topic 稳定哈希”，截断时保留哈希后缀。
9. unsubscribe 必须从 bus 的活动 subscription 集合中移除；Stop 只处理仍活跃对象。
10. 现有同步调用 handler 的 fake JetStream 测试改成发布/消费分离模型，避免把异步消费者错误错误地传播给发布者。

验收：同毫秒 10 万次同 key 发布没有 MsgID 重复；NATS 与 JetStream 对 handler error 的外部语义一致；transport 切换不改变业务执行次数。

### M2：收紧 Replica 信封和 Context（P1/P2）

修改范围：`core/replica`、`core/sync`、`kit/sync`。

1. 内层 `Envelope.Topic/Key/Version` 允许为零值并由外层补齐；只要非零，就必须和外层完全一致。
2. 校验 `Op`、Key、Version、delete payload 等基本不变量；非法信封不得进入 Store。
3. `Start` 配置不完整必须返回错误，不再静默禁用。
4. 在 core 暴露可选的 context-aware publisher capability；JetStream 实现使用调用方 context，普通 NATS 至少在准入前检查取消。
5. `Publish`/`PublishDelete` 不再接受 context 后立即丢弃。

验收：伪造内层 topic/key/version 的消息全部 fail-closed；取消 context 不产生新发布；兼容内层字段为空的旧消息。

### M3：ConfigData 全局发布槽位使用条件回滚（P1）

修改范围：`core/configdata`、`core/ctx`。

1. Store 自身 `current` 仍按原事务回滚。
2. 全局 `DefaultStore` 与 runtime config 只有在仍指向本次 `target` 时才能恢复为 `prev`。
3. 如果另一个 Store 已成功发布，新失败事务只回滚自己的 Store，不覆盖后继全局发布。
4. 两个全局槽位的所有权判断和更新必须在同一 `publishMu` 临界区完成。
5. 发生“本次已被后继发布取代”时记录冲突指标，不能把它误报为回滚失败。

验收：使用 barrier 精确构造 A publish → B publish success → A AfterApply fail，最终全局必须保持 B，A 自身恢复旧 snapshot。

### M4：关闭 Remote Entity 锁代际证明缺口（P2）

按第 4 节实施 acquisition token、unlock operation ID、公开 fence capability 和对抗性测试。该修改只增强锁控制面，不改变 Remote Commit、Nest、WAL、Mongo 四维 CAS 或 handler 模型。

### M5：Saga/JetStream 终态与停机顺序（P2）

修改范围：`core/nats`、`kit/nats`、`kit/saga`。

1. Saga Stop 捕获 subscription 引用，先发起 drain，并在 shutdown context 内等待全部 `Closed()`。
2. drain 完成后再 cancel run context、停止 Engine、等待 run goroutine。
3. 聚合 drain timeout、Engine Stop 和 run error；不得忽略 `engine.Stop` 返回值。
4. Start/Stop 增加互斥状态，重复 Start、Stop 与并发 Stop 结果确定。
5. 不把 generic JetStream 改成庞大的新结果体系；先在现有 `error` 契约上增加明确的 permanent/terminal 分类。
6. 临时错误继续 Nak/有界退避；协议版本、JSON、schema 等永久错误 Term，并产生指标和可检索失败记录。
7. 达到 MaxDeliver 的消息必须有 advisory/指标和运维查询入口；修正文档，明确是单条消息终止，不是 consumer 整体停止。

验收：阻塞中的 start/completion handler 在 drain 后完成；超时明确返回；永久错误不重复执行五次；临时错误仍重投；毒消息可定位。

### M6：Etcd election/watch 终态契约（P2）

修改范围：`core/etcd`、`kit/etcd`。

1. 第一次 Campaign 使用 `NewElection` 时已经暴露的 leader channel，避免 Campaign 开始时无条件替换。
2. 文档明确后续任期必须在 Campaign 成功后重新取得 `LeaderChan`。
3. `Leader()` 保留原始 context、权限和网络错误链，同时允许 `errors.Is(err, ErrElectionNoLeader)`。
4. watcher 检测 canceled response、compaction 和底层 channel close，记录终止原因。
5. 以可选扩展接口提供 `Done()/Err()`，不破坏现有 `IServiceWatcher`；调用方据此重新 Discover 并建立 watch。
6. JSON 损坏事件计数并记录 key/revision，不能静默跳过。

验收：Campaign 前取得的首任 channel 能正确关闭；取消与网络错误可区分；watch 终止后调用方可以完成“重新全量发现 + 重建 watch”。

### M7：Redis best-effort 锁状态机（P2）

修改范围：`core/redis`、`kit/redis`。

保留“无 fencing、只用于可容忍双执行”的设计边界，不把普通锁升级成 Remote Entity 锁。

需要修改：

1. `ttl <= 0` 在 Acquire 时明确报配置错误，禁止生成永久锁。
2. owner token 按 acquisition 生成，而不是按对象生成。
3. 增加本地 acquired/uncertain 状态；释放结果不确定时禁止同对象直接重获。
4. Release/Extend 在发请求前冻结本次 acquisition token，避免并发重获改变参数。
5. AutoExtendLock 拒绝重复 Acquire 覆盖活动 watchdog；Stop/Release 只等待对应代际 watcher。
6. 明确手工 `Extend(newTTL)` 是一次性延长还是更新 watchdog 策略；实现和文档必须一致。

这不是增加 correctness-grade fencing；需要防止陈旧业务写的调用仍必须使用 `IVersionedLock`。

### M8：适配层输入与 panic 边界（P2/P3）

1. NATS `Subscribe/QueueSubscribe` 在包装 callback 前拒绝 nil handler 和空 subject。
2. callback panic 在 kit 边界转换为结构化错误、指标和可配置 fatal 通知，不能直接穿透第三方库 goroutine。
3. Mongo `BulkWrite` 遇到未知 `WriteModelType` 立即返回带 index 的错误，不把 nil model 传给 driver。
4. Mongo Update/Replace 在无 upsert 时返回空 `UpsertedID`，不返回字符串 `"<nil>"`。

### M9：Lifecycle cleanup 执行策略（P2）

1. `app.init`、`mods.started`、`service.started`、`config.reload` 保持 fail-fast。
2. `service.stopping`、`service.stopped` 执行全部 hook，panic-contained 后用 `errors.Join` 聚合。
3. 如果不希望改变 `Emit` 的既有契约，可增加 `EmitAll`，由 App cleanup 路径显式调用。

## 6. 明确不进入本轮的项目

### 6.1 FeatureFlag

不把它改造成远程配置、灰度规则引擎或强一致版本存储。可低成本增加 `SnapshotWithVersion()` 和严格 Set/Replace API，但不作为鲁棒性 8 分门禁。

### 6.2 Admin

不直接删除重名覆盖语义，因为现有测试明确允许替换。后续可增加 `RegisterStrict`/`Replace` 和 `DecodePayloadStrict`，由高风险命令选择；审批、RBAC、审计仍属于上层控制面。

### 6.3 Health

不通过为每个 checker 启动不可终止 goroutine 来制造“强制超时”。继续要求 checker 遵守 context；可补充慢检查指标、重复注册诊断和可选 unregister。

### 6.4 Sync wire 大迁移

不直接给全部旧字段改 JSON tag 或删除旧字段。wire version、字段命名迁移必须提供双读、单写和滚动升级测试，放入兼容性工作包，不和本轮逻辑修复混做。

### 6.5 Checkpoint/WAL snapshot 重复落地

按用户要求，本问题由用户自行修改，本方案不改、不合并、不重复实现。

## 7. 实施顺序

### 阶段 A：先写失败测试和契约（1 个 PR）

- 为 M1-M7 建立确定性时序测试；
- 每个测试必须先能在当前实现上失败，避免“修复”不可达问题；
- 更新 core 接口注释，明确 Sync error、普通锁 best-effort、fenced lock 和 watcher 终态。

### 阶段 B：数据/消息正确性（3 个 PR）

1. M1 Sync identity + transport 语义；
2. M2 Replica identity/context；
3. M3 ConfigData 条件回滚。

这是最直接的 P1 增分组。三项全部完成前，不上调鲁棒性评分。

### 阶段 C：代际和生命周期（3 个 PR）

1. M4 Remote Entity acquisition/unlock operation；
2. M5 Saga drain + JetStream terminal；
3. M6 Etcd election/watch terminal。

### 阶段 D：公共适配边界（2 个 PR）

1. M7 Redis best-effort lock state machine；
2. M8 NATS/Mongo + M9 lifecycle cleanup。

### 阶段 E：真实依赖验收和发布门禁（1 个 PR）

- core、kit 各自 `go test ./...`、`go vet ./...`、关键包 `go test -race`；
- Redis：延迟命令、响应丢失、TTL 过期、重获；
- NATS JetStream：真实异步 publish/consume、dedup、Nak/Term、MaxDeliver、drain；
- etcd：session loss、watch cancel、compaction/relist；
- Mongo replica set：双提交 CAS、事务回滚、ConfigData 并发只需单测；
- source-head 与已发布 core/kit 双组合兼容；
- 不使用停止维护的 `cube` 作为通过或失败证据。

## 8. 完成标准

只有同时满足以下条件，才能把这批修改计入“鲁棒性与工程性达到 8 分”的证据：

1. NATS 与 JetStream 的同一 Sync handler 在成功、业务错误、永久协议错误下具有一致且文档化的外部语义；
2. 任意两条合法 patch 不会因墙钟分辨率或回拨共享投递 ID；
3. Replica 不可能把 topic A/key 1 的外层消息应用成 topic B/key 2；
4. ConfigData 后继成功发布不会被前驱失败回滚覆盖；
5. Remote Entity 的旧 unlock operation 无法删除新 acquisition；
6. Saga Stop 在 deadline 内完成 drain，或明确返回未完成错误；
7. Etcd watcher/election 的终止原因可观察并可重建；
8. Redis 普通锁不会因非法 TTL 形成永久锁，也不会覆盖活动 watchdog；
9. 所有新增并发测试在 race 下稳定通过，真实依赖测试不依赖同步 fake 模型；
10. 没有改变 handler 异步 Context 原则：异步只走 Nest 或 core worker，新 Context 由框架创建，业务数据显式作为参数传递。

## 9. 评分影响

本轮复评的价值不是增加问题数量，而是删除错误扣分并把修改集中到可证明的语义缺陷：

- Remote Entity 经典 ABA 已有设计，不再作为 P1 扣分；
- FeatureFlag、Admin、Health 的设计取舍不再冒充阻断缺陷；
- 真正的 P1 集中在 Sync、Replica、ConfigData；
- P2 集中在锁控制面、Saga/Etcd 生命周期和适配层 fail-closed。

完成 M1-M9 并通过真实依赖门禁后，才有充分依据把代码鲁棒性与工程性评为 8 分以上；只改注释、只跑 mock 单测或继续扩大功能面都不计分。

## 10. 主要代码证据索引

- Sync“不重试”契约：`cube-core/sync/sync.go`；JetStream Sync 返回 handler error：`cube-kit/sync/jetstream_sync.go`。
- PatchSyncer 墙钟 Version：`cube-core/sync/patch_syncer.go`；JetStream MsgID tuple：`cube-kit/sync/jetstream_sync.go`。
- generic JetStream nil handler ACK、error Nak：`cube-kit/nats/jetstream.go`。
- Replica 内层字段覆盖外层、context 被丢弃：`cube-core/replica/replica.go`。
- ConfigData 发布后释放 `publishMu`、失败时无条件恢复全局槽位：`cube-core/configdata/configdata.go`。
- Remote lock 固定 token、unlock version 幂等分支：`cube-kit/remote_entity/versioned_lock.go`、`versioned_lock_lua.go`。
- Remote Commit 四维信息与 Mongo CAS：`cube-core/entity/remote_protocol.go`、`cube-kit/remote_entity/batch.go`、`mongo_committer.go`。
- Saga drain/cancel/Stop 顺序：`cube-kit/saga/mod.go`；JetStream `Closed()` 语义：`cube-core/nats/jetstream.go`、`cube-kit/nats/jetstream.go`。
- Election channel replacement和错误压平：`cube-kit/etcd/election.go`；watch 终止：`cube-kit/etcd/discovery.go`。
- Redis best-effort 契约与实现：`cube-core/redis/lock.go`、`cube-kit/redis/lock.go`。
- NATS callback 包装：`cube-kit/nats/client.go`；Mongo WriteModel 转换：`cube-kit/mongo/collection.go`。

## 11. 实施结果（2026-09-01）

M1–M9 已按复评后的边界落地：

- M1：Sync 增加独立 `MessageID`、可选 `IContextPublisher`、Patch 业务 `VersionOf`，NATS/JetStream handler error 统一为记录后不重试，durable 名称增加稳定 hash；
- M2：Replica 以外层信封为权威，拒绝内外 topic/key/version 分叉，并传递 publish deadline；
- M3：ConfigData 回滚只在全局槽位仍指向本次 target 时恢复，跨 Store 后继发布不会被覆盖；
- M4：versioned lock 每次 acquisition 更换 owner token，unlock 以 operation receipt 幂等；core 暴露 `IFencedVersionedLock`；
- M5：Saga drain 等待 consumer 关闭后才取消 worker；JetStream 支持 permanent/MaxDeliver `Term` 与终态指标；
- M6：修复首轮 Election channel 替换，保留 Leader 底层错误，watcher 暴露 `Done/Err`；
- M7：普通 Redis 锁补齐 idle/acquired/uncertain 状态机与 watchdog 配置门禁；
- M8：NATS callback/空依赖边界 fail-closed，Mongo 拒绝未知 BulkWrite 类型且不再返回 `"<nil>"` UpsertedID；
- M9：cleanup 使用 `EmitAll`，单个 hook 失败不阻断后续释放。

验收结果：core 与 kit 的 `go vet ./...` 通过；kit `go test ./...` 通过；core 全量测试除沙箱内 Windows 临时目录真实路径权限外均通过，configdata 在非沙箱环境复跑通过；M1–M9 相关关键包 `go test -race` 全通过。Redis 8.8 本机真实进程验证了“旧持有者在 TTL 过期后 Release 不会删除新 acquisition”，测试由 `ROOST_REDIS_TEST_ADDR` 门控；NATS/etcd/Mongo 本机无服务端，真实依赖门禁仍需 CI 环境完成。未修改 checkpoint/WAL snapshot 逻辑，也未改变 handler 异步 Context 原则。

发布必须按 core → kit 顺序：kit 为兼容当前已发布 core v1.9.1，对新 `SyncMsg.MessageID` 暂用滚动升级读取；core 新版本发布并升级 kit 依赖后可改为静态字段访问。两仓直接拼入同一 `go.work` 时发现并修复了历史 `google.golang.org/genproto` 单体包与新拆分包冲突：kit 明确排除两个 2018/2019 单体旧版本后，source-head 组合 `go test ./...` 已全通过。
- Lifecycle phase 与首错返回：`cube-core/lifecycle/lifecycle.go`。
