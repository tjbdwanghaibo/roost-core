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
