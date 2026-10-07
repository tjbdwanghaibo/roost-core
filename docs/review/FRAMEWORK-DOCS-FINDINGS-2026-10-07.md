# 框架整体双文档编写中发现的问题（2026-10-07 起）

写 `docs/framework/` 时各分区 agent 报出的“文档与源码不一致”和“源码疑点”。按维护者规则（交给 review 前不留 WANTED），每条都要在本轮闭环：修复（RR 流程）、结构性守卫、或写明证明后关闭。代码改动随下一个补丁版本发布。

| # | 分区 | 类型 | 内容 | 处置 | 状态 |
| --- | --- | --- | --- | --- | --- |
| F01-1 | 01 | 文档 | `docs/INTERNALS.md:18` 说依赖图非法“在初始化前失败”，实际服务专属 Mod 在共享 Mod 启动后才排序校验（`app/app.go:352`） | 改文档或改代码使其名副其实 | 待处理 |
| F01-2 | 01 | 文档 | `docs/INTERNALS.md:20` 说 RuntimeFailure “只记录第一个根因”，实际 `Err()` 合并全部、`Done` 投递首个（`app/runtime_failure.go:40/:51`） | 改文档 | 待处理 |
| F01-3 | 01 | 注释 | `internal/configschema/schema.go:11` 点名不存在的 `TestSharedConfigSchemaStaysALeaf`（实为 `TestSharedConfigRulesStayALeaf`） | 改注释 | 待处理 |
| F01-4 | 01/12 | 生成文档 | `render_deploy.go:1196` 生成的部署说明 `total_timeout` 公式漏单实例锁 +3s（`shutdown_budget.go:147` 已算） | 改生成器文案 + 一致性测试 | 待处理 |
| F01-5 | 01 | 文档 | `docs/USER_GUIDE.md` 顶部变更标题仍写“main，未发版”（:23、:47 等） | 改为对应版本 | 待处理 |
| F01-6 | 01 | 示例 | `app/example_test.go` 的 `ManagerMod` 在 Provide 里启动 manager、Stop 不带 ctx，与 `kit/manager` 语义不一致 | 改示例为真实语义 | 待处理 |
| F01-7 | 01/12 | 文档 | `demo/README.md` 说 codegen “见 go.mod，只有 yaml.v3”，codegen 已无独立 go.mod | 改文档 | 待处理 |
| F01-8 | 01 | 源码疑点 | `--check-config` 默认路径缺失时只 Warn 并按缺省检查、仍打印 `config ok: <默认路径>`（`app/app.go:100/:140`） | 先红后绿：缺失时明确失败或输出不误导 | 待处理 |
| F01-9 | 01 | 源码疑点 | 服务专属 Mod 与共享 Mod 同名时 `sortMods` 不拒绝 | 先红后绿：加守卫 | 待处理 |
| F01-10 | 01 | 源码疑点 | 拿锁后到 `Service.Init` 前不处理 SIGTERM；启动阶段 hook 无期限（单实例锁方案 §3.4 记为现状） | 评估并闭环（修或写明证明） | 待处理 |
| F03-1 | 03 | **缺陷（探针已证实）** | `durability=memory` 的事务带 `rollback=state|undo` 改了持久字段，不进 WAL 也不报错（`nest/rollback.go:586-600` memory 分支直接返回；探针 err=nil、committer 0 次、PrepareMutation 0 次）；`NEST_TRANSACTION_WAL.md` §3 “禁止修改 persistent 字段”无运行期强制；`rollback.go:587` 注释“经 release hook 持久化”过时 | RR 流程先红后绿：memory 事务改持久字段时拒绝（或编译 / 注册期拒绝） | 待处理 |
| F03-2 | 03 | 文档 | `docs/USER_GUIDE.md` §4：async 只等写入不等 fsync（`nestwal/wal.go:337`）；strict 只等 WAL fsync 不等 Mongo 投影 | 改文档 | 待处理 |
| F03-3 | 03 | 文档 | `docs/USER_GUIDE.md` §5 backlog 超限“触发 runtime failure”不准：WAL 磁盘 / 未确认年龄 → 健康失败；未 ack 达上限 → 准入拒绝（缺省不限）；仅 outbox 硬上限 fence | 改文档 | 待处理 |
| F03-4 | 03 | 文档 | `docs/INTERNALS.md` §4 状态机把 Mongo 写画在解锁前，实际投影在解锁后（held） | 改文档 | 待处理 |
| F03-5 | 03 | 文档 | `docs/INTERNALS.md` §5～§6、`NEST_TRANSACTION_WAL.md` §2 仍写 `kit/nestwal`、“kit Projector”；实为根包 `nestwal/`、`dataengine/engine/` | 改文档 | 待处理 |
| F03-6 | 03 | 文档 | `NEST_TRANSACTION_WAL.md` §6 “相同或更高 version 视为成功”；`MongoStore` 只认版本等于 Next 且 `_last_tx` 相同，更高版本 fatal（`mongo_store.go:258-269`） | 改文档（并确认源码语义为准） | 待处理 |
| F03-7 | 03 | 文档 | `NEST_PIPELINED_COMMIT.md` §8 测试路径仍是旧 kit 路径 | 改文档 | 待处理 |
| F03-8 | 03 | 观察 | `nestwal.Committer` 与 `OpenRuntime` 仓内无生产调用方 | 判定：删除或说明保留理由（C8 零调用方 API 维护者定“保持”，需对照） | 待处理 |
| F03-9 | 03 | 观察 | `//roost:nest` 标记不接受 pipelined，只能手工注册 | 补支持或写明为何不支持 | 待处理 |
| F03-10 | 03 | 观察 | async 记录可能在 fsync 前被投影进 Mongo（崩溃后 Mongo 有、WAL 无） | 论证是否违反契约；违反则修 | 待处理 |
| F03-11 | 03 | 观察 | `CommitRecord` digest 为 JSON 序列化，改结构体字段后跨版本重放判身份冲突 | 线上未部署不做兼容；写明升级须排空 WAL，或改 digest 口径 | 待处理 |
| F03-12 | 03 | 观察 | 事务标记 TTL 必须大于 WAL 最长未确认时间，无启动校验 | 加启动校验（先红后绿） | 待处理 |
| F02-1 | 02 | **缺陷（同 F03-1，两分区独立证实）** | 生成器会产出 `rollback=state|undo` + `durability=memory`（`codegen/internal/nest/gen.go:202`）；`durableCommit` 在 memory 且无 effect 时直接返回（`nest/rollback.go:589`），持久字段改动不进任何提交记录、不报错，重载后回到旧值；文档（`NEST_RUNTIME.zh-CN.md:80`、`CODEGEN_REFERENCE.zh-CN.md:234`）说 memory handler 不能改持久字段，源码只在 `rollback=none` 时 panic | 维护者 2026-10-07 “handler按照推荐处理” → 选 A：改实现强制契约，memory 事务改持久字段时整笔失败回滚并点名字段；同时改 F02-6 过时注释 | 待处理（随修复批） |
| F02-2 | 02 | 死配置 / 死代码 | `nest.heartbeat_worker_num` 被读取并传入引擎但 `NewDispatcher` 不用（`kit/nest/nest_mod.go:101`、`nest/dispatcher.go:87`）；`ensureAsyncDispatchAllowed`（`nest/nest.go:699`）无调用方 | 删除死配置键与死代码（A4① schema 同步） | 待处理 |
| F02-3 | 02 | glsvet 盲区 | handler 并发检查只看包级函数（`cmd/glsvet/main.go:389/:550`），codegen 支持的指针方法 handler 里开 goroutine 不报 | 先红后绿：覆盖方法 handler | 待处理 |
| F02-4 | 02 | 文档 / 能力 | 生成器不接受 `durability=pipelined`（`codegen/internal/nest/parse.go:177`），`NEST_RUNTIME.zh-CN.md` 升级步骤第 8 条却要求测 pipelined handler；同文“旧 `//roost:nest` 不属于生产协议”与现行标注同名自相矛盾（同 F03-9） | 补 pipelined 支持或写明不支持并改文档 | 待处理 |
| F02-5 | 02 | 文档 / 注释 | `docs/INTERNALS.md` §3 “worker 哈希串行”过时（现为 ID 依赖链 + 共享 worker），`nest/pipelined_completion.go:284` 注释同；`RUNTIME_EXECUTION_MODEL.md` 仍写独立 roost-codegen 仓与多仓发布顺序 | 改文档与注释 | 待处理 |
| F02-6 | 02 | 注释 | `nest/rollback.go:587` “Memory-only handlers persist through entity release hooks” 过时（同 F03-1 注释项） | 随 F02-1 一并改 | 待处理 |
| F02-7 | 02 | 推断 | `ActionRunner.submit`（`actionflow/action_runner.go:362`）不在 defer 里复位 `executing`（`MissionRunner` 在 defer 里复位）；现所有回调有 recover，路径走不到 | 改为 defer 复位（结构性，消除依赖 recover 的前提）+ 用例 | 待处理 |
| F06-S1 | 06 | **缺陷（探针已证实）** | 重试退避期间送达的成功永久丢失：同一代际尝试 k 已生效、结果在 k 超时后下次派发前送达（记录 `Pending`、`Attempt≥1`），`Complete` 只接收 `Waiting`（`saga/engine.go:443`）→ `ErrNotWaiting` → Term（`nest_completion_consumer.go:150-159`）；之后若截止 / 人工 Compensate / 定义缺失关闭该操作，这一步已生效却不在 `CompletedSteps`、`LateStep=0`、不补偿不告警（探针 `status=failed completed=0 late=0`，扣款落库 1 次） | RR 流程先红后绿 | 待处理 |
| F06-S7 | 06 | **缺陷（探针已证实）** | `EmitStart` 允许 Data 到 4 MiB（`saga/nest.go:36`），`StartSaga` 按 `MaxPayloadBytes`（缺省 64 KiB）返回 `ErrInvalidRecord`（`engine.go:224-226`）；`handleNestStart` 对它和 `ErrIdentityConflict` 不标 Permanent（`nest_start_consumer.go:102-107`）→ nak 退避约 8.7 天才 Term，saga 静默不创建 | RR 流程先红后绿（确定性错误 Term + 告警；EmitStart 侧上限对齐） | 待处理 |
| F06-S2 | 06 | 不一致 | 原生步骤消费者对坏信封 nak 到 MaxDeliver（`command_consumer.go:411-414/:518-530`），另外四个消费者直接 Term；`SAGA.md:24-25` 只与原生一致 | 统一口径 + 用例断言 permanent | 待处理 |
| F06-S3 | 06 | 低 | `ClaimDue` 先领取后校验，一条坏记录导致整批丢弃（`mongo_store.go:226-237`、`engine.go:759-767`），坏记录永不进 ManualRequired；`List` 遇坏记录整次失败（`mongo_store.go:144-151`） | 坏记录隔离（单条进 ManualRequired / 跳过并告警） | 待处理 |
| F06-S4 | 06 | 文档 | demo 注释说原生步骤遇基础设施错误会“立刻重投重试”（`gift_debit.go.tmpl:25-27`、`gift_saga.go.tmpl:76-77`），实际不交还租约、重投为 Duplicate、等到截止 | 改注释（或改行为，需论证） | 待处理 |
| F06-S5 | 06 | **待维护者决定** | `Completed` 的 saga 可被人工 `Compensate` 带回补偿（`engine.go:383-388` 只拒绝 waiting 与无可补偿步骤），不计 `reopened_total`，无文档无用例 | 允许 or 拒绝 | 待决定 |
| F06-S6 | 06 | 文档 | `SAGA.md:71` `sagaKit.ReservationFromContext` 实为 `saga.ReservationFromContext`；`kit/README.md:27/:536/:540` 旧说法（先占位、roost-kit/saga、exactly-once、claim 条件写） | 改文档 | 待处理 |
| F06-C1 | 06 | 配置缺口 | 未校验 `AckWait` 与步骤 `Timeout` 关系（步骤超时大于 AckWait 时处理中被重投），只校验 `LeaseDuration > AckWait` | 加启动校验（A4① 跨键规则） | 待处理 |
