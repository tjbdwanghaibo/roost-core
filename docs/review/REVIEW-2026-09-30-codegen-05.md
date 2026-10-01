# 2026-09-30 Codegen 第五轮：schema 消费者与合仓依赖回写

本轮确认[三个新 P2](../bug/REVIEW-2026-09-30-codegen-05.md)：RR-20260930-CG-12 false 索引无用导入、RR-CG-13 bean 生成命名空间遗漏、RR-CG-14 依赖事务丢弃自动迁移的 Go/manifest 改动。没有修源码；[可复跑证据](evidence/codegen-review-20260930-05/README.md)、[机制学习](IMPLEMENTATION-CFGGEN-NAMESPACE-AND-DEPENDENCY-MIGRATION.md)、[跨轮进度](PROGRESS.md)一起归档。

## 同步与上轮状态

2026-09-30 Core 干净 main 从 `153cac3dbb94baaaf8960032812079448c46b265` fetch 并快进到 `origin/main` 的 `30085d628bc70eb89f051329b90020d652d97c52`。实际代码/逻辑已合并在 Core，本轮不把历史独立 Kit/Codegen 当主审对象。

增量包括 CI 集成环境、Core 最低版本 v1.18.0、Cache 测试修复以及 bugfix skill 合并。Codegen 生产差异主要在版本下限；上轮 RR-06～10 的五个修复实现没有再次变化，已有修复记录保留，本轮 Codegen 包回归重跑通过。Cache RR-03/11 属于另一 agent 的独立修复；读取索引了解状态，没有冒称完成独立验收。本轮没有执行发布/部署。

收尾再次 fetch 时远端已到 `96f0f7c57d8c0d24d09358be786c2595441716a8`，新增四个提交均为文档，记录另一工作线已发布 v1.18.0 及 CI/生成链检查。本轮源码验证基线仍为上面的 `30085d62`，没有把最新发布文档当成三项新问题的修复/验收证明。同步保留新的发布状态，不覆盖另一 agent 文档。

## 本轮覆盖与证据

| 有界范围 | 已读/已执行 | 结果与边界 |
| --- | --- | --- |
| cfggen schema → validateMeta → exportForGroups → generate/writeStruct/accessor | 主文件当前源码；图谱 49 个函数返回无后续页；Run 双向调用；现有 main/groups 测试 | 原有类型、引用、循环/命名、group 的源码链已读；不代表所有组合已执行 |
| 禁用数字索引 | 当前 CLI 生成；独立模块 tidy/编译；true 与省略 index 控制 | false 编译失败，两个控制通过，RR-12 |
| bean/固定函数/import 命名空间 | 当前 CLI 和独立模块编译 | 两个冲突都 exit0 后编译失败，共一个 RR-13 根因 |
| 复合 schema 消费者 | 嵌套 bean、合法切片递归、int/bool/string 索引、globals、server 分组 | CLI、消费包编译通过；client-only 表/字段不在输出。没有把此编译样本称为真实 JSON 加载/引用校验 |
| 依赖事务 → needsConsolidation → ConsolidateProject → stage/root 对账 | 当前 dependency/consolidate/manifest/CLI 与现有测试；临时 overlay 接入 resolver 注入点 | 成功的 stage 迁移被丢弃，RR-14；resolver 失败 root 字节保留控制通过；真实代理解析未测 |
| 显式 upgrade 与 deps 区分 | CLI 分支当前源码 | upgrade --consolidate 先迁移 root；新缺陷限定 deps 自动分支。显式升级消费者本轮未运行 |

图谱使用 `roost-core`、Tier 2；index_status 返回 ready，git 起点对齐基线，覆盖 generation `2026-09-30T07:37:30Z`。最初已检查的候选路径无 recorded gap，但 freshness 为 metadata_changed，已回读当前源码。依赖入口的双向 trace 完整返回、无下一页。后续显式 full 刷新、两条补充 snippet 和含 CLI 的九路径 coverage 请求均在 300 秒超时；CLI 补充覆盖未获得返回，这些范围由当前源码和执行结果降级补证，不声称 graph 同代/穷尽。聚合 D-whb_s 不用于基线判断。元数据摘要见证据 README。

## 执行结果

`Run-Review.ps1` 的六个 cfggen 项目：三个 CLI exit0 后消费者编译反例（对应 RR-12 与 RR-13），三个正常编译控制；另加依赖成功反例和失败保护控制。合计 **4 个预期失败反例，4 个通过控制**，对应三个新根因。所有消费者只有临时 go.mod 的 local replace，没有修改发布模块、go.work 或主项目输入。

包回归：`go test ./codegen/... -skip TestDeployScriptsCarryNoKnownShellcheckFindings -count=1`；定向 race：`go test -race ./codegen/internal/cfggen ./codegen/internal/roost -run 'TestCfggen|TestFrameworkDependencyUpdate|TestUpdateFrameworkDependencies|TestConsolidat' -count=1`。实际结果及原始日志见[证据](evidence/codegen-review-20260930-05/README.md)。这两组正式测试不包含文档 overlay，正常通过不能替代新反例。

本机缺少 sh/shellcheck 的 shell 检查沿用上轮环境限制，明确跳过。使用 Go 1.27、独立 GOCACHE/GOTMPDIR/GOMODCACHE 与本机下载缓存 file proxy，无需修改用户依赖或联网解析。

## 停点与下一入口

本轮从上一轮的 cfggen 复杂 schema/升级分支继续，新增三个未修问题，Codegen 尚未整体收敛。下一轮优先：生成绑定实际注册/JSON 加载/required/ref/skipempty 与索引值往返；显式 upgrade 的 dry-run、迁移失败和生成消费者；正式 Webroute 服务启动/旧 URL 退役验证。进程强杀/磁盘故障、发布 tag 与旧客户端兼容仍单列未验证，未重新扫描已确认且未改动的所有生成器。修复由另一次 bugfix 请求实施。
