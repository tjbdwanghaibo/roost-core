# 2026-09-30 Codegen 第二轮：旧产物修复与 Entity/Nest 退役审查

## 基线与范围

开始时三仓 fetch；Core 从上轮 `22d1f994` 快进至 `f6566d4e`（新增两个其他 agent 文档提交，Codegen 源码未变），Kit `f0e5b67a`、独立冻结 Codegen `1e028fa4` 无新源码。当前可执行生成器仍在 Core `codegen/`。用户要求先 bugfix、再 review：本轮只实施 [RR-20260930-01](../bugfix/RR-20260930-01.md) 和 [RR-20260930-02](../bugfix/RR-20260930-02.md) 及其必要的 `roost` 暂存/检查调用链；新发现的 [RR-20260930-04/05](../bug/REVIEW-2026-09-30-codegen-02.md) 仅交接。

Codebase Memory `roost-core` 在审查起点 HEAD 对齐 `22d1f994`、ready，图谱定位并追踪 `servicerpc.Run/GenerateWith/ParseDir`、`protocol.Run`、`dao.removeOrphanGenerated`、`roost.snapshotGenerated/planStagedProjectCommit`、`entity.run`、`nest.run`、`registry.Run/Scan`。随后 Core 的两个文档提交使图谱 HEAD 先于新主树；所用结构无代码变化，关键文件均以 `f6566d4e` 源码复读。候选范围 `check_index_coverage` 无记录缺口但均 `metadata_changed`，所以结论按源码和实际 CLI/测试限定，不称更新后全图穷尽。

## 两项修复的实际结果

| 入口 | 修前红证据 | 修后绿证据与行为 |
| --- | --- | --- |
| RPC marker 删除 | 新测试 `-check accepted retired transport: <nil>`；上轮 CLI `CHECK_EXIT=0 BEFORE=2 AFTER=2` | `-check` 报两个 `(orphan)`、exit 1；正常生成移除两个、exit 0。跨包装配只删本命令半边，其他源、传输半边及手写同名文件保留。 |
| Protocol 最后定义删除 | 新测试 `retired output remains ...protocol.proto`；上轮 CLI `SECOND_EXIT=0 BEFORE=4 AFTER=4` | 当前 CLI 移除 proto/PB/msgid/manifest 4/4、exit 0；带 bootstrap、部分 handler 域退役、手写文件保护与失败前置校验通过。`SyncProject` 真实提交 4 个删除，`generate --check` 分别发现 markerless proto/manifest 漂移。 |

[完整复跑步骤与命令输出](evidence/codegen-review-20260930-02/README.md)。新增正式行为测试位于 `codegen/internal/servicerpc/retirement_test.go`、`protocol/retirement_test.go`、`roost/protocol_retirement_test.go`。按变化跑定向测试与 `-race` 均通过；全 Codegen 包测试见证据。已有 `TestDeployScriptsCarryNoKnownShellcheckFindings` 在此 Windows PATH 缺 `sh`，本轮全包命令仅跳过这一具名 Linux shell 静态检查，不改该测试，也不把它记为产品故障。

## 新增有界审查

| 路径 | 已读/实测 | 结论 |
| --- | --- | --- |
| `entity/main.go` → `gen.go` → `registry/parse.go` | 扫描有 marker 的目录、生成 wire；隔离生成后删唯一 marker，CLI 第二次成功而 wire 留存，wire 仍有 `//roost:register phase=entity` | [RR-20260930-04 P2](../bug/REVIEW-2026-09-30-codegen-02.md) 未修。Registry 继续发现旧注册属于源码调用链推断；最终消费待测。 |
| `nest/main.go` 的 wrapper/sender/syncsender → bootstrap | 隔离生成 4 个文件后删唯一 marker；第二次成功打印 `all files up to date`，旧四份仍在 | [RR-20260930-05 P2](../bug/REVIEW-2026-09-30-codegen-02.md) 未修。game 根目录 bootstrap 可更新，不能声称其注册一定未变。 |
| `registry.Run/Scan/Generate` | 图谱与源码确认 aggregate 每次按扫描 marker 重新生成，空集合仍生成；未运行新的最终业务聚合 | 该层能收敛当前 marker，但会如实读取生成文件中的陈旧 marker；下一轮做跨层生成项目验证。 |

本轮不是 Codegen 全域完成。下一入口为 RR-04/05 的正式生成项目与消费者验证，再查事件、表格/配置、webroute 等生成器的输入改名/移除边界。新问题未实施，不将原两项修复外推到别的生成器。
