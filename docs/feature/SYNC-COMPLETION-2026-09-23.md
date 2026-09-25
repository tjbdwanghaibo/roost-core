# Sync 收尾与性能基准（2026-09-23）

状态：**本轮约定范围已完成，验收通过；尚未发布。** 本记录为本轮交付入口。

**当前结论（2026-09-24）：已授权的 Sync 优化和正式双模式接入可以收尾，转入维护。** 最新截止复核见本文末尾；下文 09-23 的待办与测试数字保留为历史基线，不能当作当前未完成清单。

后续用户授权的 A～E 优化也已完成：[AOI/Flush 减分配、会话按需复制、观测/异步负载与 profile 改进](REFACTOR-2026-09-23-sync-next-steps.md#10-实施与验收记录)。下文保留第一轮收尾基线；字段视图配置和迁移见 [Sync Profiles](SYNC-PROFILES.md)。

基线：`967bc69` + 已完成的 R1～R3 和 RR-20260923-01～03 工作树。用户授权完成现有 Sync，明确同意将共享帧协议、网关多播及客户端配套单独推进。保持现有八个包，不新增抽象包。

## 未完成项与本轮决定

| 项目 | 本轮范围 |
| --- | --- |
| 在途 Push 与 Hold、重开、换 profile、退订/退役 | 用确定性交错测试复现，修复会话状态和订阅意图被旧交付覆盖的问题 |
| Stop/Close deadline 与 Start 交错 | 等待 Flush 可取消；取消周期 Push；退出和最后一次 Flush 完成前拒绝重启 |
| ARCH-11 多来源订阅 | 以来源令牌持有 profile；同来源重复订阅幂等、换 profile 替换；不同来源独立释放，选 LOD、Key、SchemaVersion 升序最优者 |
| Group 部分失败及所有权 | 添加失败回滚该 Group 的订阅和成员记录，可重试；Group RemoveSubject/Close 只释放本组订阅。实体注册归 Manager，由实体生命周期显式 Unregister |
| ARCH-11 tick 级共享组件编码 | 保持线格式和会话时钟；缓存严格限定本次捕获，不跨 tick 复用；基准比较前后开销 |
| 性能基准 | 保留已有 AOI/lockstep 基准，补 EntitySync 主链路与帧编解码基准、可复跑脚本和实测记录 |
| 共享帧/网关多播 | 本轮范围外，仍待网关和客户端单独设计实现 |

## API 与所有权

新增 `Manager.NewSubscriptionSource()`，返回绑定 Manager 的 `SubscriptionSource`，提供同签名的 Subscribe/Unsubscribe。每个 policy 实例拥有一个来源；来源内部重复 Subscribe 替换自己的 profile，不累计不可回收的计数。Manager 原有 Subscribe/Unsubscribe 作为默认来源保留，无法撤掉其他来源。低优先级来源变化不出帧；生效 profile 改变时发全量，最后一个来源离开才 remove。

Group 以前通过 Unregister 退役整个实体，会破坏其他政策仍然持有的订阅。本轮明确将实体生命周期留给 Manager：AddSubject 仍可便利注册，失败时注册保留供重试；移出组和关闭组仅释放本组订阅。调用者需要在实体真正销毁时调用 Manager.Unregister。这是有意的行为调整，必须同步测试和使用文档。

## 交付与并发

会话编码副本只在仍匹配捕获时的会话状态时采纳；旧 Push 失败也不能关闭同 ID 的新会话。订阅捕获记录其指针和意图 revision，后续变化不能被旧结算覆盖。首次 create 在途时保留 leaving 记录，确保已接受的对象最终收到 remove。会话重置后不再发送该 tick 的剩余旧帧。

停止以独立状态记录在途循环和最终 Flush；context 控制等待，不通过后台无界等待锁来伪装 deadline。Transport 必须响应 Push context，且传输侧要将已开始的 Push 固定到原连接；Manager 无法撤回已被外部传输接受的字节。

## 验收记录

- R1～R3 已在上一轮完成；本轮将四项具体正确性边界落为 RR-20260923-04～07，各有独立问题/修复记录和修前失败证据。
- **M-19 已实施**：来源令牌替代隐式引用计数，三个 policy 全部接入；覆盖重复订阅、默认来源隔离、最优视图、回落全量、最终 remove、同 LOD 的 Key/SchemaVersion 平局、退役不可复活及政策相互释放。
- **M-20 已实施**：直接共享每次捕获的组件编码，保持外层会话帧独立；覆盖同 profile 字节复用、full/delta/不同 profile 隔离、跨 tick 更新和传输改写已交付帧缓冲的隔离。
- `go test -race ./sync/... ./entity -count=1`：全部通过。
- `go test . ./robot/... ./codegen/internal/roost -count=1`：通过，生成器约 45 秒。
- `go vet ./sync/...`、`go build ./...`：通过。
- 最终捕获缓冲集中分配后，`go test -race ./sync/entitysync/... -count=1` 通过；在途交付、停止/关闭、来源优先级和退役场景再连续运行 20 次通过，`go vet ./sync/...`、`go build ./...` 再次通过。
- 基准脚本全场景 smoke（`count=1`、`benchtime=200ms`）及目标场景 CPU/堆 pprof 实际跑通；同机五样本对比与取舍见 [Sync 性能基准](SYNC-BENCHMARKS.md)。
- 29 个变更 Go 文件的 gofmt、脚本 `bash -n`、`git diff --check` 通过；变更 Markdown 的 81 个本地链接目标均存在。
- 代码索引首次全量刷新返回 pipeline 错误，按提示以 fast 模式重试成功；最终查询代际为 `2026-09-23T10:23:29Z`，新增 API、编码辅助函数和基准函数可检索。29 个变更 Go 文件均为 `metadata_match`，`roost-core/sync` 范围无记录缺口；此信号不代表源码或图谱完整性证明。

## 本轮完成与后续边界

本轮选定的 Sync 代码收尾已完成；包结构仍是原来的八包。没有以“零 TODO”或局部回归宣称整个 Sync 已穷尽审计。

### 收尾清单

- [x] R1～R3 可读性重构与 RR-20260923-01～07 修复完成，问题和修复文档齐全。
- [x] M-19 多来源订阅、M-20 共享组件编码完成，相关调用方及核心中文注释已同步。
- [x] 回归、race、vet、构建、基准脚本及 pprof 验证完成，结果见上文验收记录。
- [x] Group 生命周期迁移说明已进入使用文档：Group 只释放自身订阅，实体销毁显式调用 Manager.Unregister。
- [x] 代码索引刷新完成；基准原始结果保留在本地 artifacts/perf/sync，生成文件不纳入源码。

### 后续事项

| 事项 | 当前证据与下一步 |
| --- | --- |
| 单会话性能 | 五次短采样耗时约增加 15%；结合实际扇出、dirty 比例和 payload 做长采样，再决定是否需要单会话路径优化 |
| 分配热点 | 本地 pprof 指向外层 frame.Encode、Flush 临时容器和会话 map 复制；后续优化先建立相同工作量的对照 |
| 真实环境验证 | 需要 broker、客户端及部署参数，覆盖慢消费者、重连、故障恢复和容量 |
| 共享帧与网关多播 | 需要网关和客户端配套，按约定单独推进 |

仍独立保留：ARCH-11 的共享帧/网关多播及客户端配套；真实 NATS/JetStream 集群、真实游戏客户端、跨进程故障注入和生产容量测试。这些需要具体部署与工作负载，按用户本轮确认不混入当前实现。历史 CARRYOVER 的 B2 是合仓前后同机性能证据，本轮仅对比共享编码，不据此关闭 B2。

## 相关记录

- [RR-04 在途会话与订阅意图](../bug/RR-20260923-04.md) · [修复](../bugfix/RR-20260923-04.md)
- [RR-05 停止与关闭边界](../bug/RR-20260923-05.md) · [修复](../bugfix/RR-20260923-05.md)
- [RR-06 Group 部分失败](../bug/RR-20260923-06.md) · [修复](../bugfix/RR-20260923-06.md)
- [RR-07 跨政策所有权](../bug/RR-20260923-07.md) · [修复](../bugfix/RR-20260923-07.md)
- [ARCH-11 实施更正](../bugfix/ARCH-11-view-priority-and-shared-encoding.md#4-2026-09-23-实施更正)

## 2026-09-24 最终截止复核

基线：`967bc69` 加当前既有 Sync/Nest 工作树，Go 1.27.0 / darwin arm64。本次复核围绕已授权范围及 Nest 收尾后的正式接入，没有新增运行代码、包或协议，也没有新增确认 bug。代码收尾完成不等于生产环境的严格延迟验收完成；尚未提交、发布。

### 当前完成清单

| 范围 | 截止状态与证据 |
| --- | --- |
| R1～R3、RR-20260923-01～07、多来源订阅与共享编码 | 已完成，问题与修复记录见上文 |
| A～E、Profile 字段选择/优先级/重复打包 | 已完成，见 [A～E 实施结果](REFACTOR-2026-09-23-sync-next-steps.md#10-实施与验收记录)及 [Profile 契约](SYNC-PROFILES.md) |
| 六项后续优化 | 完整 EntitySync 包组帧、Profile 装配校验、全量预算、Flush/编码/AOI 内存复用、队列治理与计数观测均已完成，见[六项记录](REFACTOR-2026-09-24-sync-six-items.md) |
| 正式双模式及 Nest 接入 | periodic 默认；on_change 成功准入时在 Entity 锁内冻结，全部解锁且提交确认后唤醒，默认 50ms 兜底。setter 只标脏，不逐 setter 发送。见[实施记录](IMPLEMENTATION-2026-09-24-sync-modes.md) |
| 业务装配与生命周期 | 正式 Nest option、Kit 配置/启动、生成 DAO 自动变化收集、Interest 提交事实队列及停止生产者后的 Stop/Drain/Close 已接通 |
| Nest 最近修复的影响 | 重跑提交/释放/完成、Sync 双模式及生成工程回归；锁释放与提交确认仍共同控制交付，不在释放锁之前发送网络帧 |

### 本次实际验证

```sh
GOCACHE=/tmp/roost-nest-go-cache GOWORK=off go test -race \
  ./sync/... ./entity ./nest ./nestwal ./dataengine ./dataengine/engine \
  ./spatial ./kit/nest ./codegen/internal/entity ./codegen/internal/dao \
  ./scripts/perf/sync-aoi -count=1 -timeout=120s
GOCACHE=/tmp/roost-nest-go-cache GOWORK=off bash scripts/test-sync-modes-generated.sh -count=1 -timeout=120s
```

18 个包完成 race 回归。首轮中 17 个包通过，nettransport 的 UDP/QUIC/KCP 回环因沙箱禁止本地 UDP bind 失败；随后在允许本地端口的环境独立执行 `go test -race ./sync/nettransport -count=1 -timeout=120s`，通过（1.372s）。保留此环境失败说明，不将首次整条命令写为通过。生成 DAO/Entity 的独立最小工程双模式 race 通过（1.527s）。

同范围 `go vet`、`go build ./...` 通过；本次修改的 Markdown 本地链接与 `git diff --check` 通过。没有重新压测，也没有把历史压测数字写成本轮测量。

### 容量结论与单独保留的工作

既有正式 Nest → Interest/AOI → EntitySync → AsyncTransport → 双进程回环 TCP 测量使用 1000 玩家、10000 实体、约 50 可见/人，每 50ms 输入窗口变化 1%/5%。[双模式原始结果与测量口径](IMPLEMENTATION-2026-09-24-sync-modes.md#业务负载对照)：

- on_change / 1%：两轮修改到客户端 p99 为 2.40～2.95ms，最大 13.95～18.79ms，两轮无超过 50ms 的样本。
- on_change / 5%：两轮 p99 为 5.24～10.40ms，最大 22.71～64.24ms；第二轮 530 / 565579 个样本超过 50ms，因此严格最大延迟目标尚不能关闭。
- periodic 仍是适用于延迟要求较宽松业务的正式模式，默认 20Hz；不会因本次收尾调整默认配置。

真实 WAL、跨机/弱网、真实客户端与长稳运行属于部署验收；共享帧协议/网关多播及客户端配套按既有约定独立推进。并行 Flush、空间分片和每 Profile 独立版本链没有纳入已授权收尾，不作为当前遗留实现。后续仅由明确业务需求、可复现问题或新 profile 证据启动优化，不继续滚动扩大本轮范围。

### 索引与审查边界

采用 codebase-memory Verify：最近项目 `Users-whb-roost-roost-core`，代际 `2026-09-24T06:50:16Z`，ready，17968 nodes / 141504 edges。复核 Admit/Release/Confirm、Nest 执行与 pipelined 准入、Manager 冻结/唤醒、Drain 和 Kit 生命周期；相关符号与双向一跳查询无剩余分页。19 个正式源码/测试候选路径均为 metadata_match/no_recorded_issue，sync scope 无记录缺口。这是 best-effort 信号，不是穷尽审计证明。

CaptureSync 通过接口调用，图谱入边为空不能解释为未使用；已结合 SyncCommitObserver、调用源码和实际集成测试确认。docs/scripts/生成 testdata 按规则排除，已直接阅读文档与夹具并实际生成执行。两处既有 demo 模板 parse_partial 不属于本次链路，未依赖其覆盖作结论。本次只更新文档，无须为排除目录重新建立代码索引。lockstep、syncbus 跑包级回归，未新增逐函数审计或真实 Broker 集群验收。
