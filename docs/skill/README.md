# 游戏 Skill 专项手册

本目录说明游戏内技能，不包含开发 AI 提示词。现行模块为 roost-core/skill，Runtime checkpoint=10；历史改名与旧格式只从 Git 追溯，不作为当前接入步骤。

- [整体设计与边界](../framework/guide/08-skill.md)
- [实现、类型与回归入口](../framework/impl/08-skill.md)
- [DSL 与最小接入](skill.md)
- [施法和战斗语义](skill-casting-and-combat.md)
- [详细实现手册](skill-implementation-guide.md)
- [测试方法](skill-testing-guide.md)
- [表现与数据同步](visual-sync-production-guide.md)

Runtime 不参与 Nest DAO 回滚；Spawn、Summon 和业务实体有不同拥有者。Host 能力如实声明，包装转发可选接口；实际游戏确定性和真实引擎验收由业务完成。
