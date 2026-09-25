# Sync 优化：结构审查、缺陷修复与可读性重构

- 日期：2026-09-23；审查基线：`967bc69`，M-15～M-18 已提交，开始时工作树干净。
- 状态：三个确定性缺陷已在本次工作树修复；用户授权后，R1～R3 纯重构批次已实施并通过回归。
- 工具链：模块声明与本机均为 Go 1.27.0。

## 1. 结论与证据范围

同步块已经完成 [ARCH-12](../bugfix/ARCH-12-sync-package-layout.md) 的目录收拢。当前保留八个 Go 包更合适：其中 frame 是无传输依赖的协议格式，nettransport 承接具体协议，driver 隔离 NATS/JetStream 依赖；它们不是仅为单个接口制造的层次。优先改善同包中的代码组织与状态含义，不再搬一轮顶层目录。

本轮完成八包目录/导入结构核对、现有测试基线，深入检查 entitysync 的捕获→编码→准入→结算与退订链路，抽查 policy、AsyncTransport、lockstep、syncbus/mirror 的职责与实现入口。不是八包逐函数穷尽审计，也未证明所有并发交错正确。

初次审查时，图谱项目 `Users-whb-roost` 的代际为 `2026-09-22T07:43:00Z`；搜索新路径无结果，coverage 对 `sync/` 证据文件返回 `not_tracked`，对 `entity/subject_sync.go` 返回 `metadata_changed`。因此初次审查按 Verify 要求回退当前源码、文件清单和 `go list`；未将旧图谱连边当作当前调用事实。源码基线与图谱覆盖不能混为一谈。

## 2. 当前与目标包结构

当前与目标目录树相同，八包均保留，不新增 manager/service/helper 子包：

```text
sync/
├── entitysync/          内容复制机制、会话与订阅
│   └── policy/          Interest/AOI、Group、Direct
├── frame/               帧编解码、结构与限制
├── nettransport/        UDP/KCP/QUIC、可靠队列与传输契约
├── lockstep/            输入排序、房间广播与追帧
└── syncbus/             服务间总线契约与 PatchSyncer
    ├── driver/          NATS/JetStream
    └── mirror/          Envelope 副本复制
```

| 现包 → 目标包 | 保留原因与依赖 |
| --- | --- |
| `entitysync` → 原包 | 组织状态到交付状态的机制；依赖 entity、frame、nettransport 和观测原语 |
| `entitysync/policy` → 原包 | 谁观察谁；依赖 entitysync 与空间几何，不直接编码或发送帧 |
| `frame` → 原包 | 标准库实现的线格式，可独立解码，不引入会话和协议连接 |
| `nettransport` → 原包 | 可靠队列与具体网络协议，供 entitysync 与 lockstep 共用 |
| `lockstep` → 原包 | 输入帧的语义与实体状态复制不同，当前通过 nettransport 发送 |
| `syncbus` → 原包 | 服务间契约与 patch 适配，不需引入网络实现 |
| `syncbus/driver` → 原包 | 隔离具体中间件依赖，装配仍由 kit 完成 |
| `syncbus/mirror` → 原包 | 依赖 syncbus，供 cache/remoteentity 使用，不反向依赖消费者 |

`go list -f '{{.ImportPath}}: {{join .Imports " "}}' ./sync/...` 核对了上述直接导入；未用函数同名连边推断依赖。`entity/subject_sync.go` 仍属于内容层，`spatial` 与 `syncstream` 仍是块外基建。

## 3. 本轮确认并修复的缺陷

| 问题 | 触发与变化 | 记录 |
| --- | --- | --- |
| RR-20260923-01，P2 | 换 profile / 撤回退订后再退订或退役，原先漏 remove；现在按已交付引用表决定 | [问题](../bug/RR-20260923-01.md) · [修复](../bugfix/RR-20260923-01.md) |
| RR-20260923-02，P1 | 部分帧已交付后 RetryLater，原先重复旧 delta 或倒退 BaseTick；现在逐帧采纳、受影响订阅重试全量 | [问题](../bug/RR-20260923-02.md) · [修复](../bugfix/RR-20260923-02.md) |
| RR-20260923-03，P2 | 满容量删一增一受 ID 大小影响；现在先释放再分配 | [问题](../bug/RR-20260923-03.md) · [修复](../bugfix/RR-20260923-03.md) |

三份测试均先在未修对应问题的实现上复现，再验证修后结果。补充了核心交付边界的中文注释；policy/lockstep 注释同步当前包归属。

## 4. 可读性重构批次

### R1：在 entitysync 原包内组织 Manager 职责

依据：基线 `manager.go` 952 行，同时包含配置、注册、会话/订阅、tick、生命周期和观测。一次退订的含义还依赖 `subject.go` 的订阅意图与 `session.go` 的已交付引用表，本轮 RR-01 正是两者混用。

目标文件映射：

| 当前内容 | 目标文件 | 迁移方法 |
| --- | --- | --- |
| ManagerConfig/Manager/NewManager，subject 注册、生命周期及观测 | `manager.go` | 保留类型与所有权，避免再抽公共接口 |
| Open/Hold/Ready/CloseSession、Subscribe/Unsubscribe、相关包内查询 | `subscriptions.go` | 从 manager.go 同包移动；保留公开签名与锁范围 |
| settlement、Flush、capturedAbove、abortAll、pending 调度 | `flush.go` | 按捕获、会话准入、失败恢复、内容提交四段阅读；必要的包内辅助函数表达真实步骤 |
| session 引用分配、逐帧编码 | 现有 `session.go` | 保持编码与状态副本在一起 |
| subject 订阅表与 profile 集合 | 现有 `subject.go` | 保持订阅意图归属 |

执行时先仅移动声明，编译和现有行为测试通过后再提取步骤；不把每个分支拆成函数，不改变错误语义。目标是沿一次 Flush 能直接读懂主要步骤，非单纯压缩行数。

验收：现有与新增八个行为场景、`go test -race ./sync/entitysync/... ./entity`；确认 public API、锁获取顺序、通知回调时机无变化。回退以本批文件移动为单位，不混入新协议。

### R2：采用合适的当前 Go 标准库 API

当前源码在 manager/subject、policy/AOI、lockstep 中仍有简单数值 `sort.Slice` 和手写浅拷贝。现有工具链 `go doc slices.SortFunc`、`go doc maps.Clone` 已确认相应 API。

- 数值 ID 排序使用 `slices.Sort`；多字段使用 `slices.SortFunc` + `cmp.Compare`，保持原先字段优先级。
- `RelationSource.Flush`、AOI 事件流的稳定排序需要保留稳定性，使用 `slices.SortStableFunc`，不能机械替换成不稳定排序。
- `session.clone` 的值类型 map 可考虑 `maps.Clone`，slice 可用 `slices.Clone`；核对 nil/空容器与缓冲所有权，禁止把深拷贝误换成浅拷贝。
- 本轮 RR-03 的排序语义修复已使用 `slices.SortFunc`，其余替换留在该批次。

验收保持原事件顺序、引用隔离和编码结果；针对真实语义检查，不为 API 替换新增照抄实现的测试。不升级 go.mod，不以“新 API”声称性能收益。可按包独立回退。

### R3：局部命名和文档对齐

- `policy/group.go` 的接收者仍叫 `r`，注释混用 room；`InterestConfig.AOI` 注释仍以 Spatial 开头。用 Group/Interest/AOI 的当前职责解释代码，不再按已删除的 room/coordinator 理解。
- 核对 `nettransport.channel.go` 中 `AsyncTransport.session` 等旧双 lane 时代的辅助代码：当前文本调用检索未看到该方法的使用，删除前仍需检查同包测试、构建标签和方法值引用，不能据旧图谱宣布全仓死码。
- `wire.go` 的 statesync 旧名、总览文档中旧路径按当前包对齐；历史复现文档保留原始基线，追加去向，不全局替换历史证据。
- 包/核心函数中文注释优先解释准入、版本、锁和失败边界，不翻译每条语句。

验收包括包注释/文档链接核对、相关包测试；若修改生成模板，补生成工程编译。上述三批 import 路径不变，kit/codegen/demo 无需迁移映射。

## 5. 与 ARCH-11 的衔接

[ARCH-11](../bugfix/ARCH-11-view-priority-and-shared-encoding.md) 尚属独立方案：多 profile 所有权、共享编码和多播不能作为本轮纯重构顺手落地。

多 profile 方案中“Subscribe 每次加计数”与当前政策重试/换 band 的使用方式需先统一：怎样区分同一来源重试与新来源持有、如何释放原 profile，必须写清；否则计数会成为另一份不可靠状态。可选帧模式/多播还涉及客户端契约，单独验收。

tick 内组件编码缓存可作为独立性能批次，先做 N 会话 × M subject × P profile 的编码次数、分配与时间基准，再决定实现。保持 BaseVersion、Full/Delta、Namespace 和 payload 不变，不用包大小证明收益。ARCH-11 旧建议中的 M-15/M-16 编号已被后续实施使用，实际执行时分配新编号。

## 6. 后续正确性审查范围

以下为尚未独立复现的审查入口，不作为本次已确认 bug：

- Flush 的 Push 在途时发生 Hold/Close/Open 同 ID，会话副本采纳是否需校验同一生命周期。
- Prepare 与交付之间换 profile/退订，结算是否只作用于本次捕获对应的订阅意图。
- Stop 的 deadline、阻塞传输与再次 Start 的交错。
- Group 添加 subject 的部分失败与跨政策共同持有，结合 ARCH-11 定义其所有权。

这些状态语义应先用确定性屏障测试建立证据，再选择修法；不要在文件拆分时改变。

## 7. 本轮验证与限制

- 基线 `go test ./sync/... -count=1` 通过；首次沙箱运行因禁止 UDP bind 失败，经允许在本机执行后八包全通过。
- 修后 `go test -race ./sync/... ./entity -count=1` 通过。
- `go test . ./robot/... ./codegen/internal/roost -count=1` 通过，覆盖仓库依赖边界、机器人包及项目生成器测试。
- `go vet ./sync/...`、`go build ./...` 通过；`git diff --check` 与新增七份文档的本地链接检查通过。
- 未运行真实 NATS/JetStream 服务集群、真实游戏客户端、多进程故障注入与吞吐基准。现有 driver 单测不等于这些场景已验证。

R1 → R2 → R3 已按以下实施记录完成；ARCH-11 仍独立。本次重构保持包布局、公开 API、存储和线格式不变。

## 8. 重构实施记录（2026-09-23）

用户明确要求先更新 codebase 索引，再实施重构。本次先对原项目 `Users-whb-roost`（根 `/Users/whb/roost`）执行 full 索引，代际更新为 `2026-09-23T09:32:53Z`，状态 ready；本轮 14 个代码证据文件均为 `metadata_match`、无记录缺口，nettransport 范围亦无记录缺口。工作区其他项目和部分模板仍有解析缺口，不据此宣称全仓完整。重构结束后再次完成 full 索引，代际为 `2026-09-23T09:41:46Z`。21 个变更 Go 文件均返回 `metadata_match`、无记录缺口；图谱已将 Flush 定位到 `flush.go`，Subscribe 定位到 `subscriptions.go`，两个提取函数亦可查询。

### R1 已实施：按职责分文件

- `manager.go` 从本轮修复后的 980 行收拢到约 430 行：保留配置、注册、生命周期和观测。
- `subscriptions.go` 放置会话与订阅入口、查询和状态采纳；`flush.go` 放置 pending、Flush 与结算。
- 首批仅移动声明并通过 `go test ./sync/entitysync/... ./entity -count=1` 后，再提取 `requireSnapshotsAfterRetry` 和 `settleSubscriptions`；主流程以中文注释标明捕获、准入、重试和内容提交的边界。
- 源码对照确认原 Manager 文件的函数全部保留；函数正文仅 Flush（提取步骤）与 takePending（排序 API）发生计划内变化。搬移不调整锁范围和顺序、回调时机或错误分支。临时源码对照还确认：展开两个提取函数、还原排序 API 并去除注释与空白后，Flush 语句与重构前完全一致。

### R2 已实施：标准库写法

- entitysync、policy/AOI、lockstep 的数值 ID 排序采用 `slices.Sort`，多字段比较采用 `slices.SortFunc` 与 `cmp.Compare`；字段优先级和整数比较方向保持不变。
- RelationSource、AOI 和 AOICluster 的事件排序采用 `slices.SortStableFunc`，保留同一 `(Observer, Subject)` 的产生顺序。
- session 的两个值类型 map 使用 `maps.Clone`；实际会话均由 `newSession` 初始化非 nil map。`free` 保留 `append([]uint16(nil), ...)`，维持原先空切片归 nil 的细节及独立缓冲所有权，没有机械替换为 `slices.Clone`。
- Go 版本保持 1.27.0，不据本次可读性整理宣称性能提升。

### R3 已实施：命名、旧辅助函数与文档

- Group 接收者改为 `g`，中文注释说明成员与实体的两个集合以及 Manager 的会话所有权；InterestConfig.AOI 与 wire 注释对齐当前包名。
- 新图谱发现 `AsyncTransport.session` 有一个测试调用者；源码核实为两处重复内部断言，并非生产调用。覆盖检查、同包选择器和构建标签检索后，移除该私有方法及这两处断言，保留通过 `SendReliable` 验证 nil、未注册、排空和失败会话的行为测试。更正原方案中“文本调用检索未看到使用”的初步观察。
- 更新 README、INTERNALS、USER_GUIDE、kit 与 sync 总览中的现行路径，修正每 tick 必然只有一帧和旧 ControlPlane/latest-only 描述；历史设计保持原基线，只追加当前去向。

### 重构验收

- `go test ./sync/entitysync/... ./entity -count=1`：移动声明后及步骤/API 整理后均通过。
- `go test -race ./sync/... ./entity -count=1`：八个 Sync 包和 Entity 全通过，包括前一轮三项缺陷回归。
- `go test . ./robot/... ./codegen/internal/roost -count=1`：通过；生成器测试约 42 秒。
- `go vet ./sync/...`、`go build ./...`、`git diff --check` 通过；21 个变更 Go 文件符合 gofmt，35 个新增/变更文档本地链接目标存在。`go list ./sync/...` 确认仍为八个包。
- 真实中间件集群、真实客户端、多进程故障与性能基准仍未运行；第 6 节的并发审查入口仍未作为本次已修问题。

## 9. 收尾去向

2026-09-23 用户继续授权后，第 6 节四项审查入口已完成定向验证：在途交付、停止边界、Group 部分失败和跨政策持有升级为 RR-20260923-04～07 并修复；ARCH-11 的多来源订阅与共享组件编码以 M-19 / M-20 落地。原始审查结论保留在上文，当前状态与验证见[Sync 收尾](SYNC-COMPLETION-2026-09-23.md)，基准见[运行说明](SYNC-BENCHMARKS.md)。网关多播依用户确认单独推进，真实集群与客户端容量证据不以微基准替代。
