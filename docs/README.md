# Roost 稳定版文档入口

当前版本：**v1.25.0**；[发布说明](release/v1.25.0-NOTES.md)与[验收记录](release/v1.25.0-IMPLEMENTATION.md)。

先读 [第一次认识和使用 Roost](GETTING-STARTED.md)：用一次购买道具解释三大核心，并运行一个不需要数据库的示例。

1. [最终剩余文档清单](maintenance/DOCUMENTS.md)：本目录只保留维护、设计、实现和必要使用参考。
2. [框架分区目录](framework/README.md)：核心 Nest/DataEngine/Sync，次核心 Service/Codegen/Wiring，以及其他包。
3. [代码一致性核对](maintenance/CONSISTENCY.md)：具体纠正项、源码证据与核对限制。
4. [维护手册](maintenance/README.md)：升级、生成、测试、停机、故障定位和发布。
5. [已知边界](maintenance/KNOWN-LIMITS.md)：未验证不等于缺陷，也不计作已验收。
6. [独立 Gate 设计](framework/GATEWAY.md)：当前网关能力、待实施方案、验收条件及 TCP 文档纠正项。
7. [Gate 设计评审与实施方案](framework/GATEWAY-IMPLEMENTATION.md)：基于当前源码的具体接线、绑定与发送契约、资源限制和开发顺序，尚未实施。
8. [包目录分类、Wiring 与 Service 收敛方案](framework/PACKAGE-REORGANIZATION.md)：Kit 接线已迁为 Wiring，领域与运行实现归所属模块；包含改前映射、当前目录和 Gate 的前置验收。

[v1.24.0 发布说明](release/v1.24.0-NOTES.md) · [v1.24.1 发布说明](release/v1.24.1-NOTES.md)
