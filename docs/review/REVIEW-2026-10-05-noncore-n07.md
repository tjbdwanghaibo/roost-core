# N07 第一批：configdata / attribute / event / errcode 有界场景矩阵

2026-10-05，分支 `revn07`，基线 `50e9a4e853641f348c879b85c282af9c5813f656`（origin/main）。本机 macOS / Go 1.27.0，`GOWORK=off`。接力清单 [N07 行](REMAINING-REVIEW-HANDOFF-2026-10-05.md) · [跨轮进度](PROGRESS.md) · [证据](../bugfix/evidence/noncore-bugfix-20261005-n07/README.md)。

结论：登记并修复 4 条（NC-60 P2、NC-61 P2、NC-62 P3、NC-63 P2），均先红后绿，未发版。event 与 configdata 本批没有确认缺陷，记为控制与观察。N07 **场景部分完成**，不计 completed/15；停点见末节。

## 读代码的限制

图谱项目 `Users-whb-roost-roost-core`，generation `2026-09-30T11:28:30Z`，根路径是主检出，不是本 worktree。`check_index_coverage` 对 `event`、`attribute`、`errcode`、`configdata`、`kit/configdata`、`codegen/internal/errcode` 无记录缺口；`codegen/internal/{eventgen,attribute}/testdata` 按设计不索引。该 generation 之后本范围只有 `153cac3d`（codegen 退役陈旧 attribute/event 输出）改过文件；结论全部以基线源码直接阅读补证，行号指基线。

## 范围（分母）

本单元的生产源码：`configdata/{configdata,auto,external}.go`、`kit/configdata/configdata.go`、`attribute/{attribute,container}.go`、`event/*.go`、`errcode/errcode.go`；跨包消费者 `servicerpc/status.go`、`bus/rpc_error.go`、`entity/entity_base.go`（事件总线挂点）、`fctx/context.go`（快照绑定）；Kit / codegen 同行：`codegen/internal/{attribute,eventgen,errcode}`、`codegen/internal/roost/attribute_runtime.go`、`codegen/internal/tablegen` 的生成 getter；正式模板消费者 `demo/game/entities/player/{attribute,profile}_component.go.tmpl`、`demo/game/handler/{add_exp,claim_dungeon,equip_item}.go.tmpl`、`demo/internal/service/game/{flags,gm}.go.tmpl`、`demo/internal/errors/*.go.tmpl`。cfggen 的 required/ref/skipempty 与 JSON 往返归 N08，本批只读 configdata 运行时一侧。

## 场景矩阵

“控制”= 有既有或本批具名用例 / 源码判定且结果符合承诺；“观察”= 行为已确认但没有可违反的写明契约或没有触发路径，不改行为。

### configdata（热更新、失败校验、读取可见性）

| # | 情形 | 场景 | 依据 | 结果 |
| --- | --- | --- | --- | --- |
| C1 | 正常 | Load → Reload 发布新快照，进行中请求经 `ActiveSnapshot` 读旧快照 | `TestStoreLoadReloadAndActiveSnapshot`；`fctx/context.go:131` NewContext 绑定、`configdata.go:1242` | 控制 |
| C2 | 正常 | Rollback 回到上一次成功发布；连续两次 Rollback 第二次拒绝 | `TestRollbackRestoresPreviousPublishedSnapshot`、`TestRollbackTwiceIsRejected` | 控制 |
| C3 | 正常 | DryRun 构建 + 校验不发布、不占 s.mu | `TestDryRunBuildsSnapshotWithoutPublishing`、`configdata.go:905` | 控制 |
| C4 | 故障 | 构建 / JSON / 校验失败保留旧快照；null / 空 / 多 wrapper 文档拒绝；strict 未知字段 | `TestReloadFailureKeepsOldSnapshot`、`TestReadJSONRejectsNullAndAmbiguousWrappers`、`TestStrictJSONRejectsUnknownFields` | 控制 |
| C5 | 故障 | BeforeApply / AfterApply / lifecycle emit 失败或 panic：只回滚已准备的监听者、倒序、首载跳过回滚 | `reload_commit_test.go` 6 用例、`TestBeforeApplyPanicRollsBackOnlyPrepared` | 控制 |
| C6 | 故障 | 失败提交不回滚另一个 Store 的全局发布 | `TestFailedCommitDoesNotRollBackAnotherStorePublication`、`TestRevertRestoresAllGlobalSlots` | 控制 |
| C7 | 并发 | Reload / DryRun / SetDir 并发；validate 单线程 | `TestConcurrentReloadDryRunAndSetDir`（本批 `-race -count=3`） | 控制 |
| C8 | 正常 | Kit Mod：版本 gauge 在回滚时退回 Old.Version；Stop 撤销监听 | `kit/configdata` 3 用例 | 控制 |
| C9 | 可见性 | 新快照在 AfterApply 前已发布（`configdata.go:838` commit），AfterApply 失败回退前绑定上下文的请求会读到被撤回的一代 | 源码判定 | 观察 C-O1 |
| C10 | 故障 | demo 开关钩子注释“A rollback needs no handler”（`flags.go.tmpl:50`）：显式 Rollback 成立；若排在它后面的监听者 AfterApply 失败，开关已按新表发布而 Store 回退 | 源码判定；模板只有 metrics（在前）与 flags 两个监听者，无触发路径 | 观察 C-O2 |
| C11 | 可见性 | Table.Get / Rows 返回值的浅拷贝：行内切片 / map 被读者修改会改到快照 | 源码判定 | 观察 C-O3 |

### attribute（变更、传播、回滚）

| # | 情形 | 场景 | 依据 | 结果 |
| --- | --- | --- | --- | --- |
| A1 | 正常 | 快照是副本、空层回答、零 Selector=Base、层隔离 | `container_promises_test.go` 4 用例 | 控制 |
| A2 | 并发 | Snapshot 复制期间 Apply / ClearDirty 写同一 profile | 新增 `TestSnapshotCopiesUnderTheContainerLock` | **NC-60** 红→绿 |
| A3 | 正常 | 生成 profile：派生公式拓扑序、环检测、SetDirectAttr 拒派生、ExportValues 稀疏 | `codegen/internal/attribute` 既有用例、`attribute-runtime.sh` 往返 | 控制 |
| A4 | 故障 | float 字段静默截断；max>64、AttrID 越界生成后编译失败 | 新增 `TestParseDirRejectsAttributesTheWireCannotCarry` | **NC-62** 红→绿 |
| A5 | 回滚 | game-demo 升级 / 换装后 handler 失败或提交被拒：DAO 回滚、容器层未回滚，下次成功提交持久化虚高 Base | 新增 `TestAttributeLayersRollBackWithTheTransaction`（生成工程） | **NC-61** 红→绿 |
| A6 | 传播 | Base / Final 写回 DAO 后由 Sync 发布；Final nopersist 只复制 | `TestSyncProfilesFilterGeneratedDAOPayload`（生成工程） | 控制 |
| A7 | 故障 | int32 等窄字段 `SetAttr` 超范围按 Go 转换回绕 | 探针（证据 README） | 观察 A-O1（文档写明“setter 转换”） |

### event（订阅、退订、重入、取消、关闭）

| # | 情形 | 场景 | 依据 | 结果 |
| --- | --- | --- | --- | --- |
| E1 | 正常 | 自身同步、他人 async / 无 async 时同步；多组去重；Destroy 全部退订 | `event_test.go` 6 用例 | 控制 |
| E2 | 重入 | 处理中订阅新 handler：本次发布不投递 | 探针 | 控制（快照语义） |
| E3 | 重入 | 处理中销毁后面的订阅者：本次发布仍投递一次，之后不再 | 探针 | 观察 E-O1 |
| E4 | 故障 | handler panic：后续订阅者不投递，panic 传给发布者 | 探针 | 观察 E-O1 |
| E5 | 并发 | 无 AsyncDispatcher 时，发布者 goroutine 同步执行其他实体的 handler | 探针 `-race` 报 DATA RACE（探针计数器） | 观察 E-O2 |
| E6 | 关闭 | DelSub 原地过滤，切片尾部仍引用已退订 handler（探针：1 个旧槽） | 探针 | 观察 E-O3 |
| E7 | 生成 | eventgen 生成 InitSub / SyncHandleEvent，方法缺失生成期失败 | `codegen/internal/eventgen` 既有用例 | 控制 |
| E8 | 接线 | 框架与 game-demo 都不调用 `SetEventBus` / `NewEventBus`，`EntityBase.SubEvent/PubEvent` 在没有业务接线时是空操作 | `rg` 全仓（排除 docs） | 观察 E-O4 |

### errcode（错误映射、包装、正式 RPC 输出）

| # | 情形 | 场景 | 依据 | 结果 |
| --- | --- | --- | --- | --- |
| R1 | 正常 | Wrap 保留 cause、ClientError 取最外层定义、字段只进 Error() 不进客户端 | `errcode_test.go` 3 用例；`errcode.go:47,99` | 控制 |
| R2 | 跨包 | bus RPC 失败信封：服务端 ClientError → 客户端 `Remote(code, reason)`，`IntError.Is` 按 code 比较使本地哨兵仍可 `errors.Is` | `bus/rpc_error.go:26-56`、`errcode.go:190` | 控制 |
| R3 | 跨包 | 普通错误（ctx 取消、框架暂时性错误）经 RPC 变成 (1, "server error")，调用方失去 `errors.Is` 与可重试分类 | 源码判定；`servicerpc/status.go` 文档写明的约定 | 观察 R-O1 |
| R4 | 故障 | `Define` 静默覆盖同号注册、不拒绝 0/1 号 | 源码判定；框架 55～62 万段与业务 10 万段不重叠 | 观察 R-O2 |
| R5 | 生成 | errcode 扫描：非字面量 / 别名导入的 Define 被跳过，不查重名，与使用说明不符 | 新增 `TestExtractDefinitionsRefusesWhatItCannotRead` | **NC-63** 红→绿 |

## 观察与设计建议（未改行为）

- **C-O1**：AfterApply 是“发布后”回调，失败回退前开始的请求整个生命周期读被撤回的一代。若要“未提交的一代不可见”，需要把 AfterApply 改成发布前的第二个准备阶段，并改 flags 钩子读 `event.New` 而不是 `store.Current()`；属于契约选择，需维护者定。
- **C-O2**：flags 钩子缺 Rollback 分支，在模板当前两个监听者的顺序下无触发路径；若业务在其后新增会失败的 AfterApply，开关与配置会分叉。建议补 Rollback 用 `event.Old` 重发或改注释，等有触发路径或维护者要求时再改。
- **C-O3**：快照“不可变”靠约定；行类型含切片 / map 时读者可改到共享快照。建议在生成 getter 文档里写明，或对含引用字段的行返回深拷贝（有分配代价）。
- **A-O1**：窄类型回绕保持文档行为；若要拒绝，需在生成 setter 里加范围检查并改返回语义。
- **E-O1 / E-O2 / E-O3 / E-O4**：event 包没有任何框架或模板接线（E8），四条都没有正式触发路径，所以不登记 RR。真要用于 Entity 时，E-O2 会让发布者在自己的锁下执行别的实体的业务（违反 Nest 单实体执行权），E-O1 会把事件投给同一轮里已销毁的实体。建议二选一：接入 Nest（AsyncDispatcher 必填、走 Cast，销毁后的投递在接收侧丢弃），或标注为实验 / 移除 `EntityBase` 的空挂点；E-O3 可顺手 `clear` 尾部。
- **R-O1**：这是 servicerpc 写明的边界（“peer refused” vs “call did not happen”），但服务端内部错误的“结果未知”与“明确失败”在线上同为 code 1；需要重试语义的调用方应自己定义可重试的业务码。
- **R-O2**：可在 `Define` 里对同号不同名 panic 或记录告警；跨模块同号目前只有生成器在业务工程内检查。

## 验证

修复合并后的本地矩阵（[validate-1](../bugfix/evidence/noncore-bugfix-20261005-n07/validate-1.txt)）：gofmt 空；改动包 vet；8 个相关包 `-race -count=3`；根包 `go test -count=1 .`；`go build ./... && go vet ./...`；`go test -count=1 ./codegen/...` 无失败；`attribute-runtime.sh`；全新生成 game-demo（replace 到本分支）build / vet 与 player / attribute / handler 包 `-race`，并对其跑 errcode 生成器导出 21 条。未跑：glsvet（本批没改三大模块）、真实 Mongo / Redis / NATS 链路（本批改动不涉及存储或传输）、Linux / Windows。

## 方向判断

attribute 是同一模块在近期第二轮出问题：RR-20260917-06（框架半缺失，生成物编译不过，U-0230 补上 `attribute` 包）之后，本批又出 NC-60（新包的锁承诺没兑现）、NC-61（模板层不随事务回滚）、NC-62（生成器不检查可表示性）。三条都是“框架半补齐时只验证了编译和单线程往返”，不是同一不变量反复被打破；修复都在减少分支（锁覆盖、一次性登记逆操作、生成期拒绝），没有增加状态或重试。判断为实现细节缺口，方向本身成立。但 NC-61 暴露的前提值得维护者确认：**属性容器是组件内存，Nest 回滚只认识 DAO**。现在每个改容器的业务组件都得记得调 `captureRollback`，换一个游戏就会再犯一次。候选：(a) 维持现状，在使用说明写明“改容器前登记逆操作”，代价是靠人记；(b) 让 `attribute.Container` 提供与事务无关的 `Checkpoint()/Restore()`，由模板 / 生成组件在一处接 nest，代价是一个小 API；(c) 把 Base 等可持久层直接放进 DAO、容器只缓存派生 Final，代价是改模板分工和存量数据形状。建议先 (a)+(b)，不做 (c)。

event 不属于“反复出问题”，但它是零接线的库（E8）：继续审它的并发细节收益很低，建议维护者决定接入还是移除，再定审查深度。

## 停点与下一步

- **本批完成**：上面矩阵 C1～C11、A1～A7、E1～E8、R1～R5；NC-60～63 已修复、声明场景验证。
- **未做**：cfggen 生成的 ref/required/skipempty 与真实 JSON / 索引往返（归 N08）；`roost id` 扫描改 AST（NC-63 残余，N08）；configdata 在正式 Kit 进程里经 GM `gm.config.reload` 的端到端热更新（需起生成工程服务，本批只到包级与源码）；event 接入方向待维护者决定；Luban 外部表（`examples/lubanreal`）的真实工具链。
- **下一入口**：N07 第二批——configdata 的 GM 热更新端到端（生成工程起服务，改 `configs/data` 后 reload / 失败 / rollback，检查 flags 与 scene refresh 的读取），以及 tablegen 生成 getter 在 Nest handler 内的快照一致性；然后转 N08。
