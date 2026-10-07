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
