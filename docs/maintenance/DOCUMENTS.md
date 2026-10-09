# 最终保留文档清单

## 1. 基线和数量

清理前 docs 跟踪文件 **3070** 个；v1.24.1 整理时保留/新建 **50** 个，删除旧路径 **3034** 个。其余同路径内容按现行版本重写或保留。运行时基线 v1.24.0，文档维护版 v1.24.1。

当前文档在该清单上新增独立 Gate 设计、其评审/实施方案及包目录分类方案，合计 **53** 份。三份文档明确区分已有能力与待实施部分，不改变已发布版本的实现状态。

先确定本清单，再迁移现行设计/维护知识、核对代码、清理旧文件与修复引用。不会删除游戏ai代码或游戏Skill文档，也不删除本地安装skill。

## 2. 全部剩余文件

| 文件 | 用途 |
| --- | --- |
| [docs/GETTING-STARTED.md](../GETTING-STARTED.md) | 从购买道具的例子认识框架、术语、运行第一个示例与学习顺序 |
| [docs/README.md](../README.md) | 维护入口、清单、证据或全包映射 |
| [docs/framework/README.md](../framework/README.md) | 维护入口、清单、证据或全包映射 |
| [docs/framework/PACKAGES.md](../framework/PACKAGES.md) | 维护入口、清单、证据或全包映射 |
| [docs/framework/GATEWAY.md](../framework/GATEWAY.md) | 网关现有能力、独立 Gate 待实施设计、分阶段验收及 TCP 文档纠正 |
| [docs/framework/GATEWAY-IMPLEMENTATION.md](../framework/GATEWAY-IMPLEMENTATION.md) | Gate 设计评审、具体接线、绑定与发送契约、资源限制及实施验收 |
| [docs/framework/PACKAGE-REORGANIZATION.md](../framework/PACKAGE-REORGANIZATION.md) | 包目录分类、Wiring/运行边界、Session 含义、同轮 Gate、迁移和验收 |
| [docs/framework/guide/00-overview.md](../framework/guide/00-overview.md) | 现行设计、职责、使用与限制 |
| [docs/framework/impl/00-overview.md](../framework/impl/00-overview.md) | 实现边界、不变量、源码类型与回归入口 |
| [docs/framework/guide/01-app-lifecycle.md](../framework/guide/01-app-lifecycle.md) | 现行设计、职责、使用与限制 |
| [docs/framework/impl/01-app-lifecycle.md](../framework/impl/01-app-lifecycle.md) | 实现边界、不变量、源码类型与回归入口 |
| [docs/framework/guide/02-nest-entity.md](../framework/guide/02-nest-entity.md) | 现行设计、职责、使用与限制 |
| [docs/framework/impl/02-nest-entity.md](../framework/impl/02-nest-entity.md) | 实现边界、不变量、源码类型与回归入口 |
| [docs/framework/guide/03-dataengine.md](../framework/guide/03-dataengine.md) | 现行设计、职责、使用与限制 |
| [docs/framework/impl/03-dataengine.md](../framework/impl/03-dataengine.md) | 实现边界、不变量、源码类型与回归入口 |
| [docs/framework/guide/04-sync.md](../framework/guide/04-sync.md) | 现行设计、职责、使用与限制 |
| [docs/framework/impl/04-sync.md](../framework/impl/04-sync.md) | 实现边界、不变量、源码类型与回归入口 |
| [docs/framework/guide/05-remote-mirror.md](../framework/guide/05-remote-mirror.md) | 现行设计、职责、使用与限制 |
| [docs/framework/impl/05-remote-mirror.md](../framework/impl/05-remote-mirror.md) | 实现边界、不变量、源码类型与回归入口 |
| [docs/framework/guide/06-saga.md](../framework/guide/06-saga.md) | 现行设计、职责、使用与限制 |
| [docs/framework/impl/06-saga.md](../framework/impl/06-saga.md) | 实现边界、不变量、源码类型与回归入口 |
| [docs/framework/guide/07-config.md](../framework/guide/07-config.md) | 现行设计、职责、使用与限制 |
| [docs/framework/impl/07-config.md](../framework/impl/07-config.md) | 实现边界、不变量、源码类型与回归入口 |
| [docs/framework/guide/08-skill.md](../framework/guide/08-skill.md) | 现行设计、职责、使用与限制 |
| [docs/framework/impl/08-skill.md](../framework/impl/08-skill.md) | 实现边界、不变量、源码类型与回归入口 |
| [docs/framework/guide/09-services.md](../framework/guide/09-services.md) | 现行设计、职责、使用与限制 |
| [docs/framework/impl/09-services.md](../framework/impl/09-services.md) | 实现边界、不变量、源码类型与回归入口 |
| [docs/framework/guide/10-time.md](../framework/guide/10-time.md) | 现行设计、职责、使用与限制 |
| [docs/framework/impl/10-time.md](../framework/impl/10-time.md) | 实现边界、不变量、源码类型与回归入口 |
| [docs/framework/guide/11-observability.md](../framework/guide/11-observability.md) | 现行设计、职责、使用与限制 |
| [docs/framework/impl/11-observability.md](../framework/impl/11-observability.md) | 实现边界、不变量、源码类型与回归入口 |
| [docs/framework/guide/12-codegen.md](../framework/guide/12-codegen.md) | 现行设计、职责、使用与限制 |
| [docs/framework/impl/12-codegen.md](../framework/impl/12-codegen.md) | 实现边界、不变量、源码类型与回归入口 |
| [docs/framework/guide/13-wiring.md](../framework/guide/13-wiring.md) | 现行设计、职责、使用与限制 |
| [docs/framework/impl/13-wiring.md](../framework/impl/13-wiring.md) | 实现边界、不变量、源码类型与回归入口 |
| [docs/framework/guide/14-foundation.md](../framework/guide/14-foundation.md) | 现行设计、职责、使用与限制 |
| [docs/framework/impl/14-foundation.md](../framework/impl/14-foundation.md) | 实现边界、不变量、源码类型与回归入口 |
| [docs/skill/skill.md](../skill/skill.md) | 游戏Skill专项使用/实现参考 |
| [docs/skill/skill-casting-and-combat.md](../skill/skill-casting-and-combat.md) | 游戏Skill专项使用/实现参考 |
| [docs/skill/skill-implementation-guide.md](../skill/skill-implementation-guide.md) | 游戏Skill专项使用/实现参考 |
| [docs/skill/skill-testing-guide.md](../skill/skill-testing-guide.md) | 游戏Skill专项使用/实现参考 |
| [docs/skill/visual-sync-production-guide.md](../skill/visual-sync-production-guide.md) | 游戏Skill专项使用/实现参考 |
| [docs/skill/README.md](../skill/README.md) | 游戏Skill专项使用/实现参考 |
| [docs/maintenance/README.md](README.md) | 维护入口、清单、证据或全包映射 |
| [docs/maintenance/DOCUMENTS.md](DOCUMENTS.md) | 维护入口、清单、证据或全包映射 |
| [docs/maintenance/CONSISTENCY.md](CONSISTENCY.md) | 维护入口、清单、证据或全包映射 |
| [docs/maintenance/KNOWN-LIMITS.md](KNOWN-LIMITS.md) | 维护入口、清单、证据或全包映射 |
| [docs/maintenance/PERFORMANCE.md](PERFORMANCE.md) | 维护入口、清单、证据或全包映射 |
| [docs/maintenance/EXTERNAL-VERIFICATION.md](EXTERNAL-VERIFICATION.md) | 维护入口、清单、证据或全包映射 |
| [docs/release/v1.24.0-NOTES.md](../release/v1.24.0-NOTES.md) | 已发布基线或本版说明/验收 |
| [docs/release/v1.24.0-IMPLEMENTATION.md](../release/v1.24.0-IMPLEMENTATION.md) | 已发布基线或本版说明/验收 |
| [docs/release/v1.24.1-NOTES.md](../release/v1.24.1-NOTES.md) | 已发布基线或本版说明/验收 |
| [docs/release/v1.24.1-IMPLEMENTATION.md](../release/v1.24.1-IMPLEMENTATION.md) | 已发布基线或本版说明/验收 |

## 3. 移除类别

| 类别 | 删除旧文件数 | 当前知识去向 |
| --- | ---: | --- |
| 根目录旧指南/方案 | 16 | 现行契约进入framework/maintenance；历史过程仅Git追溯 |
| agent-skills | 13 | 现行契约进入framework/maintenance；历史过程仅Git追溯 |
| bug | 664 | 现行契约进入framework/maintenance；历史过程仅Git追溯 |
| bugfix | 1446 | 现行契约进入framework/maintenance；历史过程仅Git追溯 |
| feature | 137 | 现行契约进入framework/maintenance；历史过程仅Git追溯 |
| framework | 1 | 现行契约进入framework/maintenance；历史过程仅Git追溯 |
| history | 22 | 现行契约进入framework/maintenance；历史过程仅Git追溯 |
| release | 21 | 现行契约进入framework/maintenance；历史过程仅Git追溯 |
| review | 709 | 现行契约进入framework/maintenance；历史过程仅Git追溯 |
| skill | 5 | 现行契约进入framework/maintenance；历史过程仅Git追溯 |

## 4. 历史证据访问

清理前完整docs固定在 [9d955fb0](https://github.com/tjbdwanghaibo/roost-core/tree/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs)。旧RR、feature、review及AI开发规范不再混在当前规范中；有必要引用时使用固定commit链接，不能用main旧路径。

```sh
git show 9d955fb0df35f082dfc9be24c2f3a4524d437067:docs/bug/README.md
git show 9d955fb0df35f082dfc9be24c2f3a4524d437067:docs/feature/IMPLEMENTATION-2026-10-08-CLIENT-LOCKSTEP.md
# 需要某个旧文件时先查看内容，再决定恢复到何处；不要整目录覆盖现行文档。
```

## 5. docs外资料

根README、CHANGELOG、各包README、codegen/docs、client/README以及脚本运维手册继续保留，必要的指向旧docs链接改到现行入口或固定历史证据。它们不计入上述docs文件数。源码里的RR编号/旧路径注释仍可按Git历史查找，本轮不为清理注释修改生产代码。

## 6. 空目录

Git不跟踪空目录。本次同时删除工作树中因文档移除而留下的空目录，以及仓库其他普通空目录；从子目录向上逐个确认空后删除，不递归删除有文件的目录，不操作.git或目录链接。全新克隆不会重新出现这些空目录。
