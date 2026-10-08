# DAO 自动迁移与 Remote 旧发布端清理

维护者 2026-10-08 决定：DAO schema 自动迁移不需要；Remote 旧发布端分支不需要；Cube 三处旧 API 改用 Roost。

基线：0166a45b。此轮是已授权的能力撤销，不把历史上按原契约实现的行为登记为新 bug。

1. 保留 schema 声明与校验，Repository 在任何 DAO 水合之前校验完整聚合；旧、新或缺少声明均拒绝，生成 RestorePersisted 也在解码之前拒绝不匹配。移除 MigrationRunner、迁移重读、投影冲突特赦、DAORegistry 和生成 Migrate。SystemCommitter/ProjectionTicket 与通用业务 Registry 有其他用途，保留。
2. 正式 game-demo 移除 db/migrations 与服务启动注册；用真实生成 DAO 验证当前 schema 恢复，以及不匹配时数据和 tracker 不变。
3. Remote 兴趣发布与接收都要求非零 Generation；无 payload 的 Delete 明确失败。删除 generation=0 撤销特例，保留当前乱序水位、过期与容量保护。
4. Cube 现状仍依赖 cube-core/cube-kit，三处 Nest API 涉及框架类型整体迁移；维护者随后确认 Cube 仓库已经废弃，不需要处理，本项取消，不列后续迁移待办。

验收：定向契约、相关 race、全仓 build/vet/test、根示例实跑、正式生成 game-demo build/vet/test 与重复生成检查；Linux 交叉编译单独标注。不执行压测、部署、清库或旧 WAL 改写。
