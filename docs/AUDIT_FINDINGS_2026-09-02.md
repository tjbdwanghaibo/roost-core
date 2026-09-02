# core / kit 全量审核记录（2026-09-02）

本文是一次面向发布的全量审核的**最终记录**，不是过程日志。它按"发现"组织：
每条发现在一处写完机制、触发路径、实证、修复和回归测试，读者不需要按轮次拼接。
方法论、覆盖范围账本和穷举扫描的负面结果放在后半部分。

**结论**：16 条发现全部已修复，每条都有回归测试，且每条都通过"临时回退修复 →
测试变红"验证过不是空断言。core / kit / codegen / skill 四仓 `gofmt`、`go vet`
（kit 含 `-tags integration`）、`go test`、关键包 `-race` 全绿。

## 审核纲领（四条轴）

每一个包都按这四条轴过一遍，而不只是"能不能跑通"：

1. **实现与设计意图一致**：以包注释、README、CHANGELOG 里写下的承诺为基准，
   检查实现是否真的提供了那个语义——而不是提供了一个恰好让测试通过的近似。
2. **实现不是被测试塑形的**：警惕只为满足断言而存在的特殊写法（写死的返回值、
   只在测试路径成立的前置条件、恒为常量的指标）。
3. **测试真的覆盖多种 case**：区分"有测试"和"测试会失败"。名字承诺了某个属性
   却没有断言它的测试，比没有测试更有害——它让那块代码看起来有保护。
4. **性能被考虑过**：热路径上的无索引查询、每次调用重建的反射计划、持锁远端调用。

## 发现总表

| ID | 严重度 | 位置 | 缺陷类别 | 回归测试 |
| --- | --- | --- | --- | --- |
| F1 | 严重 | kit `dataengine/mongo_store.go`、`projector.go` | 快慢路径语义不对称 | `dataengine`（3 条） |
| F2 | 高 | kit `dataengine/entity_repository.go` | 可重试回调外累积状态 | `dataengine`（参数化 0/1/3 次重试） |
| F3 | 中 | kit `dataengine/outbox_store.go` | 热路径无索引全表扫描 | `dataengine`（索引断言 + 慢 ticker） |
| F4 | 中低 | kit `nats/rpc.go` | 恒为常量的指标 + 空断言 | 改为直接测属性（指标已删） |
| F5 | 低 | core `dataengine/load.go` | 静默吞错 | `dataengine`（降级可见性） |
| F6 | 中 | core `dataengine/lease_fence.go`、kit `dataengine/mongo_store.go` | 跨包字面量耦合 | `dataengine`（谓词共用 + 计数器） |
| F7 | 低 | core `entity/entity_guard.go` | 导出可变内部状态 | `entity`（快照隔离） |
| F8 | 低 | CI | 集成测试不在门禁内 | `go vet -tags integration` |
| F9 | 中 | core `cache/read_through.go` | 持锁远端调用无上界 | `cache`（有界等待） |
| F10 | 中 | kit 五处手写 Mongo fake | 测试替身不求值 filter | `internal/mongofake`（13 条自测） |
| F11 | 低 | core `configdata`、`nest` | 测试名承诺的属性未被断言 | 两条空断言已补实 |
| F12 | 中低 | core `cache/ref_hmap.go` | 每次操作重建反射计划 | `cache`（key 格式逐字节钉死） |
| F13 | 低 | core `nest/pipelined_completion.go` | 释放义务无 defer 保障 | `nest`（幂等释放） |
| F14 | 中 | kit `ops/ops_mod.go` | 非常量时间比较密钥 | `ops`（常量时间 + 空 token 拒绝） |
| F15 | 低 | core `app/config_validation.go` | 生产校验漏 ops 绑定地址 | `app`（回环校验 + opt-out） |
| F16 | 低 | core `bus/bus.go` | 持生命周期锁排空 | `bus`（锁内取走、锁外排空） |

---

## F1（严重）快路径与批路径的 marker 不对称 → 崩溃恢复变成永久启动失败

**位置**：kit `dataengine/mongo_store.go` 的 `Project` 单变更快路径与 `ProjectBatch`
短缺分支；`dataengine/projector.go` 的 `projectSegment` 与 `replayPass`。

**机制**：`Project` 的单变更快路径直接 `applyMutation` 后返回，**不写 transaction
marker**（省一次往返，是有意的设计）。但 `ProjectBatch` 只认 marker：找不到 marker
就把记录纳入 bulk，而它的 CAS 过滤器 `{_id, _version: ExpectedVersion}` 对已应用的
文档匹配不到（文档已是 `NextVersion`），于是 `MatchedCount != len(models)` →
`ErrProjectionConflict` → `isFatalProjection` 判定**致命**。

`projectSegment` 对 `len(segment.records)==1` 一律走快路径——**在线运行时每次
`TransactionReleased` 只唤醒一条记录，单条投影是常态**，所以"无 marker 的已应用
记录"在生产中普遍存在。

**触发路径**：

1. 一条 batch-eligible 记录（单条普通变更、无 effects/receipts/remote）在线走快路径
   投影，Mongo 已提交，无 marker。
2. WAL ACK 没落盘。`wal.Ack` 确实 fsync，所以窗口是 Mongo 提交与 checkpoint fsync
   之间的 kill -9 / 掉电；**或者 `Ack` 本身报错**（ENOSPC / EIO）——此时进程还活着，
   退避重试时新记录已到，同样会成批。
3. 重放把它和邻居收成一个多记录段 → `ProjectBatch` → 无 marker + CAS 打空 → 致命冲突。
4. 启动期路径是 `runtime.Start` → `Projector.Flush` → 返回错误 → `Mod.Start` 失败 →
   **服务拒绝启动，且每次重启都复现**，必须人工改库才能恢复。

**实证**：

```
CONFIRMED: batch replay of a fast-path-projected record is fatal:
dataengine mongo: fatal projection version conflict: batch matched=1 upserted=0 expected=2
```

同时确认前提成立：快路径确实没写任何 marker（`inserted=0 bulk=0`）。

**修复**：关键判断是**不给快路径补 marker**——省掉那次往返正是快路径存在的意义，
作者的取舍是对的；错的是批路径把自己无力判定的情况直接判成了最严厉的结论。

新增导出哨兵 `ErrProjectionBatchNeedsPerRecord`（导出因此第三方
`BatchProjectionStore` 能用同一协议）：

- `ProjectBatch` 在**批量匹配短缺**或**upsert 撞 `_id`**（已应用的 Put 正是这个形状）
  时返回该哨兵，而不是 `ErrProjectionConflict`。
- `replayPass` 捕获后闩上 `perRecord`，该 segment 余下记录改走单记录路径——
  `classifyNoMatch` 比对存储版本与 `_last_tx`，能区分"已应用的幂等重放"与"真实冲突"。
  闩住而非只重试失败单元，避免重复发出注定还会延后的批量写。
- `isFatalProjection` 显式豁免该哨兵：它是路由信号，不是判决。
- **真实冲突语义不变**：逐条重投后 `Project` 仍返回 `ErrProjectionConflict`，仍然致命。
  改的只是"由有能力判定的一方来判"。

代价只在罕见的重放/冲突路径上支付（一次恢复中的一个 segment 退化为逐条），
正常批量恢复路径零开销。

**顺带修好的测试替身缺口**：写 F1 回归测试时，"延后的批必须一个字节都没写"这条
断言暴露了 `internal/mongofake` 没有事务语义。已补上：`WithTransaction` 按快照回滚
（含事务内新建的集合），每次重试从事务前状态重跑。`ProjectBatch` 的原子性是文档
承诺的设计属性，此前无法被测试。

## F2（高）可重试回调外累积状态 → 健康数据被误判为损坏

**位置**：kit `dataengine/entity_repository.go` 的 `readAggregate`。

```go
loaded := make([]loadedDAO, 0, len(builder.DaoBuilders))   // ← 回调外
err := repository.store.ReadConsistent(ctx, func(readCtx context.Context) error {
    missing, tombstones := 0, 0                             // ← 回调内，已正确重置
    ...
    for _, existing := range loaded { /* 重名即报 corrupt */ }
    loaded = append(loaded, ...)
```

**机制**：`ReadConsistent` → `session.WithTransaction`，kit 的 session 直接委托 driver
的**重试版** `WithTransaction`。driver 在 TransientTransactionError /
UnknownTransactionCommitResult 上会**重新调用回调**（副本集切主、snapshot 不可用、
网络抖动）。第二次进入时重名守卫立刻命中上一轮条目 →
`ErrEntityAggregateCorrupt: duplicate DAO resource "..."`。即使没有重名守卫，
`len(loaded) != total` 也会误判。

危害是**误诊**：数据完好，但实体加载失败并报"数据损坏"，且恰好发生在故障切换
这类最需要平稳降级的时刻。

代码库本身清楚这条规则——`missing`/`tombstones` 就重置在回调内，saga 的 mongo store
专门写了注释、`Project` 对 `transactionSkipped` 也做了同样处理——唯独此处漏了。

**实证**：

```
CONFIRMED (attempts=2): healthy aggregate reported corrupt:
dataengine repository: entity aggregate is incomplete or corrupt: duplicate DAO resource "repository_profile"
```

零重试基线加载正常，一次重试即失败。

**修复**：`loaded`/`remoteVector` 的重置移入回调首行，并把"回调会被自动重试、
因此累加器必须在回调内重置"写进注释。回归测试用 `transientRetries` 参数化 0/1/3 次
重试，断言聚合始终完整且回调调用次数正确。

## F3（中）outbox backlog 探针在热路径上做无索引全表扫描

**位置**：kit `dataengine/outbox_store.go` 的 `Backlog`，由 `outbox_worker.go` 的
`RunOnce` 末尾**无条件调用**。

`Backlog` 每次做两件事：`CountDocuments(bson.M{})`（driver 的 `countDocuments` 走
聚合，不是 O(1) 元数据）+ `find({}).sort({created_at:1}).limit(1)`。而
`EnsureInfrastructure` 只建了 `claim_due` 与 `uniq_effect`，**没有 `created_at` 索引**。

默认 2 workers × 100ms 轮询 = 每秒 20 次全量计数 + 20 次无索引求最小值，打在投影
正在写的同一个 Mongo 上。开销随积压线性增长——**探针会放大它所要度量的积压**，
`MaxPending`/`MaxOldestAge` 硬闸门恰在最需要时最迟钝。

**修复**：补 `created_at` 索引；backlog 探测移到独立的慢 ticker，不再挂在每次
`RunOnce` 上。

## F4（中低）恒为 0 的指标 + 结构上不可能失败的断言

**位置**：kit `nats/rpc.go` 在构造时 `SetGauge("nats.rpc.duplicate_completion", nil, 0)`，
全仓再无任何自增点（唯一另一处引用是测试里断言它 `!= 0`）。

CHANGELOG 中"exactly-once completion 不仅被实现，也能被监控"这条没有活信号支撑，
那条测试也是结构上不可能失败的空断言。现状比没有指标更糟——它给出虚假的安全感。

**修复过程本身是一条教训**：第一版实现把"每一次第二次到达"都计成重复。跑 10 万
待处理任务的测试时报出 100000 次重复，才发现 `worker.Worker.safeHandle` 既调
`handler(task)` 又 `defer task.OnRelease()`——**第二次到达是正常路径**，这个指标
在原理上不可伪证。最终结论是删除指标与那条断言，改用 `sync.Once` → 到达计数，
并用直接测试覆盖"恰好完成一次"这条真实属性。

## F5（低）非 strict 模板静默吞错

**位置**：core `dataengine/load.go`。

```go
if err := template.OnLoad(doc); err != nil && template.Strict {
    return fmt.Errorf(...)
}
return nil
```

`Strict=false` 时错误被完全丢弃：无日志、无计数。字段改名之类的系统性失败会让整张表
静默加载 0 个实体，与框架自身"降级必须可见"的惯例（`cache.refhmap.write_degraded_total`、
`metrics.series.dropped`）相悖。**修复**：非 strict 路径改为记录 + 计数，语义不变。

## F6（中）lease fence 的跨包字面量耦合 —— 漂移即静默失效

**位置**：core `dataengine/lease_fence.go` 与 kit `dataengine/mongo_store.go`。

fence 的字段名、状态值和 `_id` 构造在两个仓库里各写一遍字面量。任何一侧改名，
另一侧的过滤器就静默匹配不到——fence 变成 no-op，而 no-op 的 fence 不报错。

**修复**：让 fence 自己产出谓词。core 侧新增 `LeaseFenceFieldOwner/Token/Digest/
Status/LeaseUntil`、`LeaseFenceStatusPending` 与 `func (fence LeaseFence) Predicate(now
time.Time) bson.M`；kit 侧 `leaseFencesMatch` 改用 `fence.Predicate(now)`。同时给
未命中加 `dataengine.fence.skipped.total{resource}` 计数器——**静默 no-op 变可见**，
这一条无论字面量是否统一都该做。

## F7（低）`EntityGuard.Entities()` 导出可变的锁作用域账本

**位置**：core `entity/entity_guard.go`。`Entities()` 直接返回内部映射，调用方可以
改写守卫账本。

**修复**：`Entities()` 改为返回快照；新增无分配的 `Guarded(id)` / `GuardedCount()`，
框架内 6 处调用点（`entity/subject_sync.go`、`nest/cast.go`、`nest/group_lock.go`、
`nest/nest_dispatch.go`）全部迁移到新 API，热路径不再为只读判断分配。

## F8（低，流程）集成测试不在任何自动门禁内

带 `//go:build integration` 的文件被 `vet`/`build`/`test ./...` 全部排除，因此可以
腐烂很久而无人发现。**修复**：CI 加一步 `go vet -tags integration ./...`（零成本防腐）。

## F9（中）远端写缺上界 + 持锁调用

**位置**：core `cache/read_through.go` 的 `Set` / `Delete`：持锁调用远端 L2，
且远端调用没有超时上界。一次远端卡死会把整条读穿透链路挂住。

**修复**：`ReadThroughOptions` 新增 `RemoteTimeout`，`remoteContext(ctx)` 在未配置时
回退到 `LoadTimeout`；`Set`/`Delete` 一律 `defer cancel()`；远端失败进入 `remoteError`
计数。回归测试用 goroutine + `select` 做**有界断言**——第一版写成了靠 `go test`
15 秒超时来判失败，那正是 F11 批评过的坏信号形态，已重写。

## F10（中，系统性）Mongo fake 不求值 filter —— 查询语义整体不受测试保护

kit 里有五处手写的 Mongo 替身，它们**不求值 filter**：无论过滤条件是什么都返回
预置文档。这意味着所有基于 CAS 过滤器、租约谓词、去重索引的语义——版本 CAS、
fence、outbox 唯一性、tombstone 防复活——**在测试里全是假通过**。F1 之所以能潜伏到
生产形态，直接原因就是它。

**修复**：新建 `internal/mongofake`（1628 行 + 13 条自测），一个真正求值 filter 的
内存 Mongo，替换全部五处手写替身。设计要点写在包注释里：

- **不支持的构造返回 `ErrUnsupported`，而不是静默匹配**——替身的沉默是 F10 的根因。
- `Client.TransientRetries` 复现 `ISession.WithTransaction` 文档承诺的自动重试
  （F2 的回归测试依赖它）。
- `_id` 等值/`$in` 走索引快路径（`idCandidatesLocked`），避免基准测试被替身的
  线性扫描扭曲。
- `normalizeScalar` 做**真实的 bson round-trip**，而不是手写的加宽规则——
  第一版手写规则漏了 `int8/int16/uint8/uint16`，导致一个 `uint8` 字段存成 bson int32
  后比较为假；用 round-trip 后替身不可能与 driver 分叉。
- `WithTransaction` 快照/恢复全部集合，abort 真的回滚（F1 的原子性断言依赖它）。

顺带发现两处替身与真 Mongo 的语义差：bson 日期是**毫秒精度**，一条按纳秒断言
oldest-age 的测试因此改为容差窗口——这不是替身的缺陷，是它忠于 Mongo。

## F11（低）测试名承诺的属性未被断言

两条测试的名字承诺了某个属性，测试体里却没有断言它——`configdata` 的一条与
`nest` 的一条。这类测试比没有测试更有害：它让那块代码看起来有保护。

**修复**：前者断言"读到新标志值 ⇒ 版本 ≥ 该值写入时的版本"；后者断言 `Prepare` 在
`mu.Unlock()` 之前确实未完成（检查 `asyncDone` 仍开启）、`err` 为 nil，并给等待加上
有界超时（挂起时快速失败并给出明确信息，而不是靠框架超时）。

## F12（中低）每次操作重建反射计划

**位置**：core `cache/ref_hmap.go`。每次操作都重新走一遍反射建计划，而 `base`
（key 前缀）被烘进节点里，所以不能简单加缓存。

**修复**：类型树与 key 前缀分离——`refHMapNode.suffix` 只描述结构，
`refHMapPlan{root, base}` 承载前缀；`layout()` 用 `sync.Once` 每个 store 只建一次树
（错误也一并缓存）。key 格式由一条回归测试**逐字节钉死**，确保重构没有改变线上
已有数据的 key。

## F13（低）完成链的释放义务没有 defer 保障

**位置**：core `nest/pipelined_completion.go`。降级分支上"必须释放完成链"是一条
靠调用方记得执行的**义务**，不是保障。

**修复**：`completionOrder.release()` 用 `releaseOnce sync.Once` 改为幂等；
`prepareCompletion` 额外返回 `order.release`，调用方（`nest/rollback.go`）在
`!deferred` 时 `defer releaseOrder()`。约三行，把"义务"变成"保障"。

## F14（中）admin token 非常量时间比较

**位置**：kit `ops/ops_mod.go` 的 `authorized()` 用 `==` 比较 admin token。

**修复**：`crypto/subtle.ConstantTimeCompare`（经 `secretEqual`）；顺带让
`authorized` 在配置 token 为空时直接拒绝（纵深防御——空 token 绕过当前不可达，
但那依赖 `Init` 与 `OpsMod` 直接构造之间的约定）；`bearerToken` 按 RFC 7235 改为
大小写不敏感。

## F15（低）生产校验不覆盖 ops 绑定地址

**位置**：core `app/config_validation.go`。生产模式校验了 dev token，却没校验 ops
监听地址——一个绑到 `0.0.0.0` 的运维端口不会被拦下。

**修复**：新增 `validateProductionOpsExposure` + `isLoopbackListenAddr`，生产模式要求
`ops.addr` 为回环地址，除非显式 `ops.allow_public_addr`（沿用 `allow_dev_token` 的形态）。

## F16（低）Start 失败清理仍持生命周期锁排空

**位置**：core `bus/bus.go`。`Start` 的失败分支在持生命周期锁的状态下排空资源，
与 `StopWithContext` 的做法不一致。

**修复**：`Start()` 变成一层薄壳，调用 `startLocked()` 返回一个排空闭包，在锁释放后
执行；`stopLocked` 变为 `detachLocked`，返回 `func() error { return b.stopResources(...) }`。
两条路径现在共用同一个"锁内取走资源、锁外排空"的形态。

---

## 缺陷类别与它们的实际产出率

16 条发现全部落在下面 8 个类别里。这份清单的用途是下一次审核的检查表——
它是经验数据，不是理论分类：

| 缺陷类别 | 产出 | 说明 |
| --- | --- | --- |
| 临界区里的无界远端调用 | F9 | 持锁 + 无超时 = 一次远端卡死挂住整条链路 |
| 非常量时间的密钥比较 | F14 | |
| 空洞测试 / 过于宽容的替身 | F4、F10、F11 | 产出最多的一类；替身的沉默尤其危险 |
| 可重试回调外累积的状态 | F2 | driver 会重放回调，累加器必须在回调内 |
| 跨包字面量耦合 | F6 | 漂移不报错，只是静默失效 |
| 静默吞掉的错误 | F5 | |
| 指标钉死在常量上 | F4 | 比没有指标更糟 |
| 释放义务没有 defer 保障 | F13、F16 | |
| 快慢路径语义不对称 | F1 | 唯一的严重级；两条路径对同一状态的判据不同 |
| 热路径上的重复计算 / 无索引查询 | F3、F12 | 轴 4 的产出 |
| 导出可变的内部状态 | F7 | |

## 覆盖范围与穷举扫描的负面结果

负面结果同样是审核产物——它们界定了"已经找过、确实没有"的范围。以下都是**全包
穷举**，不是抽样：

- **空的 error 分支**（`if err != nil {}` / `_ = err`）：0 处。
- **钉死在常量上的指标**：F4 修复后 0 处。
- **未 defer 的清理**：1 处，是我自己 F9 修复的第一版（已改）。
- **锁纪律**（持锁调用外部代码、锁序倒置）：干净。
- **`==` 比较哨兵错误**：仅剩一处 `io.EOF`（惯例安全）。
- **map 迭代顺序影响输出**：干净。
- **导出可变内部状态**：1 处，即 F7。

已确认无问题、避免重复排查的具体结论：

- **投影分段与 ACK 水位**：`planProjectionSegments` 只切连续片段，**保序**；逐单元 ACK 安全。
- **`wal.Ack` 持久性**：确为 fsync（`storeCheckpoint`）。
- **outbox 租约 CAS**：`Claim` 的 `lease_token` CAS 正确；`_id == effect_id`，`Ack` 的过滤器没错。
- **`worker.Pool.Dispatch`**：拒绝时确实调 `OnRelease`，注释与实现一致，回调不会丢。
- **其余 5 处可重试回调**：saga ×3、`remoteentity/mongo_committer`、`nestwal/effect_inbox`
  都正确重置或整体覆盖。
- **`digestRecord` 确定性**：`json.Marshal` 对 map 键排序、`[]byte` 走 base64，
  跨重放稳定；两条投影路径共用同一函数。

## 发布前的测试质量普查

16 条发现之后又做了一次面向发布的测试普查：不是抽查，而是对 core + kit 全部
**997 个** Test 函数（core 613 / kit 384）做机器扫描，找两种"看起来有保护、
实际没有"的形态。两轮都做到清零，且每条重写的断言都用**变异测试**验证过——
临时破坏实现，确认对应断言精确打红。

### 形态一：零断言测试（9 → 0）

函数体内没有任何 `t.Error`/`t.Fatal`。其中 3 条是合理的（委托给带断言的
`exerciseMap` helper），6 条是真问题：

| 测试 | 问题 | 重写后断言的真实设计属性 | 变异验证 |
| --- | --- | --- | --- |
| `container.TestObjectPool` | 有一个**空的 `if` 体**和一句"我不确定语义"的注释 | freelist 取出的对象**原样返回**、`resetFunc` 只在 sync.Pool 路径生效；`Put` 在两个列表间**移动**而非复制；`Clear` 丢弃而 `Release` 归还；nil 构造函数 panic | freelist 也 reset → 红；Put 不摘除 → 红；Clear 改为归还 → 红 |
| `nest.TestTickerStopIdempotentAndCallbackPanicSafe` | 名字承诺 panic 安全，唯一"断言"是进程没崩 | `doTick` 对**每个**回调单独套 `SafeFunc`，所以 panic 既不停 ticker 也不阻断同一 tick 里后续回调；被 Stop 过的 ticker 不能再 Start | SafeFunc 提到循环外 → 红；Start 不看 stopped → 红 |
| `lock.TestReentrantMutex_Basic` | 只证明"Lock 两次不死锁"，换成 no-op 也能过 | 内层 `Unlock` **不释放**锁（递归必须归零）；`TryLock` 只对 owner 可重入；非 owner / 已释放后 `Unlock` panic | Unlock 总是释放 → 红；TryLock 不认 owner → 红；非 owner 静默返回 → 红 |
| `kit/spatial.TestGridTerrainConcurrentAccess` | 丢弃全部返回值，只靠 race 检测器；写全被拒也能过 | 并发写后每格保留最后一次写入（期望值**由测试自己的写入计划推导**，不硬编码）；`SetBlocked` 的 error 被检查 | `SetBlocked` 变 no-op → 红 |
| `kit/ai.TestBlackboardConcurrentAccess` | 同上，丢弃全部返回值 | 每个 worker 的最后一次写入都可读回；`Snapshot()` 返回**拷贝**而非活状态别名 | Snapshot 返回内部 map → 红 |
| `bus.TestBusStopBeforeStartIsSafe` | 调两次 `Stop()`，不断言 | 未启动的 bus 上 `Stop` 是**有界**的成功 no-op，且第二次也及时返回 | — |

`ObjectPool` 还顺带补上一条未声明的约束：它内嵌 `sync.Pool`（并发安全）但
`workList`/`freeList` 是裸 slice，跨 goroutine 共享会 race。名字和内嵌的
`sync.Pool` 都在诱导相反的假设，因此把这条写进了包注释。

### 形态二：以 `go test` 超时当失败信号（21 → 0）

测试主体顶层的裸通道接收：属性一旦被破坏，测试**挂住**，10 分钟后得到一份
堆栈而不是一句话。这正是 F9 和 F11 认定的坏信号形态，之前只在个别位置修过。

第一版探针把 goroutine 内的"起跑栅栏"也算成了命中（那是安全的，栅栏不阻塞测试
本身）；收紧到只看顶层接收后是 21 处。修法是每个包一份泛型 helper（Go 的测试
helper 不能跨包），把等待变成有上界、且**说明在等什么**：

```go
func awaitChan[T any](t *testing.T, ch <-chan T, what string) T {
	select {
	case value := <-ch:
		return value
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		...
	}
}
```

kit `etcd/local_mirror_test.go` 一个文件里 11 处是同一形态，一个
`awaitWatchStarted` helper 一次替换完。

验证 helper 有效时又踩了一次自己的坑：第一次探针去禁用 fake 内部的
`close(transport.started)`，结果破坏的是 fake 的不变量而非被测属性，触发了**另一处**
阻塞，测试照样挂了 10 分钟——这不能证明 helper 无效，也不能证明有效。改用隔离的
最小探针（一个永不写入的通道）后确认：5 秒失败，输出
`timed out waiting for a signal that never arrives`。**探针本身必须被隔离**，
否则它验证的不是你以为的那个东西。

### 覆盖

两轮扫描现在都是 0 命中。全量验证：core / kit / codegen / skill 四仓 `gofmt`、
`go vet`（kit 含 `-tags integration`）、`go test` 全绿；core 与 kit 的
`go test -race ./...` **全包**全绿。业务仓 `cube` 零改动。

## 方法论：缺陷密度与停止判据

审核不是无限进行的。实际的产出曲线是：前 10 轮 15 条，第 11 轮 1 条（且那一条是
**我自己修复代码里的**未 defer 清理，说明自审有效），之后约 50 个检查点 0 条。
决定停止的依据是这个曲线加上"穷举扫描已覆盖全部已知高产类别"，不是"看不出问题了"。

一条方法论教训：**每条修复都必须临时回退一次，确认对应测试真的变红**。16 条
全部这样验证过。没有这一步，修复本身就可能是空断言——F4 的第一版实现正是这样
被抓出来的。

## Go 测试组织的硬约束

审核期间重构测试文件时确认过的规则，记录以免重复摸索：

- 测试文件**必须**和被测包在同一目录，不能集中到一个聚合包里。
- 两种形态：`package foo`（内部测试，可访问未导出标识符）与 `package foo_test`
  （外部测试，只能用导出 API）。同一目录可以并存。
- 共享测试辅助代码必须是一个**普通包**（惯例放 `internal/xxx`），
  这就是 `internal/mongofake` 的位置来源。
- `testdata/` 目录被 go 工具链忽略。
- 带 `//go:build integration` 的文件被 `vet`/`build`/`test ./...` 排除（见 F8）。
