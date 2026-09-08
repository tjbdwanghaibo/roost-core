# roost-skill 文档导航

本文档目录按使用角色组织。Go 的稳定核心包路径是
`github.com/tjbdwanghaibo/roost-skill/skill`；JSON wire schema
`roost.skill/v2` 和编译器语义 `skillv2-compiler-2` 是独立的持久协议版本，
不会再体现在 Go 目录名中。

## 新接入项目

1. [稳定 Skill API 与最小接入](skill.md)
2. [施法语义与战斗内容电池](skill-casting-and-combat.md)
3. [架构、Host 边界与同步流程](architecture-and-migration.md)
4. [Visual 与数据同步生产指南](visual-sync-production-guide.md)

## 框架维护者

1. [当前实现学习手册](skill-implementation-guide.md)
2. [阅读与测试清单](skill-testing-guide.md)
3. [生产门槛](production-readiness.md)

## 升级与生成

- 从旧 `/skillv2` Go 包迁移：[稳定包破坏性升级手册](breaking-upgrade-skill-package.md)
- AI 生成约束：[system prompt](ai-skill-system-prompt.md) 与
  [user prompt](ai-skill-user-prompt.md)

## 文档事实来源

- Go API：源码注释与 `go doc github.com/tjbdwanghaibo/roost-skill/skill`
- JSON 字段：`skill/wire_*.go`、严格 parser 和 `skill/testdata/*.json`
- 运行语义：`skill/runtime*.go`、`skill/host*.go` 与验收测试
- 生产约束：`production-readiness.md` 和 CI；README 只提供入口，不覆盖这些门槛

修改公开类型、wire 字段、checkpoint、同步 schema 或 Host 契约时，必须在同一提交中
更新对应文档、fixture、迁移说明和测试。
