# Sync 后续优化评估（2026-09-24）

截止更新：下文是实施前评估，六项及后来授权的事件触发双模式均已完成，不再作为当前待办。当前状态见[最终截止复核](../feature/SYNC-COMPLETION-2026-09-23.md#2026-09-24-最终截止复核)，双模式契约见[正式接入](../feature/IMPLEMENTATION-2026-09-24-sync-modes.md)。

状态：本次评估完成后，用户授权实施六项（最后一项合并队列治理与观测）。当前实施进展及验证见[六项实施记录](../feature/REFACTOR-2026-09-24-sync-six-items.md)。下文保留评估时的证据和建议；“按字节拆帧”已明确为按完整 EntitySync 包边界组帧，字节数只决定装包数量，不切开实体数据。

基线为当前工作树：`967bc69` 加上 09-23 已完成的正确性、可读性和 A～E 优化。以 [上轮实施记录](../feature/REFACTOR-2026-09-23-sync-next-steps.md) 为起点，已完成的共享组件编码、字段白名单、显式优先级、AOI scratch、引用表按需复制、队列字节限制不再列为待办。

本轮重点是用户的 Interest/AOI 模型：1000 玩家、10000 总实体、每人约 50 可见、20Hz、低变化率。未重新审计 lockstep、syncbus 或全部传输协议。

## 结论与优先级

当前容量已获用户认可。下一步优先完善大包、配置和突发负载的处理，再压缩正常负载的分配；保留现有包结构。

| 顺序 | 方向 | 当前依据 | 建议与验收 |
| --- | --- | --- | --- |
| 1 | 按字节拆帧并对齐传输上限 | `session.encode` 只按 MaxObjects 分块；`frame.Encode` 超过 MaxFrameBytes 返回错误，Flush 的编码失败会关闭该会话。默认帧上限 4MiB，AsyncTransport 默认单消息上限 1MiB，两层需要装配方对齐 | 同时按对象数、帧字节数、传输消息上限分帧。单对象过大单独报错；拆帧保留 remove-before-create、逐帧采纳和部分失败恢复。验证多个合法组件聚合超限、边界字节数及中途 Push 失败。当前小 payload 基准没有覆盖这个范围，暂按能力改进记录 |
| 2 | Profile 配置整体校验 | NewInterest 校验来源名称和非负 band，但不知道实体 packer 支持的视图；NewMaskedSyncPacker 在实际捕获时拒绝未知 profile，Flush 会将失败 subject 重新排队 | 在业务装配处统一定义视图、优先级、来源/band 映射，并检查每种实体的支持集合。缺失映射、SchemaVersion 不符及同一 profile 优先级冲突在启动时失败。保留自定义 packer 接口；动态 Profile 回调仍需运行时诊断。补 owner→near→far 客户端全量替换验证，确认缩小视图后旧字段消失 |
| 3 | 进入/重连的全量突发控制 | Flush 获取本轮全部 pending 后统一捕获和准入；现有容量限制、单帧上限与队列背压不是每 tick 全量预算。基准首次建基线及预热发生在计时窗口之前 | 先测 100/500/1000 玩家集中进入、重连、传送，再决定默认预算。候选为每 tick 全量对象/字节预算与会话轮转；未发送快照保留待处理状态，不通过关闭正常会话实现限流，也不能饿死正常更新。涉及提交和基线语义，应单独设计后实施 |
| 4 | Flush 与编码临时分配 | 新采样中 Flush 自身约占累计分配 15.31%，frame.Encode 13.10%，session.encode 自身 3.54% | 优先减少 entries/settlements、ObjectDelta/ComponentDelta 中间切片的扩容和临时对象；评估同包内有限容量 scratch。帧最终字节需要独立所有权，不应直接池化后提前复用。组件共享编码已经存在，不再重复建设。以相同输入的字节/帧数一致、race、分配下降为验收 |
| 5 | AOI 查询与进出事件分配 | spatial.collect 约占累计分配 5.58%，AOI.emit 5.42%；evictFarther 仍需遍历可见集合，本轮 CPU 累计约 6.50% | 可评估复用空间查询结果及去重容器、事件缓冲区；设保留容量上限。只在明确“同 tick 只关心最终可见集合”后考虑合并进出事件，避免改变订阅语义。约 50 可见时不急于引入堆、树或新分片架构 |
| 6 | 多慢客户端下的队列治理 | 已有每会话消息数/字节上限、SendTimeout 和 OldestReliableAge；SendTimeout 从实际发送时开始，队列等待年龄目前用于观测 | 如实际网络测量显示需要，增加全局待发字节预算和最大排队年龄策略，记录关闭原因。过期时结束并重建该会话基线，不能随意丢 reliable 增量。先覆盖部分慢读、带宽受限、断开重连及持续运行后的内存回落 |
| 7 | 低成本运行观测 | Manager.Stats 复制全部 subject 指针并逐个加锁统计订阅；AsyncTransport.Stats 扫描全部会话队列 | 当前秒级采样可继续使用。若观测开销明显，再将频繁计数改为生命周期维护，详细遍历作为诊断接口；增加捕获/编码/准入耗时和固定原因计数。维护计数必须覆盖回滚、部分成功、关闭与重试 |

源码入口：[会话编码](../../sync/entitysync/session.go)、[Flush](../../sync/entitysync/flush.go)、[Manager 配置与 Stats](../../sync/entitysync/manager.go)、[Interest](../../sync/entitysync/policy/interest.go)、[视图 packer](../../entity/sync_view.go)、[帧编码](../../sync/frame/codec.go)、[默认帧限制](../../sync/frame/frame_limits.go)、[异步传输](../../sync/nettransport/channel.go)、[AOI](../../sync/entitysync/policy/aoi.go)、[空间查询](../../spatial/block_index.go)。

## 当前代码的新采样

命令：

```sh
ROOST_PERF_LABEL=remaining-review-20260924-1pct \
ROOST_PERF_COUNT=1 ROOST_PERF_CPU=4 \
./scripts/perf/sync-aoi.sh \
  -players=1000 -entities=10000 -visible=50 -hz=20 -dirty=1 -ticks=200 -profile
```

产物在 `artifacts/perf/sync/remaining-review-20260924-1pct/`。Go 1.27.0 / darwin arm64，每进程 GOMAXPROCS=4；使用同步回环 TCP 路径，未启用 `-async`。1% 是本次假设，不代表已采集的生产变化率；实体更新同时包含移动，payload 样本 181B。

- 数据校验完成：91625 帧、35454282 字节、108139 个变更延迟样本，最终平均可见 49.586；SessionsLost=0、FlushFailures=0。
- 计时窗口分配约 215.80MB / 10.01s，即 21.56MB/s。工作耗时 p95 14.31ms，Flush p95 8.62ms；这是开启 profiler 的单轮数据，仅作定位，不作新的延迟验收或 A/B 收益结论。
- 变更到客户端 p99 50.77ms，最大 54.33ms；严格 50ms 门禁仍失败，脚本返回 1 并保留报告。未将其改为通过。
- CPU 样本合计 2.46s。Flush 累计 43.50% 中包含同步 TCP Push 的 33.33%，不能把整个 Flush 占比当作可通过内部算法消除的开销；网络系统调用和计时器相关开销也不等价于业务 CPU 饱和。
- `allocs.out` 为累计分配采样，包含启动和预热，总采样约 268.71MiB。表中百分比来自此口径，特别是 subscribe/applyEvent 不能直接外推为稳态热点。下一次优化实验应补起止分配快照差值，并使用实际 AsyncTransport 路径。
- 累计分配前列还包括 subscribe 10.79%、Interest.applyEvent 自身 8.21%、maps.clone 5.77%。引用表克隆只在成员变化时发生的优化已经完成；这些数字不能解释成它仍在每次纯 update 时复制。

## 实施边界与后续验证

1. 先做字节拆帧和 profile 装配校验的具体设计，沿用当前包和协议；拆帧会改变帧数量，属于行为增强。明确 Transport 可用消息大小如何传递，不依赖对某个具体传输实现的类型断言。
2. 补多 profile、不同字段大小、进入/重连突发与慢客户端的负载场景；结合数据决定全量预算和队列治理。已有两客户端慢读隔离测试及千客户端正常异步测试保留，不把这些已完成验证重复列为缺失。
3. 最后对临时内存逐项 A/B。限制 scratch 容量，先做简单切片复用；若实际收益很小，保留较易读的实现。各批单独回退，不改数据版本链来换取分配下降。

仍暂缓：并行 Flush、网关多播、新增包层次、每 profile 独立版本链。20Hz 空 tick 不发帧已有实现；事件触发发送和 LOD 降频是额外调度策略，只有实际延迟或带宽需求出现时再推进。

证据方式：codebase-memory Verify，项目 `Users-whb-roost`，代际 `2026-09-23T13:12:02Z`；候选符号检索、encode 双向一跳及精确源码核对，相关查询无未处理分页。涉及路径均为 metadata_match/no_recorded_issue，entitysync/nettransport scope 无记录缺口。图谱为 best-effort；同名函数的低置信度跨仓边未作为证据。未声称完整审计全部 Sync，也未执行新一轮完整测试套件。
