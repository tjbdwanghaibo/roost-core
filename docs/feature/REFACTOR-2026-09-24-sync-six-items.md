# Sync 六项优化实施

状态：六项代码与验证已完成，未提交、未发布。分帧以完整 EntitySync 包为最小单位；目录结构和线协议保持。

## 范围与设计

1. **完整实体包组帧**：保持协议格式；一次传输帧可以包含多个完整实体更新。对象数和字节数共同约束装包，绝不分割 entity payload。Transport 可选声明最大帧大小；AsyncTransport 自动提供上限。单包超过硬上限明确失败。保留逐帧提交、remove-before-create 和失败恢复。
2. **Profile 装配校验**：在现有 entitysync/policy 包提供装配校验，将按实体类型声明的 SyncViewSet 与 Manager 优先级、Interest 来源映射校验关联。静态空间档位和 self 在构造时检查，关系来源的动态 band 在使用前检查。不要求现有自定义 packer 强制迁移。
3. **全量预算**：可选每 Flush 快照对象/字节总预算及每会话对象额度，轮转会话避免饥饿；仅延后完整快照，增量和 remove 保持原交付语义。未发快照保留 kindSnapshot 并重排 pending，不能提前结算其基线。单个大快照允许独占一轮软预算以保证进度，仍受帧硬上限约束。
4. **临时分配**：同包复用 Flush 会话批次、帧对象/组件工作区；限定保留容量并清空指针。最终传输字节保持独立所有权。增加阶段耗时，验证分配改善。
5. **AOI 查询与事件缓冲**：空间索引提供追加到调用方切片的单块查询，避免每块临时去重 map；Interest 内部消费完 AOI 事件后回收有限容量缓冲。公开 Flush 返回的事件所有权不变；不合并中间事件。
6. **队列治理与观测**：增加全局驻留可靠字节上限（含在途）、从准入起算的最大消息年龄；失败结束会话而不跳过可靠增量。提供原因计数和低成本计数快照；详细年龄/订阅扫描另保留诊断入口。

## 结构与兼容

现有包目录保持不变。新职责放入同包文件（profile 校验、快照预算、工作区），不新增包。Entity 不依赖 policy/nettransport；policy 依赖 entitysync，entitysync 通过可选 Transport 能力获知限制。新增预算默认关闭；新装配校验显式启用。字节组帧可能改变帧数，不改变实体内容、版本链或线协议。

## 批次与验收

先完成组帧和 profile 校验，再完成预算与队列治理，最后验证内存复用。覆盖大包边界、部分成功重试、缩小视图、轮转公平性、预算延迟后最新快照、关闭/取消的资源释放。运行 Sync/Entity/Spatial 的 race 和相关模块检查，再使用 1000/10000/50、20Hz 低变化率与集中进入/重连/慢读负载。性能对照不启用 profiler，保留失败结果；不把 50ms 门禁改为通过。

按独立配置与实现批次回退；保留旧公开 API。若发现可复现 bug，另写问题与修复记录。本文件最终补充实际验证结果、收益和限制。

## 已落地的接口与契约

- `entitysync.FrameSizeLimiter`：可选的传输能力，`MaxFrameBytes()` 返回可接收的完整帧大小。Manager 取它与自身 Limits 的较小值；AsyncTransport 自动提供单消息限制，测试包装扣除 24B 信封。单实体包超硬上限以 `frame.ErrFrameTooLarge` 报错，不发送半包。
- `InterestConfig.ViewSets` 与 `entity.MergeSyncViewPriorities`：启动校验有限视图、fallback 和 Manager 排序一致性，订阅前再次校验回调产生的视图。此配置需要业务与 packer 共享真实 ViewSet，不尝试推断任意自定义 packer。
- `ManagerConfig.SnapshotBudget{MaxObjects, MaxBytes, PerSessionObjects}`：默认关闭；会话和同会话实体轮转。只有 `kindSnapshot` 进入预算，正常 delta/full-dirty 与 remove 继续交付。延后数按每轮每个订阅计数，可能重复累计同一个等待请求。字节预算含对象/组件头，不含帧头；单个合法大实体包可独占一轮软额度。
- `workspace.go`：Flush 复用 map 与会话工作区，最多保留 2048 个批次，每个批次 entries/settlements 容量超过 256 时释放；回收清空业务指针。帧编码一次分配连续对象/组件工作区，多帧复用；最终字节始终独立。
- `spatial.AppendBlockIDs`：只对追加段排序；单块不再创建去重 map。AOI 查询缓冲超过 4096 丢弃，Interest 内部消费后复用不超过 4096 的事件缓冲；公开 Flush 结果仍归调用方持有，不改变事件顺序。
- `AsyncTransportConfig.MaxResidentReliableBytes`：全局排队+在途额度，复制 payload 前以 CAS 预留，成功、失败、取消和 Close 超时后归还。`MaxReliableAge` 从准入起算，与 SendTimeout 取较早 deadline；过期为 `ErrReliableExpired`，终止该会话可靠流。
- `Manager.Counters` 和 `AsyncTransport.Counters`：原子读取累计值；详细数量及最老消息年龄仍在 Stats。捕获/编码/准入时长为累计耗时，捕获阶段包含对象额度预选，编码阶段包含按实际包长调度，准入时长包含同步 Transport.Push 的耗时。

新增文件都位于原包：entitysync 的 `snapshot_budget.go`、`workspace.go`、`counters.go`，policy 的 `profiles.go`，nettransport 的 `counters.go`。没有包迁移，也没有新增游戏业务中的配置服务。

## 参数示例

```go
managerConfig.SnapshotBudget = entitysync.SnapshotBudget{
    MaxObjects: 1000,
    PerSessionObjects: 10,
}
transportConfig.MaxResidentReliableBytes = 128 << 20
transportConfig.MaxReliableAge = 5 * time.Second
```

这些是本机恢复验证参数，不是所有游戏的生产默认值。1000 人各 50 个实体约有 5 万个快照请求，1000 个/轮会主动把初始基线铺开到约 50 轮。预算限制的是快照准入，对象数额度先于捕获筛选，避免为未轮到的实体反复打包；纯字节预算需捕获后才能判断，仍不能把预算说成严格 CPU 时间上限。异步错误需要通过 OnError 接入业务会话关闭/重建；可靠增量不能丢旧保新。

## 同模型对照（已完成）

Go 1.27.0 / darwin arm64，GOMAXPROCS=4；1000 玩家、10000 实体、每人约 50 可见、20Hz、每轮 200 ticks。每种变化率三组前后交替采样，未开启 profiler；正常负载对照关闭新预算。前版保存在 `artifacts/perf/sync/remaining-baseline/sync-aoi`，后版在 `six-items-build/sync-aoi`。

| 变化率 | 修改前 MB/s | 修改后 MB/s | 本机分配下降 | 工作 p95 前/后（ms） |
| --- | --- | --- | --- | --- |
| 1% | 21.43–21.44 | 16.04–16.05 | 约 25% | 12.18–15.79 / 13.50–14.49 |
| 5% | 112.94–113.02 | 85.38–85.79 | 约 24% | 22.76–25.72 / 21.83–29.76 |

全部 12 轮客户端数据验证通过。1% 的每轮输出均为 91625 帧、35454282B；5% 为 199615 帧、177768585B。变化样本数、create/remove/update 与最终可见集在每组前后一致。SessionsLost=0。原始产物 `artifacts/perf/sync/six-{before,after}-{1,5}pct-{1,2,3}/`。

耗时有交叉与离群，尤其 5% 后版第三轮更慢，未删除该样本；不承诺稳定的 CPU/延迟收益。严格最大 50ms 门禁 12 轮仍返回失败，报告保留；本轮主要收益是内存分配和边界能力。

## 恢复负载复跑

```sh
ROOST_PERF_LABEL=six-recovery-example ROOST_PERF_COUNT=1 ROOST_PERF_CPU=4 \
./scripts/perf/sync-aoi.sh \
  -players=1000 -entities=10000 -visible=50 -hz=20 -dirty=1 -ticks=200 \
  -async -snapshot-objects=1000 -snapshot-per-session=10 \
  -reconnect-tick=50 -reconnect-players=1000
```

新增参数 `-snapshot-bytes` 可控制全量包字节软预算。报告新增 startup_ticks/startup_ms/recovery_setup_ms 和 async_counters。恢复通过 HoldSession/ReadySession 重建 epoch，使用既有 TCP 连接，验证协议恢复而非 TCP 重新握手/鉴权；初始建基线单独计时，稳态计时窗口不含建基线与预热。测试错误和严格延迟门禁仍分开保留。

## 集中恢复验证（最终实现）

全部场景保持 1000 玩家、10000 实体、约 50 可见、20Hz/1%，经过生产 AsyncTransport 与独立 TCP 客户端。第 50 tick 分别重置 100/500/1000 个会话；对象预算 1000/轮、每会话 10/轮。另保留千会话恢复时关闭预算的参考组。每个场景为一轮，不能用于宣称统计显著的耗时收益。

| 重置会话数 / 预算 | 初始建基线 | 重置调用耗时 | 工作 p95 / 最大（ms） | 分配 MB/s |
| --- | --- | --- | --- | --- |
| 100 / 开启 | 50 轮，2453.76ms | 59.64ms | 13.75 / 64.24 | 29.21 |
| 500 / 开启 | 50 轮，2452.71ms | 164.98ms | 13.21 / 171.41 | 43.19 |
| 1000 / 开启 | 50 轮，2451.98ms | 323.57ms | 15.58 / 331.78 | 64.90 |
| 1000 / 关闭 | 1 轮，40.21ms | 301.53ms | 14.22 / 329.78 | 48.45 |

四轮均通过数据/版本/最终可见集校验；Pending=0、SessionsLost=0，SendErrors/GlobalBackpressure/ReliableExpired=0，结束时 ResidentReliableBytes=0。产物 `artifacts/perf/sync/six-final-recovery-{100,500,1000}-budget1000/` 和 `six-final-recovery-1000-budget0/`。另有小规模 `six-recovery-smoke` 通过数据及严格 50ms 门禁。

第一版恢复预算在捕获之后筛选，千会话样本发生 727728 次快照捕获；压测发现这项浪费后改成对象额度前置，最终为 84979 次，约减少 88%。第一版恢复数据保留在 `six-recovery-*`，不作为最终结果；关闭预算的稳态 A/B 路径不受这项前置筛选影响。

预算的作用是平滑快照准入，并不能让批量 Hold/Ready 的全实体遍历消失；批量重置的最大耗时仍超过 50ms。初始基线约 2.45s 是预算主动延后的结果。恢复期间分配也高于关闭预算组，因此预算保持显式可选，不替业务强行开启。这四轮的严格 50ms 门禁均失败，未改验收门槛。真实跨机网络、TCP 重新握手/鉴权和长时间 soak 未在本轮覆盖。

## 验证范围

- 新增完整实体包多帧、硬上限、第二帧失败恢复、快照轮转/版本、延后后退订、未获额度不捕获及 owner→near→far 客户端字段替换测试。
- 新增 profile fallback、SchemaVersion、跨类型优先级冲突、配置所有权与动态未知视图拒绝测试。
- 新增跨会话全局驻留额度（包含在途）、100 会话并发准入、排队期间过期、取消及 Close 超时额度归还测试；保留原有真实慢读隔离测试。
- Go race 覆盖 `./sync/... ./entity ./dataengine ./spatial ./scripts/perf/sync-aoi`；相关 vet、`go build ./...`。
- 运行代码没有提高 Go 最低版本，没有新增包或改变线协议。未提交、未发布；无本轮新增已确认存量 bug，能力改进与实施中发现的浪费均记录在本文件。

## 索引与完成状态

最终相关 race、vet 和全模块 build 均通过；227 个本地文档链接及 `git diff --check` 通过。未执行发布或提交。

工作区级 `Users-whb-roost` 刷新返回 pipeline error，旧索引保留。随后成功建立最近的核心模块索引 `Users-whb-roost-roost-core`，根目录 `/Users/whb/roost/roost-core`，代际 `2026-09-24T02:59:00Z`（fast，17820 nodes / 139381 edges）。19 个本轮运行代码与新测试路径均为 metadata_match/no_recorded_issue，新函数检索可用；这仍是 best-effort 覆盖信号，不是完整性保证。fast 模式排除 docs/scripts，文档与性能工具以当前源码、编译和实际负载补证。后续核心模块查询优先使用此较近项目，不沿用工作区级旧代际推断新代码。
