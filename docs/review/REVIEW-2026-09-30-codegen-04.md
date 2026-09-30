# 2026-09-30 Codegen 第四轮：RR-06～10 修复与旧验证缺口复查

Core `origin/main` 已 fetch，工作起点 `da56c99896a0564d8b88340efc8f8e915706d66f`，隔离工作树干净且与远端一致；实际 Codegen 位于 Core `codegen/`。Kit、历史独立 Codegen 仓不是本次修复范围。`roost-core` 图谱的项目 HEAD 已对齐本轮起点，索引报告 generation `2026-09-30T07:37:30Z`；候选七个入口的调用方向与源码片段已查询，`check_index_coverage` 对相关路径和六个 `codegen/internal` scope 无记录缺口，但文件 freshness 为 `metadata_changed`。因此修后结论以当前工作树源码、正式测试及隔离项目实跑为准，不把图谱当新代码的穷尽证明。

## 修复状态

| 问题 | 本轮最终行为 | 证据与限制 |
| --- | --- | --- |
| [RR-06 Attribute](../bugfix/RR-20260930-06.md) | 零 marker、改名按自身生成头退役旧文件；手写同名保留 | 正式生成→清空输入测试、race 通过；显式 `-output` 多 profile 现在拒绝 |
| [RR-07 Event](../bugfix/RR-20260930-07.md) | 零定义退役三固定文件及孤儿 handler；部分 receiver 删除也对账 | 正式失败前不退役、部分删除回归与 race 通过；旧事件 ID 业务兼容未验 |
| [RR-08 Webroute](../bugfix/RR-20260930-08.md) | 最后路由移除后退役有自身生成头的 `webroute_gen.go` | 正式测试与 race 通过；真实服务启动注册未独立运行 |
| [RR-09 Tablegen](../bugfix/RR-20260930-09.md) | v2 manifest 记录 JSON 名称和哈希；零 meta 对账并安全删除；上层暂存接收删除 | 正式所有权/改动保护、`--check`/暂存测试、外部工程消费者编译通过；v1 归属不明时显式报错 |
| [RR-10 Errcode](../bugfix/RR-20260930-10.md) | AST 只提取实际 Define 调用 | 注释/块注释/字符串用例和 race 通过；保留原字面量约束 |

修前失败证据仍在[第三轮复现](evidence/codegen-review-20260930-03/README.md)，本轮新正式用例覆盖退役后的正反例。`go test ./codegen/... -skip TestDeployScriptsCarryNoKnownShellcheckFindings` 全包通过；六个相关包的新增用例 `-race -count=1` 全通过。修复后直接执行 `go test ./codegen/internal/roost -count=1` 时，唯一失败是 `TestDeployScriptsCarryNoKnownShellcheckFindings`：本机 PATH 没有 `sh`，也没有 `shellcheck`，测试报告的是工具缺失。同包 `go test ./codegen/internal/roost -count=1 -skip TestDeployScriptsCarryNoKnownShellcheckFindings` 重跑通过；不能称未跳过全绿。

## 之前的验证障碍

上轮正式外部工程 `go mod tidy` 在默认模块缓存写锁处中止。本轮创建独立 `example.com/consumer` 工程，在临时项目的 `go.mod` 指向当前 Core 工作树；以独立 `GOMODCACHE`、本机模块下载缓存的只读 `file://` 代理、Go 1.27 工具链运行，`go mod tidy` 成功。首次 `project new` 的联网依赖解析仍因本机网络沙箱失败，但项目文件已经落盘；后续用上述本地环境完成验证，不把第一次联网失败算作产品缺陷。

该真实隔离工程依次执行：写一张 `monster` schema/CSV，`roost generate` 生成 `configs/data/monster.json`，`go test ./...` 通过；删除最后 schema 标记和 CSV，`roost generate --check` 返回 1 并报告 `configs/data/_manifest.json`、`monster.json` 和生成 Go 文件过期；重新 `roost generate` 报退役 JSON，`--check` 转绿，第二次 `go test ./...` 全包通过。正式包测试还直接验证 `planStagedProjectCommit`/`commitSyncChanges` 将 JSON 删除落回原项目。因此上轮“消费者编译受缓存锁限制”和本轮表格暂存/漂移两项均已补证。

## 继续验证的边界

这轮没有运行真实 HTTP 服务来确认旧 Webroute URL 在启动后不可达；生成文件退役已验证，服务消费者仍需单独运行。旧事件/协议 ID、旧路由和旧配置在已部署客户端及线上数据中的兼容窗口，也不是源码测试能决定。旧 v1 表格 manifest 无所有权信息，含遗留 JSON 的项目需按 [RR-09 恢复步骤](../bugfix/RR-20260930-09.md)人工处理。此前矩阵中 cfggen 复杂 schema、roost upgrade/脚手架分支、进程强杀或磁盘故障仍未在本轮验证；Codegen 整体不能标为完全收敛。本轮没有发布、部署或生产数据迁移。
