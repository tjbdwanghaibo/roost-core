# 2026-09-30 Codegen 第一轮：生成链与输入退役

## 基线与范围

三仓 fetch 后远端默认分支未前进、工作树干净：Core `1b7a2fc5aa4bfd9ae921ae8c80efd2392d08a165`，Kit `f0e5b67aa43473c2bcabd2ae180c69559fd6e20e`，独立 Codegen `1e028fa4b2927ffb5d7772b90440e7447dc665cf`。独立 Codegen 仓库的最终提交已说明生成器并入 Core，故本轮审查当前被 `roost generate` 调用的 Core `codegen/`，未把冻结仓当生产入口。[上一轮 Service 验证已归档](REVIEW-2026-09-30-services-13.md)；真实外部资源、跨机 HA/分区和生产长稳仍须在具备环境时做，重复本机代理/单机 Redis 不再增加同等级结论。

Codebase Memory 的 `roost-core` 当前 generation `2026-09-29T14:50:03Z`、ready 且 HEAD 对齐。默认 Verify：图谱定位 `GenerateTransactional`、`planStagedProjectCommit`、`commitSyncChanges`、`servicerpc.Run/ParseDir`、`protocol.Run`、`dao.Run`，并追 inbound（CLI 与 `roost` 生成链）；逐项读取当前源码。`check_index_coverage` 对所依赖路径没有记录缺口，但标记 `metadata_changed`，因此以实际源码复核，不把该信号解释为全仓索引完整保证。

## 本轮已读与实际验证

| 路径/不变量 | 源码与执行证据 | 结论/下一步 |
| --- | --- | --- |
| `roost.GenerateTransactional` 的暂存、输入快照、变更规划、提交/回滚 | `codegen/internal/roost/generate.go:163-236,241-306,358-448`；`project.go:256-366,502-572,600-685`；现有 `TestTransactionalGenerateDoesNotCommitEarlierGeneratorOnLaterFailure` 与包回归 | 暂存成功后仅提交生成物及 go.mod/sum；逐文件 optimistic guard 与逆序回滚。**未做强杀/磁盘故障注入，不声称跨文件原子提交。** |
| RPC 输入解析与 `-check`/双半输出 | `servicerpc/parse.go:88-152`、`run.go:15-131`，`-emit`/`-out` 已有测试；隔离目录生成后删标记 | [RR-20260930-01](../bug/REVIEW-2026-09-30-codegen-01.md)：零服务时跳过旧文件检测，实测 `-check` 0 退出。 |
| Protocol 定义到 proto/PB/msgid/manifest/handler | `protocol/run.go:26-190`，隔离 module 生成后删最后定义 | [RR-20260930-02](../bug/REVIEW-2026-09-30-codegen-01.md)：空定义成功返回而旧四份文件留存。单条消息移除/旧 handler 待补动态验证。 |
| DAO 定义退役对照 | `dao/main.go:32-159`，现有 `TestRunSweepsOrphansWhenAllDefinitionsRemoved` | DAO 已为最后定义删除和重命名执行自有产物清理；只作实现对照，不据此推断其它生成器也有此保障。 |

两项复现均使用当前仓内 CLI、临时输出目录，不修改 Core 生产代码。[命令和输出](evidence/codegen-review-20260930-01/README.md)。本轮为 Codegen 高风险入口第一批有界审查，不是全部生成器逐路径覆盖；尤其 entity/nest、config/table、event/registry、webroute/errcode 的退役及交叉生成、真实项目构建尚未审完。

## 测试与环境边界

`go test ./codegen/internal/servicerpc ./codegen/internal/roost` 中 servicerpc 通过；roost 的 `TestDeployScriptsCarryNoKnownShellcheckFindings` 因本机 PATH 无 `sh` 而失败，日志同时说明 shellcheck 未安装。该项是 Windows 测试工具环境限制，不计产品 bug。随后执行 `go test ./codegen/... -skip TestDeployScriptsCarryNoKnownShellcheckFindings`，**所有有测试的 Codegen 包通过**，无其它失败；跳过的只是该具名 shell 静态检查，不用它代替 Linux CI。未运行生成项目实际发布/跨版本迁移。

## 停点

下轮先按 [问题交接](../bug/REVIEW-2026-09-30-codegen-01.md) 复读旧产物所有权/引用链，再审 **protocol 单条消息/handler 退役**、**servicerpc 接口重命名与跨包半边**、**entity/nest 和 registry 的孤儿产物/注册一致性**。用户若继续只要求 review，则不实施修复。生产代码未改，审查文档提交到 Core。
