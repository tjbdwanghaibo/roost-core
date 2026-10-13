# Roost 文档入口

当前版本：**v1.26.0**（[发布说明](release/v1.26.0-NOTES.md) · [实现与证据](release/v1.26.0-IMPLEMENTATION.md) · [发布验收](release/v1.26.0-VALIDATION.md)）。升级要求见发布说明 §2；之后未发版的变化见 [CHANGELOG `[Unreleased]`](../CHANGELOG.md)。

第一次接触先读 [第一次认识和使用 Roost](GETTING-STARTED.md)：用一次购买道具解释三大核心，并运行一个不需要数据库的示例。

## 按目的找文档

| 目的 | 入口 |
| --- | --- |
| 理解框架、查模块契约 | [模块目录](framework/README.md)：核心 Nest / DataEngine / Sync，次核心 Service / Codegen / Wiring，其他包与专项契约 |
| 建工程、写生成标记 | [codegen 入口与快速上手](../codegen/README.md) · [命令与标记参考](../codegen/docs/CODEGEN_REFERENCE.zh-CN.md) · [项目生成器](../codegen/docs/PROJECT_GENERATOR.zh-CN.md) |
| 跑完整示例工程 | [game-demo 模板](../demo/README.md) · [功能逐项说明](../demo/WALKTHROUGH.md) |
| 写技能 / 接战斗 | [Skill 文档导航](skill/README.md) |
| 处理回复错误（能否重试） | [回复错误判别](framework/guide/02-nest-entity.md#6-回复错误判别是否可能已提交能否重试) |
| 修改代码、升级、排障、发版 | [维护手册](maintenance/README.md) · [已知限制](maintenance/KNOWN-LIMITS.md) · [外部验证清单](maintenance/EXTERNAL-VERIFICATION.md) |
| 性能结论与复跑 | [性能报告与复跑](performance/README.md) |
| 某个文件是干什么的 | [文档清单](maintenance/DOCUMENTS.md)：每个现存文档一行职责，及已删除资料的固定提交追溯 |

## 专题

- [独立 Gate 设计](framework/GATEWAY.md) 与 [Gate 设计评审与实施方案](framework/GATEWAY-IMPLEMENTATION.md)：随 v1.25.0 交付的网关能力、协议契约与验收边界。
- [包目录分类、Wiring 与 Service 收敛方案](framework/PACKAGE-REORGANIZATION.md)：原 kit 接线已迁为 Wiring，领域与运行实现归所属模块。
- [Nest 三池与 Await](framework/NEST-AWAIT.md)：short / long / io 三池、匿名异步恢复与池命名迁移表（随 v1.26.0 发布）。
- [玩家接入与 Sync 命名后续事项](maintenance/ACCESS-SYNC-FOLLOWUPS.md)：Session 服务可选化等待定方向。

## 历史

- [v1.25.0 发布说明](release/v1.25.0-NOTES.md)（[实现与验证](release/v1.25.0-IMPLEMENTATION.md) · [功能验收](release/v1.25.0-VALIDATION.md)）· [v1.24.1 发布说明](release/v1.24.1-NOTES.md) · [v1.24.0 发布说明](release/v1.24.0-NOTES.md)
- 合仓前 codegen / kit / service 的变更记录：[docs/release/history](release/history/codegen-CHANGELOG.md)（只读）
- 开发过程资料（旧 RR、bugfix、review、feature、USER_GUIDE、TROUBLESHOOTING）：[v1.24.1 清理前的 docs（固定提交）](https://github.com/tjbdwanghaibo/roost-core/tree/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs)
