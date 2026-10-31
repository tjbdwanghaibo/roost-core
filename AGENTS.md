# 仓库维护入口

版本只看 [CHANGELOG](CHANGELOG.md)：最上面的已归档段是最新发布版本，`[Unreleased]` 是 main 上未发版的变化；各功能版本的升级要求见 [docs/release](docs/release/README.md)，发版用 `scripts/release.sh`（[维护手册 §5](docs/maintenance/README.md#5-发版)）。生成物最低 core 见 `codegen/internal/roost/manifest.go` 的 `minimumVersions`。修改前阅读 [维护手册](docs/maintenance/README.md)、[模块目录](docs/framework/README.md) 和 [已知限制](docs/maintenance/KNOWN-LIMITS.md)。设计与实现的现行说明在docs/framework；开发过程资料已迁入Git历史，不再要求读取已删除的docs/agent-skills。

业务代码修改应补必要中文契约注释、按影响范围测试、更新对应现行文档。文档补丁不得混入行为重构。不等待GitHub CI，不能把未运行检查记成通过。

## 游戏接入 skill

按玩法生成或验证工程时使用本机 `roost-genre`（`~/.claude/skills/roost-genre/SKILL.md`，以及同目录 `references/`）。它是 agent 的游戏接入流程，不是 `gameplay/skill` 游戏内技能系统，也不替代本文件的维护规则。其他 agent 可直接阅读该文件；未安装时按下面的仓内入口执行，不假设本机目录在其他机器存在。Skill 保持仓外维护，不将个人安装路径作为仓内 Markdown 链接。

- 权威说明：[按玩法学习与验收](docs/framework/README.md#学习文档按游戏场景学框架)、[已知限制](docs/maintenance/KNOWN-LIMITS.md)。覆盖 SLG、MOBA、MMORPG、卡牌 RPG、塔防、生存撤离、自走棋、放置、合成、派对房间。
- 生成器只生成框架骨架；玩法参考来自 `scripts/testdata/<genre>/`。完整参考工程沿用 `-template game-demo`，源码在 [demo](demo/README.md)，不另建一套重复的完整 demo。
- 先确认目标 tag / main：skill 命令、夹具、CLI 与运行依赖必须属于同一版本。派对房间 v1.33.2 / v1.34.0 / v1.34.1 的接线不同；以目标检出的 `scripts/test-party-room-generated.sh` 为准，不把最新命令套到旧夹具。
- 变更玩法夹具、生成步骤或公共接线时，同步核对 skill 对应 reference（已安装时）与仓内手册。验收包含自定义 module、实际场景数、零意外 SKIP、build / vet、ID 与生成物漂移检查；不能仅凭脚本退出 0 宣称已覆盖。
- 默认纵切使用替身提交器；`REAL` 模式才验证真实依赖与恢复。客户端 TCP、Unity、Linux、跨机、性能与长稳分别记录；不能把内存场景通过描述为生产验收完成。
- 心跳最短 50ms 是心跳档位约束，不是 Sync 的发送上限；`on_change` 在 handler 提交、确认与解锁之后汇总变化唤醒。普通持久实体新建的落地规则见 [DataEngine](docs/framework/guide/03-dataengine.md)，业务初始化和首次入场仍需自己的事务与幂等事实。

## Codebase Memory

- 结构发现优先使用 codebase-memory-mcp：search_graph → trace_path → get_code_snippet；复杂关系用 query_graph，高层概览用 get_architecture。字符串、配置和非代码文档可直接搜索。
- 每次会话开始或上下文压缩后先通过 list_projects/index_status 确认最近项目与 generation，默认 Verify；处理相关分页。
- 候选路径明确后，用 check_index_coverage 检查全部证据路径；否定/穷尽结论加对应 scopes。过期、部分解析、跳过、排除或未知覆盖必须读取源码补证；无记录缺口不代表完整。
- 工具不可用时明确限制，按精确源码和 rg 继续。不要声称使用了不可用索引，或把旧代际当新代码。
- 若任务获准委派，先在父任务完成图谱/coverage 定位，传递项目、代际、范围、符号、分页、缺口和源码补证；子 agent 无图谱能力时使用这些证据和当前源码。
